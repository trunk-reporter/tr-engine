package database

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migration defines a single idempotent schema migration.
type migration struct {
	name  string
	sql   string
	check string // query that returns true if the migration is already applied
	// irreversible: applying it removes what an older engine running on the
	// same database needs (its user accounts, its API key columns), so the
	// CLI applies it only when asked (PendingMigrations).
	irreversible bool
}

// migrations is the ordered list of schema migrations to apply.
// Each must be idempotent (use IF NOT EXISTS, IF EXISTS, etc.). Migrate
// evaluates every check before applying anything, so a migration must not
// rely on the effects of another one pending in the same run, and its SQL
// must also be correct when an earlier pending migration has just run.
var migrations = []migration{
	{
		name:  "add calls.incidentdata",
		sql:   `ALTER TABLE calls ADD COLUMN IF NOT EXISTS incidentdata jsonb`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'calls' AND column_name = 'incidentdata')`,
	},
	{
		name:  "add unit_events.incidentdata",
		sql:   `ALTER TABLE unit_events ADD COLUMN IF NOT EXISTS incidentdata jsonb`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'unit_events' AND column_name = 'incidentdata')`,
	},
	{
		name:  "add talkgroups.alpha_tag_source",
		sql:   `ALTER TABLE talkgroups ADD COLUMN IF NOT EXISTS alpha_tag_source text`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'talkgroups' AND column_name = 'alpha_tag_source')`,
	},
	{
		name: "add talkgroup stats cache columns",
		sql: `ALTER TABLE talkgroups
			ADD COLUMN IF NOT EXISTS call_count_30d int NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS calls_1h int NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS calls_24h int NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS unit_count_30d int NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS stats_updated_at timestamptz`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'talkgroups' AND column_name = 'call_count_30d')`,
	},
	{
		name: "replace unique sysid/wacn index with non-unique",
		sql: `DROP INDEX IF EXISTS uq_systems_sysid_wacn;
CREATE INDEX IF NOT EXISTS idx_systems_sysid_wacn ON systems (sysid, wacn)
    WHERE system_type IN ('p25', 'smartnet') AND deleted_at IS NULL AND sysid <> '0'`,
		check: `SELECT NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_systems_sysid_wacn')`,
	},
	{
		name:  "add decode_rates time-only index",
		sql:   `CREATE INDEX IF NOT EXISTS idx_decode_rates_time ON decode_rates ("time" DESC)`,
		check: `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_decode_rates_time')`,
	},
	{
		name:  "add recorder_snapshots time-only index",
		sql:   `CREATE INDEX IF NOT EXISTS idx_recorder_snapshots_time ON recorder_snapshots ("time" DESC)`,
		check: `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_recorder_snapshots_time')`,
	},
	{
		name: "expand system_type CHECK for conventional variants",
		sql: `ALTER TABLE systems DROP CONSTRAINT IF EXISTS systems_system_type_check;
ALTER TABLE systems ADD CONSTRAINT systems_system_type_check
    CHECK (system_type IN ('p25', 'smartnet', 'conventional', 'conventionalP25', 'conventionalDMR', 'conventionalSIGMF'))`,
		check: `SELECT EXISTS (
    SELECT 1 FROM information_schema.check_constraints
    WHERE constraint_name = 'systems_system_type_check'
      AND check_clause LIKE '%conventionalP25%'
)`,
	},
	{
		name:  "add provider_ms to transcriptions",
		sql:   `ALTER TABLE transcriptions ADD COLUMN IF NOT EXISTS provider_ms int`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'transcriptions' AND column_name = 'provider_ms')`,
	},
	{
		name: "add empty to transcription_status check constraints",
		sql: `
			ALTER TABLE calls DROP CONSTRAINT IF EXISTS calls_transcription_status_check;
			ALTER TABLE calls ADD CONSTRAINT calls_transcription_status_check
				CHECK (transcription_status IN ('none', 'auto', 'reviewed', 'verified', 'excluded', 'empty'));
			ALTER TABLE call_groups DROP CONSTRAINT IF EXISTS call_groups_transcription_status_check;
			ALTER TABLE call_groups ADD CONSTRAINT call_groups_transcription_status_check
				CHECK (transcription_status IN ('none', 'auto', 'reviewed', 'verified', 'excluded', 'empty'))`,
		check: `SELECT EXISTS (
			SELECT 1 FROM information_schema.check_constraints
			WHERE constraint_name = 'calls_transcription_status_check'
			  AND check_clause LIKE '%empty%')`,
	},
	{
		name: "create config_overrides table",
		sql: `CREATE TABLE IF NOT EXISTS config_overrides (
			key        text PRIMARY KEY,
			value      text NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
		check: `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE tablename = 'config_overrides')`,
	},
	{
		// The current shape; databases that had the older, user-owned shape are
		// converted by "convert api_keys to app keys" below.
		name: "create api_keys table",
		sql: `CREATE TABLE IF NOT EXISTS api_keys (
			id              serial       PRIMARY KEY,
			key_hash        text         UNIQUE NOT NULL,
			key_prefix      text         NOT NULL,
			name            text         NOT NULL,
			scopes          text[]       NOT NULL,
			restriction     jsonb,
			expires_at      timestamptz,
			revoked_at      timestamptz,
			rate_limit_rps  real,
			legacy          boolean      NOT NULL DEFAULT false,
			created_at      timestamptz  NOT NULL DEFAULT now(),
			last_used_at    timestamptz,
			CONSTRAINT api_keys_scopes_check CHECK (cardinality(scopes) > 0)
		);
		CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash)`,
		check: `SELECT to_regclass('api_keys') IS NOT NULL`,
	},
	{
		name: "create data_fixups table",
		sql: `CREATE TABLE IF NOT EXISTS data_fixups (
			name       text        PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now(),
			detail     jsonb
		);
		ALTER TABLE data_fixups ADD COLUMN IF NOT EXISTS detail jsonb`,
		check: `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'data_fixups' AND column_name = 'detail')`,
	},
	{
		// Recorder-reported and over-the-air unit tags, stored beside alpha_tag
		// so a manual/csv rename no longer discards what the radio broadcasts.
		// Not indexed: keeps per-event UpsertUnit updates HOT-eligible.
		name: "add units recorder/OTA alpha tag columns",
		sql: `ALTER TABLE units
			ADD COLUMN IF NOT EXISTS recorder_alpha_tag text,
			ADD COLUMN IF NOT EXISTS recorder_alpha_tag_seen timestamptz,
			ADD COLUMN IF NOT EXISTS ota_alpha_tag text,
			ADD COLUMN IF NOT EXISTS ota_alpha_tag_first_seen timestamptz,
			ADD COLUMN IF NOT EXISTS ota_alpha_tag_last_seen timestamptz`,
		check: `SELECT (SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'units' AND column_name IN (
				'recorder_alpha_tag', 'recorder_alpha_tag_seen',
				'ota_alpha_tag', 'ota_alpha_tag_first_seen', 'ota_alpha_tag_last_seen')) = 5`,
	},
	{
		name: "create unit_tag_suggestions and scan_cursors tables",
		sql: `CREATE TABLE IF NOT EXISTS unit_tag_suggestions (
			id                   bigserial    PRIMARY KEY,
			system_id            int          NOT NULL REFERENCES systems (system_id),
			unit_id              int          NOT NULL,
			tag_key              text         NOT NULL,
			proposed_tag         text         NOT NULL,
			status               text         NOT NULL DEFAULT 'pending'
			                     CHECK (status IN ('pending', 'approved', 'dismissed')),
			occurrences          int          NOT NULL DEFAULT 0,
			call_count           int          NOT NULL DEFAULT 0,
			matches_current_tag  boolean      NOT NULL DEFAULT false,
			tag_at_sighting      text,
			first_seen           timestamptz  NOT NULL,
			last_seen            timestamptz  NOT NULL,
			evidence             jsonb        NOT NULL DEFAULT '[]'::jsonb,
			applied_tag          text,
			previous_tag         text,
			previous_tag_source  text,
			decided_at           timestamptz,
			decided_by           text,
			created_at           timestamptz  NOT NULL DEFAULT now(),
			updated_at           timestamptz  NOT NULL DEFAULT now(),
			UNIQUE (system_id, unit_id, tag_key)
		);
		ALTER TABLE unit_tag_suggestions ADD COLUMN IF NOT EXISTS tag_at_sighting text;
		DROP TRIGGER IF EXISTS trg_unit_tag_suggestions_updated_at ON unit_tag_suggestions;
		CREATE TRIGGER trg_unit_tag_suggestions_updated_at
			BEFORE UPDATE ON unit_tag_suggestions FOR EACH ROW EXECUTE FUNCTION set_updated_at();
		CREATE TABLE IF NOT EXISTS scan_cursors (
			name        text         PRIMARY KEY,
			last_id     bigint       NOT NULL DEFAULT 0,
			updated_at  timestamptz  NOT NULL DEFAULT now()
		)`,
		check: `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE tablename = 'unit_tag_suggestions')
			AND EXISTS (SELECT 1 FROM pg_tables WHERE tablename = 'scan_cursors')
			AND EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name = 'unit_tag_suggestions' AND column_name = 'tag_at_sighting')`,
	},
	{
		// API keys stop belonging to users and become client credentials with
		// scopes (docs/auth.md). One DO block, and every statement that touches
		// the old columns or users goes through EXECUTE, so it parses on any
		// database and works as the manual SQL MigrationError prints. It does
		// nothing unless api_keys still has the old label column (a database
		// that gets api_keys from "create api_keys table" already has the new
		// shape). users is resolved like the old code resolved it (unqualified),
		// and read only if it is tr-engine's (trEngineUsersSQL); otherwise
		// every key is treated as unowned.
		//
		// A user-owned key keeps the lower of its own role and its owner's
		// current role (viewer < editor < admin) and gets " (<username>)" or
		// " (<username>, disabled)" appended to its name; keys of disabled users
		// are revoked. Roles map to scopes viewer → listen, editor → edit,
		// admin → admin, and service-account keys (the documented upload
		// credential) also get upload. A role outside the three maps to listen
		// and the key is revoked.
		name:         "convert api_keys to app keys",
		irreversible: true,
		sql: `DO $mig$
DECLARE
    key_rank  CONSTANT text := $r$CASE k.role WHEN 'admin' THEN 3 WHEN 'editor' THEN 2 WHEN 'viewer' THEN 1 ELSE 0 END$r$;
    rank_expr text := key_rank;
    rank_src  text := 'api_keys k';
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                   WHERE attrelid = to_regclass('api_keys') AND attname = 'label' AND NOT attisdropped) THEN
        RETURN;
    END IF;

    EXECUTE 'ALTER TABLE api_keys
        ADD COLUMN IF NOT EXISTS scopes text[],
        ADD COLUMN IF NOT EXISTS restriction jsonb,
        ADD COLUMN IF NOT EXISTS expires_at timestamptz,
        ADD COLUMN IF NOT EXISTS revoked_at timestamptz,
        ADD COLUMN IF NOT EXISTS rate_limit_rps real,
        ADD COLUMN IF NOT EXISTS legacy boolean NOT NULL DEFAULT false';

    IF ` + trEngineUsersSQL + ` THEN
        EXECUTE 'UPDATE api_keys k SET revoked_at = COALESCE(k.revoked_at, now())
                 FROM users u WHERE u.id = k.user_id AND u.enabled IS NOT TRUE';
        EXECUTE $q$
            UPDATE api_keys k SET label = COALESCE(NULLIF(btrim(k.label), ''), 'key ' || k.key_prefix)
                || ' (' || u.username || CASE WHEN u.enabled IS TRUE THEN ')' ELSE ', disabled)' END
            FROM users u WHERE u.id = k.user_id$q$;
        rank_expr := format($r$LEAST(%s, CASE WHEN u.id IS NULL THEN 3 WHEN u.role = 'admin' THEN 3
            WHEN u.role = 'editor' THEN 2 WHEN u.role = 'viewer' THEN 1 ELSE 0 END)$r$, key_rank);
        rank_src := 'api_keys k LEFT JOIN users u ON u.id = k.user_id';
    END IF;
    EXECUTE format($q$
        UPDATE api_keys k SET
            scopes = CASE r.rank WHEN 3 THEN ARRAY['admin'] WHEN 2 THEN ARRAY['edit'] ELSE ARRAY['listen'] END
                     || CASE WHEN k.is_service_account THEN ARRAY['upload'] ELSE '{}'::text[] END,
            revoked_at = CASE WHEN r.rank = 0 THEN COALESCE(k.revoked_at, now()) ELSE k.revoked_at END
        FROM (SELECT k.id, %s AS rank FROM %s) r
        WHERE r.id = k.id$q$, rank_expr, rank_src);

    EXECUTE 'ALTER TABLE api_keys RENAME COLUMN label TO name';
    EXECUTE $q$UPDATE api_keys SET name = 'key ' || key_prefix WHERE btrim(name) = ''$q$;
    EXECUTE 'ALTER TABLE api_keys ALTER COLUMN name DROP DEFAULT';
    EXECUTE 'ALTER TABLE api_keys ALTER COLUMN scopes SET NOT NULL';
    EXECUTE 'ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (cardinality(scopes) > 0)';
    EXECUTE 'DROP INDEX IF EXISTS idx_api_keys_user_id';
    EXECUTE 'ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id, DROP COLUMN IF EXISTS role,
        DROP COLUMN IF EXISTS is_service_account';
    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys (key_hash)';
END
$mig$`,
		check: `SELECT EXISTS (SELECT 1 FROM pg_attribute
				WHERE attrelid = to_regclass('api_keys') AND attname = 'scopes' AND NOT attisdropped)
			AND NOT EXISTS (SELECT 1 FROM pg_attribute
				WHERE attrelid = to_regclass('api_keys') AND attname = 'label' AND NOT attisdropped)`,
	},
	{
		// tr-engine has no user accounts any more. Record who had one
		// (data_fixups 'removed-user-accounts', logged once at startup) and drop
		// the table. Runs after "convert api_keys to app keys", which reads it.
		// A users table that isn't tr-engine's (trEngineUsersSQL) is left alone.
		name:         "record and drop users",
		irreversible: true,
		sql: `DO $mig$
DECLARE
    login_col text := 'NULL::timestamptz';
BEGIN
    IF NOT ` + trEngineUsersSQL + ` THEN
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_attribute
               WHERE attrelid = to_regclass('users') AND attname = 'last_login' AND NOT attisdropped) THEN
        login_col := 'last_login';
    END IF;
    EXECUTE format($q$
        INSERT INTO data_fixups (name, detail)
        SELECT 'removed-user-accounts', COALESCE(jsonb_agg(jsonb_build_object(
                   'username', username, 'role', role, 'enabled', enabled, 'last_login', %s)
                   ORDER BY id), '[]'::jsonb)
        FROM users
        ON CONFLICT (name) DO NOTHING$q$, login_col);
    EXECUTE 'DROP TABLE users CASCADE';
END
$mig$`,
		check: `SELECT NOT ` + trEngineUsersSQL,
	},
	{
		name: "create auth_settings table",
		sql: `CREATE TABLE IF NOT EXISTS auth_settings (
			name        text         PRIMARY KEY,
			value       jsonb        NOT NULL,
			updated_at  timestamptz  NOT NULL DEFAULT now()
		)`,
		check: `SELECT to_regclass('auth_settings') IS NOT NULL`,
	},
	{
		name: "create audit_log table",
		sql: `CREATE TABLE IF NOT EXISTS audit_log (
			id          bigserial    PRIMARY KEY,
			"time"      timestamptz  NOT NULL DEFAULT now(),
			key_id      int          NOT NULL,
			key_name    text         NOT NULL,
			actor       text,
			method      text         NOT NULL,
			path        text         NOT NULL,
			status      int          NOT NULL,
			request_id  text
		);
		CREATE INDEX IF NOT EXISTS idx_audit_log_time ON audit_log ("time" DESC);
		CREATE INDEX IF NOT EXISTS idx_audit_log_key_time ON audit_log (key_id, "time" DESC)`,
		check: `SELECT to_regclass('audit_log') IS NOT NULL
			AND to_regclass('idx_audit_log_time') IS NOT NULL
			AND to_regclass('idx_audit_log_key_time') IS NOT NULL`,
	},
}

