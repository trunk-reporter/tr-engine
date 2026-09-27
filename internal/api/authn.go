package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// authStore is what principal resolution and the audit log need from the
// database. *database.DB implements it; tests use a stub.
type authStore interface {
	ResolveAPIKeyByHash(ctx context.Context, hash string) (*database.APIKey, error)
	GetAPIKeyByID(ctx context.Context, id int) (*database.APIKey, error)
	TouchAPIKey(ctx context.Context, id int) error
	GetAnonymousAccess(ctx context.Context) (database.AnonymousAccess, error)
	GetRetiredPublicTokenHash(ctx context.Context) (string, error)
	GetOrCreateTicketSecret(ctx context.Context) ([]byte, error)
	AnyMergedAwaySystem(ctx context.Context, systemIDs []int) (bool, error)
	InsertAuditLog(ctx context.Context, e database.AuditEntry) error
}

// Resolution timing (§8).
const (
	credentialLookupTimeout = 2 * time.Second
	settingsCacheTTL        = 30 * time.Second
	lastUsedInterval        = time.Minute // last_used_at is written at most this often per key
	retiredTokenWarnEvery   = time.Hour
	uploadRejectWarnEvery   = time.Minute // per client IP
)

// authError is a request the auth layer refuses.
type authError struct {
	status     int
	code       ErrorCode
	msg        string
	retryAfter string // seconds, for 429
	// uploadReason is the short reason logged for a rejected upload (§11.2).
	uploadReason string
}

func (e *authError) write(w http.ResponseWriter) {
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	WriteErrorWithCode(w, e.status, e.code, e.msg)
}

var (
	errRateLimited = &authError{status: http.StatusTooManyRequests, code: ErrRateLimited,
		msg: "rate limit exceeded", retryAfter: "1", uploadReason: "rate limited"}
	errLookupFailed = &authError{status: http.StatusServiceUnavailable, code: ErrServiceUnavail,
		msg: "credential lookup failed; try again", uploadReason: "credential lookup failed"}
)

func invalidKeyError(status database.KeyStatus) *authError {
	switch status {
	case database.KeyRevoked:
		return &authError{status: http.StatusUnauthorized, code: ErrInvalidKey, msg: "API key revoked", uploadReason: "revoked key"}
	case database.KeyExpired:
		return &authError{status: http.StatusUnauthorized, code: ErrInvalidKey, msg: "API key expired", uploadReason: "expired key"}
	}
	return &authError{status: http.StatusUnauthorized, code: ErrInvalidKey, msg: "invalid API key", uploadReason: "unknown key"}
}

func invalidTicketError(msg string) *authError {
	return &authError{status: http.StatusUnauthorized, code: ErrInvalidTicket, msg: msg}
}

// anonymousSettings is the cached part of auth_settings that every request
// may need: the anonymous access policy and the retired public token.
type anonymousSettings struct {
	anon        database.AnonymousAccess
	retiredHash string
}

// authenticator resolves a request's principal (§3.3) with the rate limiting
// interleaved as §8 describes, and caches key lookups and settings.
type authenticator struct {
	store   authStore
	proxies *TrustedProxies
	log     zerolog.Logger
	now     func() time.Time

	ipRPS   float64
	ipBurst int
	ipLim   *limiterSet
	keyLim  *limiterSet
	keys    *keyCache

	settingsMu  sync.Mutex
	settings    anonymousSettings
	settingsAt  time.Time
	settingsGen uint64
	settingsOK  bool

	secretMu sync.Mutex
	secret   []byte

	mergedMu  sync.Mutex
	merged    map[string]mergedEntry // sorted system IDs -> result
	mergedGen uint64

	touchMu sync.Mutex
	touched map[int]time.Time

	warnMu      sync.Mutex
	retiredWarn time.Time
	uploadWarn  map[string]time.Time
}

type mergedEntry struct {
	merged  bool
	expires time.Time
}

func newAuthenticator(store authStore, proxies *TrustedProxies, ipRPS float64, ipBurst int, log zerolog.Logger) *authenticator {
	a := &authenticator{
		store:      store,
		proxies:    proxies,
		log:        log,
		now:        time.Now,
		ipRPS:      ipRPS,
		ipBurst:    ipBurst,
		merged:     make(map[string]mergedEntry),
		touched:    make(map[int]time.Time),
		uploadWarn: make(map[string]time.Time),
	}
	// The caches and limiters read the authenticator's clock, which tests
	// replace.
	clock := func() time.Time { return a.now() }
	a.ipLim = newLimiterSet(clock)
	a.keyLim = newLimiterSet(clock)
	a.keys = newKeyCache(keyCacheSize, clock)
	return a
}

