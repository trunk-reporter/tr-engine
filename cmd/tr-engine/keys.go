package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	trengine "github.com/snarg/tr-engine"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// The keys and access subcommands (§10.2) manage API keys and the anonymous
// access policy directly in the database, like export/import. They log to
// stderr at WARN, fail on a migration error, and print on stdout only their
// result: the table for list/show, the plaintext key for create, the new ID
// for import. Every message goes to stderr, so a script can capture stdout
// (docker compose exec -T tr-engine tr-engine keys create ...).
//
// They are not subject to the API's last-admin guard: the CLI is how access
// is recovered when no admin key is left.

const keysUsage = `Usage: tr-engine keys <command> [flags]

Manage API keys. Keys are sent as "Authorization: Bearer <key>".

Commands:
  list   [--all]
  create --name NAME --scopes listen|edit|admin[,upload] | upload
         [--expires 90d|720h|2026-12-31|2026-12-31T00:00:00Z]
         [--all-talkgroups] [--systems 1,2] [--talkgroups 1:9178,1:9179]
         [--exclude-talkgroups 1:5001] [--rate-limit RPS]
  update ID|--prefix PREFIX [--name NAME] [--scopes ...]
         [--expires ...|--no-expiry] [--rate-limit RPS|--no-rate-limit]
         [restriction flags|--no-restriction]
  revoke ID|--prefix PREFIX
  import --name NAME --scopes ...   (reads an existing secret from stdin)

A number is always a key ID; select a key by its prefix with --prefix.
create prints only the new key on stdout; import prints only the new ID.
Restriction flags (only for keys whose scopes are exactly "listen") replace
the whole restriction; --systems "" or --talkgroups "" is an empty list.
Every command also takes --env-file and --database-url.

Running engines apply changes within 30 s, open streams within 60 s.
`

const accessUsage = `Usage: tr-engine access <command> [flags]

Manage the anonymous access policy: what a request without an API key may do.

Commands:
  show
  set --anonymous off|listen [--all-talkgroups] [--systems 1,2]
      [--talkgroups 1:9178] [--exclude-talkgroups 1:5001] [--no-restriction]
  forget-retired-token

set without restriction flags keeps the stored restriction; restriction
flags replace it, and --no-restriction clears it.
forget-retired-token stops treating the pre-upgrade public AUTH_TOKEN as no
credential (do this once no proxy injects it any more).
Every command also takes --env-file and --database-url.

Running engines apply changes within 30 s, open streams within 60 s.
`

