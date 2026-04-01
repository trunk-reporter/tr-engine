package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/snarg/tr-engine/internal/config"
)

func TestAuthModes_OpenMode_NoAuthRequired(t *testing.T) {
	mw := WriteAuth("", "", false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/systems", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode GET: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/admin/systems/merge", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode POST: expected 200, got %d", rec.Code)
	}
}

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

func TestAuthModes_FullMode_WriteRequiresRole(t *testing.T) {
	mw := WriteAuth("", "read-token", true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 0, "", "viewer", "token")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("expected 403 for viewer PATCH, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/api/v1/talkgroups/1", nil)
	req = setAuthContext(req, 1, "editor-user", "editor", "jwt")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 for editor PATCH, got %d", rec.Code)
	}
}
