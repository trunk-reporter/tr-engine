package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// The request pipeline (§6.2) runs on the root router, in this order:
// RequestID, CORS, GetHead, Recoverer, Logger, APIHeaders, Match, Resolve
// (principal resolution with rate limiting), Authorize and Audit. Route-group
// middleware (MaxBodySize, the upload middleware, ResponseTimeout) follows.

type ctxKey int

const (
	ctxRoute ctxKey = iota
	ctxPrincipal
	ctxKeyRecord
)

// routeMatch is the route a request matched and its policy.
type routeMatch struct {
	Pattern string      // e.g. "/api/v1/calls/{id}"
	Key     string      // "METHOD pattern" as in routePolicies; HEAD uses the GET entry
	Policy  RoutePolicy // the route's policy
}

// routeFrom returns the route Match stored, or nil for an unmatched request.
func routeFrom(r *http.Request) *routeMatch {
	m, _ := r.Context().Value(ctxRoute).(*routeMatch)
	return m
}

// PrincipalFrom returns the request's principal, or nil if none was resolved
// (unmatched routes, and handlers called directly in tests). A nil principal
// is allowed nothing.
func PrincipalFrom(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(ctxPrincipal).(*auth.Principal)
	return p
}

// keyRecordFrom returns the API key behind a key or ticket principal.
func keyRecordFrom(r *http.Request) *database.APIKey {
	k, _ := r.Context().Value(ctxKeyRecord).(*database.APIKey)
	return k
}

func withPrincipal(r *http.Request, p *auth.Principal, k *database.APIKey) *http.Request {
	ctx := context.WithValue(r.Context(), ctxPrincipal, p)
	if k != nil {
		ctx = context.WithValue(ctx, ctxKeyRecord, k)
	}
	return r.WithContext(ctx)
}

// findRoute returns the pattern chi dispatches method and path to, or "".
// HEAD falls back to GET when there is no HEAD route (middleware.GetHead);
// the returned method is the one the pattern was found under. Find mutates
// the context it is given, so each lookup gets a fresh one.
func findRoute(root *chi.Mux, method, path string) (string, string) {
	if pattern := root.Find(chi.NewRouteContext(), method, path); pattern != "" {
		return method, pattern
	}
	if method == http.MethodHead {
		if pattern := root.Find(chi.NewRouteContext(), http.MethodGet, path); pattern != "" {
			return http.MethodGet, pattern
		}
	}
	return method, ""
}

// routingPath is the path chi routes on: the raw (still escaped) path when
// the URL has one, so that %2F stays inside a parameter.
func routingPath(r *http.Request) string {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		path = "/"
	}
	return path
}

// Match finds the route root will dispatch the request to and stores it with
// its policy (§6.2 step 4). An unmatched request is passed on untouched:
// chi answers it with 404 or 405 and runs no handler. A matched route
// without a policy fails closed with 403.
func Match(root *chi.Mux, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, pattern := findRoute(root, r.Method, routingPath(r))
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}
			key := method + " " + pattern
			pol, ok := routePolicies[key]
			if !ok {
				log.Error().Str("route", key).Msg("route has no auth policy")
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "route has no auth policy")
				return
			}
			if privateCacheRoutes[key] {
				w.Header().Set("Cache-Control", "private")
			}
			ctx := context.WithValue(r.Context(), ctxRoute, &routeMatch{Pattern: pattern, Key: key, Policy: pol})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Resolve establishes the principal of a matched request (§3.3), with rate
// limiting interleaved (§8). A presented but invalid credential is refused;
// it never falls back to anonymous.
func (a *authenticator) Resolve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := routeFrom(r)
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		res, aerr := a.resolve(r, m.Policy)
		if aerr != nil {
			a.refuse(w, r, m, aerr)
			return
		}
		next.ServeHTTP(w, withPrincipal(r, res.principal, res.key))
	})
}

// Authorize applies the route's policy to the principal (§6.2 step 6).
func (a *authenticator) Authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := routeFrom(r)
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		if aerr := authorize(m.Policy, PrincipalFrom(r)); aerr != nil {
			a.refuse(w, r, m, aerr)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize decides whether p may call a route with policy pol, and returns
// the refusal if not.
func authorize(pol RoutePolicy, p *auth.Principal) *authError {
	anonymous := p == nil || p.Kind == auth.KindAnonymous
	switch {
	case pol.Scope == "":
		return nil
	case pol.FormKey && anonymous:
		// No header credential: the upload middleware reads the key from the
		// multipart form and decides.
		return nil
	case anonymous && pol.KeyRequired:
		return &authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
			msg: "an API key is required for this endpoint", uploadReason: "no key"}
	case anonymous && pol.Scope != auth.ScopeListen:
		return &authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
			msg: fmt.Sprintf("an API key with the %s scope is required", pol.Scope), uploadReason: "no key"}
	case anonymous && !p.Has(auth.ScopeListen):
		return &authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
			msg: "an API key is required: anonymous access is off"}
	case !p.Has(pol.Scope):
		return &authError{status: http.StatusForbidden, code: ErrInsufficientScope,
			msg:          fmt.Sprintf("this credential lacks the %s scope this endpoint needs", pol.Scope),
			uploadReason: fmt.Sprintf("key #%d lacks upload", p.KeyID)}
	case p.Restricted() && pol.Restricted == Deny:
		return &authError{status: http.StatusForbidden, code: ErrRestrictedCredential,
			msg: "this credential is restricted to some systems or talkgroups, and this endpoint can't enforce that"}
	}
	return nil
}