// usageError is a command-line mistake (exit status 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// cliEnv is where a keys/access command reads and writes.
type cliEnv struct {
	db     *database.DB
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// cliAction runs a parsed command.
type cliAction func(ctx context.Context, env *cliEnv) error

// runKeys runs `tr-engine keys ...` and returns the exit status.
func runKeys(args []string, overrides config.Overrides) int {
	return runCLI(args, overrides, keysUsage, parseKeysArgs)
}

// runAccess runs `tr-engine access ...` and returns the exit status.
func runAccess(args []string, overrides config.Overrides) int {
	return runCLI(args, overrides, accessUsage, parseAccessArgs)
}

type cliParser func(args []string, overrides *config.Overrides, now time.Time) (cliAction, error)

func runCLI(args []string, overrides config.Overrides, usage string, parse cliParser) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	}
	action, err := parse(args, &overrides, time.Now())
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "Run with --help for usage.\n")
			return 2
		}
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := openCLIDatabase(ctx, overrides, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer db.Close()

	if err := action(ctx, &cliEnv{db: db, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// openCLIDatabase connects like the server does (config, schema, migrations)
// with logs on stderr at WARN. A failed migration is an error: the commands
// write tables that migrations create.
func openCLIDatabase(ctx context.Context, overrides config.Overrides, stderr io.Writer) (*database.DB, error) {
	log := zerolog.New(stderr).With().Timestamp().Logger().Level(zerolog.WarnLevel)
	cfg, err := config.Load(overrides)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	db, err := database.Connect(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if _, err := db.ApplySchemaIfEmpty(ctx, trengine.SchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema initialization: %w", err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema migration: %w", err)
	}
	return db, nil
}

// newFlagSet returns a FlagSet for one command, with the connection flags
// every command takes.
func newFlagSet(name string, overrides *config.Overrides) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&overrides.EnvFile, "env-file", overrides.EnvFile, "Path to .env file")
	fs.StringVar(&overrides.DatabaseURL, "database-url", overrides.DatabaseURL, "PostgreSQL connection URL")
	return fs
}

// parseFlags parses args, allowing flags after positional arguments (the
// flag package stops at the first one), and returns the positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{msg: err.Error()}
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// flagsSet returns the names of the flags given on the command line.
func flagsSet(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// restrictionFlags are the flags that describe a restriction (§10.2).
type restrictionFlags struct {
	allTalkgroups bool
	systems       string
	talkgroups    string
	exclude       string
	none          bool
}

func (rf *restrictionFlags) register(fs *flag.FlagSet, withNone bool) {
	fs.BoolVar(&rf.allTalkgroups, "all-talkgroups", false, "Allow every talkgroup (allow_all), minus --exclude-talkgroups")
	fs.StringVar(&rf.systems, "systems", "", `Allowed system IDs, comma-separated ("" = none)`)
	fs.StringVar(&rf.talkgroups, "talkgroups", "", `Allowed talkgroups as system_id:tgid, comma-separated ("" = none)`)
	fs.StringVar(&rf.exclude, "exclude-talkgroups", "", "Talkgroups never allowed, as system_id:tgid, comma-separated")
	if withNone {
		fs.BoolVar(&rf.none, "no-restriction", false, "Remove the restriction (unrestricted)")
	}
}

// restrictionChange says what the restriction flags ask for.
type restrictionChange int

const (
	restrictionUnchanged restrictionChange = iota // no restriction flags
	restrictionCleared                            // --no-restriction
	restrictionReplaced                           // restriction flags: build() is the new restriction
)

// build returns what the given restriction flags ask for, and the
// restriction for restrictionReplaced.
func (rf *restrictionFlags) build(set map[string]bool) (restrictionChange, *auth.Restriction, error) {
	given := set["all-talkgroups"] || set["systems"] || set["talkgroups"] || set["exclude-talkgroups"]
	if set["no-restriction"] && rf.none {
		if given {
			return 0, nil, usagef("--no-restriction can't be combined with other restriction flags")
		}
		return restrictionCleared, nil, nil
	}
	if !given {
		return restrictionUnchanged, nil, nil
	}
	r := &auth.Restriction{AllowAll: rf.allTalkgroups, Systems: []int{}, Talkgroups: []auth.TG{}, ExcludeTalkgroups: []auth.TG{}}
	for _, s := range splitList(rf.systems) {
		id, err := strconv.Atoi(s)
		if err != nil || id <= 0 {
			return 0, nil, usagef("--systems: %q is not a positive system ID", s)
		}
		r.Systems = append(r.Systems, id)
	}
	var err error
	if r.Talkgroups, err = parseTGList("--talkgroups", rf.talkgroups); err != nil {
		return 0, nil, err
	}
	if r.ExcludeTalkgroups, err = parseTGList("--exclude-talkgroups", rf.exclude); err != nil {
		return 0, nil, err
	}
	return restrictionReplaced, r, nil
}

// splitList splits a comma-separated flag value; "" is an empty list.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseTGList(flagName, s string) ([]auth.TG, error) {
	out := []auth.TG{}
	for _, part := range splitList(s) {
		tg, err := auth.ParseTG(part)
		if err != nil {
			return nil, usagef("%s: %v", flagName, err)
		}
		out = append(out, tg)
	}
	return out, nil
}

// parseScopesFlag parses --scopes (comma-separated).
func parseScopesFlag(s string) (auth.Scopes, error) {
	scopes, err := auth.ParseScopes(splitList(s))
	if err != nil {
		return nil, usagef("--scopes: %v", err)
	}
	return scopes, nil
}

// parseRateLimit parses --rate-limit (requests per second, > 0).
func parseRateLimit(s string) (*float32, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 32)
	if err != nil || v <= 0 {
		return nil, usagef("--rate-limit: %q is not a number greater than 0", s)
	}
	f := float32(v)
	return &f, nil
}

// keySelector picks a key by ID (a positional number) or by --prefix.
type keySelector struct {
	id     int
	prefix string
}

func parseKeySelector(positional []string, prefix string) (keySelector, error) {
	switch {
	case len(positional) > 1:
		return keySelector{}, usagef("unexpected arguments: %s", strings.Join(positional[1:], " "))
	case len(positional) == 1 && prefix != "":
		return keySelector{}, usagef("give either a key ID or --prefix, not both")
	case len(positional) == 1:
		id, err := strconv.Atoi(positional[0])
		if err != nil || id <= 0 {
			return keySelector{}, usagef("%q is not a key ID; select a key by prefix with --prefix", positional[0])
		}
		return keySelector{id: id}, nil
	case prefix != "":
		return keySelector{prefix: prefix}, nil
	}
	return keySelector{}, usagef("give a key ID or --prefix PREFIX")
}

