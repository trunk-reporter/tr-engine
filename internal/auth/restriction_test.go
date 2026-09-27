package auth

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func tgs(ss ...string) []TG {
	out := make([]TG, len(ss))
	for i, s := range ss {
		t, err := ParseTG(s)
		if err != nil {
			panic(err)
		}
		out[i] = t
	}
	return out
}

func TestParseTG(t *testing.T) {
	valid := map[string]TG{
		"1:9178":                {1, 9178},
		"2:1":                   {2, 1},
		"2147483647:2147483647": {2147483647, 2147483647},
		"007:0042":              {7, 42}, // leading zeros are plain decimal
	}
	for in, want := range valid {
		got, err := ParseTG(in)
		if err != nil || got != want {
			t.Errorf("ParseTG(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	invalid := []string{
		"", "1", "1:", ":1", ":", "0:1", "1:0", "0:0", "-1:2", "1:-2", "+1:2", "1:+2",
		" 1:2", "1:2 ", "1 :2", "1:2:3", "a:b", "1:b", "1.0:2", "1_0:2", "0x1:2", "1e3:2",
		"2147483648:1", "1:2147483648", "99999999999999999999:1", "\uff11:2", "1/2",
	}
	for _, in := range invalid {
		if got, err := ParseTG(in); err == nil {
			t.Errorf("ParseTG(%q) = %v, want error", in, got)
		}
	}
}

func TestTGStringAndJSON(t *testing.T) {
	tg := TG{SystemID: 2, Tgid: 9178}
	if tg.String() != "2:9178" {
		t.Errorf("String() = %q", tg.String())
	}
	b, err := json.Marshal([]TG{tg, {1, 5}})
	if err != nil || string(b) != `["2:9178","1:5"]` {
		t.Errorf("Marshal = %s, %v", b, err)
	}
	var back []TG
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, []TG{tg, {1, 5}}) {
		t.Errorf("Unmarshal round trip = %v, %v", back, err)
	}
	for _, in := range []string{`1`, `null`, `"1:0"`, `"x"`, `{"SystemID":1,"Tgid":2}`, `["1:2"]`, `true`} {
		var v TG
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("Unmarshal(%s) = %v, want error", in, v)
		}
	}
	var list []TG
	if err := json.Unmarshal([]byte(`["1:2",null]`), &list); err == nil {
		t.Errorf("Unmarshal with a null entry = %v, want error", list)
	}
}

