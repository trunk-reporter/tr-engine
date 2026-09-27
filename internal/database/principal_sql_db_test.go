package database

// auth.Principal.SQL against a real PostgreSQL, with auth.Principal.AllowsTG
// as the oracle. Skipped unless TEST_DATABASE_URL is set (see
// export_units_db_test.go).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/snarg/tr-engine/internal/auth"
)

type sysTG struct {
	sys int
	tg  *int // nil = NULL tgid
}

func (p sysTG) String() string {
	if p.tg == nil {
		return fmt.Sprintf("%d:NULL", p.sys)
	}
	return fmt.Sprintf("%d:%d", p.sys, *p.tg)
}

// sqlGrid is every (system, tgid) row the test queries: systems 1-3, and a
// NULL, a 0 and three real tgids in each.
func sqlGrid() []sysTG {
	var rows []sysTG
	for sys := 1; sys <= 3; sys++ {
		rows = append(rows, sysTG{sys: sys})
		for _, tg := range []int{0, 100, 200, 300} {
			tg := tg
			rows = append(rows, sysTG{sys: sys, tg: &tg})
		}
	}
	return rows
}

// oracle is what p may see of rows: everything when unrestricted, otherwise
// only rows with a tgid that every restriction allows.
func oracle(p *auth.Principal, rows []sysTG) []string {
	var out []string
	for _, r := range rows {
		allowed := p != nil && !p.Restricted()
		if p != nil && p.Restricted() {
			allowed = r.tg != nil && p.AllowsTG(r.sys, *r.tg)
		}
		if allowed {
			out = append(out, r.String())
		}
	}
	slices.Sort(out)
	return out
}

// principalRows runs SELECT over restr_rows with p's clause after one
// placeholder of the query's own, so the clause's numbering starts at $2.
func principalRows(t *testing.T, db *DB, p *auth.Principal) []string {
	t.Helper()
	clause, args := p.SQL("r.system_id", "r.tgid", 2)
	rows, err := db.Pool.Query(context.Background(),
		`SELECT r.system_id, r.tgid FROM restr_rows r WHERE r.system_id > $1`+clause,
		append([]any{0}, args...)...)
	if err != nil {
		t.Fatalf("query with %q: %v", clause, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r sysTG
		if err := rows.Scan(&r.sys, &r.tg); err != nil {
			t.Fatal(err)
		}
		out = append(out, r.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func restricted(rs ...auth.Restriction) *auth.Principal {
	return &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen}, Restrictions: rs}
}

func tgs(pairs ...int) []auth.TG {
	var out []auth.TG
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, auth.TG{SystemID: pairs[i], Tgid: pairs[i+1]})
	}
	return out
}

func TestPrincipalSQL(t *testing.T) {
	db := authDB(t)
	mustExec(t, db, `CREATE TABLE restr_rows (system_id int NOT NULL, tgid int)`)
	grid := sqlGrid()
	for _, r := range grid {
		mustExec(t, db, `INSERT INTO restr_rows VALUES ($1, $2)`, r.sys, r.tg)
	}

	cases := []struct {
		name string
		p    *auth.Principal
		want []string // nil = only compare with the oracle
	}{
		{name: "unrestricted sees everything, NULL and 0 too", p: &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen}}},
		{name: "nil principal sees nothing", p: nil, want: []string{}},
		{name: "allow_all hides NULL and 0", p: restricted(auth.Restriction{AllowAll: true})},
		{name: "allow_all + exclude", p: restricted(auth.Restriction{AllowAll: true, ExcludeTalkgroups: tgs(1, 100, 2, 200)})},
		{name: "systems only", p: restricted(auth.Restriction{Systems: []int{1}}),
			want: []string{"1:100", "1:200", "1:300"}},
		{name: "systems + exclude", p: restricted(auth.Restriction{Systems: []int{1, 3}, ExcludeTalkgroups: tgs(1, 100, 2, 200)})},
		{name: "talkgroups only", p: restricted(auth.Restriction{Talkgroups: tgs(2, 200, 3, 300, 3, 0)}),
			want: []string{"2:200", "3:300"}},
		{name: "systems + talkgroups + exclude", p: restricted(auth.Restriction{Systems: []int{1}, Talkgroups: tgs(2, 200), ExcludeTalkgroups: tgs(1, 300)})},
		{name: "allow nothing", p: restricted(auth.Restriction{}), want: []string{}},
		{name: "allow nothing with exclusions", p: restricted(auth.Restriction{ExcludeTalkgroups: tgs(1, 100)}), want: []string{}},
		{name: "two restrictions intersect",
			p:    restricted(auth.Restriction{Systems: []int{1, 2}}, auth.Restriction{Talkgroups: tgs(1, 100, 2, 200, 3, 300)}),
			want: []string{"1:100", "2:200"}},
		{name: "empty intersection",
			p:    restricted(auth.Restriction{Systems: []int{1}}, auth.Restriction{Systems: []int{2}}),
			want: []string{}},
		{name: "exclusion in one restriction applies to the intersection",
			p:    restricted(auth.Restriction{AllowAll: true, ExcludeTalkgroups: tgs(1, 100)}, auth.Restriction{Systems: []int{1}}),
			want: []string{"1:200", "1:300"}},
		{name: "ticket narrowing to nothing", p: restricted(auth.Restriction{AllowAll: true}, auth.Restriction{}), want: []string{}},
		{name: "out-of-range IDs match nothing",
			p:    restricted(auth.Restriction{Systems: []int{1 << 40}, Talkgroups: tgs(1, 1<<40, 2, 100)}),
			want: []string{"2:100"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := principalRows(t, db, c.p)
			if got == nil {
				got = []string{}
			}
			want := oracle(c.p, grid)
			if want == nil {
				want = []string{}
			}
			if !slices.Equal(got, want) {
				t.Errorf("SQL rows %v, AllowsTG oracle %v", got, want)
			}
			if c.want != nil && !slices.Equal(got, c.want) {
				t.Errorf("SQL rows %v, want %v", got, c.want)
			}
		})
	}
}

