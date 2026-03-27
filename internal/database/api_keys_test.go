package database

import (
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error: %v", err)
	}

	if !strings.HasPrefix(plaintext, "tre_") {
		t.Errorf("plaintext must start with 'tre_', got %q", plaintext)
	}

	if len(prefix) != 12 {
		t.Errorf("prefix length = %d, want 12", len(prefix))
	}

	if !strings.HasPrefix(prefix, "tre_") {
		t.Errorf("prefix must start with 'tre_', got %q", prefix)
	}

	// SHA-256 produces 32 bytes = 64 hex chars
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}

	// Hash must be the SHA-256 of the plaintext
	if HashAPIKey(plaintext) != hash {
		t.Error("hash does not match HashAPIKey(plaintext)")
	}

	// Two calls must produce different keys (entropy check)
	p2, h2, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("second GenerateAPIKey() error: %v", err)
	}
	if plaintext == p2 {
		t.Error("two GenerateAPIKey calls returned identical plaintext")
	}
	if hash == h2 {
		t.Error("two GenerateAPIKey calls returned identical hash")
	}
}

func TestHashAPIKey(t *testing.T) {
	key := "tre_abc123"

	h1 := HashAPIKey(key)
	h2 := HashAPIKey(key)

	// Deterministic
	if h1 != h2 {
		t.Error("HashAPIKey is not deterministic")
	}

	// SHA-256 = 64 hex chars
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64", len(h1))
	}

	// Different inputs produce different hashes
	h3 := HashAPIKey("tre_different")
	if h1 == h3 {
		t.Error("different inputs produced the same hash")
	}
}

