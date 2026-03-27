package database

import "testing"

func TestNormalizeUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Admin@Example.COM", "admin@example.com"},
		{"  alice@example.com  ", "alice@example.com"},
		{"ALICE@EXAMPLE.COM", "alice@example.com"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := NormalizeUsername(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	valid := []string{
		"admin@example.com",
		"user.name+tag@domain.co.uk",
		"a@b.io",
	}
	for _, u := range valid {
		if !ValidateUsername(u) {
			t.Errorf("ValidateUsername(%q) = false, want true", u)
		}
	}

	invalid := []string{
		"",
		"notanemail",
		"@nodomain.com",
		"nodot@domain",
		"no@.com",
		"no@com.",
		"ab", // too short (< 3 chars, but let's check)
	}
	for _, u := range invalid {
		if ValidateUsername(u) {
			t.Errorf("ValidateUsername(%q) = true, want false", u)
		}
	}
}
