package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/snarg/tr-engine/internal/database"
)

// okHandler is a trivial handler that writes 200 OK.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestRequestID(t *testing.T) {
	t.Run("generates_id_when_missing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		RequestID(okHandler).ServeHTTP(rec, req)
		id := rec.Header().Get("X-Request-ID")
		if len(id) != 16 {
			t.Errorf("expected 16-char hex ID, got %q (len %d)", id, len(id))
		}
	})

	t.Run("preserves_provided_id", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Request-ID", "my-custom-id")
		RequestID(okHandler).ServeHTTP(rec, req)
		id := rec.Header().Get("X-Request-ID")
		if id != "my-custom-id" {
			t.Errorf("expected preserved ID %q, got %q", "my-custom-id", id)
		}
	})
}

func TestCORSWithOrigins(t *testing.T) {
	t.Run("empty_origins_allows_all", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		CORSWithOrigins(nil)(okHandler).ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Error("missing Access-Control-Allow-Origin: *")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("allowed_origin", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Origin", "https://example.com")
		CORSWithOrigins([]string{"https://example.com"})(okHandler).ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
			t.Error("expected origin echo")
		}
		if rec.Header().Get("Vary") != "Origin" {
			t.Error("expected Vary: Origin")
		}
	})

	t.Run("disallowed_origin_no_cors_headers", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Origin", "https://evil.com")
		CORSWithOrigins([]string{"https://example.com"})(okHandler).ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("should not set CORS header for disallowed origin")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("request should still be served, got %d", rec.Code)
		}
	})

	t.Run("disallowed_origin_options_returns_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/", nil)
		req.Header.Set("Origin", "https://evil.com")
		CORSWithOrigins([]string{"https://example.com"})(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("options_preflight_returns_204", func(t *testing.T) {
		called := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/", nil)
		CORSWithOrigins(nil)(inner).ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("expected 204, got %d", rec.Code)
		}
		if called {
			t.Error("inner handler should not be called on OPTIONS preflight")
		}
	})
}

