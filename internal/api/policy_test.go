package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
	"go.yaml.in/yaml/v2"
)

// TestRoutePolicyTable checks that every route buildRouter registers (with
// every optional feature) has a policy, and every policy is a real route.
func TestRoutePolicyTable(t *testing.T) {
	r := buildRouter(allFeaturesOptions(newTestAuthenticator(newStubAuthStore())))
	routes := walkRoutes(t, r)
	if len(routes) < 90 {
		t.Fatalf("only %d routes registered; expected every conditional route too", len(routes))
	}
	for route := range routes {
		if _, ok := routePolicies[route]; !ok {
			t.Errorf("route %s has no entry in routePolicies", route)
		}
	}
	for route := range routePolicies {
		if !routes[route] {
			t.Errorf("routePolicies entry %s is not a registered route", route)
		}
	}
	for route := range auditSkipRoutes {
		if _, ok := routePolicies[route]; !ok {
			t.Errorf("auditSkipRoutes entry %s is not in the policy table", route)
		}
	}
	for route := range privateCacheRoutes {
		if _, ok := routePolicies[route]; !ok {
			t.Errorf("privateCacheRoutes entry %s is not in the policy table", route)
		}
	}
}

// enforceableRoutes are the routes §6.3 lists as "listen, Restricted =
// Enforced". Nothing else may be Enforced.
var enforceableRoutes = map[string]bool{
	"GET /api/v1/systems":                   true,
	"GET /api/v1/systems/{id}":              true,
	"GET /api/v1/sites/{id}":                true,
	"GET /api/v1/talkgroups":                true,
	"GET /api/v1/talkgroups/{id}":           true,
	"GET /api/v1/talkgroups/{id}/calls":     true,
	"GET /api/v1/talkgroup-directory":       true,
	"GET /api/v1/calls":                     true,
	"GET /api/v1/calls/active":              true,
	"GET /api/v1/calls/{id}":                true,
	"GET /api/v1/calls/{id}/audio":          true,
	"GET /api/v1/calls/{id}/frequencies":    true,
	"GET /api/v1/calls/{id}/transmissions":  true,
	"GET /api/v1/calls/{id}/transcription":  true,
	"GET /api/v1/calls/{id}/transcriptions": true,
	"GET /api/v1/call-groups":               true,
	"GET /api/v1/call-groups/{id}":          true,
	"GET /api/v1/transcriptions/search":     true,
	"GET /api/v1/transcriptions/batch":      true,
	"GET /api/v1/events/stream":             true,
	"GET /api/v1/audio/live":                true,
	"POST /api/v1/tickets":                  true,
}

