package database

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/snarg/tr-engine/internal/auth"
)

// auth_settings rows (schema.sql §25).
const (
	settingAnonymousAccess    = "anonymous_access"
	settingTicketSecret       = "ticket_secret"
	settingRetiredPublicToken = "retired_public_token"
	settingWeakLegacyKeys     = "weak_legacy_keys"
	// settingForgottenPublicTokens keeps the hashes of retired public tokens
	// after `access forget-retired-token`, only so `keys import` keeps
	// refusing them (JSON array of HashAPIKey digests).
	settingForgottenPublicTokens = "forgotten_public_tokens"
)

// ticketSecretLength is the size of the generated ticket secret, in bytes.
const ticketSecretLength = 32

// Anonymous access policies (§3.6): requests without a key may do nothing,
// or listen.
const (
	AccessOff    = "off"
	AccessListen = "listen"
)

// dbtx is the pool or a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AnonymousAccess is the anonymous access policy: what a request without a
// key may do. The restriction is kept while access is off, so an operator can
// prepare it.
type AnonymousAccess struct {
	Access      string            `json:"access"`      // AccessOff or AccessListen
	Restriction *auth.Restriction `json:"restriction"` // nil = unrestricted
	UpdatedAt   *time.Time        `json:"updated_at"`  // nil until first set
}

// anonymousAccessValue is the stored JSON of AnonymousAccess.
type anonymousAccessValue struct {
	Access      string            `json:"access"`
	Restriction *auth.Restriction `json:"restriction"`
}

func decodeAnonymousAccess(raw []byte) (AnonymousAccess, error) {
	var v anonymousAccessValue
	if err := json.Unmarshal(raw, &v); err != nil {
		return AnonymousAccess{}, fmt.Errorf("auth_settings %s: %w", settingAnonymousAccess, err)
	}
	if v.Access != AccessOff && v.Access != AccessListen {
		return AnonymousAccess{}, fmt.Errorf("auth_settings %s: unknown access %q", settingAnonymousAccess, v.Access)
	}
	return AnonymousAccess{Access: v.Access, Restriction: v.Restriction.Normalize()}, nil
}

// GetAnonymousAccess returns the anonymous access policy; it is off with no
// restriction until it is first set. A stored value that doesn't decode, or
// names an access other than off or listen, is an error (never treated as
// any particular policy).
func (db *DB) GetAnonymousAccess(ctx context.Context) (AnonymousAccess, error) {
	var raw []byte
	var updated time.Time
	err := db.Pool.QueryRow(ctx,
		`SELECT value, updated_at FROM auth_settings WHERE name = $1`, settingAnonymousAccess,
	).Scan(&raw, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnonymousAccess{Access: AccessOff}, nil
	}
	if err != nil {
		return AnonymousAccess{}, err
	}
	a, err := decodeAnonymousAccess(raw)
	if err != nil {
		return AnonymousAccess{}, err
	}
	a.UpdatedAt = &updated
	return a, nil
}

