package auth

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Restriction limits (§3.2, §3.5).
const (
	MaxRestrictionEntries = 1000 // per array
	MaxTicketEntries      = 100  // all arrays together, on a ticket narrowing

	// System IDs and tgids are PostgreSQL int columns, and the SQL clause
	// passes them as int4 arrays.
	maxID = math.MaxInt32
)

// Validation errors that callers map to specific API messages.
var (
	ErrAllowsNothing          = errors.New("restriction allows nothing")
	ErrAnonymousAllowsNothing = errors.New("restriction allows nothing; use access: off instead")
	ErrTicketTooLarge         = errors.New("ticket restriction too large — narrow by system or mint several tickets")
)

// TG identifies one talkgroup within a system. Its JSON form is the composite
// string "system_id:tgid".
type TG struct {
	SystemID int
	Tgid     int
}

// ParseTG parses "<system_id>:<tgid>", where both parts are plain decimal
// integers in 1..2147483647 (no sign, no spaces).
func ParseTG(s string) (TG, error) {
	sys, tg, ok := strings.Cut(s, ":")
	a, okA := parseID(sys)
	b, okB := parseID(tg)
	if !ok || !okA || !okB {
		return TG{}, fmt.Errorf("invalid talkgroup %q: want \"system_id:tgid\" with positive integer IDs", s)
	}
	return TG{SystemID: a, Tgid: b}, nil
}

// parseID accepts only ASCII digits (ParseUint rejects signs and, in base 10,
// underscores) and a value in 1..maxID.
func parseID(s string) (int, bool) {
	n, err := strconv.ParseUint(s, 10, 31)
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}

func validID(n int) bool { return n > 0 && n <= maxID }

func (t TG) valid() bool { return validID(t.SystemID) && validID(t.Tgid) }

// String returns the composite form, e.g. "1:9178".
func (t TG) String() string {
	return strconv.Itoa(t.SystemID) + ":" + strconv.Itoa(t.Tgid)
}

func (t TG) compare(u TG) int {
	if c := cmp.Compare(t.SystemID, u.SystemID); c != 0 {
		return c
	}
	return cmp.Compare(t.Tgid, u.Tgid)
}

// MarshalJSON encodes t as its composite string.
func (t TG) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// UnmarshalJSON accepts only a composite string that ParseTG accepts. Unlike
// most unmarshalers it rejects null: a talkgroup entry is never optional.
func (t *TG) UnmarshalJSON(b []byte) error {
	var s string
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("invalid talkgroup: null")
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid talkgroup: want a \"system_id:tgid\" string")
	}
	v, err := ParseTG(s)
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// Restriction limits which radio data a listen credential can see (§3.2).
//
// A pair (system, tgid) is allowed when tgid is non-zero, and (AllowAll, or
// system is in Systems, or the pair is in Talkgroups), and the pair is not in
// ExcludeTalkgroups. With AllowAll false and both allow lists empty the
// restriction allows nothing. Only a nil *Restriction means unrestricted.
type Restriction struct {
	AllowAll          bool  `json:"allow_all"`
	Systems           []int `json:"systems"`
	Talkgroups        []TG  `json:"talkgroups"`
	ExcludeTalkgroups []TG  `json:"exclude_talkgroups"`
}

// UnmarshalJSON decodes a restriction object and rejects unknown fields: a
// misspelled "exclude_talkgroups" would otherwise silently drop exclusions.
func (r *Restriction) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	type plain Restriction
	var v plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("invalid restriction: %w", err)
	}
	*r = Restriction(v)
	return nil
}

// Normalize returns a copy with every array deduplicated and sorted
// (talkgroups by system, then tgid). The copy shares no memory with r, its
// arrays are never nil (they encode as []), and a non-nil r never yields nil.
// A nil r stays nil.
func (r *Restriction) Normalize() *Restriction {
	if r == nil {
		return nil
	}
	return &Restriction{
		AllowAll:          r.AllowAll,
		Systems:           normInts(r.Systems),
		Talkgroups:        normTGs(r.Talkgroups),
		ExcludeTalkgroups: normTGs(r.ExcludeTalkgroups),
	}
}

func normInts(in []int) []int {
	out := append(make([]int, 0, len(in)), in...)
	slices.Sort(out)
	return slices.Compact(out)
}

func normTGs(in []TG) []TG {
	out := append(make([]TG, 0, len(in)), in...)
	slices.SortFunc(out, TG.compare)
	return slices.Compact(out)
}

// Validate checks r as a restriction on a credential of the given kind
// (KindKey, KindAnonymous or KindTicket; any other kind is an error). A nil r
// (unrestricted) is always valid. It checks the arrays as they are, so a
// caller may validate before or after Normalize.
//
//   - Every array holds at most MaxRestrictionEntries entries; a ticket
//     narrowing holds at most MaxTicketEntries entries in total
//     (ErrTicketTooLarge).
//   - System IDs and both parts of every talkgroup are in 1..2147483647.
//   - AllowAll is not combined with non-empty Systems or Talkgroups.
//   - A restriction that allows nothing is rejected on keys
//     (ErrAllowsNothing) and on the anonymous policy
//     (ErrAnonymousAllowsNothing), and accepted on tickets.
//
// Whether a key may carry a restriction at all depends on its scopes; see
// ValidateKey.
func (r *Restriction) Validate(kind Kind) error { return r.validate(kind, true) }

