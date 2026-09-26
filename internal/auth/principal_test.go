package auth

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrincipalHasAndRestricted(t *testing.T) {
	var nilP *Principal
	if nilP.Has(ScopeListen) || nilP.Has(ScopeUpload) {
		t.Error("nil principal has scopes")
	}
	if !nilP.Restricted() {
		t.Error("nil principal must count as restricted")
	}

	upload := &Principal{Kind: KindKey, Scopes: Scopes{ScopeUpload}}
	if upload.Has(ScopeListen) || !upload.Has(ScopeUpload) {
		t.Error("upload-only key: wrong Has results")
	}
	edit := &Principal{Kind: KindKey, Scopes: Scopes{ScopeEdit}}
	if !edit.Has(ScopeListen) || !edit.Has(ScopeEdit) || edit.Has(ScopeAdmin) || edit.Has(ScopeUpload) {
		t.Error("edit key: wrong Has results")
	}
	anonOff := &Principal{Kind: KindAnonymous}
	if anonOff.Has(ScopeListen) {
		t.Error("anonymous principal under policy off has listen")
	}
	if anonOff.Restricted() {
		t.Error("principal without restrictions reported restricted")
	}
	// Even an allow_all restriction restricts: it drops data without a talkgroup.
	allowAll := &Principal{Kind: KindAnonymous, Scopes: Scopes{ScopeListen}, Restrictions: []Restriction{{AllowAll: true}}}
	if !allowAll.Restricted() {
		t.Error("principal with an allow_all restriction not reported restricted")
	}
	if allowAll.AllowsTG(1, 0) {
		t.Error("allow_all restriction allowed a tgid of 0")
	}
}

func TestInternal(t *testing.T) {
	if Internal.Kind != KindInternal {
		t.Errorf("Internal.Kind = %q", Internal.Kind)
	}
	if err := Internal.Scopes.Validate(); err != nil {
		t.Errorf("Internal scopes invalid: %v", err)
	}
	for _, s := range []Scope{ScopeListen, ScopeEdit, ScopeAdmin, ScopeUpload} {
		if !Internal.Has(s) {
			t.Errorf("Internal lacks %q", s)
		}
	}
	if Internal.Restricted() || !Internal.AllowsTG(1, 0) || !Internal.SystemVisible(7) {
		t.Error("Internal is not unrestricted")
	}
	if clause, args := Internal.SQL("system_id", "tgid", 1); clause != "" || args != nil {
		t.Errorf("Internal.SQL = %q, %v; want empty", clause, args)
	}
}

func TestPrincipalAllowsTGIntersection(t *testing.T) {
	key := Restriction{Systems: []int{1, 2}}
	ticket := Restriction{Talkgroups: tgs("2:5", "3:5"), ExcludeTalkgroups: tgs("2:6")}
	p := &Principal{Kind: KindTicket, Scopes: Scopes{ScopeListen}, Restrictions: []Restriction{key, ticket}}
	tests := []struct {
		sys, tg int
		want    bool
	}{
		{2, 5, true},  // allowed by both
		{3, 5, false}, // ticket allows it, key doesn't
		{1, 5, false}, // key allows it, ticket doesn't
		{2, 6, false}, // ticket excludes it
		{2, 0, false},
		{0, 5, false},
	}
	for _, tt := range tests {
		if got := p.AllowsTG(tt.sys, tt.tg); got != tt.want {
			t.Errorf("AllowsTG(%d, %d) = %v, want %v", tt.sys, tt.tg, got, tt.want)
		}
	}
	if !p.SystemVisible(2) || p.SystemVisible(1) || p.SystemVisible(3) {
		t.Errorf("SystemVisible: 1=%v 2=%v 3=%v, want only 2", p.SystemVisible(1), p.SystemVisible(2), p.SystemVisible(3))
	}

	disjoint := &Principal{Restrictions: []Restriction{{Systems: []int{1}}, {Systems: []int{2}}}}
	if got := allowedSet(disjoint); len(got) != 0 {
		t.Errorf("empty intersection allows %v", got)
	}
	if disjoint.SystemVisible(1) || disjoint.SystemVisible(2) {
		t.Error("empty intersection sees a system")
	}

	unrestricted := &Principal{Kind: KindKey, Scopes: Scopes{ScopeListen}}
	if !unrestricted.AllowsTG(1, 0) || !unrestricted.AllowsTG(0, 0) || !unrestricted.SystemVisible(9) {
		t.Error("unrestricted principal denied something")
	}
	var nilP *Principal
	if nilP.AllowsTG(1, 1) || nilP.SystemVisible(1) {
		t.Error("nil principal allowed something")
	}
}