// SetAnonymousAccess validates and stores the anonymous access policy, and
// bumps the auth generation. access must be AccessOff or AccessListen; a
// restriction that allows nothing is refused ("use access: off instead").
// A restriction naming a merged-away system is rewritten as the merge would
// have rewritten it (rewriteForMerges). Invalid input is a *FieldError.
func (db *DB) SetAnonymousAccess(ctx context.Context, access string, r *auth.Restriction) (AnonymousAccess, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return AnonymousAccess{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	a, err := setAnonymousAccess(ctx, tx, access, r)
	if err != nil {
		return AnonymousAccess{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AnonymousAccess{}, fmt.Errorf("commit: %w", err)
	}
	auth.Bump()
	return a, nil
}

func setAnonymousAccess(ctx context.Context, tx pgx.Tx, access string, r *auth.Restriction) (AnonymousAccess, error) {
	if access != AccessOff && access != AccessListen {
		return AnonymousAccess{}, fieldErr("access", fmt.Errorf("must be %q or %q", AccessOff, AccessListen))
	}
	// The entry limit applies to new input only: the stored restriction,
	// sent back unchanged (`access set` without restriction flags, the
	// admin pages saving the policy), may have outgrown it through system
	// merges (r2-11).
	validate := r.Validate
	if r != nil {
		stored, err := storedAnonymousRestriction(ctx, tx)
		if err != nil {
			return AnonymousAccess{}, err
		}
		if stored != nil && r.Equal(stored) {
			validate = r.ValidateStored
		}
	}
	if err := validate(auth.KindAnonymous); err != nil {
		return AnonymousAccess{}, fieldErr("restriction", err)
	}
	r, err := rewriteForMerges(ctx, tx, r.Normalize())
	if err != nil {
		return AnonymousAccess{}, err
	}
	v, err := json.Marshal(anonymousAccessValue{Access: access, Restriction: r})
	if err != nil {
		return AnonymousAccess{}, err
	}
	var updated time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO auth_settings (name, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		RETURNING updated_at`, settingAnonymousAccess, v).Scan(&updated); err != nil {
		return AnonymousAccess{}, err
	}
	return AnonymousAccess{Access: access, Restriction: r, UpdatedAt: &updated}, nil
}

// storedAnonymousRestriction returns the anonymous policy's stored
// restriction (nil when none is stored).
func storedAnonymousRestriction(ctx context.Context, tx pgx.Tx) (*auth.Restriction, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM auth_settings WHERE name = $1`, settingAnonymousAccess).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read anonymous access: %w", err)
	}
	a, err := decodeAnonymousAccess(raw)
	if err != nil {
		return nil, err
	}
	return a.Restriction, nil
}

// restrictionMergeLockKey is a transaction-level advisory lock serializing
// the writes of stored restrictions (keys, the anonymous policy) with
// MergeSystems' restriction rewrite: a restriction written around a merge is
// either rewritten by it or finds it in system_merge_log (rewriteForMerges).
// ASCII "tr_restr".
const restrictionMergeLockKey int64 = 0x74725F7265737472

// lockRestrictions takes restrictionMergeLockKey in tx. Nothing that holds a
// row lock on api_keys or auth_settings may take it afterwards: MergeSystems
// takes it before locking those rows.
func lockRestrictions(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, restrictionMergeLockKey); err != nil {
		return fmt.Errorf("lock restrictions against system merges: %w", err)
	}
	return nil
}

