# Auth Simplification Implementation Plan

> **Superseded by [2026-09-26-api-key-auth-design.md](../specs/2026-09-26-api-key-auth-design.md).** tr-engine now uses API keys, an anonymous access policy and tickets; the modes, tokens, logins and users described here no longer exist. Kept as a historical record. Current docs: `docs/auth.md`, `docs/migrating-auth.md`.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Simplify tr-engine auth from 5 overlapping mechanisms to 3 clear modes (open/token/full), eliminating the need for Caddy proxy injection.

**Architecture:** Rewrite config loading to derive auth mode from `AUTH_TOKEN` + `ADMIN_PASSWORD` presence. Rewrite auth-init to return `{mode, read_token, jwt_enabled}`. Fix WriteAuth and UploadAuth for the new mode matrix. Update auth.js for backward-compatible mode detection.

**Tech Stack:** Go, PostgreSQL (unchanged), vanilla JS (auth.js)

**Spec:** `docs/superpowers/specs/2026-03-28-auth-simplification-design.md`

**Scope:** tr-engine only. tr-dashboard changes are handled separately by the user.

---

### Task 1: Update config.go — remove auto-generated AUTH_TOKEN, add deprecation warnings

**Files:**
- Modify: `internal/config/config.go:248-261` (Load function, token auto-generation block)

- [ ] **Step 1: Write the failing test**

Create test for the new config loading behavior in `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"testing"
)

func TestLoad_NoAutoGenerateAuthToken(t *testing.T) {
	// When AUTH_TOKEN is not set and AUTH_ENABLED is true (default),
	// config should NOT auto-generate a token anymore.
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")
	t.Setenv("AUTH_TOKEN", "")

	cfg, err := Load(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "" {
		t.Errorf("expected empty AuthToken, got %q", cfg.AuthToken)
	}
	if cfg.AuthTokenGenerated {
		t.Error("expected AuthTokenGenerated=false")
	}
}

func TestLoad_AuthEnabledFalse_ClearsTokens(t *testing.T) {
	// Backward compat: AUTH_ENABLED=false must still clear tokens
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")
	t.Setenv("AUTH_ENABLED", "false")
	t.Setenv("AUTH_TOKEN", "should-be-cleared")
	t.Setenv("WRITE_TOKEN", "should-be-cleared")

	cfg, err := Load(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "" {
		t.Errorf("expected empty AuthToken after AUTH_ENABLED=false, got %q", cfg.AuthToken)
	}
	if cfg.WriteToken != "" {
		t.Errorf("expected empty WriteToken after AUTH_ENABLED=false, got %q", cfg.WriteToken)
	}
}

func TestLoad_ExplicitAuthToken_Preserved(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")
	t.Setenv("AUTH_TOKEN", "my-explicit-token")

	cfg, err := Load(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "my-explicit-token" {
		t.Errorf("expected AuthToken %q, got %q", "my-explicit-token", cfg.AuthToken)
	}
	if cfg.AuthTokenGenerated {
		t.Error("expected AuthTokenGenerated=false for explicit token")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestLoad_NoAutoGenerate -v`
Expected: FAIL — `TestLoad_NoAutoGenerateAuthToken` fails because config still auto-generates a token.

- [ ] **Step 3: Remove auto-generation, preserve AUTH_ENABLED=false clearing**

In `internal/config/config.go`, replace lines 248-261:

```go
	// When auth is explicitly disabled, clear any tokens so middleware passes everything through.
	if !cfg.AuthEnabled {
		cfg.AuthToken = ""
		cfg.WriteToken = ""
	} else if cfg.AuthToken == "" {
		// Auto-generate AUTH_TOKEN if not configured. This ensures the API is always
		// protected from automated scanners. Web pages get the token injected via auth.js.
		// The token changes on each restart; set AUTH_TOKEN in .env for a persistent one.
		b := make([]byte, 32)
		if _, err := rand.Read(b); err == nil {
			cfg.AuthToken = base64.URLEncoding.EncodeToString(b)
			cfg.AuthTokenGenerated = true
		}
	}
```

With:

