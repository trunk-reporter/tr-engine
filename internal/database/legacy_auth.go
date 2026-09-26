package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/snarg/tr-engine/internal/auth"
)

// data_fixups claims of the API-key auth upgrade (§10.1, §11).
const (
	// LegacyAuthImportFixup: the one-time import of the old auth variables.
	// Its detail is a LegacyAuthDetail.
	LegacyAuthImportFixup = "import-legacy-auth"
	// BootstrapAdminKeyFixup: the one-time bootstrap admin key. Its detail
	// says whether a key was minted, and which (never the secret).
	BootstrapAdminKeyFixup = "bootstrap-admin-key"
	// RemovedUserAccountsFixup is written by the "record and drop users"
	// migration: a JSON array of the dropped user accounts.
	RemovedUserAccountsFixup = "removed-user-accounts"
	// removedUserAccountsLoggedFixup marks that RemovedUserAccountsSummary
	// has handed out its one-time log line.
	removedUserAccountsLoggedFixup = "removed-user-accounts-logged"
	// SchemaCreatedFixup is inserted by schema.sql itself: the database's
	// schema was created by a version with API-key auth, by whichever
	// process ran it (the server, a CLI subcommand, psql), so the legacy
	// import has nothing to carry over. Migrations never write it.
	SchemaCreatedFixup = "schema-created-with-api-key-auth"
)

// Names of the keys the engine creates itself.
const (
	LegacyWriteTokenKeyName = "legacy WRITE_TOKEN"
	LegacyAuthTokenKeyName  = "legacy AUTH_TOKEN"
	BootstrapAdminKeyName   = "bootstrap admin"
)

// LegacyMode is the auth mode an old engine derived from its variables.
type LegacyMode string

const (
	LegacyModeOpen           LegacyMode = "open"             // no AUTH_TOKEN, WRITE_TOKEN or ADMIN_PASSWORD
	LegacyModeWriteTokenOnly LegacyMode = "write-token-only" // only WRITE_TOKEN
	LegacyModeToken          LegacyMode = "token"            // AUTH_TOKEN without ADMIN_PASSWORD
	LegacyModeFull           LegacyMode = "full"             // ADMIN_PASSWORD
)

// Why a set legacy variable was not imported as a key (LegacySkipped.Reason).
const (
	SkipFreshDatabase     = "fresh_database"                // the schema was created by this version (SchemaCreatedFixup)
	SkipAuthDisabled      = "auth_disabled"                 // AUTH_ENABLED=false cleared it
	SkipPublicToken       = "public_token"                  // full-mode AUTH_TOKEN: /auth-init handed it out
	SkipPublishedAsPublic = "published_as_public_token"     // full-mode WRITE_TOKEN equal to AUTH_TOKEN
	SkipShellSubstitution = "unexpanded_shell_substitution" // contains "$("
	SkipSameAsWriteToken  = "same_as_write_token"           // token-mode AUTH_TOKEN equal to WRITE_TOKEN
)

// LegacyAuthInput is the raw old auth configuration, as found in the
// environment, for ImportLegacyAuth.
type LegacyAuthInput struct {
	AuthEnabled   string // AUTH_ENABLED as set; "" means unset (the old default, true)
	AuthToken     string // AUTH_TOKEN
	WriteToken    string // WRITE_TOKEN
	AdminPassword string // ADMIN_PASSWORD; only whether it is set matters
	JWTSecret     string // JWT_SECRET; only whether it is set matters
	// UploadInstanceID is UPLOAD_INSTANCE_ID, whose recent calls show that
	// HTTP uploads were in use.
	UploadInstanceID string
	// FreshDatabase: InitSchema created the schema in this process, so there
	// is nothing to carry over. ImportLegacyAuth also treats a database
	// carrying SchemaCreatedFixup as fresh, whichever process created it.
	FreshDatabase bool
}

