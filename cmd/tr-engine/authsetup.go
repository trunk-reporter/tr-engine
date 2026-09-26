package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// setupAuth runs the access-control part of startup, after migrations
// (§10.1, §11.2): the one-time import of the removed auth variables, the
// warnings for those still set, the one-time summary of removed user
// accounts, the ticket secret, the bootstrap admin key (printed to stderr
// once per database) and the anonymous policy. An error is fatal.
func setupAuth(ctx context.Context, db *database.DB, cfg *config.Config, freshDatabase, isDocker bool,
	stderr io.Writer, log zerolog.Logger) error {
	legacy := cfg.LegacyAuth
	res, err := db.ImportLegacyAuth(ctx, database.LegacyAuthInput{
		AuthEnabled:      legacy.AuthEnabled,
		AuthToken:        legacy.AuthToken,
		WriteToken:       legacy.WriteToken,
		AdminPassword:    legacy.AdminPassword,
		JWTSecret:        legacy.JWTSecret,
		UploadInstanceID: cfg.UploadInstanceID,
		FreshDatabase:    freshDatabase,
	})
	if err != nil {
		return fmt.Errorf("legacy auth import failed (it is retried on the next start): %w", err)
	}
	if res.Ran {
		logLegacyImport(res.Detail, log)
	}
	warnLegacyVariables(ctx, db, legacy, res, log)
	for _, k := range res.WeakKeys {
		log.Warn().Int("key_id", k.ID).Str("name", k.Name).Str("prefix", k.Prefix).
			Msgf("legacy key #%d is weak (%d characters) — replace it", k.ID, k.Length)
	}

	if summary, err := db.RemovedUserAccountsSummary(ctx); err != nil {
		log.Warn().Err(err).Msg("reading the removed user accounts failed")
	} else if summary != "" {
		log.Warn().Msg(summary)
	}

	if _, err := db.GetOrCreateTicketSecret(ctx); err != nil {
		return fmt.Errorf("ticket secret: %w", err)
	}

	plaintext, key, _, err := db.ClaimBootstrapAdminKey(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap admin key: %w", err)
	}
	if plaintext != "" {
		printBootstrapBanner(stderr, plaintext, isDocker)
		log.Warn().Int("key_id", key.ID).Str("prefix", key.Prefix).
			Msg("no admin API key existed, so the bootstrap admin key was created and printed to stderr (shown once)")
	} else if exists, err := db.ActiveAdminKeyExists(ctx); err != nil {
		log.Warn().Err(err).Msg("checking for an active admin API key failed")
	} else if !exists {
		log.Error().Msg(`no active admin API key exists (every admin key was revoked, expired or lost the admin scope) — create one on the host with: tr-engine keys create --name "admin" --scopes admin`)
	}

	anon, err := db.GetAnonymousAccess(ctx)
	if err != nil {
		return fmt.Errorf("anonymous access policy: %w", err)
	}
	ev := log.Info().Str("access", anon.Access).Bool("restricted", anon.Restriction != nil)
	if r := anon.Restriction; r != nil {
		ev = ev.Bool("allow_all", r.AllowAll).Int("systems", len(r.Systems)).
			Int("talkgroups", len(r.Talkgroups)).Int("exclude_talkgroups", len(r.ExcludeTalkgroups))
	}
	ev.Msg("anonymous access policy (requests without an API key); change it with tr-engine access set")
	return nil
}

// logLegacyImport logs what the one-time legacy import decided, on the start
// that ran it.
func logLegacyImport(d database.LegacyAuthDetail, log zerolog.Logger) {
	log.Info().Str("old_mode", string(d.OldMode)).Str("fixup", database.LegacyAuthImportFixup).Msg(d.Summary())
	for _, s := range d.Skipped {
		switch s.Reason {
		case database.SkipPublishedAsPublic:
			log.Error().Msg("WRITE_TOKEN was published by /auth-init as the public read token; not imported — create a new admin key")
		case database.SkipShellSubstitution:
			log.Error().Str("variable", s.Variable).
				Msgf(`%s contains "$(" (an unexpanded shell substitution copied from old docs); not imported — create a new key with tr-engine keys create`, s.Variable)
		}
	}
	if d.SuggestAnonymousListen {
		log.Info().Msg("anonymous access stays off; for public read access without a key run: tr-engine access set --anonymous listen")
	}
	if d.NoUploadKey {
		log.Warn().Msg("HTTP uploads were in use but no key can upload now — create one with `tr-engine keys create --name 'uploads' --scopes upload` and configure it in trunk-recorder")
	}
	if len(d.RecentKeysWithoutUpload) > 0 {
		refs := make([]string, len(d.RecentKeysWithoutUpload))
		for i, k := range d.RecentKeysWithoutUpload {
			refs[i] = fmt.Sprintf("#%d %q (%s)", k.ID, k.Name, k.Prefix)
		}
		log.Warn().Strs("keys", refs).
			Msg("migrated keys without the upload scope were used in the last 7 days; if one of them uploads calls, give it upload (tr-engine keys update ID --scopes ...,upload)")
	}
}

