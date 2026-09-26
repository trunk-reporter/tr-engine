package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/snarg/tr-engine/internal/auth"
)

// API key errors that callers map to specific responses.
var (
	ErrAPIKeyNotFound = errors.New("api key not found")
	// ErrAPIKeyRevoked: a revoked key can't be changed (409).
	ErrAPIKeyRevoked = errors.New("api key is revoked")
	// ErrLastAdminKey: the change would leave no active admin key, now or
	// when the remaining ones expire (409). Errors that say which also match
	// it with errors.Is.
	ErrLastAdminKey = errors.New("this is the last active admin key: create another admin key first")
	// ErrRetiredPublicToken: `keys import` of the pre-upgrade public
	// AUTH_TOKEN (§11.2), which is never imported.
	ErrRetiredPublicToken = errors.New("this value is the pre-upgrade public AUTH_TOKEN, which the old engine handed to every visitor, so it can't become a key: create a new key with `tr-engine keys create` and configure the client with that")
	// ErrLegacySecretUnexpanded: a legacy secret contains "$(", an unexpanded
	// shell substitution copied from old docs. It is never imported.
	ErrLegacySecretUnexpanded = errors.New(`value contains "$(" (an unexpanded shell substitution); not imported`)
)

// API key limits and formats (§3.4).
const (
	APIKeyPrefix        = "tre_"
	LegacyKeyPrefix     = "legacy_"
	MaxAPIKeyNameLength = 100 // characters
	// LegacyMinLength is the length below which an imported legacy secret is
	// flagged as weak (it is still imported).
	LegacyMinLength = 16
)

// apiKeyAdminLockKey is a transaction-level advisory lock serializing
// everything that counts active admin keys and then changes one: the
// last-admin guard (PatchAPIKey, RevokeAPIKey) and ClaimBootstrapAdminKey.
// ASCII "tr_admin".
const apiKeyAdminLockKey int64 = 0x74725F61646D696E

// KeyStatus is an API key's computed state.
type KeyStatus string

const (
	KeyActive  KeyStatus = "active"
	KeyExpired KeyStatus = "expired"
	KeyRevoked KeyStatus = "revoked"
)

// APIKey is a client credential (§3.4). The secret is never stored or
// returned after creation; only its SHA-256 is kept, and that stays in the
// database.
type APIKey struct {
	ID           int               `json:"id"`
	Name         string            `json:"name"`
	Prefix       string            `json:"prefix"`
	Scopes       auth.Scopes       `json:"scopes"`
	Restriction  *auth.Restriction `json:"restriction"` // nil = unrestricted; only on ["listen"] keys
	ExpiresAt    *time.Time        `json:"expires_at"`
	RateLimitRPS *float32          `json:"rate_limit_rps"` // nil = no per-key limit
	Legacy       bool              `json:"legacy"`
	CreatedAt    time.Time         `json:"created_at"`
	LastUsedAt   *time.Time        `json:"last_used_at"`
	RevokedAt    *time.Time        `json:"revoked_at"`
	// Status is computed when the key is read. Code that keeps a key around
	// (the key cache) must use StatusAt instead.
	Status KeyStatus `json:"status"`
}

// StatusAt returns the key's state at time now: revoked wins over expired.
func (k *APIKey) StatusAt(now time.Time) KeyStatus {
	switch {
	case k.RevokedAt != nil:
		return KeyRevoked
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return KeyExpired
	}
	return KeyActive
}

// ActiveAt reports whether the key can be used at time now.
func (k *APIKey) ActiveAt(now time.Time) bool {
	return k.StatusAt(now) == KeyActive
}

// APIKeyWithPlaintext is returned only at creation time.
type APIKeyWithPlaintext struct {
	APIKey
	Plaintext string `json:"key"`
}

// NewAPIKey holds the fields of a key to create.
type NewAPIKey struct {
	Name         string
	Scopes       auth.Scopes
	Restriction  *auth.Restriction
	ExpiresAt    *time.Time
	RateLimitRPS *float32
}

