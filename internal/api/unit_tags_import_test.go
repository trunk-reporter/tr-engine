package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

type importedUnitTag struct {
	SystemID int
	UnitID   int
	AlphaTag string
}

// mockUnitTagImporter implements unitTagImporter for testing.
type mockUnitTagImporter struct {
	systems    map[int]bool                         // existing system IDs
	named      map[string][]database.AmbiguousMatch // existing systems by name
	lookedUp   string                               // last name passed to FindSystemsByName
	resolveErr error
	importErr  error
	imports    int // ImportUnitTags calls
	imported   []importedUnitTag
}

func (m *mockUnitTagImporter) GetSystemByID(_ context.Context, _ *auth.Principal, systemID int) (*database.SystemAPI, error) {
	if !m.systems[systemID] {
		return nil, errors.New("no rows in result set")
	}
	return &database.SystemAPI{SystemID: systemID}, nil
}

func (m *mockUnitTagImporter) FindSystemsByName(_ context.Context, name string) ([]database.AmbiguousMatch, error) {
	m.lookedUp = name
	if m.resolveErr != nil {
		return nil, m.resolveErr
	}
	return m.named[name], nil
}

func (m *mockUnitTagImporter) ImportUnitTags(_ context.Context, systemID int, tags []database.UnitTag) (int64, error) {
	m.imports++
	if m.importErr != nil {
		return 0, m.importErr
	}
	for _, t := range tags {
		m.imported = append(m.imported, importedUnitTag{systemID, t.UnitID, t.AlphaTag})
	}
	return int64(len(tags)), nil
}

