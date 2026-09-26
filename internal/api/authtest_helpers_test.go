package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// stubAuthStore is an in-memory authStore.
type stubAuthStore struct {
	mu       sync.Mutex
	byHash   map[string]*database.APIKey
	byID     map[int]*database.APIKey
	anon     database.AnonymousAccess
	retired  string
	secret   []byte
	merged   map[int]bool
	audit    []database.AuditEntry
	touched  []int
	lookups  int   // ResolveAPIKeyByHash calls
	idReads  int   // GetAPIKeyByID calls
	failWith error // returned by every key lookup when set
	nextID   int
}

func newStubAuthStore() *stubAuthStore {
	return &stubAuthStore{
		byHash: make(map[string]*database.APIKey),
		byID:   make(map[int]*database.APIKey),
		anon:   database.AnonymousAccess{Access: database.AccessOff},
		secret: []byte(strings.Repeat("s", 32)),
		merged: make(map[int]bool),
	}
}

// add stores a key for plaintext and returns the stored record (the stub
// keeps the pointer, so tests can change it in place under mu).
func (s *stubAuthStore) add(plaintext string, k database.APIKey) *database.APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.ID == 0 {
		s.nextID++
		k.ID = 1000 + s.nextID
	}
	if k.Name == "" {
		k.Name = "key " + plaintext
	}
	if k.Prefix == "" {
		k.Prefix = database.HashAPIKey(plaintext)[:12]
	}
	k.Status = k.StatusAt(time.Now())
	rec := &k
	s.byHash[database.HashAPIKey(plaintext)] = rec
	s.byID[k.ID] = rec
	return rec
}

func (s *stubAuthStore) copyKey(k *database.APIKey) *database.APIKey {
	c := *k
	c.Status = c.StatusAt(time.Now())
	return &c
}

func (s *stubAuthStore) ResolveAPIKeyByHash(_ context.Context, hash string) (*database.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups++
	if s.failWith != nil {
		return nil, s.failWith
	}
	if k, ok := s.byHash[hash]; ok {
		return s.copyKey(k), nil
	}
	return nil, database.ErrAPIKeyNotFound
}

func (s *stubAuthStore) GetAPIKeyByID(_ context.Context, id int) (*database.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idReads++
	if s.failWith != nil {
		return nil, s.failWith
	}
	if k, ok := s.byID[id]; ok {
		return s.copyKey(k), nil
	}
	return nil, database.ErrAPIKeyNotFound
}

func (s *stubAuthStore) TouchAPIKey(_ context.Context, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, id)
	return nil
}

func (s *stubAuthStore) GetAnonymousAccess(context.Context) (database.AnonymousAccess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anon, nil
}

func (s *stubAuthStore) GetRetiredPublicTokenHash(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retired, nil
}

func (s *stubAuthStore) GetOrCreateTicketSecret(context.Context) ([]byte, error) {
	return s.secret, nil
}

func (s *stubAuthStore) AnyMergedAwaySystem(_ context.Context, ids []int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if s.merged[id] {
			return true, nil
		}
	}
	return false, nil
}

func (s *stubAuthStore) InsertAuditLog(_ context.Context, e database.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	return nil
}

func (s *stubAuthStore) setAnonymous(access string, r *auth.Restriction) {
	s.mu.Lock()
	s.anon = database.AnonymousAccess{Access: access, Restriction: r}
	s.mu.Unlock()
	auth.Bump() // as SetAnonymousAccess does
}

func (s *stubAuthStore) auditEntries() []database.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]database.AuditEntry(nil), s.audit...)
}

// newTestAuthenticator returns an authenticator over store with per-IP
// limits no test reaches (rate limiting has its own tests).
func newTestAuthenticator(store authStore) *authenticator {
	return newAuthenticator(store, nil, 1e9, 1<<30, zerolog.Nop())
}

// allFeaturesOptions are router options with a stub for every optional
// dependency, so buildRouter registers every conditional route.
func allFeaturesOptions(a *authenticator) routerOptions {
	return routerOptions{
		ServerOptions: ServerOptions{
			Config: &config.Config{
				MetricsEnabled:     true,
				WriteTimeout:       5 * time.Second,
				UploadInstanceID:   "http-upload",
				StreamMaxClients:   5,
				DebugReportDisable: true,
			},
			Uploader:      &mockCallUploader{},
			AudioStreamer: &mockAudioStreamer{bus: audio.NewAudioBus(), enabled: true},
			WebFiles:      fstest.MapFS{"web/index.html": {Data: []byte("<html>tr-engine</html>")}},
			OpenAPISpec:   []byte("openapi: 3.0.3\n"),
			Version:       "v9.9.9 (commit=test, built=now)",
			StartTime:     time.Now(),
			Log:           zerolog.Nop(),
		},
		authn: a,
	}
}

// walkRoutes returns every "METHOD pattern" chi.Walk finds on r.
func walkRoutes(t *testing.T, r *chi.Mux) map[string]bool {
	t.Helper()
	routes := make(map[string]bool)
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return routes
}

// reachedHeader marks a response written by a shadow router's route handler,
// i.e. a request the auth pipeline let through.
const reachedHeader = "X-Test-Reached"

// shadowRouter has exactly the routes of buildRouter(all features), each
// answered by a handler that records what chi dispatched to, behind the same
// root pipeline (and the upload middleware on the upload route). It tests
// auth decisions without running the real handlers.
func shadowRouter(t *testing.T, a *authenticator) *chi.Mux {
	t.Helper()
	real := buildRouter(allFeaturesOptions(a))
	shadow := chi.NewRouter()
	shadow.Use(RequestID, CORS, middleware.GetHead, APIHeaders, Match(shadow, zerolog.Nop()), a.Resolve, a.Authorize, a.Audit)
	for route := range walkRoutes(t, real) {
		method, pattern, _ := strings.Cut(route, " ")
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(reachedHeader, chi.RouteContext(r.Context()).RoutePattern())
			if m := routeFrom(r); m != nil {
				w.Header().Set("X-Test-Matched", m.Pattern)
			}
			if p := PrincipalFrom(r); p != nil {
				w.Header().Set("X-Test-Principal", string(p.Kind))
			}
			w.WriteHeader(http.StatusOK)
		})
		if routePolicies[route].FormKey {
			h = MaxBodySize(1 << 20)(a.UploadAuth(h))
		}
		shadow.Method(method, pattern, h)
	}
	return shadow
}

// do sends a request through h and returns the recorder.
func do(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bearer(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key}
}

// errorCode returns the "code" of a JSON error body.
func errorCode(rec *httptest.ResponseRecorder) string {
	body := rec.Body.String()
	i := strings.Index(body, `"code":"`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"code":"`):]
	return rest[:strings.IndexByte(rest, '"')]
}

var errStubDown = errors.New("database is down")

// withRouteForTest stores a matched route in the request, as Match does.
func withRouteForTest(r *http.Request, m *routeMatch) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxRoute, m))
}
