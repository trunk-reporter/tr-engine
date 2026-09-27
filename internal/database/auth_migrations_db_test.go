package database

// Runs against a real PostgreSQL; skipped unless TEST_DATABASE_URL is set (see
// export_units_db_test.go).

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The user-owned api_keys shape and the users table as the versions before
// API-key auth created them (the removed "create users table", "add
// display_name and last_login to users" and old "create api_keys table"
// migrations).
const (
	oldUsersDDL = `CREATE TABLE users (
		id            serial       PRIMARY KEY,
		username      text         UNIQUE NOT NULL,
		password_hash text         NOT NULL,
		role          text         NOT NULL DEFAULT 'viewer'
		              CHECK (role IN ('viewer', 'editor', 'admin')),
		enabled       boolean      NOT NULL DEFAULT true,
		created_at    timestamptz  NOT NULL DEFAULT now(),
		updated_at    timestamptz  NOT NULL DEFAULT now()
	);
	CREATE TRIGGER trg_users_updated_at
		BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at()`
	oldUsersColumnsDDL = `ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS last_login timestamptz`
	oldAPIKeysDDL = `CREATE TABLE api_keys (
		id                 serial       PRIMARY KEY,
		key_hash           text         UNIQUE NOT NULL,
		key_prefix         text         NOT NULL,
		user_id            int          REFERENCES users(id) ON DELETE CASCADE,
		role               text         NOT NULL DEFAULT 'viewer'
		                   CHECK (role IN ('viewer', 'editor', 'admin')),
		label              text         NOT NULL DEFAULT '',
		is_service_account boolean      NOT NULL DEFAULT false,
		created_at         timestamptz  NOT NULL DEFAULT now(),
		last_used_at       timestamptz
	);
	CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id);
	CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash)`
)

func readSchema(t *testing.T) []byte {
	t.Helper()
	schema, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func mustExec(t *testing.T, db *DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// authDB is a fresh database with schema.sql and all migrations applied.
func authDB(t *testing.T) *DB {
	t.Helper()
	db := emptyDB(t)
	ctx := context.Background()
	fresh, err := db.ApplySchemaIfEmpty(ctx, readSchema(t))
	if err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if !fresh {
		t.Fatal("ApplySchemaIfEmpty on an empty database reported not fresh")
	}
	migrateTwice(t, db)
	return db
}

// upgradedDB is authDB without schema.sql's SchemaCreatedFixup marker: a
// database upgraded from a version before API-key auth, whose old auth
// configuration the legacy import carries over.
func upgradedDB(t *testing.T) *DB {
	t.Helper()
	db := authDB(t)
	mustExec(t, db, `DELETE FROM data_fixups WHERE name = $1`, SchemaCreatedFixup)
	return db
}

// migrateTwice runs Migrate twice (the second run must be a no-op) and then
// checks that every migration's check reports it applied.
func migrateTwice(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate pass %d: %v", i+1, err)
		}
	}
	for _, m := range migrations {
		var applied bool
		if err := db.Pool.QueryRow(ctx, m.check).Scan(&applied); err != nil {
			t.Fatalf("check of %q: %v", m.name, err)
		}
		if !applied {
			t.Errorf("migration %q still pending after Migrate", m.name)
		}
	}
}

type columnShape struct {
	Name, Type string
	Nullable   bool
	HasDefault bool
}

