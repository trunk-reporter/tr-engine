package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

// requestIDPattern is what a client-supplied X-Request-ID must look like to
// be kept (§5); anything else is replaced by a generated ID.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func Logger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		h := hlog.NewHandler(log)
		// Every request gets an access line, refusals included: failed and
		// anonymous attempts are recorded nowhere else (§9). hlog's writer
		// keeps http.Hijacker, so WebSocket upgrades work through it; their
		// line is written when the socket closes.
		accessLog := hlog.AccessHandler(func(r *http.Request, status, size int, dur time.Duration) {
			if status == 0 && websocket.IsWebSocketUpgrade(r) {
				// The upgrade wrote 101 on the hijacked connection.
				status = http.StatusSwitchingProtocols
			}
			hlog.FromRequest(r).Info().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", status).
				Int("size", size).
				Dur("duration_ms", dur).
				Msg("request")
		})
		return h(accessLog(next))
	}
}

// Recoverer turns a handler panic into a JSON 500 and logs it, with its
// stack, through the request's logger (it runs inside Logger, which also
// records the 500 in the access line). http.ErrAbortHandler is re-raised:
// it asks net/http to abort the response.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				if rv == http.ErrAbortHandler {
					panic(rv)
				}
				value, stack := rv, debug.Stack()
				if hp, ok := rv.(*handlerPanic); ok {
					value, stack = hp.value, hp.stack
				}
				hlog.FromRequest(r).Error().Interface("panic", value).Str("stack", string(stack)).
					Str("method", r.Method).Str("path", r.URL.Path).Msg("recovered from panic")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"code":"internal_error","error":"internal server error"}`)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS headers (§5). No request carries ambient credentials (no cookies), so
// every origin may call the API, and credentials are never allowed.
const (
	corsAllowMethods  = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, Last-Event-ID, X-Actor, X-Request-ID"
	corsExposeHeaders = "X-Request-ID, Retry-After, WWW-Authenticate"
	corsMaxAge        = "600"
)

// CORS allows every origin without credentials and answers OPTIONS with 204
// before any routing or auth.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// APIHeaders marks every /api/v1 response as uncacheable and dependent on
// the credential (§4). Match replaces Cache-Control with "private" for call
// audio.
func APIHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPrefix || strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
			h := w.Header()
			h.Set("Cache-Control", "no-store")
			h.Add("Vary", "Authorization")
		}
		next.ServeHTTP(w, r)
	})
}

// ResponseTimeout wraps non-streaming handlers with a write deadline.
// SSE and audio endpoints are excluded since they stream indefinitely.
func ResponseTimeout(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip streaming endpoints
			if strings.HasSuffix(r.URL.Path, "/events/stream") ||
				strings.HasSuffix(r.URL.Path, "/audio") ||
				strings.HasSuffix(r.URL.Path, "/audio/live") {
				next.ServeHTTP(w, r)
				return
			}
			h := http.TimeoutHandler(keepPanicStack(next), timeout, `{"code":"request_timeout","error":"request timeout"}`)
			h.ServeHTTP(w, r)
		})
	}
}

// handlerPanic carries a handler's panic value and the stack it happened on
// across http.TimeoutHandler, which runs the handler in another goroutine
// and re-panics in its own, so Recoverer's stack would not show the handler.
type handlerPanic struct {
	value any
	stack []byte
}

func (p *handlerPanic) String() string { return fmt.Sprint(p.value) }

// keepPanicStack re-panics a panic of next as a *handlerPanic that keeps its
// stack (http.ErrAbortHandler stays as it is).
func keepPanicStack(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				if _, ok := rv.(*handlerPanic); ok || rv == http.ErrAbortHandler {
					panic(rv)
				}
				panic(&handlerPanic{value: rv, stack: debug.Stack()})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MaxBodySize limits request body size. Returns 413 if exceeded.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
