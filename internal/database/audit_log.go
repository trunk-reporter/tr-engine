package database

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// AuditEntry is one state-changing API request made with a key (§9).
type AuditEntry struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	KeyID     int       `json:"key_id"`
	KeyName   string    `json:"key_name"`
	Actor     *string   `json:"actor"` // sanitized X-Actor header; nil when absent
	Method    string    `json:"method"`
	Path      string    `json:"path"` // without the query string
	Status    int       `json:"status"`
	RequestID string    `json:"request_id"`
}

// MaxAuditPathBytes caps the path stored in an audit entry. A route
// parameter matches a segment of any length, so without a cap one request
// could store a path of up to the header size limit.
const MaxAuditPathBytes = 1024

// TruncateAuditPath cuts anything from a '?' on, and a path longer than
// MaxAuditPathBytes at a UTF-8 boundary, marking how long it was. Apply it
// once, to the path as requested: its result can itself be longer than
// MaxAuditPathBytes, and truncating that again would record the result's
// length instead of the request's.
func TruncateAuditPath(path string) string {
	path, _, _ = strings.Cut(path, "?")
	if len(path) <= MaxAuditPathBytes {
		return path
	}
	cut := MaxAuditPathBytes
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return path[:cut] + "…(truncated, " + strconv.Itoa(len(path)) + " bytes)"
}

// InsertAuditLog records e. ID and Time are assigned by the database; an
// empty Actor is stored as NULL, and Path, the path as requested, goes
// through TruncateAuditPath here (callers pass it untruncated).
func (db *DB) InsertAuditLog(ctx context.Context, e AuditEntry) error {
	path := TruncateAuditPath(e.Path)
	var actor *string
	if e.Actor != nil && *e.Actor != "" {
		actor = e.Actor
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO audit_log (key_id, key_name, actor, method, path, status, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))`,
		e.KeyID, e.KeyName, actor, e.Method, path, e.Status, e.RequestID)
	return err
}

// AuditLogFilter selects audit entries: all set fields are ANDed. Since is
// inclusive and Until exclusive.
type AuditLogFilter struct {
	KeyID  *int
	Since  *time.Time
	Until  *time.Time
	Limit  int // default 50, at most 1000
	Offset int
}

// ListAuditLog returns the matching entries, newest first, and how many
// match in total. The result is never nil.
func (db *DB) ListAuditLog(ctx context.Context, f AuditLogFilter) ([]AuditEntry, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	f.Limit = min(f.Limit, 1000)
	f.Offset = max(f.Offset, 0)

	const where = `
		WHERE ($1::int IS NULL OR key_id = $1)
		  AND ($2::timestamptz IS NULL OR "time" >= $2)
		  AND ($3::timestamptz IS NULL OR "time" < $3)`
	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`+where,
		f.KeyID, f.Since, f.Until).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id, "time", key_id, key_name, actor, method, path, status, COALESCE(request_id, '')
		FROM audit_log`+where+`
		ORDER BY "time" DESC, id DESC
		LIMIT $4 OFFSET $5`,
		f.KeyID, f.Since, f.Until, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Time, &e.KeyID, &e.KeyName, &e.Actor, &e.Method, &e.Path,
			&e.Status, &e.RequestID); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// PurgeAuditLogOlderThan deletes audit entries older than retention
// (RETENTION_AUDIT_LOG). A retention that is not positive is refused rather
// than deleting the whole log.
func (db *DB) PurgeAuditLogOlderThan(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, errors.New("audit log retention must be positive")
	}
	tag, err := db.Pool.Exec(ctx, `DELETE FROM audit_log WHERE "time" < now() - $1::interval`, retention)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
