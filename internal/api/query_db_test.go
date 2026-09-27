package api

// POST /query against a real PostgreSQL (TEST_DATABASE_URL): it runs only on
// its own login (QUERY_DATABASE_URL), which can read the data tables but
// not the auth tables, server files or programs.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// queryLogin creates a login role (roles are cluster-wide, so its name is
// unique) and returns its name and a URL for it on db's database. The role
// is dropped at cleanup, before db's database.
func queryLogin(t *testing.T, db *database.DB) (string, string) {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("tr_query_test_%d", time.Now().UnixNano())
	if _, err := db.Pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		// DROP OWNED also removes the role's privileges in this database.
		if _, err := db.Pool.Exec(context.Background(), "DROP OWNED BY "+role); err != nil {
			t.Errorf("drop owned by %s: %v", role, err)
		}
		if _, err := db.Pool.Exec(context.Background(), "DROP ROLE "+role); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	var dbName string
	if err := db.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	u.Path = "/" + dbName
	return role, u.String()
}

// openQueryDB opens a fresh query pool, so its first connection is checked
// against the role's current privileges.
func openQueryDB(t *testing.T, dsn string) *database.QueryDB {
	t.Helper()
	q, err := database.OpenQueryDB(context.Background(), dsn, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Pool.Close)
	return q
}

func queryRouter(t *testing.T, db *database.DB, q *database.QueryDB) http.Handler {
	t.Helper()
	opts := allFeaturesOptions(newAuthenticator(db, nil, 1e9, 1<<30, zerolog.Nop()))
	opts.DB = db
	opts.QueryDB = q
	return buildRouter(opts)
}