// refuse writes an auth refusal, and logs rejected uploads (§11.2).
func (a *authenticator) refuse(w http.ResponseWriter, r *http.Request, m *routeMatch, aerr *authError) {
	if m.Policy.FormKey {
		// The body is not parsed here, so the form's system is unknown.
		a.warnUploadRejected(a.proxies.ClientIP(r), "", "", aerr.uploadReason)
	}
	aerr.write(w)
}

// Audit records state-changing requests made with an API key (§9): matched
// routes, methods other than GET, HEAD and OPTIONS, except call upload and
// ticket minting. It sits outside ResponseTimeout, so it records the status
// the client received, including refusals from inside handlers.
func (a *authenticator) Audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := routeFrom(r)
		p := PrincipalFrom(r)
		if m == nil || p == nil || p.Kind != auth.KindKey || auditSkipRoutes[m.Key] {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		aw := &auditWriter{ResponseWriter: w, status: http.StatusOK}
		panicked := true
		defer func() {
			status := aw.status
			if panicked && !aw.wrote {
				status = http.StatusInternalServerError // what Recoverer sends
			}
			a.recordAudit(r, p, status, w.Header().Get("X-Request-ID"))
		}()
		next.ServeHTTP(aw, r)
		panicked = false
	})
}

func (a *authenticator) recordAudit(r *http.Request, p *auth.Principal, status int, requestID string) {
	actor := p.Actor
	e := database.AuditEntry{
		KeyID:     p.KeyID,
		KeyName:   p.KeyName,
		Actor:     &actor,
		Method:    r.Method,
		Path:      r.URL.EscapedPath(), // as requested, without the query
		Status:    status,
		RequestID: requestID,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := a.store.InsertAuditLog(ctx, e); err != nil {
		hlog.FromRequest(r).Error().Err(err).Str("path", e.Path).Int("key_id", e.KeyID).
			Msg("audit log: recording a request failed")
	}
}

// auditWriter captures the status code of an audited response. It keeps
// Flush, Hijack and Unwrap working for streaming handlers.
type auditWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *auditWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *auditWriter) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *auditWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("wrapped ResponseWriter does not implement http.Hijacker")
}

func (w *auditWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// uploadMaxMemory is how much of a multipart upload is kept in memory; the
// rest spills to temporary files (the route's MaxBodySize caps the total).
const uploadMaxMemory = 32 << 20

// UploadAuth authenticates POST /call-upload (§5). A Bearer header
// principal was already resolved and authorized. Without one, the key comes
// from the multipart fields key, then api_key, read from the parsed
// multipart body only (never the URL or FormValue). A present but invalid key
// is 401 invalid_key without trying the other field. The key needs the upload
// scope. Every rejection is logged, at most once a minute per client IP.
func (a *authenticator) UploadAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := PrincipalFrom(r); p != nil && p.Kind == auth.KindKey {
			next.ServeHTTP(w, r)
			return
		}
		ip := a.proxies.ClientIP(r)
		var system, shortName string
		reject := func(aerr *authError) {
			if r.MultipartForm != nil {
				r.MultipartForm.RemoveAll()
			}
			a.warnUploadRejected(ip, system, shortName, aerr.uploadReason)
			aerr.write(w)
		}

		if err := r.ParseMultipartForm(uploadMaxMemory); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				a.warnUploadRejected(ip, "", "", "request body too large")
				WriteErrorWithCode(w, http.StatusRequestEntityTooLarge, ErrBadRequest, "request body too large")
				return
			}
			reject(&authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
				msg: "an API key with the upload scope is required", uploadReason: "no key (not a multipart form)"})
			return
		}
		system = formField(r, "system")
		shortName = formField(r, "shortName")

		token := formField(r, "key")
		if token == "" {
			token = formField(r, "api_key")
		}
		if token == "" {
			reject(&authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
				msg: "an API key with the upload scope is required", uploadReason: "no key"})
			return
		}
		k, aerr := a.resolveKey(r.Context(), token, ip)
		if aerr != nil {
			reject(aerr)
			return
		}
		if k == nil {
			// The retired public AUTH_TOKEN counts as no credential.
			reject(&authError{status: http.StatusUnauthorized, code: ErrKeyRequired,
				msg: "an API key with the upload scope is required", uploadReason: "no key (the retired public AUTH_TOKEN)"})
			return
		}
		p := keyPrincipal(k, auth.SanitizeActor(r.Header.Get("X-Actor")))
		if !p.Has(auth.ScopeUpload) {
			reject(&authError{status: http.StatusForbidden, code: ErrInsufficientScope,
				msg: "this credential lacks the upload scope this endpoint needs", uploadReason: fmt.Sprintf("key #%d lacks upload", k.ID)})
			return
		}
		next.ServeHTTP(w, withPrincipal(r, p, k))
	})
}

// formField returns the first value of a multipart field, from the parsed
// multipart body only.
func formField(r *http.Request, name string) string {
	if r.MultipartForm == nil {
		return ""
	}
	if v := r.MultipartForm.Value[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}