func TestPrincipalAttribution(t *testing.T) {
	tests := []struct {
		p    *Principal
		want string
	}{
		{nil, ""},
		{&Principal{Kind: KindKey, KeyName: "tr-dashboard"}, "tr-dashboard"},
		{&Principal{Kind: KindKey, KeyName: "club site", Actor: "alice#1234"}, "club site / alice#1234"},
		{&Principal{Kind: KindAnonymous}, "anonymous"},
		{Internal, "internal"},
	}
	for _, tt := range tests {
		if got := tt.p.Attribution(); got != tt.want {
			t.Errorf("Attribution() = %q, want %q", got, tt.want)
		}
	}
}

func i32(v ...int32) []int32 { return v }

func TestPrincipalSQL(t *testing.T) {
	const tgNotZero = "c.tgid > 0 AND c.system_id > 0"
	restricted := func(rs ...Restriction) *Principal {
		return &Principal{Kind: KindKey, Scopes: Scopes{ScopeListen}, Restrictions: rs}
	}
	tests := []struct {
		name     string
		p        *Principal
		firstArg int
		clause   string
		args     []any
	}{
		{"unrestricted", &Principal{Kind: KindKey, Scopes: Scopes{ScopeAdmin}}, 1, "", nil},
		{"nil principal", nil, 1, " AND FALSE", nil},
		{"allows nothing", restricted(Restriction{}), 1,
			" AND ((" + tgNotZero + " AND FALSE))", nil},
		{"allows nothing, exclusions dropped", restricted(Restriction{ExcludeTalkgroups: tgs("1:5")}), 1,
			" AND ((" + tgNotZero + " AND FALSE))", nil},
		{"allow_all", restricted(Restriction{AllowAll: true}), 1,
			" AND ((" + tgNotZero + "))", nil},
		{"allow_all + exclusions", restricted(Restriction{AllowAll: true, ExcludeTalkgroups: tgs("1:5001", "1:5002")}), 3,
			" AND ((" + tgNotZero + " AND NOT EXISTS (SELECT 1 FROM unnest($3::int[], $4::int[]) AS x(s, t) WHERE x.s = c.system_id AND x.t = c.tgid)))",
			[]any{i32(1, 1), i32(5001, 5002)}},
		{"systems only", restricted(Restriction{Systems: []int{1, 3}}), 1,
			" AND ((" + tgNotZero + " AND c.system_id = ANY($1::int[])))",
			[]any{i32(1, 3)}},
		{"talkgroups only", restricted(Restriction{Talkgroups: tgs("2:9178", "2:9179")}), 1,
			" AND ((" + tgNotZero + " AND (c.system_id, c.tgid) IN (SELECT s, t FROM unnest($1::int[], $2::int[]) AS x(s, t))))",
			[]any{i32(2, 2), i32(9178, 9179)}},
		{"systems + talkgroups + exclusions", restricted(Restriction{
			Systems: []int{1, 3}, Talkgroups: tgs("2:9178"), ExcludeTalkgroups: tgs("1:5001")}), 10,
			" AND ((" + tgNotZero + " AND (c.system_id = ANY($10::int[]) OR (c.system_id, c.tgid) IN " +
				"(SELECT s, t FROM unnest($11::int[], $12::int[]) AS x(s, t))) AND " +
				"NOT EXISTS (SELECT 1 FROM unnest($13::int[], $14::int[]) AS x(s, t) WHERE x.s = c.system_id AND x.t = c.tgid)))",
			[]any{i32(1, 3), i32(2), i32(9178), i32(1), i32(5001)}},
		{"key ∩ ticket, numbering continues", restricted(
			Restriction{Systems: []int{1}},
			Restriction{Talkgroups: tgs("1:5"), ExcludeTalkgroups: tgs("1:6")}), 5,
			" AND ((" + tgNotZero + " AND c.system_id = ANY($5::int[])) AND (" + tgNotZero +
				" AND (c.system_id, c.tgid) IN (SELECT s, t FROM unnest($6::int[], $7::int[]) AS x(s, t)) AND " +
				"NOT EXISTS (SELECT 1 FROM unnest($8::int[], $9::int[]) AS x(s, t) WHERE x.s = c.system_id AND x.t = c.tgid)))",
			[]any{i32(1), i32(1), i32(5), i32(1), i32(6)}},
		{"key ∩ allow-nothing ticket", restricted(Restriction{AllowAll: true}, Restriction{}), 1,
			" AND ((" + tgNotZero + ") AND (" + tgNotZero + " AND FALSE))", nil},
		{"out-of-range allow entries match nothing", restricted(Restriction{Systems: []int{0, 1 << 40}, Talkgroups: []TG{{1, 1 << 33}}}), 1,
			" AND ((" + tgNotZero + " AND FALSE))", nil},
		{"out-of-range entries are dropped, not wrapped", restricted(Restriction{
			Systems: []int{1 << 32, 7}, ExcludeTalkgroups: []TG{{1<<32 + 1, 5}}}), 1,
			" AND ((" + tgNotZero + " AND c.system_id = ANY($1::int[])))",
			[]any{i32(7)}},
	}
	for _, tt := range tests {
		clause, args := tt.p.SQL("c.system_id", "c.tgid", tt.firstArg)
		if clause != tt.clause {
			t.Errorf("%s: clause\n got  %q\n want %q", tt.name, clause, tt.clause)
		}
		if !reflect.DeepEqual(args, tt.args) {
			t.Errorf("%s: args = %#v, want %#v", tt.name, args, tt.args)
		}
		for i, a := range args {
			v, ok := a.([]int32)
			if !ok || v == nil || len(v) == 0 {
				t.Errorf("%s: arg %d is %#v, want a non-empty []int32", tt.name, i, a)
			}
		}
		if n := strings.Count(clause, "::int[]"); n != len(args) {
			t.Errorf("%s: %d placeholders for %d args", tt.name, n, len(args))
		}
	}
}