// allowIP takes a per-IP token (RATE_LIMIT_RPS / RATE_LIMIT_BURST).
func (a *authenticator) allowIP(ip string) bool {
	return a.ipLim.allow(ip, a.ipRPS, a.ipBurst)
}

// allowKey takes a token from a key's own limiter (burst 2 × rps, at least 1).
func (a *authenticator) allowKey(k *database.APIKey) bool {
	rps := float64(*k.RateLimitRPS)
	return a.keyLim.allow(strconv.Itoa(k.ID), rps, max(1, int(2*rps)))
}

// anonymousSettings returns the anonymous policy and retired token hash,
// cached for settingsCacheTTL and reloaded when the auth generation moves.
func (a *authenticator) anonymousSettings(ctx context.Context) (anonymousSettings, error) {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	gen := auth.Generation()
	if a.settingsOK && gen == a.settingsGen && a.now().Sub(a.settingsAt) < settingsCacheTTL {
		return a.settings, nil
	}
	lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
	defer cancel()
	anon, err := a.store.GetAnonymousAccess(lctx)
	if err != nil {
		return anonymousSettings{}, err
	}
	retired, err := a.store.GetRetiredPublicTokenHash(lctx)
	if err != nil {
		return anonymousSettings{}, err
	}
	a.settings = anonymousSettings{anon: anon, retiredHash: retired}
	a.settingsAt, a.settingsGen, a.settingsOK = a.now(), gen, true
	return a.settings, nil
}

// invalidateSettings drops the cached anonymous policy (after a PUT).
func (a *authenticator) invalidateSettings() {
	a.settingsMu.Lock()
	a.settingsOK = false
	a.settingsMu.Unlock()
}

// ticketSecret returns the ticket signing secret, loading (and on first
// start creating) it once.
func (a *authenticator) ticketSecret(ctx context.Context) ([]byte, error) {
	a.secretMu.Lock()
	defer a.secretMu.Unlock()
	if a.secret != nil {
		return a.secret, nil
	}
	lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
	defer cancel()
	s, err := a.store.GetOrCreateTicketSecret(lctx)
	if err != nil {
		return nil, err
	}
	a.secret = s
	return s, nil
}

// bearerToken returns the request's credential from Authorization: only the
// Bearer scheme (case-insensitive) with a non-empty token counts (§3.3).
func bearerToken(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// keyPrincipal builds the principal of an active key.
func keyPrincipal(k *database.APIKey, actor string) *auth.Principal {
	p := &auth.Principal{
		Kind:    auth.KindKey,
		KeyID:   k.ID,
		KeyName: k.Name,
		Legacy:  k.Legacy,
		Scopes:  k.Scopes,
		Actor:   actor,
	}
	if k.Restriction != nil {
		p.Restrictions = []auth.Restriction{*k.Restriction}
	}
	return p
}

// anonymousPrincipal builds the principal of a request without a
// credential from the anonymous policy. Its restriction applies even while
// access is off, so whoami's restricted reflects the policy.
func anonymousPrincipal(s anonymousSettings, actor string) *auth.Principal {
	p := &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{}, Actor: actor}
	if s.anon.Access == database.AccessListen {
		p.Scopes = auth.Scopes{auth.ScopeListen}
	}
	if s.anon.Restriction != nil {
		p.Restrictions = []auth.Restriction{*s.anon.Restriction}
	}
	return p
}

// resolved is the outcome of principal resolution.
type resolved struct {
	principal *auth.Principal
	key       *database.APIKey // the key record, for key and ticket principals
}

// resolve establishes the request's principal for a route with policy pol
// (§3.3, §8): a ?ticket= on ticket routes (GET/HEAD) first, then the Bearer
// header, else anonymous. A presented but invalid credential is an error,
// never anonymous.
func (a *authenticator) resolve(r *http.Request, pol RoutePolicy) (resolved, *authError) {
	ctx := r.Context()
	ip := a.proxies.ClientIP(r)
	actor := auth.SanitizeActor(r.Header.Get("X-Actor"))

	if pol.Ticket && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if t := r.URL.Query().Get("ticket"); t != "" {
			// Tickets live in many browsers, each with its own IP: they are
			// limited per IP, never charged to the key.
			if !a.allowIP(ip) {
				return resolved{}, errRateLimited
			}
			p, k, aerr := a.resolveTicket(ctx, t, false)
			if aerr != nil {
				return resolved{}, aerr
			}
			p.Actor = actor
			return resolved{principal: p, key: k}, nil
		}
	}

	if token, ok := bearerToken(r); ok {
		k, aerr := a.resolveKey(ctx, token, ip)
		if aerr != nil {
			return resolved{}, aerr
		}
		if k != nil {
			return resolved{principal: keyPrincipal(k, actor), key: k}, nil
		}
		// The retired public token counts as no credential.
	}

	if !a.allowIP(ip) {
		return resolved{}, errRateLimited
	}
	s, err := a.anonymousSettings(ctx)
	if err != nil {
		a.log.Error().Err(err).Msg("auth: reading the anonymous access policy failed")
		return resolved{}, errLookupFailed
	}
	return resolved{principal: anonymousPrincipal(s, actor)}, nil
}

