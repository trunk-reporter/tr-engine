package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/database"
)

// QueryRunner runs POST /query SQL (*database.QueryDB).
type QueryRunner interface {
	Execute(ctx context.Context, sql string, params []any, maxRows int) (*database.QueryResult, error)
}

// QueryHandler serves POST /query on its own database login
// (QUERY_DATABASE_URL). Without one the route answers 503 query_disabled.
type QueryHandler struct {
	q QueryRunner
}

func NewQueryHandler(q QueryRunner) *QueryHandler {
	return &QueryHandler{q: q}
}

const queryDisabledMessage = "POST /query is disabled: set QUERY_DATABASE_URL to a read-only database role (docs/auth.md)"

type queryRequest struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
	Limit  int    `json:"limit"`
}

func (h *QueryHandler) ExecuteQuery(w http.ResponseWriter, r *http.Request) {
	log := hlog.FromRequest(r)

	if h.q == nil {
		WriteErrorWithCode(w, http.StatusServiceUnavailable, ErrQueryDisabled, queryDisabledMessage)
		return
	}

	var req queryRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}

	sql := strings.TrimSpace(req.SQL)
	if sql == "" {
		WriteError(w, http.StatusBadRequest, "sql field is required")
		return
	}

	if strings.Contains(sql, ";") {
		log.Warn().Str("sql", sql).Msg("query rejected: semicolons forbidden")
		WriteError(w, http.StatusBadRequest, "multiple statements not allowed (semicolons are forbidden)")
		return
	}

	maxRows := req.Limit
	if maxRows <= 0 {
		maxRows = 1000
	}
	if maxRows > 50000 {
		WriteError(w, http.StatusBadRequest, "limit must be <= 50000")
		return
	}

	if req.Params == nil {
		req.Params = []any{}
	}

	log.Info().Str("sql", sql).Int("limit", maxRows).Msg("executing query")

	result, err := h.q.Execute(r.Context(), sql, req.Params, maxRows)
	var unsafe *database.UnsafeQueryRoleError
	var connectErr *pgconn.ConnectError
	switch {
	case errors.As(err, &unsafe):
		log.Error().Err(err).Msg("query refused: QUERY_DATABASE_URL role is not safe")
		WriteErrorWithCodeDetail(w, http.StatusServiceUnavailable, ErrQueryDisabled,
			"POST /query is disabled: the QUERY_DATABASE_URL role can read more than it should (docs/auth.md)", err.Error())
		return
	case errors.As(err, &connectErr):
		log.Warn().Err(err).Msg("query failed: can't connect with QUERY_DATABASE_URL")
		WriteErrorWithCode(w, http.StatusServiceUnavailable, ErrServiceUnavail, "can't connect to the query database; retry later")
		return
	case err != nil:
		log.Warn().Err(err).Str("sql", sql).Msg("query failed")
		WriteErrorWithCodeDetail(w, http.StatusBadRequest, ErrQueryFailed, "query failed", err.Error())
		return
	}

	log.Info().Str("sql", sql).Int("row_count", result.RowCount).Msg("query completed")
	WriteJSON(w, http.StatusOK, result)
}

func (h *QueryHandler) Routes(r chi.Router) {
	r.Post("/query", h.ExecuteQuery)
}