// LegacyImportedKey is a legacy variable imported as an API key.
type LegacyImportedKey struct {
	Variable string      `json:"variable"` // "AUTH_TOKEN" or "WRITE_TOKEN"
	KeyID    int         `json:"key_id"`
	Name     string      `json:"name"`
	Scopes   auth.Scopes `json:"scopes"`
	// Existing: the secret's hash was already stored, so that key was kept
	// (not duplicated, not changed).
	Existing bool `json:"existing,omitempty"`
	Weak     bool `json:"weak,omitempty"`
	Length   int  `json:"length,omitempty"` // characters; recorded only for weak secrets
}

// LegacySkipped is a set legacy variable that was not imported.
type LegacySkipped struct {
	Variable string `json:"variable"`
	Reason   string `json:"reason"` // one of the Skip* codes
}

// LegacyKeyRef names a key in the import's upload report.
type LegacyKeyRef struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// LegacyAuthDetail is what ImportLegacyAuth decided; it is stored in
// data_fixups.detail. It never holds a secret or a character of one.
type LegacyAuthDetail struct {
	OldMode            LegacyMode `json:"old_mode"`
	VariablesSet       []string   `json:"variables_set"` // names of the old variables that were set
	AuthEnabledInvalid bool       `json:"auth_enabled_invalid,omitempty"`
	JWTSecretDropped   bool       `json:"jwt_secret_dropped,omitempty"`
	FreshDatabase      bool       `json:"fresh_database"`

	Imported []LegacyImportedKey `json:"imported"`
	Skipped  []LegacySkipped     `json:"skipped"`
	// RetiredPublicToken: the full-mode AUTH_TOKEN's hash was stored as
	// auth_settings.retired_public_token.
	RetiredPublicToken bool `json:"retired_public_token"`

	// AnonymousAccess is the policy after the import. AnonymousKept: a policy
	// was already stored (set with the CLI before this start) and was left
	// alone. SuggestAnonymousListen: the old mode was write-token-only and the
	// policy stayed off, so the operator may want `access set --anonymous
	// listen`.
	AnonymousAccess        string `json:"anonymous_access"`
	AnonymousKept          bool   `json:"anonymous_access_kept,omitempty"`
	SuggestAnonymousListen bool   `json:"suggest_anonymous_listen,omitempty"`

	// UploadsInUse: calls from UPLOAD_INSTANCE_ID in the last 7 days.
	// NoUploadKey: ... and no active key can upload after the import.
	// RecentKeysWithoutUpload: migrated (non-legacy) active keys without
	// upload that were used in the last 7 days; filled when UploadsInUse.
	UploadsInUse            bool           `json:"uploads_in_use"`
	NoUploadKey             bool           `json:"no_upload_key"`
	RecentKeysWithoutUpload []LegacyKeyRef `json:"recent_keys_without_upload"`

	// Decisions are the same decisions in words, in order.
	Decisions []string `json:"decisions"`
}

// ImportedKey returns the key a legacy variable ("AUTH_TOKEN",
// "WRITE_TOKEN") was imported as.
func (d LegacyAuthDetail) ImportedKey(variable string) (LegacyImportedKey, bool) {
	for _, k := range d.Imported {
		if k.Variable == variable {
			return k, true
		}
	}
	return LegacyImportedKey{}, false
}

// SkipReason returns why a set legacy variable was not imported.
func (d LegacyAuthDetail) SkipReason(variable string) (string, bool) {
	for _, s := range d.Skipped {
		if s.Variable == variable {
			return s.Reason, true
		}
	}
	return "", false
}

// Summary is the import's one-line log message.
func (d LegacyAuthDetail) Summary() string {
	return "legacy auth import (old mode " + string(d.OldMode) + "): " + strings.Join(d.Decisions, "; ")
}

func (d *LegacyAuthDetail) decide(format string, args ...any) {
	d.Decisions = append(d.Decisions, fmt.Sprintf(format, args...))
}

// LegacyAuthResult is what ImportLegacyAuth reports.
type LegacyAuthResult struct {
	// Ran: this call made the import. When false it ran on an earlier start,
	// and Detail and ImportedAt are what it recorded then.
	Ran        bool
	ImportedAt time.Time
	Detail     LegacyAuthDetail
	// WeakKeys are the active imported keys with a secret under
	// LegacyMinLength characters, for the WARN logged on every start.
	WeakKeys []WeakLegacyKey
}

