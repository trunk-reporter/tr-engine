package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// Long-lived connections (the SSE stream and the live-audio WebSocket)
// re-check their principal every streamRecheckInterval and at once when the
// auth generation moves (§7.5). A principal that still has listen is swapped
// in place; otherwise the connection is closed with a signal the client can
// tell from a network blip.

// streamRecheckInterval is how often an open stream re-reads its credential
// from the database, which is how changes made by the CLI (another process,
// no generation bump) reach it.
const streamRecheckInterval = 60 * time.Second

// streamRecheckOverride, when positive, replaces streamRecheckInterval
// (nanoseconds; tests shorten it).
var streamRecheckOverride atomic.Int64

func recheckInterval() time.Duration {
	if d := streamRecheckOverride.Load(); d > 0 {
		return time.Duration(d)
	}
	return streamRecheckInterval
}

// Close signals of long-lived connections (§7.5): the SSE `event: auth` code,
// and the WebSocket close reason.
const (
	streamInvalidKey        = "invalid_key"
	streamKeyRequired       = "key_required"
	streamInsufficientScope = "insufficient_scope"
	streamTicketExpired     = "ticket_expired"
)

// WebSocket close codes for the signals (§7.5).
const (
	wsCloseUnauthorized = 4401 // invalid_key, key_required, ticket_expired
	wsCloseForbidden    = 4403 // insufficient_scope
)

// wsCloseCode returns the WebSocket close code for a close signal.
func wsCloseCode(signal string) int {
	if signal == streamInsufficientScope {
		return wsCloseForbidden
	}
	return wsCloseUnauthorized
}

type streamResolverKey struct{}

// streamResolver is what Resolve leaves for the stream handlers: the
// authenticator that resolved the principal, and the auth generation read
// before it did, so a change that raced the connection's start is caught.
type streamResolver struct {
	a   *authenticator
	gen uint64
}

// withStreamResolver records who resolved r's principal, and under which
// auth generation, for the stream re-check.
func withStreamResolver(r *http.Request, a *authenticator, gen uint64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), streamResolverKey{}, streamResolver{a: a, gen: gen}))
}

// streamGuard holds the principal of one open stream, which the event and
// audio buses read for every event or frame, and re-checks it.
type streamGuard struct {
	a      *authenticator // nil when the handler runs without the pipeline (tests): no re-check, only expiry
	gen    uint64         // auth generation the principal was resolved under
	ticket string         // the ?ticket= a ticket principal came from
	actor  string
	log    zerolog.Logger

	// principal is the stream's current principal. It is swapped when a
	// re-check finds a change that keeps listen, and cleared (nil: nothing
	// passes) right before the stream is closed.
	principal atomic.Pointer[auth.Principal]
	// ticketExpires is when a ticket stream's ticket expires (zero for
	// other principals); the stream closes then (§3.5).
	ticketExpires time.Time
	// keyExpires is the key's expires_at as last read (zero: none); the
	// stream re-checks then. Only the watch goroutine uses it.
	keyExpires time.Time
}

// newStreamGuard returns the guard of r's stream, or nil if r has no
// principal (a handler reached without the pipeline).
func newStreamGuard(r *http.Request, log zerolog.Logger) *streamGuard {
	p := PrincipalFrom(r)
	if p == nil {
		return nil
	}
	g := &streamGuard{actor: p.Actor, log: log}
	if res, ok := r.Context().Value(streamResolverKey{}).(streamResolver); ok {
		g.a, g.gen = res.a, res.gen
	}
	if p.Kind == auth.KindTicket {
		// Resolve took the principal from this ticket (on ticket routes a
		// ?ticket= wins over the header).
		g.ticket = r.URL.Query().Get("ticket")
		g.ticketExpires = p.TicketExpiry
	}
	g.keyExpires = keyExpiry(keyRecordFrom(r))
	g.principal.Store(p)
	return g
}

// keyExpiry returns k's expires_at, or zero if it has none.
func keyExpiry(k *database.APIKey) time.Time {
	if k == nil || k.ExpiresAt == nil {
		return time.Time{}
	}
	return *k.ExpiresAt
}

func (g *streamGuard) now() time.Time {
	if g.a != nil {
		return g.a.now()
	}
	return time.Now()
}

// watch re-checks the stream's principal until ctx ends or the principal no
// longer allows the stream. In that case it clears the principal and sends
// the close signal on the returned channel, once.
func (g *streamGuard) watch(ctx context.Context) <-chan string {
	out := make(chan string, 1)
	go g.run(ctx, out)
	return out
}

