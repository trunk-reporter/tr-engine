// Package auth holds the pure building blocks of tr-engine's API-key auth:
// scopes, restrictions, principals, the SQL restriction clause, stream tickets
// and the auth generation counter. It depends on no other tr-engine package so
// that database, ingest, audio and api can all import it.
//
// Everything here fails closed: an empty allow list allows nothing, a nil
// *Principal is allowed nothing, and a restriction object is never turned into
// "unrestricted" (only a nil *Restriction means that).
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Scope is one grant held by a credential. The empty Scope is used by route
// policies to mean "public"; no credential ever holds it.
type Scope string

const (
	ScopeListen Scope = "listen" // read radio data, stream events and audio, mint tickets
	ScopeEdit   Scope = "edit"   // change radio metadata; implies listen
	ScopeAdmin  Scope = "admin"  // everything else; implies edit and listen
	ScopeUpload Scope = "upload" // POST /call-upload only; implies nothing
)

// level orders the listen < edit < admin chain. upload is outside the chain
// and unknown scopes have no level.
func (s Scope) level() int {
	switch s {
	case ScopeListen:
		return 1
	case ScopeEdit:
		return 2
	case ScopeAdmin:
		return 3
	}
	return 0
}

// Valid reports whether s is one of the four known scopes.
func (s Scope) Valid() bool {
	return s.level() > 0 || s == ScopeUpload
}

// Implies reports whether holding s grants t: admin implies edit and listen,
// edit implies listen, and every valid scope implies itself. upload implies
// nothing else and is implied by nothing else. Unknown or empty scopes imply
// nothing and are implied by nothing.
func (s Scope) Implies(t Scope) bool {
	if !s.Valid() || !t.Valid() {
		return false
	}
	if s == ScopeUpload || t == ScopeUpload {
		return s == t
	}
	return s.level() >= t.level()
}

// Scopes is a credential's set of scopes, as stored on a key: at most one of
// listen/edit/admin, optionally plus upload.
type Scopes []Scope

// ParseScopes converts and validates a scope list as received from an API
// body or the CLI. The result is normalized (sorted).
func ParseScopes(in []string) (Scopes, error) {
	s := make(Scopes, len(in))
	for i, v := range in {
		s[i] = Scope(v)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s.Normalize(), nil
}

// Validate checks the §3.1 rules: the set is non-empty, every entry is a
// known scope, no scope appears twice, and at most one of listen/edit/admin
// is present (edit and admin already imply the lower levels).
func (s Scopes) Validate() error {
	if len(s) == 0 {
		return errors.New("scopes must not be empty")
	}
	var level Scope
	seen := make(map[Scope]bool, len(s))
	for _, sc := range s {
		if !sc.Valid() {
			return fmt.Errorf("unknown scope %q (valid: listen, edit, admin, upload)", string(sc))
		}
		if seen[sc] {
			return fmt.Errorf("duplicate scope %q", string(sc))
		}
		seen[sc] = true
		if sc.level() > 0 {
			if level != "" {
				return fmt.Errorf("scopes %q and %q cannot be combined: use only the higher one (admin implies edit, edit implies listen)",
					string(level), string(sc))
			}
			level = sc
		}
	}
	return nil
}

// Has reports whether any scope in s implies want.
func (s Scopes) Has(want Scope) bool {
	for _, sc := range s {
		if sc.Implies(want) {
			return true
		}
	}
	return false
}

// Normalize returns a sorted copy of s without duplicates. Scopes sort
// alphabetically, which is also the stored order: admin, edit, listen, upload.
// The result is never nil.
func (s Scopes) Normalize() Scopes {
	out := make(Scopes, 0, len(s))
	out = append(out, s...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Expand returns the effective scopes with implications expanded, sorted, as
// shown by whoami: ["edit"] becomes ["edit","listen"]. Unknown entries are
// dropped. The result is never nil, so it encodes as [] rather than null.
func (s Scopes) Expand() Scopes {
	out := Scopes{}
	for _, sc := range []Scope{ScopeAdmin, ScopeEdit, ScopeListen, ScopeUpload} {
		if s.Has(sc) {
			out = append(out, sc)
		}
	}
	return out
}

// Strings returns the scopes as plain strings (e.g. for a text[] column).
// The result is never nil.
func (s Scopes) Strings() []string {
	out := make([]string, len(s))
	for i, sc := range s {
		out[i] = string(sc)
	}
	return out
}

// MarshalJSON encodes a nil Scopes as [] rather than null.
func (s Scopes) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Strings())
}