// legacyConfig is an old engine's effective auth configuration.
type legacyConfig struct {
	mode                  LegacyMode
	authToken, writeToken string
	authDisabled          bool // AUTH_ENABLED=false cleared every credential
	authEnabledInvalid    bool // AUTH_ENABLED didn't parse; the old engine refused to start
	jwtSecretDropped      bool // JWT_SECRET without ADMIN_PASSWORD
}

// deriveLegacyConfig reproduces the old config.Load: AUTH_ENABLED=false
// clears every credential, JWT_SECRET without ADMIN_PASSWORD is dropped, and
// the mode follows from which of ADMIN_PASSWORD, AUTH_TOKEN and WRITE_TOKEN
// remain. An AUTH_ENABLED the old parser rejected made the old engine refuse
// to start; it is taken as the default (enabled), which imports credentials
// rather than opening access.
func deriveLegacyConfig(in LegacyAuthInput) legacyConfig {
	c := legacyConfig{authToken: in.AuthToken, writeToken: in.WriteToken}
	adminPassword, jwtSecret := in.AdminPassword, in.JWTSecret
	if in.AuthEnabled != "" {
		enabled, err := strconv.ParseBool(in.AuthEnabled)
		if err != nil {
			c.authEnabledInvalid = true
		} else if !enabled {
			c.authDisabled = true
			c.authToken, c.writeToken, adminPassword, jwtSecret = "", "", "", ""
		}
	}
	if jwtSecret != "" && adminPassword == "" {
		c.jwtSecretDropped = true
	}
	switch {
	case adminPassword != "":
		c.mode = LegacyModeFull
	case c.authToken != "":
		c.mode = LegacyModeToken
	case c.writeToken != "":
		c.mode = LegacyModeWriteTokenOnly
	default:
		c.mode = LegacyModeOpen
	}
	return c
}

// variablesSet lists which old auth variables were set, by name.
func (in LegacyAuthInput) variablesSet() []string {
	out := []string{}
	for _, v := range []struct{ name, val string }{
		{"AUTH_ENABLED", in.AuthEnabled},
		{"AUTH_TOKEN", in.AuthToken},
		{"WRITE_TOKEN", in.WriteToken},
		{"ADMIN_PASSWORD", in.AdminPassword},
		{"JWT_SECRET", in.JWTSecret},
	} {
		if v.val != "" {
			out = append(out, v.name)
		}
	}
	return out
}

