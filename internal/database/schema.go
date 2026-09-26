package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaLockKey is a session-level advisory lock that serializes schema
// setup (ApplySchemaIfEmpty, Migrate) between processes: two engines, or a
// `tr-engine keys ...` command run while the server starts, on one database.
// Without it both evaluate the same migrations as pending and one fails
// half-way with an error that misleadingly asks for superuser SQL. ASCII
// "tr_schem".
const schemaLockKey int64 = 0x74725F736368656D

// withSchemaLock runs fn on one pooled connection that holds schemaLockKey,
// waiting for another process to finish its schema setup first.
func (db *DB) withSchemaLock(ctx context.Context, fn func(conn *pgxpool.Conn) error) error {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for schema setup: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schemaLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("lock schema setup: %w", err)
	}
	if !locked {
		db.log.Info().Msg("another tr-engine process is setting up the database schema; waiting for it")
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, schemaLockKey); err != nil {
			return fmt.Errorf("lock schema setup: %w", err)
		}
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, schemaLockKey); err != nil {
			// Never hand a connection that may still hold the lock back to
			// the pool: closing it releases the lock.
			conn.Conn().Close(uctx)
		}
	}()
	return fn(conn)
}

// InitSchema applies the full schema on a fresh database.
// It checks whether the "systems" table exists as a proxy for
// whether schema.sql has been loaded. If missing, it executes
// the embedded schema SQL. If present, it's a no-op.
func (db *DB) InitSchema(ctx context.Context, schemaSQL []byte) error {
	_, err := db.ApplySchemaIfEmpty(ctx, schemaSQL)
	return err
}

// ApplySchemaIfEmpty is InitSchema, and also reports whether it applied the
// schema, i.e. whether this process created the database's schema (the
// legacy auth import's FreshDatabase). It holds schemaLockKey, so of several
// processes starting on an empty database exactly one applies the schema.
func (db *DB) ApplySchemaIfEmpty(ctx context.Context, schemaSQL []byte) (applied bool, err error) {
	err = db.withSchemaLock(ctx, func(conn *pgxpool.Conn) error {
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT FROM pg_tables WHERE schemaname = 'public' AND tablename = 'systems')`,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			db.log.Debug().Msg("schema already initialized, skipping")
			return nil
		}

		db.log.Info().Msg("fresh database detected — applying schema")
		if _, err := conn.Exec(ctx, string(schemaSQL)); err != nil {
			return err
		}
		db.log.Info().Msg("schema applied successfully")
		applied = true
		return nil
	})
	return applied, err
}
