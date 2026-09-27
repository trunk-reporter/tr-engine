package unittags

// End-to-end tests against a real PostgreSQL. Skipped unless TEST_DATABASE_URL
// points at a server where the user may CREATE DATABASE; each run creates a
// throwaway database (tr_engine_test_*), loads schema.sql + migrations, and
// drops it afterwards. Example:
//
//	TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/postgres go test ./internal/unittags/

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/database/sqlcdb"
	"github.com/snarg/tr-engine/internal/transcribe"
)

func integrationDB(t *testing.T) *database.DB {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := fmt.Sprintf("tr_engine_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		admin.Close()
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := database.Connect(ctx, u.String(), zerolog.Nop())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(db.Close)

	schema, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InitSchema(ctx, schema); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	for i := 0; i < 2; i++ { // second pass proves the migrations are idempotent
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate pass %d: %v", i+1, err)
		}
	}
	return db
}

type fixture struct {
	t    *testing.T
	db   *database.DB
	base time.Time
	n    int
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// call inserts a call where each utterance is one transmission, and a
// transcription with word-level attribution built the same way the
// transcription worker builds it. Returns (call_id, transcription_id).
func (f *fixture) call(systemID, tgid int, tgTag string, utts ...Utterance) (int64, int64) {
	f.t.Helper()
	f.n++
	return f.callAt(systemID, tgid, tgTag, f.base.Add(time.Duration(f.n)*time.Minute), 0, utts...)
}

// callAt is call with an explicit start time and call group (0 = none), e.g.
// to insert each site's recording of one transmission on a multi-site system.
func (f *fixture) callAt(systemID, tgid int, tgTag string, start time.Time, groupID int, utts ...Utterance) (int64, int64) {
	f.t.Helper()
	ctx := context.Background()

	var src []map[string]any
	var words []transcribe.Word
	var text string
	pos := 0.0
	for _, u := range utts {
		src = append(src, map[string]any{"src": u.Src, "tag": u.SrcTag, "pos": pos})
		for i, w := range splitWords(u.Text) {
			words = append(words, transcribe.Word{Word: w, Start: pos + float64(i)*0.3, End: pos + float64(i)*0.3 + 0.25})
		}
		if text != "" {
			text += " "
		}
		text += u.Text
		pos += 5
	}
	srcJSON, _ := json.Marshal(src)

	var group *int
	if groupID > 0 {
		group = &groupID
	}
	var callID int64
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO calls (system_id, tgid, tg_alpha_tag, start_time, duration, src_list, call_group_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING call_id`,
		systemID, tgid, tgTag, start, pos, srcJSON, group).Scan(&callID); err != nil {
		f.t.Fatalf("insert call: %v", err)
	}
	return callID, f.transcribe(callID, start, text, words, srcJSON, pos)
}

// transcribe inserts another transcription for an existing call.
func (f *fixture) transcribe(callID int64, start time.Time, text string, words []transcribe.Word, srcJSON []byte, dur float64) int64 {
	f.t.Helper()
	return f.transcribeAs("auto", callID, start, text, words, srcJSON, dur)
}

// transcribeAs inserts a primary transcription from the given source ("auto",
// or "human" for a correction), superseding the call's previous one.
func (f *fixture) transcribeAs(source string, callID int64, start time.Time, text string, words []transcribe.Word, srcJSON []byte, dur float64) int64 {
	f.t.Helper()
	tw := transcribe.AttributeWords(words, transcribe.ParseSrcList(srcJSON, dur), text)
	wordsJSON, _ := json.Marshal(tw)
	id, err := f.db.InsertTranscription(context.Background(), &database.TranscriptionRow{
		CallID: callID, CallStartTime: start, Text: text, Source: source, IsPrimary: true,
		WordCount: len(words), Words: wordsJSON,
	})
	if err != nil {
		f.t.Fatalf("insert transcription: %v", err)
	}
	// Age the row past the scanner's settle delay.
	f.exec(`UPDATE transcriptions SET created_at = now() - interval '1 hour' WHERE id = $1`, id)
	return int64(id)
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// timedWords gives each word of text a 0.3s slot from the start of the call.
func timedWords(text string) []transcribe.Word {
	var words []transcribe.Word
	for i, w := range splitWords(text) {
		words = append(words, transcribe.Word{Word: w, Start: float64(i) * 0.3, End: float64(i)*0.3 + 0.25})
	}
	return words
}

// mergeBeforeApply is the real store with a hook that runs just before the
// scanner writes a batch, to interleave a system merge between the batch's
// fetch and its write.
type mergeBeforeApply struct {
	*database.DB
	before func()
}

func (m *mergeBeforeApply) ApplyUnitTagSightings(ctx context.Context, cursor string, from, to int64, s []database.UnitTagSighting, evidenceCap int) (bool, error) {
	if m.before != nil {
		m.before()
		m.before = nil
	}
	return m.DB.ApplyUnitTagSightings(ctx, cursor, from, to, s, evidenceCap)
}

func suggestionsOnSystem(t *testing.T, db *database.DB, systemID int) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM unit_tag_suggestions WHERE system_id = $1`, systemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// mergeLockKey is database.unitTagMergeLockKey.
const mergeLockKey int64 = 0x74725F7574616773

// holdMergeLock opens a transaction holding the unit tag merge lock, as an
// in-flight scanner write or system merge would.
func holdMergeLock(t *testing.T, db *database.DB) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Release on failure too, or closing the pool would wait forever.
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, mergeLockKey); err != nil {
		t.Fatal(err)
	}
	return tx
}

func waitsForLock(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s did not wait for the lock", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func finishesAfterLock(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s still blocked after the lock was released", what)
	}
}

