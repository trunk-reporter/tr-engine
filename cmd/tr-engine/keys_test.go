package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

var testNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// parseKeys parses a keys command line without running it.
func parseKeys(t *testing.T, line string) (cliAction, config.Overrides, error) {
	t.Helper()
	var o config.Overrides
	a, err := parseKeysArgs(strings.Fields(line), &o, testNow)
	return a, o, err
}

func isUsage(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}

func TestParseKeysArgs(t *testing.T) {
	for _, line := range []string{
		"list",
		"list --all",
		"list --all --env-file /tmp/x.env --database-url postgres://x/y",
		"create --name dashboard --scopes edit",
		"create --name uploads --scopes upload",
		"create --name all --scopes admin,upload --expires 90d --rate-limit 2.5",
		"create --name club --scopes listen --all-talkgroups --exclude-talkgroups 1:5001,1:5002",
		"create --name club --scopes listen --systems 1,2 --talkgroups 3:9178",
		"create --name x --scopes listen --expires 2026-12-31",
		"create --name x --scopes listen --expires 2026-12-31T00:00:00Z",
		"create --name x --scopes listen --expires 720h",
		"update 3 --name renamed",
		"update --prefix tre_1a2b3c4d --scopes listen",
		"update 3 --no-expiry --no-rate-limit --no-restriction",
		"update --name x 3",
		"revoke 7",
		"revoke --prefix legacy_9f2c1a",
		"import --name old --scopes listen",
	} {
		if _, _, err := parseKeys(t, line); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}

	for _, line := range []string{
		"frobnicate",
		"list extra",
		"create --scopes edit",
		"create --name x",
		"create --name x --scopes listen,edit",
		"create --name x --scopes root",
		"create --name x --scopes listen --expires yesterday",
		"create --name x --scopes listen --expires 2001-01-01",
		"create --name x --scopes listen --rate-limit 0",
		"create --name x --scopes listen --rate-limit fast",
		"create --name x --scopes listen --systems one",
		"create --name x --scopes listen --talkgroups 1-2",
		"create --name x --scopes listen --no-restriction",
		"update",
		"update abc",
		"update 0 --name x",
		"update 3",
		"update 3 --prefix tre_x --name y",
		"update 3 4 --name y",
		"update 3 --expires 1d --no-expiry",
		"update 3 --rate-limit 1 --no-rate-limit",
		"update 3 --no-restriction --systems 1",
		"revoke",
		"revoke tre_1a2b3c4d",
		"create --name x --scopes listen --bogus",
	} {
		if _, _, err := parseKeys(t, line); err == nil || !isUsage(err) {
			t.Errorf("%q: got %v, want a usage error", line, err)
		}
	}

	// Connection flags reach the config overrides.
	_, o, err := parseKeys(t, "list --env-file /tmp/prod.env --database-url postgres://h/db")
	if err != nil || o.EnvFile != "/tmp/prod.env" || o.DatabaseURL != "postgres://h/db" {
		t.Errorf("overrides = %+v, %v", o, err)
	}

	// --help is not an error.
	if _, _, err := parseKeys(t, "create --help"); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help: %v", err)
	}
}

func TestRestrictionFlags(t *testing.T) {
	build := func(args ...string) (restrictionChange, *auth.Restriction, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var rf restrictionFlags
		rf.register(fs, true)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return rf.build(flagsSet(fs))
	}
	if c, r, err := build(); c != restrictionUnchanged || r != nil || err != nil {
		t.Errorf("no flags: %v %v %v", c, r, err)
	}
	if c, r, err := build("--no-restriction"); c != restrictionCleared || r != nil || err != nil {
		t.Errorf("--no-restriction: %v %v %v", c, r, err)
	}
	// An explicitly empty list is an empty list, not "absent": allows nothing.
	c, r, err := build("--systems", "")
	if c != restrictionReplaced || err != nil || r == nil || !r.AllowsNothing() {
		t.Errorf(`--systems "": %v %+v %v, want an allow-nothing restriction`, c, r, err)
	}
	c, r, err = build("--all-talkgroups", "--exclude-talkgroups", "1:5001, 1:5002")
	if c != restrictionReplaced || err != nil || !r.AllowAll || len(r.ExcludeTalkgroups) != 2 || r.ExcludeTalkgroups[1] != (auth.TG{SystemID: 1, Tgid: 5002}) {
		t.Errorf("allow_all + exclusions: %v %+v %v", c, r, err)
	}
	c, r, err = build("--systems", "1,2", "--talkgroups", "3:9178")
	if err != nil || len(r.Systems) != 2 || len(r.Talkgroups) != 1 || r.AllowAll {
		t.Errorf("systems + talkgroups: %+v %v", r, err)
	}
}

