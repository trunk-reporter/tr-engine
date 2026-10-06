package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateMonthlyPartition calls the SQL helper to create a monthly partition.
// The function is idempotent — returns "(already exists)" if the partition exists.
func (db *DB) CreateMonthlyPartition(ctx context.Context, table string, partitionStart time.Time) (string, error) {
	var result string
	err := db.Pool.QueryRow(ctx,
		`SELECT create_monthly_partition($1, $2::date)`,
		table, partitionStart.Format("2006-01-02"),
	).Scan(&result)
	return result, err
}

// CreateMonthlyPartitionForTime creates the partition containing an instant.
// Resolve its month in PostgreSQL, using the same session timezone that the SQL
// helper uses for partition bounds. Formatting a date in Go can select another
// month when the engine and database use different timezones.
func (db *DB) CreateMonthlyPartitionForTime(ctx context.Context, table string, timestamp time.Time) (string, error) {
	var result string
	err := db.Pool.QueryRow(ctx,
		`SELECT create_monthly_partition($1, date_trunc('month', $2::timestamptz)::date)`,
		table, timestamp,
	).Scan(&result)
	return result, err
}

// CreateWeeklyPartition calls the SQL helper to create a weekly partition.
// The function auto-aligns to Monday and is idempotent.
func (db *DB) CreateWeeklyPartition(ctx context.Context, table string, partitionStart time.Time) (string, error) {
	var result string
	err := db.Pool.QueryRow(ctx,
		`SELECT create_weekly_partition($1, $2::date)`,
		table, partitionStart.Format("2006-01-02"),
	).Scan(&result)
	return result, err
}

// DecimateResult holds the counts returned by decimate_state_table().
type DecimateResult struct {
	Deleted1w int64 // rows deleted in 1-week–1-month window (kept 1/min)
	Deleted1m int64 // rows deleted in >1-month window (kept 1/hour)
}

// DecimateStateTable calls the SQL helper to thin out old state rows.
// Phase 1 (1 week – 1 month): keep 1 per minute.
// Phase 2 (> 1 month): keep 1 per hour.
func (db *DB) DecimateStateTable(ctx context.Context, table, timeColumn string) (DecimateResult, error) {
	var r DecimateResult
	err := db.Pool.QueryRow(ctx,
		`SELECT * FROM decimate_state_table($1, $2)`,
		table, timeColumn,
	).Scan(&r.Deleted1w, &r.Deleted1m)
	return r, err
}

// PurgeOlderThan deletes rows older than the given retention period.
// Table and column names are hardcoded by callers (not user input).
func (db *DB) PurgeOlderThan(ctx context.Context, table, timeColumn string, retention time.Duration) (int64, error) {
	query := fmt.Sprintf(
		`DELETE FROM %s WHERE %s < now() - $1::interval`,
		pgx.Identifier{table}.Sanitize(), pgx.Identifier{timeColumn}.Sanitize(),
	)
	tag, err := db.Pool.Exec(ctx, query, retention)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DropOldWeeklyPartitions finds and drops weekly partitions whose upper bound
// is older than the given duration. Returns the names of dropped partitions.
func (db *DB) DropOldWeeklyPartitions(ctx context.Context, parentTable string, olderThan time.Duration) ([]string, error) {
	// Find child partitions with their upper bound timestamps
	rows, err := db.Pool.Query(ctx, `
		SELECT c.relname,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1] AS upper_bound
		FROM pg_inherits i
		JOIN pg_class p ON i.inhparent = p.oid
		JOIN pg_class c ON i.inhrelid = c.oid
		WHERE p.relname = $1
		  AND c.relpartbound IS NOT NULL
		ORDER BY c.relname
	`, parentTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cutoff := time.Now().Add(-olderThan)
	var dropped []string

	type partition struct {
		name       string
		upperBound string
	}
	var candidates []partition

	for rows.Next() {
		var p partition
		if err := rows.Scan(&p.name, &p.upperBound); err != nil {
			return dropped, err
		}
		candidates = append(candidates, p)
	}
	if err := rows.Err(); err != nil {
		return dropped, err
	}

	for _, p := range candidates {
		upper, err := time.Parse("2006-01-02", p.upperBound)
		if err != nil {
			continue // skip partitions with unparseable bounds
		}
		if upper.Before(cutoff) {
			_, err := db.Pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, pgx.Identifier{p.name}.Sanitize()))
			if err != nil {
				return dropped, fmt.Errorf("drop %s: %w", p.name, err)
			}
			dropped = append(dropped, p.name)
		}
	}

	return dropped, nil
}
