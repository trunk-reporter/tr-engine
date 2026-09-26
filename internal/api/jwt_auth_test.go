package api

// Tests for middleware and helpers NOT covered in middleware_test.go:
//   - RoleLevel
//   - AuthRateLimiter (login-specific per-IP limiter)
//   - jwtKeyFunc (algorithm enforcement)

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// ── AuthRateLimiter ───────────────────────────────────────────────────────────

func TestAuthRateLimiter(t *testing.T) {
	t.Run("allows_initial_burst_of_5", func(t *testing.T) {
		handler := AuthRateLimiter(nil)(okHandler)
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.RemoteAddr = "1.2.3.4:1234"
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: want 200, got %d", i+1, rec.Code)
			}
		}
	})

	t.Run("blocks_after_burst_exhausted", func(t *testing.T) {
		handler := AuthRateLimiter(nil)(okHandler)
		ip := "9.8.7.6:1234"
		// Exhaust the 5-request burst
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.RemoteAddr = ip
			handler.ServeHTTP(rec, req)
		}
		// 6th request must be rate-limited
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/auth/login", nil)
		req.RemoteAddr = ip
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("6th request: want 429, got %d", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("missing Retry-After header on 429")
		}
	})

	t.Run("different_ips_are_independent", func(t *testing.T) {
		handler := AuthRateLimiter(nil)(okHandler)
		// Exhaust IP A
		for i := 0; i < 6; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.RemoteAddr = "11.11.11.11:1234"
			handler.ServeHTTP(rec, req)
		}
		// IP B should still be allowed
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/auth/login", nil)
		req.RemoteAddr = "22.22.22.22:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("different IP: want 200, got %d", rec.Code)
		}
	})

	t.Run("spoofed_forwarded_for_does_not_reset_limit", func(t *testing.T) {
		// A client talking to the engine directly must not be able to dodge the
		// limiter by inventing a new X-Forwarded-For value on every attempt.
		proxies, _ := ParseTrustedProxies("loopback,private")
		handler := AuthRateLimiter(proxies)(okHandler)
		allowed := 0
		for i := 0; i < 20; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.RemoteAddr = "203.0.113.50:1234"
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
			handler.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				allowed++
			}
		}
		if allowed != 5 {
			t.Errorf("allowed %d of 20 attempts with rotating X-Forwarded-For, want 5", allowed)
		}
	})
}

// ── jwtKeyFunc ────────────────────────────────────────────────────────────────

func TestJWTKeyFunc(t *testing.T) {
	secret := []byte("my-secret")

	t.Run("hs256_returns_secret", func(t *testing.T) {
		tok := jwt.New(jwt.SigningMethodHS256)
		key, err := jwtKeyFunc(secret)(tok)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := key.([]byte)
		if !ok {
			t.Fatalf("expected []byte, got %T", key)
		}
		if string(got) != string(secret) {
			t.Errorf("key = %q, want %q", got, secret)
		}
	})

	t.Run("hs512_rejected_to_prevent_algorithm_confusion", func(t *testing.T) {
		tok := jwt.New(jwt.SigningMethodHS512)
		_, err := jwtKeyFunc(secret)(tok)
		if err == nil {
			t.Error("expected error for non-HS256 algorithm, got nil")
		}
	})

	t.Run("rs256_rejected", func(t *testing.T) {
		tok := jwt.New(jwt.SigningMethodRS256)
		_, err := jwtKeyFunc(secret)(tok)
		if err == nil {
			t.Error("expected error for RS256 algorithm, got nil")
		}
	})
}