// APIKeyPatch is a PATCH /keys/{id}. Each Set* flag says the field was present
// in the request; an absent field is left unchanged. A present Restriction,
// ExpiresAt or RateLimitRPS that is nil clears it. Name and Scopes can't be
// cleared.
type APIKeyPatch struct {
	SetName         bool
	Name            string
	SetScopes       bool
	Scopes          auth.Scopes
	SetRestriction  bool
	Restriction     *auth.Restriction
	SetExpiresAt    bool
	ExpiresAt       *time.Time
	SetRateLimitRPS bool
	RateLimitRPS    *float32
}

// LastAdminGuard says whether PatchAPIKey and RevokeAPIKey refuse to take away
// the last active admin key (the API does), or not (the CLI, §10.2).
type LastAdminGuard bool

const (
	GuardLastAdmin   LastAdminGuard = true
	NoLastAdminGuard LastAdminGuard = false
)

// FieldError reports an invalid field of a key or of the anonymous access
// policy; the API answers 400 with it.
type FieldError struct {
	Field string // JSON field name, e.g. "expires_at"
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(field string, err error) error { return &FieldError{Field: field, Err: err} }

// GenerateAPIKey creates a new API key with the tre_ prefix.
// Returns the plaintext key (show once) and its SHA-256 hash (store in DB).
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}
	plaintext = APIKeyPrefix + hex.EncodeToString(b)
	prefix = plaintext[:12] // "tre_" + 8 hex chars

	h := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(h[:])
	return plaintext, hash, prefix, nil
}

// HashAPIKey returns the SHA-256 hex hash of a presented credential. Lookups
// hash whatever bearer value is presented; "tre_" is a convention only.
func HashAPIKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// legacyPrefix is the display prefix of an imported legacy secret: "legacy_"
// and the first 6 hex characters of its hash, so no character of the secret
// itself is ever stored or shown.
func legacyPrefix(hash string) string {
	return LegacyKeyPrefix + hash[:6]
}

// CheckLegacySecret applies the legacy strength rules (§11.2) to a secret
// about to be imported: a value containing "$(" is refused
// (ErrLegacySecretUnexpanded), and one shorter than LegacyMinLength
// characters is weak but accepted.
func CheckLegacySecret(secret string) (weak bool, err error) {
	if secret == "" {
		return false, errors.New("value is empty")
	}
	if strings.Contains(secret, "$(") {
		return false, ErrLegacySecretUnexpanded
	}
	return utf8.RuneCountInString(secret) < LegacyMinLength, nil
}

// NormalizeAPIKeyName trims a key name and checks it: 1 to 100 characters,
// no control or format characters (names end up in logs and admin pages).
func NormalizeAPIKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 {
		return "", fieldErr("name", errors.New("is required"))
	}
	if n > MaxAPIKeyNameLength {
		return "", fieldErr("name", fmt.Errorf("must be at most %d characters", MaxAPIKeyNameLength))
	}
	if !utf8.ValidString(name) {
		return "", fieldErr("name", errors.New("is not valid UTF-8"))
	}
	for _, r := range name {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return "", fieldErr("name", errors.New("must not contain control characters"))
		}
	}
	return name, nil
}

