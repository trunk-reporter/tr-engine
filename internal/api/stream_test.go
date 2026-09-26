package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// guardFor builds the stream guard of a request whose principal a resolved
// under generation gen, as Resolve leaves it.
func guardFor(t *testing.T, a *authenticator, gen uint64, p *auth.Principal, k *database.APIKey, target string) *streamGuard {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r = withPrincipal(withStreamResolver(r, a, gen), p, k)
	g := newStreamGuard(r, zerolog.Nop())
	if g == nil {
		t.Fatal("no guard")
	}
	return g
}

// awaitSignal waits for the guard's close signal ("" if none within d).
func awaitSignal(ch <-chan string, d time.Duration) string {
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		return ""
	}
}

func TestStreamGuardRecheck(t *testing.T) {
	store := newStubAuthStore()
	a := newTestAuthenticator(store)
	ctx := context.Background()

	listen := store.add("listen", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	keyP := keyPrincipal(listen, "alice")

	t.Run("key: unchanged, changed, revoked, lookup failure", func(t *testing.T) {
		g := guardFor(t, a, auth.Generation(), keyP, listen, "/api/v1/events/stream")
		if sig := g.recheck(ctx); sig != "" || g.principal.Load() != keyP {
			t.Fatalf("unchanged key: signal %q, principal swapped = %v", sig, g.principal.Load() != keyP)
		}

		store.mu.Lock()
		listen.Restriction = &auth.Restriction{Systems: []int{3}}
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != "" {
			t.Fatalf("restricted key: signal %q", sig)
		}
		p := g.principal.Load()
		if p == keyP || !p.Restricted() || !p.AllowsTG(3, 1) || p.AllowsTG(4, 1) || p.Actor != "alice" {
			t.Fatalf("principal after the restriction change = %+v", p)
		}

		store.mu.Lock()
		store.failWith = errStubDown
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != "" || g.principal.Load() != p {
			t.Fatalf("lookup failure: signal %q; the stream must stay as it is", sig)
		}
		store.mu.Lock()
		store.failWith = nil
		now := time.Now()
		listen.RevokedAt = &now
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != streamInvalidKey {
			t.Fatalf("revoked key: signal %q, want invalid_key", sig)
		}
	})

	t.Run("key: unknown or re-scoped", func(t *testing.T) {
		k := store.add("rescoped", database.APIKey{Scopes: auth.Scopes{auth.ScopeEdit}})
		g := guardFor(t, a, auth.Generation(), keyPrincipal(k, ""), k, "/")
		store.mu.Lock()
		k.Scopes = auth.Scopes{auth.ScopeUpload}
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != streamInsufficientScope {
			t.Fatalf("upload-only key: signal %q, want insufficient_scope", sig)
		}
		gone := &auth.Principal{Kind: auth.KindKey, KeyID: 999999, Scopes: auth.Scopes{auth.ScopeListen}}
		if sig := guardFor(t, a, auth.Generation(), gone, nil, "/").recheck(ctx); sig != streamInvalidKey {
			t.Fatalf("unknown key: signal %q, want invalid_key", sig)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		store.setAnonymous(database.AccessListen, nil)
		anon := anonymousPrincipal(anonymousSettings{anon: database.AnonymousAccess{Access: database.AccessListen}}, "")
		g := guardFor(t, a, auth.Generation(), anon, nil, "/")
		if sig := g.recheck(ctx); sig != "" || g.principal.Load() != anon {
			t.Fatalf("unchanged policy: signal %q", sig)
		}
		store.setAnonymous(database.AccessListen, &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 2}}})
		if sig := g.recheck(ctx); sig != "" || !g.principal.Load().Restricted() {
			t.Fatalf("restricted policy: signal %q, principal %+v", sig, g.principal.Load())
		}
		store.setAnonymous(database.AccessOff, nil)
		if sig := g.recheck(ctx); sig != streamKeyRequired {
			t.Fatalf("policy off: signal %q, want key_required", sig)
		}
	})

	t.Run("ticket", func(t *testing.T) {
		k := store.add("ticket key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		sign := func(exp time.Time, n *auth.Restriction) (string, *auth.Principal) {
			tk, err := auth.SignTicket(store.secret, auth.TicketPayload{KeyID: k.ID, ExpiresAt: exp, Narrowing: n})
			if err != nil {
				t.Fatal(err)
			}
			p, _, aerr := a.resolveTicket(ctx, tk, true)
			if aerr != nil {
				t.Fatal(aerr.msg)
			}
			return tk, p
		}
		exp := time.Now().Add(10 * time.Minute)
		narrow := &auth.Restriction{Systems: []int{7}}
		tk, p := sign(exp, narrow)
		g := guardFor(t, a, auth.Generation(), p, k, "/api/v1/events/stream?ticket="+url.QueryEscape(tk))
		if g.ticket != tk || g.ticketExpires.IsZero() {
			t.Fatalf("guard ticket %q, expiry %v", g.ticket, g.ticketExpires)
		}
		if sig := g.recheck(ctx); sig != "" || g.principal.Load() != p {
			t.Fatalf("unchanged ticket: signal %q", sig)
		}

		// The key gains a restriction: the ticket's principal carries both.
		store.mu.Lock()
		k.Restriction = &auth.Restriction{Systems: []int{7, 8}}
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != "" || len(g.principal.Load().Restrictions) != 2 {
			t.Fatalf("restricted key: signal %q, principal %+v", sig, g.principal.Load())
		}

		// The narrowing names a system merged away: a new ticket fixes it.
		store.mu.Lock()
		store.merged[7] = true
		store.mu.Unlock()
		auth.Bump() // merges bump the generation, which clears the merged-system cache
		if sig := g.recheck(ctx); sig != streamTicketExpired {
			t.Fatalf("merged narrowing: signal %q, want ticket_expired", sig)
		}
		store.mu.Lock()
		delete(store.merged, 7)
		store.mu.Unlock()
		auth.Bump()

		// The key loses listen, or is revoked.
		store.mu.Lock()
		k.Restriction = nil
		k.Scopes = auth.Scopes{auth.ScopeUpload}
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != streamInsufficientScope {
			t.Fatalf("key without listen: signal %q, want insufficient_scope", sig)
		}
		store.mu.Lock()
		k.Scopes = auth.Scopes{auth.ScopeListen}
		now := time.Now()
		k.RevokedAt = &now
		store.mu.Unlock()
		if sig := g.recheck(ctx); sig != streamInvalidKey {
			t.Fatalf("revoked key: signal %q, want invalid_key", sig)
		}

		// An expired ticket (the clock moved past it).
		store.mu.Lock()
		k.RevokedAt = nil
		store.mu.Unlock()
		a.now = func() time.Time { return exp.Add(time.Second) }
		defer func() { a.now = time.Now }()
		if sig := g.recheck(ctx); sig != streamTicketExpired {
			t.Fatalf("expired ticket: signal %q, want ticket_expired", sig)
		}
	})
}

