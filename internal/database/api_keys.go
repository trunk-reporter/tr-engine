package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIKey represents a named API key with role-based permissions.
type APIKey struct {
	ID               int        `json:"id"`
	KeyPrefix        string     `json:"key_prefix"`
	UserID           *int       `json:"user_id,omitempty"`
	Username         *string    `json:"username,omitempty"` // populated in admin list view
	Role             string     `json:"role"`
	Label            string     `json:"label"`
	IsServiceAccount bool       `json:"is_service_account"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
}

// APIKeyWithPlaintext is returned only at creation time.
type APIKeyWithPlaintext struct {
	APIKey
	Plaintext string `json:"key"`
}

// GenerateAPIKey creates a new API key with the tre_ prefix.
// Returns the plaintext key (show once) and its SHA-256 hash (store in DB).
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}
	plaintext = "tre_" + hex.EncodeToString(b)
	prefix = plaintext[:12] // "tre_" + 8 hex chars

	h := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(h[:])
	return plaintext, hash, prefix, nil
}

// HashAPIKey returns the SHA-256 hex hash of a plaintext API key.
func HashAPIKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// CreateAPIKey inserts a new API key and returns it with the plaintext (shown once).
func (db *DB) CreateAPIKey(ctx context.Context, userID *int, role, label string, isServiceAccount bool) (*APIKeyWithPlaintext, error) {
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, err
	}

	var key APIKey
	err = db.Pool.QueryRow(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, user_id, role, label, is_service_account)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, key_prefix, user_id, role, label, is_service_account, created_at, last_used_at`,
		hash, prefix, userID, role, label, isServiceAccount,
	).Scan(&key.ID, &key.KeyPrefix, &key.UserID, &key.Role, &key.Label, &key.IsServiceAccount, &key.CreatedAt, &key.LastUsedAt)
	if err != nil {
		return nil, err
	}

	return &APIKeyWithPlaintext{APIKey: key, Plaintext: plaintext}, nil
}

// ResolveAPIKey looks up an API key by its plaintext value (hashed for lookup).
// Returns nil if not found.
func (db *DB) ResolveAPIKey(ctx context.Context, plaintext string) (*APIKey, error) {
	hash := HashAPIKey(plaintext)

	var key APIKey
	err := db.Pool.QueryRow(ctx,
		`SELECT id, key_prefix, user_id, role, label, is_service_account, created_at, last_used_at
		 FROM api_keys WHERE key_hash = $1`, hash,
	).Scan(&key.ID, &key.KeyPrefix, &key.UserID, &key.Role, &key.Label, &key.IsServiceAccount, &key.CreatedAt, &key.LastUsedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// TouchAPIKey updates last_used_at for an API key.
func (db *DB) TouchAPIKey(ctx context.Context, id int) error {
	_, err := db.Pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// ListAPIKeysByUser returns all keys owned by a user.
func (db *DB) ListAPIKeysByUser(ctx context.Context, userID int) ([]APIKey, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, key_prefix, user_id, role, label, is_service_account, created_at, last_used_at
		 FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAPIKeys(rows)
}

// ListAllAPIKeys returns all keys with owner username (admin view).
func (db *DB) ListAllAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT ak.id, ak.key_prefix, ak.user_id, ak.role, ak.label, ak.is_service_account, ak.created_at, ak.last_used_at, u.username
		 FROM api_keys ak
		 LEFT JOIN users u ON u.id = ak.user_id
		 ORDER BY ak.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.UserID, &k.Role, &k.Label, &k.IsServiceAccount, &k.CreatedAt, &k.LastUsedAt, &k.Username); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteAPIKey deletes a key by ID. Returns pgx.ErrNoRows if not found.
func (db *DB) DeleteAPIKey(ctx context.Context, id int) error {
	ct, err := db.Pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteAPIKeyOwned deletes a key only if owned by the given user.
func (db *DB) DeleteAPIKeyOwned(ctx context.Context, id, userID int) error {
	ct, err := db.Pool.Exec(ctx,
		`DELETE FROM api_keys WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteAPIKeysByUser deletes all keys owned by a user (cascade on user delete).
func (db *DB) DeleteAPIKeysByUser(ctx context.Context, userID int) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1`, userID)
	return err
}

// GetAPIKeyByID returns a single key by ID.
func (db *DB) GetAPIKeyByID(ctx context.Context, id int) (*APIKey, error) {
	var key APIKey
	err := db.Pool.QueryRow(ctx,
		`SELECT id, key_prefix, user_id, role, label, is_service_account, created_at, last_used_at
		 FROM api_keys WHERE id = $1`, id,
	).Scan(&key.ID, &key.KeyPrefix, &key.UserID, &key.Role, &key.Label, &key.IsServiceAccount, &key.CreatedAt, &key.LastUsedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

func scanAPIKeys(rows pgx.Rows) ([]APIKey, error) {
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.UserID, &k.Role, &k.Label, &k.IsServiceAccount, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
