package database

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/snarg/tr-engine/internal/auth"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error: %v", err)
	}

	if !strings.HasPrefix(plaintext, "tre_") {
		t.Errorf("plaintext must start with 'tre_', got %q", plaintext)
	}
	if len(plaintext) != 4+64 {
		t.Errorf("plaintext length = %d, want 68", len(plaintext))
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
	if len(h1) != 64 || !validSHA256Hex(h1) {
		t.Errorf("hash %q is not 64 lowercase hex characters", h1)
	}

	// Different inputs produce different hashes
	h3 := HashAPIKey("tre_different")
	if h1 == h3 {
		t.Error("different inputs produced the same hash")
	}
}

// A legacy key's prefix comes from its hash, never from the secret.
func TestLegacyPrefix(t *testing.T) {
	secret := "abcdef0123456789-legacy"
	hash := HashAPIKey(secret)
	p := legacyPrefix(hash)
	if p != "legacy_"+hash[:6] {
		t.Errorf("legacyPrefix = %q, want legacy_ + %q", p, hash[:6])
	}
	if strings.Contains(p, secret[:3]) {
		t.Errorf("prefix %q contains characters of the secret", p)
	}
}

func TestCheckLegacySecret(t *testing.T) {
	cases := []struct {
		secret   string
		weak     bool
		wantErr  error
		anyError bool
	}{
		{secret: "0123456789abcdef", weak: false},
		{secret: "0123456789abcde", weak: true},
		{secret: "short", weak: true},
		{secret: "ééééééééééééééé", weak: true}, // 15 characters, 30 bytes
		{secret: "$(openssl rand -hex 32)", wantErr: ErrLegacySecretUnexpanded},
		{secret: "prefix-$(cat /x)-suffix-long-enough", wantErr: ErrLegacySecretUnexpanded},
		{secret: "$HOME-is-fine-here-long", weak: false},
		{secret: "", anyError: true},
	}
	for _, c := range cases {
		weak, err := CheckLegacySecret(c.secret)
		switch {
		case c.wantErr != nil:
			if !errors.Is(err, c.wantErr) {
				t.Errorf("CheckLegacySecret(%q) err = %v, want %v", c.secret, err, c.wantErr)
			}
		case c.anyError:
			if err == nil {
				t.Errorf("CheckLegacySecret(%q) = nil error, want one", c.secret)
			}
		default:
			if err != nil || weak != c.weak {
				t.Errorf("CheckLegacySecret(%q) = (%v, %v), want (%v, nil)", c.secret, weak, err, c.weak)
			}
		}
	}
}