// resolve returns the selected key's ID. A prefix must match exactly one key.
func (s keySelector) resolve(ctx context.Context, db *database.DB) (int, error) {
	if s.id > 0 {
		return s.id, nil
	}
	keys, err := db.FindAPIKeysByPrefix(ctx, s.prefix)
	if err != nil {
		return 0, err
	}
	switch len(keys) {
	case 0:
		return 0, fmt.Errorf("no key has the prefix %q (see tr-engine keys list --all)", s.prefix)
	case 1:
		return keys[0].ID, nil
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = "#" + strconv.Itoa(k.ID)
	}
	return 0, fmt.Errorf("prefix %q matches keys %s; use the ID", s.prefix, strings.Join(ids, ", "))
}

// parseKeysArgs parses `keys <command> ...`.
func parseKeysArgs(args []string, overrides *config.Overrides, now time.Time) (cliAction, error) {
	cmd, args := args[0], args[1:]
	fs := newFlagSet("keys "+cmd, overrides)
	switch cmd {
	case "list":
		all := fs.Bool("all", false, "Include revoked keys")
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usagef("unexpected arguments: %s", strings.Join(pos, " "))
		}
		return func(ctx context.Context, env *cliEnv) error { return keysList(ctx, env, *all) }, nil

	case "create", "import":
		name := fs.String("name", "", "What the key is for, e.g. \"tr-dashboard at home\" (required)")
		scopes := fs.String("scopes", "", "listen, edit or admin, optionally plus upload; or upload (required)")
		expires := fs.String("expires", "", "Expiry: 90d, 720h, 2026-12-31 or an RFC3339 time (default: never)")
		rateLimit := fs.String("rate-limit", "", "Per-key requests per second (default: no per-key limit)")
		var rf restrictionFlags
		rf.register(fs, false)
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usagef("unexpected arguments: %s", strings.Join(pos, " "))
		}
		set := flagsSet(fs)
		if !set["name"] || strings.TrimSpace(*name) == "" {
			return nil, usagef("--name is required: say what the key is for")
		}
		if !set["scopes"] {
			return nil, usagef("--scopes is required")
		}
		in := database.NewAPIKey{Name: *name}
		if in.Scopes, err = parseScopesFlag(*scopes); err != nil {
			return nil, err
		}
		if set["expires"] {
			t, err := auth.ParseExpires(*expires, now)
			if err != nil {
				return nil, usagef("--expires: %v", err)
			}
			in.ExpiresAt = &t
		}
		if set["rate-limit"] {
			if in.RateLimitRPS, err = parseRateLimit(*rateLimit); err != nil {
				return nil, err
			}
		}
		if _, in.Restriction, err = rf.build(set); err != nil {
			return nil, err
		}
		if cmd == "import" {
			return func(ctx context.Context, env *cliEnv) error { return keysImport(ctx, env, in) }, nil
		}
		return func(ctx context.Context, env *cliEnv) error { return keysCreate(ctx, env, in) }, nil

	case "update":
		prefix := fs.String("prefix", "", "Select the key by its prefix")
		name := fs.String("name", "", "New name")
		scopes := fs.String("scopes", "", "New scopes")
		expires := fs.String("expires", "", "New expiry: 90d, 720h, 2026-12-31 or an RFC3339 time")
		noExpiry := fs.Bool("no-expiry", false, "Remove the expiry")
		rateLimit := fs.String("rate-limit", "", "New per-key requests per second")
		noRateLimit := fs.Bool("no-rate-limit", false, "Remove the per-key rate limit")
		var rf restrictionFlags
		rf.register(fs, true)
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		sel, err := parseKeySelector(pos, *prefix)
		if err != nil {
			return nil, err
		}
		set := flagsSet(fs)
		var p database.APIKeyPatch
		if set["name"] {
			p.SetName, p.Name = true, *name
		}
		if set["scopes"] {
			if p.Scopes, err = parseScopesFlag(*scopes); err != nil {
				return nil, err
			}
			p.SetScopes = true
		}
		switch {
		case set["expires"] && set["no-expiry"]:
			return nil, usagef("--expires and --no-expiry can't be combined")
		case set["expires"]:
			t, err := auth.ParseExpires(*expires, now)
			if err != nil {
				return nil, usagef("--expires: %v", err)
			}
			p.SetExpiresAt, p.ExpiresAt = true, &t
		case set["no-expiry"] && *noExpiry:
			p.SetExpiresAt = true
		}
		switch {
		case set["rate-limit"] && set["no-rate-limit"]:
			return nil, usagef("--rate-limit and --no-rate-limit can't be combined")
		case set["rate-limit"]:
			if p.RateLimitRPS, err = parseRateLimit(*rateLimit); err != nil {
				return nil, err
			}
			p.SetRateLimitRPS = true
		case set["no-rate-limit"] && *noRateLimit:
			p.SetRateLimitRPS = true
		}
		change, r, err := rf.build(set)
		if err != nil {
			return nil, err
		}
		if change != restrictionUnchanged {
			p.SetRestriction, p.Restriction = true, r // nil for --no-restriction
		}
		if !p.SetName && !p.SetScopes && !p.SetExpiresAt && !p.SetRateLimitRPS && !p.SetRestriction {
			return nil, usagef("nothing to change: give --name, --scopes, --expires, --rate-limit or restriction flags")
		}
		return func(ctx context.Context, env *cliEnv) error { return keysUpdate(ctx, env, sel, p) }, nil

	case "revoke":
		prefix := fs.String("prefix", "", "Select the key by its prefix")
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		sel, err := parseKeySelector(pos, *prefix)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, env *cliEnv) error { return keysRevoke(ctx, env, sel) }, nil
	}
	return nil, usagef("unknown keys command %q (list, create, update, revoke, import)", cmd)
}