func drain(t *testing.T, s *Scanner) {
	t.Helper()
	for i := 0; i < 100; i++ {
		n, err := s.ScanOnce(context.Background())
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("scanner did not catch up")
}

// waitForClusterXminPast waits until the xmin of a fresh snapshot
// (pg_snapshot_xmin(pg_current_snapshot()), the oldest xid of a transaction
// still running anywhere in the cluster) is past xid, i.e. until every
// transaction that got its xid no later than xid has ended. The scanner settles a row
// inside the in-doubt window only once its writer precedes that xmin, and it
// is cluster-wide on purpose: a write transaction left open on any database,
// such as another test package's running in parallel on the same server,
// holds the scanner back too. A test that expects rows to be scanned after its
// own blocking transaction ends waits here first, so it does not depend on
// what other sessions are doing at that moment.
func waitForClusterXminPast(t *testing.T, db *database.DB, xid int64, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	for {
		var xmin int64
		if err := db.Pool.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint`).Scan(&xmin); err != nil {
			t.Fatalf("read cluster xmin: %v", err)
		}
		if xmin > xid {
			if waited := time.Since(start); waited > 50*time.Millisecond {
				t.Logf("waited %v for other sessions' transactions to pass xid %d", waited.Round(time.Millisecond), xid)
			}
			return
		}
		if time.Since(start) > timeout {
			t.Fatalf("cluster xmin still %d after %v, not past xid %d of the committed rows, so the scanner "+
				"(correctly) still treats them as in doubt. Another session on this PostgreSQL server, probably "+
				"another package's test, has held a write transaction open since before they were written. "+
				"Oldest open transactions: %s", xmin, timeout, xid, openTransactions(db))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// openTransactions describes the oldest transactions holding an xid, for
// waitForClusterXminPast's failure message.
func openTransactions(db *database.DB) string {
	rows, err := db.Pool.Query(context.Background(), `
		SELECT line FROM (
			SELECT age(backend_xid) AS age,
				format('pid %s db %s app %L state %s xid %s open %ss: %L', pid, datname, application_name, state,
					backend_xid, round(extract(epoch FROM now() - xact_start)::numeric, 1),
					regexp_replace(left(query, 160), '\s+', ' ', 'g')) AS line
			FROM pg_stat_activity WHERE backend_xid IS NOT NULL
			UNION ALL
			SELECT age(transaction), format('prepared transaction %L db %s xid %s since %s', gid, database, transaction, prepared)
			FROM pg_prepared_xacts
		) x ORDER BY age DESC LIMIT 5`)
	if err != nil {
		return fmt.Sprintf("(query failed: %v)", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Sprintf("(query failed: %v)", err)
	}
	if len(list) == 0 {
		return "none now"
	}
	return fmt.Sprintf("%q", list)
}

// findSuggestion looks a row up by its natural key, bypassing the review gate
// (like GET /unit-tag-suggestions/{id}).
func findSuggestion(t *testing.T, db *database.DB, systemID, unitID int, key string) *database.UnitTagSuggestionAPI {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := db.Pool.QueryRow(ctx, `SELECT id FROM unit_tag_suggestions WHERE system_id = $1 AND unit_id = $2 AND tag_key = $3`,
		systemID, unitID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.GetUnitTagSuggestion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pendingKeys(t *testing.T, db *database.DB, minCalls int, minShare float64) map[string]bool {
	t.Helper()
	rows, total, err := db.ListUnitTagSuggestions(context.Background(), database.UnitTagSuggestionFilter{
		MinCalls: minCalls, MinShare: minShare, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(rows) {
		t.Errorf("total = %d, rows = %d", total, len(rows))
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[fmt.Sprintf("%d:%d:%s", r.SystemID, r.UnitID, r.TagKey)] = true
	}
	return out
}

func TestIntegrationUnitTagSuggestions(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := &fixture{t: t, db: db, base: time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)}

	f.exec(`INSERT INTO systems (system_id, system_type, name) VALUES (1, 'p25', 'Butler'), (2, 'p25', 'Butler dup')`)
	f.exec(`INSERT INTO units (system_id, unit_id, alpha_tag, alpha_tag_source) VALUES
		(1, 1001, NULL, NULL),
		(1, 1002, 'Engine 71', 'csv'),
		(1, 1003, 'Console 3', 'mqtt'),
		(1, 1004, '1004', 'mqtt'),
		(2, 1001, NULL, NULL)`)

	disp := Utterance{Src: 1003, Text: "Copy, 14:32."}
	// Unit 1001 identifies itself as Medic 12 in three calls; once also names Truck 3.
	c1, _ := f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1001, Text: "County, Medic 12 on scene."}, disp)
	f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1001, Text: "Medic 12 to County, we'll be transporting."}, disp)
	f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1001, Text: "County, uh, Medic 12, show us clear."}, disp)
	f.call(1, 101, "BC Fire Ops", Utterance{Src: 1001, Text: "Truck 3 on scene."})
	// Unit 1002 says its existing tag: counted, never offered.
	for i := 0; i < 3; i++ {
		f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1002, Text: "Dispatch, Engine 71 responding."})
	}
	// The dispatch console echoes four different units, three calls each:
	// every candidate is only 25% of its self-ID evidence.
	for _, u := range []string{"Medic 14", "Engine 9", "Ladder 1", "Rescue 4"} {
		for i := 0; i < 3; i++ {
			f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1003, Text: u + " on scene."})
		}
	}
	// A talkgroup name is never a candidate; neither is a mention by the dispatcher.
	f.call(1, 102, "Rescue 2 Ops", Utterance{Src: 1004, Text: "County, Rescue 2 on scene."})
	f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1003, Text: "Medic 12, respond to 400 Oak Street."})

	// Newest transcription is still settling: the scanner must stop before it.
	_, youngID := f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1004, Text: "County, Squad 5 on scene."})
	f.exec(`UPDATE transcriptions SET created_at = now() WHERE id = $1`, youngID)

	s := NewScanner(db, Options{BatchSize: 4, Log: zerolog.Nop()})
	drain(t, s)
	st, err := db.GetUnitTagScanStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastTranscriptionID != youngID-1 || st.MaxTranscriptionID != youngID || st.UpdatedAt == nil {
		t.Fatalf("scan status = %+v, want cursor %d (stopped before settling row %d)", st, youngID-1, youngID)
	}

	t.Run("accumulates evidence per unit and tag", func(t *testing.T) {
		m12 := findSuggestion(t, db, 1, 1001, "MEDIC 12")
		if m12 == nil {
			t.Fatal("no MEDIC 12 suggestion for unit 1001")
		}
		if m12.ProposedTag != "Medic 12" || m12.CallCount != 3 || m12.Occurrences != 3 || m12.Status != "pending" {
			t.Errorf("MEDIC 12 = %+v", m12)
		}
		if len(m12.Evidence) != 3 || m12.Evidence[2].CallID != c1 || m12.Evidence[0].AudioURL == "" ||
			m12.Evidence[2].Excerpt != "County, Medic 12 on scene." {
			t.Errorf("evidence (newest first) = %+v", m12.Evidence)
		}
		if m12.Share != 0.75 { // 3 of unit 1001's 4 self-ID calls
			t.Errorf("share = %v, want 0.75", m12.Share)
		}
		if e71 := findSuggestion(t, db, 1, 1002, "ENGINE 71"); e71 == nil || !e71.MatchesCurrentTag {
			t.Errorf("ENGINE 71 = %+v, want matches_current_tag", e71)
		}
		if r2 := findSuggestion(t, db, 1, 1004, "RESCUE 2"); r2 != nil {
			t.Errorf("talkgroup name became a candidate: %+v", r2)
		}
		if m := findSuggestion(t, db, 1, 1003, "MEDIC 12"); m != nil {
			t.Errorf("dispatcher mention became a candidate: %+v", m)
		}
	})

	t.Run("review gate: min calls, share, current tag", func(t *testing.T) {
		got := pendingKeys(t, db, 3, 0.3)
		if len(got) != 1 || !got["1:1001:MEDIC 12"] {
			t.Errorf("pending (min 3 calls, 30%%) = %v, want only 1:1001:MEDIC 12", got)
		}
		got = pendingKeys(t, db, 3, 0.2)
		if len(got) != 5 || !got["1:1003:MEDIC 14"] || got["1:1001:TRUCK 3"] || got["1:1002:ENGINE 71"] {
			t.Errorf("pending (min 3 calls, 20%%) = %v, want MEDIC 12 + four console echoes", got)
		}
		if got = pendingKeys(t, db, 4, 0); len(got) != 0 {
			t.Errorf("pending (min 4 calls) = %v, want none", got)
		}
		if got = pendingKeys(t, db, 1, 0); !got["1:1001:TRUCK 3"] || got["1:1002:ENGINE 71"] {
			t.Errorf("pending (min 1 call) = %v, want TRUCK 3 included, ENGINE 71 (current tag) excluded", got)
		}
		all, _, err := db.ListUnitTagSuggestions(ctx, database.UnitTagSuggestionFilter{Status: "all", MinCalls: 3, MinShare: 0.3, Limit: 100})
		if err != nil || len(all) != 1 {
			t.Errorf("status=all with nothing decided = %d rows (err %v), want the 1 gated pending row", len(all), err)
		}
	})

	t.Run("re-transcription of a counted call is not double counted", func(t *testing.T) {
		var start time.Time
		var src []byte
		if err := db.Pool.QueryRow(ctx, `SELECT start_time, src_list FROM calls WHERE call_id = $1`, c1).Scan(&start, &src); err != nil {
			t.Fatal(err)
		}
		text := "County, Medic 12 on scene, command established."
		var words []transcribe.Word
		for i, w := range splitWords(text) {
			words = append(words, transcribe.Word{Word: w, Start: float64(i) * 0.3, End: float64(i)*0.3 + 0.25})
		}
		f.transcribe(c1, start, text, words, src, 10)
		f.exec(`UPDATE transcriptions SET created_at = now() - interval '1 hour' WHERE id = $1`, youngID)
		drain(t, s)
		if m12 := findSuggestion(t, db, 1, 1001, "MEDIC 12"); m12.CallCount != 3 || m12.Occurrences != 3 {
			t.Errorf("after re-transcription: calls=%d occurrences=%d, want 3/3", m12.CallCount, m12.Occurrences)
		}
		if sq := findSuggestion(t, db, 1, 1004, "SQUAD 5"); sq == nil {
			t.Error("settled row was not scanned after aging")
		}
	})

	t.Run("approve applies through the unit write path with provenance", func(t *testing.T) {
		m12 := findSuggestion(t, db, 1, 1001, "MEDIC 12")
		a, err := db.ApproveUnitTagSuggestion(ctx, m12.ID, "", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if a.AppliedTag != "Medic 12" || a.SystemID != 1 || a.UnitID != 1001 {
			t.Errorf("approval = %+v", a)
		}
		u, err := db.GetUnitByComposite(ctx, 1, 1001)
		if err != nil {
			t.Fatal(err)
		}
		if u.AlphaTag != "Medic 12" || u.AlphaTagSource != "manual" {
			t.Errorf("unit = %+v, want Medic 12 / manual", u)
		}
		got, _ := db.GetUnitTagSuggestion(ctx, m12.ID)
		if got.Status != "approved" || *got.AppliedTag != "Medic 12" || got.PreviousTag != nil ||
			*got.DecidedBy != "alice" || got.DecidedAt == nil || !got.MatchesCurrentTag {
			t.Errorf("approved suggestion = %+v", got)
		}

		var se *database.SuggestionStatusError
		if _, err := db.ApproveUnitTagSuggestion(ctx, m12.ID, "Other", ""); !errors.As(err, &se) || se.Status != "approved" {
			t.Errorf("second approve err = %v, want SuggestionStatusError(approved)", err)
		}
		if err := db.DismissUnitTagSuggestion(ctx, m12.ID, ""); !errors.As(err, &se) {
			t.Errorf("dismiss after approve err = %v, want SuggestionStatusError", err)
		}
		if u, _ := db.GetUnitByComposite(ctx, 1, 1001); u.AlphaTag != "Medic 12" {
			t.Errorf("unit changed by rejected calls: %+v", u)
		}
		if _, err := db.ApproveUnitTagSuggestion(ctx, 999999, "", ""); !errors.Is(err, database.ErrSuggestionNotFound) {
			t.Errorf("approve missing err = %v", err)
		}

		// Manual source survives the MQTT upsert path.
		mqttTag, evType := "MQTT TAG", "call"
		if _, err := db.Q.UpsertUnit(ctx, sqlcdb.UpsertUnitParams{SystemID: 1, UnitID: 1001, AlphaTag: &mqttTag,
			EventTime: pgtype.Timestamptz{Time: time.Now(), Valid: true}, EventType: &evType}); err != nil {
			t.Fatal(err)
		}
		if u, _ := db.GetUnitByComposite(ctx, 1, 1001); u.AlphaTag != "Medic 12" || u.RecorderAlphaTag != "MQTT TAG" {
			t.Errorf("manual tag overwritten, or recorder tag not kept beside it: %+v", u)
		}
	})

	t.Run("edit then approve records the previous tag", func(t *testing.T) {
		sq := findSuggestion(t, db, 1, 1004, "SQUAD 5")
		if _, err := db.ApproveUnitTagSuggestion(ctx, sq.ID, "BC Squad 5", "bob"); err != nil {
			t.Fatal(err)
		}
		got, _ := db.GetUnitTagSuggestion(ctx, sq.ID)
		if *got.AppliedTag != "BC Squad 5" || *got.PreviousTag != "1004" || *got.PreviousTagSource != "mqtt" {
			t.Errorf("suggestion = %+v", got)
		}
		if u, _ := db.GetUnitByComposite(ctx, 1, 1004); u.AlphaTag != "BC Squad 5" || u.AlphaTagSource != "manual" {
			t.Errorf("unit = %+v", u)
		}
	})

	t.Run("dismissed stays dismissed while it keeps counting", func(t *testing.T) {
		m14 := findSuggestion(t, db, 1, 1003, "MEDIC 14")
		if err := db.DismissUnitTagSuggestion(ctx, m14.ID, "carol"); err != nil {
			t.Fatal(err)
		}
		if u, _ := db.GetUnitByComposite(ctx, 1, 1003); u.AlphaTag != "Console 3" {
			t.Errorf("dismiss touched the unit: %+v", u)
		}
		f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1003, Text: "Medic 14 on scene."})
		drain(t, s)
		got, _ := db.GetUnitTagSuggestion(ctx, m14.ID)
		if got.Status != "dismissed" || got.CallCount != 4 || *got.DecidedBy != "carol" {
			t.Errorf("after new sighting: %+v", got)
		}
		if pendingKeys(t, db, 1, 0)["1:1003:MEDIC 14"] {
			t.Error("dismissed candidate resurfaced as pending")
		}
		if err := db.DismissUnitTagSuggestion(ctx, m14.ID, ""); err == nil {
			t.Error("second dismiss should fail")
		}
	})

	t.Run("evidence list is capped, newest first", func(t *testing.T) {
		capped := NewScanner(db, Options{EvidenceCap: 2, Log: zerolog.Nop()})
		var last int64
		for i := 0; i < 4; i++ {
			last, _ = f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1004, Text: "County, Brush 7 responding."})
		}
		drain(t, capped)
		b7 := findSuggestion(t, db, 1, 1004, "BRUSH 7")
		if b7.CallCount != 4 || len(b7.Evidence) != 2 || b7.Evidence[0].CallID != last {
			t.Errorf("BRUSH 7 calls=%d evidence=%+v, want 4 calls, 2 newest evidence", b7.CallCount, b7.Evidence)
		}
	})

	t.Run("cursor is shared and resumable", func(t *testing.T) {
		fresh := NewScanner(db, Options{Log: zerolog.Nop()})
		if n, err := fresh.ScanOnce(ctx); err != nil || n != 0 {
			t.Errorf("new scanner after catch-up: n=%d err=%v, want 0", n, err)
		}
		// A stale compare-and-swap must not write.
		st, _ := db.GetUnitTagScanStatus(ctx)
		ok, err := db.ApplyUnitTagSightings(ctx, database.UnitTagSuggestionsCursor, st.LastTranscriptionID-1, st.LastTranscriptionID+5,
			[]database.UnitTagSighting{{SystemID: 1, UnitID: 1001, TagKey: "BOGUS 1", ProposedTag: "Bogus 1", Occurrences: 1, SeenAt: time.Now()}}, 10)
		if err != nil || ok {
			t.Errorf("stale CAS: ok=%v err=%v, want false/nil", ok, err)
		}
		if findSuggestion(t, db, 1, 1001, "BOGUS 1") != nil {
			t.Error("stale CAS wrote a suggestion")
		}
	})

	t.Run("system merge folds suggestions", func(t *testing.T) {
		src := []byte(`[{"src":1001,"pos":0}]`)
		start := f.base.Add(-time.Hour)
		var cid int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO calls (system_id, tgid, start_time, duration, src_list)
			VALUES (2, 100, $1, 5, $2) RETURNING call_id`, start, src).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		ok, err := db.ApplyUnitTagSightings(ctx, "merge-test", 0, 0, nil, 10) // no cursor row: CAS fails
		if err != nil || ok {
			t.Fatalf("CAS on missing cursor: ok=%v err=%v", ok, err)
		}
		if _, err := db.EnsureScanCursor(ctx, "merge-test"); err != nil {
			t.Fatal(err)
		}
		ev := database.UnitTagEvidence{CallID: cid, CallStartTime: start, Excerpt: "County, Medic 12 on scene."}
		if ok, err := db.ApplyUnitTagSightings(ctx, "merge-test", 0, 1, []database.UnitTagSighting{
			{SystemID: 2, UnitID: 1001, TagKey: "MEDIC 12", ProposedTag: "Medic 12", Occurrences: 2, SeenAt: start, Evidence: ev},
			{SystemID: 2, UnitID: 1001, TagKey: "SQUAD 9", ProposedTag: "Squad 9", Occurrences: 1, SeenAt: start, Evidence: ev},
		}, 10); err != nil || !ok {
			t.Fatalf("apply: ok=%v err=%v", ok, err)
		}
		if _, _, _, _, _, _, err := db.MergeSystems(ctx, 2, 1, "test"); err != nil {
			t.Fatalf("merge: %v", err)
		}
		m12 := findSuggestion(t, db, 1, 1001, "MEDIC 12")
		if m12.CallCount != 4 || m12.Occurrences != 5 || m12.Status != "approved" || len(m12.Evidence) != 4 ||
			!m12.FirstSeen.Equal(start) || m12.Evidence[3].CallID != cid {
			t.Errorf("folded MEDIC 12 = calls %d occ %d status %s evidence %d first %v", m12.CallCount, m12.Occurrences,
				m12.Status, len(m12.Evidence), m12.FirstSeen)
		}
		if sq := findSuggestion(t, db, 1, 1001, "SQUAD 9"); sq == nil {
			t.Error("SQUAD 9 was not moved to the target system")
		}
		var left int
		db.Pool.QueryRow(ctx, `SELECT count(*) FROM unit_tag_suggestions WHERE system_id = 2`).Scan(&left)
		if left != 0 {
			t.Errorf("%d suggestions left on the merged source system", left)
		}
	})

	t.Run("multi-site recordings of one transmission count once", func(t *testing.T) {
		start1 := f.base.Add(-2 * time.Hour)
		start2 := start1.Add(time.Minute)
		var g1, g2 int
		for _, g := range []struct {
			id    *int
			start time.Time
		}{{&g1, start1}, {&g2, start2}} {
			if err := db.Pool.QueryRow(ctx, `INSERT INTO call_groups (system_id, tgid, start_time)
				VALUES (1, 100, $1) RETURNING id`, g.start).Scan(g.id); err != nil {
				t.Fatal(err)
			}
		}
		u := Utterance{Src: 1005, Text: "County, Medic 40 on scene."}
		// Two transmissions, each recorded (and transcribed) by two sites.
		f.callAt(1, 100, "BC Fire Dispatch", start1, g1, u)
		f.callAt(1, 100, "BC Fire Dispatch", start1, g1, u)
		f.callAt(1, 100, "BC Fire Dispatch", start2, g2, u)
		f.callAt(1, 100, "BC Fire Dispatch", start2, g2, u)
		drain(t, s)

		m40 := findSuggestion(t, db, 1, 1005, "MEDIC 40")
		if m40 == nil {
			t.Fatal("no MEDIC 40 suggestion")
		}
		if m40.CallCount != 2 || m40.Occurrences != 2 || len(m40.Evidence) != 2 ||
			m40.Evidence[0].CallGroupID != int64(g2) || m40.Evidence[1].CallGroupID != int64(g1) {
			t.Errorf("MEDIC 40 calls=%d occurrences=%d evidence=%+v, want 2 transmissions (groups %d, %d)",
				m40.CallCount, m40.Occurrences, m40.Evidence, g2, g1)
		}
		if pendingKeys(t, db, 3, 0.2)["1:1005:MEDIC 40"] {
			t.Error("two transmissions heard by two sites passed a 3-call gate")
		}
	})

	t.Run("a correction before the scan replaces the superseded transcript", func(t *testing.T) {
		start := f.base.Add(-3 * time.Hour)
		misheard := "County, Medic 21 on scene."
		cid, _ := f.callAt(1, 100, "BC Fire Dispatch", start, 0, Utterance{Src: 1005, Text: misheard})
		var src []byte
		if err := db.Pool.QueryRow(ctx, `SELECT src_list FROM calls WHERE call_id = $1`, cid).Scan(&src); err != nil {
			t.Fatal(err)
		}
		fixed := "County, Medic 12 on scene."
		corrID := f.transcribeAs("human", cid, start, fixed, timedWords(fixed), src, 5)
		drain(t, s)

		if m21 := findSuggestion(t, db, 1, 1005, "MEDIC 21"); m21 != nil {
			t.Errorf("superseded transcript still counted: %+v", m21)
		}
		m12 := findSuggestion(t, db, 1, 1005, "MEDIC 12")
		if m12 == nil || m12.CallCount != 1 || m12.Evidence[0].TranscriptionID != corrID {
			t.Errorf("MEDIC 12 = %+v, want 1 call from correction %d", m12, corrID)
		}
	})

	t.Run("a merge between a batch's fetch and write re-reads the batch", func(t *testing.T) {
		f.exec(`INSERT INTO systems (system_id, system_type, name) VALUES (3, 'p25', 'Warren'), (4, 'p25', 'Warren dup')`)
		u := Utterance{Src: 2001, Text: "County, Medic 30 on scene."}
		for i := 0; i < 3; i++ {
			f.call(4, 200, "WC Fire", u)
		}
		drain(t, s)
		if m := findSuggestion(t, db, 4, 2001, "MEDIC 30"); m == nil || m.CallCount != 3 {
			t.Fatalf("MEDIC 30 on system 4 = %+v, want 3 calls", m)
		}

		f.call(4, 200, "WC Fire", u)
		merged := false
		racy := NewScanner(&mergeBeforeApply{DB: db, before: func() {
			if _, _, _, _, _, _, err := db.MergeSystems(ctx, 4, 3, "test"); err != nil {
				t.Errorf("merge: %v", err)
			}
			merged = true
		}}, Options{Log: zerolog.Nop()})
		if n, err := racy.ScanOnce(ctx); err != nil || n != 0 || !merged {
			t.Fatalf("batch fetched before the merge: n=%d err=%v merged=%v, want 0/nil/true", n, err, merged)
		}
		if n := suggestionsOnSystem(t, db, 4); n != 0 {
			t.Errorf("%d suggestions written to the merged-away system", n)
		}

		drain(t, s) // re-reads the batch, now with the call on system 3
		m30 := findSuggestion(t, db, 3, 2001, "MEDIC 30")
		if m30 == nil || m30.CallCount != 4 || len(m30.Evidence) != 4 {
			t.Errorf("MEDIC 30 on system 3 = %+v, want all 4 calls", m30)
		}
		if n := suggestionsOnSystem(t, db, 4); n != 0 {
			t.Errorf("%d suggestions left on the merged-away system", n)
		}
		if !pendingKeys(t, db, 3, 0.2)["3:2001:MEDIC 30"] {
			t.Error("MEDIC 30 not listed on the merge target")
		}
	})

	t.Run("merges and scanner writes serialize on one lock", func(t *testing.T) {
		f.exec(`INSERT INTO systems (system_id, system_type, name) VALUES (5, 'p25', 'X'), (6, 'p25', 'X dup'), (7, 'p25', 'Y')`)

		hold := func() pgx.Tx { return holdMergeLock(t, db) }
		blocked := func(done <-chan struct{}, what string) { waitsForLock(t, done, what) }
		finished := func(done <-chan struct{}, what string) { finishesAfterLock(t, done, what) }

		// A merge waits for an in-flight scanner write.
		tx := hold()
		done := make(chan struct{})
		var mergeErr error
		go func() {
			_, _, _, _, _, _, mergeErr = db.MergeSystems(ctx, 6, 5, "test")
			close(done)
		}()
		blocked(done, "merge")
		tx.Rollback(ctx)
		finished(done, "merge")
		if mergeErr != nil {
			t.Fatal(mergeErr)
		}

		// A scanner write waits for an in-flight merge, then sees its result.
		from, err := db.EnsureScanCursor(ctx, "lock-test")
		if err != nil {
			t.Fatal(err)
		}
		tx = hold()
		if _, err := tx.Exec(ctx, `UPDATE systems SET deleted_at = now() WHERE system_id = 7`); err != nil {
			t.Fatal(err)
		}
		done = make(chan struct{})
		var applied bool
		var applyErr error
		go func() {
			applied, applyErr = db.ApplyUnitTagSightings(ctx, "lock-test", from, from+1, []database.UnitTagSighting{
				{SystemID: 7, UnitID: 3001, TagKey: "MEDIC 50", ProposedTag: "Medic 50", Occurrences: 1, SeenAt: time.Now(),
					Evidence: database.UnitTagEvidence{CallID: 1, CallStartTime: time.Now()}},
			}, 10)
			close(done)
		}()
		blocked(done, "scanner write")
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		finished(done, "scanner write")
		if applyErr != nil || applied {
			t.Errorf("write after the merge committed: applied=%v err=%v, want false/nil", applied, applyErr)
		}
		if n := suggestionsOnSystem(t, db, 7); n != 0 {
			t.Errorf("%d suggestions written to the merged-away system", n)
		}
		if cur, _ := db.EnsureScanCursor(ctx, "lock-test"); cur != from {
			t.Errorf("cursor = %d, want %d (not advanced, so the batch is re-read)", cur, from)
		}
	})

	t.Run("approve and dismiss wait for an in-flight merge", func(t *testing.T) {
		// Approve locks a suggestion then its unit; a merge locks units then
		// suggestions. Both take the merge lock first, so they cannot deadlock.
		f.exec(`INSERT INTO units (system_id, unit_id) VALUES (1, 1010)`)
		if _, err := db.EnsureScanCursor(ctx, "decide-lock"); err != nil {
			t.Fatal(err)
		}
		ev := database.UnitTagEvidence{CallID: 1, CallStartTime: f.base}
		if ok, err := db.ApplyUnitTagSightings(ctx, "decide-lock", 0, 1, []database.UnitTagSighting{
			{SystemID: 1, UnitID: 1010, TagKey: "MEDIC 60", ProposedTag: "Medic 60", Occurrences: 1, SeenAt: f.base, Evidence: ev},
			{SystemID: 1, UnitID: 1010, TagKey: "MEDIC 61", ProposedTag: "Medic 61", Occurrences: 1, SeenAt: f.base, Evidence: ev},
		}, 10); err != nil || !ok {
			t.Fatalf("apply: ok=%v err=%v", ok, err)
		}
		m60 := findSuggestion(t, db, 1, 1010, "MEDIC 60")
		m61 := findSuggestion(t, db, 1, 1010, "MEDIC 61")

		tx := holdMergeLock(t, db)
		done := make(chan struct{})
		var approveErr error
		go func() {
			_, approveErr = db.ApproveUnitTagSuggestion(ctx, m60.ID, "", "dave")
			close(done)
		}()
		waitsForLock(t, done, "approve")
		tx.Rollback(ctx)
		finishesAfterLock(t, done, "approve")
		if approveErr != nil {
			t.Fatal(approveErr)
		}
		if u, _ := db.GetUnitByComposite(ctx, 1, 1010); u == nil || u.AlphaTag != "Medic 60" {
			t.Errorf("unit after approve = %+v, want Medic 60", u)
		}

		tx = holdMergeLock(t, db)
		done = make(chan struct{})
		var dismissErr error
		go func() {
			dismissErr = db.DismissUnitTagSuggestion(ctx, m61.ID, "dave")
			close(done)
		}()
		waitsForLock(t, done, "dismiss")
		tx.Rollback(ctx)
		finishesAfterLock(t, done, "dismiss")
		if dismissErr != nil {
			t.Fatal(dismissErr)
		}
		if got, _ := db.GetUnitTagSuggestion(ctx, m61.ID); got.Status != "dismissed" || *got.DecidedBy != "dave" {
			t.Errorf("MEDIC 61 = %+v, want dismissed by dave", got)
		}
	})

	t.Run("a merge counts a transmission both systems recorded once", func(t *testing.T) {
		f.exec(`INSERT INTO systems (system_id, system_type, name) VALUES (8, 'p25', 'Hamilton'), (9, 'p25', 'Hamilton dup')`)
		f.exec(`INSERT INTO units (system_id, unit_id) VALUES (8, 1007), (9, 1007), (9, 1008)`)
		start1 := f.base.Add(-4 * time.Hour)
		start2, start3 := start1.Add(time.Minute), start1.Add(2*time.Minute)
		group := func(systemID int, start time.Time) int {
			t.Helper()
			var id int
			if err := db.Pool.QueryRow(ctx, `INSERT INTO call_groups (system_id, tgid, start_time)
				VALUES ($1, 300, $2) RETURNING id`, systemID, start).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		g8a, g8b, g8c := group(8, start1), group(8, start2), group(8, start3)
		g9a, g9b, g9c := group(9, start1), group(9, start2), group(9, start3)

		// Each of two transmissions was recorded on both (not yet merged) systems.
		r4 := Utterance{Src: 1007, Text: "County, Rescue 4 on scene."}
		f.callAt(8, 300, "HC Fire", start1, g8a, r4)
		f.callAt(9, 300, "HC Fire", start1, g9a, r4)
		f.callAt(8, 300, "HC Fire", start2, g8b, r4)
		f.callAt(9, 300, "HC Fire", start2, g9b, r4)
		// Only the duplicate system's recording of a third transmission was transcribed.
		s8 := Utterance{Src: 1008, Text: "County, Squad 8 on scene."}
		f.callAt(9, 300, "HC Fire", start3, g9c, s8)
		drain(t, s)
		for _, sys := range []int{8, 9} {
			if r := findSuggestion(t, db, sys, 1007, "RESCUE 4"); r == nil || r.CallCount != 2 {
				t.Fatalf("RESCUE 4 on system %d = %+v, want 2 calls", sys, r)
			}
		}

		if _, _, _, _, _, _, err := db.MergeSystems(ctx, 9, 8, "test"); err != nil {
			t.Fatalf("merge: %v", err)
		}
		r := findSuggestion(t, db, 8, 1007, "RESCUE 4")
		if r.CallCount != 2 || r.Occurrences != 2 || len(r.Evidence) != 2 {
			t.Errorf("folded RESCUE 4 calls=%d occurrences=%d evidence=%d, want 2/2/2 (one per transmission)",
				r.CallCount, r.Occurrences, len(r.Evidence))
		}
		for _, e := range r.Evidence {
			if e.CallGroupID != int64(g8a) && e.CallGroupID != int64(g8b) {
				t.Errorf("evidence call group %d, want a surviving group (%d or %d)", e.CallGroupID, g8a, g8b)
			}
		}
		if pendingKeys(t, db, 3, 0.2)["8:1007:RESCUE 4"] {
			t.Error("two transmissions passed a 3-call gate after the merge")
		}
		sq := findSuggestion(t, db, 8, 1008, "SQUAD 8")
		if sq == nil || len(sq.Evidence) != 1 || sq.Evidence[0].CallGroupID != int64(g8c) {
			t.Fatalf("moved SQUAD 8 = %+v, want its evidence on surviving group %d", sq, g8c)
		}

		// Another site's recordings of the same transmissions, transcribed after the merge.
		f.callAt(8, 300, "HC Fire", start1, g8a, r4)
		f.callAt(8, 300, "HC Fire", start3, g8c, s8)
		drain(t, s)
		if r := findSuggestion(t, db, 8, 1007, "RESCUE 4"); r.CallCount != 2 {
			t.Errorf("RESCUE 4 after another copy: %d calls, want 2", r.CallCount)
		}
		if r := findSuggestion(t, db, 8, 1008, "SQUAD 8"); r.CallCount != 1 {
			t.Errorf("SQUAD 8 after another copy: %d calls, want 1", r.CallCount)
		}
	})

	t.Run("a unit tag change re-evaluates a stored current-tag match", func(t *testing.T) {
		f.exec(`INSERT INTO units (system_id, unit_id, alpha_tag, alpha_tag_source) VALUES (1, 1011, 'Medic-20', 'csv')`)
		for i := 0; i < 3; i++ {
			f.call(1, 100, "BC Fire Dispatch", Utterance{Src: 1011, Text: "County, Medic 20 on scene."})
		}
		drain(t, s)
		m20 := findSuggestion(t, db, 1, 1011, "MEDIC 20")
		if m20 == nil || !m20.MatchesCurrentTag || pendingKeys(t, db, 3, 0.2)["1:1011:MEDIC 20"] {
			t.Fatalf("MEDIC 20 = %+v, want matching the unit's tag and not listed", m20)
		}
		setTag := func(tag string) {
			t.Helper()
			src := "manual"
			if err := db.UpdateUnitFields(ctx, 1, 1011, &tag, &src); err != nil {
				t.Fatal(err)
			}
		}

		setTag("Engine 7")
		if got, _ := db.GetUnitTagSuggestion(ctx, m20.ID); got.MatchesCurrentTag || got.UnitAlphaTag != "Engine 7" {
			t.Errorf("after retag: matches=%v tag=%q, want false / Engine 7", got.MatchesCurrentTag, got.UnitAlphaTag)
		}
		if !pendingKeys(t, db, 3, 0.2)["1:1011:MEDIC 20"] {
			t.Error("MEDIC 20 still hidden after the unit was retagged Engine 7")
		}
		// A new tag naming the designator hides it again without a new sighting.
		for _, tag := range []string{"BCFD Medic 20", "MEDIC20", "Medic 20 (BC)"} {
			setTag(tag)
			if got, _ := db.GetUnitTagSuggestion(ctx, m20.ID); !got.MatchesCurrentTag {
				t.Errorf("unit tagged %q: MEDIC 20 matches_current_tag = false, want true", tag)
			}
		}
		setTag("Medic 200")
		if got, _ := db.GetUnitTagSuggestion(ctx, m20.ID); got.MatchesCurrentTag {
			t.Error(`unit tagged "Medic 200": MEDIC 20 matches_current_tag = true, want false`)
		}
	})

	t.Run("an open transaction holding a lower id keeps the cursor behind it", func(t *testing.T) {
		drain(t, s)
		bareCall := func(src int, start time.Time) int64 {
			t.Helper()
			var id int64
			if err := db.Pool.QueryRow(ctx, `INSERT INTO calls (system_id, tgid, start_time, duration, src_list)
				VALUES (1, 100, $1, 5, $2) RETURNING call_id`, start, fmt.Sprintf(`[{"src":%d,"pos":0}]`, src)).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		startA := f.base.Add(-5 * time.Hour)
		startB := startA.Add(time.Minute)
		cA, cB := bareCall(1012, startA), bareCall(1013, startB)
		// Both rows are past the settle delay but inside the in-doubt window,
		// so whether they are settled depends on the cluster xmin.
		const insert = `INSERT INTO transcriptions (call_id, call_start_time, text, source, is_primary, created_at)
			VALUES ($1, $2, $3, 'auto', true, now() - interval '5 minutes')
			RETURNING id, pg_current_xact_id()::text::bigint`

		txA, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { txA.Rollback(context.Background()) })
		var idA, idB, xidA, xidB int64
		if err := txA.QueryRow(ctx, insert, cA, startA, "County, Tanker 9 on scene.").Scan(&idA, &xidA); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, insert, cB, startB, "County, Tanker 10 on scene.").Scan(&idB, &xidB); err != nil {
			t.Fatal(err)
		}
		if idB <= idA {
			t.Fatalf("ids %d, %d not in insert order", idA, idB)
		}

		drain(t, s)
		if st, _ := db.GetUnitTagScanStatus(ctx); st.LastTranscriptionID >= idA {
			t.Fatalf("cursor %d passed id %d while its transaction was still open", st.LastTranscriptionID, idA)
		}
		if findSuggestion(t, db, 1, 1013, "TANKER 10") != nil {
			t.Error("a later row was scanned while an older transaction was open")
		}

		if err := txA.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// Other sessions on the server (e.g. test packages running in
		// parallel) may still hold transactions older than these rows, which
		// rightly keeps them in doubt; wait those out.
		waitForClusterXminPast(t, db, max(xidA, xidB), 10*time.Second)
		drain(t, s)
		if findSuggestion(t, db, 1, 1012, "TANKER 9") == nil || findSuggestion(t, db, 1, 1013, "TANKER 10") == nil {
			t.Error("rows not scanned once the open transaction committed")
		}
	})
}
