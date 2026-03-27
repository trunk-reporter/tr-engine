package api

// Tests for auth/users/keys handler behavior NOT covered elsewhere:
//   - AuthHandler.Me with no user in context
//   - generateAccessToken / generateRefreshToken claim correctness

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
)

// ── AuthHandler.Me ────────────────────────────────────────────────────────────

func TestAuthHandlerMe_NotAuthenticated(t *testing.T) {
	h := NewAuthHandler(nil, []byte("secret"), zerolog.Nop())
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	rec := httptest.NewRecorder()
	h.Me(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated me: want 401, got %d", rec.Code)
	}
}

// ── Token generation ──────────────────────────────────────────────────────────

func TestGenerateAccessToken_ClaimsAreCorrect(t *testing.T) {
	secret := []byte("test-secret")
	h := NewAuthHandler(nil, secret, zerolog.Nop())
	user := &database.User{
		ID:       99,
		Username: "alice@example.com",
		Role:     "editor",
		Enabled:  true,
	}

	signed, err := h.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken: %v", err)
	}

	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(signed, claims, jwtKeyFunc(secret))
	if err != nil || !tok.Valid {
		t.Fatalf("parse token: %v", err)
	}

	if claims.Subject != "99" {
		t.Errorf("subject = %q, want %q", claims.Subject, "99")
	}
	if claims.Username != "alice@example.com" {
		t.Errorf("username = %q, want alice@example.com", claims.Username)
	}
	if claims.Role != "editor" {
		t.Errorf("role = %q, want editor", claims.Role)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("expiry not set")
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 || ttl > accessTokenExpiry {
		t.Errorf("expiry ttl = %v, expected in (0, %v]", ttl, accessTokenExpiry)
	}
}

func TestGenerateRefreshToken_ClaimsAreCorrect(t *testing.T) {
	secret := []byte("test-secret")
	h := NewAuthHandler(nil, secret, zerolog.Nop())

	signed, err := h.generateRefreshToken(42)
	if err != nil {
		t.Fatalf("generateRefreshToken: %v", err)
	}

	claims := &RefreshClaims{}
	tok, err := jwt.ParseWithClaims(signed, claims, jwtKeyFunc(secret))
	if err != nil || !tok.Valid {
		t.Fatalf("parse token: %v", err)
	}

	if claims.Subject != "42" {
		t.Errorf("subject = %q, want %q", claims.Subject, "42")
	}
	if claims.Type != "refresh" {
		t.Errorf("type = %q, want refresh", claims.Type)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("expiry not set")
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 || ttl > refreshTokenExpiry {
		t.Errorf("expiry ttl = %v, expected in (0, %v]", ttl, refreshTokenExpiry)
	}
}
