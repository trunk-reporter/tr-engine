package ingest

// HTTP-upload ingest against a real PostgreSQL and a real audio directory:
// an upload can't replace another call's audio, and can't create partitions
// far from now. Skipped unless TEST_DATABASE_URL is set.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/storage"
)

// uploadTestPipeline is a pipeline over a fresh database with its audio
// in a temporary directory.
func uploadTestPipeline(t *testing.T) (*Pipeline, *database.DB, string) {
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	audioDir := t.TempDir()
	p := NewPipeline(PipelineOptions{
		DB:       db,
		AudioDir: audioDir,
		Store:    storage.NewLocalStore(audioDir),
		Log:      zerolog.Nop(),
	})
	t.Cleanup(p.Stop)
	return p, db, audioDir
}

func uploadFields(tg int, shortName string, start time.Time) map[string]string {
	return map[string]string{
		"talkgroup":   strconv.Itoa(tg),
		"systemLabel": shortName,
		"dateTime":    strconv.FormatInt(start.Unix(), 10),
	}
}

func readAudio(t *testing.T, audioDir, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(audioDir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(b)
}

// An upload names neither the directory nor the file its audio is stored
// in, so it can't replace another call's recording: not through a short name
// with "..", and not by reusing another system's short name, date and file
// name (MQTT audio is stored as <short_name>/<date>/<file name>).
func TestUploadCannotReplaceAnotherCallsAudio(t *testing.T) {
	p, db, audioDir := uploadTestPipeline(t)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)

	// Call A, uploaded to system sysA.
	a, err := p.ProcessUpload(ctx, "http-upload", "rdio-scanner", uploadFields(100, "sysA", start), []byte("ORIGINAL-AUDIO"), "x.m4a")
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	wantKey := fmt.Sprintf("upload/%d/%s/%d.m4a", a.SystemID, start.UTC().Format("2006-01-02"), a.CallID)
	if a.AudioFilePath != wantKey {
		t.Fatalf("A stored at %q, want %q", a.AudioFilePath, wantKey)
	}

	// An MQTT recording of a TR system whose short name is sysA.
	mqttKey := buildAudioRelPath("sysA", start, "x.m4a")
	if err := p.store.Save(ctx, mqttKey, []byte("MQTT-AUDIO"), "audio/mp4"); err != nil {
		t.Fatal(err)
	}

	// A traversing short name is refused before anything is created.
	_, err = p.ProcessUpload(ctx, "http-upload", "rdio-scanner", uploadFields(200, "sysB/../sysA", start.Add(5*time.Second)), []byte("TAMPERED"), "x.m4a")
	if !errors.Is(err, api.ErrInvalidUpload) {
		t.Fatalf("traversing short name: err = %v, want ErrInvalidUpload", err)
	}
	var systems int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM systems WHERE name LIKE '%/%'`).Scan(&systems); err != nil || systems != 0 {
		t.Errorf("systems named like a path: %d (err %v)", systems, err)
	}

	// The same short name, date and file name as the MQTT recording and
	// call A, from another instance's system.
	b, err := p.ProcessUpload(ctx, "other-upload", "rdio-scanner", uploadFields(200, "sysA", start), []byte("TAMPERED"), "x.m4a")
	if err != nil {
		t.Fatalf("upload B: %v", err)
	}
	if b.SystemID == a.SystemID {
		t.Fatalf("B landed in A's system %d", a.SystemID)
	}
	if b.AudioFilePath == a.AudioFilePath || b.AudioFilePath == mqttKey {
		t.Fatalf("B stored at %q (A %q, MQTT %q)", b.AudioFilePath, a.AudioFilePath, mqttKey)
	}
	if got := readAudio(t, audioDir, a.AudioFilePath); got != "ORIGINAL-AUDIO" {
		t.Errorf("call A's audio is now %q", got)
	}
	if got := readAudio(t, audioDir, mqttKey); got != "MQTT-AUDIO" {
		t.Errorf("the MQTT recording is now %q", got)
	}
	if got := readAudio(t, audioDir, b.AudioFilePath); got != "TAMPERED" {
		t.Errorf("call B's audio is %q", got)
	}
	var stored string
	if err := db.Pool.QueryRow(ctx, `SELECT audio_file_path FROM calls WHERE call_id = $1`, a.CallID).Scan(&stored); err != nil || stored != a.AudioFilePath {
		t.Errorf("call A's audio_file_path = %q (err %v), want %q", stored, err, a.AudioFilePath)
	}
}

// Upload start times can't create partitions far from now; months that
// are near now still get theirs on demand.
func TestUploadStartTimeBoundsPartitions(t *testing.T) {
	p, db, _ := uploadTestPipeline(t)
	ctx := context.Background()
	partition := func(name string) bool {
		t.Helper()
		var ok bool
		if err := db.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	partitionName := func(tm time.Time) string {
		return fmt.Sprintf("calls_y%04dm%02d", tm.Year(), int(tm.Month()))
	}

	year9999 := time.Date(9999, 1, 15, 12, 0, 0, 0, time.UTC)
	longAgo := time.Now().AddDate(-2, 0, 0)
	for _, c := range []struct {
		name   string
		fields map[string]string
	}{
		{"year 9999", uploadFields(901, "sys", year9999)},
		{"tomorrow", uploadFields(901, "sys", time.Now().Add(24*time.Hour))},
		{"two years ago", uploadFields(901, "sys", longAgo)},
		{"epoch", map[string]string{"talkgroup": "901", "systemLabel": "sys", "dateTime": "0"}},
		{"no start time", map[string]string{"talkgroup": "901", "systemLabel": "sys"}},
	} {
		_, err := p.ProcessUpload(ctx, "http-upload", "rdio-scanner", c.fields, nil, "")
		if !errors.Is(err, api.ErrInvalidUpload) {
			t.Errorf("%s: err = %v, want ErrInvalidUpload", c.name, err)
		}
	}
	for _, name := range []string{"calls_y9999m01", partitionName(longAgo), "calls_y1970m01"} {
		if partition(name) {
			t.Errorf("partition %s was created", name)
		}
	}
	var calls int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 0 {
		t.Errorf("calls = %d (err %v), want 0", calls, err)
	}

	// Two months ago has no partition on a fresh database (the schema
	// creates the current month and three ahead) and gets one.
	recent := time.Now().AddDate(0, -2, 0)
	if partition(partitionName(recent)) {
		t.Fatalf("%s exists before the upload", partitionName(recent))
	}
	if _, err := p.ProcessUpload(ctx, "http-upload", "rdio-scanner", uploadFields(901, "sys", recent), nil, ""); err != nil {
		t.Fatalf("upload two months ago: %v", err)
	}
	if !partition(partitionName(recent)) {
		t.Errorf("%s was not created", partitionName(recent))
	}
}