func postUnitTags(t *testing.T, h *UnitsHandler, query string, csv []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildMultipartForm(t, nil, "file", csv, "unitTags.csv")
	req := httptest.NewRequest("POST", "/api/v1/unit-tags/import"+query, body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ImportUnitTags(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestImportUnitTags(t *testing.T) {
	t.Run("imports_by_system_id", func(t *testing.T) {
		mock := &mockUnitTagImporter{systems: map[int]bool{3: true}}
		h := &UnitsHandler{tags: mock}

		csv := "\ufeffUnit ID,Alpha Tag\r\n1001,Engine 1\r\n\r\nbad,row\r\n1002,\"Medic 2, Station 4\"\r\n1003,\r\n1001,Engine 1 (new)\r\n"
		rec := postUnitTags(t, h, "?system_id=3", []byte(csv))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		want := map[string]any{"imported": 3.0, "total": 3.0, "system_id": 3.0, "skipped": 2.0, "duplicates": 1.0}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("%s = %v, want %v", k, body[k], v)
			}
		}
		wantImported := []importedUnitTag{
			{3, 1001, "Engine 1"},
			{3, 1002, "Medic 2, Station 4"},
			{3, 1001, "Engine 1 (new)"}, // passed in file order; ImportUnitTags keeps the last per unit
		}
		if len(mock.imported) != len(wantImported) {
			t.Fatalf("imported = %+v, want %+v", mock.imported, wantImported)
		}
		for i := range wantImported {
			if mock.imported[i] != wantImported[i] {
				t.Errorf("imported[%d] = %+v, want %+v", i, mock.imported[i], wantImported[i])
			}
		}
	})

	t.Run("skipped_always_present", func(t *testing.T) {
		mock := &mockUnitTagImporter{systems: map[int]bool{1: true}}
		rec := postUnitTags(t, &UnitsHandler{tags: mock}, "?system_id=1", []byte("1001,Engine 1\n"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["skipped"] != 0.0 {
			t.Errorf("skipped = %v, want 0", body["skipped"])
		}
		if _, ok := body["duplicates"]; ok {
			t.Errorf("duplicates present = %v, want omitted when 0", body["duplicates"])
		}
	})

	t.Run("system_name_uses_existing_system", func(t *testing.T) {
		mock := &mockUnitTagImporter{
			named: map[string][]database.AmbiguousMatch{"butco": {{SystemID: 4, SystemName: "butco"}}},
		}
		rec := postUnitTags(t, &UnitsHandler{tags: mock}, "?system_name=%20butco%20", []byte("1001,Engine 1\n"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if mock.lookedUp != "butco" {
			t.Errorf("looked up name %q, want trimmed butco", mock.lookedUp)
		}
		if body := decodeBody(t, rec); body["system_id"] != 4.0 {
			t.Errorf("system_id = %v, want 4", body["system_id"])
		}
		if len(mock.imported) != 1 || mock.imported[0].SystemID != 4 {
			t.Errorf("imported = %+v, want one row for system 4", mock.imported)
		}
	})

	// An unknown name is a 404, not a new system: ingest would never route
	// traffic to a system created here (it finds systems through their sites).
	t.Run("system_name_unknown_not_found", func(t *testing.T) {
		mock := &mockUnitTagImporter{named: map[string][]database.AmbiguousMatch{
			"butco": {{SystemID: 4, SystemName: "butco"}},
		}}
		rec := postUnitTags(t, &UnitsHandler{tags: mock}, "?system_name=newsys", []byte("1001,Engine 1\n"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		if body := decodeBody(t, rec); !strings.Contains(fmt.Sprint(body["error"]), `"newsys"`) {
			t.Errorf("error = %v, want it to name the system", body["error"])
		}
		if mock.imports != 0 {
			t.Errorf("imports = %d, want none", mock.imports)
		}
	})

	t.Run("system_name_ambiguous", func(t *testing.T) {
		mock := &mockUnitTagImporter{named: map[string][]database.AmbiguousMatch{
			"butco": {{SystemID: 1, SystemName: "butco"}, {SystemID: 3, SystemName: "butco"}},
		}}
		rec := postUnitTags(t, &UnitsHandler{tags: mock}, "?system_name=butco", []byte("1001,Engine 1\n"))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["code"] != string(ErrAmbiguousID) {
			t.Errorf("code = %v, want %s", body["code"], ErrAmbiguousID)
		}
		if matches, _ := body["matches"].([]any); len(matches) != 2 {
			t.Errorf("matches = %v, want 2 systems", body["matches"])
		}
		if mock.imports != 0 {
			t.Errorf("imports = %d, want no import", mock.imports)
		}
	})

	t.Run("import_failure", func(t *testing.T) {
		mock := &mockUnitTagImporter{systems: map[int]bool{1: true}, importErr: errors.New("db error")}
		rec := postUnitTags(t, &UnitsHandler{tags: mock}, "?system_id=1", []byte("1001,A\n1002,B\n"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
		}
		if body := decodeBody(t, rec); body["error"] == nil || body["imported"] != nil {
			t.Errorf("body = %v, want an error and no counts", body)
		}
	})

	errorCases := []struct {
		name   string
		mock   *mockUnitTagImporter
		query  string
		csv    []byte // nil = no file part
		status int
	}{
		{"missing_system_param", &mockUnitTagImporter{}, "", []byte("1001,A\n"), http.StatusBadRequest},
		{"unknown_system_id", &mockUnitTagImporter{systems: map[int]bool{}}, "?system_id=99", []byte("1001,A\n"), http.StatusNotFound},
		{"system_name_error", &mockUnitTagImporter{resolveErr: errors.New("boom")}, "?system_name=x", []byte("1001,A\n"), http.StatusInternalServerError},
		{"blank_system_name", &mockUnitTagImporter{}, "?system_name=%20", []byte("1001,A\n"), http.StatusBadRequest},
		{"missing_file", &mockUnitTagImporter{systems: map[int]bool{1: true}}, "?system_id=1", nil, http.StatusBadRequest},
		{"no_valid_rows", &mockUnitTagImporter{systems: map[int]bool{1: true}}, "?system_id=1", []byte("Unit ID,Alpha Tag\nabc,def\n1001,\n"), http.StatusBadRequest},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postUnitTags(t, &UnitsHandler{tags: tc.mock}, tc.query, tc.csv)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if body := decodeBody(t, rec); body["error"] == "" || body["error"] == nil {
				t.Errorf("expected error message, got %v", body)
			}
			if len(tc.mock.imported) != 0 {
				t.Errorf("imported = %+v, want none", tc.mock.imported)
			}
		})
	}

	t.Run("not_multipart", func(t *testing.T) {
		mock := &mockUnitTagImporter{systems: map[int]bool{1: true}}
		req := httptest.NewRequest("POST", "/api/v1/unit-tags/import?system_id=1", nil)
		req.Header.Set("Content-Type", "text/csv")
		rec := httptest.NewRecorder()
		(&UnitsHandler{tags: mock}).ImportUnitTags(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestImportUnitTags_RouteAdminOnly checks the route is registered on the
// units router and needs admin (§6.3), like the talkgroup-directory import.
func TestImportUnitTags_RouteAdminOnly(t *testing.T) {
	mock := &mockUnitTagImporter{systems: map[int]bool{1: true}}
	h := &UnitsHandler{tags: mock}

	store := newStubAuthStore()
	store.add("edit-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeEdit}})
	store.add("admin-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
	a := newTestAuthenticator(store)
	r := chi.NewRouter()
	r.Use(Match(r, zerolog.Nop()), a.Resolve, a.Authorize)
	h.Routes(prefixRouter{Router: r, prefix: apiPrefix})

	send := func(key string) int {
		body, ct := buildMultipartForm(t, nil, "file", []byte("1001,Engine 1\n"), "u.csv")
		req := httptest.NewRequest("POST", "/api/v1/unit-tags/import?system_id=1", body)
		req.Header.Set("Content-Type", ct)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send(""); code != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", code)
	}
	if code := send("edit-key"); code != http.StatusForbidden {
		t.Errorf("edit key: status = %d, want 403", code)
	}
	if len(mock.imported) != 0 {
		t.Fatalf("refused requests imported %+v", mock.imported)
	}
	if code := send("admin-key"); code != http.StatusOK {
		t.Errorf("admin key: status = %d, want 200", code)
	}
	if len(mock.imported) != 1 {
		t.Errorf("imported = %+v, want 1 row", mock.imported)
	}
}

func TestCheckTagSource(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		name    string
		source  *string
		allowed []string
		ok      bool
	}{
		{"nil", nil, unitTagSources, true},
		{"empty means unchanged", str(""), unitTagSources, true},
		{"manual", str("manual"), unitTagSources, true},
		{"csv", str("csv"), unitTagSources, true},
		{"unit rejects directory", str("directory"), unitTagSources, false},
		{"talkgroup allows directory", str("directory"), talkgroupTagSources, true},
		{"unknown", str("radioreference"), talkgroupTagSources, false},
		{"case sensitive", str("Manual"), unitTagSources, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if got := checkTagSource(rec, tt.source, tt.allowed); got != tt.ok {
				t.Fatalf("checkTagSource() = %v, want %v", got, tt.ok)
			}
			if !tt.ok && rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}
