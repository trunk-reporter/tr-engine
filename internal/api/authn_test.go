package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// fakeClock is a settable clock for the authenticator.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// limitedAuthenticator has a per-IP limit of burst requests and no refill
// within a test.
func limitedAuthenticator(store authStore, burst int) (*authenticator, *fakeClock) {
	a := newAuthenticator(store, nil, 0.0001, burst, zerolog.Nop())
	clock := &fakeClock{t: time.Now()}
	a.now = clock.now
	return a, clock
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		header string
		token  string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER  abc  ", "abc", true},
		{"Bearer", "", false},
		{"Bearer    ", "", false},
		{"Basic dXNlcjpwYXNz", "", false},
		{"Token abc", "", false},
		{"", "", false},
		{"Bearerabc", "", false},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		token, ok := bearerToken(req)
		if token != tc.token || ok != tc.ok {
			t.Errorf("%q: got %q %v, want %q %v", tc.header, token, ok, tc.token, tc.ok)
		}
	}
}

func TestRateLimitingOrder(t *testing.T) {
	t.Run("guesses cost a per-IP token each and stop before the database", func(t *testing.T) {
		store := newStubAuthStore()
		a, _ := limitedAuthenticator(store, 3)
		shadow := shadowRouter(t, a)
		for i := 0; i < 3; i++ {
			if rec := do(shadow, "GET", "/api/v1/calls", bearer("tre_guess"+strconv.Itoa(i))); rec.Code != http.StatusUnauthorized {
				t.Fatalf("guess %d: %d", i, rec.Code)
			}
		}
		rec := do(shadow, "GET", "/api/v1/calls", bearer("tre_guess_more"))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("4th guess: %d %v, want 429 with Retry-After", rec.Code, rec.Header())
		}
		if store.lookups != 3 {
			t.Errorf("database lookups = %d, want 3 (the 429 must not touch the database)", store.lookups)
		}
	})

	t.Run("a cached key without a rate limit is not IP-limited", func(t *testing.T) {
		store := newStubAuthStore()
		store.add("good-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		a, _ := limitedAuthenticator(store, 2)
		shadow := shadowRouter(t, a)
		if rec := do(shadow, "GET", "/api/v1/calls", bearer("good-key")); rec.Code != http.StatusOK {
			t.Fatalf("first request: %d", rec.Code) // cache miss: one IP token
		}
		do(shadow, "GET", "/api/v1/calls", nil) // second token: anonymous
		if rec := do(shadow, "GET", "/api/v1/calls", nil); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("anonymous after the bucket is empty: %d, want 429", rec.Code)
		}
		for i := 0; i < 20; i++ {
			if rec := do(shadow, "GET", "/api/v1/calls", bearer("good-key")); rec.Code != http.StatusOK {
				t.Fatalf("cached key request %d: %d, want 200", i, rec.Code)
			}
		}
		if store.lookups != 1 {
			t.Errorf("database lookups = %d, want 1", store.lookups)
		}
	})

	t.Run("legacy keys are IP-limited", func(t *testing.T) {
		store := newStubAuthStore()
		store.add("legacy", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}, Legacy: true})
		a, _ := limitedAuthenticator(store, 3)
		shadow := shadowRouter(t, a)
		codes := []int{}
		for i := 0; i < 4; i++ {
			codes = append(codes, do(shadow, "GET", "/api/v1/calls", bearer("legacy")).Code)
		}
		if codes[2] != http.StatusOK || codes[3] != http.StatusTooManyRequests {
			t.Errorf("codes = %v, want the 4th to be 429", codes)
		}
	})

	t.Run("a key with rate_limit_rps has its own limiter", func(t *testing.T) {
		store := newStubAuthStore()
		rps := float32(1)
		store.add("limited", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, RateLimitRPS: &rps})
		a, clock := limitedAuthenticator(store, 1000)
		shadow := shadowRouter(t, a)
		codes := []int{}
		for i := 0; i < 3; i++ { // burst = 2 × rps
			codes = append(codes, do(shadow, "GET", "/api/v1/calls", bearer("limited")).Code)
		}
		if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
			t.Errorf("codes = %v, want 200 200 429", codes)
		}
		clock.advance(time.Second)
		if rec := do(shadow, "GET", "/api/v1/calls", bearer("limited")); rec.Code != http.StatusOK {
			t.Errorf("after a second: %d, want 200", rec.Code)
		}
	})

	t.Run("tickets are IP-limited and not charged to the key", func(t *testing.T) {
		store := newStubAuthStore()
		rps := float32(0.001) // a burst of 1
		k := store.add("limited", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, RateLimitRPS: &rps})
		a, _ := limitedAuthenticator(store, 2)
		shadow := shadowRouter(t, a)
		tok, _ := auth.SignTicket(store.secret, auth.TicketPayload{KeyID: k.ID, ExpiresAt: time.Now().Add(time.Minute)})
		codes := []int{}
		for i := 0; i < 3; i++ {
			codes = append(codes, do(shadow, "GET", "/api/v1/events/stream?ticket="+tok, nil).Code)
		}
		if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
			t.Errorf("codes = %v, want 200 200 429 (per IP, burst 2)", codes)
		}
	})
}