// rewriteForMerges brings a restriction about to be stored up to date with
// the system merges already done (§3.2): after taking restrictionMergeLockKey
// it applies every merge in system_merge_log, oldest first, as MergeSystems
// would have (auth.Restriction.RewriteSystem). A restriction written with a
// merged-away system ID (from a page loaded before an automatic merge, or an
// old ID copied from somewhere) would otherwise exclude nothing, because that
// system's data now lives under the target ID. r must be normalized; a nil r,
// or one naming no system, is returned as is without taking the lock.
func rewriteForMerges(ctx context.Context, tx pgx.Tx, r *auth.Restriction) (*auth.Restriction, error) {
	if r == nil || len(r.ReferencedSystems()) == 0 {
		return r, nil
	}
	if err := lockRestrictions(ctx, tx); err != nil {
		return nil, err
	}
	merges, err := readSystemMerges(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := r.Normalize()
	applyMerges(out, merges)
	return out, nil
}

// systemMerge is one system_merge_log row.
type systemMerge struct{ source, target int }

// readSystemMerges returns every logged system merge, oldest first.
func readSystemMerges(ctx context.Context, tx pgx.Tx) ([]systemMerge, error) {
	rows, err := tx.Query(ctx, `SELECT source_id, target_id FROM system_merge_log ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read system merges: %w", err)
	}
	defer rows.Close()
	var merges []systemMerge
	for rows.Next() {
		var m systemMerge
		if err := rows.Scan(&m.source, &m.target); err != nil {
			return nil, err
		}
		merges = append(merges, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read system merges: %w", err)
	}
	return merges, nil
}

// applyMerges rewrites r for merges (oldest first) as MergeSystems does
// (auth.Restriction.RewriteSystem), then mirrors the exclusions over every
// merge until they stop growing, so that an exclusion naming any system of
// a chain of merges names all of them: rows can be written under a
// merged-away ID around its merge and are never moved (r2-07). It reports
// whether r changed.
func applyMerges(r *auth.Restriction, merges []systemMerge) bool {
	changed := false
	for _, m := range merges {
		if r.RewriteSystem(m.source, m.target) {
			changed = true
		}
	}
	// Each pass carries every exclusion at least one merge further along
	// its chain; no chain is longer than len(merges).
	for range merges {
		grew := false
		for _, m := range merges {
			if r.MirrorExclusions(m.source, m.target) {
				grew = true
			}
		}
		if !grew {
			break
		}
		changed = true
	}
	return changed
}

// GetOrCreateTicketSecret returns the ticket signing secret, generating and
// storing 32 random bytes the first time. Concurrent first calls agree on one
// secret. Deleting the row rotates it (and invalidates every ticket).
func (db *DB) GetOrCreateTicketSecret(ctx context.Context) ([]byte, error) {
	b := make([]byte, ticketSecretLength)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate ticket secret: %w", err)
	}
	fresh, err := json.Marshal(base64.StdEncoding.EncodeToString(b))
	if err != nil {
		return nil, err
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO auth_settings (name, value) VALUES ($1, $2)
		ON CONFLICT (name) DO NOTHING`, settingTicketSecret, fresh); err != nil {
		return nil, fmt.Errorf("store ticket secret: %w", err)
	}
	var raw []byte
	if err := db.Pool.QueryRow(ctx,
		`SELECT value FROM auth_settings WHERE name = $1`, settingTicketSecret).Scan(&raw); err != nil {
		return nil, fmt.Errorf("read ticket secret: %w", err)
	}
	var enc string
	if err := json.Unmarshal(raw, &enc); err != nil {
		return nil, fmt.Errorf("auth_settings %s is malformed (delete the row and restart to rotate it): %w", settingTicketSecret, err)
	}
	secret, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(secret) < auth.MinTicketSecretLength {
		return nil, fmt.Errorf("auth_settings %s is malformed (delete the row and restart to rotate it)", settingTicketSecret)
	}
	return secret, nil
}

// validSHA256Hex reports whether s is a lowercase hex SHA-256 digest, the
// form HashAPIKey produces.
func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// GetRetiredPublicTokenHash returns the SHA-256 (HashAPIKey) of the
// pre-upgrade public AUTH_TOKEN, or "" if none is stored. A bearer with this
// hash is treated as no credential (§3.3).
func (db *DB) GetRetiredPublicTokenHash(ctx context.Context) (string, error) {
	var raw []byte
	err := db.Pool.QueryRow(ctx,
		`SELECT value FROM auth_settings WHERE name = $1`, settingRetiredPublicToken).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var hash string
	if err := json.Unmarshal(raw, &hash); err != nil || !validSHA256Hex(hash) {
		return "", fmt.Errorf("auth_settings %s is malformed", settingRetiredPublicToken)
	}
	return hash, nil
}

// SetRetiredPublicTokenHash stores hash (a HashAPIKey digest) as the retired
// public token.
func (db *DB) SetRetiredPublicTokenHash(ctx context.Context, hash string) error {
	return setRetiredPublicTokenHash(ctx, db.Pool, hash)
}

func setRetiredPublicTokenHash(ctx context.Context, q dbtx, hash string) error {
	if !validSHA256Hex(hash) {
		return errors.New("retired public token: not a SHA-256 hex digest")
	}
	v, err := json.Marshal(hash)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO auth_settings (name, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		settingRetiredPublicToken, v)
	return err
}

// ErrRetiredTokenIsKey: an active API key has the retired public token's
// hash (imported before imports refused it), so forgetting the token would
// turn the public value into a working key.
var ErrRetiredTokenIsKey = errors.New("an active API key holds the retired public token")

// ClearRetiredPublicToken forgets the retired public token
// (`tr-engine access forget-retired-token`) and reports whether one was
// stored; a request carrying it then gets 401 invalid_key. Its hash is kept
// in forgotten_public_tokens so `keys import` still refuses it. If an active
// API key has that hash it refuses, with an error wrapping
// ErrRetiredTokenIsKey that names the key: that key must be revoked first.
func (db *DB) ClearRetiredPublicToken(ctx context.Context) (bool, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT value FROM auth_settings WHERE name = $1 FOR UPDATE`, settingRetiredPublicToken).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var hash string
	if err := json.Unmarshal(raw, &hash); err == nil && validSHA256Hex(hash) {
		var id int
		err := tx.QueryRow(ctx, `SELECT id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, hash).Scan(&id)
		if err == nil {
			return false, fmt.Errorf("%w: key #%d; revoke it first (tr-engine keys revoke %d)", ErrRetiredTokenIsKey, id, id)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_settings (name, value, updated_at) VALUES ($1, jsonb_build_array($2::text), now())
			ON CONFLICT (name) DO UPDATE SET value = CASE
				WHEN auth_settings.value @> EXCLUDED.value THEN auth_settings.value
				ELSE auth_settings.value || EXCLUDED.value END, updated_at = now()`,
			settingForgottenPublicTokens, hash); err != nil {
			return false, fmt.Errorf("remember forgotten public token: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_settings WHERE name = $1`, settingRetiredPublicToken); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// IsRetiredPublicToken reports whether hash (a HashAPIKey digest) is the
// retired public token's, or one forgotten with ClearRetiredPublicToken.
func (db *DB) IsRetiredPublicToken(ctx context.Context, hash string) (bool, error) {
	return isRetiredPublicToken(ctx, db.Pool, hash)
}

// isRetiredPublicToken reports whether hash is the retired public token's,
// stored or forgotten: a value the old engine handed to every visitor.
func isRetiredPublicToken(ctx context.Context, q dbtx, hash string) (bool, error) {
	var retired bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auth_settings
			WHERE (name = $1 AND value = to_jsonb($3::text))
			   OR (name = $2 AND jsonb_typeof(value) = 'array' AND value @> jsonb_build_array($3::text)))`,
		settingRetiredPublicToken, settingForgottenPublicTokens, hash).Scan(&retired)
	if err != nil {
		return false, fmt.Errorf("check retired public token: %w", err)
	}
	return retired, nil
}

// WeakLegacyKey is an imported legacy key whose secret is shorter than
// LegacyMinLength characters.
type WeakLegacyKey struct {
	ID     int
	Name   string
	Prefix string
	Length int // characters
}

// recordWeakLegacyKey remembers that key id was imported from a secret of
// length characters under LegacyMinLength, for the warning WeakLegacyKeys
// feeds on every start. Only the length is kept.
func recordWeakLegacyKey(ctx context.Context, tx pgx.Tx, id, length int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO auth_settings (name, value)
		VALUES ($1, jsonb_build_array(jsonb_build_object('id', $2::int, 'length', $3::int)))
		ON CONFLICT (name) DO UPDATE SET value = auth_settings.value || EXCLUDED.value, updated_at = now()`,
		settingWeakLegacyKeys, id, length)
	if err != nil {
		return fmt.Errorf("record weak legacy key: %w", err)
	}
	return nil
}