func (g *streamGuard) run(ctx context.Context, out chan<- string) {
	closeWith := func(signal string) {
		g.principal.Store(nil)
		out <- signal
	}

	// A ticket stream closes when its ticket expires, whatever else happens.
	var ticketExpired <-chan time.Time
	if !g.ticketExpires.IsZero() {
		t := time.NewTimer(g.ticketExpires.Sub(g.now()))
		defer t.Stop()
		ticketExpired = t.C
	}

	if g.a == nil {
		// No authenticator to re-check with: only the ticket's expiry applies.
		select {
		case <-ctx.Done():
		case <-ticketExpired:
			closeWith(streamTicketExpired)
		}
		return
	}

	// A key with expires_at is re-checked when it expires. The timer is not
	// re-armed for a time that has passed (a re-check that couldn't reach the
	// database), so a failing lookup is retried by the periodic re-check
	// rather than in a loop.
	var keyTimer *time.Timer
	var keyExpired <-chan time.Time
	armKeyExpiry := func() {
		if keyTimer != nil {
			keyTimer.Stop()
			keyTimer, keyExpired = nil, nil
		}
		if d := g.keyExpires.Sub(g.now()); !g.keyExpires.IsZero() && d > 0 {
			keyTimer = time.NewTimer(d)
			keyExpired = keyTimer.C
		}
	}
	armKeyExpiry()
	defer func() {
		if keyTimer != nil {
			keyTimer.Stop()
		}
	}()

	periodic := time.NewTicker(recheckInterval())
	defer periodic.Stop()

	// Take the generation channel before every re-check, so a bump during a
	// re-check wakes the next wait instead of being missed.
	gen, bumped := auth.Watch()
	check := gen != g.gen // something changed while the connection started
	for {
		if ctx.Err() != nil {
			return // the stream ended; nothing to re-check or signal
		}
		if check {
			if signal := g.recheck(ctx); signal != "" {
				closeWith(signal)
				return
			}
			armKeyExpiry()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticketExpired:
			closeWith(streamTicketExpired)
			return
		case <-keyExpired:
			keyTimer, keyExpired = nil, nil
		case <-bumped:
		case <-periodic.C:
		}
		_, bumped = auth.Watch()
		check = true
	}
}

// recheck re-resolves the stream's principal from the database, bypassing
// the caches (§7.5): a key by its ID; a ticket by verifying it again (its
// expiry, its key and the key's current restriction, its narrowing); the
// anonymous policy as stored. It swaps the principal when it changed but
// still has listen, and otherwise returns the close signal. A failed lookup
// keeps the stream as it is until the next re-check.
func (g *streamGuard) recheck(ctx context.Context) string {
	cur := g.principal.Load()
	var next *auth.Principal
	switch cur.Kind {
	case auth.KindKey:
		k, err := g.a.keyByID(ctx, cur.KeyID, true)
		if err != nil {
			g.log.Warn().Err(err).Int("key_id", cur.KeyID).Msg("stream: re-checking the key failed; keeping the stream open")
			return ""
		}
		if k == nil || !k.ActiveAt(g.now()) {
			return streamInvalidKey
		}
		next = keyPrincipal(k, g.actor)
		g.keyExpires = keyExpiry(k)
	case auth.KindTicket:
		p, k, aerr := g.a.resolveTicket(ctx, g.ticket, true)
		if aerr == errLookupFailed {
			g.log.Warn().Int("key_id", cur.KeyID).Msg("stream: re-checking the ticket failed; keeping the stream open")
			return ""
		}
		if aerr != nil {
			return g.ticketFailure(ctx, cur)
		}
		p.Actor = g.actor
		next = p
		g.keyExpires = keyExpiry(k)
	case auth.KindAnonymous:
		lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
		anon, err := g.a.store.GetAnonymousAccess(lctx)
		cancel()
		if err != nil {
			g.log.Warn().Err(err).Msg("stream: re-reading the anonymous access policy failed; keeping the stream open")
			return ""
		}
		next = anonymousPrincipal(anonymousSettings{anon: anon}, g.actor)
		if !next.Has(auth.ScopeListen) {
			return streamKeyRequired
		}
	default:
		return "" // internal principals never come from a request
	}
	if !next.Has(auth.ScopeListen) {
		return streamInsufficientScope
	}
	if !samePrincipal(cur, next) {
		g.principal.Store(next)
		g.log.Info().Str("credential", string(next.Kind)).Int("key_id", next.KeyID).
			Strs("scopes", next.Scopes.Strings()).Bool("restricted", next.Restricted()).
			Msg("stream: credential changed; continuing with its new access")
	}
	return ""
}

// ticketFailure says why a ticket that verified when the stream opened no
// longer does.
func (g *streamGuard) ticketFailure(ctx context.Context, cur *auth.Principal) string {
	secret, err := g.a.ticketSecret(ctx)
	if err != nil {
		return ""
	}
	if _, err := auth.VerifyTicket(secret, g.ticket, g.now()); err != nil {
		return streamTicketExpired // the secret can't change while the engine runs
	}
	k, err := g.a.keyByID(ctx, cur.KeyID, true)
	switch {
	case err != nil:
		return "" // try again at the next re-check
	case k == nil || !k.ActiveAt(g.now()):
		return streamInvalidKey
	case !k.Scopes.Has(auth.ScopeListen):
		return streamInsufficientScope
	}
	// The narrowing names a system that has since been merged away: a new
	// ticket (minted for the merged system) fixes that.
	return streamTicketExpired
}

// samePrincipal reports whether a and b allow the same things.
func samePrincipal(a, b *auth.Principal) bool {
	if a.Kind != b.Kind || a.KeyID != b.KeyID || a.KeyName != b.KeyName ||
		!equalScopes(a.Scopes, b.Scopes) || len(a.Restrictions) != len(b.Restrictions) {
		return false
	}
	for i := range a.Restrictions {
		if !a.Restrictions[i].Equal(&b.Restrictions[i]) {
			return false
		}
	}
	return true
}

func equalScopes(a, b auth.Scopes) bool {
	a, b = a.Normalize(), b.Normalize()
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