func TestKeyCache(t *testing.T) {
	t.Run("negative results are cached", func(t *testing.T) {
		store := newStubAuthStore()
		shadow := shadowRouter(t, newTestAuthenticator(store))
		for i := 0; i < 3; i++ {
			do(shadow, "GET", "/api/v1/calls", bearer("tre_unknown"))
		}
		if store.lookups != 1 {
			t.Errorf("lookups = %d, want 1", store.lookups)
		}
	})

	t.Run("a lookup error is 503, never cached and never anonymous", func(t *testing.T) {
		store := newStubAuthStore()
		store.add("good", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		store.setAnonymous(database.AccessListen, nil)
		store.failWith = errStubDown
		shadow := shadowRouter(t, newTestAuthenticator(store))
		for i := 0; i < 2; i++ {
			rec := do(shadow, "GET", "/api/v1/calls", bearer("good"))
			if rec.Code != http.StatusServiceUnavailable || errorCode(rec) != ErrServiceUnavail {
				t.Fatalf("attempt %d: %d %s, want 503", i, rec.Code, rec.Body.String())
			}
		}
		if store.lookups != 2 {
			t.Errorf("lookups = %d, want 2 (errors are not cached)", store.lookups)
		}
		store.mu.Lock()
		store.failWith = nil
		store.mu.Unlock()
		if rec := do(shadow, "GET", "/api/v1/calls", bearer("good")); rec.Code != http.StatusOK {
			t.Errorf("after recovery: %d", rec.Code)
		}
	})

	t.Run("revoked and expired keys say so", func(t *testing.T) {
		store := newStubAuthStore()
		past := time.Now().Add(-time.Minute)
		store.add("revoked", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, RevokedAt: &past})
		store.add("expired", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, ExpiresAt: &past})
		shadow := shadowRouter(t, newTestAuthenticator(store))
		for key, msg := range map[string]string{"revoked": "API key revoked", "expired": "API key expired", "tre_x": "invalid API key"} {
			for i := 0; i < 2; i++ { // second time from the negative cache
				rec := do(shadow, "GET", "/api/v1/whoami", bearer(key))
				if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), msg) {
					t.Errorf("%s (%d): %d %s, want 401 %q", key, i, rec.Code, rec.Body.String(), msg)
				}
			}
		}
	})

	t.Run("expiry is checked on every cache hit", func(t *testing.T) {
		store := newStubAuthStore()
		exp := time.Now().Add(10 * time.Second)
		store.add("soon", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, ExpiresAt: &exp})
		a, clock := limitedAuthenticator(store, 1000)
		shadow := shadowRouter(t, a)
		if rec := do(shadow, "GET", "/api/v1/calls", bearer("soon")); rec.Code != http.StatusOK {
			t.Fatalf("before expiry: %d", rec.Code)
		}
		clock.advance(11 * time.Second) // still within the cache TTL
		rec := do(shadow, "GET", "/api/v1/calls", bearer("soon"))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
			t.Errorf("after expiry: %d %s, want 401 expired", rec.Code, rec.Body.String())
		}
	})

	t.Run("the cache expires, and a generation bump clears it", func(t *testing.T) {
		store := newStubAuthStore()
		k := store.add("key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
		a, clock := limitedAuthenticator(store, 1000)
		shadow := shadowRouter(t, a)
		do(shadow, "GET", "/api/v1/keys", bearer("key"))

		// Revoked in the database by another process (the CLI): the cached
		// record is used until the TTL runs out.
		now := time.Now()
		store.mu.Lock()
		k.RevokedAt = &now
		store.mu.Unlock()
		if rec := do(shadow, "GET", "/api/v1/keys", bearer("key")); rec.Code != http.StatusOK {
			t.Fatalf("within the TTL: %d, want 200 (cached)", rec.Code)
		}
		clock.advance(keyCacheTTL + time.Second)
		if rec := do(shadow, "GET", "/api/v1/keys", bearer("key")); rec.Code != http.StatusUnauthorized {
			t.Errorf("after the TTL: %d, want 401", rec.Code)
		}

		// Un-revoke (only possible in a test), cache it again, then an
		// in-process change bumps the generation: the next request reads
		// the database.
		store.mu.Lock()
		k.RevokedAt = nil
		store.mu.Unlock()
		clock.advance(keyCacheTTL + time.Second)
		if rec := do(shadow, "GET", "/api/v1/keys", bearer("key")); rec.Code != http.StatusOK {
			t.Fatalf("re-cached: %d", rec.Code)
		}
		store.mu.Lock()
		k.Scopes = auth.Scopes{auth.ScopeListen}
		store.mu.Unlock()
		auth.Bump()
		if rec := do(shadow, "GET", "/api/v1/keys", bearer("key")); rec.Code != http.StatusForbidden {
			t.Errorf("after a bump: %d, want 403 (scope change seen at once)", rec.Code)
		}
	})

	t.Run("invalidateKey drops a key's entries", func(t *testing.T) {
		store := newStubAuthStore()
		k := store.add("key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
		a := newTestAuthenticator(store)
		shadow := shadowRouter(t, a)
		do(shadow, "GET", "/api/v1/keys", bearer("key"))
		store.mu.Lock()
		k.Scopes = auth.Scopes{auth.ScopeListen}
		store.mu.Unlock()
		a.invalidateKey(k.ID)
		if rec := do(shadow, "GET", "/api/v1/keys", bearer("key")); rec.Code != http.StatusForbidden {
			t.Errorf("after invalidateKey: %d, want 403", rec.Code)
		}
	})

	t.Run("the LRUs are bounded and separate", func(t *testing.T) {
		clock := &fakeClock{t: time.Now()}
		c := newKeyCache(2, clock.now)
		good := &database.APIKey{ID: 1}
		c.put(0, "good", good)
		for i := 0; i < 10; i++ {
			c.putNegative(0, "guess"+strconv.Itoa(i), "")
		}
		if k, ok := c.getByHash(0, "good"); !ok || k != good {
			t.Error("negative entries evicted a positive one")
		}
		if _, ok := c.getNegative(0, "guess0"); ok {
			t.Error("the negative cache is not bounded")
		}
		if _, ok := c.getByID(0, 1); !ok {
			t.Error("the positive cache is not indexed by ID")
		}
		c.put(0, "b", &database.APIKey{ID: 2})
		c.put(0, "c", &database.APIKey{ID: 3})
		if _, ok := c.getByHash(0, "good"); ok {
			t.Error("the positive cache is not bounded")
		}
		// A lookup that started before a generation bump is not cached.
		c.put(0, "d", &database.APIKey{ID: 4})
		if _, ok := c.getByHash(1, "d"); ok {
			t.Error("a new generation must start empty")
		}
		c.put(0, "e", &database.APIKey{ID: 5})
		if _, ok := c.getByHash(1, "e"); ok {
			t.Error("a record read under an old generation was cached")
		}
	})
}

func TestLastUsedThrottled(t *testing.T) {
	store := newStubAuthStore()
	k := store.add("key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	a, clock := limitedAuthenticator(store, 1000)
	shadow := shadowRouter(t, a)
	// touches waits for the asynchronous writes to land and returns them.
	touches := func(want int) []int {
		deadline := time.Now().Add(2 * time.Second)
		for {
			store.mu.Lock()
			got := append([]int(nil), store.touched...)
			store.mu.Unlock()
			if len(got) >= want || time.Now().After(deadline) {
				time.Sleep(20 * time.Millisecond) // catch writes beyond want
				store.mu.Lock()
				defer store.mu.Unlock()
				return append([]int(nil), store.touched...)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for i := 0; i < 5; i++ {
		do(shadow, "GET", "/api/v1/calls", bearer("key"))
	}
	if got := touches(1); len(got) != 1 || got[0] != k.ID {
		t.Fatalf("touches after 5 requests = %v, want one for key %d", got, k.ID)
	}
	clock.advance(time.Minute + time.Second)
	do(shadow, "GET", "/api/v1/calls", bearer("key"))
	if got := touches(2); len(got) != 2 {
		t.Errorf("touched = %v, want a second touch after a minute", got)
	}
}

func TestTickets(t *testing.T) {
	store := newStubAuthStore()
	listen := store.add("listen", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	restricted := store.add("restricted", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen},
		Restriction: &auth.Restriction{Systems: []int{1}}})
	a, clock := limitedAuthenticator(store, 1000)
	shadow := shadowRouter(t, a)

	mint := func(keyID int, n *auth.Restriction, ttl time.Duration) string {
		tok, err := auth.SignTicket(store.secret, auth.TicketPayload{KeyID: keyID, ExpiresAt: clock.now().Add(ttl), Narrowing: n})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	t.Run("a ticket is a listen principal on ticket routes, GET and HEAD", func(t *testing.T) {
		tok := mint(listen.ID, nil, time.Minute)
		for _, method := range []string{"GET", "HEAD"} {
			rec := do(shadow, method, "/api/v1/calls/5/audio?ticket="+tok, nil)
			if rec.Code != http.StatusOK || rec.Header().Get("X-Test-Principal") != "ticket" {
				t.Errorf("%s: %d principal %q", method, rec.Code, rec.Header().Get("X-Test-Principal"))
			}
		}
		// Ignored elsewhere: anonymous (off) is key_required.
		if rec := do(shadow, "GET", "/api/v1/calls?ticket="+tok, nil); errorCode(rec) != ErrKeyRequired {
			t.Errorf("non-ticket route: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("expired and orphaned tickets are invalid_ticket", func(t *testing.T) {
		tok := mint(listen.ID, nil, time.Minute)
		clock.advance(2 * time.Minute)
		rec := do(shadow, "GET", "/api/v1/events/stream?ticket="+tok, nil)
		if errorCode(rec) != ErrInvalidTicket || !strings.Contains(rec.Body.String(), "expired") {
			t.Errorf("expired: %d %s", rec.Code, rec.Body.String())
		}
		if rec := do(shadow, "GET", "/api/v1/events/stream?ticket="+mint(99999, nil, time.Minute), nil); errorCode(rec) != ErrInvalidTicket {
			t.Errorf("unknown key: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the key's current restriction applies, plus the narrowing", func(t *testing.T) {
		store.merged[3] = true
		if rec := do(shadow, "GET", "/api/v1/events/stream?ticket="+mint(listen.ID, &auth.Restriction{Systems: []int{3}}, time.Minute), nil); errorCode(rec) != ErrInvalidTicket {
			t.Errorf("merged system: %d %s, want invalid_ticket", rec.Code, rec.Body.String())
		}
		// Every ticket route is Enforced: a restricted key's ticket gets
		// through, and the stream applies the restriction (§7.3, §7.4).
		if rec := do(shadow, "GET", "/api/v1/events/stream?ticket="+mint(restricted.ID, nil, time.Minute), nil); rec.Code != http.StatusOK || rec.Header().Get("X-Test-Principal") != "ticket" {
			t.Errorf("restricted key's ticket: %d %s", rec.Code, rec.Body.String())
		}
		p, _, aerr := a.resolveTicket(t.Context(), mint(restricted.ID, &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5}}}, time.Minute), false)
		if aerr != nil {
			t.Fatal(aerr.msg)
		}
		if len(p.Restrictions) != 2 || !p.AllowsTG(1, 4) || p.AllowsTG(1, 5) || p.AllowsTG(2, 4) || !p.Has(auth.ScopeListen) || p.Has(auth.ScopeEdit) {
			t.Errorf("principal = %+v", p)
		}
	})
}

func TestRetiredPublicToken(t *testing.T) {
	store := newStubAuthStore()
	store.retired = database.HashAPIKey("old-public")
	store.setAnonymous(database.AccessListen, nil)
	var logBuf bytes.Buffer
	a := newAuthenticator(store, nil, 1e9, 1<<30, zerolog.New(&logBuf))
	shadow := shadowRouter(t, a)
	for i := 0; i < 3; i++ {
		rec := do(shadow, "GET", "/api/v1/calls", bearer("old-public"))
		if rec.Code != http.StatusOK || rec.Header().Get("X-Test-Principal") != "anonymous" {
			t.Fatalf("retired token: %d %q, want anonymous", rec.Code, rec.Header().Get("X-Test-Principal"))
		}
	}
	if n := strings.Count(logBuf.String(), "pre-upgrade public AUTH_TOKEN"); n != 1 {
		t.Errorf("warnings = %d, want 1 (at most hourly)", n)
	}
	if store.lookups != 0 {
		t.Errorf("the retired token was looked up as a key %d times", store.lookups)
	}
}

func TestAudit(t *testing.T) {
	store := newStubAuthStore()
	store.add("admin", database.APIKey{Name: "ops script", Scopes: auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}})
	store.setAnonymous(database.AccessListen, nil)
	shadow := shadowRouter(t, newTestAuthenticator(store))

	do(shadow, "GET", "/api/v1/keys", bearer("admin"))                 // GET: not audited
	do(shadow, "HEAD", "/api/v1/calls", bearer("admin"))               // HEAD: not audited
	do(shadow, "POST", "/api/v1/tickets", bearer("admin"))             // skipped route
	do(shadow, "PATCH", "/api/v1/talkgroups/1:2", nil)                 // anonymous: not audited
	do(shadow, "DELETE", "/api/v1/keys/7", bearer("tre_bad"))          // failed auth: not audited
	do(shadow, "POST", "/api/v1/nowhere", bearer("admin"))             // unmatched: not audited
	do(shadow, "DELETE", "/api/v1/keys/7?x=secret", map[string]string{ // audited
		"Authorization": "Bearer admin", "X-Actor": "alice‮", "X-Request-ID": "req-1",
	})
	entries := store.auditEntries()
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly the DELETE", entries)
	}
	e := entries[0]
	if e.KeyName != "ops script" || e.Method != "DELETE" || e.Path != "/api/v1/keys/7" || e.Status != 200 ||
		e.RequestID != "req-1" || e.Actor == nil || *e.Actor != "alice" {
		t.Errorf("entry = %+v (actor %v)", e, *e.Actor)
	}

	// The status the client received, including refusals from handlers.
	a := newTestAuthenticator(store)
	h := a.Audit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteErrorWithCode(w, http.StatusConflict, ErrConflict, "no")
	}))
	req := httptest.NewRequest("POST", "/api/v1/keys", nil)
	req = req.WithContext(t.Context())
	m := &routeMatch{Pattern: "/api/v1/keys", Key: "POST /api/v1/keys", Policy: routePolicies["POST /api/v1/keys"]}
	req = withRouteForTest(req, m)
	req = withPrincipal(req, &auth.Principal{Kind: auth.KindKey, KeyID: 1, KeyName: "k"}, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := store.auditEntries(); got[len(got)-1].Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", got[len(got)-1].Status)
	}

	// A panic is recorded as the 500 Recoverer sends.
	panicky := Recoverer(a.Audit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })))
	panicky.ServeHTTP(httptest.NewRecorder(), req)
	if got := store.auditEntries(); got[len(got)-1].Status != http.StatusInternalServerError {
		t.Errorf("status after panic = %d, want 500", got[len(got)-1].Status)
	}
}