// validateKeyFields checks everything about a key except its name, as it
// will be stored: the scopes and restriction together (auth.ValidateKey, or
// auth.ValidateStoredKey when the restriction is the stored one rather than
// newRestriction input), an expiry after now, and a positive, finite rate
// limit. It returns the normalized scopes and restriction.
func validateKeyFields(scopes auth.Scopes, r *auth.Restriction, newRestriction bool, expiresAt *time.Time, rps *float32, now time.Time) (auth.Scopes, *auth.Restriction, error) {
	if err := scopes.Validate(); err != nil {
		return nil, nil, fieldErr("scopes", err)
	}
	scopes = scopes.Normalize()
	r = r.Normalize()
	// The entry limit applies to a restriction the request sets, not to the
	// stored one, which system merges may have grown past it (r2-11).
	validate := auth.ValidateKey
	if !newRestriction {
		validate = auth.ValidateStoredKey
	}
	if err := validate(scopes, r); err != nil {
		return nil, nil, fieldErr("restriction", err)
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return nil, nil, fieldErr("expires_at", errors.New("must be in the future"))
	}
	if rps != nil {
		if v := float64(*rps); math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return nil, nil, fieldErr("rate_limit_rps", errors.New("must be a number greater than 0"))
		}
	}
	return scopes, r, nil
}

func restrictionJSON(r *auth.Restriction) ([]byte, error) {
	if r == nil {
		return nil, nil // SQL NULL
	}
	return json.Marshal(r.Normalize())
}

// apiKeyColumns are the columns scanAPIKey reads, in order.
const apiKeyColumns = `id, name, key_prefix, scopes, restriction, expires_at, rate_limit_rps,
	legacy, created_at, last_used_at, revoked_at`

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	var scopes []string
	var restriction []byte
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &scopes, &restriction, &k.ExpiresAt, &k.RateLimitRPS,
		&k.Legacy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
		return nil, err
	}
	k.Scopes = make(auth.Scopes, len(scopes))
	for i, s := range scopes {
		k.Scopes[i] = auth.Scope(s)
	}
	if restriction != nil && string(restriction) != "null" {
		r := new(auth.Restriction)
		if err := json.Unmarshal(restriction, r); err != nil {
			return nil, fmt.Errorf("api key %d: stored restriction: %w", k.ID, err)
		}
		k.Restriction = r.Normalize()
	}
	k.Status = k.StatusAt(time.Now())
	return &k, nil
}

