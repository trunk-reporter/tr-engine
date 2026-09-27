package auth

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseScopes(t *testing.T) {
	tests := []struct {
		in      []string
		want    Scopes
		wantErr string // substring; "" = valid
	}{
		{[]string{"listen"}, Scopes{ScopeListen}, ""},
		{[]string{"edit"}, Scopes{ScopeEdit}, ""},
		{[]string{"admin"}, Scopes{ScopeAdmin}, ""},
		{[]string{"upload"}, Scopes{ScopeUpload}, ""},
		{[]string{"upload", "admin"}, Scopes{ScopeAdmin, ScopeUpload}, ""},
		{[]string{"upload", "listen"}, Scopes{ScopeListen, ScopeUpload}, ""},
		{[]string{"edit", "upload"}, Scopes{ScopeEdit, ScopeUpload}, ""},

		{nil, nil, "must not be empty"},
		{[]string{}, nil, "must not be empty"},
		{[]string{"listen", "edit"}, nil, "cannot be combined"},
		{[]string{"admin", "listen"}, nil, "cannot be combined"},
		{[]string{"admin", "edit", "upload"}, nil, "cannot be combined"},
		{[]string{"read"}, nil, `unknown scope "read"`},
		{[]string{"Listen"}, nil, "unknown scope"},
		{[]string{""}, nil, "unknown scope"},
		{[]string{"listen", "viewer"}, nil, "unknown scope"},
		{[]string{"listen", "listen"}, nil, "duplicate scope"},
		{[]string{"upload", "upload"}, nil, "duplicate scope"},
	}
	for _, tt := range tests {
		got, err := ParseScopes(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseScopes(%q) error = %v, want containing %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScopes(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseScopes(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScopeImplies(t *testing.T) {
	all := []Scope{ScopeListen, ScopeEdit, ScopeAdmin, ScopeUpload, "", "bogus"}
	want := map[Scope][]Scope{
		ScopeListen: {ScopeListen},
		ScopeEdit:   {ScopeListen, ScopeEdit},
		ScopeAdmin:  {ScopeListen, ScopeEdit, ScopeAdmin},
		ScopeUpload: {ScopeUpload},
		"":          nil,
		"bogus":     nil,
	}
	for _, s := range all {
		for _, u := range all {
			exp := false
			for _, w := range want[s] {
				if w == u {
					exp = true
				}
			}
			if got := s.Implies(u); got != exp {
				t.Errorf("%q.Implies(%q) = %v, want %v", s, u, got, exp)
			}
		}
	}
}

func TestScopesHas(t *testing.T) {
	tests := []struct {
		s    Scopes
		want map[Scope]bool
	}{
		{nil, map[Scope]bool{}},
		{Scopes{ScopeListen}, map[Scope]bool{ScopeListen: true}},
		{Scopes{ScopeEdit}, map[Scope]bool{ScopeListen: true, ScopeEdit: true}},
		{Scopes{ScopeAdmin}, map[Scope]bool{ScopeListen: true, ScopeEdit: true, ScopeAdmin: true}},
		{Scopes{ScopeUpload}, map[Scope]bool{ScopeUpload: true}},
		{Scopes{ScopeAdmin, ScopeUpload}, map[Scope]bool{ScopeListen: true, ScopeEdit: true, ScopeAdmin: true, ScopeUpload: true}},
		{Scopes{ScopeListen, ScopeUpload}, map[Scope]bool{ScopeListen: true, ScopeUpload: true}},
		{Scopes{"bogus"}, map[Scope]bool{}},
	}
	for _, tt := range tests {
		for _, sc := range []Scope{ScopeListen, ScopeEdit, ScopeAdmin, ScopeUpload, "", "bogus"} {
			if got := tt.s.Has(sc); got != tt.want[sc] {
				t.Errorf("%q.Has(%q) = %v, want %v", tt.s, sc, got, tt.want[sc])
			}
		}
	}
}

func TestScopesNormalizeAndExpand(t *testing.T) {
	tests := []struct {
		s          Scopes
		normalized Scopes
		expanded   Scopes
	}{
		{nil, Scopes{}, Scopes{}},
		{Scopes{ScopeListen}, Scopes{ScopeListen}, Scopes{ScopeListen}},
		{Scopes{ScopeEdit}, Scopes{ScopeEdit}, Scopes{ScopeEdit, ScopeListen}},
		{Scopes{ScopeUpload, ScopeAdmin}, Scopes{ScopeAdmin, ScopeUpload},
			Scopes{ScopeAdmin, ScopeEdit, ScopeListen, ScopeUpload}},
		{Scopes{ScopeUpload}, Scopes{ScopeUpload}, Scopes{ScopeUpload}},
		{Scopes{ScopeUpload, ScopeListen, ScopeUpload}, Scopes{ScopeListen, ScopeUpload},
			Scopes{ScopeListen, ScopeUpload}},
	}
	for _, tt := range tests {
		orig := append(Scopes(nil), tt.s...)
		if got := tt.s.Normalize(); !reflect.DeepEqual(got, tt.normalized) {
			t.Errorf("%q.Normalize() = %q, want %q", tt.s, got, tt.normalized)
		}
		if !reflect.DeepEqual(tt.s, orig) {
			t.Errorf("Normalize modified its receiver: %q -> %q", orig, tt.s)
		}
		if got := tt.s.Expand(); !reflect.DeepEqual(got, tt.expanded) {
			t.Errorf("%q.Expand() = %q, want %q", tt.s, got, tt.expanded)
		}
	}
}

func TestScopesJSON(t *testing.T) {
	for _, tt := range []struct {
		s    Scopes
		want string
	}{
		{nil, `[]`},
		{Scopes{}, `[]`},
		{Scopes{ScopeAdmin, ScopeUpload}, `["admin","upload"]`},
	} {
		b, err := json.Marshal(tt.s)
		if err != nil || string(b) != tt.want {
			t.Errorf("Marshal(%q) = %s, %v; want %s", tt.s, b, err, tt.want)
		}
	}
	var s Scopes
	if err := json.Unmarshal([]byte(`["edit","upload"]`), &s); err != nil || !reflect.DeepEqual(s, Scopes{ScopeEdit, ScopeUpload}) {
		t.Errorf("Unmarshal = %q, %v", s, err)
	}
	if got := (Scopes{ScopeListen}).Strings(); !reflect.DeepEqual(got, []string{"listen"}) {
		t.Errorf("Strings() = %q", got)
	}
	if got := Scopes(nil).Strings(); got == nil {
		t.Error("Strings() of nil Scopes returned nil")
	}
}
