package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func (p *Pipeline) handleSystems(payload []byte) error {
	var msg SystemsMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	for _, sys := range msg.Systems {
		if err := p.processSystemInfo(msg.InstanceID, &sys); err != nil {
			p.log.Error().Err(err).Str("sys_name", sys.SysName).Msg("failed to process system info")
		}
	}
	return nil
}

func (p *Pipeline) handleSystem(payload []byte) error {
	var msg SystemMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	return p.processSystemInfo(msg.InstanceID, &msg.System)
}

// processSystemInfo handles a single system info entry from either systems or system topics.
// It updates identity fields and triggers auto-merge when two systems share the same (sysid, wacn).
func (p *Pipeline) processSystemInfo(instanceID string, sys *SystemInfoData) error {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	// Resolve identity (creates system/site if needed)
	identity, err := p.identity.Resolve(ctx, instanceID, sys.SysName)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	// Check for merge opportunity BEFORE updating identity.
	// If another system already owns this (sysid, wacn), we merge into it
	// rather than trying to set the same values on our system (which would
	// violate the unique constraint).
	if p.mergeP25Systems && sys.Sysid != "" && sys.Sysid != "0" && sys.Wacn != "" && sys.Wacn != "0" {
		targetID, err := p.db.FindSystemBySysidWacn(ctx, sys.Sysid, sys.Wacn, identity.SystemID)
		if err != nil {
			return fmt.Errorf("find merge target: %w", err)
		}

		if targetID > 0 {
			// Merge this system into the existing one. After merge,
			// our system is soft-deleted — no need to update its identity.
			p.mergeSystem(ctx, identity.SystemID, targetID, sys.SysName)
			p.completeWarmup()

			// Update site fields on the (now-moved) site
			if err := p.db.UpdateSite(ctx, identity.SiteID, sys.SysNum, sys.Nac, sys.RFSS, sys.SiteID, sys.Type); err != nil {
				p.log.Warn().Err(err).Msg("failed to update site after merge")
			}

			p.log.Debug().
				Str("sys_name", sys.SysName).
				Int("merged_into", targetID).
				Str("sysid", sys.Sysid).
				Str("wacn", sys.Wacn).
				Msg("system info processed (merged)")
			return nil
		}
	}

	// No merge needed — update this system's identity (progressive refinement)
	if err := p.db.UpdateSystemIdentity(ctx, identity.SystemID, sys.Type, sys.Sysid, sys.Wacn, ""); err != nil {
		return fmt.Errorf("update system identity: %w", err)
	}

	// Release warmup gate when system identity is established:
	// - P25/smartnet: real sysid received, OR type is known (TR may send
	//   an early systems message with type="p25" before sysid is decoded)
	// - Conventional: type is known (no sysid to wait for)
	if (sys.Sysid != "" && sys.Sysid != "0") || sys.Type != "" {
		p.completeWarmup()
	}

	// Update site with P25-specific fields
	if err := p.db.UpdateSite(ctx, identity.SiteID, sys.SysNum, sys.Nac, sys.RFSS, sys.SiteID, sys.Type); err != nil {
		return fmt.Errorf("update site: %w", err)
	}

	p.log.Debug().
		Str("sys_name", sys.SysName).
		Int("system_id", identity.SystemID).
		Str("sysid", sys.Sysid).
		Str("wacn", sys.Wacn).
		Str("nac", sys.Nac).
		Msg("system info processed")

	return nil
}

// mergeSystem merges sourceID into targetID (the one that already has the sysid/wacn).
// The target is the "older" system — the one we found by sysid/wacn lookup.
func (p *Pipeline) mergeSystem(ctx context.Context, sourceID, targetID int, sysName string) {
	p.log.Info().
		Int("source_system_id", sourceID).
		Int("target_system_id", targetID).
		Str("sys_name", sysName).
		Msg("merging duplicate systems")

	callsMoved, tgMoved, tgMerged, unitsMoved, unitsMerged, eventsMoved, err := p.db.MergeSystems(ctx, sourceID, targetID, "auto:p25-identity-match")
	if err != nil {
		p.log.Error().Err(err).
			Int("source", sourceID).
			Int("target", targetID).
			Msg("system merge failed")
		return
	}

	// Update the identity cache so future lookups resolve to the merged
	// target, and relabel in-memory data (active calls, SSE replay buffer).
	p.RewriteSystemID(sourceID, targetID)

	p.log.Info().
		Int("source_system_id", sourceID).
		Int("target_system_id", targetID).
		Int("calls_moved", callsMoved).
		Int("talkgroups_moved", tgMoved).
		Int("talkgroups_merged", tgMerged).
		Int("units_moved", unitsMoved).
		Int("units_merged", unitsMerged).
		Int("events_moved", eventsMoved).
		Msg("system merge completed")
}