func TestRateLimiter(t *testing.T) {
	t.Run("allows_normal_traffic", func(t *testing.T) {
		handler := RateLimiter(100, 100)(okHandler)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("blocks_excess_traffic", func(t *testing.T) {
		// 1 req/s, burst of 2 — third request should be blocked
		handler := RateLimiter(1, 2)(okHandler)
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "5.6.7.8:1234"
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: expected 200, got %d", i, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "5.6.7.8:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("expected 429, got %d", rec.Code)
		}
		if rec.Header().Get("Retry-After") != "1" {
			t.Error("expected Retry-After header")
		}
	})

	t.Run("different_ips_independent", func(t *testing.T) {
		handler := RateLimiter(1, 1)(okHandler)
		// Exhaust IP A
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		handler.ServeHTTP(rec, req)

		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("GET", "/", nil)
		req2.RemoteAddr = "10.0.0.1:1234"
		handler.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusTooManyRequests {
			t.Errorf("IP A second request: expected 429, got %d", rec2.Code)
		}

		// IP B should still work
		rec3 := httptest.NewRecorder()
		req3 := httptest.NewRequest("GET", "/", nil)
		req3.RemoteAddr = "10.0.0.2:1234"
		handler.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusOK {
			t.Errorf("IP B first request: expected 200, got %d", rec3.Code)
		}
	})
}

func TestBearerAuth(t *testing.T) {
	t.Run("empty_token_passes_all", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		BearerAuth("")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("valid_bearer_header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer secret123")
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("invalid_bearer_header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("missing_auth", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("query_param_fallback", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/?token=secret123", nil)
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("invalid_query_param", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/?token=wrong", nil)
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("non_bearer_prefix", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Basic c2VjcmV0")
		BearerAuth("secret123")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})
}

func TestUploadAuth(t *testing.T) {
	token := "test-secret-token"

	t.Run("bearer_header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("query_param_token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload?token="+token, nil)
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("no_auth", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", nil)
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong_bearer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("form_field_key", func(t *testing.T) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		writer.WriteField("key", token)
		writer.WriteField("talkgroup", "9044")
		writer.Close()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (rdio-scanner key field)", rec.Code)
		}
	})

	t.Run("form_field_api_key", func(t *testing.T) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		writer.WriteField("api_key", token)
		writer.WriteField("talkgroup_num", "9044")
		writer.Close()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (OpenMHz api_key field)", rec.Code)
		}
	})

	t.Run("wrong_form_field_key", func(t *testing.T) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		writer.WriteField("key", "wrong-token")
		writer.Close()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		UploadAuth(token)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("empty_token_passes_all", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/call-upload", nil)
		UploadAuth("")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}


// ── JWTOrTokenAuth ────────────────────────────────────────────────────────

// mockAPIKeyDB implements apiKeyResolver for testing.
type mockAPIKeyDB struct {
	key *database.APIKey
	err error
}

func (m *mockAPIKeyDB) ResolveAPIKey(_ context.Context, _ string) (*database.APIKey, error) {
	return m.key, m.err
}

func (m *mockAPIKeyDB) TouchAPIKey(_ context.Context, _ int) error { return nil }

// makeSignedJWT creates a signed HS256 JWT for testing.
func makeSignedJWT(t *testing.T, secret []byte, userID int, username, role string, expiry time.Duration) string {
	t.Helper()
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.Itoa(userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
		},
		Username: username,
		Role:     role,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return s
}

func TestJWTOrTokenAuth(t *testing.T) {
	secret := []byte("test-secret-key")
	writeToken := "write-tok"
	authToken := "read-tok"

	// captureRole records the role set in context by the middleware.
	captureRole := func(got *string, gotUserID *int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*got = ContextRole(r)
			*gotUserID = ContextUserID(r)
			w.WriteHeader(http.StatusOK)
		})
	}

	t.Run("no_auth_configured_passes_through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		JWTOrTokenAuth(nil, "", "")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("missing_token_with_auth_configured_returns_401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		JWTOrTokenAuth(secret, writeToken, authToken)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("valid_jwt_sets_context", func(t *testing.T) {
		tok := makeSignedJWT(t, secret, 42, "alice@example.com", "admin", time.Hour)
		var gotRole string
		var gotUserID int
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		JWTOrTokenAuth(secret, "", "")(captureRole(&gotRole, &gotUserID)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if gotRole != "admin" {
			t.Errorf("expected role=admin, got %q", gotRole)
		}
		if gotUserID != 42 {
			t.Errorf("expected userID=42, got %d", gotUserID)
		}
	})

	t.Run("expired_jwt_returns_401", func(t *testing.T) {
		tok := makeSignedJWT(t, secret, 1, "bob@example.com", "viewer", -time.Minute)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		JWTOrTokenAuth(secret, "", "")(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for expired token, got %d", rec.Code)
		}
	})

	t.Run("alg_none_attack_rejected", func(t *testing.T) {
		// Craft a JWT with alg:none — no signature required. Without the algorithm
		// check in jwtKeyFunc, this token would grant admin access to anyone.
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(
			`{"sub":"1","username":"attacker","role":"admin","iat":9999999999,"exp":9999999999}`))
		forgedToken := header + "." + payload + "."

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+forgedToken)
		JWTOrTokenAuth(secret, writeToken, authToken)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("alg:none token must be rejected, got %d", rec.Code)
		}
	})

	t.Run("wrong_secret_jwt_rejected", func(t *testing.T) {
		tok := makeSignedJWT(t, []byte("different-secret"), 1, "eve@example.com", "admin", time.Hour)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		JWTOrTokenAuth(secret, writeToken, authToken)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for wrong-secret token, got %d", rec.Code)
		}
	})

	t.Run("api_key_resolved_sets_context", func(t *testing.T) {
		uid := 7
		mockDB := &mockAPIKeyDB{key: &database.APIKey{ID: 1, Role: "editor", Label: "my-key", UserID: &uid}}
		var gotRole string
		var gotUserID int
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer tre_abc123definitelynotvalid")
		JWTOrTokenAuth(secret, "", "", mockDB)(captureRole(&gotRole, &gotUserID)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if gotRole != "editor" {
			t.Errorf("expected role=editor, got %q", gotRole)
		}
		if gotUserID != 7 {
			t.Errorf("expected userID=7, got %d", gotUserID)
		}
	})

	t.Run("api_key_not_found_falls_through_to_legacy", func(t *testing.T) {
		mockDB := &mockAPIKeyDB{key: nil}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer tre_doesnotexist")
		JWTOrTokenAuth(secret, writeToken, authToken, mockDB)(okHandler).ServeHTTP(rec, req)
		// "tre_doesnotexist" doesn't match write or auth token either → 401
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("legacy_write_token_grants_admin", func(t *testing.T) {
		var gotRole string
		var gotUserID int
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+writeToken)
		JWTOrTokenAuth(secret, writeToken, authToken)(captureRole(&gotRole, &gotUserID)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if gotRole != "admin" {
			t.Errorf("expected role=admin, got %q", gotRole)
		}
	})

	t.Run("legacy_auth_token_grants_viewer", func(t *testing.T) {
		var gotRole string
		var gotUserID int
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+authToken)
		JWTOrTokenAuth(secret, writeToken, authToken)(captureRole(&gotRole, &gotUserID)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if gotRole != "viewer" {
			t.Errorf("expected role=viewer, got %q", gotRole)
		}
	})

	t.Run("invalid_token_returns_401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer garbage-token")
		JWTOrTokenAuth(secret, writeToken, authToken)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})
}