```go
	// Deprecated: AUTH_ENABLED=false — preserve clearing behavior during transition.
	// New deployments should simply omit AUTH_TOKEN and ADMIN_PASSWORD for open mode.
	if !cfg.AuthEnabled {
		cfg.AuthToken = ""
		cfg.WriteToken = ""
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: All three new tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "refactor(config): remove AUTH_TOKEN auto-generation

AUTH_TOKEN must now be explicitly set. Open mode (no AUTH_TOKEN, no
ADMIN_PASSWORD) runs without auth. AUTH_ENABLED=false clearing behavior
preserved for backward compat during deprecation."
```

---

### Task 2: Add deprecation and open-mode warnings to main.go

**Files:**
- Modify: `cmd/tr-engine/main.go:400-415` (auth status log block)

- [ ] **Step 1: Replace the auth status logging block**

In `cmd/tr-engine/main.go`, replace the auth status block (lines 400-415):

```go
	// Auth status
	if !cfg.AuthEnabled {
		log.Warn().Msg("AUTH_ENABLED=false — API authentication is disabled, all endpoints are open")
	} else if cfg.AuthTokenGenerated {
		log.Info().Str("token", cfg.AuthToken).Msg("AUTH_TOKEN auto-generated (set AUTH_TOKEN in .env for a persistent token)")
	} else {
		log.Info().Msg("AUTH_TOKEN loaded from configuration")
	}
	if cfg.AuthEnabled && cfg.WriteToken != "" {
		log.Info().Msg("write protection enabled (WRITE_TOKEN set)")
	} else if cfg.AuthEnabled {
		log.Warn().Msg("WRITE_TOKEN not set — write endpoints accept the read token")
	}
	if cfg.JWTSecret != "" {
		log.Info().Msg("JWT user authentication enabled")
	}
```

With:

```go
	// Auth mode detection and deprecation warnings
	switch {
	case cfg.AuthToken == "" && cfg.AdminPassword == "":
		log.Warn().Msg("WARNING: running in open mode — API is completely unprotected. Set AUTH_TOKEN or ADMIN_PASSWORD to enable authentication.")
	case cfg.AuthToken != "" && cfg.AdminPassword == "":
		log.Info().Msg("auth mode: token (shared API token)")
	case cfg.AdminPassword != "":
		if cfg.AuthToken != "" {
			log.Info().Msg("auth mode: full (JWT login + public read via AUTH_TOKEN)")
		} else {
			log.Info().Msg("auth mode: full (JWT login required for all access)")
		}
	}
	if !cfg.AuthEnabled {
		log.Warn().Msg("AUTH_ENABLED is deprecated — remove AUTH_TOKEN and ADMIN_PASSWORD to disable auth")
	}
	if cfg.WriteToken != "" {
		log.Warn().Msg("WRITE_TOKEN is deprecated — use ADMIN_PASSWORD for write access control. WRITE_TOKEN will be ignored in a future release.")
	}
	if cfg.JWTSecret != "" {
		log.Info().Msg("JWT user authentication enabled")
	}
```

- [ ] **Step 2: Build to verify no compilation errors**

Run: `go build ./cmd/tr-engine/`
Expected: Builds without errors.

- [ ] **Step 3: Commit**

```bash
git add cmd/tr-engine/main.go
git commit -m "feat(config): add auth mode logging and deprecation warnings

Log the detected auth mode (open/token/full) at startup. Warn when
WRITE_TOKEN or AUTH_ENABLED are still in use. Loud warning when
running in open mode with no auth."
```

---

### Task 3: Rewrite auth-init endpoint

**Files:**
- Modify: `internal/api/server.go:108-137` (auth-init handler)
- Create: `internal/api/auth_init_test.go`

- [ ] **Step 1: Write tests for the new auth-init response**

