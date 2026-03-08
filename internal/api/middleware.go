package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"golang.org/x/time/rate"
)

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func Logger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		h := hlog.NewHandler(log)
		accessLog := hlog.AccessHandler(func(r *http.Request, status, size int, dur time.Duration) {
			hlog.FromRequest(r).Info().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", status).
				Int("size", size).
				Dur("duration_ms", dur).
				Msg("request")
		})
		return h(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip access log wrapping for WebSocket endpoints — hlog's
			// statusWriter doesn't implement http.Hijacker, breaking upgrades.
			if strings.HasSuffix(r.URL.Path, "/audio/live") {
				next.ServeHTTP(w, r)
				return
			}
			accessLog(next).ServeHTTP(w, r)
		}))
	}
}

func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				log := hlog.FromRequest(r)
				log.Error().Interface("panic", rv).Msg("recovered from panic")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"code":"internal_error","error":"internal server error"}`)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORSWithOrigins returns CORS middleware that restricts to the given origins.
// If origins is empty, all origins are allowed (*).
func CORSWithOrigins(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if len(allowed) == 0 {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			} else {
				// Origin not allowed — still serve the request but without CORS headers.
				// Browsers will block the response on the client side.
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}


// RateLimiter returns middleware that applies per-IP rate limiting.
// rps is requests per second, burst is the maximum burst size.
func RateLimiter(rps float64, burst int) func(http.Handler) http.Handler {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

	getLimiter := func(ip string) *rate.Limiter {
		mu.Lock()
		defer mu.Unlock()
		if lim, ok := limiters[ip]; ok {
			return lim
		}
		lim := rate.NewLimiter(rate.Limit(rps), burst)
		limiters[ip] = lim
		return lim
	}

	// Background cleanup of stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			mu.Lock()
			// Simple strategy: clear the map periodically.
			// Active clients will re-create their limiter on next request.
			limiters = make(map[string]*rate.Limiter)
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !getLimiter(ip).Allow() {
				w.Header().Set("Retry-After", "1")
				WriteErrorWithCode(w, http.StatusTooManyRequests, ErrRateLimited, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
			h := http.TimeoutHandler(next, timeout, `{"code":"request_timeout","error":"request timeout"}`)
			h.ServeHTTP(w, r)
		})
	}
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

// clientIP extracts the client IP, checking X-Forwarded-For and X-Real-IP
// headers first (for reverse proxy setups), then falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	// X-Forwarded-For: client, proxy1, proxy2 — take the first (leftmost)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(ip)
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// extractBearerToken reads the bearer token from the Authorization header
// or the ?token= query parameter (fallback for EventSource/SSE).
func extractBearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return auth[7:]
	}
	if qt := r.URL.Query().Get("token"); qt != "" {
		return qt
	}
	return ""
}

// UploadAuth is like BearerAuth but also accepts auth via form field "key" or "api_key"
// in multipart uploads. This supports trunk-recorder upload plugins (rdio-scanner, OpenMHz)
// which send the API key as a form field rather than an Authorization header.
// Check order: Authorization header → ?token= query param → form field "key" → form field "api_key"
func UploadAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}

			// 1. Check Authorization header / ?token= query param
			if provided := extractBearerToken(r); provided != "" {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}

			// 2. Check form field "key" (rdio-scanner) or "api_key" (OpenMHz)
			if err := r.ParseMultipartForm(32 << 20); err == nil {
				for _, fieldName := range []string{"key", "api_key"} {
					if val := r.FormValue(fieldName); val != "" {
						if subtle.ConstantTimeCompare([]byte(val), []byte(token)) == 1 {
							next.ServeHTTP(w, r)
							return
						}
					}
				}
			}

			WriteError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}

// BearerAuth requires a valid bearer token matching any of the provided tokens.
// Empty tokens in the list are skipped. If all tokens are empty, all requests pass through.
func BearerAuth(tokens ...string) func(http.Handler) http.Handler {
	// Filter to non-empty tokens
	var valid []string
	for _, t := range tokens {
		if t != "" {
			valid = append(valid, t)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(valid) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			provided := extractBearerToken(r)
			for _, t := range valid {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(t)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}

			WriteError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}

// WriteAuth requires the write token for mutating HTTP methods (POST, PATCH, PUT, DELETE).
// Read methods (GET, HEAD, OPTIONS) pass through unconditionally.
//   - writeToken set: mutations must provide it
//   - writeToken empty + authToken set: mutations blocked (read-only mode)
//   - both empty: all methods pass through (no auth configured)
func WriteAuth(writeToken, authToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if writeToken == "" && authToken == "" {
				next.ServeHTTP(w, r) // no auth at all
				return
			}

			switch r.Method {
			case "GET", "HEAD", "OPTIONS":
				next.ServeHTTP(w, r)
				return
			}

			if writeToken == "" {
				// Auth enabled but no WRITE_TOKEN → read-only
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require WRITE_TOKEN")
				return
			}

			provided := extractBearerToken(r)
			if subtle.ConstantTimeCompare([]byte(provided), []byte(writeToken)) != 1 {
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require WRITE_TOKEN")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
