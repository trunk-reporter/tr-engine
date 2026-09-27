package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// mockSuggestionStore implements unitTagSuggestionStore with a tiny in-memory
// model of one suggestion and its unit.
type mockSuggestionStore struct {
	filter     database.UnitTagSuggestionFilter
	list       []database.UnitTagSuggestionAPI
	total      int
	listErr    error
	suggestion database.UnitTagSuggestionAPI // the row approve/dismiss act on
	unitTag    string
	unitSource string
	unitExists bool

	approveCalls  int
	approveTag    string
	decidedBy     string
	dismissCalled bool
}

func (m *mockSuggestionStore) ListUnitTagSuggestions(_ context.Context, f database.UnitTagSuggestionFilter) ([]database.UnitTagSuggestionAPI, int, error) {
	m.filter = f
	return m.list, m.total, m.listErr
}

func (m *mockSuggestionStore) GetUnitTagSuggestion(_ context.Context, id int64) (*database.UnitTagSuggestionAPI, error) {
	if id != m.suggestion.ID {
		return nil, database.ErrSuggestionNotFound
	}
	s := m.suggestion
	return &s, nil
}

func (m *mockSuggestionStore) ApproveUnitTagSuggestion(_ context.Context, id int64, alphaTag, decidedBy string) (*database.UnitTagApproval, error) {
	m.approveCalls++
	if id != m.suggestion.ID {
		return nil, database.ErrSuggestionNotFound
	}
	if m.suggestion.Status != database.SuggestionPending {
		return nil, &database.SuggestionStatusError{Status: m.suggestion.Status}
	}
	if !m.unitExists {
		return nil, database.ErrSuggestionUnitNotFound
	}
	m.approveTag, m.decidedBy = alphaTag, decidedBy
	applied := alphaTag
	if applied == "" {
		applied = m.suggestion.ProposedTag
	}
	prev, prevSrc := m.unitTag, m.unitSource
	m.unitTag, m.unitSource = applied, "manual"
	m.suggestion.Status = database.SuggestionApproved
	m.suggestion.AppliedTag = &applied
	m.suggestion.PreviousTag, m.suggestion.PreviousTagSource = &prev, &prevSrc
	return &database.UnitTagApproval{SuggestionID: id, SystemID: m.suggestion.SystemID, UnitID: m.suggestion.UnitID, AppliedTag: applied}, nil
}

func (m *mockSuggestionStore) DismissUnitTagSuggestion(_ context.Context, id int64, decidedBy string) error {
	m.dismissCalled = true
	if id != m.suggestion.ID {
		return database.ErrSuggestionNotFound
	}
	if m.suggestion.Status != database.SuggestionPending {
		return &database.SuggestionStatusError{Status: m.suggestion.Status}
	}
	m.decidedBy = decidedBy
	m.suggestion.Status = database.SuggestionDismissed
	return nil
}

func (m *mockSuggestionStore) GetUnitTagScanStatus(context.Context) (database.UnitTagScanStatus, error) {
	return database.UnitTagScanStatus{LastTranscriptionID: 40, MaxTranscriptionID: 50}, nil
}

func (m *mockSuggestionStore) GetUnitByComposite(_ context.Context, systemID, unitID int) (*database.UnitAPI, error) {
	if !m.unitExists || systemID != m.suggestion.SystemID || unitID != m.suggestion.UnitID {
		return nil, errors.New("no rows")
	}
	return &database.UnitAPI{SystemID: systemID, UnitID: unitID, AlphaTag: m.unitTag, AlphaTagSource: m.unitSource}, nil
}

func newPendingStore() *mockSuggestionStore {
	return &mockSuggestionStore{
		suggestion: database.UnitTagSuggestionAPI{
			ID: 7, SystemID: 1, UnitID: 1001, TagKey: "MEDIC 12", ProposedTag: "Medic 12",
			Status: database.SuggestionPending, CallCount: 4, Evidence: []database.UnitTagEvidence{},
		},
		unitTag: "1001", unitSource: "mqtt", unitExists: true,
	}
}