// Removing any allowed entry from a restriction, including the last one,
// never lets more rows through.
func TestPrincipalSQL_RemovingEntriesNeverWidens(t *testing.T) {
	db := authDB(t)
	mustExec(t, db, `CREATE TABLE restr_rows (system_id int NOT NULL, tgid int)`)
	for _, r := range sqlGrid() {
		mustExec(t, db, `INSERT INTO restr_rows VALUES ($1, $2)`, r.sys, r.tg)
	}

	bases := []auth.Restriction{
		{Systems: []int{2}},
		{Talkgroups: tgs(1, 100)},
		{Systems: []int{1, 3}, ExcludeTalkgroups: tgs(3, 300)},
		{Systems: []int{1}, Talkgroups: tgs(2, 200, 3, 100), ExcludeTalkgroups: tgs(1, 200)},
	}
	for bi, base := range bases {
		// Remove entries one at a time until the allow lists are empty.
		cur := *base.Normalize()
		prev := principalRows(t, db, restricted(cur))
		for step := 0; len(cur.Systems)+len(cur.Talkgroups) > 0; step++ {
			next := cur
			if len(cur.Talkgroups) > 0 {
				next.Talkgroups = cur.Talkgroups[1:]
			} else {
				next.Systems = cur.Systems[1:]
			}
			got := principalRows(t, db, restricted(next))
			for _, row := range got {
				if !slices.Contains(prev, row) {
					t.Errorf("base %d step %d: removing an entry widened access to %s (%+v → %+v)", bi, step, row, cur, next)
				}
			}
			cur, prev = next, got
		}
		if len(prev) != 0 {
			t.Errorf("base %d: with every allowed entry removed, rows %v are still visible", bi, prev)
		}
	}
}

// The clause works as the shared WHERE of a real list query: rows and the
// count agree, with the clause after the query's own arguments.
func TestPrincipalSQL_CallsCountAndRows(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_id, system_type, name) VALUES (1, 'p25', 'a'), (2, 'p25', 'b')`)
	start := time.Now().Add(-time.Hour)
	for _, c := range [][2]int{{1, 0}, {1, 100}, {1, 200}, {2, 100}, {2, 200}, {2, 200}} {
		mustExec(t, db, `INSERT INTO calls (system_id, tgid, start_time) VALUES ($1, $2, $3)`, c[0], c[1], start)
	}
	p := restricted(auth.Restriction{AllowAll: true, ExcludeTalkgroups: tgs(1, 200)},
		auth.Restriction{Systems: []int{2}, Talkgroups: tgs(1, 100, 1, 200)})

	since := start.Add(-time.Minute)
	clause, args := p.SQL("c.system_id", "c.tgid", 2)
	where := ` WHERE c.start_time >= $1` + clause
	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM calls c`+where, append([]any{since}, args...)...).Scan(&total); err != nil {
		t.Fatal(err)
	}
	// LIMIT/OFFSET placeholders follow the clause's arguments.
	n := 2 + len(args)
	rows, err := db.Pool.Query(ctx, fmt.Sprintf(`SELECT c.system_id, c.tgid FROM calls c%s
		ORDER BY c.system_id, c.tgid LIMIT $%d OFFSET $%d`, where, n, n+1), append(append([]any{since}, args...), 100, 0)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s, tg int
		if err := rows.Scan(&s, &tg); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%d", s, tg))
	}
	want := []string{"1:100", "2:100", "2:200", "2:200"}
	if strings.Join(got, ",") != strings.Join(want, ",") || total != len(want) {
		t.Errorf("rows %v (total %d), want %v (total %d)", got, total, want, len(want))
	}
}
