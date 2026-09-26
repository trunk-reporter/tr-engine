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
	// Repeated list flags add up; none is silently dropped (r2-06).
	c, r, err = build("--all-talkgroups", "--exclude-talkgroups", "1:5001", "--exclude-talkgroups", "1:5002,1:5003",
		"--exclude-talkgroups", "")
	if c != restrictionReplaced || err != nil ||
		!r.Equal(&auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5001}, {SystemID: 1, Tgid: 5002}, {SystemID: 1, Tgid: 5003}}}) {
		t.Errorf("repeated --exclude-talkgroups: %v %+v %v", c, r, err)
	}
	c, r, err = build("--systems", "1", "--systems", "2", "--talkgroups", "3:1", "--talkgroups", "3:2")
	if err != nil || !r.Equal(&auth.Restriction{Systems: []int{1, 2}, Talkgroups: []auth.TG{{SystemID: 3, Tgid: 1}, {SystemID: 3, Tgid: 2}}}) {
		t.Errorf("repeated --systems/--talkgroups: %+v %v", r, err)
	}
	if _, _, err := build("--exclude-talkgroups", "1:5001", "--exclude-talkgroups", "bogus", "--all-talkgroups"); err == nil {
		t.Error("a bad value in a repeated flag was accepted")
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
		if got := legacyVariableWarning(name, res, nil); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want it to contain %q", name, got, want)
		}
	}

	// With this process's value looked up: the record describes it only if
	// the value is the imported key's or the retired token (r2-14).
	// A value that is another stored key is described as that key, not as
	// "not imported" (r3-02); a forgotten public token gets 401, not
	// anonymous access (r3-03).
	active, revoked, expired := database.KeyActive, database.KeyRevoked, database.KeyExpired
	noRecord := database.LegacyAuthResult{ImportedAt: testNow}
	fresh := database.LegacyAuthResult{ImportedAt: testNow, Detail: database.LegacyAuthDetail{
		Skipped: []database.LegacySkipped{{Variable: "AUTH_TOKEN", Reason: database.SkipFreshDatabase}}}}
	for _, c := range []struct {
		name    string
		res     database.LegacyAuthResult
		check   legacyValueCheck
		want    string
		notWant string
	}{
		{"WRITE_TOKEN", res, legacyValueCheck{keyID: 3, keyStatus: active}, "imported as API key #3 'legacy WRITE_TOKEN' on 2026-09-26) — remove it", "401"},
		{"WRITE_TOKEN", res, legacyValueCheck{keyID: 3, keyStatus: revoked}, "imported as API key #3 'legacy WRITE_TOKEN' on 2026-09-26; that key is revoked, so clients sending it get 401 invalid_key)", ""},
		{"WRITE_TOKEN", res, legacyValueCheck{}, "did not import (it recorded a different one), so clients sending it get 401 invalid_key", ""},
		{"WRITE_TOKEN", res, legacyValueCheck{keyID: 9, keyStatus: active}, "WRITE_TOKEN is no longer used (its value is API key #9) — remove it", "401"},
		{"WRITE_TOKEN", res, legacyValueCheck{keyID: 9, keyStatus: expired}, "(its value is API key #9; that key is expired, so clients sending it get 401 invalid_key)", "did not import"},
		{"AUTH_TOKEN", res, legacyValueCheck{retired: true}, "treated as anonymous", ""},
		{"AUTH_TOKEN", res, legacyValueCheck{forgotten: true}, "forget-retired-token: requests that still carry it get 401 invalid_key", "anonymous"},
		{"AUTH_TOKEN", res, legacyValueCheck{keyID: 9, keyStatus: active}, "AUTH_TOKEN is no longer used (its value is API key #9) — remove it", "401"},
		{"AUTH_TOKEN", res, legacyValueCheck{keyID: 9, keyStatus: revoked}, "(its value is API key #9; that key is revoked, so clients sending it get 401 invalid_key)", "did not import"},
		{"AUTH_TOKEN", res, legacyValueCheck{}, "tr-engine keys import", ""},
		{"AUTH_TOKEN", noRecord, legacyValueCheck{forgotten: true}, "forgotten with tr-engine access forget-retired-token: requests that still carry it get 401 invalid_key", "anonymous"},
		{"AUTH_TOKEN", noRecord, legacyValueCheck{keyID: 4, keyStatus: active}, "(its value is API key #4)", ""},
		{"AUTH_TOKEN", noRecord, legacyValueCheck{}, "register a secret that is still in use with tr-engine keys import", ""},
		{"AUTH_TOKEN", fresh, legacyValueCheck{}, "not imported into a new database", ""},
		{"AUTH_TOKEN", fresh, legacyValueCheck{keyID: 4, keyStatus: active}, "(its value is API key #4)", "new database"},
	} {
		check := c.check
		got := legacyVariableWarning(c.name, c.res, &check)
		if !strings.Contains(got, c.want) || (c.notWant != "" && strings.Contains(got, c.notWant)) {
			t.Errorf("%s %+v: %q, want it to contain %q and not %q", c.name, c.check, got, c.want, c.notWant)
		}
	}
}