func TestIntegrationQueryRole(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	admin, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO systems (system_type, name) VALUES ('p25', 'query test')`); err != nil {
		t.Fatal(err)
	}
	query := func(h http.Handler, sql string) (*database.QueryResult, int, string, string) {
		t.Helper()
		var out database.QueryResult
		rec := call(t, h, "POST", "/api/v1/query", admin.Plaintext, map[string]any{"sql": sql}, &out)
		return &out, rec.Code, errorCode(rec), rec.Body.String()
	}

	t.Run("without QUERY_DATABASE_URL the endpoint is disabled", func(t *testing.T) {
		_, code, errCode, body := query(queryRouter(t, db, nil), "SELECT 1")
		if code != http.StatusServiceUnavailable || errCode != string(ErrQueryDisabled) {
			t.Fatalf("got %d %s, want 503 query_disabled", code, body)
		}
	})

	role, dsn := queryLogin(t, db)
	granted, skipped, err := db.GrantQueryRole(ctx, role)
	if err != nil {
		t.Fatalf("GrantQueryRole: %v", err)
	}
	if granted < 10 || len(skipped) != 0 {
		t.Fatalf("GrantQueryRole granted %d, skipped %v", granted, skipped)
	}

	t.Run("a read-only login reads the data tables and nothing else", func(t *testing.T) {
		q := openQueryDB(t, dsn)
		if err := q.Check(ctx); err != nil {
			t.Fatalf("Check: %v", err)
		}
		h := queryRouter(t, db, q)

		res, code, _, body := query(h, "SELECT name, current_user AS who FROM systems")
		if code != http.StatusOK || res.RowCount != 1 || res.Rows[0][0] != "query test" || res.Rows[0][1] != role {
			t.Fatalf("data table: got %d %s", code, body)
		}

		for _, sql := range []string{
			"SELECT key_hash FROM api_keys",
			"SELECT count(*) FROM auth_settings",
			"SELECT pg_read_file('/etc/hostname')",
			"SELECT * FROM pg_ls_dir('.')",
			"COPY (SELECT 1) TO PROGRAM 'true'",
			// SET ROLE inside the engine's own connection could be undone
			// like this; a separate login has nothing to switch back to.
			"SELECT set_config('role', 'none', true), query_to_xml('SELECT key_hash FROM api_keys', true, false, '')",
			"SELECT set_config('role', 'postgres', true)",
			"INSERT INTO systems (system_type, name) VALUES ('p25', 'nope')",
		} {
			_, code, errCode, body := query(h, sql)
			if code != http.StatusBadRequest || errCode != string(ErrQueryFailed) ||
				!strings.Contains(body, "permission denied") && !strings.Contains(body, "read-only transaction") {
				t.Errorf("%s: got %d %s, want 400 query_failed (permission denied or read-only)", sql, code, body)
			}
		}
	})

	t.Run("a role that can read an auth table is refused", func(t *testing.T) {
		if _, err := db.Pool.Exec(ctx, "GRANT SELECT (key_prefix) ON api_keys TO "+role); err != nil {
			t.Fatal(err)
		}
		q := openQueryDB(t, dsn)
		var unsafe *database.UnsafeQueryRoleError
		if err := q.Check(ctx); !errors.As(err, &unsafe) || !strings.Contains(err.Error(), "api_keys") {
			t.Fatalf("Check: got %v, want UnsafeQueryRoleError naming api_keys", err)
		}
		_, code, errCode, body := query(queryRouter(t, db, q), "SELECT 1")
		if code != http.StatusServiceUnavailable || errCode != string(ErrQueryDisabled) || !strings.Contains(body, "api_keys") {
			t.Fatalf("got %d %s, want 503 query_disabled naming api_keys", code, body)
		}

		// Refused connections aren't pooled: fixing the role is enough.
		if _, err := db.Pool.Exec(ctx, "REVOKE SELECT (key_prefix) ON api_keys FROM "+role); err != nil {
			t.Fatal(err)
		}
		if err := q.Check(ctx); err != nil {
			t.Fatalf("Check on the same pool after the revoke: %v", err)
		}
	})

	t.Run("a role that can read server files is refused", func(t *testing.T) {
		if _, err := db.Pool.Exec(ctx, "GRANT pg_read_server_files TO "+role); err != nil {
			t.Fatal(err)
		}
		defer db.Pool.Exec(ctx, "REVOKE pg_read_server_files FROM "+role)
		var unsafe *database.UnsafeQueryRoleError
		if err := openQueryDB(t, dsn).Check(ctx); !errors.As(err, &unsafe) || !strings.Contains(err.Error(), "pg_read_server_files") {
			t.Fatalf("Check: got %v, want UnsafeQueryRoleError naming pg_read_server_files", err)
		}
	})

	t.Run("a role that can SET ROLE to the engine's role is refused", func(t *testing.T) {
		helper := role + "_owner"
		if _, err := db.Pool.Exec(ctx, "CREATE ROLE "+helper+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			db.Pool.Exec(ctx, "DROP OWNED BY "+helper)
			db.Pool.Exec(ctx, "DROP ROLE "+helper)
		}()
		if _, err := db.Pool.Exec(ctx, "GRANT SELECT ON auth_settings TO "+helper); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, "GRANT "+helper+" TO "+role+" WITH INHERIT FALSE"); err != nil {
			t.Fatal(err)
		}
		defer db.Pool.Exec(ctx, "REVOKE "+helper+" FROM "+role)
		var unsafe *database.UnsafeQueryRoleError
		if err := openQueryDB(t, dsn).Check(ctx); !errors.As(err, &unsafe) || !strings.Contains(err.Error(), "auth_settings") {
			t.Fatalf("Check: got %v, want UnsafeQueryRoleError naming auth_settings", err)
		}
	})

	t.Run("the engine's own role is refused", func(t *testing.T) {
		var engineRole string
		if err := db.Pool.QueryRow(ctx, "SELECT session_user").Scan(&engineRole); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.GrantQueryRole(ctx, engineRole); !errors.Is(err, database.ErrQueryRoleIsEngineRole) {
			t.Fatalf("GrantQueryRole(engine role): got %v", err)
		}
		// The engine's own URL (here a superuser) on the same database.
		var dbName string
		if err := db.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + dbName
		var unsafe *database.UnsafeQueryRoleError
		if err := openQueryDB(t, u.String()).Check(ctx); !errors.As(err, &unsafe) {
			t.Fatalf("Check with the engine's URL: got %v, want UnsafeQueryRoleError", err)
		}
	})

	t.Run("a view over an auth table is not granted", func(t *testing.T) {
		if _, err := db.Pool.Exec(ctx, "CREATE VIEW key_view AS SELECT key_hash FROM api_keys"); err != nil {
			t.Fatal(err)
		}
		defer db.Pool.Exec(ctx, "DROP VIEW key_view")
		if _, _, err := db.GrantQueryRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		var ok bool
		if err := db.Pool.QueryRow(ctx, "SELECT has_table_privilege($1, 'key_view', 'SELECT')", role).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("GrantQueryRole granted a view over api_keys")
		}
	})

	t.Run("every secret-looking column is in a sensitive table", func(t *testing.T) {
		// A new table holding a secret must be added to
		// database.QuerySensitiveTables, or POST /query can read it.
		rows, err := db.Pool.Query(ctx, `
			SELECT c.relname || '.' || a.attname
			FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
			WHERE c.relnamespace = current_schema()::regnamespace
			  AND c.relkind IN ('r', 'p', 'v', 'm') AND NOT c.relispartition
			  AND a.attnum > 0 AND NOT a.attisdropped
			  AND a.attname ~* '(hash|secret|passw|token|credential|api_?key)'
			  AND c.relname <> ALL($1::text[])`, database.QuerySensitiveTables)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				t.Fatal(err)
			}
			t.Errorf("%s looks like a secret but its table is not in database.QuerySensitiveTables", col)
		}
	})

	t.Run("an unknown role is an error", func(t *testing.T) {
		if _, _, err := db.GrantQueryRole(ctx, role+"_missing"); err == nil {
			t.Fatal("GrantQueryRole(unknown role): no error")
		}
	})
}