// ValidateStored checks a stored restriction the way Validate checks input,
// except for the per-array MaxRestrictionEntries limit, which applies to
// input only: a system merge keeps every exclusion and adds a copy under the
// other system ID (RewriteSystem), which can grow a stored list past it. A
// ticket narrowing is never stored and keeps its MaxTicketEntries limit.
func (r *Restriction) ValidateStored(kind Kind) error { return r.validate(kind, false) }

func (r *Restriction) validate(kind Kind, input bool) error {
	switch kind {
	case KindKey, KindAnonymous, KindTicket:
	default:
		return fmt.Errorf("restrictions are not valid on %q credentials", string(kind))
	}
	if r == nil {
		return nil
	}
	if kind == KindTicket {
		if n := len(r.Systems) + len(r.Talkgroups) + len(r.ExcludeTalkgroups); n > MaxTicketEntries {
			return fmt.Errorf("%w (%d entries, at most %d)", ErrTicketTooLarge, n, MaxTicketEntries)
		}
	}
	for _, a := range []struct {
		name string
		n    int
	}{
		{"systems", len(r.Systems)},
		{"talkgroups", len(r.Talkgroups)},
		{"exclude_talkgroups", len(r.ExcludeTalkgroups)},
	} {
		if input && a.n > MaxRestrictionEntries {
			return fmt.Errorf("%s: at most %d entries allowed, got %d", a.name, MaxRestrictionEntries, a.n)
		}
	}
	for i, s := range r.Systems {
		if !validID(s) {
			return fmt.Errorf("systems[%d]: system id %d is not a positive 32-bit integer", i, s)
		}
	}
	if err := validateTGs("talkgroups", r.Talkgroups); err != nil {
		return err
	}
	if err := validateTGs("exclude_talkgroups", r.ExcludeTalkgroups); err != nil {
		return err
	}
	if r.AllowAll && (len(r.Systems) > 0 || len(r.Talkgroups) > 0) {
		return errors.New("allow_all cannot be combined with systems or talkgroups")
	}
	if r.AllowsNothing() {
		switch kind {
		case KindKey:
			return ErrAllowsNothing
		case KindAnonymous:
			return ErrAnonymousAllowsNothing
		}
	}
	return nil
}

func validateTGs(name string, tgs []TG) error {
	for i, t := range tgs {
		if !t.valid() {
			return fmt.Errorf("%s[%d]: %q is not a valid talkgroup: want \"system_id:tgid\" with positive integer IDs",
				name, i, t.String())
		}
	}
	return nil
}

// ValidateKey checks a key's scopes and restriction together: the scopes must
// be valid, and a restriction is only allowed when the scopes are exactly
// ["listen"]; it is then validated as a KindKey restriction.
func ValidateKey(scopes Scopes, r *Restriction) error {
	return validateKey(scopes, r, true)
}

// ValidateStoredKey is ValidateKey for a key's stored restriction, which is
// checked with ValidateStored (no MaxRestrictionEntries limit).
func ValidateStoredKey(scopes Scopes, r *Restriction) error {
	return validateKey(scopes, r, false)
}

func validateKey(scopes Scopes, r *Restriction, input bool) error {
	if err := scopes.Validate(); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	if len(scopes) != 1 || scopes[0] != ScopeListen {
		return errors.New(`restriction is only allowed on keys whose scopes are exactly ["listen"]`)
	}
	return r.validate(KindKey, input)
}

// AllowsNothing reports whether r is a restriction object whose allow part is
// empty: AllowAll false and no Systems or Talkgroups. A nil r (unrestricted)
// allows everything.
func (r *Restriction) AllowsNothing() bool {
	return r != nil && !r.AllowAll && len(r.Systems) == 0 && len(r.Talkgroups) == 0
}

// AllowsTG reports whether r allows data for (sys, tg). A nil r allows
// everything. Otherwise a tgid or system ID that is zero or negative (no
// talkgroup, or an unresolved system that could not be matched against
// exclusions) is never allowed.
func (r *Restriction) AllowsTG(sys, tg int) bool {
	if r == nil {
		return true
	}
	if sys <= 0 || tg <= 0 {
		return false
	}
	t := TG{SystemID: sys, Tgid: tg}
	if !r.AllowAll && !slices.Contains(r.Systems, sys) && !slices.Contains(r.Talkgroups, t) {
		return false
	}
	return !slices.Contains(r.ExcludeTalkgroups, t)
}