// serveSuggestions routes a request through the handler's real chi routes.
func serveSuggestions(h *UnitTagSuggestionsHandler, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	h.Routes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeSuggestionBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func TestListUnitTagSuggestions(t *testing.T) {
	t.Run("defaults to pending and passes the review gate", func(t *testing.T) {
		m := &mockSuggestionStore{list: []database.UnitTagSuggestionAPI{{ID: 1, ProposedTag: "Medic 12"}}, total: 1}
		h := &UnitTagSuggestionsHandler{db: m, scannerEnabled: true, minCalls: 3, minShare: 0.2}
		rec := serveSuggestions(h, httptest.NewRequest("GET", "/unit-tag-suggestions?system_id=1,2&unit_id=1001&limit=10&offset=5", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		f := m.filter
		if f.Status != "pending" || f.MinCalls != 3 || f.MinShare != 0.2 || f.Limit != 10 || f.Offset != 5 ||
			len(f.SystemIDs) != 2 || f.UnitIDs[0] != 1001 {
			t.Errorf("filter = %+v", f)
		}
		var body struct {
			Suggestions []database.UnitTagSuggestionAPI `json:"suggestions"`
			Total       int                             `json:"total"`
			Scanner     map[string]any                  `json:"scanner"`
		}
		decodeSuggestionBody(t, rec, &body)
		if len(body.Suggestions) != 1 || body.Total != 1 {
			t.Errorf("body = %+v", body)
		}
		if body.Scanner["enabled"] != true || body.Scanner["min_calls"] != float64(3) ||
			body.Scanner["last_transcription_id"] != float64(40) || body.Scanner["max_transcription_id"] != float64(50) {
			t.Errorf("scanner = %+v", body.Scanner)
		}
	})

	t.Run("status all", func(t *testing.T) {
		m := &mockSuggestionStore{}
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("GET", "/unit-tag-suggestions?status=all", nil))
		if rec.Code != http.StatusOK || m.filter.Status != "all" {
			t.Errorf("status = %d filter = %+v", rec.Code, m.filter)
		}
	})

	t.Run("invalid status", func(t *testing.T) {
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: &mockSuggestionStore{}}, httptest.NewRequest("GET", "/unit-tag-suggestions?status=bogus", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("db error", func(t *testing.T) {
		m := &mockSuggestionStore{listErr: errors.New("boom")}
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("GET", "/unit-tag-suggestions", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}

func TestApproveUnitTagSuggestion(t *testing.T) {
	t.Run("approve proposed tag, writes back CSV", func(t *testing.T) {
		m := newPendingStore()
		csv := filepath.Join(t.TempDir(), "units.csv")
		if err := os.WriteFile(csv, []byte("1001,Old Tag\n2002,Other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		h := &UnitTagSuggestionsHandler{db: m, csvPaths: map[int]string{1: csv}}
		req := httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", nil)
		req = withPrincipal(req, editorPrincipal("tr-dashboard at home", "alice"), nil)
		rec := serveSuggestions(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if m.approveTag != "" || m.decidedBy != "tr-dashboard at home / alice" {
			t.Errorf("approve called with tag=%q by=%q, want proposed tag by the key / alice", m.approveTag, m.decidedBy)
		}
		var body struct {
			Suggestion database.UnitTagSuggestionAPI `json:"suggestion"`
			Unit       database.UnitAPI              `json:"unit"`
		}
		decodeSuggestionBody(t, rec, &body)
		if body.Suggestion.Status != "approved" || *body.Suggestion.AppliedTag != "Medic 12" || *body.Suggestion.PreviousTag != "1001" {
			t.Errorf("suggestion = %+v", body.Suggestion)
		}
		if body.Unit.AlphaTag != "Medic 12" || body.Unit.AlphaTagSource != "manual" {
			t.Errorf("unit = %+v", body.Unit)
		}
		data, _ := os.ReadFile(csv)
		if !strings.Contains(string(data), "1001,Medic 12") || !strings.Contains(string(data), "2002,Other") {
			t.Errorf("CSV not written back: %q", data)
		}
	})

	t.Run("edit then approve trims the override", func(t *testing.T) {
		m := newPendingStore()
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m},
			httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", strings.NewReader(`{"alpha_tag":"  BCFD Medic 12 "}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if m.approveTag != "BCFD Medic 12" || m.unitTag != "BCFD Medic 12" {
			t.Errorf("approve tag = %q unit tag = %q", m.approveTag, m.unitTag)
		}
	})

	t.Run("empty object body approves the proposed tag", func(t *testing.T) {
		m := newPendingStore()
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m},
			httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", strings.NewReader(`{}`)))
		if rec.Code != http.StatusOK || m.unitTag != "Medic 12" {
			t.Errorf("status = %d unit tag = %q", rec.Code, m.unitTag)
		}
	})

	for _, tc := range []struct {
		name, body string
	}{
		{"blank override", `{"alpha_tag":"   "}`},
		{"malformed json", `{"alpha_tag":`},
	} {
		t.Run(tc.name+" is rejected", func(t *testing.T) {
			m := newPendingStore()
			rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m},
				httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if m.approveCalls != 0 || m.unitTag != "1001" {
				t.Error("unit must not change on a rejected request")
			}
		})
	}

	t.Run("already decided returns 409", func(t *testing.T) {
		for _, status := range []string{"approved", "dismissed"} {
			m := newPendingStore()
			m.suggestion.Status = status
			rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", nil))
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s: status = %d, want 409", status, rec.Code)
			}
			var body ErrorResponse
			decodeSuggestionBody(t, rec, &body)
			if body.Code != ErrConflict || body.Detail != "status: "+status {
				t.Errorf("%s: body = %+v", status, body)
			}
			if m.unitTag != "1001" {
				t.Errorf("%s: unit changed", status)
			}
		}
	})

	t.Run("approve twice: second is 409", func(t *testing.T) {
		m := newPendingStore()
		h := &UnitTagSuggestionsHandler{db: m}
		if rec := serveSuggestions(h, httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", nil)); rec.Code != http.StatusOK {
			t.Fatalf("first approve = %d", rec.Code)
		}
		if rec := serveSuggestions(h, httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", strings.NewReader(`{"alpha_tag":"X"}`))); rec.Code != http.StatusConflict {
			t.Errorf("second approve = %d, want 409", rec.Code)
		}
		if m.unitTag != "Medic 12" {
			t.Errorf("unit tag = %q, second approve must not apply", m.unitTag)
		}
	})

	t.Run("unknown suggestion is 404", func(t *testing.T) {
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: newPendingStore()}, httptest.NewRequest("POST", "/unit-tag-suggestions/99/approve", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("missing unit is 404", func(t *testing.T) {
		m := newPendingStore()
		m.unitExists = false
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("POST", "/unit-tag-suggestions/7/approve", nil))
		if rec.Code != http.StatusNotFound || m.suggestion.Status != "pending" {
			t.Errorf("status = %d suggestion status = %s", rec.Code, m.suggestion.Status)
		}
	})

	t.Run("invalid id is 400", func(t *testing.T) {
		for _, id := range []string{"abc", "0", "-3"} {
			rec := serveSuggestions(&UnitTagSuggestionsHandler{db: newPendingStore()}, httptest.NewRequest("POST", "/unit-tag-suggestions/"+id+"/approve", nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("id %s: status = %d, want 400", id, rec.Code)
			}
		}
	})
}

func TestDismissUnitTagSuggestion(t *testing.T) {
	t.Run("dismiss leaves the unit alone", func(t *testing.T) {
		m := newPendingStore()
		req := withPrincipal(httptest.NewRequest("POST", "/unit-tag-suggestions/7/dismiss", nil), editorPrincipal("review script", ""), nil)
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Suggestion database.UnitTagSuggestionAPI `json:"suggestion"`
		}
		decodeSuggestionBody(t, rec, &body)
		if body.Suggestion.Status != "dismissed" || m.decidedBy != "review script" || m.unitTag != "1001" || m.approveCalls != 0 {
			t.Errorf("suggestion = %+v decidedBy = %q unit = %q", body.Suggestion, m.decidedBy, m.unitTag)
		}
	})

	t.Run("dismiss after approve is 409", func(t *testing.T) {
		m := newPendingStore()
		m.suggestion.Status = "approved"
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("POST", "/unit-tag-suggestions/7/dismiss", nil))
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("unknown suggestion is 404", func(t *testing.T) {
		rec := serveSuggestions(&UnitTagSuggestionsHandler{db: newPendingStore()}, httptest.NewRequest("POST", "/unit-tag-suggestions/8/dismiss", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestGetUnitTagSuggestion(t *testing.T) {
	m := newPendingStore()
	m.suggestion.Evidence = []database.UnitTagEvidence{{CallID: 55, CallStartTime: time.Unix(0, 0).UTC(), AudioURL: "/api/v1/calls/55/audio"}}
	rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("GET", "/unit-tag-suggestions/7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var s database.UnitTagSuggestionAPI
	decodeSuggestionBody(t, rec, &s)
	if s.ID != 7 || len(s.Evidence) != 1 || s.Evidence[0].AudioURL != "/api/v1/calls/55/audio" {
		t.Errorf("suggestion = %+v", s)
	}
	if rec := serveSuggestions(&UnitTagSuggestionsHandler{db: m}, httptest.NewRequest("GET", "/unit-tag-suggestions/8", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", rec.Code)
	}
}

// editorPrincipal is an edit key's principal, acting for actor ("" = none).
func editorPrincipal(keyName, actor string) *auth.Principal {
	return &auth.Principal{Kind: auth.KindKey, KeyID: 3, KeyName: keyName, Scopes: auth.Scopes{auth.ScopeEdit}, Actor: actor}
}

// TestUnitTagSuggestionWriteAuth checks the routes' policies through the
// auth pipeline: reads need listen, approve and dismiss need edit (like
// PATCH /units/{id}), and the decision records the key name and X-Actor.
func TestUnitTagSuggestionWriteAuth(t *testing.T) {
	store := newStubAuthStore()
	store.add("listen-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	store.add("edit-key", database.APIKey{Name: "tr-dashboard", Scopes: auth.Scopes{auth.ScopeEdit}})
	a := newTestAuthenticator(store)
	r := chi.NewRouter()
	r.Use(Match(r, zerolog.Nop()), a.Resolve, a.Authorize, a.Audit)
	m := newPendingStore()
	(&UnitTagSuggestionsHandler{db: m}).Routes(prefixRouter{Router: r, prefix: apiPrefix})

	send := func(method, path string, headers map[string]string) int {
		return do(r, method, apiPrefix+path, headers).Code
	}

	if code := send("GET", "/unit-tag-suggestions", bearer("listen-key")); code != http.StatusOK {
		t.Errorf("list with a listen key = %d, want 200", code)
	}
	if code := send("POST", "/unit-tag-suggestions/7/approve", nil); code != http.StatusUnauthorized {
		t.Errorf("approve without a key = %d, want 401", code)
	}
	if code := send("POST", "/unit-tag-suggestions/7/approve", bearer("listen-key")); code != http.StatusForbidden {
		t.Errorf("approve with a listen key = %d, want 403", code)
	}
	if code := send("POST", "/unit-tag-suggestions/7/dismiss", bearer("listen-key")); code != http.StatusForbidden {
		t.Errorf("dismiss with a listen key = %d, want 403", code)
	}
	if m.approveCalls != 0 || m.dismissCalled {
		t.Fatal("store must not be reached without edit")
	}
	if code := send("POST", "/unit-tag-suggestions/7/approve", map[string]string{
		"Authorization": "Bearer edit-key", "X-Actor": " carol\u200e\n ",
	}); code != http.StatusOK {
		t.Errorf("approve with an edit key = %d, want 200", code)
	}
	if m.decidedBy != "tr-dashboard / carol" {
		t.Errorf("decided_by = %q, want the key name and the sanitized actor", m.decidedBy)
	}
	if entries := store.auditEntries(); len(entries) != 1 || entries[0].Path != "/api/v1/unit-tag-suggestions/7/approve" ||
		entries[0].Status != http.StatusOK || *entries[0].Actor != "carol" {
		t.Errorf("audit log = %+v, want the approve", entries)
	}
}