// uploadRequest builds a multipart upload with the given fields.
func uploadRequest(t *testing.T, target string, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("audio", "call.m4a")
	fw.Write([]byte("fake audio"))
	w.Close()
	req := httptest.NewRequest("POST", target, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.RemoteAddr = "192.0.2.20:5555"
	return req
}

func TestUploadAuth(t *testing.T) {
	store := newStubAuthStore()
	store.add("upload-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeUpload}})
	store.add("listen-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	store.setAnonymous(database.AccessListen, nil)
	var logBuf bytes.Buffer
	a := newAuthenticator(store, nil, 1e9, 1<<30, zerolog.New(&logBuf))
	r := buildRouter(allFeaturesOptions(a))
	fields := map[string]string{"system": "butco", "talkgroup": "9178", "dateTime": "1700000000", "frequency": "853262500"}

	send := func(target string, extra map[string]string, headers map[string]string) *httptest.ResponseRecorder {
		f := map[string]string{}
		for k, v := range fields {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		req := uploadRequest(t, target, f)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name    string
		target  string
		extra   map[string]string
		headers map[string]string
		status  int
		code    string
	}{
		{"form key", "/api/v1/call-upload", map[string]string{"key": "upload-key"}, nil, 201, ""},
		{"form api_key", "/api/v1/call-upload", map[string]string{"api_key": "upload-key"}, nil, 201, ""},
		{"bearer header", "/api/v1/call-upload", nil, bearer("upload-key"), 201, ""},
		{"no key", "/api/v1/call-upload", nil, nil, 401, ErrKeyRequired},
		{"key in the URL is ignored", "/api/v1/call-upload?key=upload-key&api_key=upload-key", nil, nil, 401, ErrKeyRequired},
		{"listen key in the form", "/api/v1/call-upload", map[string]string{"key": "listen-key"}, nil, 403, ErrInsufficientScope},
		{"listen key in the header", "/api/v1/call-upload", nil, bearer("listen-key"), 403, ErrInsufficientScope},
		{"invalid key does not fall through to api_key", "/api/v1/call-upload",
			map[string]string{"key": "wrong", "api_key": "upload-key"}, nil, 401, ErrInvalidKey},
		{"invalid bearer is 401 even with a good form key", "/api/v1/call-upload",
			map[string]string{"key": "upload-key"}, bearer("wrong"), 401, ErrInvalidKey},
	} {
		rec := send(tc.target, tc.extra, tc.headers)
		wantOK := tc.status == 201
		if wantOK && rec.Code >= 300 || !wantOK && (rec.Code != tc.status || errorCode(rec) != tc.code) {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
	if !strings.Contains(logBuf.String(), "call upload rejected") || !strings.Contains(logBuf.String(), `"system":"butco"`) {
		t.Errorf("rejections are not logged with the form's system: %s", logBuf.String())
	}
	// At most once a minute per IP: many rejections above, one line.
	if n := strings.Count(logBuf.String(), "call upload rejected"); n != 1 {
		t.Errorf("rejection warnings = %d, want 1 per minute per IP", n)
	}
}

func TestPurgeAge(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"48h", 48 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1h", time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"59m", 0, false},
		{"0h", 0, false},
		{"0d", 0, false},
		{"-7d", 0, false},
		{"-48h", 0, false},
		{"d", 0, false},
		{"7days", 0, false},
		{"", 0, false},
		{"999999999999d", 0, false},
	} {
		got, err := parsePurgeAge(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parsePurgeAge(%q) = %v, %v; want %v ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}
