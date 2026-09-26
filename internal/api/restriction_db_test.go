package api

// Integration tests of restriction enforcement on the Restricted = Enforced
// REST routes (§6.3, §7.1, §7.2): every route is called through the real
// router (buildRouter) over a real, seeded PostgreSQL with restricted keys,
// a restricted ticket, a restricted anonymous policy and unrestricted keys.
// Skipped unless TEST_DATABASE_URL is set (see integrationDB).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// restrictionLive is the live data the Enforced routes read: active calls
// and the transcription queue. Other LiveDataSource methods are never called
// by these tests (the embedded nil interface would panic if they were).
type restrictionLive struct {
	LiveDataSource
	active   []ActiveCallData
	enqueued []int64
}

func (l *restrictionLive) ActiveCalls() []ActiveCallData { return l.active }

func (l *restrictionLive) EnqueueTranscription(callID int64) bool {
	l.enqueued = append(l.enqueued, callID)
	return true
}

// viewer is one principal the tests call the routes as, with an oracle for
// what it may see, written out independently of internal/auth.
type viewer struct {
	name    string
	key     string                 // "" = anonymous
	allows  func(sys, tg int) bool // (system, talkgroup) pairs it may see
	visible func(sys int) bool     // systems whose metadata it may see
}

// seededCall is a call the fixture created.
type seededCall struct {
	id      int64
	sys, tg int
	group   int
}

func (c seededCall) pair() string { return tgPair(c.sys, c.tg) }

func tgPair(sys, tg int) string { return fmt.Sprintf("%d:%d", sys, tg) }

type restrictionFixture struct {
	db      *database.DB
	r       *chi.Mux
	live    *restrictionLive
	calls   []seededCall
	groups  map[int]string // call group id → pair
	admin   string         // admin key (unrestricted, edit implied)
	adminID int
	allow   string // listen key: systems [2], talkgroups ["1:100"]
	allowID int
	viewers []viewer
}

// The seeded data: three systems, talkgroup 100 in all of them (plain-ID
// ambiguity), a call without a talkgroup (tgid 0), directory entries that
// were never heard, and patched talkgroups on two calls.
var (
	seedTalkgroups = [][2]int{{1, 100}, {1, 200}, {1, 300}, {2, 100}, {2, 500}, {3, 100}, {3, 700}}
	seedDirectory  = [][2]int{{1, 100}, {1, 200}, {1, 999}, {2, 100}, {2, 888}, {3, 700}}
	seedCallPairs  = [][2]int{{1, 100}, {1, 200}, {1, 300}, {1, 0}, {2, 100}, {2, 500}, {3, 100}, {3, 700}}
	seedPatched    = map[string][]int{"1:100": {200, 300}, "2:100": {100, 500}}
)