// ── AdminOnly ─────────────────────────────────────────────────────────────

func TestAdminOnly(t *testing.T) {
	cases := []struct {
		role   string
		wantOK bool
	}{
		{"admin", true},
		{"editor", false},
		{"viewer", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("role_"+tc.role, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req = setAuthContext(req, 1, "u", tc.role, "jwt")
			AdminOnly(okHandler).ServeHTTP(rec, req)
			if tc.wantOK && rec.Code != http.StatusOK {
				t.Errorf("expected 200 for role=%q, got %d", tc.role, rec.Code)
			}
			if !tc.wantOK && rec.Code != http.StatusForbidden {
				t.Errorf("expected 403 for role=%q, got %d", tc.role, rec.Code)
			}
		})
	}
}

// ── EditorOrAbove ─────────────────────────────────────────────────────────

func TestEditorOrAbove(t *testing.T) {
	cases := []struct {
		role   string
		wantOK bool
	}{
		{"admin", true},
		{"editor", true},
		{"viewer", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("role_"+tc.role, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req = setAuthContext(req, 1, "u", tc.role, "jwt")
			EditorOrAbove(okHandler).ServeHTTP(rec, req)
			if tc.wantOK && rec.Code != http.StatusOK {
				t.Errorf("expected 200 for role=%q, got %d", tc.role, rec.Code)
			}
			if !tc.wantOK && rec.Code != http.StatusForbidden {
				t.Errorf("expected 403 for role=%q, got %d", tc.role, rec.Code)
			}
		})
	}
}

// ── WriteAuth ─────────────────────────────────────────────────────────────

func TestWriteAuth(t *testing.T) {
	writeToken := "wt"
	authToken := "at"

	t.Run("no_auth_configured_passes_all", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		WriteAuth("", "", false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("GET_always_passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for GET, got %d", rec.Code)
		}
	})

	t.Run("POST_with_editor_role_passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req = setAuthContext(req, 1, "u", "editor", "jwt")
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for editor, got %d", rec.Code)
		}
	})

	t.Run("POST_with_admin_role_passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req = setAuthContext(req, 1, "u", "admin", "jwt")
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for admin, got %d", rec.Code)
		}
	})

	t.Run("POST_with_viewer_role_returns_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req = setAuthContext(req, 1, "u", "viewer", "jwt")
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 for viewer, got %d", rec.Code)
		}
	})

	t.Run("POST_no_role_matching_write_token_passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+writeToken)
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 with write token, got %d", rec.Code)
		}
	})

	t.Run("POST_no_role_wrong_token_returns_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		WriteAuth(writeToken, authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 with wrong token, got %d", rec.Code)
		}
	})

	t.Run("POST_no_role_no_write_token_configured_returns_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+authToken)
		WriteAuth("", authToken, false)(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 when write token not set, got %d", rec.Code)
		}
	})
}

func TestRecoverer(t *testing.T) {
	t.Run("normal_request_passes_through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		Recoverer(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("panic_produces_500_json", func(t *testing.T) {
		panicker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("test panic")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		Recoverer(panicker).ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if body["error"] != "internal server error" {
			t.Errorf("expected error message, got %v", body)
		}
	})
}

func TestWriteAuth_OpenMode(t *testing.T) {
	mw := WriteAuth("", "", false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("open mode POST should pass, got %d", rec.Code)
	}
}

func TestWriteAuth_FullMode_NoTokens_RequiresJWTRole(t *testing.T) {
	mw := WriteAuth("", "", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/talkgroups/1", nil)
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
	req = setAuthContext(req, 0, "", "admin", "token")
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 with deprecated write token, got %d", rec.Code)
	}
}
