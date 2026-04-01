package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/time/rate"
)

// Context keys for auth info
type contextKey int

const (
	ctxKeyUserID   contextKey = iota
	ctxKeyUsername
	ctxKeyRole
	ctxKeyAuthType
)

// ContextUserID returns the authenticated user's ID from the request context, or 0.
func ContextUserID(r *http.Request) int {
	if v, ok := r.Context().Value(ctxKeyUserID).(int); ok {
		return v
	}
	return 0
}

// ContextUsername returns the authenticated user's username from the request context.
func ContextUsername(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyUsername).(string); ok {
		return v
	}
	return ""
}

// ContextRole returns the authenticated user's role from the request context.
func ContextRole(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyRole).(string); ok {
		return v
	}
	return ""
}

// ContextAuthType returns "jwt" or "token" depending on how the request was authenticated.
func ContextAuthType(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyAuthType).(string); ok {
		return v
	}
	return ""
}

// setAuthContext returns a new request with auth info in the context.
func setAuthContext(r *http.Request, userID int, username, role, authType string) *http.Request {
	ctx := r.Context()
	ctx = context.WithValue(ctx, ctxKeyUserID, userID)
	ctx = context.WithValue(ctx, ctxKeyUsername, username)
	ctx = context.WithValue(ctx, ctxKeyRole, role)
	ctx = context.WithValue(ctx, ctxKeyAuthType, authType)
	return r.WithContext(ctx)
}

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
				// Echo back the request origin instead of * to support credentials.
				// * is incompatible with Access-Control-Allow-Credentials: true.
				if origin != "" {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			} else if allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
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

// JWTOrTokenAuth authenticates requests via JWT, API keys, or legacy bearer tokens.
// Resolution order:
//  1. JWT (token contains two dots) → parse claims for user_id, role
//  2. API key (starts with "tre_") → hash lookup in DB for role
//  3. Legacy WRITE_TOKEN → role=admin
//  4. Legacy AUTH_TOKEN → role=viewer
//
// If no JWT secret and no tokens are configured, all requests pass through.
func JWTOrTokenAuth(jwtSecret []byte, writeToken, authToken string, db ...apiKeyResolver) func(http.Handler) http.Handler {
	hasTokens := writeToken != "" || authToken != ""
	var keyDB apiKeyResolver
	if len(db) > 0 {
		keyDB = db[0]
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No auth configured at all — pass through
			if len(jwtSecret) == 0 && !hasTokens {
				next.ServeHTTP(w, r)
				return
			}

			provided := extractBearerToken(r)
			if provided == "" {
				WriteError(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			// JWT path: token contains two dots (header.payload.signature)
			if len(jwtSecret) > 0 && strings.Count(provided, ".") == 2 {
				claims := &Claims{}
				token, err := jwt.ParseWithClaims(provided, claims, jwtKeyFunc(jwtSecret))
				if err == nil && token.Valid {
					userID, _ := strconv.Atoi(claims.Subject)
					r = setAuthContext(r, userID, claims.Username, claims.Role, "jwt")
					next.ServeHTTP(w, r)
					return
				}
				// JWT parse failed — fall through to API key / legacy token check
			}

			// API key path: starts with "tre_" prefix
			if keyDB != nil && strings.HasPrefix(provided, "tre_") {
				key, err := keyDB.ResolveAPIKey(r.Context(), provided)
				if err == nil && key != nil {
					userID := 0
					if key.UserID != nil {
						userID = *key.UserID
					}
					r = setAuthContext(r, userID, key.Label, key.Role, "api_key")
					// Best-effort touch last_used_at (don't block request)
					go keyDB.TouchAPIKey(context.Background(), key.ID)
					next.ServeHTTP(w, r)
					return
				}
			}

			// Legacy token path: check write token first (admin), then auth token (viewer)
			if writeToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(writeToken)) == 1 {
				r = setAuthContext(r, 0, "", "admin", "token")
				next.ServeHTTP(w, r)
				return
			}
			if authToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(authToken)) == 1 {
				r = setAuthContext(r, 0, "", "viewer", "token")
				next.ServeHTTP(w, r)
				return
			}

			WriteError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}

// apiKeyResolver is the interface needed for API key lookup in middleware.
// Satisfied by *database.DB.
type apiKeyResolver interface {
	ResolveAPIKey(ctx context.Context, plaintext string) (*database.APIKey, error)
	TouchAPIKey(ctx context.Context, id int) error
}

// EditorOrAbove requires the caller to have editor or admin role.
func EditorOrAbove(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := ContextRole(r)
		if RoleLevel(role) < RoleLevel("editor") {
			WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "editor or admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteAuth requires the write token for mutating HTTP methods (POST, PATCH, PUT, DELETE).
// WriteAuth gates mutating HTTP methods (POST, PATCH, PUT, DELETE).
// Read methods (GET, HEAD, OPTIONS) always pass through.
//
// Logic for writes:
//  1. If no auth at all (no tokens, no JWT) → pass through (open mode)
//  2. If caller has a role from JWTOrTokenAuth → check editor+ role
//  3. Legacy fallback: check WRITE_TOKEN (deprecated)
func WriteAuth(writeToken, authToken string, jwtEnabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No auth configured at all — open mode
			if writeToken == "" && authToken == "" && !jwtEnabled {
				next.ServeHTTP(w, r)
				return
			}

			switch r.Method {
			case "GET", "HEAD", "OPTIONS":
				next.ServeHTTP(w, r)
				return
			}

			// Check role from context (set by JWTOrTokenAuth)
			role := ContextRole(r)
			if role != "" {
				if RoleLevel(role) >= RoleLevel("editor") {
					next.ServeHTTP(w, r)
					return
				}
				// viewer role → forbidden
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "insufficient permissions for write operations")
				return
			}

			// No role in context — if JWT is enabled, this means caller is
			// unauthenticated or used a read-only token. Reject.
			if jwtEnabled {
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require login with editor or admin role")
				return
			}

			// Legacy fallback: WRITE_TOKEN (deprecated path)
			if writeToken == "" {
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

// AuthRateLimiter limits login/setup attempts to 5 per minute per IP.
// Separate from the global RateLimiter to avoid locking out legitimate API usage.
func AuthRateLimiter() func(http.Handler) http.Handler {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)
	// Clean stale entries every 5 minutes
	go func() {
		for range time.Tick(5 * time.Minute) {
			mu.Lock()
			for ip, lim := range limiters {
				if lim.Tokens() >= 5 { // fully replenished = idle
					delete(limiters, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			mu.Lock()
			lim, ok := limiters[ip]
			if !ok {
				// 5 requests per 60 seconds, burst of 5
				lim = rate.NewLimiter(rate.Every(12*time.Second), 5)
				limiters[ip] = lim
			}
			mu.Unlock()

			if !lim.Allow() {
				w.Header().Set("Retry-After", "60")
				WriteErrorWithCode(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts, try again in 60 seconds")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AdminOnly rejects requests from non-admin users and logs denied attempts.
func AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RoleLevel(ContextRole(r)) < RoleLevel("admin") {
			log := hlog.FromRequest(r)
			log.Warn().
				Str("path", r.URL.Path).
				Str("username", ContextUsername(r)).
				Int("user_id", ContextUserID(r)).
				Str("role", ContextRole(r)).
				Msg("admin access denied")
			WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