func tableShape(t *testing.T, db *DB, table string) []columnShape {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), `
		SELECT column_name, udt_name, is_nullable = 'YES', column_default IS NOT NULL
		FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1
		ORDER BY column_name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []columnShape
	for rows.Next() {
		var c columnShape
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.HasDefault); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	return cols
}

func indexNames(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1 ORDER BY indexname`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func tableExistsT(t *testing.T, db *DB, table string) bool {
	t.Helper()
	var ok bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func hasScopesCheck(t *testing.T, db *DB) bool {
	t.Helper()
	var ok bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = 'api_keys'::regclass AND conname = 'api_keys_scopes_check' AND contype = 'c')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// assertNewAuthShape checks api_keys, auth_settings and audit_log against a
// database created from schema.sql, and that users is gone.
func assertNewAuthShape(t *testing.T, db, reference *DB) {
	t.Helper()
	for _, table := range []string{"api_keys", "auth_settings", "audit_log"} {
		got, want := tableShape(t, db, table), tableShape(t, reference, table)
		if !slices.Equal(got, want) {
			t.Errorf("%s columns differ from schema.sql:\n got  %+v\n want %+v", table, got, want)
		}
		if got, want := indexNames(t, db, table), indexNames(t, reference, table); !slices.Equal(got, want) {
			t.Errorf("%s indexes = %v, want %v", table, got, want)
		}
	}
	if !hasScopesCheck(t, db) {
		t.Error("api_keys_scopes_check constraint missing")
	}
	if tableExistsT(t, db, "users") {
		t.Error("users table still exists")
	}
}

func TestAuthMigrations_FreshDatabase(t *testing.T) {
	db := authDB(t)
	if tableExistsT(t, db, "users") {
		t.Fatal("a fresh database has a users table")
	}
	cols := tableShape(t, db, "api_keys")
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	want := []string{"created_at", "expires_at", "id", "key_hash", "key_prefix", "last_used_at", "legacy",
		"name", "rate_limit_rps", "restriction", "revoked_at", "scopes"}
	if !slices.Equal(names, want) {
		t.Errorf("api_keys columns = %v, want %v", names, want)
	}
	if !hasScopesCheck(t, db) {
		t.Error("api_keys_scopes_check missing")
	}
	if got := indexNames(t, db, "api_keys"); !slices.Contains(got, "idx_api_keys_key_hash") {
		t.Errorf("api_keys indexes = %v, want idx_api_keys_key_hash", got)
	}
	if got := indexNames(t, db, "audit_log"); !slices.Contains(got, "idx_audit_log_time") || !slices.Contains(got, "idx_audit_log_key_time") {
		t.Errorf("audit_log indexes = %v", got)
	}
	// An empty scopes array is refused by the CHECK.
	if _, err := db.Pool.Exec(context.Background(),
		`INSERT INTO api_keys (key_hash, key_prefix, name, scopes) VALUES ('h', 'p', 'n', '{}')`); err == nil {
		t.Error("inserted a key with no scopes")
	}
	summary, err := db.RemovedUserAccountsSummary(context.Background())
	if err != nil || summary != "" {
		t.Errorf("RemovedUserAccountsSummary on a fresh database = (%q, %v), want empty", summary, err)
	}
}

// A database from before users and api_keys existed (and before data_fixups):
// "create api_keys table" creates the new shape and the conversion, evaluated
// as pending before it ran, finds nothing to convert.
func TestAuthMigrations_PreUsersDatabase(t *testing.T) {
	reference := authDB(t)
	db := emptyDB(t)
	ctx := context.Background()
	if err := db.InitSchema(ctx, readSchema(t)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TABLE api_keys, auth_settings, audit_log, data_fixups`)
	migrateTwice(t, db)
	assertNewAuthShape(t, db, reference)
	summary, err := db.RemovedUserAccountsSummary(ctx)
	if err != nil || summary != "" {
		t.Errorf("RemovedUserAccountsSummary = (%q, %v), want empty", summary, err)
	}
}

// oldKey is a row of the user-owned api_keys shape.
type oldKey struct {
	secret  string
	user    int // 0 = no owner
	role    string
	label   string
	service bool
	// expected after the migration
	name    string // "" = 'key ' || prefix, "*" = derived from prefix + suffix
	scopes  string
	revoked bool
}

func TestAuthMigrations_UsersDatabase(t *testing.T) {
	reference := authDB(t)
	db := emptyDB(t)
	ctx := context.Background()
	if err := db.InitSchema(ctx, readSchema(t)); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Turn it back into a current-version (pre-API-key) database.
	mustExec(t, db, `DROP TABLE api_keys, auth_settings, audit_log`)
	mustExec(t, db, oldUsersDDL)
	mustExec(t, db, oldUsersColumnsDDL)
	mustExec(t, db, oldAPIKeysDDL)
	login := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role, enabled, last_login) VALUES
		(1, 'alice@example.com', 'x', 'admin',  true,  $1),
		(2, 'bob@example.com',   'x', 'editor', true,  NULL),
		(3, 'carol@example.com', 'x', 'viewer', false, NULL),
		(4, 'dave@example.com',  'x', 'admin',  false, NULL)`, login)

	keys := []oldKey{
		{secret: "tre_alice_admin", user: 1, role: "admin", label: "alice laptop",
			name: "alice laptop (alice@example.com)", scopes: "admin"},
		{secret: "tre_bob_admin", user: 2, role: "admin", label: "bob script", // capped at bob's editor role
			name: "bob script (bob@example.com)", scopes: "edit"},
		{secret: "tre_carol_editor", user: 3, role: "editor", label: "carol", // disabled viewer
			name: "carol (carol@example.com, disabled)", scopes: "listen", revoked: true},
		{secret: "tre_alice_uploader", user: 1, role: "editor", label: "uploads", service: true,
			name: "uploads (alice@example.com)", scopes: "edit,upload"},
		{secret: "tre_unowned_service", role: "viewer", label: "", service: true,
			name: "", scopes: "listen,upload"},
		{secret: "tre_unowned_admin", role: "admin", label: "ops",
			name: "ops", scopes: "admin"},
		{secret: "tre_dave_service", user: 4, role: "admin", label: "dave uploads", service: true,
			name: "dave uploads (dave@example.com, disabled)", scopes: "admin,upload", revoked: true},
		{secret: "tre_bob_viewer_nolabel", user: 2, role: "viewer", label: "  ",
			name: "*", scopes: "listen"},
	}
	used := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	for _, k := range keys {
		var user any
		if k.user != 0 {
			user = k.user
		}
		hash := HashAPIKey(k.secret)
		mustExec(t, db, `INSERT INTO api_keys (key_hash, key_prefix, user_id, role, label, is_service_account, last_used_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, hash, "tre_"+hash[:8], user, k.role, k.label, k.service, used)
	}

	migrateTwice(t, db)
	assertNewAuthShape(t, db, reference)

	for _, k := range keys {
		got, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(k.secret))
		if err != nil {
			t.Fatalf("%s: %v", k.secret, err)
		}
		wantName := k.name
		switch k.name {
		case "":
			wantName = "key " + got.Prefix
		case "*":
			wantName = "key " + got.Prefix + " (bob@example.com)"
		}
		if got.Name != wantName {
			t.Errorf("%s: name = %q, want %q", k.secret, got.Name, wantName)
		}
		if s := strings.Join(got.Scopes.Strings(), ","); s != k.scopes {
			t.Errorf("%s: scopes = %s, want %s", k.secret, s, k.scopes)
		}
		if (got.RevokedAt != nil) != k.revoked || (got.Status == KeyRevoked) != k.revoked {
			t.Errorf("%s: revoked_at = %v status %s, want revoked=%v", k.secret, got.RevokedAt, got.Status, k.revoked)
		}
		if got.Legacy || got.Restriction != nil || got.ExpiresAt != nil || got.RateLimitRPS != nil {
			t.Errorf("%s: unexpected new fields set: %+v", k.secret, got)
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(used) {
			t.Errorf("%s: last_used_at = %v, want %v", k.secret, got.LastUsedAt, used)
		}
	}

	// The dropped accounts are recorded, and summarized once.
	var detail []byte
	if err := db.Pool.QueryRow(ctx, `SELECT detail FROM data_fixups WHERE name = $1`, RemovedUserAccountsFixup).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var removed []RemovedUserAccount
	if err := json.Unmarshal(detail, &removed); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 || removed[0].Username != "alice@example.com" || removed[0].LastLogin == nil ||
		!removed[0].LastLogin.Equal(login) || removed[2].Enabled {
		t.Errorf("removed-user-accounts detail = %s", detail)
	}
	summary, err := db.RemovedUserAccountsSummary(ctx)
	want := "removed 4 user accounts: alice@example.com (admin), bob@example.com (editor), " +
		"carol@example.com (viewer, disabled), dave@example.com (admin, disabled) — give each person or their client an API key"
	if err != nil || summary != want {
		t.Errorf("RemovedUserAccountsSummary = (%q, %v)\nwant %q", summary, err, want)
	}
	if again, err := db.RemovedUserAccountsSummary(ctx); err != nil || again != "" {
		t.Errorf("second RemovedUserAccountsSummary = (%q, %v), want empty", again, err)
	}

	// Two admin keys stay active (alice's and the unowned "ops"), and the
	// converted table takes new keys.
	if ok, err := db.ActiveAdminKeyExists(ctx); err != nil || !ok {
		t.Errorf("ActiveAdminKeyExists = (%v, %v), want true", ok, err)
	}
	if _, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "after migration", Scopes: scopesOf("listen")}); err != nil {
		t.Errorf("create key after migration: %v", err)
	}
}

// A database whose users table predates the display_name/last_login columns:
// the record-and-drop migration records last_login as null.
func TestAuthMigrations_UsersWithoutLastLogin(t *testing.T) {
	db := emptyDB(t)
	ctx := context.Background()
	if err := db.InitSchema(ctx, readSchema(t)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TABLE api_keys, auth_settings, audit_log`)
	mustExec(t, db, oldUsersDDL)
	mustExec(t, db, oldAPIKeysDDL)
	mustExec(t, db, `INSERT INTO users (username, password_hash, role, enabled) VALUES ('erin', 'x', 'editor', true)`)
	mustExec(t, db, `INSERT INTO api_keys (key_hash, key_prefix, user_id, role, label) VALUES ($1, 'tre_00000000', 1, 'editor', 'e')`,
		HashAPIKey("erin-key"))
	migrateTwice(t, db)
	if tableExistsT(t, db, "users") {
		t.Fatal("users still exists")
	}
	summary, err := db.RemovedUserAccountsSummary(ctx)
	if err != nil || summary != "removed 1 user account: erin (editor) — give each person or their client an API key" {
		t.Errorf("summary = (%q, %v)", summary, err)
	}
	k, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey("erin-key"))
	if err != nil || k.Name != "e (erin)" || strings.Join(k.Scopes.Strings(), ",") != "edit" {
		t.Errorf("converted key = %+v, %v", k, err)
	}
}

// foreignUsersDDL is another application's users table in a shared schema,
// with a table that references it.
const foreignUsersDDL = `CREATE TABLE users (
		id       serial PRIMARY KEY,
		username text   NOT NULL,
		email    text,
		role     text,
		enabled  boolean
	);
	CREATE TABLE orders (id serial PRIMARY KEY, user_id int REFERENCES users(id));
	INSERT INTO users (username, email, role, enabled) VALUES
		('alice', 'alice@shop.example', 'owner', true), ('bob', 'bob@shop.example', 'member', true);
	INSERT INTO orders (user_id) VALUES (1), (2)`

// assertForeignUsersKept checks that the other application's users table,
// its rows and the foreign key to it survived.
func assertForeignUsersKept(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	var users, fks int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email LIKE '%@shop.example'`).Scan(&users); err != nil || users != 2 {
		t.Errorf("foreign users rows = %d (%v), want 2", users, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'orders'::regclass AND contype = 'f'`).Scan(&fks); err != nil || fks != 1 {
		t.Errorf("orders foreign keys = %d (%v), want 1", fks, err)
	}
	var recorded bool
	if err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM data_fixups WHERE name = 'removed-user-accounts')`).Scan(&recorded); err != nil || recorded {
		t.Errorf("the foreign users were recorded as removed tr-engine accounts (%v)", err)
	}
}

// Another application's users table in the same schema is never read as
// tr-engine's user list or dropped, on a fresh install or an upgrade (r1-14).
func TestAuthMigrations_ForeignUsersTable(t *testing.T) {
	t.Run("fresh install", func(t *testing.T) {
		db := emptyDB(t)
		mustExec(t, db, foreignUsersDDL)
		if _, err := db.ApplySchemaIfEmpty(context.Background(), readSchema(t)); err != nil {
			t.Fatal(err)
		}
		migrateTwice(t, db)
		assertForeignUsersKept(t, db)
	})

	t.Run("fresh install, table without role or enabled", func(t *testing.T) {
		db := emptyDB(t)
		mustExec(t, db, `CREATE TABLE users (id serial PRIMARY KEY, username text, email text)`)
		if _, err := db.ApplySchemaIfEmpty(context.Background(), readSchema(t)); err != nil {
			t.Fatal(err)
		}
		migrateTwice(t, db)
		if !tableExistsT(t, db, "users") {
			t.Error("the foreign users table was dropped")
		}
	})

	t.Run("upgrade of an engine that used the foreign table", func(t *testing.T) {
		db := emptyDB(t)
		ctx := context.Background()
		if err := db.InitSchema(ctx, readSchema(t)); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `DROP TABLE api_keys, auth_settings, audit_log`)
		// The old engine found a users table, skipped creating its own, added
		// its columns and pointed api_keys.user_id at it.
		mustExec(t, db, foreignUsersDDL)
		mustExec(t, db, oldUsersColumnsDDL)
		mustExec(t, db, oldAPIKeysDDL)
		mustExec(t, db, `INSERT INTO api_keys (key_hash, key_prefix, user_id, role, label) VALUES
			($1, 'tre_00000001', 1, 'admin', 'ops'), ($2, 'tre_00000002', NULL, 'viewer', 'reader')`,
			HashAPIKey("owned"), HashAPIKey("unowned"))
		migrateTwice(t, db)
		assertForeignUsersKept(t, db)
		for secret, want := range map[string]string{"owned": "ops/admin", "unowned": "reader/listen"} {
			k, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(secret))
			if err != nil || k.Name+"/"+strings.Join(k.Scopes.Strings(), ",") != want || k.RevokedAt != nil {
				t.Errorf("key %s = %+v, %v; want %s, active", secret, k, err, want)
			}
		}
	})
}

// Two processes migrating an old database at once: one waits for the other,
// and neither fails (r1-15).
func TestAuthMigrations_ConcurrentMigrate(t *testing.T) {
	db := emptyDB(t)
	ctx := context.Background()
	if err := db.InitSchema(ctx, readSchema(t)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TABLE api_keys, auth_settings, audit_log`)
	mustExec(t, db, oldUsersDDL)
	mustExec(t, db, oldUsersColumnsDDL)
	mustExec(t, db, oldAPIKeysDDL)
	mustExec(t, db, `INSERT INTO users (username, password_hash, role, enabled) VALUES ('erin', 'x', 'editor', true)`)
	mustExec(t, db, `INSERT INTO api_keys (key_hash, key_prefix, user_id, role, label) VALUES ($1, 'tre_00000000', 1, 'editor', 'e')`,
		HashAPIKey("erin-key"))

	var dbs []*DB
	for i := 0; i < 3; i++ {
		other, err := Connect(ctx, db.Pool.Config().ConnString(), zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		dbs = append(dbs, other)
	}
	errs := make([]error, len(dbs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, d := range dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := d.ApplySchemaIfEmpty(ctx, readSchema(t))
			if err == nil {
				err = d.Migrate(ctx)
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("process %d: %v", i, err)
		}
	}
	migrateTwice(t, db)
	k, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey("erin-key"))
	if err != nil || k.Name != "e (erin)" || strings.Join(k.Scopes.Strings(), ",") != "edit" {
		t.Errorf("converted key = %+v, %v", k, err)
	}

	// Several processes on an empty database: exactly one applies the schema.
	empty := emptyDB(t)
	var applied atomic.Int32
	var wg2 sync.WaitGroup
	for i := 0; i < 3; i++ {
		other, err := Connect(ctx, empty.Pool.Config().ConnString(), zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			fresh, err := other.ApplySchemaIfEmpty(ctx, readSchema(t))
			if err != nil {
				t.Errorf("apply schema: %v", err)
			}
			if fresh {
				applied.Add(1)
			}
		}()
	}
	wg2.Wait()
	if applied.Load() != 1 {
		t.Errorf("%d processes applied the schema, want 1", applied.Load())
	}
}