// parseAccessArgs parses `access <command> ...`.
func parseAccessArgs(args []string, overrides *config.Overrides, _ time.Time) (cliAction, error) {
	cmd, args := args[0], args[1:]
	fs := newFlagSet("access "+cmd, overrides)
	switch cmd {
	case "show", "forget-retired-token":
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usagef("unexpected arguments: %s", strings.Join(pos, " "))
		}
		if cmd == "show" {
			return accessShow, nil
		}
		return accessForgetRetiredToken, nil

	case "set":
		anonymous := fs.String("anonymous", "", "off or listen (required)")
		var rf restrictionFlags
		rf.register(fs, true)
		pos, err := parseFlags(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usagef("unexpected arguments: %s", strings.Join(pos, " "))
		}
		if *anonymous != database.AccessOff && *anonymous != database.AccessListen {
			return nil, usagef("--anonymous must be off or listen")
		}
		change, r, err := rf.build(flagsSet(fs))
		if err != nil {
			return nil, err
		}
		access := *anonymous
		return func(ctx context.Context, env *cliEnv) error { return accessSet(ctx, env, access, change, r) }, nil
	}
	return nil, usagef("unknown access command %q (show, set, forget-retired-token)", cmd)
}

const cliApplyNote = "running engines apply this within 30 s (open streams within 60 s)"

func keysList(ctx context.Context, env *cliEnv, all bool) error {
	keys, err := env.db.ListAPIKeys(ctx, all)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tSCOPES\tSTATUS\tEXPIRES\tLAST USED\tRATE LIMIT\tRESTRICTION")
	for _, k := range keys {
		status := string(k.Status)
		if k.Legacy {
			status += " (legacy)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, k.Name, k.Prefix, strings.Join(k.Scopes.Strings(), ","), status,
			formatTime(k.ExpiresAt, "never"), formatTime(k.LastUsedAt, "never"),
			formatRateLimit(k.RateLimitRPS), formatRestriction(k.Restriction))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintln(env.stderr, "no keys")
	}
	return nil
}

func keysCreate(ctx context.Context, env *cliEnv, in database.NewAPIKey) error {
	key, err := env.db.CreateAPIKey(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.stdout, key.Plaintext)
	fmt.Fprintf(env.stderr, "created key #%d %q (%s, scopes %s). Store it now: it will not be shown again.\n",
		key.ID, key.Name, key.Prefix, strings.Join(key.Scopes.Strings(), ","))
	fmt.Fprintln(env.stderr, "A key used in web pages that other people load is public: give such pages no key, or a restricted listen key.")
	return nil
}

func keysImport(ctx context.Context, env *cliEnv, in database.NewAPIKey) error {
	secret, err := readSecret(env.stdin)
	if err != nil {
		return err
	}
	key, created, err := env.db.CreateLegacyAPIKey(ctx, secret, in)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("this secret is already stored as key #%d %q (scopes %s); nothing was changed",
			key.ID, key.Name, strings.Join(key.Scopes.Strings(), ","))
	}
	fmt.Fprintln(env.stdout, key.ID)
	fmt.Fprintf(env.stderr, "imported the secret as key #%d %q (%s, scopes %s, legacy)\n",
		key.ID, key.Name, key.Prefix, strings.Join(key.Scopes.Strings(), ","))
	if weak, _ := database.CheckLegacySecret(secret); weak {
		fmt.Fprintf(env.stderr, "warning: legacy key #%d is weak (%d characters) — replace it\n", key.ID, len([]rune(secret)))
	}
	return nil
}