// resolveKey looks up a presented key (§8): the positive cache first, else a
// per-IP token, the negative cache and the database. It returns nil and no
// error for the retired public token, which counts as no credential.
func (a *authenticator) resolveKey(ctx context.Context, token, ip string) (*database.APIKey, *authError) {
	hash := database.HashAPIKey(token)

	s, err := a.anonymousSettings(ctx)
	if err != nil {
		a.log.Error().Err(err).Msg("auth: reading auth settings failed")
		return nil, errLookupFailed
	}
	if s.retiredHash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(s.retiredHash)) == 1 {
		a.warnRetiredToken()
		return nil, nil
	}

	gen := auth.Generation()
	now := a.now()
	if k, ok := a.keys.getByHash(gen, hash); ok {
		if st := k.StatusAt(now); st != database.KeyActive {
			a.keys.putNegative(gen, hash, st)
			return nil, invalidKeyError(st)
		}
		if aerr := a.chargeKey(k, ip); aerr != nil {
			return nil, aerr
		}
		a.touch(k.ID)
		return k, nil
	}

	// Not known to be valid: a guess costs one per-IP token, taken before any
	// lookup.
	if !a.allowIP(ip) {
		return nil, errRateLimited
	}
	if st, ok := a.keys.getNegative(gen, hash); ok {
		return nil, invalidKeyError(st)
	}
	lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
	defer cancel()
	k, err := a.store.ResolveAPIKeyByHash(lctx, hash)
	if errors.Is(err, database.ErrAPIKeyNotFound) {
		a.keys.putNegative(gen, hash, "")
		return nil, invalidKeyError("")
	}
	if err != nil {
		a.log.Error().Err(err).Msg("auth: API key lookup failed")
		return nil, errLookupFailed
	}
	if st := k.StatusAt(now); st != database.KeyActive {
		a.keys.putNegative(gen, hash, st)
		return nil, invalidKeyError(st)
	}
	a.keys.put(gen, hash, k)
	// Legacy keys were already charged the per-IP token above.
	if !k.Legacy && k.RateLimitRPS != nil && !a.allowKey(k) {
		return nil, errRateLimited
	}
	a.touch(k.ID)
	return k, nil
}

// chargeKey applies the limit for a key found in the positive cache: legacy
// keys are limited per IP (they may have been effectively public), keys with
// rate_limit_rps per key, and other keys not at all.
func (a *authenticator) chargeKey(k *database.APIKey, ip string) *authError {
	switch {
	case k.Legacy:
		if !a.allowIP(ip) {
			return errRateLimited
		}
	case k.RateLimitRPS != nil:
		if !a.allowKey(k) {
			return errRateLimited
		}
	}
	return nil
}

