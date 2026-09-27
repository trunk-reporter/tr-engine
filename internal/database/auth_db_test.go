package database

// Runs against a real PostgreSQL; skipped unless TEST_DATABASE_URL is set (see
// export_units_db_test.go).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/snarg/tr-engine/internal/auth"
)

func scopesOf(s ...string) auth.Scopes {
	out := make(auth.Scopes, len(s))
	for i, v := range s {
		out[i] = auth.Scope(v)
	}
	return out
}

func mustCreateKey(t *testing.T, db *DB, name string, scopes ...string) *APIKeyWithPlaintext {
	t.Helper()
	k, err := db.CreateAPIKey(context.Background(), NewAPIKey{Name: name, Scopes: scopesOf(scopes...)})
	if err != nil {
		t.Fatalf("create key %q: %v", name, err)
	}
	return k
}

func isFieldErr(err error, field string) bool {
	var fe *FieldError
	return errors.As(err, &fe) && fe.Field == field
}

func TestAPIKeys_CreateResolveList(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	rps := float32(2.5)
	created, err := db.CreateAPIKey(ctx, NewAPIKey{
		Name:         "  public site backend ",
		Scopes:       scopesOf("listen"),
		Restriction:  &auth.Restriction{Systems: []int{3, 1, 3}, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 5}}},
		ExpiresAt:    &exp,
		RateLimitRPS: &rps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Plaintext, "tre_") || created.Prefix != created.Plaintext[:12] {
		t.Errorf("plaintext %q / prefix %q", created.Plaintext, created.Prefix)
	}
	if created.Name != "public site backend" || created.Status != KeyActive || created.Legacy {
		t.Errorf("created = %+v", created.APIKey)
	}
	if !slices.Equal(created.Restriction.Systems, []int{1, 3}) {
		t.Errorf("restriction not normalized: %+v", created.Restriction)
	}

	got, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(created.Plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || !got.ExpiresAt.Equal(exp) || *got.RateLimitRPS != rps ||
		!got.Restriction.Equal(created.Restriction) || !slices.Equal(got.Scopes, scopesOf("listen")) {
		t.Errorf("resolved = %+v, want %+v", got, created.APIKey)
	}
	if _, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey("tre_unknown")); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrAPIKeyNotFound", err)
	}
	if _, err := db.GetAPIKeyByID(ctx, 99999); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown id: err = %v, want ErrAPIKeyNotFound", err)
	}

	// Invalid keys are refused before anything is stored.
	for _, in := range []NewAPIKey{
		{Name: "", Scopes: scopesOf("listen")},
		{Name: "x", Scopes: scopesOf("listen", "edit")},
		{Name: "x", Scopes: scopesOf("admin"), Restriction: &auth.Restriction{AllowAll: true}},
		{Name: "x", Scopes: scopesOf("listen"), Restriction: &auth.Restriction{}},
	} {
		if _, err := db.CreateAPIKey(ctx, in); err == nil {
			t.Errorf("created invalid key %+v", in)
		}
	}

	// Expired keys resolve with their status; revoked keys too.
	expired := mustCreateKey(t, db, "soon expired", "edit")
	mustExec(t, db, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID)
	if k, err := db.GetAPIKeyByID(ctx, expired.ID); err != nil || k.Status != KeyExpired {
		t.Errorf("expired key: %+v, %v", k, err)
	}
	revoked := mustCreateKey(t, db, "revoked", "upload")
	if _, err := db.RevokeAPIKey(ctx, revoked.ID, GuardLastAdmin); err != nil {
		t.Fatal(err)
	}
	if k, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(revoked.Plaintext)); err != nil || k.Status != KeyRevoked {
		t.Errorf("revoked key: %+v, %v", k, err)
	}
	again, err := db.RevokeAPIKey(ctx, revoked.ID, GuardLastAdmin)
	if err != nil || again.RevokedAt == nil {
		t.Errorf("second revoke = %+v, %v; want idempotent success", again, err)
	}

	list, err := db.ListAPIKeys(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, k := range list {
		ids = append(ids, k.ID)
	}
	if !slices.Equal(ids, []int{created.ID, expired.ID}) {
		t.Errorf("active+expired list = %v", ids)
	}
	all, err := db.ListAPIKeys(ctx, true)
	if err != nil || len(all) != 3 || all[2].ID != revoked.ID {
		t.Errorf("list with revoked = %v, %v", all, err)
	}

	byPrefix, err := db.FindAPIKeysByPrefix(ctx, created.Prefix)
	if err != nil || len(byPrefix) != 1 || byPrefix[0].ID != created.ID {
		t.Errorf("FindAPIKeysByPrefix = %v, %v", byPrefix, err)
	}

	// Touch writes at most once a minute.
	if err := db.TouchAPIKey(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := db.GetAPIKeyByID(ctx, created.ID)
	if first.LastUsedAt == nil {
		t.Fatal("last_used_at not set")
	}
	if err := db.TouchAPIKey(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	second, _ := db.GetAPIKeyByID(ctx, created.ID)
	if !second.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Errorf("second touch within a minute moved last_used_at: %v → %v", first.LastUsedAt, second.LastUsedAt)
	}
}

func TestAPIKeys_Legacy(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	secret := "old-shared-write-token-0123456789"
	k, created, err := db.CreateLegacyAPIKey(ctx, secret, NewAPIKey{Name: "imported", Scopes: scopesOf("admin", "upload")})
	if err != nil || !created {
		t.Fatalf("CreateLegacyAPIKey = %v, %v, %v", k, created, err)
	}
	hash := HashAPIKey(secret)
	if !k.Legacy || k.Prefix != "legacy_"+hash[:6] || strings.Contains(k.Prefix, secret[:4]) {
		t.Errorf("legacy key = %+v", k)
	}
	if r, err := db.ResolveAPIKeyByHash(ctx, hash); err != nil || r.ID != k.ID {
		t.Errorf("resolve legacy = %v, %v", r, err)
	}
	// Same secret again: not duplicated, existing key returned unchanged.
	dup, created, err := db.CreateLegacyAPIKey(ctx, secret, NewAPIKey{Name: "other", Scopes: scopesOf("listen")})
	if err != nil || created || dup.ID != k.ID || dup.Name != "imported" {
		t.Errorf("duplicate import = %+v, %v, %v", dup, created, err)
	}
	if _, _, err := db.CreateLegacyAPIKey(ctx, "$(openssl rand -hex 32)", NewAPIKey{Name: "x", Scopes: scopesOf("listen")}); !errors.Is(err, ErrLegacySecretUnexpanded) {
		t.Errorf(`"$(" secret: err = %v`, err)
	}

	weak, created, err := db.CreateLegacyAPIKey(ctx, "short", NewAPIKey{Name: "weak one", Scopes: scopesOf("listen")})
	if err != nil || !created {
		t.Fatal(err)
	}
	list, err := db.WeakLegacyKeys(ctx)
	if err != nil || len(list) != 1 || list[0].ID != weak.ID || list[0].Length != 5 || list[0].Name != "weak one" {
		t.Errorf("WeakLegacyKeys = %+v, %v", list, err)
	}
	if _, err := db.RevokeAPIKey(ctx, weak.ID, GuardLastAdmin); err != nil {
		t.Fatal(err)
	}
	if list, err := db.WeakLegacyKeys(ctx); err != nil || len(list) != 0 {
		t.Errorf("WeakLegacyKeys after revoke = %+v, %v", list, err)
	}
}

func TestAPIKeys_Patch(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustCreateKey(t, db, "admin", "admin") // keeps the guard out of the way

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	rps := float32(10)
	k, err := db.CreateAPIKey(ctx, NewAPIKey{
		Name: "viewer", Scopes: scopesOf("listen"), ExpiresAt: &exp, RateLimitRPS: &rps,
		Restriction: &auth.Restriction{Talkgroups: []auth.TG{{SystemID: 1, Tgid: 100}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Absent fields are left alone.
	got, err := db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetName: true, Name: "renamed"}, GuardLastAdmin)
	if err != nil || got.Name != "renamed" || got.ExpiresAt == nil || got.RateLimitRPS == nil || got.Restriction == nil {
		t.Fatalf("rename = %+v, %v", got, err)
	}
	// An explicit null clears expires_at and rate_limit_rps.
	got, err = db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetExpiresAt: true, SetRateLimitRPS: true}, GuardLastAdmin)
	if err != nil || got.ExpiresAt != nil || got.RateLimitRPS != nil || got.Restriction == nil || got.Name != "renamed" {
		t.Fatalf("clear expiry/limit = %+v, %v", got, err)
	}
	// Changing scopes away from ["listen"] while restricted is refused...
	if _, err := db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("edit")}, GuardLastAdmin); !isFieldErr(err, "restriction") {
		t.Errorf("edit with restriction kept: err = %v, want restriction FieldError", err)
	}
	// ...unless the same patch clears the restriction.
	got, err = db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("upload", "edit"), SetRestriction: true}, GuardLastAdmin)
	if err != nil || got.Restriction != nil || strings.Join(got.Scopes.Strings(), ",") != "edit,upload" {
		t.Fatalf("edit + clear restriction = %+v, %v", got, err)
	}
	// Validation errors name the field.
	past := time.Now().Add(-time.Minute)
	zero := float32(0)
	for field, p := range map[string]APIKeyPatch{
		"name":           {SetName: true, Name: " "},
		"scopes":         {SetScopes: true, Scopes: scopesOf("listen", "admin")},
		"expires_at":     {SetExpiresAt: true, ExpiresAt: &past},
		"rate_limit_rps": {SetRateLimitRPS: true, RateLimitRPS: &zero},
		"restriction":    {SetRestriction: true, Restriction: &auth.Restriction{AllowAll: true}}, // edit key can't be restricted
	} {
		if _, err := db.PatchAPIKey(ctx, k.ID, p, GuardLastAdmin); !isFieldErr(err, field) {
			t.Errorf("%s: err = %v, want FieldError", field, err)
		}
	}
	// An expired key can still be patched (e.g. to extend it) without its
	// old expiry tripping validation.
	mustExec(t, db, `UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, k.ID)
	if got, err := db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetName: true, Name: "still expired"}, GuardLastAdmin); err != nil || got.Status != KeyExpired {
		t.Errorf("patch expired key = %+v, %v", got, err)
	}
	later := time.Now().Add(48 * time.Hour)
	if got, err := db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetExpiresAt: true, ExpiresAt: &later}, GuardLastAdmin); err != nil || got.Status != KeyActive {
		t.Errorf("extend expired key = %+v, %v", got, err)
	}

	// Patching a revoked or unknown key.
	if _, err := db.RevokeAPIKey(ctx, k.ID, GuardLastAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PatchAPIKey(ctx, k.ID, APIKeyPatch{SetName: true, Name: "x"}, GuardLastAdmin); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Errorf("patch revoked: err = %v", err)
	}
	if _, err := db.PatchAPIKey(ctx, 99999, APIKeyPatch{}, GuardLastAdmin); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("patch unknown: err = %v", err)
	}
	if _, err := db.RevokeAPIKey(ctx, 99999, GuardLastAdmin); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("revoke unknown: err = %v", err)
	}

	// Patch and revoke bump the auth generation.
	g := auth.Generation()
	other := mustCreateKey(t, db, "other", "listen")
	if _, err := db.PatchAPIKey(ctx, other.ID, APIKeyPatch{SetName: true, Name: "o"}, GuardLastAdmin); err != nil {
		t.Fatal(err)
	}
	if auth.Generation() == g {
		t.Error("PatchAPIKey did not bump the auth generation")
	}
}

func TestAPIKeys_LastAdminGuard(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	a := mustCreateKey(t, db, "a", "admin")
	b := mustCreateKey(t, db, "b", "admin", "upload")
	expiredAdmin := mustCreateKey(t, db, "expired admin", "admin")
	mustExec(t, db, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expiredAdmin.ID)

	if _, err := db.RevokeAPIKey(ctx, a.ID, GuardLastAdmin); err != nil {
		t.Fatalf("revoke one of two admin keys: %v", err)
	}
	// b is now the last active admin key (the expired one doesn't count).
	if _, err := db.RevokeAPIKey(ctx, b.ID, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("revoke last admin: err = %v, want ErrLastAdminKey", err)
	}
	if _, err := db.PatchAPIKey(ctx, b.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("edit", "upload")}, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("demote last admin: err = %v, want ErrLastAdminKey", err)
	}
	// Changes that keep admin are fine.
	if _, err := db.PatchAPIKey(ctx, b.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("admin"), SetName: true, Name: "b2"}, GuardLastAdmin); err != nil {
		t.Errorf("keep admin: %v", err)
	}
	// Changing the expired admin key's scopes isn't guarded (it isn't active).
	if _, err := db.PatchAPIKey(ctx, expiredAdmin.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("listen")}, GuardLastAdmin); err != nil {
		t.Errorf("demote expired admin: %v", err)
	}
	// The CLI isn't subject to the guard.
	if _, err := db.RevokeAPIKey(ctx, b.ID, NoLastAdminGuard); err != nil {
		t.Errorf("CLI revoke of last admin: %v", err)
	}
	if ok, err := db.ActiveAdminKeyExists(ctx); err != nil || ok {
		t.Errorf("ActiveAdminKeyExists = %v, %v; want false", ok, err)
	}
}

// A rate limit too low to lift again locks an admin key out just as an
// expiry does: the guard covers it, and a key throttled like that doesn't
// count as the admin key that remains (r2-13).
func TestAPIKeys_LastAdminGuardRateLimit(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	limit := func(id int, rps *float32) error {
		_, err := db.PatchAPIKey(ctx, id, APIKeyPatch{SetRateLimitRPS: true, RateLimitRPS: rps}, GuardLastAdmin)
		return err
	}
	rps := func(v float32) *float32 { return &v }

	only := mustCreateKey(t, db, "only", "admin")
	for _, v := range []float32{0.0001, 0.5} {
		if err := limit(only.ID, rps(v)); !errors.Is(err, ErrLastAdminKey) {
			t.Errorf("rate limit %g on the only admin key: err = %v, want ErrLastAdminKey", v, err)
		} else if !strings.Contains(err.Error(), "rate limit") {
			t.Errorf("message = %q, want it to explain the rate limit", err)
		}
	}
	// A limit it can still work with is fine, and so is raising or lifting one.
	for _, v := range []*float32{rps(1), rps(50), nil} {
		if err := limit(only.ID, v); err != nil {
			t.Errorf("rate limit %v on the only admin key: %v", v, err)
		}
	}
	// A rename leaves the limit alone and isn't guarded.
	if _, err := db.PatchAPIKey(ctx, only.ID, APIKeyPatch{SetName: true, Name: "renamed"}, GuardLastAdmin); err != nil {
		t.Errorf("rename: %v", err)
	}

	// With another usable admin key, the throttle is allowed...
	second := mustCreateKey(t, db, "second", "admin")
	if err := limit(only.ID, rps(0.0001)); err != nil {
		t.Fatalf("throttle with another admin key: %v", err)
	}
	// ...and then the other key is the last usable one: it can't be
	// revoked, demoted or throttled.
	if _, err := db.RevokeAPIKey(ctx, second.ID, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("revoke the last usable admin key: err = %v, want ErrLastAdminKey", err)
	}
	if _, err := db.PatchAPIKey(ctx, second.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("edit")}, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("demote the last usable admin key: err = %v, want ErrLastAdminKey", err)
	}
	if err := limit(second.ID, rps(0.1)); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("throttle the last usable admin key: err = %v, want ErrLastAdminKey", err)
	}
	// Lowering the throttled key further is still guarded by the usable one;
	// the CLI isn't guarded at all.
	if err := limit(only.ID, rps(0.00001)); err != nil {
		t.Errorf("lower an already throttled key: %v", err)
	}
	if _, err := db.PatchAPIKey(ctx, second.ID, APIKeyPatch{SetRateLimitRPS: true, RateLimitRPS: rps(0.1)}, NoLastAdminGuard); err != nil {
		t.Errorf("CLI throttle: %v", err)
	}
}

// An expiry is a removal on a timer: the guard covers it, and a key about to
// expire doesn't count as the admin key that remains (r1-10).
func TestAPIKeys_LastAdminGuardExpiry(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	in := func(d time.Duration) *time.Time { v := time.Now().Add(d); return &v }
	expire := func(id int, d time.Duration) error {
		_, err := db.PatchAPIKey(ctx, id, APIKeyPatch{SetExpiresAt: true, ExpiresAt: in(d)}, GuardLastAdmin)
		return err
	}

	only := mustCreateKey(t, db, "only", "admin")
	if err := expire(only.ID, 5*time.Second); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("near-future expiry on the only admin key: err = %v, want ErrLastAdminKey", err)
	}
	if err := expire(only.ID, 365*24*time.Hour); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("any expiry on the only admin key: err = %v, want ErrLastAdminKey", err)
	}

	second := mustCreateKey(t, db, "second", "admin")
	// Another admin key without an expiry outlasts it: allowed.
	if err := expire(only.ID, time.Hour); err != nil {
		t.Fatalf("expiry with another lasting admin key: %v", err)
	}
	// Now the other one can't be revoked, demoted or given an expiry: the
	// remaining key would be gone in an hour.
	if _, err := db.RevokeAPIKey(ctx, second.ID, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("revoke the lasting admin key: err = %v, want ErrLastAdminKey", err)
	} else if !strings.Contains(err.Error(), "expires before") {
		t.Errorf("message = %q, want it to explain the expiry", err)
	}
	if _, err := db.PatchAPIKey(ctx, second.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("edit")}, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("demote the lasting admin key: err = %v, want ErrLastAdminKey", err)
	}
	if err := expire(second.ID, 2*time.Hour); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("expiry on the lasting admin key: err = %v, want ErrLastAdminKey", err)
	}
	// Moving the short expiry later is never guarded; clearing it makes the
	// other key's changes fine again.
	if err := expire(only.ID, 3*time.Hour); err != nil {
		t.Errorf("later expiry: %v", err)
	}
	if _, err := db.PatchAPIKey(ctx, only.ID, APIKeyPatch{SetExpiresAt: true}, GuardLastAdmin); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	if err := expire(second.ID, 2*time.Hour); err != nil {
		t.Errorf("expiry once the other key lasts: %v", err)
	}
	// Shortening the longest-lived admin key below it is refused too...
	if err := expire(only.ID, 4*time.Hour); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("expiry on the only lasting admin key: err = %v, want ErrLastAdminKey", err)
	}
	// ...while both expiring keys can be ordered: the later one outlasts.
	if err := expire(second.ID, time.Hour); err != nil {
		t.Errorf("earlier expiry on a key another outlasts: %v", err)
	}
	third := mustCreateKey(t, db, "third", "admin")
	if _, err := db.RevokeAPIKey(ctx, second.ID, GuardLastAdmin); err != nil {
		t.Errorf("revoke a key the others outlast: %v", err)
	}
	if _, err := db.RevokeAPIKey(ctx, third.ID, GuardLastAdmin); err != nil {
		t.Errorf("revoke one of two lasting keys: %v", err)
	}
	if _, err := db.RevokeAPIKey(ctx, only.ID, GuardLastAdmin); !errors.Is(err, ErrLastAdminKey) {
		t.Errorf("revoke the last admin key: err = %v, want ErrLastAdminKey", err)
	}
}

// Concurrent requests that each take away one of the two remaining admin keys
// must not both pass the guard.
func TestAPIKeys_LastAdminGuardConcurrent(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	for round := 0; round < 25; round++ {
		mustExec(t, db, `UPDATE api_keys SET revoked_at = now() WHERE revoked_at IS NULL`)
		a := mustCreateKey(t, db, "a", "admin")
		b := mustCreateKey(t, db, "b", "admin")

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, op := range []func() error{
			func() error { _, err := db.RevokeAPIKey(ctx, a.ID, GuardLastAdmin); return err },
			func() error {
				if round%2 == 0 {
					_, err := db.RevokeAPIKey(ctx, b.ID, GuardLastAdmin)
					return err
				}
				_, err := db.PatchAPIKey(ctx, b.ID, APIKeyPatch{SetScopes: true, Scopes: scopesOf("edit")}, GuardLastAdmin)
				return err
			},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = op()
			}()
		}
		close(start)
		wg.Wait()

		var ok, guarded int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLastAdminKey):
				guarded++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || guarded != 1 {
			t.Fatalf("round %d: %d succeeded and %d were guarded, want 1 and 1", round, ok, guarded)
		}
		if exists, err := db.ActiveAdminKeyExists(ctx); err != nil || !exists {
			t.Fatalf("round %d: no active admin key left (%v)", round, err)
		}
	}
}

func TestBootstrapAdminKey(t *testing.T) {
	ctx := context.Background()

	t.Run("mints once", func(t *testing.T) {
		db := authDB(t)
		plaintext, key, already, err := db.ClaimBootstrapAdminKey(ctx)
		if err != nil || already || key == nil || !strings.HasPrefix(plaintext, "tre_") {
			t.Fatalf("first claim = %q, %+v, %v, %v", plaintext, key, already, err)
		}
		if key.Name != BootstrapAdminKeyName || !slices.Equal(key.Scopes, scopesOf("admin")) {
			t.Errorf("bootstrap key = %+v", key)
		}
		if r, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(plaintext)); err != nil || r.ID != key.ID {
			t.Errorf("bootstrap key doesn't resolve: %v, %v", r, err)
		}
		var detail string
		if err := db.Pool.QueryRow(ctx, `SELECT detail::text FROM data_fixups WHERE name = $1`, BootstrapAdminKeyFixup).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, plaintext[4:]) || !strings.Contains(detail, `"minted": true`) {
			t.Errorf("fixup detail = %s", detail)
		}
		plaintext, key, already, err = db.ClaimBootstrapAdminKey(ctx)
		if err != nil || !already || key != nil || plaintext != "" {
			t.Errorf("second claim = %q, %+v, %v, %v", plaintext, key, already, err)
		}
	})

	t.Run("not after a legacy admin import", func(t *testing.T) {
		db := upgradedDB(t)
		mustExec(t, db, `INSERT INTO systems (system_type, name) VALUES ('p25', 'x')`)
		res, err := db.ImportLegacyAuth(ctx, LegacyAuthInput{WriteToken: "write-token-0123456789"})
		if err != nil || len(res.Detail.Imported) != 1 {
			t.Fatalf("import = %+v, %v", res, err)
		}
		plaintext, key, already, err := db.ClaimBootstrapAdminKey(ctx)
		if err != nil || already || key != nil || plaintext != "" {
			t.Errorf("claim = %q, %+v, %v, %v; want claimed without a key", plaintext, key, already, err)
		}
	})

	t.Run("not re-minted after all admin keys are revoked", func(t *testing.T) {
		db := authDB(t)
		_, key, _, err := db.ClaimBootstrapAdminKey(ctx)
		if err != nil || key == nil {
			t.Fatal(err)
		}
		if _, err := db.RevokeAPIKey(ctx, key.ID, NoLastAdminGuard); err != nil {
			t.Fatal(err)
		}
		plaintext, key, already, err := db.ClaimBootstrapAdminKey(ctx)
		if err != nil || !already || key != nil || plaintext != "" {
			t.Errorf("claim after revoke = %q, %+v, %v, %v", plaintext, key, already, err)
		}
		if ok, _ := db.ActiveAdminKeyExists(ctx); ok {
			t.Error("an admin key exists")
		}
	})

	t.Run("concurrent starts mint one key", func(t *testing.T) {
		db := authDB(t)
		var wg sync.WaitGroup
		minted := make([]bool, 4)
		for i := range minted {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, key, _, err := db.ClaimBootstrapAdminKey(ctx)
				if err != nil {
					t.Error(err)
				}
				minted[i] = key != nil
			}()
		}
		wg.Wait()
		n := 0
		for _, m := range minted {
			if m {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d bootstrap keys minted, want 1", n)
		}
	})
}

func TestAnonymousAccess(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	a, err := db.GetAnonymousAccess(ctx)
	if err != nil || a.Access != AccessOff || a.Restriction != nil || a.UpdatedAt != nil {
		t.Fatalf("default = %+v, %v", a, err)
	}

	g := auth.Generation()
	r := &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 9}, {SystemID: 1, Tgid: 2}}}
	set, err := db.SetAnonymousAccess(ctx, AccessListen, r)
	if err != nil || set.UpdatedAt == nil {
		t.Fatalf("set = %+v, %v", set, err)
	}
	if auth.Generation() == g {
		t.Error("SetAnonymousAccess did not bump the auth generation")
	}
	a, err = db.GetAnonymousAccess(ctx)
	if err != nil || a.Access != AccessListen || !a.Restriction.Equal(r) || a.UpdatedAt == nil ||
		a.Restriction.ExcludeTalkgroups[0].Tgid != 2 {
		t.Errorf("get = %+v, %v", a, err)
	}

	// The restriction is kept while access is off; null clears it.
	if _, err := db.SetAnonymousAccess(ctx, AccessOff, r); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAnonymousAccess(ctx); a.Access != AccessOff || a.Restriction == nil {
		t.Errorf("off with restriction = %+v", a)
	}
	if _, err := db.SetAnonymousAccess(ctx, AccessListen, nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAnonymousAccess(ctx); a.Access != AccessListen || a.Restriction != nil {
		t.Errorf("unrestricted listen = %+v", a)
	}

	if _, err := db.SetAnonymousAccess(ctx, "edit", nil); !isFieldErr(err, "access") {
		t.Errorf("access edit: err = %v", err)
	}
	_, err = db.SetAnonymousAccess(ctx, AccessListen, &auth.Restriction{ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 1}}})
	if !isFieldErr(err, "restriction") || !errors.Is(err, auth.ErrAnonymousAllowsNothing) {
		t.Errorf("allow-nothing restriction: err = %v", err)
	}

	// A corrupted row is an error, never a policy.
	mustExec(t, db, `UPDATE auth_settings SET value = '{"access": "admin"}' WHERE name = 'anonymous_access'`)
	if _, err := db.GetAnonymousAccess(ctx); err == nil {
		t.Error("access admin was accepted")
	}
	mustExec(t, db, `UPDATE auth_settings SET value = '{"access": "listen", "restriction": {"sytems": [1]}}' WHERE name = 'anonymous_access'`)
	if _, err := db.GetAnonymousAccess(ctx); err == nil {
		t.Error("misspelled restriction field was accepted")
	}
}

func TestTicketSecretAndRetiredToken(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	s1, err := db.GetOrCreateTicketSecret(ctx)
	if err != nil || len(s1) != 32 {
		t.Fatalf("secret = %x, %v", s1, err)
	}
	s2, err := db.GetOrCreateTicketSecret(ctx)
	if err != nil || string(s1) != string(s2) {
		t.Errorf("second call returned a different secret")
	}
	mustExec(t, db, `DELETE FROM auth_settings WHERE name = 'ticket_secret'`)
	s3, err := db.GetOrCreateTicketSecret(ctx)
	if err != nil || len(s3) != 32 || string(s3) == string(s1) {
		t.Errorf("rotation: %x, %v", s3, err)
	}
	mustExec(t, db, `UPDATE auth_settings SET value = '"c2hvcnQ="' WHERE name = 'ticket_secret'`)
	if _, err := db.GetOrCreateTicketSecret(ctx); err == nil {
		t.Error("a 5-byte secret was accepted")
	}

	if h, err := db.GetRetiredPublicTokenHash(ctx); err != nil || h != "" {
		t.Errorf("none stored: %q, %v", h, err)
	}
	hash := HashAPIKey("the-old-public-token")
	if err := db.SetRetiredPublicTokenHash(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRetiredPublicTokenHash(ctx, "the-old-public-token"); err == nil {
		t.Error("stored a plaintext token as the retired hash")
	}
	if h, err := db.GetRetiredPublicTokenHash(ctx); err != nil || h != hash {
		t.Errorf("get = %q, %v", h, err)
	}
	if existed, err := db.ClearRetiredPublicToken(ctx); err != nil || !existed {
		t.Errorf("clear = %v, %v", existed, err)
	}
	if existed, err := db.ClearRetiredPublicToken(ctx); err != nil || existed {
		t.Errorf("clear again = %v, %v", existed, err)
	}
}

func TestAuditLog(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()

	actor := "alice"
	empty := ""
	for i, e := range []AuditEntry{
		{KeyID: 1, KeyName: "admin", Method: "PATCH", Path: "/api/v1/keys/3?x=secret", Status: 200, RequestID: "r1", Actor: &actor},
		{KeyID: 2, KeyName: "editor", Method: "PUT", Path: "/api/v1/calls/5/transcription", Status: 403, RequestID: "r2", Actor: &empty},
		{KeyID: 1, KeyName: "admin", Method: "DELETE", Path: "/api/v1/keys/4", Status: 204},
	} {
		if err := db.InsertAuditLog(ctx, e); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	mustExec(t, db, `UPDATE audit_log SET "time" = now() - interval '10 days' WHERE request_id = 'r1'`)

	all, total, err := db.ListAuditLog(ctx, AuditLogFilter{})
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("list all = %d/%d, %v", len(all), total, err)
	}
	if all[0].Method != "DELETE" || all[2].RequestID != "r1" {
		t.Errorf("not newest first: %+v", all)
	}
	if all[2].Path != "/api/v1/keys/3" || all[2].Actor == nil || *all[2].Actor != "alice" {
		t.Errorf("stored entry = %+v", all[2])
	}
	if all[1].Actor != nil || all[0].RequestID != "" {
		t.Errorf("empty actor/request id: %+v / %+v", all[1], all[0])
	}

	one := 1
	since := time.Now().Add(-24 * time.Hour)
	got, total, err := db.ListAuditLog(ctx, AuditLogFilter{KeyID: &one, Since: &since})
	if err != nil || total != 1 || len(got) != 1 || got[0].Method != "DELETE" {
		t.Errorf("key 1 since yesterday = %+v (%d), %v", got, total, err)
	}
	got, total, err = db.ListAuditLog(ctx, AuditLogFilter{Until: &since})
	if err != nil || total != 1 || got[0].RequestID != "r1" {
		t.Errorf("until yesterday = %+v (%d), %v", got, total, err)
	}
	got, total, err = db.ListAuditLog(ctx, AuditLogFilter{Limit: 1, Offset: 1})
	if err != nil || total != 3 || len(got) != 1 || got[0].RequestID != "r2" {
		t.Errorf("page 2 = %+v (%d), %v", got, total, err)
	}

	// Long paths are capped at a UTF-8 boundary (r1-08).
	if err := db.InsertAuditLog(ctx, AuditEntry{KeyID: 3, KeyName: "e", Method: "PATCH",
		Path: "/api/v1/units/" + strings.Repeat("é", 2000) + "?q=1", Status: 400}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.Pool.QueryRow(ctx, `SELECT path FROM audit_log WHERE key_id = 3`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored) || len(stored) > MaxAuditPathBytes+64 || !strings.HasSuffix(stored, "…(truncated, 4014 bytes)") {
		t.Errorf("stored long path = %d bytes, valid %v, ends %q", len(stored), utf8.ValidString(stored), stored[max(0, len(stored)-40):])
	}
	mustExec(t, db, `DELETE FROM audit_log WHERE key_id = 3`)

	if _, err := db.PurgeAuditLogOlderThan(ctx, 0); err == nil {
		t.Error("purge with zero retention was accepted")
	}
	n, err := db.PurgeAuditLogOlderThan(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Errorf("purge = %d, %v; want 1", n, err)
	}
}

func TestGetCallAccess(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_id, system_type, name) VALUES (7, 'p25', 'x')`)
	var id int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO calls (system_id, tgid, start_time) VALUES (7, 9178, now()) RETURNING call_id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	sys, tg, err := db.GetCallAccess(ctx, id)
	if err != nil || sys != 7 || tg != 9178 {
		t.Errorf("GetCallAccess = %d, %d, %v", sys, tg, err)
	}
	if _, _, err := db.GetCallAccess(ctx, id+1000); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing call: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestMergeSystems_RewritesRestrictions(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_id, system_type, name, sysid, wacn) VALUES
		(1, 'p25', 'target', '348', 'BEE00'), (2, 'p25', 'source', '348', 'BEE00'), (3, 'p25', 'other', '1', '1')`)

	tg := func(s, t int) auth.TG { return auth.TG{SystemID: s, Tgid: t} }
	restricted, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "restricted", Scopes: scopesOf("listen"), Restriction: &auth.Restriction{
		Systems:           []int{2, 3},
		Talkgroups:        []auth.TG{tg(1, 100), tg(2, 100), tg(2, 200)},
		ExcludeTalkgroups: []auth.TG{tg(2, 5), tg(1, 5), tg(3, 5)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	untouched, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "untouched", Scopes: scopesOf("listen"),
		Restriction: &auth.Restriction{Systems: []int{3}}})
	if err != nil {
		t.Fatal(err)
	}
	unrestricted := mustCreateKey(t, db, "unrestricted", "listen")
	if _, err := db.SetAnonymousAccess(ctx, AccessListen, &auth.Restriction{AllowAll: true,
		ExcludeTalkgroups: []auth.TG{tg(2, 7), tg(1, 7), tg(2, 8)}}); err != nil {
		t.Fatal(err)
	}
	if merged, err := db.AnyMergedAwaySystem(ctx, []int{1, 2, 3}); err != nil || merged {
		t.Fatalf("before merge: %v, %v", merged, err)
	}
	// Directory rows: the source's move to the target (r1-02); the target
	// keeps its own non-empty values and fills gaps from the source.
	mustExec(t, db, `INSERT INTO talkgroup_directory (system_id, tgid, alpha_tag, description, category) VALUES
		(2, 100, 'SWAT-TAC', 'Secret SWAT tactical', 'Secret'),
		(2, 200, 'FIRE-DISP', 'Fire dispatch', 'Fire'),
		(1, 100, 'TGT-100', '', NULL),
		(1, 300, 'EMS-1', 'EMS', 'EMS')`)
	excl2 := &auth.Principal{Kind: auth.KindKey, KeyID: 99, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(1, 100)}}}}

	g := auth.Generation()
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 2, 1, "test"); err != nil {
		t.Fatal(err)
	}
	if auth.Generation() == g {
		t.Error("MergeSystems did not bump the auth generation")
	}

	got, err := db.GetAPIKeyByID(ctx, restricted.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Exclusions keep their source entries beside the rewritten ones.
	want := &auth.Restriction{
		Systems:           []int{1, 3},
		Talkgroups:        []auth.TG{tg(1, 100), tg(1, 200)},
		ExcludeTalkgroups: []auth.TG{tg(1, 5), tg(2, 5), tg(3, 5)},
	}
	if !got.Restriction.Equal(want) {
		t.Errorf("rewritten restriction = %+v, want %+v", got.Restriction, want)
	}
	if k, _ := db.GetAPIKeyByID(ctx, untouched.ID); !k.Restriction.Equal(&auth.Restriction{Systems: []int{3}}) {
		t.Errorf("untouched restriction = %+v", k.Restriction)
	}
	if k, _ := db.GetAPIKeyByID(ctx, unrestricted.ID); k.Restriction != nil {
		t.Errorf("unrestricted key gained a restriction: %+v", k.Restriction)
	}
	a, err := db.GetAnonymousAccess(ctx)
	if err != nil || a.Access != AccessListen ||
		!a.Restriction.Equal(&auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(1, 7), tg(1, 8), tg(2, 7), tg(2, 8)}}) {
		t.Errorf("anonymous after merge = %+v, %v", a, err)
	}

	var srcRows int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM talkgroup_directory WHERE system_id = 2`).Scan(&srcRows); err != nil || srcRows != 0 {
		t.Errorf("%d directory rows left on the merged-away system (%v)", srcRows, err)
	}
	var alpha, desc, cat string
	if err := db.Pool.QueryRow(ctx, `SELECT alpha_tag, description, category FROM talkgroup_directory
		WHERE system_id = 1 AND tgid = 100`).Scan(&alpha, &desc, &cat); err != nil ||
		alpha != "TGT-100" || desc != "Secret SWAT tactical" || cat != "Secret" {
		t.Errorf("merged directory row 1:100 = %q %q %q (%v)", alpha, desc, cat, err)
	}
	dir, total, err := db.SearchTalkgroupDirectory(ctx, excl2, TalkgroupDirectoryFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var dirTGs []string
	for _, r := range dir {
		dirTGs = append(dirTGs, fmt.Sprintf("%d:%d", r.SystemID, r.Tgid))
	}
	if total != 2 || strings.Join(dirTGs, ",") != "1:200,1:300" {
		t.Errorf("directory for exclude 1:100 after merge = %v (total %d), want [1:200 1:300]", dirTGs, total)
	}

	// Restrictions stored after the merge that name the merged-away system
	// are rewritten as the merge would have rewritten them (r1-11).
	if a, err := db.SetAnonymousAccess(ctx, AccessListen, &auth.Restriction{AllowAll: true,
		ExcludeTalkgroups: []auth.TG{tg(2, 5001)}}); err != nil ||
		!a.Restriction.Equal(&auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(1, 5001), tg(2, 5001)}}) {
		t.Errorf("anonymous policy set after merge = %+v, %v", a.Restriction, err)
	}
	if a, _ := db.GetAnonymousAccess(ctx); a.Restriction.AllowsTG(1, 5001) {
		t.Error("stored anonymous policy allows 1:5001 although 2:5001 (merged into 1) was excluded")
	}
	late, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "late", Scopes: scopesOf("listen"),
		Restriction: &auth.Restriction{Systems: []int{2}, Talkgroups: []auth.TG{tg(2, 9)}}})
	if err != nil || !late.Restriction.Equal(&auth.Restriction{Systems: []int{1}, Talkgroups: []auth.TG{tg(1, 9)}}) {
		t.Errorf("key created after merge: %+v, %v", late, err)
	}
	patched, err := db.PatchAPIKey(ctx, late.ID, APIKeyPatch{SetRestriction: true,
		Restriction: &auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(2, 6)}}}, GuardLastAdmin)
	if err != nil || !patched.Restriction.Equal(&auth.Restriction{AllowAll: true, ExcludeTalkgroups: []auth.TG{tg(1, 6), tg(2, 6)}}) {
		t.Errorf("key patched after merge: %+v, %v", patched, err)
	}

	for _, c := range []struct {
		ids  []int
		want bool
	}{{[]int{2}, true}, {[]int{1, 3}, false}, {[]int{3, 2}, true}, {nil, false}, {[]int{0, -1}, false}} {
		if merged, err := db.AnyMergedAwaySystem(ctx, c.ids); err != nil || merged != c.want {
			t.Errorf("AnyMergedAwaySystem(%v) = %v, %v; want %v", c.ids, merged, err, c.want)
		}
	}

	// A restriction that can't be decoded fails the merge instead of being
	// left pointing at the merged-away system.
	mustExec(t, db, `UPDATE api_keys SET restriction = '{"systems": [3], "bogus": 1}' WHERE id = $1`, untouched.ID)
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 3, 1, "test"); err == nil {
		t.Error("merge with an undecodable restriction succeeded")
	}
	var deleted bool
	if err := db.Pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM systems WHERE system_id = 3`).Scan(&deleted); err != nil || deleted {
		t.Errorf("failed merge was not rolled back (deleted=%v, %v)", deleted, err)
	}
}

// An exclusion naming the surviving system of a merge also hides the
// merged-away ID, whether it was stored before the merge or after, and
// along a chain of merges: rows written under the old ID around a merge are
// never moved (r2-07).
func TestMergeSystems_ExclusionsCoverMergedAwayIDs(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_id, system_type, name) VALUES
		(1, 'p25', 'target'), (2, 'p25', 'source'), (5, 'p25', 'a'), (6, 'p25', 'b'), (7, 'p25', 'c')`)
	tg := func(s, t int) auth.TG { return auth.TG{SystemID: s, Tgid: t} }
	excluding := func(tgs ...auth.TG) *auth.Restriction {
		return &auth.Restriction{AllowAll: true, ExcludeTalkgroups: tgs}
	}

	before, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "before", Scopes: scopesOf("listen"), Restriction: excluding(tg(1, 600))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAnonymousAccess(ctx, AccessListen, excluding(tg(1, 600))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 2, 1, "test"); err != nil {
		t.Fatal(err)
	}
	// A straggler: a call written under the merged-away ID after the merge
	// moved the rows (an ingest that resolved identity before the rewrite).
	mustExec(t, db, `INSERT INTO calls (system_id, tgid, start_time) VALUES (2, 600, now())`)
	after, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "after", Scopes: scopesOf("listen"), Restriction: excluding(tg(1, 600))})
	if err != nil {
		t.Fatal(err)
	}

	anon, err := db.GetAnonymousAccess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]*auth.Restriction{"anonymous": anon.Restriction, "after": after.Restriction}
	if k, err := db.GetAPIKeyByID(ctx, before.ID); err != nil {
		t.Fatal(err)
	} else {
		stored["before"] = k.Restriction
	}
	for name, r := range stored {
		if !r.Equal(excluding(tg(1, 600), tg(2, 600))) {
			t.Errorf("%s: stored %+v, want exclusions 1:600 and 2:600", name, r)
		}
		p := &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen}, Restrictions: []auth.Restriction{*r}}
		if p.AllowsTG(2, 600) {
			t.Errorf("%s: AllowsTG(2, 600) = true", name)
		}
		clause, args := p.SQL("c.system_id", "c.tgid", 1)
		var n int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM calls c WHERE c.tgid = 600`+clause, args...).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: sees %d calls on talkgroup 600 (%v)", name, n, err)
		}
	}

	// A chain: 5 merged into 6, then 6 into 7. An exclusion of 7:9, stored
	// before or after, names all three.
	chainBefore, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "chain before", Scopes: scopesOf("listen"), Restriction: excluding(tg(7, 9))})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 5, 6, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 6, 7, "test"); err != nil {
		t.Fatal(err)
	}
	chainAfter, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "chain after", Scopes: scopesOf("listen"), Restriction: excluding(tg(7, 9))})
	if err != nil {
		t.Fatal(err)
	}
	k, err := db.GetAPIKeyByID(ctx, chainBefore.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*auth.Restriction{"chain before": k.Restriction, "chain after": chainAfter.Restriction} {
		if !r.Equal(excluding(tg(5, 9), tg(6, 9), tg(7, 9))) {
			t.Errorf("%s: %+v, want exclusions 5:9, 6:9 and 7:9", name, r)
		}
	}
}

// A merge can grow a stored exclusion list past the 1000-entry input limit;
// that limit then applies to new input only, so the key and the policy can
// still be edited, and the policy switched off (r2-11).
func TestRestrictionsOutgrowingTheInputLimit(t *testing.T) {
	db := authDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_id, system_type, name) VALUES (1, 'p25', 'target'), (2, 'p25', 'source')`)
	big := &auth.Restriction{AllowAll: true}
	for i := 1; i <= 600; i++ {
		big.ExcludeTalkgroups = append(big.ExcludeTalkgroups, auth.TG{SystemID: 2, Tgid: i})
	}
	key, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "big", Scopes: scopesOf("listen"), Restriction: big})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAnonymousAccess(ctx, AccessListen, big); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := db.MergeSystems(ctx, 2, 1, "test"); err != nil {
		t.Fatal(err)
	}
	k, err := db.GetAPIKeyByID(ctx, key.ID)
	if err != nil || len(k.Restriction.ExcludeTalkgroups) != 1200 {
		t.Fatalf("stored exclusions after the merge: %v (%v), want 1200", k, err)
	}
	stored := k.Restriction

	rps := float32(5)
	later := time.Now().Add(24 * time.Hour)
	for name, p := range map[string]APIKeyPatch{
		"rename":                           {SetName: true, Name: "renamed"},
		"expiry":                           {SetExpiresAt: true, ExpiresAt: &later},
		"rate limit":                       {SetRateLimitRPS: true, RateLimitRPS: &rps},
		"rename with the same restriction": {SetName: true, Name: "again", SetRestriction: true, Restriction: stored},
	} {
		if _, err := db.PatchAPIKey(ctx, key.ID, p, GuardLastAdmin); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A new restriction over the limit is still refused.
	grown := *stored
	grown.ExcludeTalkgroups = append(append([]auth.TG{}, stored.ExcludeTalkgroups...), auth.TG{SystemID: 1, Tgid: 9999})
	if _, err := db.PatchAPIKey(ctx, key.ID, APIKeyPatch{SetRestriction: true, Restriction: &grown}, GuardLastAdmin); !isFieldErr(err, "restriction") {
		t.Errorf("new oversized restriction: err = %v, want a restriction field error", err)
	}
	if _, err := db.CreateAPIKey(ctx, NewAPIKey{Name: "copy", Scopes: scopesOf("listen"), Restriction: stored}); !isFieldErr(err, "restriction") {
		t.Errorf("new key with an oversized restriction: err = %v, want a restriction field error", err)
	}

	// The anonymous policy keeps its stored restriction while switching off
	// and on again (what `access set --anonymous off` and the admin pages do).
	anon, err := db.GetAnonymousAccess(ctx)
	if err != nil || len(anon.Restriction.ExcludeTalkgroups) != 1200 {
		t.Fatalf("anonymous after the merge: %+v (%v)", anon, err)
	}
	for _, access := range []string{AccessOff, AccessListen} {
		if a, err := db.SetAnonymousAccess(ctx, access, anon.Restriction); err != nil || a.Access != access {
			t.Errorf("set %s with the stored restriction: %+v, %v", access, a, err)
		}
	}
	if _, err := db.SetAnonymousAccess(ctx, AccessListen, &grown); !isFieldErr(err, "restriction") {
		t.Errorf("new oversized anonymous restriction: err = %v, want a restriction field error", err)
	}
}