// readSecret reads a secret from stdin: the first line, without surrounding
// white space.
func readSecret(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the secret from stdin: %w", err)
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", errors.New("no secret on stdin (pipe it in, e.g. tr-engine keys import ... < token.txt)")
	}
	return secret, nil
}

func keysUpdate(ctx context.Context, env *cliEnv, sel keySelector, p database.APIKeyPatch) error {
	id, err := sel.resolve(ctx, env.db)
	if err != nil {
		return err
	}
	key, err := env.db.PatchAPIKey(ctx, id, p, database.NoLastAdminGuard)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stderr, "updated key #%d %q (%s): scopes %s, expires %s, rate limit %s, restriction %s; %s\n",
		key.ID, key.Name, key.Prefix, strings.Join(key.Scopes.Strings(), ","), formatTime(key.ExpiresAt, "never"),
		formatRateLimit(key.RateLimitRPS), formatRestriction(key.Restriction), cliApplyNote)
	return nil
}

func keysRevoke(ctx context.Context, env *cliEnv, sel keySelector) error {
	id, err := sel.resolve(ctx, env.db)
	if err != nil {
		return err
	}
	before, err := env.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		return err
	}
	key, err := env.db.RevokeAPIKey(ctx, id, database.NoLastAdminGuard)
	if err != nil {
		return err
	}
	if before.RevokedAt != nil {
		fmt.Fprintf(env.stderr, "key #%d %q was already revoked\n", key.ID, key.Name)
		return nil
	}
	fmt.Fprintf(env.stderr, "revoked key #%d %q (%s); %s\n", key.ID, key.Name, key.Prefix, cliApplyNote)
	if key.Scopes.Has(auth.ScopeAdmin) {
		if exists, err := env.db.ActiveAdminKeyExists(ctx); err == nil && !exists {
			fmt.Fprintln(env.stderr, `warning: no active admin key is left; create one with: tr-engine keys create --name "admin" --scopes admin`)
		}
	}
	return nil
}

func accessShow(ctx context.Context, env *cliEnv) error {
	a, err := env.db.GetAnonymousAccess(ctx)
	if err != nil {
		return err
	}
	retired, err := env.db.GetRetiredPublicTokenHash(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "anonymous access:     %s\n", a.Access)
	fmt.Fprintf(env.stdout, "restriction:          %s\n", formatRestriction(a.Restriction))
	fmt.Fprintf(env.stdout, "updated:              %s\n", formatTime(a.UpdatedAt, "never (default)"))
	if retired != "" {
		fmt.Fprintln(env.stdout, "retired public token: stored (requests carrying the old public AUTH_TOKEN are treated as anonymous)")
	} else {
		fmt.Fprintln(env.stdout, "retired public token: none")
	}
	return nil
}

func accessSet(ctx context.Context, env *cliEnv, access string, change restrictionChange, r *auth.Restriction) error {
	if change == restrictionUnchanged {
		cur, err := env.db.GetAnonymousAccess(ctx)
		if err != nil {
			return err
		}
		r = cur.Restriction
	}
	a, err := env.db.SetAnonymousAccess(ctx, access, r)
	if errors.Is(err, auth.ErrAnonymousAllowsNothing) {
		return errors.New("the restriction allows nothing; use --anonymous off instead")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stderr, "anonymous access set to %s, restriction %s; %s\n",
		a.Access, formatRestriction(a.Restriction), cliApplyNote)
	return nil
}

func accessForgetRetiredToken(ctx context.Context, env *cliEnv) error {
	cleared, err := env.db.ClearRetiredPublicToken(ctx)
	if err != nil {
		return err
	}
	if cleared {
		fmt.Fprintf(env.stderr, "forgot the retired public token: a request carrying it is now an invalid key (401); %s\n", cliApplyNote)
	} else {
		fmt.Fprintln(env.stderr, "no retired public token was stored")
	}
	return nil
}

func formatTime(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

func formatRateLimit(rps *float32) string {
	if rps == nil {
		return "none"
	}
	return strconv.FormatFloat(float64(*rps), 'g', -1, 32) + "/s"
}

func formatRestriction(r *auth.Restriction) string {
	if r == nil {
		return "none"
	}
	b, err := json.Marshal(r.Normalize())
	if err != nil {
		return "?"
	}
	return string(b)
}