func TestParseAccessArgs(t *testing.T) {
	for _, line := range []string{
		"show", "forget-retired-token", "set --anonymous off", "set --anonymous listen --no-restriction",
		"set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001",
	} {
		var o config.Overrides
		if _, err := parseAccessArgs(strings.Fields(line), &o, testNow); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	for _, line := range []string{"set", "set --anonymous edit", "show extra", "frob", "set --anonymous listen --no-restriction --systems 1"} {
		var o config.Overrides
		if _, err := parseAccessArgs(strings.Fields(line), &o, testNow); err == nil || !isUsage(err) {
			t.Errorf("%q: got %v, want a usage error", line, err)
		}
	}
}

func TestPrintBootstrapBanner(t *testing.T) {
	var b bytes.Buffer
	printBootstrapBanner(&b, "tre_0123", false)
	if !strings.Contains(b.String(), "\n   tre_0123\n") || strings.Contains(b.String(), "docker compose") ||
		!strings.Contains(b.String(), "tr-engine keys --help") {
		t.Errorf("banner:\n%s", b.String())
	}
	b.Reset()
	printBootstrapBanner(&b, "tre_0123", true)
	if !strings.Contains(b.String(), "docker compose exec -T tr-engine tr-engine keys --help") {
		t.Errorf("docker banner:\n%s", b.String())
	}
}

func TestLegacyVariableWarning(t *testing.T) {
	res := database.LegacyAuthResult{
		ImportedAt: testNow,
		Detail: database.LegacyAuthDetail{
			Imported: []database.LegacyImportedKey{{Variable: "WRITE_TOKEN", KeyID: 3, Name: "legacy WRITE_TOKEN"}},
			Skipped:  []database.LegacySkipped{{Variable: "AUTH_TOKEN", Reason: database.SkipPublicToken}},
		},
	}
	for name, want := range map[string]string{
		"WRITE_TOKEN":    "WRITE_TOKEN is no longer used (imported as API key #3 'legacy WRITE_TOKEN' on 2026-09-26) — remove it from your configuration; see docs/migrating-auth.md",
		"AUTH_TOKEN":     "public read token",
		"ADMIN_PASSWORD": "ADMIN_PASSWORD is no longer used — tr-engine has no user accounts; clients use API keys. See docs/auth.md",
		"CORS_ORIGINS":   "CORS_ORIGINS is no longer needed: the API allows all origins and never uses cookies",
		"AUTH_ENABLED":   "AUTH_ENABLED is no longer used",
	} {
		if got := legacyVariableWarning(name, res); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want it to contain %q", name, got, want)
		}
	}
}