// WeakLegacyKeys returns the imported legacy keys with a weak secret that are
// still active (not revoked or expired), ordered by ID, for the "legacy key
// #N is weak" warning logged on every start. The result is never nil.
func (db *DB) WeakLegacyKeys(ctx context.Context) ([]WeakLegacyKey, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT k.id, k.name, k.key_prefix, (w ->> 'length')::int
		FROM auth_settings s
		CROSS JOIN LATERAL jsonb_array_elements(s.value) AS w
		JOIN api_keys k ON k.id = (w ->> 'id')::int
		WHERE s.name = $1 AND jsonb_typeof(s.value) = 'array'
		  AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())
		ORDER BY k.id`, settingWeakLegacyKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []WeakLegacyKey{}
	for rows.Next() {
		var k WeakLegacyKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Length); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// rewriteRestrictionsForMerge rewrites every restriction reference to system
// source for its merge into target (§3.2, auth.Restriction.RewriteSystem):
// all api_keys.restriction values and the anonymous policy's restriction.
// MergeSystems calls it inside its transaction, before it logs the merge; the
// caller bumps the auth generation after commit. It takes
// restrictionMergeLockKey first, so restrictions stored concurrently are
// either rewritten here or rewritten on store (rewriteForMerges). A stored
// restriction that doesn't decode fails the merge rather than being left
// pointing at the merged-away system (which would void its exclusions).
func rewriteRestrictionsForMerge(ctx context.Context, tx pgx.Tx, source, target int) (keysChanged int, anonChanged bool, err error) {
	if err := lockRestrictions(ctx, tx); err != nil {
		return 0, false, err
	}
	// This merge, after the logged ones: applyMerges also mirrors the
	// exclusions across earlier merges that this one extends into a chain.
	merges, err := readSystemMerges(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	merges = append(merges, systemMerge{source, target})
	rows, err := tx.Query(ctx,
		`SELECT id, restriction FROM api_keys WHERE restriction IS NOT NULL ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, false, fmt.Errorf("read key restrictions: %w", err)
	}
	type change struct {
		id  int
		val []byte
	}
	var changes []change
	for rows.Next() {
		var id int
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, false, err
		}
		if string(raw) == "null" {
			continue
		}
		r := new(auth.Restriction)
		if err := json.Unmarshal(raw, r); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("api key %d: stored restriction: %w", id, err)
		}
		if !applyMerges(r, merges) {
			continue
		}
		v, err := json.Marshal(r.Normalize())
		if err != nil {
			rows.Close()
			return 0, false, err
		}
		changes = append(changes, change{id, v})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("iterate key restrictions: %w", err)
	}
	for _, c := range changes {
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET restriction = $2 WHERE id = $1`, c.id, c.val); err != nil {
			return 0, false, fmt.Errorf("rewrite restriction of key %d: %w", c.id, err)
		}
	}

	var raw []byte
	err = tx.QueryRow(ctx, `SELECT value FROM auth_settings WHERE name = $1 FOR UPDATE`, settingAnonymousAccess).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return len(changes), false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read anonymous access: %w", err)
	}
	a, err := decodeAnonymousAccess(raw)
	if err != nil {
		return 0, false, err
	}
	if a.Restriction == nil || !applyMerges(a.Restriction, merges) {
		return len(changes), false, nil
	}
	v, err := json.Marshal(anonymousAccessValue{Access: a.Access, Restriction: a.Restriction.Normalize()})
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_settings SET value = $2, updated_at = now() WHERE name = $1`,
		settingAnonymousAccess, v); err != nil {
		return 0, false, fmt.Errorf("rewrite anonymous access restriction: %w", err)
	}
	return len(changes), true, nil
}

// AnyMergedAwaySystem reports whether any of systemIDs was ever the source
// of a system merge. Ticket verification rejects narrowings that name such a
// system (§3.2); the client mints a new ticket.
func (db *DB) AnyMergedAwaySystem(ctx context.Context, systemIDs []int) (bool, error) {
	if len(systemIDs) == 0 {
		return false, nil
	}
	ids := make([]int32, 0, len(systemIDs))
	for _, id := range systemIDs {
		if id > 0 && id <= math.MaxInt32 {
			ids = append(ids, int32(id))
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	var merged bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM system_merge_log WHERE source_id = ANY ($1::int[]))`, ids).Scan(&merged)
	return merged, err
}