// A database whose schema this version created is fresh whichever process
// created it (a CLI command, psql, a server that stopped before its import
// committed): the old variables are not imported and anonymous access stays
// off, even with data in it (r2-05). Only a database without the marker,
// upgraded from an older version, carries them over.
func TestImportLegacyAuth_SchemaMarkerMeansFresh(t *testing.T) {
	ctx := context.Background()
	in := LegacyAuthInput{AdminPassword: "p", AuthToken: strongAuth, WriteToken: strongWrite}

	db := authDB(t) // FreshDatabase false: another process applied the schema
	var marked bool
	if err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM data_fixups WHERE name = $1)`, SchemaCreatedFixup).Scan(&marked); err != nil || !marked {
		t.Fatalf("schema.sql did not write its marker (%v)", err)
	}
	mustExec(t, db, `INSERT INTO systems (system_type, name) VALUES ('p25', 'imported')`)
	res, err := db.ImportLegacyAuth(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	assertKeyCount(t, db, 0)
	assertAnonymous(t, db, res, AccessOff)
	assertRetired(t, db, "")
	if r, _ := res.Detail.SkipReason("WRITE_TOKEN"); r != SkipFreshDatabase || !res.Detail.FreshDatabase {
		t.Errorf("detail = %+v", res.Detail)
	}

	old := upgradedDB(t)
	mustExec(t, old, `INSERT INTO systems (system_type, name) VALUES ('p25', 'old')`)
	res, err = old.ImportLegacyAuth(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Detail.FreshDatabase || importedScopes(t, res, "WRITE_TOKEN") != "admin,upload" {
		t.Errorf("upgraded database: detail = %+v", res.Detail)
	}
	assertAnonymous(t, old, res, AccessListen)
}

// legacyCase is one ImportLegacyAuth scenario on its own database.
type legacyCase struct {
	name    string
	in      LegacyAuthInput
	systems bool // the database has data
	setup   func(t *testing.T, db *DB)
	check   func(t *testing.T, db *DB, res LegacyAuthResult)
}

const (
	strongWrite = "write-token-0123456789abcdef"
	strongAuth  = "auth-token-0123456789abcdef"
)

func importedScopes(t *testing.T, res LegacyAuthResult, variable string) string {
	t.Helper()
	k, ok := res.Detail.ImportedKey(variable)
	if !ok {
		return ""
	}
	return strings.Join(k.Scopes.Strings(), ",")
}

func assertKeyCount(t *testing.T, db *DB, want int) {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("%d api keys, want %d", n, want)
	}
}

func assertAnonymous(t *testing.T, db *DB, res LegacyAuthResult, want string) {
	t.Helper()
	a, err := db.GetAnonymousAccess(context.Background())
	if err != nil || a.Access != want || a.Restriction != nil || res.Detail.AnonymousAccess != want {
		t.Errorf("anonymous access = %+v (%v), detail says %q; want %s", a, err, res.Detail.AnonymousAccess, want)
	}
}

func assertRetired(t *testing.T, db *DB, secret string) {
	t.Helper()
	h, err := db.GetRetiredPublicTokenHash(context.Background())
	want := ""
	if secret != "" {
		want = HashAPIKey(secret)
	}
	if err != nil || h != want {
		t.Errorf("retired public token hash = %q, %v; want %q", h, err, want)
	}
}

func assertResolves(t *testing.T, db *DB, secret, scopes string) {
	t.Helper()
	k, err := db.ResolveAPIKeyByHash(context.Background(), HashAPIKey(secret))
	if err != nil {
		t.Errorf("%q does not resolve: %v", secret, err)
		return
	}
	if got := strings.Join(k.Scopes.Strings(), ","); got != scopes || !k.Legacy || k.Status != KeyActive {
		t.Errorf("%q resolves to %+v, want legacy active %s", secret, k, scopes)
	}
}

func TestImportLegacyAuth(t *testing.T) {
	ctx := context.Background()
	cases := []legacyCase{
		{
			name: "fresh database imports nothing",
			in:   LegacyAuthInput{AdminPassword: "p", AuthToken: strongAuth, WriteToken: strongWrite, FreshDatabase: true},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertAnonymous(t, db, res, AccessOff)
				assertRetired(t, db, "")
				if r, _ := res.Detail.SkipReason("WRITE_TOKEN"); r != SkipFreshDatabase || !res.Detail.FreshDatabase {
					t.Errorf("detail = %+v", res.Detail)
				}
			},
		},
		{
			name:    "open with data: anonymous listen",
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertAnonymous(t, db, res, AccessListen)
				if res.Detail.OldMode != LegacyModeOpen {
					t.Errorf("old mode = %s", res.Detail.OldMode)
				}
			},
		},
		{
			name: "open without data: anonymous off",
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertAnonymous(t, db, res, AccessOff)
			},
		},
		{
			name:    "AUTH_ENABLED=false: open, nothing imported",
			in:      LegacyAuthInput{AuthEnabled: "false", AdminPassword: "p", AuthToken: strongAuth, WriteToken: strongWrite},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertAnonymous(t, db, res, AccessListen)
				assertRetired(t, db, "")
				if res.Detail.OldMode != LegacyModeOpen {
					t.Errorf("old mode = %s", res.Detail.OldMode)
				}
				for _, v := range []string{"AUTH_TOKEN", "WRITE_TOKEN"} {
					if r, _ := res.Detail.SkipReason(v); r != SkipAuthDisabled {
						t.Errorf("%s skip reason = %q", v, r)
					}
				}
			},
		},
		{
			name:    "write-token-only",
			in:      LegacyAuthInput{WriteToken: strongWrite},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 1)
				assertResolves(t, db, strongWrite, "admin,upload")
				assertAnonymous(t, db, res, AccessOff)
				k, _ := res.Detail.ImportedKey("WRITE_TOKEN")
				if res.Detail.OldMode != LegacyModeWriteTokenOnly || !res.Detail.SuggestAnonymousListen || k.Name != LegacyWriteTokenKeyName {
					t.Errorf("detail = %+v", res.Detail)
				}
			},
		},
		{
			name:    "token mode without WRITE_TOKEN",
			in:      LegacyAuthInput{AuthToken: strongAuth},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 1)
				assertResolves(t, db, strongAuth, "listen,upload")
				assertAnonymous(t, db, res, AccessOff)
				if k, _ := res.Detail.ImportedKey("AUTH_TOKEN"); k.Name != LegacyAuthTokenKeyName {
					t.Errorf("detail = %+v", res.Detail)
				}
			},
		},
		{
			name:    "token mode with WRITE_TOKEN",
			in:      LegacyAuthInput{AuthToken: strongAuth, WriteToken: strongWrite},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 2)
				assertResolves(t, db, strongAuth, "listen")
				assertResolves(t, db, strongWrite, "admin,upload")
				assertAnonymous(t, db, res, AccessOff)
				assertRetired(t, db, "")
			},
		},
		{
			name:    "token mode with AUTH_TOKEN equal to WRITE_TOKEN",
			in:      LegacyAuthInput{AuthToken: strongWrite, WriteToken: strongWrite},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 1)
				assertResolves(t, db, strongWrite, "admin,upload")
				if r, _ := res.Detail.SkipReason("AUTH_TOKEN"); r != SkipSameAsWriteToken {
					t.Errorf("AUTH_TOKEN skip reason = %q", r)
				}
			},
		},
		{
			name:    "full mode: public AUTH_TOKEN retired, WRITE_TOKEN imported",
			in:      LegacyAuthInput{AdminPassword: "p", AuthToken: strongAuth, WriteToken: strongWrite, JWTSecret: "s"},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 1)
				assertResolves(t, db, strongWrite, "admin,upload")
				assertRetired(t, db, strongAuth)
				assertAnonymous(t, db, res, AccessListen)
				if r, _ := res.Detail.SkipReason("AUTH_TOKEN"); r != SkipPublicToken || !res.Detail.RetiredPublicToken {
					t.Errorf("detail = %+v", res.Detail)
				}
				if _, err := db.ResolveAPIKeyByHash(ctx, HashAPIKey(strongAuth)); !errors.Is(err, ErrAPIKeyNotFound) {
					t.Errorf("the public AUTH_TOKEN became a key (%v)", err)
				}
			},
		},
		{
			name:    "full mode with WRITE_TOKEN equal to the public AUTH_TOKEN",
			in:      LegacyAuthInput{AdminPassword: "p", AuthToken: strongAuth, WriteToken: strongAuth},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertRetired(t, db, strongAuth)
				assertAnonymous(t, db, res, AccessListen)
				if r, _ := res.Detail.SkipReason("WRITE_TOKEN"); r != SkipPublishedAsPublic {
					t.Errorf("WRITE_TOKEN skip reason = %q", r)
				}
				// The bootstrap key is then minted.
				if _, key, _, err := db.ClaimBootstrapAdminKey(ctx); err != nil || key == nil {
					t.Errorf("bootstrap after equal tokens: %+v, %v", key, err)
				}
			},
		},
		{
			name:    "full mode without AUTH_TOKEN: anonymous off",
			in:      LegacyAuthInput{AdminPassword: "p"},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				assertAnonymous(t, db, res, AccessOff)
				assertRetired(t, db, "")
			},
		},
		{
			name:    `"$(" values are refused`,
			in:      LegacyAuthInput{AuthToken: "$(openssl rand -hex 32)", WriteToken: "$(openssl rand -hex 16)"},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 0)
				for _, v := range []string{"AUTH_TOKEN", "WRITE_TOKEN"} {
					if r, _ := res.Detail.SkipReason(v); r != SkipShellSubstitution {
						t.Errorf("%s skip reason = %q", v, r)
					}
				}
			},
		},
		{
			name:    "weak token is imported and flagged",
			in:      LegacyAuthInput{AuthToken: "hunter2"},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertResolves(t, db, "hunter2", "listen,upload")
				k, _ := res.Detail.ImportedKey("AUTH_TOKEN")
				if !k.Weak || k.Length != 7 {
					t.Errorf("imported = %+v", k)
				}
				if len(res.WeakKeys) != 1 || res.WeakKeys[0].ID != k.KeyID || res.WeakKeys[0].Length != 7 {
					t.Errorf("WeakKeys = %+v", res.WeakKeys)
				}
				// Reported again on every start, until revoked.
				again, err := db.ImportLegacyAuth(ctx, LegacyAuthInput{AuthToken: "hunter2"})
				if err != nil || again.Ran || len(again.WeakKeys) != 1 {
					t.Errorf("second start = %+v, %v", again, err)
				}
				if _, err := db.RevokeAPIKey(ctx, k.KeyID, GuardLastAdmin); err != nil {
					t.Fatal(err)
				}
				if again, _ := db.ImportLegacyAuth(ctx, LegacyAuthInput{}); len(again.WeakKeys) != 0 {
					t.Errorf("WeakKeys after revoke = %+v", again.WeakKeys)
				}
			},
		},
		{
			name:    "invalid AUTH_ENABLED is taken as enabled",
			in:      LegacyAuthInput{AuthEnabled: "maybe", AuthToken: strongAuth},
			systems: true,
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertResolves(t, db, strongAuth, "listen,upload")
				assertAnonymous(t, db, res, AccessOff)
				if !res.Detail.AuthEnabledInvalid {
					t.Errorf("detail = %+v", res.Detail)
				}
			},
		},
		{
			name:    "an already stored secret is not duplicated",
			in:      LegacyAuthInput{WriteToken: strongWrite},
			systems: true,
			setup: func(t *testing.T, db *DB) {
				if _, _, err := db.CreateLegacyAPIKey(ctx, strongWrite, NewAPIKey{Name: "registered", Scopes: scopesOf("edit")}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertKeyCount(t, db, 1)
				assertResolves(t, db, strongWrite, "edit")
				if k, _ := res.Detail.ImportedKey("WRITE_TOKEN"); !k.Existing || k.Name != "registered" {
					t.Errorf("imported = %+v", k)
				}
			},
		},
		{
			name:    "a stored anonymous policy is kept",
			systems: true,
			setup: func(t *testing.T, db *DB) {
				if _, err := db.SetAnonymousAccess(ctx, AccessOff, nil); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				assertAnonymous(t, db, res, AccessOff)
				if !res.Detail.AnonymousKept {
					t.Errorf("detail = %+v", res.Detail)
				}
			},
		},
		{
			name:    "uploads in use without an upload key",
			in:      LegacyAuthInput{AdminPassword: "p", AuthToken: strongAuth, UploadInstanceID: "http-upload"},
			systems: true,
			setup: func(t *testing.T, db *DB) {
				mustExec(t, db, `INSERT INTO calls (system_id, tgid, start_time, instance_id)
					VALUES ((SELECT min(system_id) FROM systems), 1, now() - interval '1 day', 'http-upload')`)
				k := mustCreateKey(t, db, "old user key", "edit")
				mustExec(t, db, `UPDATE api_keys SET last_used_at = now() - interval '2 days' WHERE id = $1`, k.ID)
				idle := mustCreateKey(t, db, "idle key", "listen")
				mustExec(t, db, `UPDATE api_keys SET last_used_at = now() - interval '30 days' WHERE id = $1`, idle.ID)
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				d := res.Detail
				if !d.UploadsInUse || !d.NoUploadKey || len(d.RecentKeysWithoutUpload) != 1 ||
					d.RecentKeysWithoutUpload[0].Name != "old user key" {
					t.Errorf("upload report = %+v / %v / %+v", d.UploadsInUse, d.NoUploadKey, d.RecentKeysWithoutUpload)
				}
			},
		},
		{
			// What the upload pipeline actually writes: calls without an
			// instance_id, on a site created for UPLOAD_INSTANCE_ID.
			name:    "uploads in use: calls on an upload site",
			in:      LegacyAuthInput{UploadInstanceID: "http-upload"},
			systems: true,
			setup: func(t *testing.T, db *DB) {
				mustExec(t, db, `SELECT create_monthly_partition('calls', (date_trunc('month', now()) - interval '1 month')::date)`)
				mustExec(t, db, `INSERT INTO instances (instance_id) VALUES ('http-upload')`)
				mustExec(t, db, `INSERT INTO sites (system_id, instance_id, short_name)
					VALUES ((SELECT min(system_id) FROM systems), 'http-upload', 'butco')`)
				mustExec(t, db, `INSERT INTO calls (system_id, site_id, tgid, start_time)
					VALUES ((SELECT min(system_id) FROM systems), (SELECT min(site_id) FROM sites), 1, now() - interval '2 days')`)
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				if !res.Detail.UploadsInUse || !res.Detail.NoUploadKey {
					t.Errorf("upload report = %+v", res.Detail)
				}
			},
		},
		{
			name:    "uploads not in use: old calls on an upload site, recent calls elsewhere",
			in:      LegacyAuthInput{UploadInstanceID: "http-upload"},
			systems: true,
			setup: func(t *testing.T, db *DB) {
				mustExec(t, db, `SELECT create_monthly_partition('calls', (date_trunc('month', now()) - interval '1 month')::date)`)
				mustExec(t, db, `INSERT INTO instances (instance_id) VALUES ('http-upload'), ('tr-1')`)
				mustExec(t, db, `INSERT INTO sites (system_id, instance_id, short_name) VALUES
					((SELECT min(system_id) FROM systems), 'http-upload', 'butco'),
					((SELECT min(system_id) FROM systems), 'tr-1', 'butco')`)
				mustExec(t, db, `INSERT INTO calls (system_id, site_id, tgid, start_time) VALUES
					((SELECT min(system_id) FROM systems), (SELECT site_id FROM sites WHERE instance_id = 'http-upload'), 1, now() - interval '8 days'),
					((SELECT min(system_id) FROM systems), (SELECT site_id FROM sites WHERE instance_id = 'tr-1'), 1, now() - interval '1 hour')`)
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				if res.Detail.UploadsInUse || res.Detail.NoUploadKey {
					t.Errorf("upload report = %+v", res.Detail)
				}
			},
		},
		{
			name:    "uploads in use with an imported upload key",
			in:      LegacyAuthInput{WriteToken: strongWrite, UploadInstanceID: "http-upload"},
			systems: true,
			setup: func(t *testing.T, db *DB) {
				mustExec(t, db, `INSERT INTO calls (system_id, tgid, start_time, instance_id)
					VALUES ((SELECT min(system_id) FROM systems), 1, now() - interval '1 hour', 'http-upload')`)
			},
			check: func(t *testing.T, db *DB, res LegacyAuthResult) {
				if !res.Detail.UploadsInUse || res.Detail.NoUploadKey {
					t.Errorf("upload report = %+v", res.Detail)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := upgradedDB(t)
			if c.systems {
				mustExec(t, db, `INSERT INTO systems (system_type, name) VALUES ('p25', 'butco')`)
			}
			if c.setup != nil {
				c.setup(t, db)
			}
			res, err := db.ImportLegacyAuth(ctx, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Ran || res.ImportedAt.IsZero() || len(res.Detail.Decisions) == 0 {
				t.Errorf("result = %+v", res)
			}
			c.check(t, db, res)

			// The import runs once per database; later starts get the record.
			var keys int
			db.Pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&keys)
			later, err := db.ImportLegacyAuth(ctx, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if later.Ran || !later.ImportedAt.Equal(res.ImportedAt) || later.Detail.OldMode != res.Detail.OldMode ||
				len(later.Detail.Imported) != len(res.Detail.Imported) {
				t.Errorf("second call = %+v, want the recorded import", later)
			}
			assertKeyCount(t, db, keys)
			if strings.Contains(res.Detail.Summary(), strongAuth) || strings.Contains(res.Detail.Summary(), strongWrite) {
				t.Errorf("summary contains a secret: %s", res.Detail.Summary())
			}
		})
	}
}

// No secret, or any part of one, ends up in the recorded detail.
func TestImportLegacyAuth_DetailHasNoSecrets(t *testing.T) {
	db := upgradedDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO systems (system_type, name) VALUES ('p25', 'x')`)
	in := LegacyAuthInput{AuthToken: "AAAAauthsecretZZZZ", WriteToken: "BBBBwritesecretYYYY", AdminPassword: "CCCCpasswordXXXX"}
	if _, err := db.ImportLegacyAuth(ctx, in); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := db.Pool.QueryRow(ctx, `SELECT detail::text FROM data_fixups WHERE name = $1`, LegacyAuthImportFixup).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"AAAA", "authsecret", "BBBB", "writesecret", "CCCC", "password"} {
		if strings.Contains(detail, s) {
			t.Errorf("detail contains %q: %s", s, detail)
		}
	}
	var prefixes string
	db.Pool.QueryRow(ctx, `SELECT string_agg(key_prefix || name, ' ') FROM api_keys`).Scan(&prefixes)
	for _, s := range []string{"AAAA", "BBBB"} {
		if strings.Contains(prefixes, s) {
			t.Errorf("key prefix or name contains %q: %s", s, prefixes)
		}
	}
}