// integrationDB creates a throwaway database (tr_engine_test_*) with the
// schema and migrations, dropped after the test. Skipped unless
// TEST_DATABASE_URL is set.
func integrationDB(t *testing.T) (*database.DB, string) {
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
	// openCLIDatabase is what the commands use: config, schema, migrations.
	t.Setenv("DATABASE_URL", u.String())
	db, err := openCLIDatabase(ctx, config.Overrides{EnvFile: "nonexistent.env", DatabaseURL: u.String()}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	return db, u.String()
}

// runCmd parses and runs a keys/access command line against db.
func runCmd(t *testing.T, db *database.DB, group, line, stdin string) (stdout, stderr string, err error) {
	t.Helper()
	var o config.Overrides
	args := strings.Fields(line)
	var action cliAction
	if group == "keys" {
		action, err = parseKeysArgs(args, &o, time.Now())
	} else {
		action, err = parseAccessArgs(args, &o, time.Now())
	}
	if err != nil {
		return "", "", err
	}
	var out, errOut bytes.Buffer
	err = action(context.Background(), &cliEnv{db: db, stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut})
	return out.String(), errOut.String(), err
}

func TestIntegrationKeysCLI(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()

	// create prints only the key on stdout.
	out, errOut, err := runCmd(t, db, "keys", "create --name dashboard --scopes edit --expires 90d", "")
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(out)
	if !strings.HasPrefix(key, "tre_") || len(key) != 68 || strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout = %q, want only the key", out)
	}
	if !strings.Contains(errOut, "created key #") {
		t.Errorf("stderr = %q", errOut)
	}
	k, err := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey(key))
	if err != nil || k.Name != "dashboard" || k.ExpiresAt == nil || strings.Join(k.Scopes.Strings(), ",") != "edit" {
		t.Fatalf("stored key = %+v, %v", k, err)
	}

	// A restricted listen key; allow-nothing is refused.
	if _, _, err := runCmd(t, db, "keys", "create --name club --scopes listen --exclude-talkgroups 1:5001", ""); err == nil {
		t.Error("an exclude-only (allow-nothing) key was created")
	}
	out, _, err = runCmd(t, db, "keys", "create --name club --scopes listen --all-talkgroups --exclude-talkgroups 1:5001 --rate-limit 3", "")
	if err != nil {
		t.Fatal(err)
	}
	club, _ := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey(strings.TrimSpace(out)))
	if club.Restriction == nil || !club.Restriction.AllowAll || club.RateLimitRPS == nil || *club.RateLimitRPS != 3 {
		t.Errorf("club key = %+v", club)
	}

	// update by ID and by prefix; absent flags change nothing.
	if _, _, err := runCmd(t, db, "keys", fmt.Sprintf("update %d --name %s", club.ID, "club-v2"), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, db, "keys", "update --prefix "+club.Prefix+" --no-rate-limit --systems 1,2", ""); err != nil {
		t.Fatal(err)
	}
	club, _ = db.GetAPIKeyByID(ctx, club.ID)
	if club.Name != "club-v2" || club.RateLimitRPS != nil || club.Restriction == nil || club.Restriction.AllowAll ||
		len(club.Restriction.Systems) != 2 {
		t.Errorf("after updates: %+v %+v", club, club.Restriction)
	}
	if _, _, err := runCmd(t, db, "keys", fmt.Sprintf("update %d --scopes edit", club.ID), ""); err == nil {
		t.Error("changed a restricted key's scopes without clearing the restriction")
	}
	if _, _, err := runCmd(t, db, "keys", fmt.Sprintf("update %d --scopes edit --no-restriction", club.ID), ""); err != nil {
		t.Errorf("scopes + --no-restriction: %v", err)
	}
	if _, _, err := runCmd(t, db, "keys", "update --prefix tre_00000000 --name x", ""); err == nil {
		t.Error("an unknown prefix matched")
	}

	// list shows keys on stdout, revoked ones only with --all.
	out, _, err = runCmd(t, db, "keys", "list", "")
	if err != nil || !strings.Contains(out, "dashboard") || !strings.Contains(out, "club-v2") {
		t.Errorf("list: %v\n%s", err, out)
	}

	// The CLI is not subject to the last-admin guard.
	admin, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "only admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	_, errOut, err = runCmd(t, db, "keys", "revoke "+strconv.Itoa(admin.ID), "")
	if err != nil || !strings.Contains(errOut, "no active admin key is left") {
		t.Errorf("revoke the last admin: %v %q", err, errOut)
	}
	if k, _ := db.GetAPIKeyByID(ctx, admin.ID); k.RevokedAt == nil {
		t.Error("not revoked")
	}
	_, errOut, _ = runCmd(t, db, "keys", "revoke --prefix "+admin.Prefix, "")
	if !strings.Contains(errOut, "already revoked") {
		t.Errorf("second revoke: %q", errOut)
	}
	out, _, _ = runCmd(t, db, "keys", "list", "")
	if strings.Contains(out, "only admin") {
		t.Errorf("list shows a revoked key:\n%s", out)
	}
	out, _, _ = runCmd(t, db, "keys", "list --all", "")
	if !strings.Contains(out, "only admin") || !strings.Contains(out, "revoked") {
		t.Errorf("list --all:\n%s", out)
	}

	// import reads the secret from stdin and prints only the ID.
	out, errOut, err = runCmd(t, db, "keys", "import --name nightly --scopes listen", "short-secret\n")
	if err != nil {
		t.Fatal(err)
	}
	id, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil || !strings.Contains(errOut, "is weak (12 characters)") {
		t.Errorf("import: stdout %q stderr %q", out, errOut)
	}
	imported, err := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey("short-secret"))
	if err != nil || imported.ID != id || !imported.Legacy || !strings.HasPrefix(imported.Prefix, "legacy_") {
		t.Errorf("imported = %+v, %v", imported, err)
	}
	if _, _, err := runCmd(t, db, "keys", "import --name again --scopes admin", "short-secret"); err == nil {
		t.Error("the same secret was imported twice")
	}
	if _, _, err := runCmd(t, db, "keys", "import --name shell --scopes listen", "$(openssl rand -hex 32)\n"); err == nil {
		t.Error(`a secret with "$(" was imported`)
	}
	if _, _, err := runCmd(t, db, "keys", "import --name empty --scopes listen", "\n"); err == nil {
		t.Error("an empty secret was imported")
	}
}