// CreateAPIKey validates and stores a new key and returns it with its
// plaintext, which is never available again.
func (db *DB) CreateAPIKey(ctx context.Context, in NewAPIKey) (*APIKeyWithPlaintext, error) {
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, err
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	key, err := insertAPIKey(ctx, tx, hash, prefix, false, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &APIKeyWithPlaintext{APIKey: *key, Plaintext: plaintext}, nil
}

// CreateLegacyAPIKey imports an existing secret (an old AUTH_TOKEN or
// WRITE_TOKEN, or one given to `tr-engine keys import`): it stores the hash
// with legacy = true and a "legacy_" prefix. The strength rules apply
// (CheckLegacySecret): "$(" is refused, and a weak secret is imported and
// remembered for WeakLegacyKeys. The retired public AUTH_TOKEN, stored or
// forgotten, is refused (ErrRetiredPublicToken). A secret whose hash is
// already stored is not duplicated: the existing key is returned with
// created = false, unchanged.
func (db *DB) CreateLegacyAPIKey(ctx context.Context, secret string, in NewAPIKey) (key *APIKey, created bool, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	key, created, err = createLegacyAPIKey(ctx, tx, secret, in)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return key, created, nil
}

func createLegacyAPIKey(ctx context.Context, tx pgx.Tx, secret string, in NewAPIKey) (*APIKey, bool, error) {
	weak, err := CheckLegacySecret(secret)
	if err != nil {
		return nil, false, err
	}
	hash := HashAPIKey(secret)
	if retired, err := isRetiredPublicToken(ctx, tx, hash); err != nil {
		return nil, false, err
	} else if retired {
		return nil, false, ErrRetiredPublicToken
	}
	if existing, err := scanAPIKey(tx.QueryRow(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = $1`, hash)); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	key, err := insertAPIKey(ctx, tx, hash, legacyPrefix(hash), true, in)
	if err != nil {
		return nil, false, err
	}
	if weak {
		if err := recordWeakLegacyKey(ctx, tx, key.ID, utf8.RuneCountInString(secret)); err != nil {
			return nil, false, err
		}
	}
	return key, true, nil
}

// insertAPIKey validates and inserts a key. A restriction naming a
// merged-away system is rewritten as the merge would have rewritten it
// (rewriteForMerges).
func insertAPIKey(ctx context.Context, tx pgx.Tx, hash, prefix string, legacy bool, in NewAPIKey) (*APIKey, error) {
	name, err := NormalizeAPIKeyName(in.Name)
	if err != nil {
		return nil, err
	}
	scopes, r, err := validateKeyFields(in.Scopes, in.Restriction, true, in.ExpiresAt, in.RateLimitRPS, time.Now())
	if err != nil {
		return nil, err
	}
	if r, err = rewriteForMerges(ctx, tx, r); err != nil {
		return nil, err
	}
	rj, err := restrictionJSON(r)
	if err != nil {
		return nil, err
	}
	return scanAPIKey(tx.QueryRow(ctx, `
		INSERT INTO api_keys (key_hash, key_prefix, name, scopes, restriction, expires_at, rate_limit_rps, legacy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+apiKeyColumns,
		hash, prefix, name, scopes.Strings(), rj, in.ExpiresAt, in.RateLimitRPS, legacy))
}

// ResolveAPIKeyByHash looks a presented credential up by its SHA-256
// (HashAPIKey). Revoked and expired keys are returned too, with Status set,
// so the caller can say why the key was rejected; an unknown hash is
// ErrAPIKeyNotFound.
func (db *DB) ResolveAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	k, err := scanAPIKey(db.Pool.QueryRow(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	return k, err
}

// GetAPIKeyByID returns a key by ID, including revoked and expired keys
// (see Status); an unknown ID is ErrAPIKeyNotFound.
func (db *DB) GetAPIKeyByID(ctx context.Context, id int) (*APIKey, error) {
	k, err := scanAPIKey(db.Pool.QueryRow(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	return k, err
}

// FindAPIKeysByPrefix returns every key whose display prefix is prefix
// (for `tr-engine keys ... --prefix`), ordered by ID. Prefixes are not
// unique, so the caller decides what more than one match means.
func (db *DB) FindAPIKeysByPrefix(ctx context.Context, prefix string) ([]APIKey, error) {
	return db.listAPIKeys(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE key_prefix = $1 ORDER BY id`, prefix)
}

// ListAPIKeys returns keys ordered by ID: active and expired ones, plus
// revoked ones when includeRevoked is set. The result is never nil.
func (db *DB) ListAPIKeys(ctx context.Context, includeRevoked bool) ([]APIKey, error) {
	return db.listAPIKeys(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
		WHERE $1 OR revoked_at IS NULL ORDER BY id`, includeRevoked)
}

func (db *DB) listAPIKeys(ctx context.Context, sql string, args ...any) ([]APIKey, error) {
	rows, err := db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, *k)
	}
	return keys, rows.Err()
}

// TouchAPIKey records that a key was used. It writes at most once a minute
// per key; the caller should also throttle.
func (db *DB) TouchAPIKey(ctx context.Context, id int) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
	return err
}

// ActiveAdminKeyExists reports whether any key with admin is neither revoked
// nor expired.
func (db *DB) ActiveAdminKeyExists(ctx context.Context) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE `+activeAdminKeySQL+`)`).Scan(&exists)
	return exists, err
}

// activeAdminKeySQL selects keys that hold admin and are neither revoked nor
// expired.
const activeAdminKeySQL = `'admin' = ANY (scopes) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`

// lockAdminKeys takes apiKeyAdminLockKey and then the key's row lock, and
// returns the key as it is now. The advisory lock comes first so that two
// transactions guarding different keys serialize before either counts.
func lockAdminKeys(ctx context.Context, tx pgx.Tx, id int) (*APIKey, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, apiKeyAdminLockKey); err != nil {
		return nil, fmt.Errorf("lock admin keys: %w", err)
	}
	k, err := scanAPIKey(tx.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	return k, err
}

// MinUsableAdminRPS is the lowest per-key rate limit (requests per second)
// at which an admin key still counts as able to undo changes for the
// last-admin guard: below it the key gets a single request per 1/rps
// seconds (a burst of 1), too few to reach the PATCH that lifts the limit.
const MinUsableAdminRPS = 1.0

// usableAdminKeySQL narrows activeAdminKeySQL to keys that are not
// rate-limited below MinUsableAdminRPS.
const usableAdminKeySQL = `(rate_limit_rps IS NULL OR rate_limit_rps >= 1)`

// lastAdminError is an ErrLastAdminKey with a more specific message.
type lastAdminError struct{ msg string }

func (e *lastAdminError) Error() string        { return e.msg }
func (e *lastAdminError) Is(target error) bool { return target == ErrLastAdminKey }

// checkLastAdmin guards a change that ends, shortens or throttles key's
// admin access (revoking it, removing admin, moving its expiry earlier, or
// rate-limiting it below MinUsableAdminRPS): it returns ErrLastAdminKey
// unless another active admin key, not rate-limited below
// MinUsableAdminRPS, lasts at least as long as key does now (it has no
// expiry, or one no earlier than key's). Otherwise a near-future expiry, a
// rate limit of one request an hour, or shortening one admin key and then
// revoking the other, would leave no usable admin key as surely as revoking
// the last one. A key that isn't an active admin key is not guarded. The
// caller holds apiKeyAdminLockKey.
func checkLastAdmin(ctx context.Context, tx pgx.Tx, key *APIKey) error {
	if !key.Scopes.Has(auth.ScopeAdmin) || !key.ActiveAt(time.Now()) {
		return nil
	}
	var others, outlasting bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM api_keys WHERE id <> $1 AND `+activeAdminKeySQL+` AND `+usableAdminKeySQL+`),
		       EXISTS (SELECT 1 FROM api_keys WHERE id <> $1 AND `+activeAdminKeySQL+` AND `+usableAdminKeySQL+`
		               AND (expires_at IS NULL OR ($2::timestamptz IS NOT NULL AND expires_at >= $2)))`,
		key.ID, key.ExpiresAt,
	).Scan(&others, &outlasting); err != nil {
		return fmt.Errorf("count admin keys: %w", err)
	}
	switch {
	case !others:
		return ErrLastAdminKey
	case !outlasting:
		return &lastAdminError{msg: "every other active admin key expires before this one, so this change would eventually leave no admin key: " +
			"give another admin key a later expiry (or none) first"}
	}
	return nil
}