func newRestrictionFixture(t *testing.T) *restrictionFixture {
	t.Helper()
	db := integrationDB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	exec(`INSERT INTO instances (instance_id) VALUES ('tr-1'), ('tr-2'), ('tr-3')`)
	exec(`INSERT INTO systems (system_id, system_type, name, sysid, wacn) VALUES
		(1, 'p25', 'alpha', '348', 'BEE00'), (2, 'p25', 'bravo', '349', 'BEE01'),
		(3, 'conventional', 'charlie', '0', '0')`)
	exec(`INSERT INTO sites (site_id, system_id, instance_id, short_name) VALUES
		(11, 1, 'tr-1', 'alpha-a'), (12, 2, 'tr-2', 'bravo-a'), (13, 3, 'tr-3', 'charlie-a')`)
	for _, p := range seedTalkgroups {
		exec(`INSERT INTO talkgroups (system_id, tgid, alpha_tag) VALUES ($1, $2, $3)`,
			p[0], p[1], fmt.Sprintf("TG %d:%d", p[0], p[1]))
	}
	for _, p := range seedDirectory {
		exec(`INSERT INTO talkgroup_directory (system_id, tgid, alpha_tag, category) VALUES ($1, $2, $3, 'Fire')`,
			p[0], p[1], fmt.Sprintf("Dir %d:%d", p[0], p[1]))
	}
	// Unit 42 exists in systems 1 and 2; only system 1's transmitted on 1:100.
	exec(`INSERT INTO units (system_id, unit_id, alpha_tag) VALUES (1, 42, 'alpha 42'), (2, 42, 'bravo 42')`)

	audioDir := t.TempDir()
	live := &restrictionLive{}
	f := &restrictionFixture{db: db, live: live, groups: make(map[int]string)}
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	insertCall := func(i, sys, tg, group int) seededCall {
		t.Helper()
		start := base.Add(time.Duration(i) * time.Second)
		file := fmt.Sprintf("call-%d.m4a", i)
		if err := os.WriteFile(filepath.Join(audioDir, file), []byte("audio "+tgPair(sys, tg)), 0o644); err != nil {
			t.Fatal(err)
		}
		var patched []int32
		for _, p := range seedPatched[tgPair(sys, tg)] {
			patched = append(patched, int32(p))
		}
		var id int64
		if err := db.Pool.QueryRow(ctx, `
			INSERT INTO calls (call_group_id, system_id, site_id, tgid, start_time, duration,
				audio_file_path, patched_tgids, unit_ids, system_name, freq_list, src_list)
			VALUES ($1, $2, $3, $4, $5, 2, $6, $7, '{42}', $8,
				'[{"freq": 851000000, "time": 1700000000, "pos": 0, "len": 2}]',
				'[{"src": 42, "time": 1700000000, "pos": 0, "duration": 2}]')
			RETURNING call_id`,
			group, sys, 10+sys, tg, start, file, patched, fmt.Sprintf("system %d", sys),
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO transcriptions (call_id, call_start_time, text, source, is_primary)
			VALUES ($1, $2, $3, 'auto', true)`, id, start, "engine responding on "+tgPair(sys, tg))
		c := seededCall{id: id, sys: sys, tg: tg, group: group}
		f.calls = append(f.calls, c)
		live.active = append(live.active, ActiveCallData{CallID: id, SystemID: sys, Tgid: tg, StartTime: start})
		return c
	}
	for i, p := range seedCallPairs {
		start := base.Add(time.Duration(i) * time.Second)
		var group int
		if err := db.Pool.QueryRow(ctx,
			`INSERT INTO call_groups (system_id, tgid, start_time) VALUES ($1, $2, $3) RETURNING id`,
			p[0], p[1], start).Scan(&group); err != nil {
			t.Fatal(err)
		}
		f.groups[group] = tgPair(p[0], p[1])
		c := insertCall(i, p[0], p[1], group)
		exec(`UPDATE call_groups SET primary_call_id = $1 WHERE id = $2`, c.id, group)
	}
	// An inconsistent member: a 1:200 call in 1:100's group. Member calls
	// of a group must still be filtered one by one.
	insertCall(len(seedCallPairs), 1, 200, f.calls[0].group)

	mkKey := func(name string, scopes auth.Scopes, r *auth.Restriction) *database.APIKeyWithPlaintext {
		t.Helper()
		k, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: name, Scopes: scopes, Restriction: r})
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return k
	}
	tg := func(sys, tg int) auth.TG { return auth.TG{SystemID: sys, Tgid: tg} }
	admin := mkKey("admin", auth.Scopes{auth.ScopeAdmin}, nil)
	listen := mkKey("listen", auth.Scopes{auth.ScopeListen}, nil)
	allow := mkKey("allow list", auth.Scopes{auth.ScopeListen},
		&auth.Restriction{Systems: []int{2}, Talkgroups: []auth.TG{tg(1, 100)}})
	exclude := mkKey("allow all but", auth.Scopes{auth.ScopeListen},
		&auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(1, 100), tg(2, 500)}})
	// Keys can't be created with a restriction that allows nothing; one
	// stored that way (e.g. by an older version) must still see nothing.
	nothing := mkKey("allows nothing", auth.Scopes{auth.ScopeListen}, &auth.Restriction{Systems: []int{3}})
	exec(`UPDATE api_keys SET restriction = '{}' WHERE id = $1`, nothing.ID)
	if _, err := db.SetAnonymousAccess(ctx, database.AccessListen,
		&auth.Restriction{Talkgroups: []auth.TG{tg(3, 700)}}); err != nil {
		t.Fatal(err)
	}

	opts := allFeaturesOptions(newAuthenticator(db, nil, 1e9, 1<<30, zerolog.Nop()))
	opts.DB = db
	opts.Live = live
	cfg := *opts.Config
	cfg.AudioDir = audioDir
	opts.Config = &cfg
	f.r = buildRouter(opts)
	f.admin, f.adminID = admin.Plaintext, admin.ID
	f.allow, f.allowID = allow.Plaintext, allow.ID

	all := func(int, int) bool { return true }
	everySystem := func(int) bool { return true }
	f.viewers = []viewer{
		{name: "admin key", key: admin.Plaintext, allows: all, visible: everySystem},
		{name: "unrestricted listen key", key: listen.Plaintext, allows: all, visible: everySystem},
		{name: "allow-list key", key: allow.Plaintext,
			allows:  func(sys, tg int) bool { return tg != 0 && (sys == 2 || (sys == 1 && tg == 100)) },
			visible: func(sys int) bool { return sys == 1 || sys == 2 }},
		{name: "allow_all+exclude key", key: exclude.Plaintext,
			allows:  func(sys, tg int) bool { return tg != 0 && !(sys == 1 && tg == 100) && !(sys == 2 && tg == 500) },
			visible: everySystem},
		{name: "allow-nothing key", key: nothing.Plaintext,
			allows:  func(int, int) bool { return false },
			visible: func(int) bool { return false }},
		{name: "restricted anonymous", key: "",
			allows:  func(sys, tg int) bool { return sys == 3 && tg == 700 },
			visible: func(sys int) bool { return sys == 3 }},
	}
	return f
}

// get sends a GET through the router, with key as a Bearer credential when
// set.
func (f *restrictionFixture) get(key, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "192.0.2.40:4000"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

// listRow is what the list tests read from a row: its talkgroup pair and
// its ID.
type listRow struct {
	ID           int     `json:"id"`
	CallID       int64   `json:"call_id"`
	SystemID     int     `json:"system_id"`
	Tgid         int     `json:"tgid"`
	PatchedTgids []int   `json:"patched_tgids"`
	Calls        []int64 `json:"-"`
}

// list GETs a list route and returns its rows (under field) and total. It
// fails the test unless the answer is 200.
func (f *restrictionFixture) list(t *testing.T, v viewer, path, field string) ([]listRow, int) {
	t.Helper()
	rec := f.get(v.key, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET %s = %d %s", v.name, path, rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: GET %s: %v", v.name, path, err)
	}
	if string(body[field]) == "null" {
		t.Errorf("%s: GET %s: %q is null, want []", v.name, path, field)
	}
	var rows []listRow
	if err := json.Unmarshal(body[field], &rows); err != nil {
		t.Fatalf("%s: GET %s: rows: %v", v.name, path, err)
	}
	total := -1
	if raw, ok := body["total"]; ok {
		if err := json.Unmarshal(raw, &total); err != nil {
			t.Fatalf("%s: GET %s: total: %v", v.name, path, err)
		}
	}
	return rows, total
}

func pairsOf(rows []listRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, tgPair(r.SystemID, r.Tgid))
	}
	slices.Sort(out)
	return out
}

// expectPairs keeps the pairs v may see, sorted.
func expectPairs(v viewer, pairs [][2]int) []string {
	out := []string{}
	for _, p := range pairs {
		if v.allows(p[0], p[1]) {
			out = append(out, tgPair(p[0], p[1]))
		}
	}
	slices.Sort(out)
	return out
}

func (f *restrictionFixture) callPairs() [][2]int {
	out := make([][2]int, len(f.calls))
	for i, c := range f.calls {
		out[i] = [2]int{c.sys, c.tg}
	}
	return out
}

// checkList checks a paginated list route for every viewer: the rows are
// exactly the allowed ones, the total counts exactly them (also when only
// one row is fetched, so the count query is restricted too), and a user
// filter that selects only forbidden data yields nothing rather than
// everything.
func (f *restrictionFixture) checkList(t *testing.T, path, field string, seeded [][2]int, forbiddenFilter string) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for _, v := range f.viewers {
		want := expectPairs(v, seeded)
		rows, total := f.list(t, v, path+sep+"limit=1000", field)
		if got := pairsOf(rows); !slices.Equal(got, want) {
			t.Errorf("%s: GET %s rows = %v, want %v", v.name, path, got, want)
		}
		if total != len(want) {
			t.Errorf("%s: GET %s total = %d, want %d", v.name, path, total, len(want))
		}
		rows, total = f.list(t, v, path+sep+"limit=1", field)
		if total != len(want) || len(rows) > 1 || (len(want) > 0) != (len(rows) == 1) {
			t.Errorf("%s: GET %s limit=1: %d rows, total %d; want total %d", v.name, path, len(rows), total, len(want))
		}
	}
	if forbiddenFilter != "" {
		allowList := f.viewers[2]
		rows, total := f.list(t, allowList, path+sep+forbiddenFilter, field)
		if len(rows) != 0 || total != 0 {
			t.Errorf("%s: GET %s%s%s = %v (total %d), want nothing: a user filter must never widen the restriction",
				allowList.name, path, sep, forbiddenFilter, pairsOf(rows), total)
		}
	}
}

func TestIntegrationRestrictedRoutes(t *testing.T) {
	f := newRestrictionFixture(t)

	t.Run("systems", func(t *testing.T) {
		for _, v := range f.viewers {
			rows, total := f.list(t, v, "/api/v1/systems", "systems")
			var got, want []int
			for _, r := range rows {
				got = append(got, r.SystemID)
			}
			for sys := 1; sys <= 3; sys++ {
				if v.visible(sys) {
					want = append(want, sys)
				}
				wantCode := http.StatusNotFound
				if v.visible(sys) {
					wantCode = http.StatusOK
				}
				if rec := f.get(v.key, fmt.Sprintf("/api/v1/systems/%d", sys)); rec.Code != wantCode {
					t.Errorf("%s: GET /systems/%d = %d, want %d", v.name, sys, rec.Code, wantCode)
				}
				if rec := f.get(v.key, fmt.Sprintf("/api/v1/sites/%d", 10+sys)); rec.Code != wantCode {
					t.Errorf("%s: GET /sites/%d = %d, want %d", v.name, 10+sys, rec.Code, wantCode)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || total != len(want) {
				t.Errorf("%s: GET /systems = %v (total %d), want %v", v.name, got, total, want)
			}
		}
		if rec := f.get(f.admin, "/api/v1/systems/99"); rec.Code != http.StatusNotFound {
			t.Errorf("missing system: %d", rec.Code)
		}
	})

	t.Run("talkgroups list", func(t *testing.T) {
		f.checkList(t, "/api/v1/talkgroups", "talkgroups", seedTalkgroups, "system_id=3")
		f.checkList(t, "/api/v1/talkgroups?search=TG", "talkgroups", seedTalkgroups, "sysid=0")
	})

	t.Run("talkgroup by id", func(t *testing.T) {
		for _, v := range f.viewers {
			for _, p := range seedTalkgroups {
				want := http.StatusNotFound
				if v.allows(p[0], p[1]) {
					want = http.StatusOK
				}
				for _, id := range []string{tgPair(p[0], p[1]), fmt.Sprintf("%d-%d", p[0], p[1])} {
					if rec := f.get(v.key, "/api/v1/talkgroups/"+id); rec.Code != want {
						t.Errorf("%s: GET /talkgroups/%s = %d, want %d", v.name, id, rec.Code, want)
					}
				}
			}
		}
		// A plain ID resolves among allowed talkgroups only; a 409 names only
		// systems whose talkgroup the caller may see.
		cases := []struct {
			viewer   int
			id       string
			status   int
			resolved int   // system the 200 answer is for
			matches  []int // systems a 409 lists
		}{
			{viewer: 0, id: "100", status: http.StatusConflict, matches: []int{1, 2, 3}},
			{viewer: 2, id: "100", status: http.StatusConflict, matches: []int{1, 2}},
			{viewer: 3, id: "100", status: http.StatusConflict, matches: []int{2, 3}},
			{viewer: 5, id: "100", status: http.StatusNotFound},
			{viewer: 4, id: "100", status: http.StatusNotFound},
			{viewer: 2, id: "500", status: http.StatusOK, resolved: 2},
			{viewer: 3, id: "500", status: http.StatusNotFound},
			{viewer: 3, id: "700", status: http.StatusOK, resolved: 3},
			{viewer: 2, id: "700", status: http.StatusNotFound},
			{viewer: 5, id: "700", status: http.StatusOK, resolved: 3},
		}
		for _, c := range cases {
			v := f.viewers[c.viewer]
			rec := f.get(v.key, "/api/v1/talkgroups/"+c.id)
			if rec.Code != c.status {
				t.Errorf("%s: GET /talkgroups/%s = %d %s, want %d", v.name, c.id, rec.Code, rec.Body.String(), c.status)
				continue
			}
			switch c.status {
			case http.StatusConflict:
				var body struct {
					Matches []struct {
						SystemID int `json:"system_id"`
					} `json:"matches"`
				}
				json.Unmarshal(rec.Body.Bytes(), &body)
				var got []int
				for _, m := range body.Matches {
					got = append(got, m.SystemID)
				}
				slices.Sort(got)
				if !slices.Equal(got, c.matches) {
					t.Errorf("%s: GET /talkgroups/%s: 409 lists systems %v, want %v", v.name, c.id, got, c.matches)
				}
			case http.StatusOK:
				var row listRow
				json.Unmarshal(rec.Body.Bytes(), &row)
				if row.SystemID != c.resolved {
					t.Errorf("%s: GET /talkgroups/%s resolved to system %d, want %d", v.name, c.id, row.SystemID, c.resolved)
				}
			}
		}
	})

	t.Run("talkgroup calls", func(t *testing.T) {
		for _, v := range f.viewers {
			for _, p := range seedTalkgroups {
				path := "/api/v1/talkgroups/" + tgPair(p[0], p[1]) + "/calls"
				if !v.allows(p[0], p[1]) {
					if rec := f.get(v.key, path); rec.Code != http.StatusNotFound {
						t.Errorf("%s: GET %s = %d, want 404", v.name, path, rec.Code)
					}
					continue
				}
				var want [][2]int
				for _, c := range f.calls {
					if c.sys == p[0] && c.tg == p[1] {
						want = append(want, [2]int{c.sys, c.tg})
					}
				}
				rows, total := f.list(t, v, path, "calls")
				if got := pairsOf(rows); !slices.Equal(got, expectPairs(v, want)) || total != len(want) {
					t.Errorf("%s: GET %s = %v (total %d), want %d calls", v.name, path, got, total, len(want))
				}
			}
		}
		// Plain 100 for the allow_all+exclude key is ambiguous between 2 and 3;
		// plain 500 resolves to the allowed 2:500 for the allow-list key.
		rows, total := f.list(t, f.viewers[2], "/api/v1/talkgroups/500/calls", "calls")
		if got := pairsOf(rows); !slices.Equal(got, []string{"2:500"}) || total != 1 {
			t.Errorf("allow-list key: GET /talkgroups/500/calls = %v (total %d)", got, total)
		}
	})

	t.Run("talkgroup directory", func(t *testing.T) {
		f.checkList(t, "/api/v1/talkgroup-directory", "talkgroups", seedDirectory, "system_id=3")
		f.checkList(t, "/api/v1/talkgroup-directory?category=Fire", "talkgroups", seedDirectory, "system_id=3")
		// No match is [] (the list helper fails on null).
		f.list(t, f.viewers[0], "/api/v1/talkgroup-directory?search=zzzznothing", "talkgroups")
	})

	t.Run("calls list", func(t *testing.T) {
		f.checkList(t, "/api/v1/calls", "calls", f.callPairs(), "system_id=3")
		f.checkList(t, "/api/v1/calls?units=42", "calls", f.callPairs(), "tgids=200,300")
		f.checkList(t, "/api/v1/calls?sort=tgid", "calls", f.callPairs(), "site_id=13")
		// patched_tgids keeps only allowed talkgroups.
		for _, v := range f.viewers {
			rows, _ := f.list(t, v, "/api/v1/calls?limit=1000", "calls")
			checkPatched(t, v, rows)
		}
	})

	t.Run("active calls", func(t *testing.T) {
		for _, v := range f.viewers {
			want := expectPairs(v, f.callPairs())
			for _, query := range []string{"", "?emergency=false"} {
				rows, total := f.list(t, v, "/api/v1/calls/active"+query, "calls")
				if got := pairsOf(rows); !slices.Equal(got, want) || total != len(want) {
					t.Errorf("%s: GET /calls/active%s = %v (total %d), want %v", v.name, query, got, total, want)
				}
			}
		}
		rows, total := f.list(t, f.viewers[2], "/api/v1/calls/active?tgid=700", "calls")
		if len(rows) != 0 || total != 0 {
			t.Errorf("allow-list key: active calls on tgid 700 = %v", pairsOf(rows))
		}
	})

	t.Run("single call routes", func(t *testing.T) {
		routes := []string{"", "/audio", "/frequencies", "/transmissions", "/transcription", "/transcriptions"}
		for _, v := range f.viewers {
			for _, c := range f.calls {
				want := http.StatusNotFound
				if v.allows(c.sys, c.tg) {
					want = http.StatusOK
				}
				for _, route := range routes {
					path := fmt.Sprintf("/api/v1/calls/%d%s", c.id, route)
					rec := f.get(v.key, path)
					if rec.Code != want {
						t.Errorf("%s: GET %s (%s) = %d %s, want %d", v.name, path, c.pair(), rec.Code, rec.Body.String(), want)
						continue
					}
					if want == http.StatusNotFound && errorCode(rec) != string(ErrNotFound) {
						t.Errorf("%s: GET %s: code %q, want not_found", v.name, path, errorCode(rec))
					}
					if route == "/audio" && want == http.StatusOK && rec.Body.String() != "audio "+c.pair() {
						t.Errorf("%s: GET %s served %q", v.name, path, rec.Body.String())
					}
					if route == "" && want == http.StatusOK {
						var row listRow
						json.Unmarshal(rec.Body.Bytes(), &row)
						checkPatched(t, v, []listRow{row})
					}
				}
			}
			for _, route := range routes {
				path := "/api/v1/calls/999999" + route
				if rec := f.get(v.key, path); rec.Code != http.StatusNotFound {
					t.Errorf("%s: GET %s (no such call) = %d, want 404", v.name, path, rec.Code)
				}
			}
		}
		// Bad pagination is a 400, no longer ignored.
		for _, route := range []string{"/frequencies", "/transmissions"} {
			for _, q := range []string{"limit=abc", "offset=-1", "limit=0"} {
				path := fmt.Sprintf("/api/v1/calls/%d%s?%s", f.calls[0].id, route, q)
				if rec := f.get(f.admin, path); rec.Code != http.StatusBadRequest || errorCode(rec) != string(ErrInvalidParameter) {
					t.Errorf("GET %s = %d %s, want 400 invalid_parameter", path, rec.Code, rec.Body.String())
				}
			}
			path := fmt.Sprintf("/api/v1/calls/%d%s?offset=5", f.calls[0].id, route)
			if rec := f.get(f.admin, path); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":1`) {
				t.Errorf("GET %s = %d %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("call audio with tickets", func(t *testing.T) {
		// Tickets of the unrestricted key come from POST /tickets. Those of
		// the restricted key are signed here with the engine's secret, so this
		// test doesn't depend on the tickets route's own restriction policy;
		// either way the key's current restriction is applied at
		// verification, intersected with the narrowing.
		mint := func(narrowing string) string {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets", strings.NewReader(narrowing))
			req.RemoteAddr = "192.0.2.40:4000"
			req.Header.Set("Authorization", "Bearer "+f.admin)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			f.r.ServeHTTP(rec, req)
			var tk ticketResponse
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tk) != nil {
				t.Fatalf("mint %s: %d %s", narrowing, rec.Code, rec.Body.String())
			}
			return tk.Ticket
		}
		secret, err := f.db.GetOrCreateTicketSecret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sign := func(keyID int, narrowing *auth.Restriction) string {
			t.Helper()
			tk, err := auth.SignTicket(secret, auth.TicketPayload{KeyID: keyID, ExpiresAt: time.Now().Add(10 * time.Minute), Narrowing: narrowing})
			if err != nil {
				t.Fatal(err)
			}
			return tk
		}
		tickets := []struct {
			name   string
			ticket string
			allows func(sys, tg int) bool
		}{
			{"unrestricted key, no narrowing", mint(`{}`), func(int, int) bool { return true }},
			{"unrestricted key narrowed to 3:700", mint(`{"restriction":{"talkgroups":["3:700"]}}`),
				func(sys, tg int) bool { return sys == 3 && tg == 700 }},
			{"unrestricted key narrowed to nothing", mint(`{"restriction":{}}`),
				func(int, int) bool { return false }},
			{"unrestricted key narrowed to nothing (signed)", sign(f.adminID, &auth.Restriction{}),
				func(int, int) bool { return false }},
			{"allow-list key, no narrowing", sign(f.allowID, nil),
				func(sys, tg int) bool { return tg != 0 && (sys == 2 || (sys == 1 && tg == 100)) }},
			{"allow-list key narrowed to system 1", sign(f.allowID, &auth.Restriction{Systems: []int{1}}),
				func(sys, tg int) bool { return sys == 1 && tg == 100 }},
			{"allow-list key narrowed to system 3 (empty intersection)", sign(f.allowID, &auth.Restriction{Systems: []int{3}}),
				func(int, int) bool { return false }},
			{"allow-list key narrowed with an exclusion", sign(f.allowID, &auth.Restriction{AllowAll: true,
				ExcludeTalkgroups: []auth.TG{{SystemID: 2, Tgid: 100}}}),
				func(sys, tg int) bool { return (sys == 2 && tg != 0 && tg != 100) || (sys == 1 && tg == 100) }},
		}
		for _, tk := range tickets {
			for _, c := range f.calls {
				want := http.StatusNotFound
				if tk.allows(c.sys, c.tg) {
					want = http.StatusOK
				}
				path := fmt.Sprintf("/api/v1/calls/%d/audio?ticket=%s", c.id, tk.ticket)
				// The allow-list key's header must not matter: the ticket wins.
				if rec := f.get(f.admin, path); rec.Code != want {
					t.Errorf("%s: audio of %s = %d, want %d", tk.name, c.pair(), rec.Code, want)
				}
			}
		}
	})

	t.Run("call groups", func(t *testing.T) {
		var groupPairs [][2]int
		for _, p := range seedCallPairs {
			groupPairs = append(groupPairs, p)
		}
		f.checkList(t, "/api/v1/call-groups", "call_groups", groupPairs, "tgid=200")
		for _, v := range f.viewers {
			for id, pair := range f.groups {
				var sys, tg int
				fmt.Sscanf(pair, "%d:%d", &sys, &tg)
				rec := f.get(v.key, fmt.Sprintf("/api/v1/call-groups/%d", id))
				if !v.allows(sys, tg) {
					if rec.Code != http.StatusNotFound {
						t.Errorf("%s: GET /call-groups/%d (%s) = %d, want 404", v.name, id, pair, rec.Code)
					}
					continue
				}
				if rec.Code != http.StatusOK {
					t.Errorf("%s: GET /call-groups/%d (%s) = %d, want 200", v.name, id, pair, rec.Code)
					continue
				}
				var body struct {
					Group listRow   `json:"call_group"`
					Calls []listRow `json:"calls"`
				}
				json.Unmarshal(rec.Body.Bytes(), &body)
				var want [][2]int
				for _, c := range f.calls {
					if c.group == id {
						want = append(want, [2]int{c.sys, c.tg})
					}
				}
				if got := pairsOf(body.Calls); !slices.Equal(got, expectPairs(v, want)) {
					t.Errorf("%s: GET /call-groups/%d member calls %v, want %v", v.name, id, got, expectPairs(v, want))
				}
				checkPatched(t, v, body.Calls)
			}
		}
	})

	t.Run("transcription search", func(t *testing.T) {
		f.checkList(t, "/api/v1/transcriptions/search?q=engine", "results", f.callPairs(), "system_id=3")
		f.checkList(t, "/api/v1/transcriptions/search?q=engine&primary_only=false", "results", f.callPairs(), "tgid=200")
	})

	t.Run("transcription batch", func(t *testing.T) {
		ids := make([]string, 0, len(f.calls)+1)
		byID := make(map[int64]seededCall)
		for _, c := range f.calls {
			ids = append(ids, fmt.Sprint(c.id))
			byID[c.id] = c
		}
		ids = append(ids, "999999")
		for _, v := range f.viewers {
			rows, _ := f.list(t, v, "/api/v1/transcriptions/batch?call_ids="+strings.Join(ids, ","), "transcriptions")
			var got, want []string
			for _, r := range rows {
				c := byID[r.CallID]
				got = append(got, c.pair())
			}
			for _, c := range f.calls {
				if v.allows(c.sys, c.tg) {
					want = append(want, c.pair())
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: batch transcriptions for %v, want %v", v.name, got, want)
			}
		}
	})
}

// checkPatched checks that every patched talkgroup v gets is one it may see,
// and that unrestricted viewers get all of them.
func checkPatched(t *testing.T, v viewer, rows []listRow) {
	t.Helper()
	for _, r := range rows {
		var want []int
		for _, tg := range seedPatched[tgPair(r.SystemID, r.Tgid)] {
			if v.allows(r.SystemID, tg) {
				want = append(want, tg)
			}
		}
		if r.CallID != 0 && !slices.Equal(r.PatchedTgids, want) {
			t.Errorf("%s: call %d (%d:%d) patched_tgids = %v, want %v",
				v.name, r.CallID, r.SystemID, r.Tgid, r.PatchedTgids, want)
		}
	}
}

// TestIntegrationEnforcedRouteFixes covers the §15 bugs fixed in the
// Enforced route families' handlers.
func TestIntegrationEnforcedRouteFixes(t *testing.T) {
	f := newRestrictionFixture(t)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.40:4000"
		req.Header.Set("Authorization", "Bearer "+f.admin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.r.ServeHTTP(rec, req)
		return rec
	}
	call := f.calls[0] // 1:100, unit 42 of system 1

	t.Run("talkgroup units stay in the talkgroup's system", func(t *testing.T) {
		rec := f.get(f.admin, "/api/v1/talkgroups/1:100/units")
		var body struct {
			Units []struct {
				SystemID int `json:"system_id"`
				UnitID   int `json:"unit_id"`
			} `json:"units"`
			Total int `json:"total"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("GET units = %d %s", rec.Code, rec.Body.String())
		}
		if len(body.Units) != 1 || body.Units[0].SystemID != 1 || body.Units[0].UnitID != 42 || body.Total != 1 {
			t.Errorf("units of 1:100 = %+v (total %d), want only system 1's unit 42", body.Units, body.Total)
		}
	})

	t.Run("correction with an invalid source is 400", func(t *testing.T) {
		path := fmt.Sprintf("/api/v1/calls/%d/transcription", call.id)
		rec := send(http.MethodPut, path, `{"text":"corrected","source":"robot"}`)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != string(ErrInvalidBody) {
			t.Errorf("PUT source=robot = %d %s, want 400 invalid_body", rec.Code, rec.Body.String())
		}
		for _, source := range []string{"", "human", "llm", "auto"} {
			rec := send(http.MethodPut, path, `{"text":"corrected","source":"`+source+`"}`)
			if rec.Code != http.StatusOK {
				t.Errorf("PUT source=%q = %d %s", source, rec.Code, rec.Body.String())
			}
		}
		if rec := send(http.MethodPut, "/api/v1/calls/999999/transcription", `{"text":"x"}`); rec.Code != http.StatusNotFound {
			t.Errorf("PUT for a missing call = %d", rec.Code)
		}
	})

	t.Run("transcribing a missing call is 404", func(t *testing.T) {
		if rec := send(http.MethodPost, "/api/v1/calls/999999/transcribe", ""); rec.Code != http.StatusNotFound {
			t.Errorf("POST transcribe for a missing call = %d %s, want 404", rec.Code, rec.Body.String())
		}
		if len(f.live.enqueued) != 0 {
			t.Errorf("a missing call was enqueued: %v", f.live.enqueued)
		}
		if rec := send(http.MethodPost, fmt.Sprintf("/api/v1/calls/%d/transcribe", call.id), ""); rec.Code != http.StatusAccepted {
			t.Errorf("POST transcribe = %d %s, want 202", rec.Code, rec.Body.String())
		}
		if !slices.Equal(f.live.enqueued, []int64{call.id}) {
			t.Errorf("enqueued = %v", f.live.enqueued)
		}
	})

	t.Run("restricted keys stay out of edit routes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/calls/%d/transcribe", call.id), nil)
		req.RemoteAddr = "192.0.2.40:4000"
		req.Header.Set("Authorization", "Bearer "+f.allow)
		rec := httptest.NewRecorder()
		f.r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("restricted listen key on an edit route = %d", rec.Code)
		}
	})
}