func TestIntegrationAccessCLI(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()

	out, _, err := runCmd(t, db, "access", "show", "")
	if err != nil || !strings.Contains(out, "anonymous access:     off") || !strings.Contains(out, "retired public token: none") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if _, _, err := runCmd(t, db, "access", "set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001", ""); err != nil {
		t.Fatal(err)
	}
	// Without restriction flags the stored restriction is kept.
	if _, _, err := runCmd(t, db, "access", "set --anonymous off", ""); err != nil {
		t.Fatal(err)
	}
	a, _ := db.GetAnonymousAccess(ctx)
	if a.Access != "off" || a.Restriction == nil || !a.Restriction.AllowAll {
		t.Errorf("after set off: %+v", a)
	}
	if _, _, err := runCmd(t, db, "access", "set --anonymous listen --talkgroups ", ""); err == nil {
		// strings.Fields drops the empty value, so this is --talkgroups with
		// no argument: a usage error, not a silent unrestricted policy.
		t.Error("--talkgroups without a value was accepted")
	}
	_, _, err = runCmd(t, db, "access", "set --anonymous listen --exclude-talkgroups 1:5", "")
	if err == nil || !strings.Contains(err.Error(), "--anonymous off") {
		t.Errorf("allow-nothing policy: %v", err)
	}
	if _, _, err := runCmd(t, db, "access", "set --anonymous listen --no-restriction", ""); err != nil {
		t.Fatal(err)
	}
	a, _ = db.GetAnonymousAccess(ctx)
	if a.Access != "listen" || a.Restriction != nil {
		t.Errorf("after --no-restriction: %+v", a)
	}

	if err := db.SetRetiredPublicTokenHash(ctx, database.HashAPIKey("old")); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runCmd(t, db, "access", "show", "")
	if !strings.Contains(out, "retired public token: stored") {
		t.Errorf("show:\n%s", out)
	}
	_, errOut, err := runCmd(t, db, "access", "forget-retired-token", "")
	if err != nil || !strings.Contains(errOut, "forgot") {
		t.Errorf("forget: %v %q", err, errOut)
	}
	if h, _ := db.GetRetiredPublicTokenHash(ctx); h != "" {
		t.Error("the retired token is still stored")
	}
}