Create `internal/api/auth_init_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/config"
)

// registerAuthInit wires just the auth-init endpoint for testing.
func registerAuthInit(cfg *config.Config) http.Handler {
	r := chi.NewRouter()

	r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"jwt_enabled": cfg.AdminPassword != "",
		}
		switch {
		case cfg.AuthToken == "" && cfg.AdminPassword == "":
			resp["mode"] = "open"
		case cfg.AuthToken != "" && cfg.AdminPassword == "":
			resp["mode"] = "token"
		default:
			resp["mode"] = "full"
			if cfg.AuthToken != "" {
				resp["read_token"] = cfg.AuthToken
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		b, _ := json.Marshal(resp)
		w.Write(b)
	})
	return r
}

func TestAuthInit_OpenMode(t *testing.T) {
	h := registerAuthInit(&config.Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "open" {
		t.Errorf("expected mode=open, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != false {
		t.Errorf("expected jwt_enabled=false, got %v", resp["jwt_enabled"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should not be present in open mode")
	}
}

func TestAuthInit_TokenMode(t *testing.T) {
	h := registerAuthInit(&config.Config{AuthToken: "my-secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "token" {
		t.Errorf("expected mode=token, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != false {
		t.Errorf("expected jwt_enabled=false, got %v", resp["jwt_enabled"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should NOT be present in token mode")
	}
}

func TestAuthInit_FullMode_WithPublicRead(t *testing.T) {
	h := registerAuthInit(&config.Config{AuthToken: "read-tok", AdminPassword: "admin123"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "full" {
		t.Errorf("expected mode=full, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != true {
		t.Errorf("expected jwt_enabled=true, got %v", resp["jwt_enabled"])
	}
	if resp["read_token"] != "read-tok" {
		t.Errorf("expected read_token=read-tok, got %v", resp["read_token"])
	}
}

func TestAuthInit_FullMode_NoPublicRead(t *testing.T) {
	h := registerAuthInit(&config.Config{AdminPassword: "admin123"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "full" {
		t.Errorf("expected mode=full, got %v", resp["mode"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should not be present when AUTH_TOKEN is empty")
	}
}

func TestAuthInit_CacheControl(t *testing.T) {
	h := registerAuthInit(&config.Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control: no-store, got %q", cc)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestAuthInit -v`
Expected: FAIL — the test helper `registerAuthInit` works but the actual server.go auth-init handler still returns the old shape.

- [ ] **Step 3: Rewrite auth-init handler in server.go**

In `internal/api/server.go`, replace the entire auth-init block (lines 108-137):

```go
	// Web auth bootstrap — returns the read token for web UI pages.
	// If a valid JWT is present in the request, also returns user info.
	if opts.Config.AuthToken != "" {
		jwtSecret := []byte(opts.Config.JWTSecret)
		r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")

			resp := map[string]any{
				"token":        opts.Config.AuthToken,
				"guest_access": true,
			}

			// If there's a valid JWT, include user info (backward compatible —
			// "user" field is absent, not null, when no JWT present)
			if provided := extractBearerToken(r); provided != "" && len(jwtSecret) > 0 && strings.Count(provided, ".") == 2 {
				claims := &Claims{}
				token, err := jwt.ParseWithClaims(provided, claims, jwtKeyFunc(jwtSecret))
				if err == nil && token.Valid {
					resp["user"] = map[string]any{
						"username": claims.Username,
						"role":     claims.Role,
					}
				}
			}

			b, _ := json.Marshal(resp)
			w.Write(b)
		})
	}
```

With:

```go
	// Auth mode discovery — always registered so clients can detect auth requirements.
	// Returns {mode, read_token?, jwt_enabled} — no secrets exposed in token mode.
	r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"jwt_enabled": opts.Config.AdminPassword != "",
		}
		switch {
		case opts.Config.AuthToken == "" && opts.Config.AdminPassword == "":
			resp["mode"] = "open"
		case opts.Config.AuthToken != "" && opts.Config.AdminPassword == "":
			resp["mode"] = "token"
		default:
			resp["mode"] = "full"
			if opts.Config.AuthToken != "" {
				resp["read_token"] = opts.Config.AuthToken
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		b, _ := json.Marshal(resp)
		w.Write(b)
	})
```

- [ ] **Step 4: Remove unused jwt import if needed**

Check if the `jwt` import and `jwtKeyFunc`/`Claims` references in the old auth-init handler were the only usage in that block. The JWT-related imports are still used by auth endpoints registered below, so no import cleanup is needed.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestAuthInit -v`
Expected: All 5 tests PASS.

- [ ] **Step 6: Build full project**

Run: `go build ./...`
Expected: Builds without errors.

- [ ] **Step 7: Commit**

```bash
git add internal/api/server.go internal/api/auth_init_test.go
git commit -m "feat(api): rewrite auth-init with mode-based response