// TestRoutePolicyInvariants checks properties the spec fixes for the whole
// table.
func TestRoutePolicyInvariants(t *testing.T) {
	ticketRoutes := map[string]bool{
		"GET /api/v1/events/stream":    true,
		"GET /api/v1/audio/live":       true,
		"GET /api/v1/calls/{id}/audio": true,
	}
	for route, pol := range routePolicies {
		method, _, _ := strings.Cut(route, " ")
		if pol.Scope != "" && !pol.Scope.Valid() {
			t.Errorf("%s: unknown scope %q", route, pol.Scope)
		}
		if pol.Ticket != ticketRoutes[route] {
			t.Errorf("%s: Ticket = %v; only the three stream/audio routes honour tickets", route, pol.Ticket)
		}
		if pol.Ticket && method != http.MethodGet {
			t.Errorf("%s: tickets are GET/HEAD only", route)
		}
		if pol.FormKey != (route == "POST /api/v1/call-upload") {
			t.Errorf("%s: FormKey = %v; only call upload reads form keys", route, pol.FormKey)
		}
		if pol.Scope == auth.ScopeUpload && route != "POST /api/v1/call-upload" {
			t.Errorf("%s: the upload scope grants call upload only", route)
		}
		// §6.3: exactly the listen routes it lists as Enforced are Enforced,
		// the two stream routes included; every other route is Deny.
		// edit/admin/upload/public routes never enforce (restricted
		// principals never hold those scopes).
		wantEnforced := enforceableRoutes[route]
		if got := pol.Restricted == Enforced; got != wantEnforced {
			t.Errorf("%s: Restricted = %s; §6.3 has it %s", route, pol.Restricted, map[bool]Mode{true: Enforced, false: Deny}[wantEnforced])
		}
		if pol.Restricted == Enforced && pol.Scope != auth.ScopeListen {
			t.Errorf("%s: Enforced route with scope %q; only listen routes enforce restrictions", route, pol.Scope)
		}
		if pol.Scope == "" && (pol.KeyRequired || pol.Ticket || pol.FormKey) {
			t.Errorf("%s: a public route has no other flags", route)
		}
	}
	for route := range enforceableRoutes {
		if _, ok := routePolicies[route]; !ok {
			t.Errorf("§6.3 Enforced route %s is not in the policy table", route)
		}
	}
	for route, want := range map[string]RoutePolicy{
		"GET /metrics":                 {Scope: auth.ScopeListen, KeyRequired: true},
		"POST /api/v1/tickets":         {Scope: auth.ScopeListen, Restricted: Enforced, KeyRequired: true},
		"GET /api/v1/calls/{id}/audio": {Scope: auth.ScopeListen, Restricted: Enforced, Ticket: true},
		"GET /api/v1/events/stream":    {Scope: auth.ScopeListen, Restricted: Enforced, Ticket: true},
		"GET /api/v1/audio/live":       {Scope: auth.ScopeListen, Restricted: Enforced, Ticket: true},
		"GET /api/v1/whoami":           {},
		"GET /*":                       {},
	} {
		if got := routePolicies[route]; got != want {
			t.Errorf("%s = %+v, want %+v", route, got, want)
		}
	}
	for _, route := range []string{"POST /api/v1/debug-report", "POST /api/v1/query", "POST /api/v1/pages",
		"GET /api/v1/console-messages", "POST /api/v1/admin/systems/merge", "GET /api/v1/admin/maintenance",
		"PATCH /api/v1/systems/{id}", "PATCH /api/v1/sites/{id}", "POST /api/v1/unit-tags/import",
		"POST /api/v1/talkgroup-directory/import", "GET /api/v1/admin/transcribe-backfill"} {
		if routePolicies[route].Scope != auth.ScopeAdmin {
			t.Errorf("%s must need admin (§6.3)", route)
		}
	}
}

var paramPattern = regexp.MustCompile(`\{[^}]+\}`)

// synthesize turns a route pattern into a concrete path, replacing each
// {param} with value and a trailing * with a two-segment path.
func synthesize(pattern, value string) string {
	p := paramPattern.ReplaceAllString(pattern, value)
	if strings.HasSuffix(p, "*") {
		p = strings.TrimSuffix(p, "*") + "zz/yy.html"
	}
	return p
}

// TestFindMatchesWalk checks that Mux.Find, which Match uses, returns for a
// concrete path of every route exactly the pattern chi.Walk reports.
func TestFindMatchesWalk(t *testing.T) {
	r := buildRouter(allFeaturesOptions(newTestAuthenticator(newStubAuthStore())))
	for route := range walkRoutes(t, r) {
		method, pattern, _ := strings.Cut(route, " ")
		path := synthesize(pattern, "123")
		if got := r.Find(chi.NewRouteContext(), method, path); got != pattern {
			t.Errorf("Find(%s %s) = %q, want %q", method, path, got, pattern)
		}
		if method == http.MethodGet {
			if m, got := findRoute(r, http.MethodHead, path); got != pattern || m != http.MethodGet {
				t.Errorf("HEAD %s: findRoute = %s %q, want GET %q", path, m, got, pattern)
			}
		}
	}
}

