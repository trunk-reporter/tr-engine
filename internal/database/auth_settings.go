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
// Invalid input is a *FieldError.
func (db *DB) SetAnonymousAccess(ctx context.Context, access string, r *auth.Restriction) (AnonymousAccess, error) {
	a, err := setAnonymousAccess(ctx, db.Pool, access, r)
	if err != nil {
		return AnonymousAccess{}, err
	}
	auth.Bump()
	return a, nil
}

func setAnonymousAccess(ctx context.Context, q dbtx, access string, r *auth.Restriction) (AnonymousAccess, error) {
	if access != AccessOff && access != AccessListen {
		return AnonymousAccess{}, fieldErr("access", fmt.Errorf("must be %q or %q", AccessOff, AccessListen))
	}
	if err := r.Validate(auth.KindAnonymous); err != nil {
		return AnonymousAccess{}, fieldErr("restriction", err)
	}
	v, err := json.Marshal(anonymousAccessValue{Access: access, Restriction: r.Normalize()})
	if err != nil {
		return AnonymousAccess{}, err
	}
	var updated time.Time
	if err := q.QueryRow(ctx, `
		INSERT INTO auth_settings (name, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		RETURNING updated_at`, settingAnonymousAccess, v).Scan(&updated); err != nil {
		return AnonymousAccess{}, err
	}
	return AnonymousAccess{Access: access, Restriction: r.Normalize(), UpdatedAt: &updated}, nil
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

// ClearRetiredPublicToken forgets the retired public token
// (`tr-engine access forget-retired-token`) and reports whether one was
// stored.
func (db *DB) ClearRetiredPublicToken(ctx context.Context) (bool, error) {
	tag, err := db.Pool.Exec(ctx, `DELETE FROM auth_settings WHERE name = $1`, settingRetiredPublicToken)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
// source into target (§3.2): all api_keys.restriction values and the
// anonymous policy's restriction. MergeSystems calls it inside its
// transaction; the caller bumps the auth generation after commit. A stored
// restriction that doesn't decode fails the merge rather than being left
// pointing at the merged-away system (which would void its exclusions).
func rewriteRestrictionsForMerge(ctx context.Context, tx pgx.Tx, source, target int) (keysChanged int, anonChanged bool, err error) {
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
		if !r.RewriteSystem(source, target) {
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
	if !a.Restriction.RewriteSystem(source, target) {
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