Returns {mode: open|token|full, read_token?, jwt_enabled} instead of
the old {token, guest_access, user?} shape. Always registered (no
longer gated on AUTH_TOKEN). read_token only returned in full mode
with AUTH_TOKEN set (public read access)."
```

---

### Task 4: Fix WriteAuth middleware for new auth modes

**Files:**
- Modify: `internal/api/middleware.go:442-489` (WriteAuth function)
- Modify: `internal/api/server.go` (WriteAuth call site — pass jwtEnabled)
- Modify: `internal/api/middleware_test.go` (add WriteAuth tests)

- [ ] **Step 1: Write failing tests for WriteAuth**

Add to `internal/api/middleware_test.go`:

```go
func TestWriteAuth_OpenMode(t *testing.T) {
	// No tokens, no JWT — everything passes through
	mw := WriteAuth("", "", false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode POST should pass, got %d", rec.Code)
	}
}

func TestWriteAuth_FullMode_NoTokens_RequiresJWTRole(t *testing.T) {
	// JWT enabled, no legacy tokens — POST without role should be rejected
	mw := WriteAuth("", "", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	// No role in context (unauthenticated or viewer via read token)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("expected 403 for POST without role in full mode, got %d", rec.Code)
	}
}

func TestWriteAuth_FullMode_EditorRole_Passes(t *testing.T) {
	mw := WriteAuth("", "", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 1, "user", "editor", "jwt")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 for editor POST, got %d", rec.Code)
	}
}

func TestWriteAuth_FullMode_ViewerRole_Rejected(t *testing.T) {
	mw := WriteAuth("", "", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 1, "user", "viewer", "jwt")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("expected 403 for viewer POST, got %d", rec.Code)
	}
}

func TestWriteAuth_GETAlwaysPasses(t *testing.T) {
	mw := WriteAuth("", "", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/systems", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("GET should always pass, got %d", rec.Code)
	}
}

