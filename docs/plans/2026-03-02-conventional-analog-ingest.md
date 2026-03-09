# Conventional/Analog Ingest Improvements — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix rough edges in tr-engine's handling of conventional and analog radio systems — correct system type taxonomy, filter invalid units, and speed up conventional system startup.

**Architecture:** Three surgical changes to the ingest pipeline (schema constraint, unit filter, warmup gate) plus supporting SQL/migration changes. No new tables, no API changes, no new dependencies.

**Tech Stack:** Go, PostgreSQL, sqlc (code generation from SQL queries)

**Design doc:** `docs/plans/2026-03-02-conventional-analog-ingest-design.md`

---

### Task 1: Expand system_type CHECK constraint

The `systems` table CHECK constraint only allows `p25`, `smartnet`, `conventional`. trunk-recorder also sends `conventionalP25`, `conventionalDMR`, `conventionalSIGMF`. These are rejected by the constraint, causing the system type to stay as the default `p25`.

**Files:**
- Modify: `schema.sql:56` (CHECK constraint on systems table)
- Modify: `internal/database/migrations.go:18-61` (add new migration)

**Step 1: Update schema.sql CHECK constraint**

In `schema.sql`, line 56, change:
```sql
    system_type  text         NOT NULL CHECK (system_type IN ('p25', 'smartnet', 'conventional')),
```
to:
```sql
    system_type  text         NOT NULL CHECK (system_type IN ('p25', 'smartnet', 'conventional', 'conventionalP25', 'conventionalDMR', 'conventionalSIGMF')),
```

**Step 2: Add migration for existing databases**

In `internal/database/migrations.go`, append to the `migrations` slice (after the last entry, before the closing `}`):

```go
	{
		name: "expand system_type CHECK for conventional variants",
		sql: `ALTER TABLE systems DROP CONSTRAINT IF EXISTS systems_system_type_check;
ALTER TABLE systems ADD CONSTRAINT systems_system_type_check
    CHECK (system_type IN ('p25', 'smartnet', 'conventional', 'conventionalP25', 'conventionalDMR', 'conventionalSIGMF'))`,
		check: `SELECT EXISTS (
    SELECT 1 FROM information_schema.check_constraints
    WHERE constraint_name = 'systems_system_type_check'
      AND check_clause LIKE '%conventionalP25%'
)`,
	},
```

**Step 3: Verify build**

Run: `bash build.sh`
Expected: compiles successfully

**Step 4: Commit**

```
feat: expand system_type CHECK for conventional variants

trunk-recorder sends conventionalP25, conventionalDMR, and
conventionalSIGMF in addition to conventional. The CHECK
constraint now accepts all six types.
```

---

### Task 2: Make CreateSystem accept system_type parameter

Currently `CreateSystem` hard-codes `'p25'` as the type. Every new system starts as P25 then gets corrected by `UpdateSystemIdentity`. This means conventional systems briefly appear as P25. Fix by passing the type at creation time.

**Files:**
- Modify: `sql/queries/systems.sql:8-11` (CreateSystem query)
- Regenerate: `internal/database/sqlcdb/systems.sql.go` (via sqlc)
- Modify: `internal/database/systems.go:22-37` (FindOrCreateSystem)
- Modify: `internal/ingest/identity.go:76-134` (Resolve method — no change needed, type not available at this layer)

**Step 1: Update CreateSystem SQL query**

In `sql/queries/systems.sql`, replace the `CreateSystem` query (lines 8-11):

```sql
-- name: CreateSystem :one
INSERT INTO systems (system_type, name, sysid, wacn)
VALUES (@system_type, @name, '0', '0')
RETURNING system_id;
```

**Step 2: Regenerate sqlc**

Run: `sqlc generate`
Expected: `internal/database/sqlcdb/systems.sql.go` is updated. `CreateSystem` now takes `CreateSystemParams{SystemType, Name}` instead of just `*string`.

**Step 3: Update FindOrCreateSystem in systems.go**

In `internal/database/systems.go`, update the function signature and body. Change `FindOrCreateSystem` (line 22) to accept a `systemType` parameter and pass it to `CreateSystem`:

```go
func (db *DB) FindOrCreateSystem(ctx context.Context, instanceID, sysName, systemType string) (int, string, error) {
	row, err := db.Q.FindSystemViaSite(ctx, sqlcdb.FindSystemViaSiteParams{
		InstanceID: instanceID,
		ShortName:  sysName,
	})
	if err == nil {
		return row.SystemID, row.Sysid, nil
	}

	// Default to "conventional" for new systems — UpdateSystemIdentity will
	// correct this when the system info message arrives. "conventional" is a
	// safer default than "p25" since conventional systems may never send a
	// system info message with sysid/wacn.
	if systemType == "" {
		systemType = "conventional"
	}

	// Create new system
	systemID, err := db.Q.CreateSystem(ctx, sqlcdb.CreateSystemParams{
		SystemType: systemType,
		Name:       &sysName,
	})
	if err != nil {
		return 0, "", fmt.Errorf("create system %q: %w", sysName, err)
	}
	return systemID, "0", nil
}
```

**Step 4: Update all callers of FindOrCreateSystem**

In `internal/ingest/identity.go`, line 106, update the call to pass an empty string (the identity resolver doesn't know the type at resolve time — the system info handler corrects it later):

```go
	systemID, sysid, err := r.db.FindOrCreateSystem(ctx, instanceID, sysName, "")
```

Search for any other callers:
Run: `grep -rn "FindOrCreateSystem" internal/`
Update each call to pass `""` as the third argument.

**Step 5: Verify build**

Run: `bash build.sh`
Expected: compiles successfully

**Step 6: Commit**

```
refactor: CreateSystem accepts system_type parameter

New systems default to "conventional" instead of "p25" when
the type is unknown. UpdateSystemIdentity corrects the type
when the system info message arrives.
```

---

### Task 3: Filter unit -1 in unit event handler

Conventional systems send `unit: -1` because there's no trunking signaling to identify the transmitter. The unit event handler currently upserts all units including -1, creating a bogus unit record in every conventional system.

**Files:**
- Modify: `internal/ingest/handler_units.go:82-89` (upsert unit block)

**Step 1: Add unit_id guard**

In `internal/ingest/handler_units.go`, wrap the unit upsert block (lines 82-89) with a guard. Change:

```go
	// Upsert unit — returns the DB's effective alpha_tag (respects manual > csv > mqtt priority)
	effectiveUnitTag := data.UnitAlphaTag
	if dbTag, err := p.db.UpsertUnit(ctx, identity.SystemID, data.Unit,
		data.UnitAlphaTag, eventType, ts, data.Talkgroup,
	); err != nil {
		p.log.Warn().Err(err).Int("unit", data.Unit).Msg("failed to upsert unit")
	} else if dbTag != "" {
		effectiveUnitTag = dbTag
	}
```

to:

```go
	// Upsert unit — skip invalid unit IDs (conventional systems send -1)
	effectiveUnitTag := data.UnitAlphaTag
	if data.Unit > 0 {
		if dbTag, err := p.db.UpsertUnit(ctx, identity.SystemID, data.Unit,
			data.UnitAlphaTag, eventType, ts, data.Talkgroup,
		); err != nil {
			p.log.Warn().Err(err).Int("unit", data.Unit).Msg("failed to upsert unit")
		} else if dbTag != "" {
			effectiveUnitTag = dbTag
		}
	}
```

**Step 2: Also guard the affiliation map update**

In the same file, the affiliation map block at line 194 uses `data.Unit` in the key. Add a guard at the top of that section. Change:

```go
	// Update affiliation map
	affKey := affiliationKey{SystemID: identity.SystemID, UnitID: data.Unit}
	switch eventType {
```

to:

```go
	// Update affiliation map — skip invalid unit IDs
	if data.Unit <= 0 {
		return nil
	}
	affKey := affiliationKey{SystemID: identity.SystemID, UnitID: data.Unit}
	switch eventType {
```

Note: this early return also skips the DB insert and SSE publish for unit -1 events via the dedup/insert block above. But we still want the DB insert and SSE publish for valid units even with tgid 0. So the guard needs to be before the dedup block too. Actually, looking more carefully, the dedup block (line 93) and insert block (line 106) should also be skipped for unit <= 0. Move the guard earlier.

Replace the approach: instead of two separate guards, add a single early return right after identity resolution. After line 67 (`return fmt.Errorf("resolve identity: %w", err)`), add:

```go
	// Skip invalid unit IDs — conventional systems send -1
	if data.Unit <= 0 {
		return nil
	}
```

This skips the entire unit event processing (upsert, dedup, DB insert, SSE publish, affiliation) for invalid units.

**Step 3: Verify build**

Run: `bash build.sh`
Expected: compiles successfully

**Step 4: Commit**

```
fix: filter unit -1 events from conventional systems

Conventional systems send unit_id=-1 on call and end events
since there's no trunking signaling. Skip these entirely to
prevent bogus unit records in the database.
```

---

### Task 4: Release warmup gate on conventional system detection

The warmup gate waits for P25 sysid before releasing. Conventional systems never have a sysid — they fall through to the 5s timeout. The `systems` message with `type=conventional` arrives within ~1s. Release immediately on conventional type detection.

**Files:**
- Modify: `internal/ingest/handler_systems.go:80-85` (warmup release condition)

**Step 1: Add conventional warmup release**

In `internal/ingest/handler_systems.go`, after line 84 (`p.completeWarmup()`), add an additional condition. Change:

```go
	// Real P25 identity established — release warmup gate so buffered
	// calls create talkgroups/units under the correct system_id.
	if sys.Sysid != "" && sys.Sysid != "0" {
		p.completeWarmup()
	}
```

to:

```go
	// Release warmup gate when system identity is established:
	// - P25/smartnet: real sysid received
	// - Conventional: type is known (no sysid to wait for)
	if (sys.Sysid != "" && sys.Sysid != "0") || strings.HasPrefix(sys.Type, "conventional") {
		p.completeWarmup()
	}
```

**Step 2: Add strings import if needed**

Check if `strings` is already imported in `handler_systems.go`. If not, add it to the import block.

**Step 3: Verify build**

Run: `bash build.sh`
Expected: compiles successfully

**Step 4: Commit**

```
fix: release warmup gate immediately for conventional systems

Instead of waiting for the 5s timeout, release the warmup gate
as soon as a conventional system type is detected in the system
info message. P25 systems still wait for real sysid.
```

---

### Task 5: Update schema.sql and CLAUDE.md

Update the canonical schema and documentation to reflect the new system type values.

**Files:**
- Modify: `CLAUDE.md` (system type description near line 244)

**Step 1: Update CLAUDE.md**

Find the system_type description in CLAUDE.md. The line that says:
```
- `system_type IN ('p25', 'smartnet', 'conventional')`
```
should become:
```
- `system_type IN ('p25', 'smartnet', 'conventional', 'conventionalP25', 'conventionalDMR', 'conventionalSIGMF')`
```

Also update the "Implementation Status" section if it mentions system type handling.

**Step 2: Verify build one final time**

Run: `bash build.sh`
Expected: compiles successfully

**Step 3: Commit**

```
docs: update CLAUDE.md for expanded system types
```

---

### Task 6: Deploy and verify with live data

Deploy to tr-dashboard and verify against live MQTT feeds.

**Step 1: Deploy**

Run: `./deploy-dev.sh`

**Step 2: Verify system types**

Check the deployed instance's systems endpoint to confirm conventional systems now show correct types:

```bash
curl -s http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/systems | python3 -m json.tool
```

**Step 3: Verify unit -1 filtering**

Check that no unit with ID -1 exists in the database:

```bash
curl -s "http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/units?search=-1" | python3 -m json.tool
```

**Step 4: Check logs for warmup behavior**

```bash
ssh root@tr-dashboard 'docker compose -f /data/tr-engine/v1/docker-compose.yml logs tr-engine --tail 50 | grep -i warmup'
```