// PatchAPIKey applies p to key id and returns the result. It fails with
// ErrAPIKeyNotFound, ErrAPIKeyRevoked, a *FieldError (including the
// scopes/restriction combination: a restricted key must clear its
// restriction in the same patch that changes its scopes away from
// ["listen"]), or, with GuardLastAdmin, ErrLastAdminKey when the patch would
// take admin away from, set an earlier expiry on, or set a rate limit below
// MinUsableAdminRPS on, an active admin key that no other usable active
// admin key outlasts (checkLastAdmin). It bumps the auth
// generation after a successful change.
func (db *DB) PatchAPIKey(ctx context.Context, id int, p APIKeyPatch, guard LastAdminGuard) (*APIKey, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// A new restriction is rewritten for past merges (rewriteForMerges),
	// under restrictionMergeLockKey, which must come before the key's row
	// lock (see lockRestrictions).
	if p.SetRestriction && len(p.Restriction.ReferencedSystems()) > 0 {
		if err := lockRestrictions(ctx, tx); err != nil {
			return nil, err
		}
	}
	cur, err := lockAdminKeys(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if cur.RevokedAt != nil {
		return nil, ErrAPIKeyRevoked
	}

	next := *cur
	if p.SetName {
		if next.Name, err = NormalizeAPIKeyName(p.Name); err != nil {
			return nil, err
		}
	}
	if p.SetScopes {
		next.Scopes = p.Scopes
	}
	if p.SetRestriction {
		next.Restriction = p.Restriction
	}
	if p.SetExpiresAt {
		next.ExpiresAt = p.ExpiresAt
	}
	if p.SetRateLimitRPS {
		next.RateLimitRPS = p.RateLimitRPS
	}
	// Only the fields being set must pass the time-dependent checks: an
	// untouched expiry in the past (an expired key) stays as it is.
	expiry := next.ExpiresAt
	if !p.SetExpiresAt {
		expiry = nil
	}
	// A restriction sent back unchanged (the admin pages send it with every
	// edit) is the stored one, not new input (see validateKeyFields).
	newRestriction := p.SetRestriction && !p.Restriction.Equal(cur.Restriction)
	if next.Scopes, next.Restriction, err = validateKeyFields(next.Scopes, next.Restriction, newRestriction, expiry, next.RateLimitRPS, time.Now()); err != nil {
		return nil, err
	}
	if p.SetRestriction {
		if next.Restriction, err = rewriteForMerges(ctx, tx, next.Restriction); err != nil {
			return nil, err
		}
	}

	if guard == GuardLastAdmin && cur.Scopes.Has(auth.ScopeAdmin) {
		demoted := !next.Scopes.Has(auth.ScopeAdmin)
		// An expiry earlier than the current one (or than none) shortens the
		// key's admin access just as surely.
		shortened := p.SetExpiresAt && next.ExpiresAt != nil &&
			(cur.ExpiresAt == nil || next.ExpiresAt.Before(*cur.ExpiresAt))
		// So does a new or lower rate limit that leaves the key too few
		// requests to lift it again.
		throttled := p.SetRateLimitRPS && next.RateLimitRPS != nil && *next.RateLimitRPS < MinUsableAdminRPS &&
			(cur.RateLimitRPS == nil || *next.RateLimitRPS < *cur.RateLimitRPS)
		if demoted || shortened || throttled {
			if err := checkLastAdmin(ctx, tx, cur); err != nil {
				if throttled && !demoted && !shortened {
					return nil, &lastAdminError{msg: fmt.Sprintf(
						"a rate limit below %g request/second on this admin key would leave no admin key that can lift it: "+
							"create another admin key without a rate limit first", MinUsableAdminRPS)}
				}
				return nil, err
			}
		}
	}

	rj, err := restrictionJSON(next.Restriction)
	if err != nil {
		return nil, err
	}
	updated, err := scanAPIKey(tx.QueryRow(ctx, `
		UPDATE api_keys SET name = $2, scopes = $3, restriction = $4, expires_at = $5, rate_limit_rps = $6
		WHERE id = $1
		RETURNING `+apiKeyColumns,
		id, next.Name, next.Scopes.Strings(), rj, next.ExpiresAt, next.RateLimitRPS))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	auth.Bump()
	return updated, nil
}

// RevokeAPIKey revokes key id (sets revoked_at) and returns it. Revoking a
// revoked key changes nothing and succeeds. With GuardLastAdmin it refuses
// (ErrLastAdminKey) to revoke an active admin key that no other active admin
// key outlasts (checkLastAdmin). It bumps the auth
// generation after a revocation.
func (db *DB) RevokeAPIKey(ctx context.Context, id int, guard LastAdminGuard) (*APIKey, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	cur, err := lockAdminKeys(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if cur.RevokedAt != nil {
		return cur, nil
	}
	if guard == GuardLastAdmin {
		if err := checkLastAdmin(ctx, tx, cur); err != nil {
			return nil, err
		}
	}
	revoked, err := scanAPIKey(tx.QueryRow(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 RETURNING `+apiKeyColumns, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	auth.Bump()
	return revoked, nil
}