// ImportLegacyAuth carries an old engine's auth configuration over, once per
// database (§11.2). It claims LegacyAuthImportFixup and, in one transaction:
//
//  1. derives the old mode (deriveLegacyConfig);
//  2. on a fresh database (FreshDatabase, or schema.sql's
//     SchemaCreatedFixup marker: the schema was created by this version,
//     not upgraded from an old one) imports nothing and leaves anonymous
//     access off;
//  3. imports WRITE_TOKEN as "legacy WRITE_TOKEN" with ["admin","upload"],
//     unless the old mode was full and it equals AUTH_TOKEN (then it was
//     public, and nothing is imported);
//  4. in token mode imports AUTH_TOKEN as "legacy AUTH_TOKEN" with
//     ["listen"], plus upload when WRITE_TOKEN is unset, unless it equals
//     WRITE_TOKEN (already handled in step 3);
//  5. in full mode stores AUTH_TOKEN's hash as the retired public token;
//  6. sets anonymous access to listen (unrestricted) if the old mode was open
//     and systems exist, or full with AUTH_TOKEN set, unless a policy is
//     already stored; otherwise leaves it as it is (off by default);
//  7. refuses secrets containing "$(" and flags ones under 16 characters
//     (CheckLegacySecret); a secret whose hash is stored already is not
//     duplicated;
//  8. checks whether HTTP uploads were in use without a key that can still
//     upload.
//
// Every decision is recorded in data_fixups.detail. On later starts it
// returns what was recorded, with Ran false. WeakKeys is filled every time.
func (db *DB) ImportLegacyAuth(ctx context.Context, in LegacyAuthInput) (LegacyAuthResult, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return LegacyAuthResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Claim first; a concurrent start blocks on the primary key until this
	// transaction ends, then finds the row and reports what was recorded.
	claim, err := tx.Exec(ctx, `INSERT INTO data_fixups (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		LegacyAuthImportFixup)
	if err != nil {
		return LegacyAuthResult{}, fmt.Errorf("claim legacy auth import: %w", err)
	}
	if claim.RowsAffected() == 0 {
		tx.Rollback(ctx)
		return db.recordedLegacyAuthImport(ctx)
	}

	// Fresh is a property of the database, not of which process created
	// its schema: `tr-engine import`/`keys`/`access`, psql -f schema.sql,
	// or a server that stopped before this import committed all create it
	// with the marker.
	if !in.FreshDatabase {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM data_fixups WHERE name = $1)`,
			SchemaCreatedFixup).Scan(&in.FreshDatabase); err != nil {
			return LegacyAuthResult{}, fmt.Errorf("check schema origin: %w", err)
		}
	}
	d, err := importLegacyAuth(ctx, tx, in)
	if err != nil {
		return LegacyAuthResult{}, err
	}
	detail, err := json.Marshal(d)
	if err != nil {
		return LegacyAuthResult{}, fmt.Errorf("encode legacy auth import detail: %w", err)
	}
	var appliedAt time.Time
	if err := tx.QueryRow(ctx, `UPDATE data_fixups SET detail = $2::jsonb WHERE name = $1 RETURNING applied_at`,
		LegacyAuthImportFixup, string(detail)).Scan(&appliedAt); err != nil {
		return LegacyAuthResult{}, fmt.Errorf("record legacy auth import: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LegacyAuthResult{}, fmt.Errorf("commit legacy auth import: %w", err)
	}

	weak, err := db.WeakLegacyKeys(ctx)
	if err != nil {
		return LegacyAuthResult{}, err
	}
	return LegacyAuthResult{Ran: true, ImportedAt: appliedAt, Detail: d, WeakKeys: weak}, nil
}