// checkLegacyValue tells the stored retired token (anonymous) from a
// forgotten one (401), and reports a stored key's status (r3-02, r3-03).
func TestIntegrationCheckLegacyValue(t *testing.T) {
	db, _ := integrationDB(t)
	ctx := context.Background()

	const public = "publicdemotoken1234567"
	if err := db.SetRetiredPublicTokenHash(ctx, database.HashAPIKey(public)); err != nil {
		t.Fatal(err)
	}
	if c, err := checkLegacyValue(ctx, db, public); err != nil || !c.retired || c.forgotten || c.keyID != 0 {
		t.Fatalf("retired: %+v %v", c, err)
	}
	if _, _, err := runCmd(t, db, "access", "forget-retired-token", ""); err != nil {
		t.Fatal(err)
	}
	if c, err := checkLegacyValue(ctx, db, public); err != nil || c.retired || !c.forgotten {
		t.Fatalf("forgotten: %+v %v", c, err)
	}

	const rotated = "writetokenBBBBBBBBBBBBBBBBBBBBBBBB"
	out, _, err := runCmd(t, db, "keys", "import --name rotated --scopes admin,upload", rotated)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.Atoi(strings.TrimSpace(out))
	if c, err := checkLegacyValue(ctx, db, rotated); err != nil || c.keyID != id || c.keyStatus != database.KeyActive || c.retired || c.forgotten {
		t.Fatalf("imported key: %+v %v", c, err)
	}
	if _, _, err := runCmd(t, db, "keys", "revoke "+strconv.Itoa(id), ""); err != nil {
		t.Fatal(err)
	}
	if c, err := checkLegacyValue(ctx, db, rotated); err != nil || c.keyID != id || c.keyStatus != database.KeyRevoked {
		t.Fatalf("revoked key: %+v %v", c, err)
	}
	if c, err := checkLegacyValue(ctx, db, "somethingelse1234567890"); err != nil || c != (legacyValueCheck{}) {
		t.Fatalf("unknown value: %+v %v", c, err)
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
	db, err := openCLIDatabase(ctx, config.Overrides{EnvFile: "nonexistent.env", DatabaseURL: u.String()}, false, &bytes.Buffer{})
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
	// Repeated exclusions are all stored, and create shows the restriction (r2-06).
	out, errOut, err = runCmd(t, db, "keys", "create --name repeat --scopes listen --all-talkgroups --exclude-talkgroups 1:5001 --exclude-talkgroups 1:5002", "")
	if err != nil {
		t.Fatal(err)
	}
	repeat, _ := db.ResolveAPIKeyByHash(ctx, database.HashAPIKey(strings.TrimSpace(out)))
	wantRepeat := &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5001}, {SystemID: 1, Tgid: 5002}}}
	if repeat == nil || !repeat.Restriction.Equal(wantRepeat) {
		t.Errorf("repeat key = %+v", repeat)
	}
	if !strings.Contains(errOut, `"exclude_talkgroups":["1:5001","1:5002"]`) {
		t.Errorf("create summary lacks the restriction: %q", errOut)
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

	// The CLI is not subject to the last-admin guard, but warns when it
	// takes away the last admin key (r1-16).
	demoted, err := db.CreateAPIKey(ctx, database.NewAPIKey{Name: "demoted admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	_, errOut, err = runCmd(t, db, "keys", "update "+strconv.Itoa(demoted.ID)+" --expires 2h", "")
	if err != nil || !strings.Contains(errOut, "every active admin key expires by") {
		t.Errorf("short expiry on the only admin key: %v %q", err, errOut)
	}
	_, errOut, err = runCmd(t, db, "keys", "update "+strconv.Itoa(demoted.ID)+" --scopes listen", "")
	if err != nil || !strings.Contains(errOut, "no active admin key is left") {
		t.Errorf("demote the only admin key: %v %q", err, errOut)
	}
	_, errOut, err = runCmd(t, db, "keys", "update "+strconv.Itoa(demoted.ID)+" --name still-listen", "")
	if err != nil || strings.Contains(errOut, "warning") {
		t.Errorf("rename of a non-admin key warned: %v %q", err, errOut)
	}
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
	if _, _, err := runCmd(t, db, "keys", "import --name again --scopes admin", "short-secret"); err == nil ||
		!strings.Contains(err.Error(), "active key #") {
		t.Errorf("the same secret imported twice: %v", err)
	}
	// Importing a revoked secret says it is revoked (r1-17).
	if _, _, err := runCmd(t, db, "keys", "revoke "+strconv.Itoa(id), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, db, "keys", "import --name again --scopes upload", "short-secret"); err == nil ||
		!strings.Contains(err.Error(), "revoked on") || !strings.Contains(err.Error(), "keys create") {
		t.Errorf("import of a revoked secret: %v", err)
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
	if _, _, err := runCmd(t, db, "access", "set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001 --exclude-talkgroups 1:5002", ""); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAnonymousAccess(ctx); !a.Restriction.Equal(&auth.Restriction{AllowAll: true,
		ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5001}, {SystemID: 1, Tgid: 5002}}}) {
		t.Errorf("repeated --exclude-talkgroups stored %+v", a.Restriction)
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
	// Allow-nothing policies get a hint that fits the command (r1-17).
	_, _, err = runCmd(t, db, "access", "set --anonymous listen --exclude-talkgroups 1:5", "")
	if err == nil || !strings.Contains(err.Error(), "--all-talkgroups") || strings.Contains(err.Error(), "--anonymous off") {
		t.Errorf("exclude-only policy: %v", err)
	}
	_, _, err = runCmd(t, db, "access", "set --anonymous listen --systems 1 --exclude-talkgroups 1:5", "")
	if err != nil {
		t.Errorf("systems + exclusions: %v", err)
	}
	if msg := allowsNothingError("off", &auth.Restriction{}).Error(); strings.Contains(msg, "--anonymous off") ||
		!strings.Contains(msg, "--no-restriction") {
		t.Errorf("allow-nothing hint with access off: %q", msg)
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
	// The retired public token can't be imported as a key (r1-05)...
	if _, _, err := runCmd(t, db, "keys", "import --name prometheus --scopes admin", "old\n"); !errors.Is(err, database.ErrRetiredPublicToken) {
		t.Errorf("import of the retired public token: %v", err)
	}
	// ...and a key imported before that rule blocks forgetting it.
	mustImportRetired := func() int {
		var id int
		if err := db.Pool.QueryRow(ctx, `INSERT INTO api_keys (key_hash, key_prefix, name, scopes, legacy)
			VALUES ($1, 'legacy_x', 'old import', '{admin}', true) RETURNING id`, database.HashAPIKey("old")).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldID := mustImportRetired()
	if _, _, err := runCmd(t, db, "access", "forget-retired-token", ""); !errors.Is(err, database.ErrRetiredTokenIsKey) ||
		!strings.Contains(err.Error(), fmt.Sprintf("key #%d", oldID)) {
		t.Errorf("forget with a key holding the token: %v", err)
	}
	if h, _ := db.GetRetiredPublicTokenHash(ctx); h == "" {
		t.Error("the retired token was forgotten although a key holds it")
	}
	if _, _, err := runCmd(t, db, "keys", "revoke "+strconv.Itoa(oldID), ""); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCmd(t, db, "access", "forget-retired-token", "")
	if err != nil || !strings.Contains(errOut, "forgot") {
		t.Errorf("forget: %v %q", err, errOut)
	}
	if h, _ := db.GetRetiredPublicTokenHash(ctx); h != "" {
		t.Error("the retired token is still stored")
	}
	// Forgotten, it still can't be imported.
	if _, _, err := runCmd(t, db, "keys", "import --name prometheus --scopes admin", "old\n"); !errors.Is(err, database.ErrRetiredPublicToken) {
		t.Errorf("import of the forgotten public token: %v", err)
	}
	if _, errOut, err := runCmd(t, db, "access", "forget-retired-token", ""); err != nil || !strings.Contains(errOut, "no retired public token") {
		t.Errorf("second forget: %v %q", err, errOut)
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
	// An upgraded database: its schema predates schema.sql's marker.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM data_fixups WHERE name = $1`, database.SchemaCreatedFixup); err != nil {
		t.Fatal(err)
	}
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

	// Another engine on the database, or a changed .env, with different
	// values: the warnings don't claim they were imported (r2-14).
	other := &config.Config{UploadInstanceID: "http-upload", LegacyAuth: config.LegacyAuthEnv{
		AuthToken: "another-auth-token-value", WriteToken: "another-write-token-value",
	}}
	logs.Reset()
	if err := setupAuth(ctx, db, other, false, false, &stderr, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "no longer used (imported") ||
		strings.Count(logs.String(), "did not import (it recorded a different one)") != 2 {
		t.Errorf("changed values:\n%s", logs.String())
	}
}

// A database whose schema a CLI command created before the first server
// start is fresh: the old variables aren't imported, anonymous access stays
// off, and the bootstrap key is printed (r2-05).
func TestIntegrationSetupAuthCLICreatedSchema(t *testing.T) {
	db, _ := integrationDB(t) // the schema was created by openCLIDatabase
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO systems (system_id, system_type, name) VALUES (1, 'p25', 'imported')`); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{UploadInstanceID: "http-upload", LegacyAuth: config.LegacyAuthEnv{
		AdminPassword: "hunter22hunter22", AuthToken: "a-long-enough-auth-token", WriteToken: "a-long-enough-write-token",
	}}
	var stderr, logs bytes.Buffer
	if err := setupAuth(ctx, db, cfg, false, false, &stderr, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "tre_") {
		t.Errorf("no bootstrap key printed:\n%s", stderr.String())
	}
	if !strings.Contains(logs.String(), "fresh database: nothing imported, anonymous access off") {
		t.Errorf("logs:\n%s", logs.String())
	}
	if keys, _ := db.ListAPIKeys(ctx, true); len(keys) != 1 || keys[0].Name != database.BootstrapAdminKeyName {
		t.Errorf("keys = %+v, want only the bootstrap key", keys)
	}
	if a, err := db.GetAnonymousAccess(ctx); err != nil || a.Access != database.AccessOff {
		t.Errorf("anonymous access = %+v, %v; want off", a, err)
	}
}

func TestStripMigrateFlag(t *testing.T) {
	for _, c := range []struct {
		in      string
		out     string
		migrate bool
	}{
		{"list", "list", false},
		{"list --migrate", "list", true},
		{"-migrate show", "show", true},
		{"create --name x --migrate=true --scopes listen", "create --name x --scopes listen", true},
		{"list --migrate=false", "list", false},
		{"create --name migrate --scopes listen", "create --name migrate --scopes listen", false},
	} {
		out, migrate, err := stripMigrateFlag(strings.Fields(c.in))
		if err != nil || strings.Join(out, " ") != c.out || migrate != c.migrate {
			t.Errorf("%q: %q %v %v, want %q %v", c.in, out, migrate, err, c.out, c.migrate)
		}
	}
	if _, _, err := stripMigrateFlag([]string{"list", "--migrate=maybe"}); !isUsage(err) {
		t.Errorf("--migrate=maybe: %v, want a usage error", err)
	}
}

// A keys/access command on a database from before API keys names the
// migrations it would apply and refuses the irreversible ones (which break
// an old engine still running on it) unless --migrate is given (r2-15).
func TestIntegrationCLIRefusesIrreversibleMigrations(t *testing.T) {
	db, dbURL := integrationDB(t)
	ctx := context.Background()
	// The old engine's users table (what "record and drop users" drops).
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE users (id serial PRIMARY KEY, username text NOT NULL,
		role text NOT NULL CHECK (role IN ('viewer', 'editor', 'admin')), enabled boolean NOT NULL DEFAULT true)`); err != nil {
		t.Fatal(err)
	}
	usersExist := func() bool {
		var ok bool
		if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('users') IS NOT NULL`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	overrides := config.Overrides{EnvFile: "nonexistent.env", DatabaseURL: dbURL}

	var stderr bytes.Buffer
	if cli, err := openCLIDatabase(ctx, overrides, false, &stderr); err == nil {
		cli.Close()
		t.Fatal("opened a pre-API-key database without --migrate")
	} else if !strings.Contains(err.Error(), "record and drop users") || !strings.Contains(err.Error(), "--migrate") {
		t.Errorf("error = %v", err)
	}
	if !usersExist() {
		t.Fatal("the refused command dropped the users table")
	}

	// export and import (skipAuthConversion) run without the irreversible
	// migrations, still applying the others; import --dry-run always does
	// this (it refuses --migrate).
	if _, err := db.Pool.Exec(ctx, `DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := migrateForCLI(ctx, db, false, skipAuthConversion, &stderr); err != nil {
		t.Fatal(err)
	}
	if !usersExist() {
		t.Fatal("export/import without --migrate dropped the users table")
	}
	var auditLog bool
	if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('audit_log') IS NOT NULL`).Scan(&auditLog); err != nil || !auditLog {
		t.Errorf("the reversible migration was not applied: %v %v", auditLog, err)
	}
	if out := stderr.String(); !strings.Contains(out, "applying 1 pending schema migration(s): create audit_log table\n") ||
		!strings.Contains(out, "(record and drop users)") || !strings.Contains(out, "--migrate") {
		t.Errorf("stderr = %q", out)
	}
	var fixups int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM data_fixups WHERE name = 'removed-user-accounts'`).Scan(&fixups); err != nil || fixups != 0 {
		t.Errorf("removed-user-accounts recorded: %d %v", fixups, err)
	}

	stderr.Reset()
	cli, err := openCLIDatabase(ctx, overrides, true, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cli.Close()
	if !strings.Contains(stderr.String(), "applying 1 pending schema migration(s): record and drop users") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if usersExist() {
		t.Error("--migrate did not apply the migration")
	}

	// On an upgraded database whose server hasn't started yet, list/show
	// say the legacy import is still to come.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM data_fixups WHERE name = $1`, database.SchemaCreatedFixup); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"access", "show"}, {"keys", "list"}} {
		if _, errOut, err := runCmd(t, db, c[0], c[1], ""); err != nil || !strings.Contains(errOut, "hasn't run yet") {
			t.Errorf("%s %s: %v %q", c[0], c[1], err, errOut)
		}
	}
	if _, err := db.ImportLegacyAuth(ctx, database.LegacyAuthInput{}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := runCmd(t, db, "access", "show", ""); err != nil || strings.Contains(errOut, "hasn't run yet") {
		t.Errorf("after the import: %v %q", err, errOut)
	}
}
