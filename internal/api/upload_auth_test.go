package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// stubKeys resolves exactly one API key.
type stubKeys struct{ valid string }

func (s stubKeys) ResolveAPIKey(_ context.Context, plaintext string) (*database.APIKey, error) {
	if plaintext == s.valid {
		return &database.APIKey{ID: 1, Role: "viewer", Label: "uploader"}, nil
	}
	return nil, nil
}
func (stubKeys) TouchAPIKey(context.Context, int) error { return nil }

func uploadRequest(field, value string) *http.Request {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if field != "" {
		mw.WriteField(field, value)
	}
	mw.WriteField("system", "1")
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/call-upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func serveUpload(cfg *config.Config, req *http.Request) int {
	var h http.Handler = okHandler
	if mw := uploadAuth(cfg, stubKeys{valid: "tre_good"}); mw != nil {
		h = mw(h)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestUploadAuth_FullMode_PublicReadTokenRejected(t *testing.T) {
	// In full mode AUTH_TOKEN is handed to every visitor by /auth-init.
	cfg := &config.Config{AuthToken: "public-read", AdminPassword: "pw"}
	for _, field := range []string{"key", "api_key"} {
		if code := serveUpload(cfg, uploadRequest(field, "public-read")); code != http.StatusUnauthorized {
			t.Errorf("form field %s with public read token: got %d, want 401", field, code)
		}
	}
	req := uploadRequest("", "")
	req.Header.Set("Authorization", "Bearer public-read")
	if code := serveUpload(cfg, req); code != http.StatusUnauthorized {
		t.Errorf("bearer public read token: got %d, want 401", code)
	}
	if code := serveUpload(cfg, uploadRequest("key", "tre_good")); code != http.StatusOK {
		t.Errorf("API key: got %d, want 200", code)
	}
}

func TestUploadAuth_FullMode_WriteTokenAccepted(t *testing.T) {
	cfg := &config.Config{AuthToken: "public-read", WriteToken: "write-secret", AdminPassword: "pw"}
	if code := serveUpload(cfg, uploadRequest("key", "write-secret")); code != http.StatusOK {
		t.Errorf("WRITE_TOKEN: got %d, want 200", code)
	}
	if code := serveUpload(cfg, uploadRequest("key", "public-read")); code != http.StatusUnauthorized {
		t.Errorf("public read token: got %d, want 401", code)
	}
}

func TestUploadAuth_TokenMode_SharedTokenAccepted(t *testing.T) {
	cfg := &config.Config{AuthToken: "shared-secret"}
	if code := serveUpload(cfg, uploadRequest("key", "shared-secret")); code != http.StatusOK {
		t.Errorf("shared AUTH_TOKEN: got %d, want 200", code)
	}
	if code := serveUpload(cfg, uploadRequest("key", "wrong")); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
}

func TestUploadAuth_OpenMode_NoCredentialNeeded(t *testing.T) {
	// docs/http-upload.md: open mode needs no credential. Previously every
	// upload was rejected because the key resolver was always present.
	cfg := &config.Config{}
	if code := serveUpload(cfg, uploadRequest("", "")); code != http.StatusOK {
		t.Errorf("open mode upload: got %d, want 200", code)
	}
}
