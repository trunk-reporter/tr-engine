package database

import "context"

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
// legacy auth import's FreshDatabase).
func (db *DB) ApplySchemaIfEmpty(ctx context.Context, schemaSQL []byte) (applied bool, err error) {
	var exists bool
	err = db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT FROM pg_tables WHERE schemaname = 'public' AND tablename = 'systems')`,
	).Scan(&exists)
	if err != nil {
		return false, err
	}

	if exists {
		db.log.Debug().Msg("schema already initialized, skipping")
		return false, nil
	}

	db.log.Info().Msg("fresh database detected — applying schema")
	if _, err := db.Pool.Exec(ctx, string(schemaSQL)); err != nil {
		return false, err
	}
	db.log.Info().Msg("schema applied successfully")
	return true, nil
}
