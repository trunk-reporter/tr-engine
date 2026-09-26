package auth

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind says how a request's principal was established (§3.3).
type Kind string

const (
	KindAnonymous Kind = "anonymous" // no credential; the anonymous access policy applies
	KindKey       Kind = "key"       // an API key
	KindTicket    Kind = "ticket"    // a ?ticket= minted by a key
	KindInternal  Kind = "internal"  // the engine itself (Internal); never from a request
)

// Principal is who a request acts as. Handlers and query functions read it;
// only the auth middleware and the stream re-check build it.
type Principal struct {
	Kind    Kind
	KeyID   int    // key: the key; ticket: the minting key; otherwise 0
	KeyName string // key and ticket: the key's name
	Legacy  bool   // key: imported from a legacy AUTH_TOKEN/WRITE_TOKEN
	Scopes  Scopes // as held, not expanded; use Has to check implications
	// Restrictions all apply: a pair is allowed only if every restriction
	// allows it (key restriction ∩ ticket narrowing). Empty means
	// unrestricted.
	Restrictions []Restriction
	Actor        string    // sanitized X-Actor header; recorded, never used to authorize
	TicketExpiry time.Time // ticket: when the ticket expires
}

// Internal is the unrestricted principal with every scope, for engine code
// that calls principal-taking functions on its own behalf. It must not be
// modified.
var Internal = &Principal{Kind: KindInternal, Scopes: Scopes{ScopeAdmin, ScopeUpload}}

// Has reports whether p holds scope, directly or by implication. A nil p has
// no scopes.
func (p *Principal) Has(scope Scope) bool {
	return p != nil && p.Scopes.Has(scope)
}

// Restricted reports whether any restriction applies to p. A nil p counts as
// restricted, so routes that deny restricted principals deny it too.
func (p *Principal) Restricted() bool {
	return p == nil || len(p.Restrictions) > 0
}

// AllowsTG reports whether every restriction on p allows (sys, tg). An
// unrestricted p allows everything, including data without a talkgroup; a nil
// p allows nothing.
func (p *Principal) AllowsTG(sys, tg int) bool {
	if p == nil {
		return false
	}
	for i := range p.Restrictions {
		if !p.Restrictions[i].AllowsTG(sys, tg) {
			return false
		}
	}
	return true
}

// SystemVisible reports whether every restriction on p lets it see the
// system's metadata. An unrestricted p sees every system; a nil p sees none.
func (p *Principal) SystemVisible(sys int) bool {
	if p == nil {
		return false
	}
	for i := range p.Restrictions {
		if !p.Restrictions[i].SystemVisible(sys) {
			return false
		}
	}
	return true
}

// Attribution names who acted, for records such as
// unit_tag_suggestions.decided_by and system_merge_log.performed_by (§9): the
// key name (or the kind, for principals without a key), plus " / <actor>"
// when the request named one.
func (p *Principal) Attribution() string {
	if p == nil {
		return ""
	}
	name := p.KeyName
	if name == "" {
		name = string(p.Kind)
	}
	if p.Actor != "" {
		name += " / " + p.Actor
	}
	return name
}

// sqlColumn is what SQL accepts as a column reference: a plain or
// alias-qualified identifier. Column names are always code constants; the
// check catches a caller passing anything else.
var sqlColumn = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

func checkSQLColumn(col string) {
	// The generated subqueries alias unnest as x(s, t), which would shadow a
	// column qualified with x or named s or t (unquoted names fold to lower
	// case).
	lc := strings.ToLower(col)
	if !sqlColumn.MatchString(col) || strings.HasPrefix(lc, "x.") || lc == "s" || lc == "t" {
		panic("auth: invalid SQL column reference " + strconv.Quote(col))
	}
}