func TestStreamGuardWatch(t *testing.T) {
	store := newStubAuthStore()
	a := newTestAuthenticator(store)

	t.Run("a generation bump re-checks at once", func(t *testing.T) {
		k := store.add("watched", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		g := guardFor(t, a, auth.Generation(), keyPrincipal(k, ""), k, "/")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := g.watch(ctx)
		if s := awaitSignal(sig, 50*time.Millisecond); s != "" {
			t.Fatalf("signal %q before anything changed", s)
		}
		store.mu.Lock()
		now := time.Now()
		k.RevokedAt = &now
		store.mu.Unlock()
		auth.Bump()
		if s := awaitSignal(sig, 2*time.Second); s != streamInvalidKey {
			t.Fatalf("signal %q, want invalid_key", s)
		}
		if g.principal.Load() != nil {
			t.Error("the principal was not cleared before the close signal")
		}
	})

	t.Run("a change between resolution and the stream's start is caught", func(t *testing.T) {
		k := store.add("raced", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		gen := auth.Generation()
		store.mu.Lock()
		now := time.Now()
		k.RevokedAt = &now
		store.mu.Unlock()
		auth.Bump() // before the guard watches
		g := guardFor(t, a, gen, keyPrincipal(k, ""), k, "/")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if s := awaitSignal(g.watch(ctx), 2*time.Second); s != streamInvalidKey {
			t.Fatalf("signal %q, want invalid_key", s)
		}
	})

	t.Run("a key's expires_at closes the stream without a bump", func(t *testing.T) {
		exp := time.Now().Add(300 * time.Millisecond)
		k := store.add("expiring", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}, ExpiresAt: &exp})
		g := guardFor(t, a, auth.Generation(), keyPrincipal(k, ""), k, "/")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		if s := awaitSignal(g.watch(ctx), 3*time.Second); s != streamInvalidKey {
			t.Fatalf("signal %q, want invalid_key", s)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("closed after %s; the key expired after 300ms", d)
		}
	})

	t.Run("a ticket closes at its expiry, also without an authenticator", func(t *testing.T) {
		exp := time.Now().Add(300 * time.Millisecond)
		p := &auth.Principal{Kind: auth.KindTicket, KeyID: 1, Scopes: auth.Scopes{auth.ScopeListen}, TicketExpiry: exp}
		r := withPrincipal(httptest.NewRequest(http.MethodGet, "/?ticket=x", nil), p, nil)
		g := newStreamGuard(r, zerolog.Nop()) // no resolver: handler reached without the pipeline
		if g == nil || g.a != nil {
			t.Fatalf("guard = %+v", g)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if s := awaitSignal(g.watch(ctx), 3*time.Second); s != streamTicketExpired {
			t.Fatalf("signal %q, want ticket_expired", s)
		}
		if time.Now().Before(exp) {
			t.Error("closed before the ticket expired")
		}
	})

	t.Run("the watch ends with its context", func(t *testing.T) {
		k := store.add("ending", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
		g := guardFor(t, a, auth.Generation(), keyPrincipal(k, ""), k, "/")
		ctx, cancel := context.WithCancel(context.Background())
		sig := g.watch(ctx)
		cancel()
		store.mu.Lock()
		now := time.Now()
		k.RevokedAt = &now
		store.mu.Unlock()
		auth.Bump()
		if s := awaitSignal(sig, 200*time.Millisecond); s != "" {
			t.Fatalf("signal %q after the stream ended", s)
		}
		if g.principal.Load() == nil {
			t.Error("a finished watch cleared the principal")
		}
	})
}

func TestStreamHandlersWithoutPrincipal(t *testing.T) {
	// Only reachable without the auth pipeline, and then fail closed.
	rec := httptest.NewRecorder()
	NewEventsHandler(newStreamTestLive()).StreamEvents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil))
	if rec.Code != http.StatusForbidden || errorCode(rec) != ErrForbidden {
		t.Errorf("SSE without a principal: %d %s", rec.Code, rec.Body.String())
	}
	if newStreamGuard(httptest.NewRequest(http.MethodGet, "/", nil), zerolog.Nop()) != nil {
		t.Error("a guard without a principal")
	}
}