// trEngineUsersSQL is a SQL condition: the relation "users" resolves to
// (unqualified, as the old engine resolved it) is the user table old
// tr-engine versions created. It has the columns the auth migrations read,
// and the trigger or role CHECK the old "create users table" migration
// created with it. That migration skipped creating the table when any
// "users" table existed, and then used it, so a database shared with another
// application may hold that application's "users" table: it is never read
// as tr-engine's user list or dropped.
const trEngineUsersSQL = `(to_regclass('users') IS NOT NULL
        AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('users')
             AND attname IN ('username', 'role', 'enabled') AND attnum > 0 AND NOT attisdropped) = 3
        AND (EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass('users') AND tgname = 'trg_users_updated_at')
             OR EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('users') AND contype = 'c'
                        AND pg_get_constraintdef(oid) LIKE '%''viewer''%'
                        AND pg_get_constraintdef(oid) LIKE '%''editor''%')))`

// PendingMigration is a migration Migrate would apply now.
type PendingMigration struct {
	Name string
	// Irreversible: it converts the old API keys or drops the old user
	// accounts, which an older engine on the same database then loses.
	Irreversible bool
}

// PendingMigrations returns the migrations Migrate would apply now, in
// order, as Migrate decides it (a check that fails counts as pending).
func (db *DB) PendingMigrations(ctx context.Context) ([]PendingMigration, error) {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	var pending []PendingMigration
	for _, m := range migrations {
		if m.check != "" {
			var exists bool
			if err := conn.QueryRow(ctx, m.check).Scan(&exists); err == nil && exists {
				continue
			}
		}
		pending = append(pending, PendingMigration{Name: m.name, Irreversible: m.irreversible})
	}
	return pending, nil
}

