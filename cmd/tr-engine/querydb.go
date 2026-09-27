package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// setupQueryDB opens POST /query's own pool (QUERY_DATABASE_URL), grants
// its role SELECT on the data tables and checks it. It returns nil, which
// disables POST /query (503 query_disabled), when QUERY_DATABASE_URL is
// unset or invalid. A role that is unsafe or unreachable at startup keeps
// its pool: each new connection is checked again, so fixing the role takes
// effect without a restart.
func setupQueryDB(ctx context.Context, db *database.DB, cfg *config.Config, log zerolog.Logger) *database.QueryDB {
	if cfg.QueryDatabaseURL == "" {
		log.Info().Msg("POST /api/v1/query is disabled: set QUERY_DATABASE_URL to a read-only database role to enable it (docs/auth.md)")
		return nil
	}
	qlog := log.With().Str("component", "query-db").Logger()
	q, err := database.OpenQueryDB(ctx, cfg.QueryDatabaseURL, qlog)
	if err != nil {
		log.Error().Err(err).Msg("POST /api/v1/query is disabled: invalid QUERY_DATABASE_URL")
		return nil
	}

	granted, skipped, err := db.GrantQueryRole(ctx, q.Role)
	switch {
	case errors.Is(err, database.ErrQueryRoleIsEngineRole):
		// The check below refuses it: the engine's role reads api_keys.
	case err != nil:
		qlog.Warn().Err(err).Str("role", q.Role).
			Msg("could not grant the QUERY_DATABASE_URL role SELECT on the data tables; grant it yourself (docs/auth.md)")
	default:
		ev := qlog.Info().Str("role", q.Role).Int("tables", granted)
		if len(skipped) > 0 {
			ev = qlog.Warn().Str("role", q.Role).Int("tables", granted).Str("not_owned", strings.Join(skipped, ", "))
		}
		ev.Msg("granted the QUERY_DATABASE_URL role SELECT on the data tables")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var unsafe *database.UnsafeQueryRoleError
	switch err := q.Check(checkCtx); {
	case err == nil:
		qlog.Info().Str("role", q.Role).Msg("POST /api/v1/query enabled")
	case errors.As(err, &unsafe):
		qlog.Error().Err(err).Str("role", q.Role).
			Msg("POST /api/v1/query refuses every query until the QUERY_DATABASE_URL role is fixed (docs/auth.md)")
	default:
		qlog.Warn().Err(err).Str("role", q.Role).
			Msg("can't connect with QUERY_DATABASE_URL yet; POST /api/v1/query will retry on each request")
	}
	return q
}