// LegacyAuthImportPending reports whether the next server start will run
// the one-time legacy import on an upgraded database: the import hasn't
// run, and schema.sql didn't create the database (SchemaCreatedFixup), so
// it may still import AUTH_TOKEN/WRITE_TOKEN as keys and set the anonymous
// policy.
func (db *DB) LegacyAuthImportPending(ctx context.Context) (bool, error) {
	var done bool
	err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM data_fixups WHERE name IN ($1, $2))`,
		LegacyAuthImportFixup, SchemaCreatedFixup).Scan(&done)
	return !done, err
}

func (db *DB) recordedLegacyAuthImport(ctx context.Context) (LegacyAuthResult, error) {
	var res LegacyAuthResult
	var detail []byte
	if err := db.Pool.QueryRow(ctx, `SELECT applied_at, detail FROM data_fixups WHERE name = $1`,
		LegacyAuthImportFixup).Scan(&res.ImportedAt, &detail); err != nil {
		return LegacyAuthResult{}, fmt.Errorf("read legacy auth import: %w", err)
	}
	if detail != nil && string(detail) != "null" {
		if err := json.Unmarshal(detail, &res.Detail); err != nil {
			return LegacyAuthResult{}, fmt.Errorf("decode legacy auth import detail: %w", err)
		}
	}
	var err error
	if res.WeakKeys, err = db.WeakLegacyKeys(ctx); err != nil {
		return LegacyAuthResult{}, err
	}
	return res, nil
}

// importLegacyAuth runs the import procedure inside the claiming transaction.
func importLegacyAuth(ctx context.Context, tx pgx.Tx, in LegacyAuthInput) (LegacyAuthDetail, error) {
	c := deriveLegacyConfig(in)
	d := LegacyAuthDetail{
		OldMode:                 c.mode,
		VariablesSet:            in.variablesSet(),
		AuthEnabledInvalid:      c.authEnabledInvalid,
		JWTSecretDropped:        c.jwtSecretDropped,
		FreshDatabase:           in.FreshDatabase,
		Imported:                []LegacyImportedKey{},
		Skipped:                 []LegacySkipped{},
		RecentKeysWithoutUpload: []LegacyKeyRef{},
	}
	if c.authEnabledInvalid {
		d.decide("AUTH_ENABLED=%q is not a boolean (the old engine refused to start); treated as enabled", in.AuthEnabled)
	}
	skip := func(variable, reason, why string) {
		d.Skipped = append(d.Skipped, LegacySkipped{Variable: variable, Reason: reason})
		d.decide("%s not imported: %s", variable, why)
	}

	anon, anonStored, err := storedAnonymousAccess(ctx, tx)
	if err != nil {
		return d, err
	}
	d.AnonymousAccess = anon.Access

	// Step 2: nothing to carry over into a database this process created.
	if in.FreshDatabase {
		for _, v := range []struct{ name, val string }{{"WRITE_TOKEN", in.WriteToken}, {"AUTH_TOKEN", in.AuthToken}} {
			if v.val != "" {
				skip(v.name, SkipFreshDatabase, "fresh database")
			}
		}
		d.decide("fresh database: nothing imported, anonymous access %s", d.AnonymousAccess)
		return d, nil
	}
	if c.authDisabled {
		for _, v := range []struct{ name, val string }{{"WRITE_TOKEN", in.WriteToken}, {"AUTH_TOKEN", in.AuthToken}} {
			if v.val != "" {
				skip(v.name, SkipAuthDisabled, "AUTH_ENABLED=false disabled it")
			}
		}
	}

	importKey := func(variable, secret, name string, scopes auth.Scopes) error {
		if _, err := CheckLegacySecret(secret); errors.Is(err, ErrLegacySecretUnexpanded) {
			skip(variable, SkipShellSubstitution, `refused: the value contains "$(", an unexpanded shell substitution`)
			return nil
		}
		key, created, err := createLegacyAPIKey(ctx, tx, secret, NewAPIKey{Name: name, Scopes: scopes})
		if err != nil {
			return fmt.Errorf("import %s: %w", variable, err)
		}
		k := LegacyImportedKey{Variable: variable, KeyID: key.ID, Name: key.Name, Scopes: key.Scopes, Existing: !created}
		if weak, _ := CheckLegacySecret(secret); weak {
			k.Weak, k.Length = true, len([]rune(secret))
		}
		d.Imported = append(d.Imported, k)
		switch {
		case k.Existing:
			d.decide("%s already stored as API key #%d %q; not duplicated", variable, k.KeyID, k.Name)
		case k.Weak:
			d.decide("%s imported as API key #%d %q %v (weak: %d characters)", variable, k.KeyID, k.Name, k.Scopes.Strings(), k.Length)
		default:
			d.decide("%s imported as API key #%d %q %v", variable, k.KeyID, k.Name, k.Scopes.Strings())
		}
		return nil
	}

	// Step 3: WRITE_TOKEN.
	if c.writeToken != "" {
		if c.mode == LegacyModeFull && c.writeToken == c.authToken {
			skip("WRITE_TOKEN", SkipPublishedAsPublic, "it equals AUTH_TOKEN, which /auth-init published as the public read token")
		} else if err := importKey("WRITE_TOKEN", c.writeToken, LegacyWriteTokenKeyName,
			auth.Scopes{auth.ScopeAdmin, auth.ScopeUpload}); err != nil {
			return d, err
		}
	}

	// Steps 4 and 5: AUTH_TOKEN.
	if c.authToken != "" {
		switch c.mode {
		case LegacyModeToken:
			if c.authToken == c.writeToken {
				skip("AUTH_TOKEN", SkipSameAsWriteToken, "it equals WRITE_TOKEN")
			} else {
				scopes := auth.Scopes{auth.ScopeListen}
				if c.writeToken == "" {
					scopes = append(scopes, auth.ScopeUpload)
				}
				if err := importKey("AUTH_TOKEN", c.authToken, LegacyAuthTokenKeyName, scopes); err != nil {
					return d, err
				}
			}
		case LegacyModeFull:
			if err := setRetiredPublicTokenHash(ctx, tx, HashAPIKey(c.authToken)); err != nil {
				return d, fmt.Errorf("retire public AUTH_TOKEN: %w", err)
			}
			d.RetiredPublicToken = true
			skip("AUTH_TOKEN", SkipPublicToken, "it was public (/auth-init handed it out); requests carrying it are treated as anonymous")
		}
	}

	// Step 6: the anonymous policy.
	want, why := AccessOff, ""
	switch c.mode {
	case LegacyModeOpen:
		var systems bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM systems)`).Scan(&systems); err != nil {
			return d, fmt.Errorf("check systems: %w", err)
		}
		if systems {
			want, why = AccessListen, "the old engine was open and has data"
		}
	case LegacyModeFull:
		if c.authToken != "" {
			want, why = AccessListen, "the old engine published AUTH_TOKEN for public reads"
		}
	}
	switch {
	case anonStored:
		d.AnonymousKept = true
		d.decide("anonymous access already set to %s; kept", anon.Access)
	case want == AccessListen:
		if _, err := setAnonymousAccess(ctx, tx, AccessListen, nil); err != nil {
			return d, fmt.Errorf("set anonymous access: %w", err)
		}
		d.AnonymousAccess = AccessListen
		d.decide("anonymous access set to listen: %s", why)
	default:
		d.decide("anonymous access stays %s", d.AnonymousAccess)
	}
	if d.AnonymousAccess == AccessOff && c.mode == LegacyModeWriteTokenOnly {
		d.SuggestAnonymousListen = true
	}

	// Step 8: were uploads in use, and can anything still upload? Uploaded
	// calls don't carry calls.instance_id (the upload pipeline leaves it
	// NULL); they belong to sites created for UPLOAD_INSTANCE_ID.
	if in.UploadInstanceID != "" {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM calls
			WHERE start_time > now() - interval '7 days'
			  AND (instance_id = $1
			       OR site_id IN (SELECT site_id FROM sites WHERE instance_id = $1)))`,
			in.UploadInstanceID).Scan(&d.UploadsInUse); err != nil {
			return d, fmt.Errorf("check recent uploads: %w", err)
		}
	}
	if d.UploadsInUse {
		var canUpload bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys
			WHERE 'upload' = ANY (scopes) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()))`,
		).Scan(&canUpload); err != nil {
			return d, fmt.Errorf("check upload keys: %w", err)
		}
		d.NoUploadKey = !canUpload
		rows, err := tx.Query(ctx, `SELECT id, name, key_prefix FROM api_keys
			WHERE NOT legacy AND NOT ('upload' = ANY (scopes))
			  AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
			  AND last_used_at > now() - interval '7 days'
			ORDER BY id`)
		if err != nil {
			return d, fmt.Errorf("list keys without upload: %w", err)
		}
		for rows.Next() {
			var k LegacyKeyRef
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix); err != nil {
				rows.Close()
				return d, err
			}
			d.RecentKeysWithoutUpload = append(d.RecentKeysWithoutUpload, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return d, err
		}
		if d.NoUploadKey {
			d.decide("HTTP uploads were in use but no key can upload now")
		}
	}
	return d, nil
}

