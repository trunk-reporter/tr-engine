# Conventional/Analog System Ingest Improvements

**Date:** 2026-03-02
**Status:** Approved

## Context

tr-engine was built primarily for P25 trunked systems. Conventional and analog systems (pure FM, conventionalP25, conventionalDMR) work through the existing ingest pipeline but have several rough edges. Community user cpg178 is testing with 7 conventional systems + 1 P25 system in northeastern PA, and gofaster identified system categorization issues with mixed conventional types.

## Changes

### 1. System Type Taxonomy

**Problem:** TR sends 6 system types (`p25`, `smartnet`, `conventional`, `conventionalP25`, `conventionalDMR`, `conventionalSIGMF`). The `systems.system_type` CHECK constraint only allows 3 (`p25`, `smartnet`, `conventional`), so `conventionalP25` etc. are rejected. Two systems with the same shortName but different types both display as "CONVENTIONAL".

**Solution:** Expand the CHECK constraint to accept all 6 TR types. Fix `CreateSystem` SQL to accept the type as a parameter instead of hard-coding `'p25'`.

```sql
-- Migration: expand system_type CHECK
ALTER TABLE systems DROP CONSTRAINT IF EXISTS systems_system_type_check;
ALTER TABLE systems ADD CONSTRAINT systems_system_type_check
    CHECK (system_type IN ('p25', 'smartnet', 'conventional', 'conventionalP25', 'conventionalDMR', 'conventionalSIGMF'));
```

**Files:**
- `schema.sql` — update CHECK constraint
- `sql/queries/systems.sql` — `CreateSystem` accepts type parameter
- `internal/database/sqlcdb/systems.sql.go` — regenerated
- `internal/database/systems.go` — pass type to `CreateSystem`
- `internal/database/migrations.go` — add migration for existing databases
- `internal/ingest/identity.go` — thread system type through `Resolve()`

### 2. Unit -1 Filtering

**Problem:** Conventional systems send `unit: -1` on `call_start` and `unit_event:call` since there's no trunking signaling to identify the transmitting unit. The unit events handler upserts all units including -1, polluting the `units` table and UI.

**Solution:** Filter out `unit_id <= 0` in the unit event handler before upserting. The call handler already guards with `if call.Unit > 0` for its upsert path.

**Files:**
- `internal/ingest/handler_units.go` — skip upsert when unit_id <= 0

### 3. Warmup Gate for Conventional Systems

**Problem:** The warmup gate waits for P25 `sysid` before releasing buffered messages. Conventional systems never have a sysid — they fall through to the 5s timeout. But the `systems` message with `type=conventional*` arrives much sooner.

**Solution:** In `processSystemInfo()`, release the warmup gate when a conventional-type system is detected (any type starting with `conventional`), not just when a non-zero sysid arrives.

**Files:**
- `internal/ingest/handler_systems.go` — release warmup on conventional type detection

### 4. Frequency-as-Talkgroup Display (future)

**Problem:** Conventional systems derive pseudo-talkgroup IDs from frequencies (e.g., 154.31 MHz -> tgid 15431). The derivation is inconsistent across systems (some multiply by 100, some by 1000). The raw integer tgid is meaningless to users.

**Solution:** Store as-is (no conversion changes). Add display formatting in the web UI that detects frequency-derived tgids and renders them as MHz values. This is UI-only work, deferred to a separate change.

## Non-Goals

- Structured signal data (MDC1200, FleetSync) — MDC IDs already flow through `call_end` unit fields and unit events. The MQTT plugin signal() PR is nice-to-have but not blocking.
- Frequency-to-tgid derivation changes in TR — that's upstream's domain.
- Full analog metadata enrichment — handle incrementally as we learn what data is available.