// Migrate runs all pending schema migrations.
// For each migration, it first checks whether the change is already present.
// If not, it attempts to apply it. If the apply fails (e.g. insufficient
// privileges), the error is returned — the caller should treat this as fatal
// since the application's queries depend on these columns existing.
//
// It holds schemaLockKey throughout, so a second process starting at the same
// time waits, then finds the migrations applied.
func (db *DB) Migrate(ctx context.Context) error {
	return db.withSchemaLock(ctx, func(conn *pgxpool.Conn) error {
		var pending []migration
		for _, m := range migrations {
			if m.check != "" {
				var exists bool
				if err := conn.QueryRow(ctx, m.check).Scan(&exists); err == nil && exists {
					continue
				}
			}
			pending = append(pending, m)
		}

		if len(pending) == 0 {
			return nil
		}

		// Try to apply each pending migration
		applied := 0
		for _, m := range pending {
			if _, err := conn.Exec(ctx, m.sql); err != nil {
				return &MigrationError{
					failed:  m,
					pending: pending[applied:],
					err:     err,
				}
			}
			db.log.Info().Str("migration", m.name).Msg("schema migration applied")
			applied++
		}
		db.log.Info().Int("applied", applied).Msg("schema migrations complete")
		return nil
	})
}

// MigrationError is returned when a migration fails.
// It includes the SQL needed to apply all remaining migrations manually.
type MigrationError struct {
	failed  migration
	pending []migration
	err     error
}

func (e *MigrationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migration %q failed: %v\n\n", e.failed.name, e.err)
	b.WriteString("Run the following SQL as a database superuser to fix this:\n\n")
	for _, m := range e.pending {
		fmt.Fprintf(&b, "  %s;\n", m.sql)
	}
	b.WriteString("\nThen restart tr-engine.")
	return b.String()
}

func (e *MigrationError) Unwrap() error {
	return e.err
}