func TestIntegrationSetupAuth(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()
	cfg := &config.Config{UploadInstanceID: "http-upload", LegacyAuth: config.LegacyAuthEnv{
		AdminPassword: "hunter22hunter22", CORSOrigins: "https://x.example", WriteToken: "a-long-enough-write-token",
	}}

	// First start of a fresh database: nothing is imported, a bootstrap key
	// is printed once, and every old variable is warned about.
	var stderr, logs bytes.Buffer
	log := zerolog.New(&logs)
	if err := setupAuth(ctx, db, cfg, true, false, &stderr, log); err != nil {
		t.Fatal(err)
	}
	banner := stderr.String()
	i := strings.Index(banner, "tre_")
	if i < 0 || !strings.Contains(banner, "no admin API key existed") {
		t.Fatalf("no banner on stderr:\n%s", banner)
	}
	key := banner[i : i+68]
	k, err := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey(key))
	if err != nil || k.Name != database.BootstrapAdminKeyName || !k.Scopes.Has(auth.ScopeAdmin) {
		t.Fatalf("bootstrap key = %+v, %v", k, err)
	}
	if strings.Contains(logs.String(), key) || !strings.Contains(logs.String(), `"prefix":"`+k.Prefix+`"`) {
		t.Errorf("the structured log must carry the prefix, never the key:\n%s", logs.String())
	}
	for _, want := range []string{
		"ADMIN_PASSWORD is no longer used",
		"CORS_ORIGINS is no longer needed",
		"WRITE_TOKEN is no longer used (not imported into a new database",
		`"access":"off"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
	if keys, _ := db.ListAPIKeys(ctx, true); len(keys) != 1 {
		t.Errorf("keys = %d, want only the bootstrap key", len(keys))
	}

	// Later starts print nothing; with no active admin key they log the
	// recovery command.
	stderr.Reset()
	logs.Reset()
	if err := setupAuth(ctx, db, cfg, false, true, &stderr, log); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("second start printed:\n%s", stderr.String())
	}
	if _, err := db.RevokeAPIKey(ctx, k.ID, database.NoLastAdminGuard); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if err := setupAuth(ctx, db, cfg, false, false, &stderr, log); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || !strings.Contains(logs.String(), `tr-engine keys create --name \"admin\" --scopes admin`) ||
		!strings.Contains(logs.String(), `"level":"error"`) {
		t.Errorf("no recovery ERROR:\n%s", logs.String())
	}
}

func TestIntegrationSetupAuthUpgrade(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO systems (system_id, system_type, name, sysid, wacn) VALUES (1, 'p25', 'x', '348', 'BEE00')`); err != nil {
		t.Fatal(err)
	}
	// An upgraded token-mode instance with a WRITE_TOKEN: both are imported,
	// the WRITE_TOKEN is an admin key, so no bootstrap key is minted.
	cfg := &config.Config{UploadInstanceID: "http-upload", LegacyAuth: config.LegacyAuthEnv{
		AuthToken: "short-token", WriteToken: "a-long-enough-write-token",
	}}
	var stderr, logs bytes.Buffer
	if err := setupAuth(ctx, db, cfg, false, false, &stderr, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a bootstrap key was printed although WRITE_TOKEN is an admin key:\n%s", stderr.String())
	}
	for _, want := range []string{
		"legacy auth import (old mode token)",
		"WRITE_TOKEN is no longer used (imported as API key #",
		"AUTH_TOKEN is no longer used (imported as API key #",
		"is weak (11 characters) — replace it",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
	w, err := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey("a-long-enough-write-token"))
	if err != nil || strings.Join(w.Scopes.Strings(), ",") != "admin,upload" {
		t.Errorf("WRITE_TOKEN key = %+v, %v", w, err)
	}

	// The per-start warnings repeat; the one-time import summary doesn't.
	logs.Reset()
	if err := setupAuth(ctx, db, cfg, false, false, &stderr, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "legacy auth import") || !strings.Contains(logs.String(), "WRITE_TOKEN is no longer used (imported") ||
		!strings.Contains(logs.String(), "is weak") {
		t.Errorf("second start logs:\n%s", logs.String())
	}
}