func TestSamePrincipal(t *testing.T) {
	base := func() *auth.Principal {
		return &auth.Principal{Kind: auth.KindKey, KeyID: 1, KeyName: "k", Scopes: auth.Scopes{auth.ScopeListen},
			Restrictions: []auth.Restriction{{Systems: []int{2, 1}}}}
	}
	if !samePrincipal(base(), base()) {
		t.Error("equal principals differ")
	}
	sorted := base()
	sorted.Restrictions[0].Systems = []int{1, 2, 2}
	if !samePrincipal(base(), sorted) {
		t.Error("the order of a restriction's lists matters")
	}
	for name, change := range map[string]func(p *auth.Principal){
		"scopes":       func(p *auth.Principal) { p.Scopes = auth.Scopes{auth.ScopeEdit} },
		"restriction":  func(p *auth.Principal) { p.Restrictions[0].Systems = []int{1} },
		"unrestricted": func(p *auth.Principal) { p.Restrictions = nil },
		"allow-nothing narrowing": func(p *auth.Principal) {
			p.Restrictions = append(p.Restrictions, auth.Restriction{})
		},
		"name": func(p *auth.Principal) { p.KeyName = "renamed" },
	} {
		p := base()
		change(p)
		if samePrincipal(base(), p) {
			t.Errorf("%s change not detected", name)
		}
	}
}

func TestWSCloseCode(t *testing.T) {
	for signal, want := range map[string]int{
		streamInvalidKey:        4401,
		streamKeyRequired:       4401,
		streamTicketExpired:     4401,
		streamInsufficientScope: 4403,
	} {
		if got := wsCloseCode(signal); got != want {
			t.Errorf("wsCloseCode(%s) = %d, want %d", signal, got, want)
		}
	}
}