// SystemVisible reports whether a system's metadata may be shown: AllowAll,
// or the system is in Systems, or some entry in Talkgroups belongs to it. A
// nil r sees every system; a non-positive system ID is never visible to a
// restriction.
func (r *Restriction) SystemVisible(sys int) bool {
	if r == nil {
		return true
	}
	if sys <= 0 {
		return false
	}
	if r.AllowAll || slices.Contains(r.Systems, sys) {
		return true
	}
	for _, t := range r.Talkgroups {
		if t.SystemID == sys {
			return true
		}
	}
	return false
}

// RewriteSystem rewrites the restriction for a merge of system from into
// system to: Systems entries and the system part of Talkgroups entries are
// rewritten, and ExcludeTalkgroups is made symmetric across the two IDs:
// every entry naming from gets a copy naming to, and every entry naming to
// gets a copy naming from, and the originals are kept. Allow lists thus
// follow the data to its new ID (data still carrying the old ID fails
// closed), and exclusions hide the talkgroup under either ID, whichever one
// the exclusion names: data written or buffered under the old ID around the
// merge (after the merge moved the rows, before every writer learned the new
// ID) stays excluded. An exclusion only ever narrows. When anything changed,
// r is replaced by its normalized form and true is returned. The old arrays
// are not modified, so copies sharing them are unaffected. Non-positive IDs,
// from == to and a nil r change nothing.
func (r *Restriction) RewriteSystem(from, to int) bool {
	if r == nil || from == to || from <= 0 || to <= 0 {
		return false
	}
	touched := false
	systems := make([]int, len(r.Systems))
	for i, s := range r.Systems {
		if s == from {
			s, touched = to, true
		}
		systems[i] = s
	}
	tgs := make([]TG, len(r.Talkgroups))
	for i, t := range r.Talkgroups {
		if t.SystemID == from {
			t.SystemID, touched = to, true
		}
		tgs[i] = t
	}
	excl, mirrored := mirrorExclusions(r.ExcludeTalkgroups, from, to)
	if !touched && !mirrored {
		return false
	}
	next := (&Restriction{AllowAll: r.AllowAll, Systems: systems, Talkgroups: tgs, ExcludeTalkgroups: excl}).Normalize()
	if next.Equal(r) {
		return false
	}
	*r = *next
	return true
}

// MirrorExclusions is the exclusion half of RewriteSystem(a, b) (or (b, a)):
// every ExcludeTalkgroups entry naming a gets a copy naming b and vice
// versa. Replaying it over a chain of merges (a into b, b into c) until
// nothing changes makes an exclusion naming any system of the chain name
// all of them. It reports whether r changed; the old arrays are not
// modified.
func (r *Restriction) MirrorExclusions(a, b int) bool {
	if r == nil || a == b || a <= 0 || b <= 0 {
		return false
	}
	excl, mirrored := mirrorExclusions(r.ExcludeTalkgroups, a, b)
	if !mirrored {
		return false
	}
	excl = normTGs(excl)
	if slices.Equal(excl, normTGs(r.ExcludeTalkgroups)) {
		return false
	}
	next := r.Normalize()
	next.ExcludeTalkgroups = excl
	*r = *next
	return true
}

// mirrorExclusions returns excl plus a copy under b of every entry naming a
// and a copy under a of every entry naming b, and whether it added any. The
// result may hold duplicates; excl is not modified.
func mirrorExclusions(excl []TG, a, b int) ([]TG, bool) {
	out := append(make([]TG, 0, len(excl)), excl...)
	added := false
	for _, t := range excl {
		switch t.SystemID {
		case a:
			out, added = append(out, TG{SystemID: b, Tgid: t.Tgid}), true
		case b:
			out, added = append(out, TG{SystemID: a, Tgid: t.Tgid}), true
		}
	}
	return out, added
}

// ReferencedSystems returns every system ID that r mentions in Systems,
// Talkgroups or ExcludeTalkgroups, sorted and deduplicated. Ticket
// verification uses it to reject narrowings that name a merged-away system.
// The result is never nil.
func (r *Restriction) ReferencedSystems() []int {
	if r == nil {
		return []int{}
	}
	ids := append([]int{}, r.Systems...)
	for _, t := range r.Talkgroups {
		ids = append(ids, t.SystemID)
	}
	for _, t := range r.ExcludeTalkgroups {
		ids = append(ids, t.SystemID)
	}
	return normInts(ids)
}

// Equal reports whether r and o describe the same restriction once
// normalized (order, duplicates and nil-vs-empty arrays don't matter). Two nil
// restrictions are equal; nil never equals a restriction object, not even one
// that allows nothing.
func (r *Restriction) Equal(o *Restriction) bool {
	if r == nil || o == nil {
		return r == nil && o == nil
	}
	a, b := r.Normalize(), o.Normalize()
	return a.AllowAll == b.AllowAll &&
		slices.Equal(a.Systems, b.Systems) &&
		slices.Equal(a.Talkgroups, b.Talkgroups) &&
		slices.Equal(a.ExcludeTalkgroups, b.ExcludeTalkgroups)
}
