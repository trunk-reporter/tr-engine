package ingest

// TEST_DATABASE_URL must allow CREATE DATABASE. Each subtest creates and drops
// an isolated database; tests skip when the variable is unset.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/database"
)

func partitionRetryPipeline(t *testing.T, timezone string) *Pipeline {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("tr_engine_partition_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	// Set every pool connection's timezone, rather than SET on one connection.
	query := u.Query()
	query.Set("timezone", timezone)
	u.RawQuery = query.Encode()
	db, err := database.Connect(ctx, u.String(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	schema, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InitSchema(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return &Pipeline{db: db, identity: NewIdentityResolver(db, zerolog.Nop()), log: zerolog.Nop(), ctx: ctx}
}

func TestPartitionRetryDatabaseTimezone(t *testing.T) {
	cases := []struct {
		name, timezone, month string
		instant               time.Time
	}{
		{
			name: "UTC month starts before western local month", timezone: "UTC", month: "2100_04",
			instant: time.Date(2100, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "western database still in previous DST month", timezone: "America/New_York", month: "2100_03",
			instant: time.Date(2100, 4, 1, 2, 0, 0, 0, time.UTC),
		},
		{
			name: "eastern database already in next year", timezone: "Asia/Tokyo", month: "2101_01",
			instant: time.Date(2100, 12, 31, 18, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		for _, mode := range []string{"on-demand helper", "HTTP upload retry"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				p := partitionRetryPipeline(t, tc.timezone)
				var month string
				if err := p.db.Pool.QueryRow(p.ctx, "SELECT to_char($1::timestamptz, 'YYYY_MM')", tc.instant).Scan(&month); err != nil {
					t.Fatal(err)
				}
				if month != tc.month {
					t.Fatalf("database month = %s, want %s", month, tc.month)
				}
				tables := []string{"calls", "call_frequencies", "call_transmissions", "unit_events", "trunking_messages"}
				counts := make(map[string]int)
				for _, table := range tables {
					var countsValue int
					if err := p.db.Pool.QueryRow(p.ctx, "SELECT count(*) FROM pg_inherits WHERE inhparent = $1::regclass", table).Scan(&countsValue); err != nil {
						t.Fatal(err)
					}
					counts[table] = countsValue
					var exists bool
					name := table + "_y" + tc.month[:4] + "m" + tc.month[5:]
					if err := p.db.Pool.QueryRow(p.ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil || exists {
						t.Fatalf("missing-partition precondition for %s: exists=%v err=%v", name, exists, err)
					}
				}
				assertPartitions := func() {
					t.Helper()
					for _, table := range tables {
						name := table + "_y" + tc.month[:4] + "m" + tc.month[5:]
						var exists bool
						if err := p.db.Pool.QueryRow(p.ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil || !exists {
							t.Fatalf("partition %s missing: %v", name, err)
						}
						var count int
						if err := p.db.Pool.QueryRow(p.ctx, "SELECT count(*) FROM pg_inherits WHERE inhparent = $1::regclass", table).Scan(&count); err != nil {
							t.Fatal(err)
						}
						if count != counts[table]+1 {
							t.Fatalf("%s partitions = %d, want %d", table, count, counts[table]+1)
						}
					}
				}
				if mode == "on-demand helper" {
					// The same instant in different Go locations must choose the
					// same database month and remain idempotent.
					for _, zone := range []string{"America/New_York", "UTC", "Asia/Tokyo"} {
						location, err := time.LoadLocation(zone)
						if err != nil {
							t.Fatal(err)
						}
						p.ensurePartitionsFor(tc.instant.In(location))
						assertPartitions()
					}
				} else {
					// The initial INSERT has no partition. The HTTP path must
					// create the correct month and succeed on its single retry.
					var body bytes.Buffer
					writer := multipart.NewWriter(&body)
					for key, value := range map[string]string{"systemLabel": "partition-test", "talkgroup": "100", "dateTime": strconv.FormatInt(tc.instant.Unix(), 10)} {
						if err := writer.WriteField(key, value); err != nil {
							t.Fatal(err)
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, "/api/v1/call-upload", &body)
					req.Header.Set("Content-Type", writer.FormDataContentType())
					rec := httptest.NewRecorder()
					api.NewUploadHandler(p, "upload-test", zerolog.Nop()).Upload(rec, req)
					if rec.Code != http.StatusCreated {
						t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
					}
					var result api.UploadCallResult
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					var storedTime time.Time
					if err := p.db.Pool.QueryRow(p.ctx, "SELECT start_time FROM calls WHERE call_id = $1", result.CallID).Scan(&storedTime); err != nil {
						t.Fatal(err)
					}
					if !storedTime.Equal(tc.instant) || !result.StartTime.Equal(tc.instant) {
						t.Fatalf("call timestamp changed: stored=%v response=%v", storedTime, result.StartTime)
					}
					assertPartitions()
				}
			})
		}
	}
}
