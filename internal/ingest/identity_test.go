package ingest

import (
	"testing"

	"github.com/rs/zerolog"
)

// newTestResolver creates an IdentityResolver with a pre-populated cache (no DB needed).
func newTestResolver(entries map[string]*ResolvedIdentity) *IdentityResolver {
	r := &IdentityResolver{
		log:       zerolog.Nop(),
		cache:     make(map[string]*ResolvedIdentity),
		instances: make(map[string]int),
	}
	for k, v := range entries {
		r.cache[k] = v
	}
	return r
}

func TestGetSystemIDForSysName(t *testing.T) {
	r := newTestResolver(map[string]*ResolvedIdentity{
		"tr-1:butco": {SystemID: 1, SystemName: "butco", Sysid: "348"},
		"tr-2:warco": {SystemID: 2, SystemName: "warco", Sysid: "34D"},
		"tr-3:butco": {SystemID: 1, SystemName: "butco", Sysid: "348"}, // same system, different instance
	})

	t.Run("found", func(t *testing.T) {
		id := r.GetSystemIDForSysName("butco")
		if id != 1 {
			t.Errorf("got %d, want 1", id)
		}
	})

	t.Run("found_other", func(t *testing.T) {
		id := r.GetSystemIDForSysName("warco")
		if id != 2 {
			t.Errorf("got %d, want 2", id)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		id := r.GetSystemIDForSysName("unknown")
		if id != 0 {
			t.Errorf("got %d, want 0", id)
		}
	})

	t.Run("empty_cache", func(t *testing.T) {
		empty := newTestResolver(nil)
		id := empty.GetSystemIDForSysName("butco")
		if id != 0 {
			t.Errorf("got %d, want 0", id)
		}
	})
}

func TestRewriteSystemID(t *testing.T) {
	t.Run("rewrites_matching_entries", func(t *testing.T) {
		r := newTestResolver(map[string]*ResolvedIdentity{
			"tr-1:butco": {SystemID: 10, SiteID: 1, SystemName: "butco", Sysid: "348"},
			"tr-2:butco": {SystemID: 10, SiteID: 2, SystemName: "butco", Sysid: "348"},
			"tr-3:warco": {SystemID: 20, SiteID: 3, SystemName: "warco", Sysid: "34D"},
		})

		r.RewriteSystemID(10, 99)

		// Rewritten entries
		if r.cache["tr-1:butco"].SystemID != 99 {
			t.Errorf("tr-1:butco SystemID = %d, want 99", r.cache["tr-1:butco"].SystemID)
		}
		if r.cache["tr-2:butco"].SystemID != 99 {
			t.Errorf("tr-2:butco SystemID = %d, want 99", r.cache["tr-2:butco"].SystemID)
		}

		// Unaffected entry
		if r.cache["tr-3:warco"].SystemID != 20 {
			t.Errorf("tr-3:warco SystemID = %d, want 20 (unchanged)", r.cache["tr-3:warco"].SystemID)
		}
	})

	t.Run("preserves_other_fields", func(t *testing.T) {
		r := newTestResolver(map[string]*ResolvedIdentity{
			"tr-1:butco": {InstanceDBID: 5, SystemID: 10, SiteID: 1, SystemName: "butco", Sysid: "348"},
		})

		r.RewriteSystemID(10, 99)

		entry := r.cache["tr-1:butco"]
		if entry.InstanceDBID != 5 {
			t.Errorf("InstanceDBID = %d, want 5", entry.InstanceDBID)
		}
		if entry.SiteID != 1 {
			t.Errorf("SiteID = %d, want 1", entry.SiteID)
		}
		if entry.SystemName != "butco" {
			t.Errorf("SystemName = %q, want butco", entry.SystemName)
		}
		if entry.Sysid != "348" {
			t.Errorf("Sysid = %q, want 348", entry.Sysid)
		}
	})

	t.Run("no_match_is_noop", func(t *testing.T) {
		r := newTestResolver(map[string]*ResolvedIdentity{
			"tr-1:butco": {SystemID: 10, SystemName: "butco"},
		})

		r.RewriteSystemID(999, 1) // no entries with SystemID=999

		if r.cache["tr-1:butco"].SystemID != 10 {
			t.Errorf("SystemID = %d, want 10 (unchanged)", r.cache["tr-1:butco"].SystemID)
		}
	})
}

func TestInstancesByShortName(t *testing.T) {
	r := newTestResolver(map[string]*ResolvedIdentity{
		"tr-b:county": {SystemID: 4, SystemName: "county"},
		"tr-a:county": {SystemID: 3, SystemName: "county"},
		"tr-a:city":   {SystemID: 5, SystemName: "city"},
	})
	if got := r.InstancesByShortName("county"); len(got) != 2 || got[0] != "tr-a" || got[1] != "tr-b" {
		t.Errorf("InstancesByShortName(county) = %v, want [tr-a tr-b]", got)
	}
	if got := r.InstancesByShortName("city"); len(got) != 1 || got[0] != "tr-a" {
		t.Errorf("InstancesByShortName(city) = %v, want [tr-a]", got)
	}
	if got := r.InstancesByShortName("none"); len(got) != 0 {
		t.Errorf("InstancesByShortName(none) = %v, want none", got)
	}
}
