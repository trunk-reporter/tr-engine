package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// QueryResult holds the result of a read-only SQL query.
type QueryResult struct {
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	RowCount int      `json:"row_count"`
}

// QuerySensitiveTables are the tables POST /query must never read: API key
// hashes, and auth_settings (the ticket secret, retired token hashes).
// GrantQueryRole never grants them and checkQueryRole refuses a role that
// can read them. A new table holding a secret belongs in this list.
var QuerySensitiveTables = []string{"api_keys", "auth_settings"}

// dangerousServerRoles are the predefined roles that read or write server
// files or run programs on the database host (COPY ... TO PROGRAM works in
// a read-only transaction).
var dangerousServerRoles = []string{"pg_read_server_files", "pg_write_server_files", "pg_execute_server_program"}

// QueryDB is the pool POST /query runs on: its own database login
// (QUERY_DATABASE_URL), never the engine's role. SET ROLE inside the
// engine's own connection would not be a boundary: a query can switch back
// with set_config('role', ...). Every new connection is checked
// (checkQueryRole) and closed if the role could read QuerySensitiveTables,
// read or write server files or run programs, so a role fixed after startup
// starts working without a restart and one made unsafe stops.
type QueryDB struct {
	Pool *pgxpool.Pool
	Role string // the login role of QUERY_DATABASE_URL
	log  zerolog.Logger
}

// UnsafeQueryRoleError is returned for a query connection whose role is
// refused. Reasons say what to change.
type UnsafeQueryRoleError struct {
	Role    string
	Reasons []string
}

func (e *UnsafeQueryRoleError) Error() string {
	return fmt.Sprintf("database role %q is not safe for POST /query: %s", e.Role, strings.Join(e.Reasons, "; "))
}

// OpenQueryDB creates the POST /query pool. It does not connect: the first
// query (or Check) does, so an unreachable database doesn't stop startup.
func OpenQueryDB(ctx context.Context, databaseURL string, log zerolog.Logger) (*QueryDB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse QUERY_DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return checkQueryRole(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("QUERY_DATABASE_URL: %w", err)
	}
	return &QueryDB{Pool: pool, Role: cfg.ConnConfig.User, log: log}, nil
}

// checkQueryRole refuses a connection whose role, or any role it could
// switch to (SET ROLE works for every role it is a member of), is a
// superuser, holds a dangerousServerRoles role, or can read a column of
// QuerySensitiveTables (in any schema, PUBLIC grants included).
func checkQueryRole(ctx context.Context, conn *pgx.Conn) error {
	var (
		role        string
		superuser   bool
		serverRoles []string
		readable    []string
	)
	err := conn.QueryRow(ctx, `
		WITH mine AS (
			SELECT oid, rolname, rolsuper FROM pg_roles
			WHERE pg_has_role(session_user, oid, 'MEMBER')
		)
		SELECT session_user::text,
		       EXISTS (SELECT 1 FROM mine WHERE rolsuper),
		       ARRAY(SELECT rolname::text FROM mine WHERE rolname = ANY($1::text[]) ORDER BY rolname),
		       ARRAY(SELECT DISTINCT c.oid::regclass::text FROM pg_class c
		             WHERE c.relname = ANY($2::text[]) AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		               AND EXISTS (SELECT 1 FROM mine WHERE has_any_column_privilege(mine.oid, c.oid, 'SELECT'))
		             ORDER BY 1)`,
		dangerousServerRoles, QuerySensitiveTables,
	).Scan(&role, &superuser, &serverRoles, &readable)
	if err != nil {
		return fmt.Errorf("check query role: %w", err)
	}
	var reasons []string
	if superuser {
		reasons = append(reasons, "it is (or can SET ROLE to) a superuser")
	}
	if len(serverRoles) > 0 && !superuser {
		reasons = append(reasons, "it is a member of "+strings.Join(serverRoles, ", "))
	}
	if len(readable) > 0 {
		reasons = append(reasons, "it (or a role it can SET ROLE to) can read "+strings.Join(readable, ", "))
	}
	if len(reasons) > 0 {
		return &UnsafeQueryRoleError{Role: role, Reasons: reasons}
	}
	return nil
}

