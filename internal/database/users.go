package database

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// User represents a user in the system.
type User struct {
	ID           int        `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"` // never expose in API responses
	Role         string     `json:"role"`
	Enabled      bool       `json:"enabled"`
	DisplayName  string     `json:"display_name,omitempty"`
	LastLogin    *time.Time `json:"last_login,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// NormalizeUsername lowercases and trims the username (which must be an email).
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// ValidateUsername checks that the username looks like an email address.
func ValidateUsername(username string) bool {
	u := NormalizeUsername(username)
	if len(u) < 3 || len(u) > 254 {
		return false
	}
	at := strings.IndexByte(u, '@')
	dot := strings.LastIndexByte(u, '.')
	return at > 0 && dot > at+1 && dot < len(u)-1
}

// GetUserByUsername looks up a user by username for login.
func (db *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at
		 FROM users WHERE username = $1`, NormalizeUsername(username),
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID looks up a user by ID for JWT validation.
func (db *DB) GetUserByID(ctx context.Context, id int) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at
		 FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns all users (for admin endpoint).
func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at
		 FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CreateUser inserts a new user with an already-hashed password.
// Username is normalized (lowercased).
func (db *DB) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	normalized := NormalizeUsername(username)
	var u User
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role)
		 VALUES ($1, $2, $3)
		 RETURNING id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at`,
		normalized, passwordHash, role,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateFirstUser atomically creates the first user only if no users exist.
// Returns (nil, nil) if users already exist (not an error — setup already done).
// This prevents the TOCTOU race in POST /auth/setup where two concurrent
// requests could both pass a CountUsers==0 check and create two admins.
func (db *DB) CreateFirstUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	normalized := NormalizeUsername(username)
	var u User
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role)
		 SELECT $1, $2, $3
		 WHERE NOT EXISTS (SELECT 1 FROM users)
		 RETURNING id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at`,
		normalized, passwordHash, role,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil // users already exist
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateLastLogin sets the last_login timestamp for a user.
func (db *DB) UpdateLastLogin(ctx context.Context, id int) error {
	_, err := db.Pool.Exec(ctx, `UPDATE users SET last_login = now() WHERE id = $1`, id)
	return err
}

// UserUpdate holds optional fields for a partial user update.
type UserUpdate struct {
	Role         *string
	PasswordHash *string
	Enabled      *bool
}

// UpdateUser applies a partial update to an existing user.
func (db *DB) UpdateUser(ctx context.Context, id int, upd UserUpdate) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`UPDATE users SET
			role = COALESCE($2, role),
			password_hash = COALESCE($3, password_hash),
			enabled = COALESCE($4, enabled)
		 WHERE id = $1
		 RETURNING id, username, password_hash, role, enabled, display_name, last_login, created_at, updated_at`,
		id, upd.Role, upd.PasswordHash, upd.Enabled,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.DisplayName, &u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteUser removes a user by ID. API keys are cascade-deleted via FK.
func (db *DB) DeleteUser(ctx context.Context, id int) error {
	ct, err := db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountAdmins returns the number of admin users (for last-admin protection).
func (db *DB) CountAdmins(ctx context.Context) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND enabled = true`).Scan(&count)
	return count, err
}

// CountUsers returns the total number of users (used for seeding check).
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count)
	return count, err
}

// DeleteAllUsers removes all users (and their API keys via CASCADE).
// Used for the downgrade-to-token-auth admin action.
func (db *DB) DeleteAllUsers(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM users`)
	return err
}