func TestNormalizeAPIKeyName(t *testing.T) {
	ok := map[string]string{
		"tr-dashboard at home":     "tr-dashboard at home",
		"  padded  ":               "padded",
		strings.Repeat("é", 100):   strings.Repeat("é", 100),
		"trunk-recorder butco (1)": "trunk-recorder butco (1)",
	}
	for in, want := range ok {
		got, err := NormalizeAPIKeyName(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAPIKeyName(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", strings.Repeat("x", 101), "line\nbreak", "bidi‮override", "nul\x00"} {
		_, err := NormalizeAPIKeyName(in)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "name" {
			t.Errorf("NormalizeAPIKeyName(%q) err = %v, want a name FieldError", in, err)
		}
	}
}

func TestAPIKeyStatusAt(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	cases := []struct {
		k    APIKey
		want KeyStatus
	}{
		{APIKey{}, KeyActive},
		{APIKey{ExpiresAt: &future}, KeyActive},
		{APIKey{ExpiresAt: &now}, KeyExpired},
		{APIKey{ExpiresAt: &past}, KeyExpired},
		{APIKey{RevokedAt: &past}, KeyRevoked},
		{APIKey{RevokedAt: &past, ExpiresAt: &past}, KeyRevoked},
	}
	for i, c := range cases {
		if got := c.k.StatusAt(now); got != c.want {
			t.Errorf("case %d: StatusAt = %q, want %q", i, got, c.want)
		}
		if c.k.ActiveAt(now) != (c.want == KeyActive) {
			t.Errorf("case %d: ActiveAt disagrees with StatusAt", i)
		}
	}
}

func TestValidateKeyFields(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	rps := func(v float32) *float32 { return &v }
	listen := auth.Scopes{auth.ScopeListen}
	restr := &auth.Restriction{Systems: []int{2, 1, 2}}

	scopes, r, err := validateKeyFields(auth.Scopes{auth.ScopeUpload, auth.ScopeAdmin}, nil, &future, rps(5), now)
	if err != nil || strings.Join(scopes.Strings(), ",") != "admin,upload" || r != nil {
		t.Fatalf("valid admin key: got (%v, %v, %v)", scopes, r, err)
	}
	_, r, err = validateKeyFields(listen, restr, nil, nil, now)
	if err != nil || len(r.Systems) != 2 || r.Systems[0] != 1 {
		t.Fatalf("restricted listen key: got (%v, %v), want normalized systems [1 2]", r, err)
	}

	bad := []struct {
		field  string
		scopes auth.Scopes
		r      *auth.Restriction
		exp    *time.Time
		rps    *float32
	}{
		{"scopes", nil, nil, nil, nil},
		{"scopes", auth.Scopes{auth.ScopeListen, auth.ScopeEdit}, nil, nil, nil},
		{"scopes", auth.Scopes{"root"}, nil, nil, nil},
		{"restriction", auth.Scopes{auth.ScopeEdit}, restr, nil, nil},
		{"restriction", auth.Scopes{auth.ScopeListen, auth.ScopeUpload}, restr, nil, nil},
		{"restriction", listen, &auth.Restriction{}, nil, nil}, // allows nothing
		{"expires_at", listen, nil, &past, nil},
		{"expires_at", listen, nil, &now, nil},
		{"rate_limit_rps", listen, nil, nil, rps(0)},
		{"rate_limit_rps", listen, nil, nil, rps(-1)},
		{"rate_limit_rps", listen, nil, nil, rps(float32(math.NaN()))},
		{"rate_limit_rps", listen, nil, nil, rps(float32(math.Inf(1)))},
	}
	for i, b := range bad {
		_, _, err := validateKeyFields(b.scopes, b.r, b.exp, b.rps, now)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != b.field {
			t.Errorf("case %d: err = %v, want a %s FieldError", i, err, b.field)
		}
	}
}

func TestDeriveLegacyConfig(t *testing.T) {
	cases := []struct {
		name  string
		in    LegacyAuthInput
		mode  LegacyMode
		auth  string
		write string
		flags string // d = auth disabled, i = AUTH_ENABLED invalid, j = JWT_SECRET dropped
	}{
		{"nothing set", LegacyAuthInput{}, LegacyModeOpen, "", "", ""},
		{"write only", LegacyAuthInput{WriteToken: "w"}, LegacyModeWriteTokenOnly, "", "w", ""},
		{"token", LegacyAuthInput{AuthToken: "a"}, LegacyModeToken, "a", "", ""},
		{"token + write", LegacyAuthInput{AuthToken: "a", WriteToken: "w"}, LegacyModeToken, "a", "w", ""},
		{"full", LegacyAuthInput{AdminPassword: "p"}, LegacyModeFull, "", "", ""},
		{"full + tokens", LegacyAuthInput{AdminPassword: "p", AuthToken: "a", WriteToken: "w", JWTSecret: "s"}, LegacyModeFull, "a", "w", ""},
		{"jwt without password", LegacyAuthInput{JWTSecret: "s"}, LegacyModeOpen, "", "", "j"},
		{"jwt without password, token", LegacyAuthInput{JWTSecret: "s", AuthToken: "a"}, LegacyModeToken, "a", "", "j"},
		{"auth disabled", LegacyAuthInput{AuthEnabled: "false", AdminPassword: "p", AuthToken: "a", WriteToken: "w", JWTSecret: "s"}, LegacyModeOpen, "", "", "d"},
		{"auth disabled, 0", LegacyAuthInput{AuthEnabled: "0", WriteToken: "w"}, LegacyModeOpen, "", "", "d"},
		{"auth enabled explicitly", LegacyAuthInput{AuthEnabled: "TRUE", AuthToken: "a"}, LegacyModeToken, "a", "", ""},
		{"auth enabled invalid", LegacyAuthInput{AuthEnabled: "nope", AuthToken: "a"}, LegacyModeToken, "a", "", "i"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveLegacyConfig(c.in)
			flags := ""
			if got.authDisabled {
				flags += "d"
			}
			if got.authEnabledInvalid {
				flags += "i"
			}
			if got.jwtSecretDropped {
				flags += "j"
			}
			if got.mode != c.mode || got.authToken != c.auth || got.writeToken != c.write || flags != c.flags {
				t.Errorf("got mode=%s auth=%q write=%q flags=%q; want mode=%s auth=%q write=%q flags=%q",
					got.mode, got.authToken, got.writeToken, flags, c.mode, c.auth, c.write, c.flags)
			}
		})
	}
}

func TestFormatRemovedUserAccounts(t *testing.T) {
	if got := formatRemovedUserAccounts(nil); got != "" {
		t.Errorf("no users: got %q, want empty", got)
	}
	got := formatRemovedUserAccounts([]RemovedUserAccount{
		{Username: "alice", Role: "admin", Enabled: true},
		{Username: "bob", Role: "editor", Enabled: true},
		{Username: "carol", Role: "viewer", Enabled: false},
	})
	want := "removed 3 user accounts: alice (admin), bob (editor), carol (viewer, disabled) — give each person or their client an API key"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := formatRemovedUserAccounts([]RemovedUserAccount{{Username: "a", Role: "admin", Enabled: true}}); !strings.HasPrefix(got, "removed 1 user account: ") {
		t.Errorf("one user: got %q", got)
	}
}