// storedAnonymousAccess reads the anonymous policy inside tx and reports
// whether one is stored.
func storedAnonymousAccess(ctx context.Context, tx pgx.Tx) (AnonymousAccess, bool, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM auth_settings WHERE name = $1`, settingAnonymousAccess).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnonymousAccess{Access: AccessOff}, false, nil
	}
	if err != nil {
		return AnonymousAccess{}, false, err
	}
	a, err := decodeAnonymousAccess(raw)
	return a, true, err
}

// ClaimBootstrapAdminKey makes the one-time bootstrap claim (§10.1), after
// migrations and the legacy import. The first call per database claims
// BootstrapAdminKeyFixup and, if no active admin key exists, creates the key
// "bootstrap admin" with ["admin"] and returns its plaintext (print it once;
// it is stored nowhere). If an admin key exists it mints nothing and returns
// an empty plaintext and nil key. Every later call returns alreadyClaimed and
// never mints, even when no active admin key is left: recovery goes through
// the CLI.
func (db *DB) ClaimBootstrapAdminKey(ctx context.Context) (plaintext string, key *APIKey, alreadyClaimed bool, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return "", nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, apiKeyAdminLockKey); err != nil {
		return "", nil, false, fmt.Errorf("lock admin keys: %w", err)
	}
	claim, err := tx.Exec(ctx, `INSERT INTO data_fixups (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		BootstrapAdminKeyFixup)
	if err != nil {
		return "", nil, false, fmt.Errorf("claim bootstrap admin key: %w", err)
	}
	if claim.RowsAffected() == 0 {
		return "", nil, true, nil
	}

	var adminExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE `+activeAdminKeySQL+`)`).Scan(&adminExists); err != nil {
		return "", nil, false, fmt.Errorf("check admin keys: %w", err)
	}
	detail := map[string]any{"minted": false, "reason": "an active admin key already existed"}
	if !adminExists {
		var hash, prefix string
		if plaintext, hash, prefix, err = GenerateAPIKey(); err != nil {
			return "", nil, false, err
		}
		if key, err = insertAPIKey(ctx, tx, hash, prefix, false,
			NewAPIKey{Name: BootstrapAdminKeyName, Scopes: auth.Scopes{auth.ScopeAdmin}}); err != nil {
			return "", nil, false, fmt.Errorf("create bootstrap admin key: %w", err)
		}
		detail = map[string]any{"minted": true, "key_id": key.ID, "prefix": key.Prefix}
	}
	js, err := json.Marshal(detail)
	if err != nil {
		return "", nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE data_fixups SET detail = $2::jsonb WHERE name = $1`,
		BootstrapAdminKeyFixup, string(js)); err != nil {
		return "", nil, false, fmt.Errorf("record bootstrap admin key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, false, fmt.Errorf("commit: %w", err)
	}
	return plaintext, key, false, nil
}

