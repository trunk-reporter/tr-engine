package database

import (
	"errors"
	"strconv"

	"github.com/snarg/tr-engine/internal/auth"
)

// ErrNoPrincipal is returned by query functions that apply a caller's
// restrictions (§7.2) when they are given a nil principal. Handlers pass the
// request's principal; engine code acting on its own behalf passes
// auth.Internal. A missing principal never means "unrestricted".
var ErrNoPrincipal = errors.New("database: no principal given (internal callers pass auth.Internal)")

// restrictSQL returns p's restriction clause for ANDing into a query's shared
// WHERE, numbered from $firstArg, and its arguments; "" and no arguments for
// an unrestricted p. A nil p is ErrNoPrincipal.
func restrictSQL(p *auth.Principal, sysCol, tgCol string, firstArg int) (string, []any, error) {
	if p == nil {
		return "", nil, ErrNoPrincipal
	}
	clause, args := p.SQL(sysCol, tgCol, firstArg)
	return clause, args, nil
}

// placeholder returns the SQL placeholder "$n".
func placeholder(n int) string {
	return "$" + strconv.Itoa(n)
}

// allowedPatchedTgids returns the talkgroups patched into a call of systemID
// that p may see (§6.3): all of them for an unrestricted p, otherwise only
// those every restriction allows, and nil when none remain (the field is then
// omitted).
func allowedPatchedTgids(p *auth.Principal, systemID int, tgids []int32) []int32 {
	if !p.Restricted() || len(tgids) == 0 {
		return tgids
	}
	var out []int32
	for _, tg := range tgids {
		if p.AllowsTG(systemID, int(tg)) {
			out = append(out, tg)
		}
	}
	return out
}