// Check connects once and reports whether queries can run: nil, an
// *UnsafeQueryRoleError, or the connection error.
func (q *QueryDB) Check(ctx context.Context) error {
	conn, err := q.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn.Release()
	return nil
}

// Close closes the pool.
func (q *QueryDB) Close() {
	q.log.Info().Msg("closing query database pool")
	q.Pool.Close()
}

// Execute runs a SQL query inside a read-only transaction with a statement
// timeout. It returns column names and up to maxRows of results.
func (q *QueryDB) Execute(ctx context.Context, sql string, params []any, maxRows int) (*QueryResult, error) {
	if strings.Contains(sql, ";") {
		return nil, fmt.Errorf("multiple statements not allowed")
	}

	tx, err := q.Pool.BeginTx(ctx, pgx.TxOptions{
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30s'"); err != nil {
		return nil, fmt.Errorf("set statement timeout: %w", err)
	}

	rows, err := tx.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// Extract column names from field descriptions.
	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, f := range fields {
		columns[i] = f.Name
	}

	var resultRows [][]any
	for rows.Next() {
		if len(resultRows) >= maxRows {
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		resultRows = append(resultRows, values)
	}
	// Close rows before checking errors or committing — breaking out of
	// the Next() loop early leaves the connection in query mode, which
	// causes "conn busy" on commit.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	if resultRows == nil {
		resultRows = [][]any{}
	}

	return &QueryResult{
		Columns:  columns,
		Rows:     resultRows,
		RowCount: len(resultRows),
	}, nil
}

// ErrQueryRoleIsEngineRole is returned by GrantQueryRole when
// QUERY_DATABASE_URL logs in as the engine's own role.
var ErrQueryRoleIsEngineRole = errors.New("QUERY_DATABASE_URL uses the engine's own database role")

// GrantQueryRole grants role SELECT on every table and view of the engine's
// schema except QuerySensitiveTables, so an operator only has to create the
// login. It runs at every start, which covers tables added by later
// migrations. Partitions are left out: query the parent tables. Relations
// the engine's role can't grant (it doesn't own them) are returned in
// skipped. It never revokes anything; checkQueryRole refuses a role that
// can read too much.
func (db *DB) GrantQueryRole(ctx context.Context, role string) (granted int, skipped []string, err error) {
	var engineRole string
	var exists bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT session_user::text, EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role,
	).Scan(&engineRole, &exists); err != nil {
		return 0, nil, err
	}
	if role == engineRole {
		return 0, nil, ErrQueryRoleIsEngineRole
	}
	if !exists {
		return 0, nil, fmt.Errorf("role %q does not exist", role)
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT format('%I.%I', n.nspname, c.relname),
		       has_table_privilege(c.oid, 'SELECT WITH GRANT OPTION')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema()
		  AND c.relkind IN ('r', 'p', 'v', 'm')
		  AND NOT c.relispartition
		  AND c.relname <> ALL($1::text[])
		  AND NOT EXISTS (
		      -- a view reading a sensitive table runs with its owner's rights
		      SELECT 1 FROM pg_depend d
		      JOIN pg_rewrite rw ON rw.oid = d.objid AND d.classid = 'pg_rewrite'::regclass
		      JOIN pg_class s ON s.oid = d.refobjid AND d.refclassid = 'pg_class'::regclass
		      WHERE rw.ev_class = c.oid AND s.relname = ANY($1::text[]))
		ORDER BY 1`, QuerySensitiveTables)
	if err != nil {
		return 0, nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		var canGrant bool
		if err := rows.Scan(&name, &canGrant); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if canGrant {
			tables = append(tables, name)
		} else {
			skipped = append(skipped, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(tables) == 0 {
		return 0, skipped, nil
	}
	sql := "GRANT SELECT ON TABLE " + strings.Join(tables, ", ") + " TO " + pgx.Identifier{role}.Sanitize()
	if _, err := db.Pool.Exec(ctx, sql); err != nil {
		return 0, skipped, err
	}
	return len(tables), skipped, nil
}
