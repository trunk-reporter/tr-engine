package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
)

func postQuery(t *testing.T, h *QueryHandler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ExecuteQuery(rec, req)
	return rec
}

func TestQueryDisabledWithoutQueryDatabaseURL(t *testing.T) {
	rec := postQuery(t, NewQueryHandler(nil))
	if rec.Code != http.StatusServiceUnavailable || errorCode(rec) != string(ErrQueryDisabled) {
		t.Fatalf("got %d %s, want 503 query_disabled", rec.Code, rec.Body.String())
	}
}

func TestQueryUnreachableDatabaseIs503(t *testing.T) {
	// Nothing listens on port 1: the connection is refused.
	q, err := database.OpenQueryDB(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=2", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Pool.Close()
	rec := postQuery(t, NewQueryHandler(q))
	if rec.Code != http.StatusServiceUnavailable || errorCode(rec) != string(ErrServiceUnavail) {
		t.Fatalf("got %d %s, want 503 service_unavailable", rec.Code, rec.Body.String())
	}
}