func TestRestrictionUnmarshal(t *testing.T) {
	type body struct {
		Restriction *Restriction `json:"restriction"`
	}
	tests := []struct {
		in      string
		want    *Restriction
		wantErr string
	}{
		{`{"restriction":null}`, nil, ""},
		{`{}`, nil, ""},
		{`{"restriction":{}}`, &Restriction{}, ""},
		{`{"restriction":{"systems":[1,3],"talkgroups":["2:9178","2:9179"],"exclude_talkgroups":["1:5001"]}}`,
			&Restriction{Systems: []int{1, 3}, Talkgroups: tgs("2:9178", "2:9179"), ExcludeTalkgroups: tgs("1:5001")}, ""},
		{`{"restriction":{"allow_all":true,"exclude_talkgroups":["1:5001","1:5002"]}}`,
			&Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001", "1:5002")}, ""},
		{`{"restriction":{"allow_all":true,"systems":null}}`, &Restriction{AllowAll: true}, ""},
		// A misspelled field must not silently drop an exclusion.
		{`{"restriction":{"allow_all":true,"exclude_talkgroup":["1:5001"]}}`, nil, "unknown field"},
		{`{"restriction":{"system":[1]}}`, nil, "unknown field"},
		{`{"restriction":{"talkgroups":["1:0"]}}`, nil, "invalid talkgroup"},
		{`{"restriction":{"talkgroups":[9178]}}`, nil, "invalid talkgroup"},
		{`{"restriction":{"systems":"1"}}`, nil, "cannot unmarshal"},
		{`{"restriction":{"allow_all":"yes"}}`, nil, "cannot unmarshal"},
		{`{"restriction":[]}`, nil, "cannot unmarshal"},
		{`{"restriction":"all"}`, nil, "cannot unmarshal"},
	}
	for _, tt := range tests {
		var b body
		err := json.Unmarshal([]byte(tt.in), &b)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Unmarshal(%s) error = %v, want containing %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("Unmarshal(%s) unexpected error: %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(b.Restriction, tt.want) {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, b.Restriction, tt.want)
		}
	}
}

func TestRestrictionNormalize(t *testing.T) {
	if (*Restriction)(nil).Normalize() != nil {
		t.Error("nil.Normalize() != nil")
	}

	empty := (&Restriction{}).Normalize()
	if empty == nil || empty.Systems == nil || empty.Talkgroups == nil || empty.ExcludeTalkgroups == nil {
		t.Fatalf("Normalize of {} = %#v, want non-nil empty arrays", empty)
	}
	b, _ := json.Marshal(empty)
	if string(b) != `{"allow_all":false,"systems":[],"talkgroups":[],"exclude_talkgroups":[]}` {
		t.Errorf("Marshal(normalized {}) = %s", b)
	}

	in := &Restriction{
		Systems:           []int{3, 1, 3, 2, 1},
		Talkgroups:        tgs("2:9179", "1:5", "2:9178", "1:5", "10:1"),
		ExcludeTalkgroups: tgs("1:5002", "1:5001", "1:5002"),
	}
	orig := &Restriction{
		Systems:           append([]int(nil), in.Systems...),
		Talkgroups:        append([]TG(nil), in.Talkgroups...),
		ExcludeTalkgroups: append([]TG(nil), in.ExcludeTalkgroups...),
	}
	got := in.Normalize()
	want := &Restriction{
		Systems:           []int{1, 2, 3},
		Talkgroups:        tgs("1:5", "2:9178", "2:9179", "10:1"),
		ExcludeTalkgroups: tgs("1:5001", "1:5002"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Normalize() = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("Normalize modified its receiver: %+v", in)
	}
	got.Systems[0] = 99
	got.Talkgroups[0] = TG{99, 99}
	if !reflect.DeepEqual(in, orig) {
		t.Error("Normalize result shares arrays with its receiver")
	}
	if !(&Restriction{AllowAll: true}).Normalize().AllowAll {
		t.Error("Normalize dropped AllowAll")
	}
}

func manyInts(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func manyTGs(n int) []TG {
	out := make([]TG, n)
	for i := range out {
		out[i] = TG{1, i + 1}
	}
	return out
}

func TestRestrictionValidate(t *testing.T) {
	type check struct {
		kind Kind
		err  error  // errors.Is target, or
		msg  string // substring; both zero = valid
	}
	ok := func(k Kind) check { return check{kind: k} }
	fails := func(k Kind, msg string) check { return check{kind: k, msg: msg} }
	is := func(k Kind, err error) check { return check{kind: k, err: err} }
	allKinds := func(c func(Kind) check) []check {
		return []check{c(KindKey), c(KindAnonymous), c(KindTicket)}
	}

	tests := []struct {
		name   string
		r      *Restriction
		checks []check
	}{
		{"nil is unrestricted", nil, allKinds(ok)},
		{"allow_all", &Restriction{AllowAll: true}, allKinds(ok)},
		{"allow_all with exclusions", &Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001")}, allKinds(ok)},
		{"systems", &Restriction{Systems: []int{1, 3}}, allKinds(ok)},
		{"talkgroups", &Restriction{Talkgroups: tgs("2:9178")}, allKinds(ok)},
		{"everything", &Restriction{Systems: []int{1}, Talkgroups: tgs("2:9178"), ExcludeTalkgroups: tgs("1:5001")}, allKinds(ok)},
		{"empty object allows nothing", &Restriction{}, []check{
			is(KindKey, ErrAllowsNothing), is(KindAnonymous, ErrAnonymousAllowsNothing), ok(KindTicket)}},
		{"empty arrays allow nothing", &Restriction{Systems: []int{}, Talkgroups: []TG{}}, []check{
			is(KindKey, ErrAllowsNothing), is(KindAnonymous, ErrAnonymousAllowsNothing), ok(KindTicket)}},
		{"exclude-only allows nothing", &Restriction{ExcludeTalkgroups: tgs("1:5001")}, []check{
			is(KindKey, ErrAllowsNothing), is(KindAnonymous, ErrAnonymousAllowsNothing), ok(KindTicket)}},
		{"allow_all with systems", &Restriction{AllowAll: true, Systems: []int{1}},
			allKinds(func(k Kind) check { return fails(k, "allow_all cannot be combined") })},
		{"allow_all with talkgroups", &Restriction{AllowAll: true, Talkgroups: tgs("1:2")},
			allKinds(func(k Kind) check { return fails(k, "allow_all cannot be combined") })},
		{"zero system", &Restriction{Systems: []int{1, 0}},
			allKinds(func(k Kind) check { return fails(k, "systems[1]") })},
		{"negative system", &Restriction{Systems: []int{-3}},
			allKinds(func(k Kind) check { return fails(k, "systems[0]") })},
		{"system beyond int4", &Restriction{Systems: []int{maxID + 1}},
			allKinds(func(k Kind) check { return fails(k, "systems[0]") })},
		{"zero tgid", &Restriction{Talkgroups: []TG{{1, 0}}},
			allKinds(func(k Kind) check { return fails(k, "talkgroups[0]") })},
		{"zero exclusion system", &Restriction{AllowAll: true, ExcludeTalkgroups: []TG{{1, 2}, {0, 5}}},
			allKinds(func(k Kind) check { return fails(k, "exclude_talkgroups[1]") })},
		{"tgid beyond int4", &Restriction{Talkgroups: []TG{{1, maxID + 1}}},
			allKinds(func(k Kind) check { return fails(k, "talkgroups[0]") })},
		{"1000 systems", &Restriction{Systems: manyInts(1000)}, []check{
			ok(KindKey), ok(KindAnonymous), is(KindTicket, ErrTicketTooLarge)}},
		{"1001 systems", &Restriction{Systems: manyInts(1001)}, []check{
			fails(KindKey, "systems: at most 1000"), fails(KindAnonymous, "systems: at most 1000"), is(KindTicket, ErrTicketTooLarge)}},
		{"1001 talkgroups", &Restriction{Talkgroups: manyTGs(1001)}, []check{
			fails(KindKey, "talkgroups: at most 1000"), is(KindTicket, ErrTicketTooLarge)}},
		{"1001 exclusions", &Restriction{AllowAll: true, ExcludeTalkgroups: manyTGs(1001)}, []check{
			fails(KindAnonymous, "exclude_talkgroups: at most 1000"), is(KindTicket, ErrTicketTooLarge)}},
		{"100 ticket entries", &Restriction{Systems: manyInts(40), Talkgroups: manyTGs(30), ExcludeTalkgroups: manyTGs(30)},
			[]check{ok(KindTicket)}},
		{"101 ticket entries", &Restriction{Systems: manyInts(40), Talkgroups: manyTGs(30), ExcludeTalkgroups: manyTGs(31)},
			[]check{is(KindTicket, ErrTicketTooLarge), ok(KindKey)}},
		{"unknown kind", &Restriction{AllowAll: true}, []check{
			fails(KindInternal, "not valid on"), fails("", "not valid on"), fails("bogus", "not valid on")}},
		{"unknown kind, nil restriction", nil, []check{fails(KindInternal, "not valid on")}},
	}
	for _, tt := range tests {
		for _, c := range tt.checks {
			err := tt.r.Validate(c.kind)
			switch {
			case c.err != nil:
				if !errors.Is(err, c.err) {
					t.Errorf("%s: Validate(%q) = %v, want %v", tt.name, c.kind, err, c.err)
				}
			case c.msg != "":
				if err == nil || !strings.Contains(err.Error(), c.msg) {
					t.Errorf("%s: Validate(%q) = %v, want containing %q", tt.name, c.kind, err, c.msg)
				}
			default:
				if err != nil {
					t.Errorf("%s: Validate(%q) unexpected error: %v", tt.name, c.kind, err)
				}
			}
		}
	}
}

func TestValidateKey(t *testing.T) {
	r := &Restriction{Systems: []int{1}}
	tests := []struct {
		scopes  Scopes
		r       *Restriction
		wantErr string
	}{
		{Scopes{ScopeListen}, nil, ""},
		{Scopes{ScopeListen}, r, ""},
		{Scopes{ScopeAdmin, ScopeUpload}, nil, ""},
		{Scopes{ScopeEdit}, r, `exactly ["listen"]`},
		{Scopes{ScopeAdmin}, r, `exactly ["listen"]`},
		{Scopes{ScopeListen, ScopeUpload}, r, `exactly ["listen"]`},
		{Scopes{ScopeUpload}, r, `exactly ["listen"]`},
		{Scopes{ScopeListen}, &Restriction{}, "allows nothing"},
		{Scopes{ScopeListen}, &Restriction{AllowAll: true, Systems: []int{1}}, "allow_all"},
		{nil, nil, "must not be empty"},
		{Scopes{ScopeListen, ScopeEdit}, nil, "cannot be combined"},
	}
	for _, tt := range tests {
		err := ValidateKey(tt.scopes, tt.r)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateKey(%q, %+v) unexpected error: %v", tt.scopes, tt.r, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ValidateKey(%q, %+v) = %v, want containing %q", tt.scopes, tt.r, err, tt.wantErr)
		}
	}
}

func TestRestrictionAllowsTG(t *testing.T) {
	mixed := &Restriction{Systems: []int{1, 3}, Talkgroups: tgs("2:9178", "2:9179"), ExcludeTalkgroups: tgs("1:5001")}
	allowAll := &Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001", "1:5002")}
	type pair struct {
		sys, tg int
		want    bool
	}
	tests := []struct {
		name  string
		r     *Restriction
		pairs []pair
	}{
		{"nil allows everything", nil, []pair{{1, 1, true}, {1, 0, true}, {0, 0, true}, {7, 9999, true}}},
		{"systems + talkgroups + exclusion", mixed, []pair{
			{1, 100, true},
			{1, 5001, false}, // excluded although its system is allowed
			{3, 7, true},
			{2, 9178, true},
			{2, 9179, true},
			{2, 9180, false}, // system 2 is only visible through two talkgroups
			{4, 1, false},
			{1, 0, false}, // no talkgroup: never allowed when restricted
			{0, 100, false},
			{-1, 100, false},
			{1, -5, false},
		}},
		{"allow_all + exclusions", allowAll, []pair{
			{1, 5001, false},
			{1, 5002, false},
			{1, 5003, true},
			{2, 5001, true},
			{99, 1, true},
			{1, 0, false},
			{0, 5001, false}, // an unresolved system can't be matched against exclusions
			{0, 7, false},
		}},
		{"systems only", &Restriction{Systems: []int{5}}, []pair{{5, 1, true}, {5, 0, false}, {6, 1, false}}},
		{"talkgroups only", &Restriction{Talkgroups: tgs("5:1")}, []pair{{5, 1, true}, {5, 2, false}, {1, 5, false}}},
		{"exclusion beats talkgroup", &Restriction{Talkgroups: tgs("2:9178"), ExcludeTalkgroups: tgs("2:9178")},
			[]pair{{2, 9178, false}}},
		{"empty allows nothing", &Restriction{}, []pair{{1, 1, false}, {1, 0, false}, {0, 0, false}}},
		{"exclude-only allows nothing", &Restriction{ExcludeTalkgroups: tgs("1:5")}, []pair{{1, 1, false}, {2, 2, false}}},
		{"allow_all with systems: allow_all dominates", &Restriction{AllowAll: true, Systems: []int{1}}, []pair{{2, 1, true}}},
	}
	for _, tt := range tests {
		for _, p := range tt.pairs {
			if got := tt.r.AllowsTG(p.sys, p.tg); got != p.want {
				t.Errorf("%s: AllowsTG(%d, %d) = %v, want %v", tt.name, p.sys, p.tg, got, p.want)
			}
		}
	}
}

func TestRestrictionAllowsNothing(t *testing.T) {
	tests := []struct {
		r    *Restriction
		want bool
	}{
		{nil, false},
		{&Restriction{}, true},
		{&Restriction{Systems: []int{}, Talkgroups: []TG{}}, true},
		{&Restriction{ExcludeTalkgroups: tgs("1:5")}, true},
		{&Restriction{AllowAll: true}, false},
		{&Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5")}, false},
		{&Restriction{Systems: []int{1}}, false},
		{&Restriction{Talkgroups: tgs("1:5")}, false},
	}
	for _, tt := range tests {
		if got := tt.r.AllowsNothing(); got != tt.want {
			t.Errorf("%+v.AllowsNothing() = %v, want %v", tt.r, got, tt.want)
		}
	}
}

func TestRestrictionSystemVisible(t *testing.T) {
	mixed := &Restriction{Systems: []int{1, 3}, Talkgroups: tgs("2:9178"), ExcludeTalkgroups: tgs("5:1")}
	tests := []struct {
		name string
		r    *Restriction
		want map[int]bool // systems 0..6; missing = false
	}{
		{"nil", nil, map[int]bool{-1: true, 0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true}},
		{"mixed", mixed, map[int]bool{1: true, 2: true, 3: true}}, // 5 only appears in exclusions
		{"allow_all", &Restriction{AllowAll: true}, map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}},
		{"empty", &Restriction{}, map[int]bool{}},
		{"exclude-only", &Restriction{ExcludeTalkgroups: tgs("5:1")}, map[int]bool{}},
	}
	for _, tt := range tests {
		for sys := -1; sys <= 6; sys++ {
			if got := tt.r.SystemVisible(sys); got != tt.want[sys] {
				t.Errorf("%s: SystemVisible(%d) = %v, want %v", tt.name, sys, got, tt.want[sys])
			}
		}
	}
}

func TestRestrictionRewriteSystem(t *testing.T) {
	r := &Restriction{
		Systems:           []int{3, 1},
		Talkgroups:        tgs("3:100", "1:100", "2:5"),
		ExcludeTalkgroups: tgs("3:5001", "1:5001", "2:7"),
	}
	alias := *r // shares r's arrays, like a cached copy would
	aliasSystems := append([]int(nil), r.Systems...)
	aliasTGs := append([]TG(nil), r.Talkgroups...)

	if !r.RewriteSystem(3, 1) {
		t.Fatal("RewriteSystem(3, 1) = false, want true")
	}
	want := &Restriction{
		Systems:           []int{1},
		Talkgroups:        tgs("1:100", "2:5"),
		ExcludeTalkgroups: tgs("1:5001", "2:7"),
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("after RewriteSystem(3, 1) = %+v, want %+v", r, want)
	}
	if !reflect.DeepEqual(alias.Systems, aliasSystems) || !reflect.DeepEqual(alias.Talkgroups, aliasTGs) {
		t.Error("RewriteSystem modified arrays shared with a copy")
	}

	// An exclusion that pointed at the merged-away system keeps hiding the
	// talkgroup under its new system ID.
	pub := &Restriction{AllowAll: true, ExcludeTalkgroups: tgs("3:5001")}
	if !pub.AllowsTG(1, 5001) {
		t.Fatal("precondition: 1:5001 should be visible before the rewrite")
	}
	pub.RewriteSystem(3, 1)
	if pub.AllowsTG(1, 5001) {
		t.Error("after merging 3 into 1, 1:5001 is visible although 3:5001 was excluded")
	}
	if !pub.AllowAll {
		t.Error("RewriteSystem dropped AllowAll")
	}

	unchanged := []*Restriction{
		{Systems: []int{1}, Talkgroups: tgs("2:5")},
		{AllowAll: true},
		{},
	}
	for _, u := range unchanged {
		before := *u
		if u.RewriteSystem(3, 1) {
			t.Errorf("RewriteSystem(3, 1) on %+v = true, want false", before)
		}
		if !reflect.DeepEqual(*u, before) {
			t.Errorf("RewriteSystem changed %+v to %+v", before, *u)
		}
	}
	same := &Restriction{Systems: []int{3}}
	for _, ft := range [][2]int{{3, 3}, {0, 1}, {3, 0}, {-3, 1}} {
		if same.RewriteSystem(ft[0], ft[1]) || !reflect.DeepEqual(same.Systems, []int{3}) {
			t.Errorf("RewriteSystem(%d, %d) changed %+v", ft[0], ft[1], same)
		}
	}
	if (*Restriction)(nil).RewriteSystem(3, 1) {
		t.Error("nil.RewriteSystem = true")
	}
}

func TestRestrictionReferencedSystems(t *testing.T) {
	r := &Restriction{Systems: []int{5}, Talkgroups: tgs("2:1", "5:3"), ExcludeTalkgroups: tgs("9:1", "2:4")}
	if got := r.ReferencedSystems(); !reflect.DeepEqual(got, []int{2, 5, 9}) {
		t.Errorf("ReferencedSystems() = %v, want [2 5 9]", got)
	}
	for _, e := range []*Restriction{nil, {}, {AllowAll: true}} {
		if got := e.ReferencedSystems(); got == nil || len(got) != 0 {
			t.Errorf("%+v.ReferencedSystems() = %#v, want empty non-nil", e, got)
		}
	}
}

func TestRestrictionEqual(t *testing.T) {
	tests := []struct {
		a, b *Restriction
		want bool
	}{
		{nil, nil, true},
		{nil, &Restriction{}, false},
		{&Restriction{}, nil, false},
		{&Restriction{}, &Restriction{Systems: []int{}, Talkgroups: []TG{}, ExcludeTalkgroups: []TG{}}, true},
		{&Restriction{Systems: []int{1, 2}}, &Restriction{Systems: []int{2, 1, 1}}, true},
		{&Restriction{Talkgroups: tgs("1:2", "1:1")}, &Restriction{Talkgroups: tgs("1:1", "1:2")}, true},
		{&Restriction{AllowAll: true}, &Restriction{}, false},
		{&Restriction{Systems: []int{1}}, &Restriction{Systems: []int{2}}, false},
		{&Restriction{Systems: []int{1}}, &Restriction{Talkgroups: tgs("1:1")}, false},
		{&Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5")}, &Restriction{AllowAll: true}, false},
	}
	for _, tt := range tests {
		if got := tt.a.Equal(tt.b); got != tt.want {
			t.Errorf("%+v.Equal(%+v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
		if got := tt.b.Equal(tt.a); got != tt.want {
			t.Errorf("%+v.Equal(%+v) = %v, want %v (symmetry)", tt.b, tt.a, got, tt.want)
		}
	}
}

// allowedSet enumerates the (system, tgid) pairs a principal allows on a small
// grid that includes zero and negative IDs.
func allowedSet(p *Principal) map[TG]bool {
	out := map[TG]bool{}
	for sys := -1; sys <= 4; sys++ {
		for tg := -1; tg <= 4; tg++ {
			if p.AllowsTG(sys, tg) {
				out[TG{sys, tg}] = true
			}
		}
	}
	return out
}

// shrinkings returns every restriction obtained from r by removing one allow
// entry, or by switching AllowAll off.
func shrinkings(r Restriction) []Restriction {
	var out []Restriction
	if r.AllowAll {
		s := r
		s.AllowAll = false
		out = append(out, s)
	}
	for i := range r.Systems {
		s := r
		s.Systems = append(append([]int{}, r.Systems[:i]...), r.Systems[i+1:]...)
		out = append(out, s)
	}
	for i := range r.Talkgroups {
		s := r
		s.Talkgroups = append(append([]TG{}, r.Talkgroups[:i]...), r.Talkgroups[i+1:]...)
		out = append(out, s)
	}
	return out
}

// TestRemovingAllowEntriesNeverWidens is the §7.1 property: removing allow
// entries, down to and including the last one, never widens access.
func TestRemovingAllowEntriesNeverWidens(t *testing.T) {
	seeds := []Restriction{
		{Systems: []int{1, 2}, Talkgroups: tgs("3:1", "3:2"), ExcludeTalkgroups: tgs("1:1")},
		{Talkgroups: tgs("1:1")},
		{Systems: []int{4}},
		{AllowAll: true, ExcludeTalkgroups: tgs("2:2")},
		{AllowAll: true},
	}
	var walk func(r Restriction, depth int)
	walk = func(r Restriction, depth int) {
		before := allowedSet(&Principal{Restrictions: []Restriction{r}})
		for _, s := range shrinkings(r) {
			after := allowedSet(&Principal{Restrictions: []Restriction{s}})
			for pair := range after {
				if !before[pair] {
					t.Errorf("shrinking %+v to %+v newly allows %v", r, s, pair)
				}
			}
			if s.AllowsNothing() {
				if len(after) != 0 {
					t.Errorf("%+v allows nothing but AllowsTG allows %v", s, after)
				}
				if clause, _ := (&Principal{Restrictions: []Restriction{s}}).SQL("system_id", "tgid", 1); !strings.Contains(clause, "FALSE") {
					t.Errorf("%+v allows nothing but SQL is %q", s, clause)
				}
			}
			walk(s, depth+1)
		}
	}
	for _, seed := range seeds {
		walk(seed, 0)
	}
}