func TestPrincipalSQLUnqualifiedColumns(t *testing.T) {
	p := &Principal{Restrictions: []Restriction{{Systems: []int{4}}}}
	clause, args := p.SQL("system_id", "tgid", 2)
	want := " AND ((tgid > 0 AND system_id > 0 AND system_id = ANY($2::int[])))"
	if clause != want || !reflect.DeepEqual(args, []any{i32(4)}) {
		t.Errorf("SQL = %q, %v; want %q, [[4]]", clause, args, want)
	}
}

func TestPrincipalSQLRejectsBadInput(t *testing.T) {
	p := &Principal{Restrictions: []Restriction{{AllowAll: true}}}
	bad := []struct {
		sys, tg string
		first   int
	}{
		{"c.system_id; DROP TABLE calls", "c.tgid", 1},
		{"c.system_id", "c.tgid OR TRUE", 1},
		{"", "tgid", 1},
		{"system_id", "", 1},
		{"a.b.c", "tgid", 1},
		{"x.system_id", "x.tgid", 1}, // would be shadowed by the unnest alias
		{"X.system_id", "c.tgid", 1},
		{"s", "tgid", 1},
		{"system_id", "T", 1},
		{"system_id", "tgid", 0},
		{"system_id", "tgid", -1},
	}
	for _, b := range bad {
		for _, principal := range []*Principal{p, nil, Internal} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("SQL(%q, %q, %d) did not panic", b.sys, b.tg, b.first)
					}
				}()
				principal.SQL(b.sys, b.tg, b.first)
			}()
		}
	}
}