// TestEncodedSlashConsistency sends every {param} route a parameter with
// %2F in it and checks that the pattern Match chose (and so the policy it
// applied) is the pattern chi actually dispatched to.
func TestEncodedSlashConsistency(t *testing.T) {
	store := newStubAuthStore()
	store.add("admin-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}})
	shadow := shadowRouter(t, newTestAuthenticator(store))
	checked := 0
	for route := range routePolicies {
		method, pattern, _ := strings.Cut(route, " ")
		if !strings.Contains(pattern, "{") {
			continue
		}
		for _, value := range []string{"a%2Fb", "1%2F..%2Fkeys", "%2F", "x%252Fy"} {
			path := synthesize(pattern, value)
			rec := do(shadow, method, path, bearer("admin-key"))
			dispatched := rec.Header().Get(reachedHeader)
			matched := rec.Header().Get("X-Test-Matched")
			if dispatched == "" {
				// chi ran no handler; Match must not have claimed a route
				// either (or refused before dispatch).
				if matched != "" {
					t.Errorf("%s %s: matched %q but chi dispatched nothing (%d)", method, path, matched, rec.Code)
				}
				continue
			}
			checked++
			if matched != dispatched {
				t.Errorf("%s %s: Match chose %q but chi dispatched to %q", method, path, matched, dispatched)
			}
			if routePolicies[method+" "+matched] != routePolicies[method+" "+dispatched] {
				t.Errorf("%s %s: policies differ", method, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no request reached a handler")
	}
}

// TestMatchRefusesRouteWithoutPolicy checks the fail-closed default: a
// matched route that is missing from the table is 403 and never runs.
func TestMatchRefusesRouteWithoutPolicy(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Match(r, zerolog.Nop()))
	ran := false
	r.Get("/api/v1/not-in-the-table", func(w http.ResponseWriter, r *http.Request) { ran = true })
	rec := do(r, "GET", "/api/v1/not-in-the-table", nil)
	if rec.Code != http.StatusForbidden || errorCode(rec) != ErrForbidden || ran {
		t.Errorf("got %d %s (ran=%v), want 403 forbidden without running the handler", rec.Code, rec.Body.String(), ran)
	}
	// Unmatched requests pass through to chi's 404.
	if rec := do(r, "GET", "/nowhere", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unmatched: got %d, want 404", rec.Code)
	}
}

// TestRouterBasics runs requests through the real router with stub
// dependencies.
func TestRouterBasics(t *testing.T) {
	store := newStubAuthStore()
	store.add("admin-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
	r := buildRouter(allFeaturesOptions(newTestAuthenticator(store)))

	t.Run("removed endpoints are 404 not_found", func(t *testing.T) {
		for _, req := range []struct{ method, path string }{
			{"GET", "/api/v1/auth-init"},
			{"POST", "/api/v1/auth/login"},
			{"POST", "/api/v1/auth/refresh"},
			{"POST", "/api/v1/auth/logout"},
			{"GET", "/api/v1/auth/setup"},
			{"POST", "/api/v1/auth/setup"},
			{"GET", "/api/v1/auth/me"},
			{"GET", "/api/v1/auth/keys"},
			{"DELETE", "/api/v1/auth/keys/3/any"},
			{"GET", "/api/v1/users"},
			{"PATCH", "/api/v1/users/2"},
		} {
			for _, headers := range []map[string]string{nil, bearer("admin-key")} {
				rec := do(r, req.method, req.path, headers)
				if rec.Code != http.StatusNotFound || errorCode(rec) != ErrNotFound {
					t.Errorf("%s %s: got %d %q, want 404 not_found", req.method, req.path, rec.Code, rec.Body.String())
				}
			}
		}
	})

	t.Run("wrong method on a real route is 405", func(t *testing.T) {
		rec := do(r, "PUT", "/api/v1/whoami", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("got %d, want 405", rec.Code)
		}
	})

	t.Run("HEAD health is 200 without a key", func(t *testing.T) {
		rec := do(r, "HEAD", "/api/v1/health", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("got %d, want 200", rec.Code)
		}
	})

	t.Run("health body is trimmed unless the key has unrestricted listen", func(t *testing.T) {
		rec := do(r, "GET", "/api/v1/health", nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "uptime_seconds") ||
			!strings.Contains(rec.Body.String(), `"checks"`) {
			t.Errorf("anonymous: %d %s", rec.Code, rec.Body.String())
		}
		rec = do(r, "GET", "/api/v1/health", bearer("admin-key"))
		if !strings.Contains(rec.Body.String(), "uptime_seconds") {
			t.Errorf("admin key: %s, want the full body", rec.Body.String())
		}
	})

	t.Run("OPTIONS is 204 with CORS headers on any path", func(t *testing.T) {
		for _, path := range []string{"/api/v1/keys", "/api/v1/auth-init", "/metrics"} {
			rec := do(r, "OPTIONS", path, map[string]string{"Origin": "https://x.example"})
			if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" ||
				rec.Header().Get("Access-Control-Allow-Methods") == "" || rec.Header().Get("Access-Control-Max-Age") != "600" {
				t.Errorf("OPTIONS %s: %d %v", path, rec.Code, rec.Header())
			}
		}
	})

	t.Run("401 carries WWW-Authenticate and no-store", func(t *testing.T) {
		rec := do(r, "GET", "/api/v1/systems", nil)
		if rec.Code != http.StatusUnauthorized || errorCode(rec) != ErrKeyRequired {
			t.Fatalf("got %d %s, want 401 key_required", rec.Code, rec.Body.String())
		}
		h := rec.Header()
		if h.Get("WWW-Authenticate") != `Bearer realm="tr-engine"` || h.Get("Cache-Control") != "no-store" ||
			h.Get("Vary") != "Authorization" || h.Get("Access-Control-Allow-Origin") != "*" ||
			h.Get("X-Request-ID") == "" {
			t.Errorf("headers = %v", h)
		}
	})

	t.Run("keys are never read from the URL", func(t *testing.T) {
		for _, q := range []string{"token", "key", "api_key", "access_token"} {
			rec := do(r, "GET", "/api/v1/systems?"+q+"=admin-key", nil)
			if rec.Code != http.StatusUnauthorized || errorCode(rec) != ErrKeyRequired {
				t.Errorf("?%s=: got %d %s, want 401 key_required", q, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("static files and public routes", func(t *testing.T) {
		if rec := do(r, "GET", "/", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "tr-engine") {
			t.Errorf("GET /: %d %s", rec.Code, rec.Body.String())
		}
		if rec := do(r, "GET", "/api/v1/openapi.yaml", nil); rec.Code != http.StatusOK {
			t.Errorf("openapi: %d", rec.Code)
		}
		if rec := do(r, "GET", "/metrics", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("metrics without a key: %d, want 401", rec.Code)
		}
	})

	t.Run("audio is cache-control private", func(t *testing.T) {
		rec := do(r, "GET", "/api/v1/calls/5/audio", nil)
		if rec.Header().Get("Cache-Control") != "private" {
			t.Errorf("Cache-Control = %q, want private", rec.Header().Get("Cache-Control"))
		}
	})
}

// principalCase is one kind of caller in the authorization matrix.
type principalCase struct {
	name    string
	headers map[string]string
	ticket  string // appended as ?ticket=
	// Outcome inputs for the oracle.
	anonymous  bool        // resolves to the anonymous principal (unless a ticket applies)
	invalid    string      // non-empty: every route answers 401 with this code
	scopes     auth.Scopes // key principals
	restricted bool
	// ticketOK: on ticket routes the ticket gives a listen principal with
	// ticketRestricted; ticketInvalid: 401 invalid_ticket there.
	ticketOK         bool
	ticketRestricted bool
	ticketInvalid    bool
}

// expectOutcome is the §6.2 decision, written out independently of
// authorize: the status and error code a principal gets on a route (200 when
// the handler runs). anonListen/anonRestricted describe the anonymous policy.
func expectOutcome(pc principalCase, pol RoutePolicy, anonListen, anonRestricted bool) (int, string) {
	scopes, restricted, anonymous := pc.scopes, pc.restricted, pc.anonymous
	if pol.Ticket && (pc.ticketOK || pc.ticketInvalid) {
		if pc.ticketInvalid {
			return http.StatusUnauthorized, ErrInvalidTicket
		}
		scopes, restricted, anonymous = auth.Scopes{auth.ScopeListen}, pc.ticketRestricted, false
	} else if pc.invalid != "" {
		return http.StatusUnauthorized, pc.invalid
	}
	if anonymous {
		scopes = nil
		if anonListen {
			scopes = auth.Scopes{auth.ScopeListen}
		}
		restricted = anonRestricted
	}
	switch {
	case pol.Scope == "":
		return http.StatusOK, ""
	case anonymous && pol.FormKey:
		return http.StatusUnauthorized, ErrKeyRequired // no form key either
	case anonymous && (pol.KeyRequired || !anonListen || pol.Scope != auth.ScopeListen):
		return http.StatusUnauthorized, ErrKeyRequired
	case !scopes.Has(pol.Scope):
		return http.StatusForbidden, ErrInsufficientScope
	case restricted && pol.Restricted == Deny:
		return http.StatusForbidden, ErrRestrictedCredential
	}
	return http.StatusOK, ""
}

// TestAuthorizationMatrix sends every route a request as every kind of
// principal, under each anonymous policy, through the real pipeline
// (resolution, rate limiting, authorization, the upload middleware) with a
// stub key store, and compares with expectOutcome.
func TestAuthorizationMatrix(t *testing.T) {
	store := newStubAuthStore()
	a := newTestAuthenticator(store)
	restriction := &auth.Restriction{Systems: []int{1}}
	past := time.Now().Add(-time.Hour)
	store.add("listen-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	store.add("restricted-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, Restriction: restriction})
	store.add("edit-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeEdit}})
	store.add("admin-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
	store.add("admin-upload-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}})
	store.add("upload-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeUpload}})
	store.add("legacy-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}, Legacy: true})
	store.add("revoked-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}, RevokedAt: &past})
	store.add("expired-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}, ExpiresAt: &past})
	store.retired = database.HashAPIKey("old-public-token")

	mint := func(keyID int, narrowing *auth.Restriction) string {
		tok, err := auth.SignTicket(store.secret, auth.TicketPayload{KeyID: keyID, ExpiresAt: time.Now().Add(10 * time.Minute), Narrowing: narrowing})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	keyID := func(plaintext string) int {
		return store.byHash[database.HashAPIKey(plaintext)].ID
	}

	cases := []principalCase{
		{name: "no credential", anonymous: true},
		{name: "Basic auth header", headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, anonymous: true},
		{name: "empty bearer", headers: map[string]string{"Authorization": "Bearer   "}, anonymous: true},
		{name: "retired public token", headers: bearer("old-public-token"), anonymous: true},
		{name: "listen key", headers: bearer("listen-key"), scopes: auth.Scopes{auth.ScopeListen}},
		{name: "lowercase bearer", headers: map[string]string{"Authorization": "bearer listen-key"}, scopes: auth.Scopes{auth.ScopeListen}},
		{name: "restricted listen key", headers: bearer("restricted-key"), scopes: auth.Scopes{auth.ScopeListen}, restricted: true},
		{name: "edit key", headers: bearer("edit-key"), scopes: auth.Scopes{auth.ScopeEdit}},
		{name: "admin key", headers: bearer("admin-key"), scopes: auth.Scopes{auth.ScopeAdmin}},
		{name: "admin+upload key", headers: bearer("admin-upload-key"), scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}},
		{name: "upload key", headers: bearer("upload-key"), scopes: auth.Scopes{auth.ScopeUpload}},
		{name: "legacy key", headers: bearer("legacy-key"), scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}},
		{name: "revoked key", headers: bearer("revoked-key"), invalid: ErrInvalidKey},
		{name: "expired key", headers: bearer("expired-key"), invalid: ErrInvalidKey},
		{name: "unknown key", headers: bearer("tre_nope"), invalid: ErrInvalidKey},
		{name: "ticket", ticket: mint(keyID("admin-key"), nil), anonymous: true, ticketOK: true},
		{name: "narrowed ticket", ticket: mint(keyID("admin-key"), &auth.Restriction{Talkgroups: []auth.TG{{SystemID: 1, Tgid: 2}}}),
			anonymous: true, ticketOK: true, ticketRestricted: true},
		{name: "ticket of a restricted key", ticket: mint(keyID("restricted-key"), nil), anonymous: true, ticketOK: true, ticketRestricted: true},
		{name: "ticket beats an invalid header", ticket: mint(keyID("listen-key"), nil), headers: bearer("tre_nope"),
			invalid: ErrInvalidKey, ticketOK: true},
		{name: "ticket with an admin header", ticket: mint(keyID("listen-key"), nil), headers: bearer("admin-key"),
			scopes: auth.Scopes{auth.ScopeAdmin}, ticketOK: true},
		{name: "ticket of an upload-only key", ticket: mint(keyID("upload-key"), nil), anonymous: true, ticketInvalid: true},
		{name: "ticket of a revoked key", ticket: mint(keyID("revoked-key"), nil), anonymous: true, ticketInvalid: true},
		{name: "forged ticket", ticket: "trt_eyJrIjoxfQ.AAAA", anonymous: true, ticketInvalid: true},
	}

	policies := []struct {
		access     string
		restricted *auth.Restriction
	}{
		{database.AccessOff, nil},
		{database.AccessListen, nil},
		{database.AccessListen, &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5}}}},
	}
	for _, anon := range policies {
		store.setAnonymous(anon.access, anon.restricted)
		shadow := shadowRouter(t, a)
		for route, pol := range routePolicies {
			method, pattern, _ := strings.Cut(route, " ")
			for _, pc := range cases {
				target := synthesize(pattern, "7")
				if pc.ticket != "" {
					target += "?ticket=" + pc.ticket
				}
				rec := do(shadow, method, target, pc.headers)
				wantStatus, wantCode := expectOutcome(pc, pol, anon.access == database.AccessListen, anon.restricted != nil)
				gotCode := errorCode(rec)
				if rec.Code != wantStatus || gotCode != wantCode {
					t.Errorf("anonymous %s restricted=%v, %s, %s: got %d %q, want %d %q",
						anon.access, anon.restricted != nil, pc.name, route, rec.Code, gotCode, wantStatus, wantCode)
				}
				if (rec.Code == http.StatusOK) != (rec.Header().Get(reachedHeader) != "") {
					t.Errorf("%s, %s: status %d but handler reached = %q", pc.name, route, rec.Code, rec.Header().Get(reachedHeader))
				}
			}
		}
	}
}

// TestAuthorizationGoldenCases pins a few decisions by hand, so the matrix
// oracle can't drift together with the implementation.
func TestAuthorizationGoldenCases(t *testing.T) {
	store := newStubAuthStore()
	store.add("listen-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	store.add("restricted-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, Restriction: &auth.Restriction{Systems: []int{1}}})
	store.add("upload-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeUpload}})
	store.add("edit-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeEdit}})
	store.setAnonymous(database.AccessListen, nil)
	shadow := shadowRouter(t, newTestAuthenticator(store))

	for _, tc := range []struct {
		method, path string
		headers      map[string]string
		status       int
		code         string
	}{
		{"GET", "/api/v1/calls", nil, 200, ""},
		{"HEAD", "/api/v1/calls", nil, 200, ""},
		{"GET", "/metrics", nil, 401, ErrKeyRequired},
		{"GET", "/metrics", bearer("listen-key"), 200, ""},
		{"POST", "/api/v1/tickets", nil, 401, ErrKeyRequired},
		{"POST", "/api/v1/tickets", bearer("upload-key"), 403, ErrInsufficientScope},
		{"POST", "/api/v1/tickets", bearer("listen-key"), 200, ""},
		{"PATCH", "/api/v1/talkgroups/1:2", nil, 401, ErrKeyRequired},
		{"PATCH", "/api/v1/talkgroups/1:2", bearer("listen-key"), 403, ErrInsufficientScope},
		{"PATCH", "/api/v1/talkgroups/1:2", bearer("edit-key"), 200, ""},
		{"POST", "/api/v1/query", bearer("edit-key"), 403, ErrInsufficientScope},
		{"POST", "/api/v1/debug-report", nil, 401, ErrKeyRequired},
		{"GET", "/api/v1/units", bearer("restricted-key"), 403, ErrRestrictedCredential},
		{"GET", "/api/v1/events/stream", bearer("restricted-key"), 200, ""}, // Enforced by the stream
		{"GET", "/api/v1/audio/live", bearer("restricted-key"), 200, ""},
		{"GET", "/api/v1/audio/jitter", bearer("restricted-key"), 403, ErrRestrictedCredential},
		{"GET", "/api/v1/calls", bearer("upload-key"), 403, ErrInsufficientScope},
		{"GET", "/api/v1/whoami", bearer("tre_unknown"), 401, ErrInvalidKey},
		{"GET", "/api/v1/health", bearer("tre_unknown"), 401, ErrInvalidKey},
		{"GET", "/api/v1/whoami", nil, 200, ""},
		{"POST", "/api/v1/call-upload", nil, 401, ErrKeyRequired},
		{"POST", "/api/v1/call-upload", bearer("listen-key"), 403, ErrInsufficientScope},
		{"POST", "/api/v1/call-upload", bearer("upload-key"), 200, ""},
		{"GET", "/api/v1/calls?ticket=trt_x.y", nil, 200, ""}, // ignored off ticket routes
	} {
		rec := do(shadow, tc.method, tc.path, tc.headers)
		if rec.Code != tc.status || errorCode(rec) != tc.code {
			t.Errorf("%s %s %v: got %d %q, want %d %q", tc.method, tc.path, tc.headers, rec.Code, errorCode(rec), tc.status, tc.code)
		}
	}

	// An insufficient_scope message names the scope the way openapi.yaml's
	// example does; clients (tr-dashboard) read the scope from it.
	for _, tc := range []struct {
		method, path string
		headers      map[string]string
		want         string
	}{
		{"PATCH", "/api/v1/talkgroups/1:2", bearer("listen-key"), "this operation needs the edit scope"},
		{"POST", "/api/v1/query", bearer("edit-key"), "this operation needs the admin scope"},
		{"GET", "/api/v1/calls", bearer("upload-key"), "this operation needs the listen scope"},
		{"POST", "/api/v1/call-upload", bearer("listen-key"), "this operation needs the upload scope"},
	} {
		rec := do(shadow, tc.method, tc.path, tc.headers)
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || errorCode(rec) != ErrInsufficientScope || body.Error != tc.want {
			t.Errorf("%s %s: %d %s, want insufficient_scope %q", tc.method, tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestSSEEventPolicy(t *testing.T) {
	restricted := &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{Systems: []int{1}, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 9}}}}}
	listen := &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{auth.ScopeListen}}
	admin := &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeAdmin}}
	none := &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{}}

	for _, tc := range []struct {
		p        *auth.Principal
		typ      string
		sys, tg  int
		expected bool
	}{
		{admin, "console", 0, 0, true},
		{listen, "console", 0, 0, false},
		{restricted, "console", 1, 5, false},
		{listen, "call_start", 0, 0, true},
		{listen, "recorder_update", 0, 0, true},
		{none, "call_start", 1, 5, false},
		{nil, "call_start", 1, 5, false},
		{restricted, "call_start", 1, 5, true},
		{restricted, "call_end", 2, 5, false},
		{restricted, "call_end", 1, 9, false}, // excluded
		{restricted, "transcription", 1, 5, true},
		{restricted, "unit_event", 1, 0, false}, // on/off: no talkgroup
		{restricted, "unit_event", 1, 5, true},
		{restricted, "call_update", 0, 5, false},
		{restricted, "recorder_update", 1, 5, false},
		{restricted, "rate_update", 1, 5, false},
		{restricted, "trunking_message", 1, 5, false},
		{listen, "some_future_type", 1, 5, false},
		{admin, "some_future_type", 1, 5, true},
	} {
		if got := SSEEventAllowed(tc.p, tc.typ, tc.sys, tc.tg); got != tc.expected {
			name := "nil"
			if tc.p != nil {
				name = fmt.Sprintf("%s%v restricted=%v", tc.p.Kind, tc.p.Scopes, tc.p.Restricted())
			}
			t.Errorf("SSEEventAllowed(%s, %s, %d, %d) = %v, want %v", name, tc.typ, tc.sys, tc.tg, got, tc.expected)
		}
	}
	// Every event type the ingest pipeline publishes is classified.
	for _, typ := range []string{"call_start", "call_update", "call_end", "transcription", "unit_event",
		"recorder_update", "rate_update", "trunking_message", "console"} {
		if _, ok := sseEventPolicies[typ]; !ok {
			t.Errorf("SSE event type %s is not classified", typ)
		}
	}
}

// TestOpenAPIAgreesWithPolicyTable reads openapi.yaml's per-operation
// x-scope / x-restricted / x-key-required and compares them with the table
// (§6.4), in both directions.
func TestOpenAPIAgreesWithPolicyTable(t *testing.T) {
	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Skip("openapi.yaml not readable:", err)
	}
	type operation struct {
		Scope       string `yaml:"x-scope"`
		Restricted  string `yaml:"x-restricted"`
		KeyRequired bool   `yaml:"x-key-required"`
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	documented := make(map[string]bool)
	for path, item := range doc.Paths {
		full := apiPrefix + path
		if path == "/metrics" {
			full = "/metrics" // served at the root, not under /api/v1
		}
		for method, rawOp := range item {
			method = strings.ToUpper(method)
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue // parameters, summary, ...
			}
			var op operation
			if b, err := yaml.Marshal(rawOp); err != nil || yaml.Unmarshal(b, &op) != nil {
				t.Errorf("%s %s: can't read the operation", method, path)
				continue
			}
			route := method + " " + full
			documented[route] = true
			pol, ok := routePolicies[route]
			if !ok {
				t.Errorf("openapi.yaml documents %s, which has no policy (is it a route?)", route)
				continue
			}
			wantScope := string(pol.Scope)
			if wantScope == "" {
				wantScope = "public"
			}
			if op.Scope != wantScope {
				t.Errorf("%s: x-scope %q, policy table %q", route, op.Scope, wantScope)
			}
			if op.KeyRequired != pol.KeyRequired {
				t.Errorf("%s: x-key-required %v, policy table %v", route, op.KeyRequired, pol.KeyRequired)
			}
			switch {
			case pol.Scope == "":
				// Public: the auth layer never looks at restrictions, so
				// either description is accurate.
			case op.Restricted == pol.Restricted.String():
			default:
				t.Errorf("%s: x-restricted %q, policy table %q", route, op.Restricted, pol.Restricted)
			}
		}
	}
	for route := range routePolicies {
		_, pattern, _ := strings.Cut(route, " ")
		if (strings.HasPrefix(pattern, apiPrefix+"/") || pattern == "/metrics") && !documented[route] {
			t.Errorf("%s has a policy but no operation in openapi.yaml", route)
		}
	}
}