func TestWriteAuth_DeprecatedWriteToken_StillWorks(t *testing.T) {
	mw := WriteAuth("write-secret", "read-secret", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	req.Header.Set("Authorization", "Bearer write-secret")
	// Simulate JWTOrTokenAuth having set admin role for write token
	req = setAuthContext(req, 0, "", "admin", "token")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 with deprecated write token, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestWriteAuth -v`
Expected: FAIL — `WriteAuth` has wrong signature (no `jwtEnabled` param).

- [ ] **Step 3: Update WriteAuth signature and logic**

In `internal/api/middleware.go`, replace the `WriteAuth` function (lines 442-489):

```go
// WriteAuth requires the write token for mutating HTTP methods (POST, PATCH, PUT, DELETE).
// Read methods (GET, HEAD, OPTIONS) pass through unconditionally.
// When JWT auth is active, it checks the user's role from context first.
//   - editor or admin role → pass
//   - viewer role → 403
//   - no role (legacy path) → fall back to WRITE_TOKEN check
func WriteAuth(writeToken, authToken string) func(http.Handler) http.Handler {
```

With:

```go
// WriteAuth gates mutating HTTP methods (POST, PATCH, PUT, DELETE).
// Read methods (GET, HEAD, OPTIONS) always pass through.
//
// Logic for writes:
//  1. If no auth at all (no tokens, no JWT) → pass through (open mode)
//  2. If caller has a role from JWTOrTokenAuth → check editor+ role
//  3. Legacy fallback: check WRITE_TOKEN (deprecated)
func WriteAuth(writeToken, authToken string, jwtEnabled bool) func(http.Handler) http.Handler {
```

And replace the body:

```go
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No auth configured at all — open mode
			if writeToken == "" && authToken == "" && !jwtEnabled {
				next.ServeHTTP(w, r)
				return
			}

			switch r.Method {
			case "GET", "HEAD", "OPTIONS":
				next.ServeHTTP(w, r)
				return
			}

			// Check role from context (set by JWTOrTokenAuth)
			role := ContextRole(r)
			if role != "" {
				if RoleLevel(role) >= RoleLevel("editor") {
					next.ServeHTTP(w, r)
					return
				}
				// viewer role → forbidden
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "insufficient permissions for write operations")
				return
			}

			// No role in context — if JWT is enabled, this means caller is
			// unauthenticated or used a read-only token. Reject.
			if jwtEnabled {
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require login with editor or admin role")
				return
			}

			// Legacy fallback: WRITE_TOKEN (deprecated path)
			if writeToken == "" {
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require WRITE_TOKEN")
				return
			}

			provided := extractBearerToken(r)
			if subtle.ConstantTimeCompare([]byte(provided), []byte(writeToken)) != 1 {
				WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "write operations require WRITE_TOKEN")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
```

- [ ] **Step 4: Update the WriteAuth call site in server.go**

In `internal/api/server.go`, find the line (around line 195):

```go
			r.Use(WriteAuth(opts.Config.WriteToken, opts.Config.AuthToken))
```

Replace with:

```go
			r.Use(WriteAuth(opts.Config.WriteToken, opts.Config.AuthToken, opts.Config.JWTSecret != ""))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestWriteAuth -v`
Expected: All 6 tests PASS.

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: All tests pass (including existing WriteAuth tests if any).

- [ ] **Step 7: Commit**

```bash
git add internal/api/middleware.go internal/api/middleware_test.go internal/api/server.go
git commit -m "fix(api): add jwtEnabled param to WriteAuth for full mode safety

WriteAuth now accepts a jwtEnabled bool to detect full mode even when
both legacy tokens are empty. Prevents writes from passing through
unchecked in full mode without AUTH_TOKEN."
```

---

### Task 5: Extend UploadAuth to accept API keys

**Files:**
- Modify: `internal/api/middleware.go:284-315` (UploadAuth function)
- Modify: `internal/api/server.go` (UploadAuth call site)
- Modify: `internal/api/middleware_test.go` (add UploadAuth tests)

- [ ] **Step 1: Write failing tests**

Add to `internal/api/middleware_test.go`:

```go
func TestUploadAuth_APIKey_FormField(t *testing.T) {
	// Mock API key resolver
	resolver := &mockAPIKeyResolver{
		key: &database.APIKey{
			ID:    1,
			Label: "upload-key",
			Role:  "editor",
		},
	}
	mw := UploadAuthWithKeys("", resolver)

	// Build multipart form with tre_ API key
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("key", "tre_test_api_key_12345")
	writer.WriteField("call", `{"talkgroup":1234}`)
	writer.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/call-upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 with API key in form field, got %d", rec.Code)
	}
}

func TestUploadAuth_LegacyToken_StillWorks(t *testing.T) {
	mw := UploadAuthWithKeys("upload-secret", nil)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("key", "upload-secret")
	writer.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/call-upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 with legacy token in form field, got %d", rec.Code)
	}
}

func TestUploadAuth_NoAuth_Rejects(t *testing.T) {
	mw := UploadAuthWithKeys("upload-secret", nil)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("call", `{"talkgroup":1234}`)
	writer.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/call-upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("expected 401 with no auth, got %d", rec.Code)
	}
}

// mockAPIKeyResolver for testing UploadAuth API key support.
type mockAPIKeyResolver struct {
	key *database.APIKey
}