// warnLegacyVariables logs one WARN per removed auth variable that is still
// set, on every start (§11.2). For AUTH_TOKEN and WRITE_TOKEN it first looks
// up what this process's value is now (checkLegacyValue): the import's record
// describes the value set when it ran, which another engine on the same
// database, or a changed .env, may not share.
func warnLegacyVariables(ctx context.Context, db *database.DB, legacy config.LegacyAuthEnv,
	res database.LegacyAuthResult, log zerolog.Logger) {
	for _, v := range legacy.Set() {
		var check *legacyValueCheck
		if v.Name == "AUTH_TOKEN" || v.Name == "WRITE_TOKEN" {
			c, err := checkLegacyValue(ctx, db, v.Value)
			if err != nil {
				log.Warn().Err(err).Str("variable", v.Name).Msg("looking up the value of a removed auth variable failed")
			} else {
				c.value = v.Value
				check = &c
			}
		}
		log.Warn().Str("variable", v.Name).Msg(legacyVariableWarning(v.Name, res, check))
	}
}

// legacyValueCheck is what the engine does with a set AUTH_TOKEN or
// WRITE_TOKEN value now.
type legacyValueCheck struct {
	value     string
	keyID     int                // the API key whose secret it is (any status); 0 = none
	keyStatus database.KeyStatus // that key's status
	// retired: the stored retired public AUTH_TOKEN; requests carrying it
	// are treated as anonymous (it is checked before the keys).
	retired bool
	// forgotten: a retired public AUTH_TOKEN forgotten with `tr-engine access
	// forget-retired-token`; requests carrying it are looked up as a key.
	forgotten bool
}

func checkLegacyValue(ctx context.Context, db *database.DB, value string) (legacyValueCheck, error) {
	var c legacyValueCheck
	hash := database.HashAPIKey(value)
	k, err := db.ResolveAPIKeyByHash(ctx, hash)
	switch {
	case err == nil:
		c.keyID, c.keyStatus = k.ID, k.Status
	case !errors.Is(err, database.ErrAPIKeyNotFound):
		return c, err
	}
	// The same test the API makes (authenticator.resolveKey): only the
	// stored retired token counts as no credential.
	stored, err := db.GetRetiredPublicTokenHash(ctx)
	if err != nil {
		return c, err
	}
	c.retired = stored != "" && stored == hash
	if !c.retired {
		// IsRetiredPublicToken also matches the forgotten ones.
		if c.forgotten, err = db.IsRetiredPublicToken(ctx, hash); err != nil {
			return c, err
		}
	}
	return c, nil
}

// keyStatusNote is added to a message naming the key check.keyID when that
// key no longer works.
func keyStatusNote(check *legacyValueCheck) string {
	if check == nil || check.keyID == 0 {
		return ""
	}
	switch check.keyStatus {
	case database.KeyRevoked, database.KeyExpired:
		return "; that key is " + string(check.keyStatus) + ", so clients sending it get 401 invalid_key"
	}
	return ""
}