// keyByID returns key id's record, from the positive cache unless fresh is
// set. An unknown ID returns nil and no error.
func (a *authenticator) keyByID(ctx context.Context, id int, fresh bool) (*database.APIKey, error) {
	gen := auth.Generation()
	if !fresh {
		if k, ok := a.keys.getByID(gen, id); ok {
			return k, nil
		}
	}
	lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
	defer cancel()
	k, err := a.store.GetAPIKeyByID(lctx, id)
	if errors.Is(err, database.ErrAPIKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if k.ActiveAt(a.now()) {
		a.keys.put(gen, "", k)
	}
	return k, nil
}

// resolveTicket verifies a ticket and builds its principal (§3.5): the MAC
// and expiry with auth.VerifyTicket, then the minting key by ID (which must
// exist, be active and have listen), then the narrowing, which must not name
// a merged-away system. The principal has only listen and carries the key's
// current restriction plus the narrowing. fresh bypasses the key cache (for
// the periodic re-check of long-lived connections).
func (a *authenticator) resolveTicket(ctx context.Context, token string, fresh bool) (*auth.Principal, *database.APIKey, *authError) {
	secret, err := a.ticketSecret(ctx)
	if err != nil {
		a.log.Error().Err(err).Msg("auth: reading the ticket secret failed")
		return nil, nil, errLookupFailed
	}
	now := a.now()
	payload, err := auth.VerifyTicket(secret, token, now)
	if errors.Is(err, auth.ErrTicketExpired) {
		return nil, nil, invalidTicketError("ticket expired")
	}
	if err != nil {
		return nil, nil, invalidTicketError("invalid ticket")
	}
	k, err := a.keyByID(ctx, payload.KeyID, fresh)
	if err != nil {
		a.log.Error().Err(err).Msg("auth: API key lookup for a ticket failed")
		return nil, nil, errLookupFailed
	}
	if k == nil || !k.ActiveAt(now) || !k.Scopes.Has(auth.ScopeListen) {
		return nil, nil, invalidTicketError("the ticket's key is no longer valid")
	}
	if payload.Narrowing != nil {
		merged, err := a.mergedAway(ctx, payload.Narrowing.ReferencedSystems())
		if err != nil {
			a.log.Error().Err(err).Msg("auth: checking a ticket for merged systems failed")
			return nil, nil, errLookupFailed
		}
		if merged {
			return nil, nil, invalidTicketError("the ticket names a merged system; mint a new one")
		}
	}
	p := &auth.Principal{
		Kind:         auth.KindTicket,
		KeyID:        k.ID,
		KeyName:      k.Name,
		Legacy:       k.Legacy,
		Scopes:       auth.Scopes{auth.ScopeListen},
		TicketExpiry: payload.ExpiresAt,
	}
	if k.Restriction != nil {
		p.Restrictions = append(p.Restrictions, *k.Restriction)
	}
	if payload.Narrowing != nil {
		p.Restrictions = append(p.Restrictions, *payload.Narrowing)
	}
	return p, k, nil
}

// mergedAway reports whether any of ids was ever merged into another
// system. Answers are cached for settingsCacheTTL and dropped when the auth
// generation moves (every merge bumps it).
func (a *authenticator) mergedAway(ctx context.Context, ids []int) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	cacheKey := strings.Join(parts, ",")
	gen := auth.Generation()
	now := a.now()

	a.mergedMu.Lock()
	if gen != a.mergedGen {
		clear(a.merged)
		a.mergedGen = gen
	}
	if e, ok := a.merged[cacheKey]; ok && now.Before(e.expires) {
		a.mergedMu.Unlock()
		return e.merged, nil
	}
	a.mergedMu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, credentialLookupTimeout)
	defer cancel()
	merged, err := a.store.AnyMergedAwaySystem(lctx, ids)
	if err != nil {
		return false, err
	}
	a.mergedMu.Lock()
	if gen == a.mergedGen {
		if len(a.merged) >= keyCacheSize {
			clear(a.merged)
		}
		a.merged[cacheKey] = mergedEntry{merged: merged, expires: now.Add(settingsCacheTTL)}
	}
	a.mergedMu.Unlock()
	return merged, nil
}

// invalidateKey drops key id from the caches (after an API PATCH or
// DELETE; the database layer also bumps the auth generation).
func (a *authenticator) invalidateKey(id int) {
	a.keys.invalidate(id)
}

// touch records that key id was used, at most once a minute, without
// delaying the request.
func (a *authenticator) touch(id int) {
	now := a.now()
	a.touchMu.Lock()
	if last, ok := a.touched[id]; ok && now.Sub(last) < lastUsedInterval {
		a.touchMu.Unlock()
		return
	}
	a.touched[id] = now
	a.touchMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.store.TouchAPIKey(ctx, id); err != nil {
			a.log.Warn().Err(err).Int("key_id", id).Msg("auth: recording last_used_at failed")
		}
	}()
}

// warnRetiredToken logs, at most once an hour, that a request carried the
// retired public AUTH_TOKEN (§3.3).
func (a *authenticator) warnRetiredToken() {
	now := a.now()
	a.warnMu.Lock()
	due := a.retiredWarn.IsZero() || now.Sub(a.retiredWarn) >= retiredTokenWarnEvery
	if due {
		a.retiredWarn = now
	}
	a.warnMu.Unlock()
	if due {
		a.log.Warn().Msg("a request carried the pre-upgrade public AUTH_TOKEN — a reverse proxy is probably still injecting it; remove the injection (it is treated as no credential)")
	}
}

// warnUploadRejected logs a rejected call upload, at most once a minute per
// client IP (§11.2).
func (a *authenticator) warnUploadRejected(ip, system, shortName, reason string) {
	now := a.now()
	a.warnMu.Lock()
	last, seen := a.uploadWarn[ip]
	due := !seen || now.Sub(last) >= uploadRejectWarnEvery
	if due {
		if len(a.uploadWarn) >= keyCacheSize {
			clear(a.uploadWarn)
		}
		a.uploadWarn[ip] = now
	}
	a.warnMu.Unlock()
	if !due {
		return
	}
	a.log.Warn().
		Str("client_ip", ip).
		Str("system", system).
		Str("short_name", shortName).
		Str("reason", reason).
		Msg("call upload rejected")
}
