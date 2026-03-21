package ingest

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
)

// ResolvedIdentity contains the resolved system/site IDs for an MQTT message.
type ResolvedIdentity struct {
	InstanceDBID int
	SystemID     int
	SiteID       int
	SystemName   string
	Sysid        string
}

// IdentityResolver caches instance/system/site mappings in memory.
// It auto-creates entries on first encounter.
type IdentityResolver struct {
	db  *database.DB
	log zerolog.Logger
	mu  sync.RWMutex

	// cache keyed by "instanceID:sysName"
	cache map[string]*ResolvedIdentity
	// instance cache keyed by instanceID
	instances map[string]int
}

func NewIdentityResolver(db *database.DB, log zerolog.Logger) *IdentityResolver {
	return &IdentityResolver{
		db:        db,
		log:       log,
		cache:     make(map[string]*ResolvedIdentity),
		instances: make(map[string]int),
	}
}

// LoadCache pre-populates the cache from existing DB records.
func (r *IdentityResolver) LoadCache(ctx context.Context) error {
	sites, err := r.db.LoadAllSites(ctx)
	if err != nil {
		return fmt.Errorf("load sites: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, s := range sites {
		key := s.InstanceID + ":" + s.ShortName
		r.cache[key] = &ResolvedIdentity{
			SystemID:   s.SystemID,
			SiteID:     s.SiteID,
			SystemName: s.ShortName,
			Sysid:      s.Sysid,
		}
	}

	r.log.Info().Int("cached_sites", len(sites)).Msg("identity cache loaded")
	return nil
}

// CacheLen returns the number of entries in the identity cache.
func (r *IdentityResolver) CacheLen() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// Resolve returns the identity for the given instance and sys_name,
// creating DB records if needed.
func (r *IdentityResolver) Resolve(ctx context.Context, instanceID, sysName string) (*ResolvedIdentity, error) {
	key := instanceID + ":" + sysName

	// Fast path: read lock
	r.mu.RLock()
	if id, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return id, nil
	}
	r.mu.RUnlock()

	// Slow path: write lock, create if needed
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check
	if id, ok := r.cache[key]; ok {
		return id, nil
	}

	// Ensure instance exists
	if _, ok := r.instances[instanceID]; !ok {
		dbID, err := r.db.UpsertInstance(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("upsert instance %q: %w", instanceID, err)
		}
		r.instances[instanceID] = dbID
	}

	// Find or create system
	systemID, sysid, err := r.db.FindOrCreateSystem(ctx, instanceID, sysName, "")
	if err != nil {
		return nil, fmt.Errorf("find/create system %q/%q: %w", instanceID, sysName, err)
	}

	// Find or create site
	siteID, err := r.db.FindOrCreateSite(ctx, systemID, instanceID, sysName)
	if err != nil {
		return nil, fmt.Errorf("find/create site %q/%q: %w", instanceID, sysName, err)
	}

	id := &ResolvedIdentity{
		InstanceDBID: r.instances[instanceID],
		SystemID:     systemID,
		SiteID:       siteID,
		SystemName:   sysName,
		Sysid:        sysid,
	}
	r.cache[key] = id

	r.log.Info().
		Str("instance_id", instanceID).
		Str("sys_name", sysName).
		Int("system_id", systemID).
		Int("site_id", siteID).
		Msg("new identity resolved and cached")

	return id, nil
}

// GetSystemIDForSysName returns the system_id for a given sys_name from any instance.
// Returns 0 if not found.
func (r *IdentityResolver) GetSystemIDForSysName(sysName string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, id := range r.cache {
		if id.SystemName == sysName {
			return id.SystemID
		}
	}
	return 0
}

// LookupByShortName finds a system/site by TR short_name. When instanceID is
// non-empty, performs a direct cache key lookup (deterministic, O(1)). When
// empty, falls back to scanning all entries and returning the first match
// (non-deterministic when multiple instances share a short_name).
func (ir *IdentityResolver) LookupByShortName(instanceID, shortName string) (systemID, siteID int, ok bool) {
	ir.mu.RLock()
	defer ir.mu.RUnlock()

	if instanceID != "" {
		key := instanceID + ":" + shortName
		if ri, found := ir.cache[key]; found {
			return ri.SystemID, ri.SiteID, true
		}
		return 0, 0, false
	}

	// Fallback: scan all entries (legacy, non-deterministic for duplicates)
	for _, ri := range ir.cache {
		if ri.SystemName == shortName {
			return ri.SystemID, ri.SiteID, true
		}
	}
	return 0, 0, false
}

// LookupByShortNameAny scans all identity cache entries for a matching short name,
// skipping entries whose instanceID is in the exclude set. Returns the first
// non-excluded match along with its instanceID. Used for auto-learning source IP
// to instance mappings in multi-instance simplestream setups.
func (ir *IdentityResolver) LookupByShortNameAny(shortName string, exclude map[string]bool) (systemID, siteID int, instanceID string, ok bool) {
	ir.mu.RLock()
	defer ir.mu.RUnlock()

	for key, ri := range ir.cache {
		if ri.SystemName != shortName {
			continue
		}
		// Extract instanceID from cache key ("instanceID:sysName")
		instID := key[:len(key)-len(shortName)-1]
		if exclude[instID] {
			continue
		}
		return ri.SystemID, ri.SiteID, instID, true
	}
	return 0, 0, "", false
}

// RewriteSystemID updates all cache entries pointing at oldSystemID to use newSystemID.
// Called after a system merge so subsequent lookups resolve to the merged target.
func (r *IdentityResolver) RewriteSystemID(oldSystemID, newSystemID int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for key, id := range r.cache {
		if id.SystemID == oldSystemID {
			r.cache[key] = &ResolvedIdentity{
				InstanceDBID: id.InstanceDBID,
				SystemID:     newSystemID,
				SiteID:       id.SiteID,
				SystemName:   id.SystemName,
				Sysid:        id.Sysid,
			}
			r.log.Info().
				Str("key", key).
				Int("old_system_id", oldSystemID).
				Int("new_system_id", newSystemID).
				Msg("cache entry rewritten after merge")
		}
	}
}
