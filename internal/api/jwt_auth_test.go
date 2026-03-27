package api

// Tests for middleware and helpers NOT covered in middleware_test.go:
//   - RoleLevel
//   - clientIP (header priority + RemoteAddr fallback)
//   - AuthRateLimiter (login-specific per-IP limiter)
//   - jwtKeyFunc (algorithm enforcement)

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// ── clientIP ──────────────────────────────────────────────────────────────────

func TestClientIP(t *testing.T) {
	t.Run("x_forwarded_for_single", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		if got := clientIP(req); got != "1.2.3.4" {
			t.Errorf("got %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("x_forwarded_for_chain_takes_leftmost", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-For", "10.0.0.1, 172.16.0.1, 192.168.0.1")
		// Leftmost = original client address
		if got := clientIP(req); got != "10.0.0.1" {
			t.Errorf("got %q, want leftmost client %q", got, "10.0.0.1")
		}
	})

	t.Run("x_real_ip_used_when_no_xff", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Real-IP", "5.5.5.5")
		if got := clientIP(req); got != "5.5.5.5" {
			t.Errorf("got %q, want %q", got, "5.5.5.5")
		}
	})

	t.Run("xff_takes_precedence_over_x_real_ip", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		req.Header.Set("X-Real-IP", "9.9.9.9")
		if got := clientIP(req); got != "1.2.3.4" {
			t.Errorf("got %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("remote_addr_fallback_strips_port", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "203.0.113.1:12345"
		if got := clientIP(req); got != "203.0.113.1" {
			t.Errorf("got %q, want %q", got, "203.0.113.1")
		}
	})
}

// ── AuthRateLimiter ───────────────────────────────────────────────────────────

func TestAuthRateLimiter(t *testing.T) {
	t.Run("allows_initial_burst_of_5", func(t *testing.T) {
		handler := AuthRateLimiter()(okHandler)
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
		handler := AuthRateLimiter()(okHandler)
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
		handler := AuthRateLimiter()(okHandler)
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
