package api

// End-to-end tests of the auth endpoints against a real PostgreSQL. Skipped
// unless TEST_DATABASE_URL points at a server where the user may CREATE
// DATABASE; each test creates a throwaway database (tr_engine_test_*),
// loads schema.sql + migrations, and drops it afterwards. Example:
//
//	TEST_DATABASE_URL=postgres://postgres@127.0.0.1:55432/postgres go test ./internal/api/

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

func integrationDB(t *testing.T) *database.DB {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := fmt.Sprintf("tr_engine_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		admin.Close()
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := database.Connect(ctx, u.String(), zerolog.Nop())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(db.Close)

	schema, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InitSchema(ctx, schema); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// dbRouter is the real router over a real database.
func dbRouter(t *testing.T, db *database.DB) *chi.Mux {
	t.Helper()
	opts := allFeaturesOptions(newAuthenticator(db, nil, 1e9, 1<<30, zerolog.Nop()))
	opts.DB = db
	return buildRouter(opts)
}

// call sends a JSON request and decodes a JSON response into out (if not nil).
func call(t *testing.T, h http.Handler, method, path, key string, body any, out any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	switch b := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(b)
	default:
		js, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(js))
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "192.0.2.30:4000"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, what string) {
	t.Helper()
	if rec.Code != status || (code != "" && errorCode(rec) != code) {
		t.Errorf("%s: got %d %s, want %d %s", what, rec.Code, rec.Body.String(), status, code)
	}
}

type createdKey struct {
	database.APIKey
	Key string `json:"key"`
}

func TestIntegrationKeysAPI(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	r := dbRouter(t, db)
	boot, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "first admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	admin := boot.Plaintext

	// whoami, as a key and anonymously.
	var who whoamiResponse
	call(t, r, "GET", "/api/v1/whoami", admin, nil, &who)
	if who.Credential != "key" || who.Key == nil || who.Key.ID != boot.ID || who.Key.Prefix != boot.Prefix ||
		strings.Join(who.Scopes.Strings(), ",") != "admin,edit,listen" || who.Restricted || who.Anonymous.Access != "off" ||
		who.Version != "v9.9.9" {
		t.Errorf("whoami(admin) = %+v", who)
	}
	var anonWho map[string]any
	call(t, r, "GET", "/api/v1/whoami", "", nil, &anonWho)
	if anonWho["credential"] != "anonymous" || anonWho["key"] != nil || fmt.Sprint(anonWho["scopes"]) != "[]" {
		t.Errorf("whoami(anonymous) = %v", anonWho)
	}

	// Create: validation.
	for name, body := range map[string]string{
		"listen+edit":            `{"name":"x","scopes":["listen","edit"]}`,
		"unknown scope":          `{"name":"x","scopes":["root"]}`,
		"no scopes":              `{"name":"x"}`,
		"no name":                `{"scopes":["listen"]}`,
		"restriction on edit":    `{"name":"x","scopes":["edit"],"restriction":{"systems":[1]}}`,
		"allow-nothing":          `{"name":"x","scopes":["listen"],"restriction":{}}`,
		"exclude only":           `{"name":"x","scopes":["listen"],"restriction":{"exclude_talkgroups":["1:5"]}}`,
		"expiry in the past":     `{"name":"x","scopes":["listen"],"expires_at":"2001-01-01T00:00:00Z"}`,
		"zero rate limit":        `{"name":"x","scopes":["listen"],"rate_limit_rps":0}`,
		"unknown field":          `{"name":"x","scopes":["listen"],"role":"admin"}`,
		"misspelled restriction": `{"name":"x","scopes":["listen"],"restriction":{"exclude_talkgroup":["1:5"],"allow_all":true}}`,
		"bad talkgroup":          `{"name":"x","scopes":["listen"],"restriction":{"talkgroups":["1-5"]}}`,
	} {
		expectStatus(t, call(t, r, "POST", "/api/v1/keys", admin, body, nil), 400, ErrInvalidBody, "create "+name)
	}

	// Create a restricted listen key and use it.
	var listen createdKey
	rec := call(t, r, "POST", "/api/v1/keys", admin, map[string]any{
		"name": "club website", "scopes": []string{"listen"},
		"restriction": map[string]any{"systems": []int{1}}, "rate_limit_rps": 5,
	}, &listen)
	expectStatus(t, rec, 201, "", "create listen key")
	if !strings.HasPrefix(listen.Key, "tre_") || len(listen.Key) != 68 || listen.Prefix != listen.Key[:12] ||
		listen.Restriction == nil || listen.Status != database.KeyActive {
		t.Fatalf("created = %+v", listen)
	}
	if strings.Contains(rec.Body.String(), "key_hash") {
		t.Error("the response exposes the key hash")
	}
	call(t, r, "GET", "/api/v1/whoami", listen.Key, nil, &who)
	if !who.Restricted || who.Key.Restriction == nil || strings.Join(who.Scopes.Strings(), ",") != "listen" {
		t.Errorf("whoami(restricted) = %+v", who)
	}

	// Get and list.
	var got database.APIKey
	expectStatus(t, call(t, r, "GET", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin, nil, &got), 200, "", "get key")
	if got.Name != "club website" || got.RateLimitRPS == nil || *got.RateLimitRPS != 5 {
		t.Errorf("get = %+v", got)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/keys/99999", admin, nil, nil), 404, ErrNotFound, "get unknown key")
	expectStatus(t, call(t, r, "GET", "/api/v1/keys/abc", admin, nil, nil), 400, ErrInvalidParameter, "get bad id")
	var list struct {
		Keys  []database.APIKey `json:"keys"`
		Total int               `json:"total"`
	}
	call(t, r, "GET", "/api/v1/keys", admin, nil, &list)
	if list.Total != 2 || len(list.Keys) != 2 || list.Keys[0].ID != boot.ID {
		t.Errorf("list = %+v", list)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/keys?include_revoked=maybe", admin, nil, nil), 400, ErrInvalidParameter, "bad include_revoked")
	expectStatus(t, call(t, r, "GET", "/api/v1/keys", listen.Key, nil, nil), 403, "", "list with a listen key")

	// PATCH: absent fields are left alone, null clears.
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	var patched database.APIKey
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin,
		map[string]any{"expires_at": exp}, &patched), 200, "", "set expiry")
	if patched.ExpiresAt == nil || !patched.ExpiresAt.Equal(exp) || patched.RateLimitRPS == nil || patched.Restriction == nil {
		t.Errorf("after setting expiry: %+v", patched)
	}
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin,
		`{"rate_limit_rps": null, "name": "club website v2"}`, &patched), 200, "", "clear rate limit")
	if patched.RateLimitRPS != nil || patched.ExpiresAt == nil || patched.Name != "club website v2" || patched.Restriction == nil {
		t.Errorf("after clearing the rate limit: %+v", patched)
	}
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin,
		`{"scopes": ["edit"]}`, nil), 400, ErrInvalidBody, "scopes away from listen with a restriction")
	for name, body := range map[string]string{
		"null name":     `{"name": null}`,
		"null scopes":   `{"scopes": null}`,
		"unknown field": `{"label": "x"}`,
		"not an object": `[1]`,
		"bad expiry":    `{"expires_at": "tomorrow"}`,
	} {
		expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin, body, nil), 400, ErrInvalidBody, "patch "+name)
	}

	// A PATCH takes effect on the next request: the key is in the cache now.
	expectStatus(t, call(t, r, "GET", "/api/v1/whoami", listen.Key, nil, nil), 200, "", "restricted key before patch")
	gen := auth.Generation()
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin,
		`{"scopes": ["upload"], "restriction": null}`, &patched), 200, "", "scopes to upload, clearing the restriction")
	if auth.Generation() == gen {
		t.Error("PATCH did not bump the auth generation")
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/talkgroups", listen.Key, nil, nil), 403, ErrInsufficientScope, "upload key after patch")

	// Last-admin guard.
	expectStatus(t, call(t, r, "DELETE", fmt.Sprintf("/api/v1/keys/%d", boot.ID), admin, nil, nil), 409, ErrConflict, "revoke the last admin")
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", boot.ID), admin, `{"scopes":["edit"]}`, nil), 409, ErrConflict, "demote the last admin")
	var second createdKey
	expectStatus(t, call(t, r, "POST", "/api/v1/keys", admin, `{"name":"second admin","scopes":["admin"]}`, &second), 201, "", "second admin")
	expectStatus(t, call(t, r, "DELETE", fmt.Sprintf("/api/v1/keys/%d", boot.ID), second.Key, nil, nil), 204, "", "revoke the first admin")
	expectStatus(t, call(t, r, "DELETE", fmt.Sprintf("/api/v1/keys/%d", boot.ID), second.Key, nil, nil), 204, "", "revoke again (idempotent)")
	rec = call(t, r, "GET", "/api/v1/whoami", admin, nil, nil)
	expectStatus(t, rec, 401, ErrInvalidKey, "revoked admin key")
	if !strings.Contains(rec.Body.String(), "revoked") {
		t.Errorf("revoked key message = %s", rec.Body.String())
	}
	expectStatus(t, call(t, r, "PATCH", fmt.Sprintf("/api/v1/keys/%d", boot.ID), second.Key, `{"name":"x"}`, nil), 409, ErrConflict, "patch a revoked key")
	expectStatus(t, call(t, r, "DELETE", "/api/v1/keys/99999", second.Key, nil, nil), 404, ErrNotFound, "revoke unknown")
	call(t, r, "GET", "/api/v1/keys?include_revoked=true", second.Key, nil, &list)
	if list.Total != 3 {
		t.Errorf("include_revoked total = %d, want 3", list.Total)
	}

	// The audit log saw the key changes, not the reads.
	var audit struct {
		Entries []database.AuditEntry `json:"entries"`
		Total   int                   `json:"total"`
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/admin/audit-log?limit=100", second.Key, nil, &audit), 200, "", "audit log")
	var lines []string
	for _, e := range audit.Entries {
		lines = append(lines, fmt.Sprintf("%s %s %d %s", e.Method, e.Path, e.Status, e.KeyName))
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"POST /api/v1/keys 201 first admin",
		"POST /api/v1/keys 400 first admin",
		fmt.Sprintf("DELETE /api/v1/keys/%d 409 first admin", boot.ID),
		fmt.Sprintf("DELETE /api/v1/keys/%d 204 second admin", boot.ID),
	} {
		if !strings.Contains(all, want) {
			t.Errorf("audit log lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "GET ") {
		t.Errorf("reads were audited:\n%s", all)
	}
	var filtered struct {
		Total int `json:"total"`
	}
	call(t, r, "GET", fmt.Sprintf("/api/v1/admin/audit-log?key_id=%d", second.ID), second.Key, nil, &filtered)
	if filtered.Total != 4 { // the second admin's two revokes, one PATCH and one unknown revoke
		t.Errorf("audit entries by the second admin = %d, want 4", filtered.Total)
	}
	call(t, r, "GET", "/api/v1/admin/audit-log?since="+url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339)), second.Key, nil, &filtered)
	if filtered.Total != 0 {
		t.Errorf("entries in the future = %d", filtered.Total)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/admin/audit-log?since=yesterday", second.Key, nil, nil), 400, ErrInvalidParameter, "bad since")
	expectStatus(t, call(t, r, "GET", "/api/v1/admin/audit-log?key_id=-1", second.Key, nil, nil), 400, ErrInvalidParameter, "bad key_id")

	// A long path is stored capped, with a marker giving the requested
	// path's length, not the length of an already-capped copy (r2-12).
	for _, n := range []int{1500, 5000} {
		long := "/api/v1/talkgroups/1:" + strings.Repeat("9", n)
		call(t, r, "PATCH", long+"?q=secret", second.Key, `{"alpha_tag":"x"}`, nil)
		var one struct {
			Entries []database.AuditEntry `json:"entries"`
		}
		call(t, r, "GET", "/api/v1/admin/audit-log?limit=1", second.Key, nil, &one)
		want := fmt.Sprintf("…(truncated, %d bytes)", len(long))
		if len(one.Entries) != 1 {
			t.Fatalf("audit entries = %+v", one.Entries)
		}
		if !strings.HasSuffix(one.Entries[0].Path, want) ||
			!strings.HasPrefix(one.Entries[0].Path, long[:database.MaxAuditPathBytes]) ||
			len(one.Entries[0].Path) != database.MaxAuditPathBytes+len(want) {
			t.Errorf("long path (%d bytes) stored as %.60q...%q", len(long), one.Entries[0].Path,
				one.Entries[0].Path[max(0, len(one.Entries[0].Path)-40):])
		}
	}
}

func TestIntegrationAnonymousAccessAPI(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	r := dbRouter(t, db)
	admin, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}

	var got database.AnonymousAccess
	call(t, r, "GET", "/api/v1/anonymous-access", admin.Plaintext, nil, &got)
	if got.Access != "off" || got.Restriction != nil {
		t.Errorf("default policy = %+v", got)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/talkgroups", "", nil, nil), 401, ErrKeyRequired, "anonymous while off")

	for name, body := range map[string]string{
		"missing restriction": `{"access":"listen"}`,
		"missing access":      `{"restriction":null}`,
		"bad access":          `{"access":"edit","restriction":null}`,
		"allow nothing":       `{"access":"listen","restriction":{}}`,
		"exclude only":        `{"access":"listen","restriction":{"exclude_talkgroups":["1:2"]}}`,
		"restriction array":   `{"access":"listen","restriction":[]}`,
		"unknown field":       `{"access":"listen","restriction":null,"extra":1}`,
	} {
		expectStatus(t, call(t, r, "PUT", "/api/v1/anonymous-access", admin.Plaintext, body, nil), 400, ErrInvalidBody, name)
	}
	rec := call(t, r, "PUT", "/api/v1/anonymous-access", admin.Plaintext, `{"access":"listen","restriction":{}}`, nil)
	if !strings.Contains(rec.Body.String(), "access: off") {
		t.Errorf("allow-nothing message = %s", rec.Body.String())
	}

	// listen, unrestricted: takes effect for the next request.
	expectStatus(t, call(t, r, "PUT", "/api/v1/anonymous-access", admin.Plaintext, `{"access":"listen","restriction":null}`, &got), 200, "", "set listen")
	if got.Access != "listen" || got.UpdatedAt == nil {
		t.Errorf("after PUT: %+v", got)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/talkgroups", "", nil, nil), 200, "", "anonymous while listen")
	expectStatus(t, call(t, r, "GET", "/api/v1/keys", "", nil, nil), 401, ErrKeyRequired, "anonymous on admin route")
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", "", nil, nil), 401, ErrKeyRequired, "anonymous tickets")

	// Restricted: whoami shows only that it is restricted.
	expectStatus(t, call(t, r, "PUT", "/api/v1/anonymous-access", admin.Plaintext,
		`{"access":"listen","restriction":{"allow_all":true,"exclude_talkgroups":["1:5001"]}}`, &got), 200, "", "set restricted")
	var who map[string]any
	call(t, r, "GET", "/api/v1/whoami", "", nil, &who)
	if who["restricted"] != true || fmt.Sprint(who["anonymous"]) != "map[access:listen restricted:true]" {
		t.Errorf("whoami = %v", who)
	}
	if b, _ := json.Marshal(who); strings.Contains(string(b), "5001") {
		t.Errorf("whoami reveals the exclusions: %s", b)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/units", "", nil, nil), 403, ErrRestrictedCredential, "restricted anonymous on a Deny route")
	expectStatus(t, call(t, r, "GET", "/api/v1/stats", "", nil, nil), 403, ErrRestrictedCredential, "restricted anonymous on a Deny route")
	expectStatus(t, call(t, r, "GET", "/api/v1/talkgroups", "", nil, nil), 200, "", "restricted anonymous on an Enforced route")

	// Health is trimmed for anonymous callers and full for the admin key.
	var health map[string]any
	call(t, r, "GET", "/api/v1/health", "", nil, &health)
	if _, ok := health["database_pool"]; ok || health["checks"] == nil {
		t.Errorf("anonymous health = %v", health)
	}
	call(t, r, "GET", "/api/v1/health", admin.Plaintext, nil, &health)
	if _, ok := health["database_pool"]; !ok {
		t.Errorf("admin health = %v, want the full body", health)
	}

	// Off again.
	expectStatus(t, call(t, r, "PUT", "/api/v1/anonymous-access", admin.Plaintext, `{"access":"off","restriction":null}`, nil), 200, "", "set off")
	expectStatus(t, call(t, r, "GET", "/api/v1/talkgroups", "", nil, nil), 401, ErrKeyRequired, "anonymous after off")
}

func TestIntegrationTickets(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	r := dbRouter(t, db)
	mustExec := func(sql string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO systems (system_id, system_type, name, sysid, wacn) VALUES
		(1, 'p25', 'target', '348', 'BEE00'), (2, 'p25', 'source', '348', 'BEE00')`)
	admin, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "ops", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	listen, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "dashboard", Scopes: auth.Scopes{auth.ScopeListen}})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "uploader", Scopes: auth.Scopes{auth.ScopeUpload}})
	if err != nil {
		t.Fatal(err)
	}

	var tk ticketResponse
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, nil, &tk), 200, "", "mint without a body")
	if !strings.HasPrefix(tk.Ticket, "trt_") {
		t.Fatalf("ticket = %+v", tk)
	}
	if d := time.Until(tk.ExpiresAt); d < 9*time.Minute || d > 10*time.Minute+time.Second {
		t.Errorf("default expiry in %v, want ~10m", d)
	}
	call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"ttl_seconds": 5}`, &tk)
	if d := time.Until(tk.ExpiresAt); d < 50*time.Second || d > 61*time.Second {
		t.Errorf("ttl 5 → expiry in %v, want the 60 s minimum", d)
	}
	call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"ttl_seconds": 9223372036854775807}`, &tk)
	if d := time.Until(tk.ExpiresAt); d < 59*time.Minute || d > time.Hour+time.Second {
		t.Errorf("huge ttl → expiry in %v, want the 1 h maximum", d)
	}
	tgs := make([]string, 101)
	for i := range tgs {
		tgs[i] = fmt.Sprintf(`"1:%d"`, i+1)
	}
	rec := call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"restriction":{"talkgroups":[`+strings.Join(tgs, ",")+`]}}`, nil)
	expectStatus(t, rec, 400, ErrInvalidBody, "101-entry narrowing")
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("message = %s", rec.Body.String())
	}
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"ttl":60}`, nil), 400, ErrInvalidBody, "unknown field")
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", upload.Plaintext, nil, nil), 403, ErrInsufficientScope, "upload-only key")

	// A ticket authenticates the audio route (404: no such call) but nothing
	// else, and dies with its key.
	call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, nil, &tk)
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42/audio?ticket="+tk.Ticket, "", nil, nil), 404, ErrNotFound, "audio with a ticket")
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42/audio?ticket="+tk.Ticket, "tre_injected_by_a_proxy", nil, nil), 404, ErrNotFound, "ticket beats the header")
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42?ticket="+tk.Ticket, "", nil, nil), 401, ErrKeyRequired, "ticket on a non-ticket route")
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42/audio?ticket="+tk.Ticket+"x", "", nil, nil), 401, ErrInvalidTicket, "tampered ticket")

	// A narrowing that names a merged-away system is rejected after the merge.
	var narrowed ticketResponse
	call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"restriction":{"systems":[2]}}`, &narrowed)
	expectStatus(t, call(t, r, "POST", "/api/v1/admin/systems/merge", admin.Plaintext, `{"source_id":2,"target_id":1}`, nil,
		"X-Actor", "alice"), 200, "", "merge")
	var by string
	if err := db.Pool.QueryRow(ctx, `SELECT performed_by FROM system_merge_log ORDER BY id DESC LIMIT 1`).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by != "ops / alice" {
		t.Errorf("performed_by = %q, want the key name and actor", by)
	}
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42/audio?ticket="+narrowed.Ticket, "", nil, nil), 401, ErrInvalidTicket, "ticket naming a merged system")
	// ...and refused at mint time once the merge happened.
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"restriction":{"systems":[2]}}`, nil),
		400, ErrInvalidBody, "minting a ticket naming a merged system")
	expectStatus(t, call(t, r, "POST", "/api/v1/tickets", listen.Plaintext, `{"restriction":{"systems":[1]}}`, nil),
		200, "", "minting a ticket naming the merge target")

	// Revoking the key kills its tickets at once.
	expectStatus(t, call(t, r, "DELETE", fmt.Sprintf("/api/v1/keys/%d", listen.ID), admin.Plaintext, nil, nil), 204, "", "revoke")
	expectStatus(t, call(t, r, "GET", "/api/v1/calls/42/audio?ticket="+tk.Ticket, "", nil, nil), 401, ErrInvalidTicket, "ticket of a revoked key")
}