// RemovedUserAccount is one entry of the RemovedUserAccountsFixup detail.
type RemovedUserAccount struct {
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	Enabled   bool       `json:"enabled"`
	LastLogin *time.Time `json:"last_login"`
}

// RemovedUserAccountsSummary returns, once per database, the log line about
// the user accounts the "record and drop users" migration removed:
// "removed 3 user accounts: alice (admin), bob (editor), carol (viewer,
// disabled) — give each person or their client an API key". It returns ""
// when there is nothing to say (no users were removed, or the line was
// handed out before).
func (db *DB) RemovedUserAccountsSummary(ctx context.Context) (string, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var detail []byte
	err = tx.QueryRow(ctx, `SELECT detail FROM data_fixups WHERE name = $1`, RemovedUserAccountsFixup).Scan(&detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read removed user accounts: %w", err)
	}
	claim, err := tx.Exec(ctx, `INSERT INTO data_fixups (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		removedUserAccountsLoggedFixup)
	if err != nil {
		return "", fmt.Errorf("claim removed user accounts summary: %w", err)
	}
	if claim.RowsAffected() == 0 {
		return "", nil
	}
	var users []RemovedUserAccount
	if detail != nil {
		if err := json.Unmarshal(detail, &users); err != nil {
			return "", fmt.Errorf("decode removed user accounts: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return formatRemovedUserAccounts(users), nil
}

func formatRemovedUserAccounts(users []RemovedUserAccount) string {
	if len(users) == 0 {
		return ""
	}
	parts := make([]string, len(users))
	for i, u := range users {
		if u.Enabled {
			parts[i] = fmt.Sprintf("%s (%s)", u.Username, u.Role)
		} else {
			parts[i] = fmt.Sprintf("%s (%s, disabled)", u.Username, u.Role)
		}
	}
	noun := "accounts"
	if len(users) == 1 {
		noun = "account"
	}
	return fmt.Sprintf("removed %d user %s: %s — give each person or their client an API key",
		len(users), noun, strings.Join(parts, ", "))
}