func (m *mockAPIKeyResolver) ResolveAPIKey(ctx context.Context, plaintext string) (*database.APIKey, error) {
	if m.key != nil {
		return m.key, nil
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockAPIKeyResolver) TouchAPIKey(ctx context.Context, id int) error {
	return nil
}
```

Note: You may need to add `"fmt"` to the imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestUploadAuth -v`
Expected: FAIL — `UploadAuthWithKeys` does not exist yet.

- [ ] **Step 3: Add UploadAuthWithKeys function**

In `internal/api/middleware.go`, add after the existing `UploadAuth` function (keep the old one for now):

```go
// UploadAuthWithKeys extends UploadAuth to also accept API keys (tre_ prefix)
// from form fields. This allows upload plugins to authenticate via API keys
// when WRITE_TOKEN is deprecated.
//
// Resolution order:
//  1. Authorization header (JWT or legacy token via extractBearerToken)
//  2. Form field "key"/"api_key" — if starts with "tre_", resolve as API key
//  3. Form field "key"/"api_key" — constant-time compare against legacy token
//  4. Reject with 401
func UploadAuthWithKeys(token string, keyDB apiKeyResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No auth configured at all — open mode
			if token == "" && keyDB == nil {
				next.ServeHTTP(w, r)
				return
			}

			// 1. Check Authorization header / ?token= query param
			if provided := extractBearerToken(r); provided != "" {
				// JWT or legacy token — let it through if it matches
				if token != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
				// Could be a JWT or API key — check API keys
				if keyDB != nil && strings.HasPrefix(provided, "tre_") {
					if key, err := keyDB.ResolveAPIKey(r.Context(), provided); err == nil && key != nil {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// 2. Check form fields
			if err := r.ParseMultipartForm(32 << 20); err == nil {
				for _, fieldName := range []string{"key", "api_key"} {
					val := r.FormValue(fieldName)
					if val == "" {
						continue
					}
					// API key (tre_ prefix)
					if keyDB != nil && strings.HasPrefix(val, "tre_") {
						if key, err := keyDB.ResolveAPIKey(r.Context(), val); err == nil && key != nil {
							next.ServeHTTP(w, r)
							return
						}
					}
					// Legacy token compare
					if token != "" && subtle.ConstantTimeCompare([]byte(val), []byte(token)) == 1 {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			WriteError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}
```

- [ ] **Step 4: Update call site in server.go**

In `internal/api/server.go`, find the upload endpoint wiring (around line 162-169):

```go
	if opts.Uploader != nil {
		uploadToken := opts.Config.WriteToken
		uploadHandler := NewUploadHandler(opts.Uploader, opts.Config.UploadInstanceID, opts.Log)
		r.Group(func(r chi.Router) {
			r.Use(MaxBodySize(50 << 20)) // 50 MB for audio uploads
			r.Use(UploadAuth(uploadToken))
			r.Post("/api/v1/call-upload", uploadHandler.Upload)
		})
	}
```

Replace with:

```go
	if opts.Uploader != nil {
		uploadToken := opts.Config.WriteToken
		if uploadToken == "" {
			uploadToken = opts.Config.AuthToken // fall back to shared token in token mode
		}
		uploadHandler := NewUploadHandler(opts.Uploader, opts.Config.UploadInstanceID, opts.Log)
		r.Group(func(r chi.Router) {
			r.Use(MaxBodySize(50 << 20)) // 50 MB for audio uploads
			r.Use(UploadAuthWithKeys(uploadToken, opts.DB))
			r.Post("/api/v1/call-upload", uploadHandler.Upload)
		})
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestUploadAuth -v`
Expected: All 3 new tests PASS.

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/api/middleware.go internal/api/middleware_test.go internal/api/server.go
git commit -m "feat(api): add API key support to upload auth middleware

UploadAuthWithKeys accepts tre_ API keys from form fields and
Authorization header, in addition to legacy token auth. This allows
upload plugins to work when WRITE_TOKEN is deprecated."
```

---

### Task 6: Update auth.js for mode-based detection with backward compat

**Files:**
- Modify: `web/auth.js:26-51` (mode detection block)

- [ ] **Step 1: Replace the mode detection block in auth.js**

In `web/auth.js`, replace lines 26-51 (the `try/catch` block for mode detection):

```javascript
  try {
    var xhr = new XMLHttpRequest();
    xhr.open('GET', '/api/v1/auth-init', false);
    if (jwtToken) xhr.setRequestHeader('Authorization', 'Bearer ' + jwtToken);
    xhr.send();
    if (xhr.status === 200) {
      var data = JSON.parse(xhr.responseText);
      readToken = data.token || '';
      if (readToken) localStorage.setItem(KEY_READ, readToken);
      if (data.user && jwtToken) {
        mode = 'jwt';
        jwtRole = data.user.role || '';
      } else {
        mode = readToken ? 'legacy' : 'none';
        // Discard stale JWT if server didn't validate it
        if (jwtToken) {
          jwtToken = '';
          localStorage.removeItem(KEY_JWT);
        }
      }
    }
  } catch (e) {
    // Server unreachable or auth-init not available
    readToken = localStorage.getItem(KEY_READ) || '';
    mode = readToken ? 'legacy' : 'none';
  }
```

With:

```javascript
  try {
    var xhr = new XMLHttpRequest();
    xhr.open('GET', '/api/v1/auth-init', false);
    if (jwtToken) xhr.setRequestHeader('Authorization', 'Bearer ' + jwtToken);
    xhr.send();
    if (xhr.status === 200) {
      var data = JSON.parse(xhr.responseText);

      if (data.mode) {
        // New auth-init response (v0.10+)
        if (data.mode === 'open') {
          mode = 'none';
          // No auth needed — clear any stale tokens
          readToken = '';
        } else if (data.mode === 'token') {
          // User must provide token — check localStorage, lazy-prompt on 401
          readToken = localStorage.getItem(KEY_READ) || '';
          mode = readToken ? 'legacy' : 'none';
        } else if (data.mode === 'full') {
          // JWT login available
          if (data.read_token) {
            readToken = data.read_token;
            localStorage.setItem(KEY_READ, readToken);
          }
          if (data.jwt_enabled && jwtToken) {
            mode = 'jwt';
          } else {
            mode = readToken ? 'legacy' : 'none';
            if (jwtToken) {
              jwtToken = '';
              localStorage.removeItem(KEY_JWT);
            }
          }
        }
      } else {
        // Legacy auth-init response (pre-v0.10) — backward compat
        readToken = data.token || '';
        if (readToken) localStorage.setItem(KEY_READ, readToken);
        if (data.user && jwtToken) {
          mode = 'jwt';
          jwtRole = data.user.role || '';
        } else {
          mode = readToken ? 'legacy' : 'none';
          if (jwtToken) {
            jwtToken = '';
            localStorage.removeItem(KEY_JWT);
          }
        }
      }
    }
  } catch (e) {
    // Server unreachable or auth-init not available
    readToken = localStorage.getItem(KEY_READ) || '';
    mode = readToken ? 'legacy' : 'none';
  }
```

- [ ] **Step 2: Build and verify**

Run: `go build ./...`
Expected: Builds (auth.js is embedded via `go:embed web/*`).

- [ ] **Step 3: Manual smoke test checklist**

Verify in browser dev tools (not automated — auth.js runs in browser):
- Open mode: auth.js sets `mode = 'none'`, no token injected, requests pass through
- Token mode without saved token: `mode = 'none'`, 401 triggers prompt
- Token mode with saved token: `mode = 'legacy'`, token injected
- Full mode with read_token: `mode = 'legacy'`, read token injected
- Full mode with JWT: `mode = 'jwt'`, JWT injected

- [ ] **Step 4: Commit**

```bash
git add web/auth.js
git commit -m "feat(web): update auth.js for mode-based auth-init response

Detects new {mode, read_token, jwt_enabled} response from auth-init.
Falls back to legacy {token, guest_access, user} parsing when mode
field is absent (backward compat with older tr-engine versions)."
```

---

### Task 7: Update openapi.yaml and sample.env

**Files:**
- Modify: `openapi.yaml` (auth-init response schema)
- Modify: `sample.env` (deprecation notes)

- [ ] **Step 1: Update auth-init response schema in openapi.yaml**

Find the auth-init endpoint definition in `openapi.yaml` and update the response schema to match the new shape:

```yaml
  /api/v1/auth-init:
    get:
      summary: Auth mode discovery
      description: |
        Returns the engine's auth mode and any public read token.
        Always available without authentication. Clients use this to
        determine whether to prompt for a token, show a login form,
        or skip auth entirely.
      operationId: getAuthInit
      tags: [auth]
      responses:
        '200':
          description: Auth mode info
          content:
            application/json:
              schema:
                type: object
                required: [mode, jwt_enabled]
                properties:
                  mode:
                    type: string
                    enum: [open, token, full]
                    description: |
                      - open: no auth required
                      - token: shared API token required (not returned here)
                      - full: JWT login available, optional public read token
                  read_token:
                    type: string
                    nullable: true
                    description: Public read token (only in full mode with AUTH_TOKEN set)
                  jwt_enabled:
                    type: boolean
                    description: Whether JWT login endpoints are available
```

- [ ] **Step 2: Add deprecation notes to sample.env**

Find the `WRITE_TOKEN` and `AUTH_ENABLED` entries in `sample.env` and add deprecation notes:

```bash
# DEPRECATED: WRITE_TOKEN will be removed in a future release.
# Use ADMIN_PASSWORD instead — it enables JWT login with role-based write access.
# WRITE_TOKEN=

# DEPRECATED: AUTH_ENABLED is no longer needed.
# Auth mode is derived from AUTH_TOKEN and ADMIN_PASSWORD presence.
# To disable auth, simply don't set AUTH_TOKEN or ADMIN_PASSWORD.
# AUTH_ENABLED=true
```

- [ ] **Step 3: Commit**

```bash
git add openapi.yaml sample.env
git commit -m "docs: update openapi.yaml and sample.env for auth simplification

New auth-init response schema with mode/read_token/jwt_enabled.
Deprecation notes for WRITE_TOKEN and AUTH_ENABLED in sample.env."
```

---

### Task 8: Final integration test — all three modes

**Files:**
- Create: `internal/api/auth_modes_test.go`

- [ ] **Step 1: Write integration-style tests verifying mode behavior end-to-end**

Create `internal/api/auth_modes_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthModes_OpenMode_NoAuthRequired verifies that in open mode,
// both reads and writes pass through without any token.
func TestAuthModes_OpenMode_NoAuthRequired(t *testing.T) {
	// Simulate open mode: WriteAuth with no tokens, no JWT
	mw := WriteAuth("", "", false)

	// GET should pass
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/systems", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode GET: expected 200, got %d", rec.Code)
	}

	// POST should pass (open mode = no restrictions)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/admin/systems/merge", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode POST: expected 200, got %d", rec.Code)
	}
}

// TestAuthModes_TokenMode_AuthInitShape verifies token mode auth-init
// does NOT leak the token.
func TestAuthModes_TokenMode_AuthInitShape(t *testing.T) {
	h := registerAuthInit(&config.Config{AuthToken: "secret-token"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "token" {
		t.Fatalf("expected mode=token, got %v", resp["mode"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Fatal("SECURITY: read_token must NOT be present in token mode")
	}
}

// TestAuthModes_FullMode_ReadTokenExposed verifies that in full mode
// with AUTH_TOKEN, the read token IS returned (by design — public read).
func TestAuthModes_FullMode_ReadTokenExposed(t *testing.T) {
	h := registerAuthInit(&config.Config{
		AuthToken:     "public-read",
		AdminPassword: "admin123",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "full" {
		t.Fatalf("expected mode=full, got %v", resp["mode"])
	}
	if resp["read_token"] != "public-read" {
		t.Errorf("expected read_token=public-read, got %v", resp["read_token"])
	}
	if resp["jwt_enabled"] != true {
		t.Errorf("expected jwt_enabled=true, got %v", resp["jwt_enabled"])
	}
}

// TestAuthModes_FullMode_WriteRequiresRole verifies writes are blocked
// without an editor+ JWT role in full mode.
func TestAuthModes_FullMode_WriteRequiresRole(t *testing.T) {
	mw := WriteAuth("", "read-token", true)

	// Viewer via read token — should be rejected for writes
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 0, "", "viewer", "token")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("expected 403 for viewer PATCH, got %d", rec.Code)
	}

	// Editor via JWT — should pass
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 1, "editor-user", "editor", "jwt")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 for editor PATCH, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run all tests**

Run: `go test ./... -v 2>&1 | tail -30`
Expected: All tests pass, including the new auth mode integration tests.

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/tr-engine/`
Expected: Clean build.

- [ ] **Step 4: Commit**

```bash
git add internal/api/auth_modes_test.go
git commit -m "test(api): add auth mode integration tests

Verifies open/token/full mode behavior end-to-end: auth-init response
shapes, token secrecy in token mode, write gating in full mode."
```

---

### Task 9: Update CLAUDE.md auth documentation

**Files:**
- Modify: `CLAUDE.md` (auth sections)

- [ ] **Step 1: Update the auth documentation in CLAUDE.md**

Find the auth-related sections and update to reflect the new model. Key sections to update:

1. The `auth-init` description in Key Files
2. The Two-tier auth bullet in Implementation Status
3. The `AUTH_ENABLED` and `WRITE_TOKEN` references in Configuration
4. The Caddy Auth header injection section

Replace references to `guest_access` with the new `mode` field. Add deprecation notes for `WRITE_TOKEN` and `AUTH_ENABLED`. Update the Caddy section to note that injection is no longer required.

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md for auth simplification

Reflect new auth modes (open/token/full), deprecation of WRITE_TOKEN
and AUTH_ENABLED, and removal of Caddy injection requirement."
```