// legacyVariableWarning is the per-start message for a removed auth variable
// that is still set. check is what this process's AUTH_TOKEN or WRITE_TOKEN
// value is now (nil when unknown: the message then follows the import's
// record alone). The import's record is used only when it describes this
// value; otherwise the message says what the value is now.
func legacyVariableWarning(name string, res database.LegacyAuthResult, check *legacyValueCheck) string {
	const remove = " — remove it from your configuration; see docs/migrating-auth.md"
	switch name {
	case "AUTH_TOKEN", "WRITE_TOKEN":
		date := res.ImportedAt.UTC().Format("2006-01-02")
		// plain: the value is no key and no retired or forgotten token (or
		// unknown), so a record that doesn't depend on the value describes it.
		plain := check == nil || (check.keyID == 0 && !check.retired && !check.forgotten)
		// recorded: the import recorded another value of this variable.
		recorded := false
		if k, ok := res.Detail.ImportedKey(name); ok {
			if check == nil || (check.keyID == k.KeyID && !check.retired) {
				return fmt.Sprintf("%s is no longer used (imported as API key #%d '%s' on %s%s)%s",
					name, k.KeyID, k.Name, date, keyStatusNote(check), remove)
			}
			recorded = true
		} else if reason, ok := res.Detail.SkipReason(name); ok {
			switch reason {
			case database.SkipPublicToken:
				switch {
				case check == nil || check.retired:
					return name + " is no longer used (it was the public read token, so it was not imported; requests that still carry it are treated as anonymous)" + remove
				case check.forgotten:
					return name + " is no longer used (it was the public read token, so it was not imported, and it was forgotten with tr-engine access forget-retired-token: requests that still carry it get 401 invalid_key)" + remove
				}
				recorded = true
			case database.SkipPublishedAsPublic:
				if check == nil || check.retired || check.forgotten {
					return name + " is no longer used (it equalled the public AUTH_TOKEN, so it was not imported)" + remove
				}
				recorded = true
			case database.SkipShellSubstitution:
				if check == nil || (plain && strings.Contains(check.value, "$(")) {
					return name + ` is no longer used (it contained "$(", so it was not imported)` + remove
				}
				recorded = true
			case database.SkipSameAsWriteToken:
				w, ok := res.Detail.ImportedKey("WRITE_TOKEN")
				if check == nil || (ok && check.keyID == w.KeyID && !check.retired) {
					return name + " is no longer used (it equalled WRITE_TOKEN, imported as that key" + keyStatusNote(check) + ")" + remove
				}
				recorded = true
			case database.SkipFreshDatabase:
				if plain {
					return name + " is no longer used (not imported into a new database; clients use API keys)" + remove
				}
			case database.SkipAuthDisabled:
				if plain {
					return name + " is no longer used (not imported: AUTH_ENABLED=false disabled it)" + remove
				}
			}
		}
		// The import's record doesn't describe this value: say what it is.
		switch {
		case check == nil:
		case check.retired:
			return name + " is no longer used (its value is the retired public read token, so requests that still carry it are treated as anonymous)" + remove
		case check.forgotten:
			return name + " is no longer used (its value is a public read token that was forgotten with tr-engine access forget-retired-token: requests that still carry it get 401 invalid_key)" + remove
		case check.keyID != 0:
			return fmt.Sprintf("%s is no longer used (its value is API key #%d%s)%s", name, check.keyID, keyStatusNote(check), remove)
		case recorded:
			return name + " is set to a value the one-time import on " + date +
				" did not import (it recorded a different one), so clients sending it get 401 invalid_key: " +
				"if it is still in use, register it with tr-engine keys import; otherwise remove it. See docs/migrating-auth.md"
		}
		return name + " is no longer used — clients use API keys; register a secret that is still in use with tr-engine keys import. See docs/auth.md"
	case "ADMIN_PASSWORD", "ADMIN_USERNAME", "JWT_SECRET":
		return name + " is no longer used — tr-engine has no user accounts; clients use API keys. See docs/auth.md"
	case "AUTH_ENABLED":
		return "AUTH_ENABLED is no longer used — access is controlled by API keys and the anonymous access policy (tr-engine access show). See docs/auth.md"
	case "CORS_ORIGINS":
		return "CORS_ORIGINS is no longer needed: the API allows all origins and never uses cookies"
	}
	return name + " is no longer used. See docs/auth.md"
}

// printBootstrapBanner prints the bootstrap admin key straight to stderr as
// plain text (§10.1), bypassing the structured log so it is readable and log
// shippers don't index it as a field.
func printBootstrapBanner(w io.Writer, key string, isDocker bool) {
	rule := strings.Repeat("=", 64)
	var b strings.Builder
	b.WriteString(rule + "\n")
	b.WriteString(" tr-engine: no admin API key existed, so one was created:\n\n")
	b.WriteString("   " + key + "\n\n")
	b.WriteString(" Store it now — it will not be shown again. Paste it into a client\n")
	b.WriteString(" (tr-dashboard, web/admin.html) or send it as\n")
	b.WriteString(" \"Authorization: Bearer <key>\" to create more keys.\n\n")
	b.WriteString(" Create or revoke keys later with:\n")
	b.WriteString("   tr-engine keys --help\n")
	if isDocker {
		b.WriteString("   (in Docker: docker compose exec -T tr-engine tr-engine keys --help)\n")
	}
	b.WriteString(rule + "\n")
	io.WriteString(w, b.String())
}