// SQL returns a clause restricting rows to p's restrictions, for ANDing into a
// query's shared WHERE (so counts and rows agree), and its arguments, which
// are numbered from $firstArg.
//
// For an unrestricted p it returns "" and no arguments. A nil p yields
// " AND FALSE". Otherwise the clause is " AND (...)" with one parenthesized
// term per restriction, all ANDed:
//
//	tgCol IS NOT NULL AND tgCol <> 0
//	AND <allow part>
//	AND NOT EXISTS (SELECT 1 FROM unnest($d::int[], $e::int[]) AS x(s, t)
//	                WHERE x.s = sysCol AND x.t = tgCol)   -- only with exclusions
//
// where the allow part is nothing for AllowAll, FALSE for a restriction that
// allows nothing (the exclusion term is then omitted too), and otherwise
//
//	(sysCol = ANY($a::int[]) OR (sysCol, tgCol) IN
//	    (SELECT s, t FROM unnest($b::int[], $c::int[]) AS x(s, t)))
//
// with whichever side is empty dropped. Every argument is a non-empty,
// non-nil []int32: pgx encodes a nil slice as NULL, which the pqIntArray
// helpers treat as "no filter", so they must never carry restrictions.
// Entries outside 1..2147483647 cannot match an int column and are left out;
// an allow part left with no entries becomes FALSE.
//
// sysCol and tgCol are column references such as "c.system_id"; anything
// else panics, as does a firstArg below 1.
func (p *Principal) SQL(sysCol, tgCol string, firstArg int) (string, []any) {
	checkSQLColumn(sysCol)
	checkSQLColumn(tgCol)
	if firstArg < 1 {
		panic("auth: SQL placeholder numbering must start at 1 or above, got " + strconv.Itoa(firstArg))
	}
	if p == nil {
		return " AND FALSE", nil
	}
	if len(p.Restrictions) == 0 {
		return "", nil
	}

	var args []any
	arg := func(v []int32) string {
		args = append(args, v)
		return "$" + strconv.Itoa(firstArg+len(args)-1) + "::int[]"
	}
	pairIn := func(sysArr, tgArr []int32) string {
		return "unnest(" + arg(sysArr) + ", " + arg(tgArr) + ") AS x(s, t)"
	}

	terms := make([]string, 0, len(p.Restrictions))
	for i := range p.Restrictions {
		r := &p.Restrictions[i]
		conj := []string{tgCol + " IS NOT NULL", tgCol + " <> 0"}

		if !r.AllowAll {
			systems := int32IDs(r.Systems)
			tgSys, tgIDs := int32Pairs(r.Talkgroups)
			var allow []string
			if len(systems) > 0 {
				allow = append(allow, sysCol+" = ANY("+arg(systems)+")")
			}
			if len(tgSys) > 0 {
				allow = append(allow, "("+sysCol+", "+tgCol+") IN (SELECT s, t FROM "+pairIn(tgSys, tgIDs)+")")
			}
			switch len(allow) {
			case 0:
				terms = append(terms, "("+strings.Join(append(conj, "FALSE"), " AND ")+")")
				continue
			case 1:
				conj = append(conj, allow[0])
			default:
				conj = append(conj, "("+strings.Join(allow, " OR ")+")")
			}
		}

		if exSys, exIDs := int32Pairs(r.ExcludeTalkgroups); len(exSys) > 0 {
			conj = append(conj, "NOT EXISTS (SELECT 1 FROM "+pairIn(exSys, exIDs)+
				" WHERE x.s = "+sysCol+" AND x.t = "+tgCol+")")
		}
		terms = append(terms, "("+strings.Join(conj, " AND ")+")")
	}
	return " AND (" + strings.Join(terms, " AND ") + ")", args
}

// int32IDs converts the IDs that an int column can hold; others match
// nothing and are dropped.
func int32IDs(ids []int) []int32 {
	out := make([]int32, 0, len(ids))
	for _, id := range ids {
		if validID(id) {
			out = append(out, int32(id))
		}
	}
	return out
}

// int32Pairs splits talkgroups into parallel system and tgid arrays for
// unnest, dropping entries an int column cannot hold.
func int32Pairs(tgs []TG) (sys, tg []int32) {
	sys = make([]int32, 0, len(tgs))
	tg = make([]int32, 0, len(tgs))
	for _, t := range tgs {
		if t.valid() {
			sys = append(sys, int32(t.SystemID))
			tg = append(tg, int32(t.Tgid))
		}
	}
	return sys, tg
}
