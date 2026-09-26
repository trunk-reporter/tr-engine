package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database/sqlcdb"
)

type System struct {
	SystemID   int
	SystemType string
	Name       string
	Sysid      string
	Wacn       string
}

// FindOrCreateSystem finds an existing system by (instance_id, sys_name) via the sites table,
// or creates a new one. Returns the system_id and sysid (P25 system identifier).
// systemType is used when creating a new system; if empty, defaults to "conventional".
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

// FindSystemsByName returns the non-deleted systems a CSV import's system_name
// refers to: those whose name, or one of whose sites' short_name (TR's
// shortName), equals name. It never creates a system: ingest finds systems only
// through their sites, so a site-less system created here would never receive
// traffic, and the first call from that trunk-recorder would create a second
// system with the same name.
func (db *DB) FindSystemsByName(ctx context.Context, name string) ([]AmbiguousMatch, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT s.system_id, COALESCE(s.name, ''), s.sysid
		FROM systems s
		WHERE s.deleted_at IS NULL
		  AND (s.name = $1 OR EXISTS (
		      SELECT 1 FROM sites st WHERE st.system_id = s.system_id AND st.short_name = $1))
		ORDER BY s.system_id
	`, name)
	if err != nil {
		return nil, fmt.Errorf("find systems named %q: %w", name, err)
	}
	defer rows.Close()
	var matches []AmbiguousMatch
	for rows.Next() {
		var m AmbiguousMatch
		if err := rows.Scan(&m.SystemID, &m.SystemName, &m.Sysid); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find systems named %q: %w", name, err)
	}
	return matches, nil
}

// UpdateSystemIdentity updates a system's P25 identity fields.
func (db *DB) UpdateSystemIdentity(ctx context.Context, systemID int, systemType, sysid, wacn, name string) error {
	return db.Q.UpdateSystemIdentity(ctx, sqlcdb.UpdateSystemIdentityParams{
		SystemID:   systemID,
		SystemType: systemType,
		Sysid:      sysid,
		Wacn:       wacn,
		Name:       name,
	})
}

// FindSystemViaSiteIdentity returns the system_id for a site identified by (instance_id, short_name).
// Returns 0, nil if not found.
func (db *DB) FindSystemViaSiteIdentity(ctx context.Context, instanceID, shortName string) (int, error) {
	row, err := db.Q.FindSystemViaSite(ctx, sqlcdb.FindSystemViaSiteParams{
		InstanceID: instanceID,
		ShortName:  shortName,
	})
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return row.SystemID, nil
}

// FindSystemBySysidWacn finds an active system by (sysid, wacn), excluding a given system_id.
func (db *DB) FindSystemBySysidWacn(ctx context.Context, sysid, wacn string, excludeSystemID int) (int, error) {
	systemID, err := db.Q.FindSystemBySysidWacn(ctx, sqlcdb.FindSystemBySysidWacnParams{
		Sysid:    sysid,
		Wacn:     wacn,
		SystemID: excludeSystemID,
	})
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return systemID, err
}

// mergeSourceWins is true when a merged source row's alpha_tag_source ($4) ranks
// at least as high as the target row's (manual > csv > MQTT-discovered/other).
// A winning source row's non-empty tag fields replace the target's; otherwise it
// only fills the target's empty fields. So a merge never replaces a manual or CSV
// tag with a lower-priority one. %s is the table name.
//
// alpha_tag_source follows alpha_tag (see mergeTagSource): the target takes the
// source's label only when it takes the source's alpha_tag, so a kept tag never
// gets another row's label.
const mergeSourceWins = `(CASE $4 WHEN 'manual' THEN 3 WHEN 'csv' THEN 2 ELSE 1 END >=
	CASE %s.alpha_tag_source WHEN 'manual' THEN 3 WHEN 'csv' THEN 2 ELSE 1 END)`

// mergeTagSource is the alpha_tag_source of a merged row: the source row's ($4)
// when its non-empty alpha_tag ($3) is taken, i.e. it wins (mergeSourceWins) or
// fills an empty target tag; otherwise the target's. %[1]s is the
// mergeSourceWins expression, %[2]s the table name.
const mergeTagSource = `CASE WHEN NULLIF($3, '') IS NULL THEN %[2]s.alpha_tag_source
	WHEN %[1]s OR NULLIF(%[2]s.alpha_tag, '') IS NULL THEN NULLIF($4, '')
	ELSE %[2]s.alpha_tag_source END`

// MergeSystems moves all child records from sourceID to targetID and soft-deletes the source.
// Talkgroup and unit tags are combined by alpha_tag_source priority (mergeSourceWins).
// API key and anonymous-policy restrictions that name the source are rewritten to
// the target in the same transaction, and the auth generation is bumped after commit.
// Returns counts of moved records for the merge log.
//
// NOTE: UPDATE statements on partitioned tables (calls, unit_events, trunking_messages)
// cannot be pruned by start_time since there is no time constraint — Postgres scans all
// partitions. This is acceptable for rare admin operations but will be slow for systems
// with years of data. Consider chunking by partition if this becomes a problem.
func (db *DB) MergeSystems(ctx context.Context, sourceID, targetID int, performedBy string) (callsMoved, tgMoved, tgMerged, unitsMoved, unitsMerged, eventsMoved int, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize with the unit tag suggestion scanner's writes and with
	// approve/dismiss (see unitTagMergeLockKey). Taken first, before any row lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, unitTagMergeLockKey); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("lock against unit tag scanner: %w", err)
	}

	// Move calls
	tag, err := tx.Exec(ctx, `UPDATE calls SET system_id = $1 WHERE system_id = $2`, targetID, sourceID)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move calls: %w", err)
	}
	callsMoved = int(tag.RowsAffected())

	// Move call_groups — handle conflicts first
	rows, err := tx.Query(ctx, `
		SELECT sg.id as source_group_id, tg.id as target_group_id
		FROM call_groups sg
		JOIN call_groups tg ON tg.system_id = $1 AND tg.tgid = sg.tgid AND tg.start_time = sg.start_time
		WHERE sg.system_id = $2
	`, targetID, sourceID)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("find conflicting call groups: %w", err)
	}
	var cgConflicts []struct{ src, dst int }
	for rows.Next() {
		var src, dst int
		if err := rows.Scan(&src, &dst); err != nil {
			rows.Close()
			return 0, 0, 0, 0, 0, 0, err
		}
		cgConflicts = append(cgConflicts, struct{ src, dst int }{src, dst})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("iterate call group conflicts: %w", err)
	}

	for _, c := range cgConflicts {
		if _, err := tx.Exec(ctx, `UPDATE calls SET call_group_id = $1 WHERE call_group_id = $2`, c.dst, c.src); err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("reassign call group calls: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM call_groups WHERE id = $1`, c.src); err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("delete conflicting call group: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE call_groups SET system_id = $1 WHERE system_id = $2`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move call groups: %w", err)
	}

	// Merge talkgroups. Tags follow the alpha_tag_source priority (see mergeSourceWins).
	type tgRow struct {
		tgid                         int
		alpha, tag, group, desc, src string
		mode                         *string
		priority                     *int
	}
	tgRows, err := tx.Query(ctx, `SELECT tgid, COALESCE(alpha_tag,''), COALESCE(tag,''), COALESCE("group",''), COALESCE(description,''),
		COALESCE(alpha_tag_source,''), mode, priority FROM talkgroups WHERE system_id = $1`, sourceID)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("read source talkgroups: %w", err)
	}
	var tgs []tgRow
	for tgRows.Next() {
		var r tgRow
		if err := tgRows.Scan(&r.tgid, &r.alpha, &r.tag, &r.group, &r.desc, &r.src, &r.mode, &r.priority); err != nil {
			tgRows.Close()
			return 0, 0, 0, 0, 0, 0, err
		}
		tgs = append(tgs, r)
	}
	tgRows.Close()
	if err := tgRows.Err(); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("iterate source talkgroups: %w", err)
	}

	tgWins := fmt.Sprintf(mergeSourceWins, "talkgroups")
	tgSource := fmt.Sprintf(mergeTagSource, tgWins, "talkgroups")
	for _, r := range tgs {
		result, err := tx.Exec(ctx, `
			INSERT INTO talkgroups (system_id, tgid, alpha_tag, alpha_tag_source, tag, "group", description, mode, priority, first_seen, last_seen)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, now(), now())
			ON CONFLICT (system_id, tgid) DO UPDATE SET
				alpha_tag = CASE WHEN `+tgWins+`
				                 THEN COALESCE(NULLIF($3, ''), talkgroups.alpha_tag)
				                 ELSE COALESCE(NULLIF(talkgroups.alpha_tag, ''), NULLIF($3, ''), talkgroups.alpha_tag) END,
				alpha_tag_source = `+tgSource+`,
				tag = CASE WHEN `+tgWins+`
				           THEN COALESCE(NULLIF($5, ''), talkgroups.tag)
				           ELSE COALESCE(NULLIF(talkgroups.tag, ''), NULLIF($5, ''), talkgroups.tag) END,
				"group" = CASE WHEN `+tgWins+`
				               THEN COALESCE(NULLIF($6, ''), talkgroups."group")
				               ELSE COALESCE(NULLIF(talkgroups."group", ''), NULLIF($6, ''), talkgroups."group") END,
				description = CASE WHEN `+tgWins+`
				                   THEN COALESCE(NULLIF($7, ''), talkgroups.description)
				                   ELSE COALESCE(NULLIF(talkgroups.description, ''), NULLIF($7, ''), talkgroups.description) END,
				mode     = CASE WHEN `+tgWins+` THEN COALESCE($8, talkgroups.mode) ELSE COALESCE(talkgroups.mode, $8) END,
				priority = CASE WHEN `+tgWins+` THEN COALESCE($9, talkgroups.priority) ELSE COALESCE(talkgroups.priority, $9) END
		`, targetID, r.tgid, r.alpha, r.src, r.tag, r.group, r.desc, r.mode, r.priority)
		if err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("merge talkgroup %d: %w", r.tgid, err)
		}
		tgMoved++
		if result.RowsAffected() == 0 {
			tgMerged++
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM talkgroups WHERE system_id = $1`, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("delete source talkgroups: %w", err)
	}

	// Merge units. Tags follow the alpha_tag_source priority (see mergeSourceWins);
	// recorder/OTA tag observations merge like an archive import (unitObservationMergeSQL).
	type unitRow struct {
		unitID                    int
		alpha, src                string
		recorderTag, otaTag       string
		recorderSeen              *time.Time
		otaFirstSeen, otaLastSeen *time.Time
	}
	uRows, err := tx.Query(ctx, `SELECT unit_id, COALESCE(alpha_tag,''), COALESCE(alpha_tag_source,''),
		COALESCE(recorder_alpha_tag,''), recorder_alpha_tag_seen,
		COALESCE(ota_alpha_tag,''), ota_alpha_tag_first_seen, ota_alpha_tag_last_seen
		FROM units WHERE system_id = $1`, sourceID)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("read source units: %w", err)
	}
	var units []unitRow
	for uRows.Next() {
		var r unitRow
		if err := uRows.Scan(&r.unitID, &r.alpha, &r.src, &r.recorderTag, &r.recorderSeen,
			&r.otaTag, &r.otaFirstSeen, &r.otaLastSeen); err != nil {
			uRows.Close()
			return 0, 0, 0, 0, 0, 0, err
		}
		units = append(units, r)
	}
	uRows.Close()
	if err := uRows.Err(); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("iterate source units: %w", err)
	}

	unitWins := fmt.Sprintf(mergeSourceWins, "units")
	unitSource := fmt.Sprintf(mergeTagSource, unitWins, "units")
	for _, r := range units {
		result, err := tx.Exec(ctx, `
			INSERT INTO units (system_id, unit_id, alpha_tag, alpha_tag_source, first_seen, last_seen,
				recorder_alpha_tag, recorder_alpha_tag_seen,
				ota_alpha_tag, ota_alpha_tag_first_seen, ota_alpha_tag_last_seen)
			VALUES ($1, $2, $3, NULLIF($4, ''), now(), now(), NULLIF($5::text, ''), $6, NULLIF($7::text, ''), $8, $9)
			ON CONFLICT (system_id, unit_id) DO UPDATE SET
				alpha_tag = CASE WHEN `+unitWins+`
				                 THEN COALESCE(NULLIF($3, ''), units.alpha_tag)
				                 ELSE COALESCE(NULLIF(units.alpha_tag, ''), NULLIF($3, ''), units.alpha_tag) END,
				alpha_tag_source = `+unitSource+`,`+unitObservationMergeSQL+`
		`, targetID, r.unitID, r.alpha, r.src, r.recorderTag, r.recorderSeen, r.otaTag, r.otaFirstSeen, r.otaLastSeen)
		if err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("merge unit %d: %w", r.unitID, err)
		}
		unitsMoved++
		if result.RowsAffected() == 0 {
			unitsMerged++
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM units WHERE system_id = $1`, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("delete source units: %w", err)
	}

	// Merge the talkgroup directory (imported CSV reference data). The
	// target keeps its non-empty values and takes the source's where it has
	// none. Leaving the rows on the source would hide them from directory
	// enrichment of the merged talkgroups, and from restrictions: an
	// exclusion rewritten to target:tgid would no longer match them.
	if _, err := tx.Exec(ctx, `
		INSERT INTO talkgroup_directory AS d (system_id, tgid, alpha_tag, mode, description, tag, category, priority, imported_at)
		SELECT $1, tgid, alpha_tag, mode, description, tag, category, priority, imported_at
		FROM talkgroup_directory WHERE system_id = $2
		ON CONFLICT (system_id, tgid) DO UPDATE SET
			alpha_tag   = COALESCE(NULLIF(d.alpha_tag, ''), EXCLUDED.alpha_tag),
			mode        = COALESCE(NULLIF(d.mode, ''), EXCLUDED.mode),
			description = COALESCE(NULLIF(d.description, ''), EXCLUDED.description),
			tag         = COALESCE(NULLIF(d.tag, ''), EXCLUDED.tag),
			category    = COALESCE(NULLIF(d.category, ''), EXCLUDED.category),
			priority    = COALESCE(d.priority, EXCLUDED.priority),
			imported_at = GREATEST(d.imported_at, EXCLUDED.imported_at)
	`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("merge talkgroup directory: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM talkgroup_directory WHERE system_id = $1`, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("delete source talkgroup directory: %w", err)
	}

	// Move unit tag suggestions. Evidence records each call's call group so
	// another site's recording of the same transmission is not counted
	// twice; point the source's evidence at the groups its calls now belong
	// to (the conflicting source groups were folded into target groups above).
	if len(cgConflicts) > 0 {
		srcGroups := make([]int64, len(cgConflicts))
		dstGroups := make([]int64, len(cgConflicts))
		for i, c := range cgConflicts {
			srcGroups[i], dstGroups[i] = int64(c.src), int64(c.dst)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE unit_tag_suggestions s SET evidence = (
				SELECT COALESCE(jsonb_agg(
					CASE WHEN m.dst IS NULL THEN e.item
					     ELSE jsonb_set(e.item, '{call_group_id}', to_jsonb(m.dst)) END
					ORDER BY e.ord), '[]'::jsonb)
				FROM jsonb_array_elements(s.evidence) WITH ORDINALITY AS e(item, ord)
				LEFT JOIN unnest($2::bigint[], $3::bigint[]) AS m(src, dst)
				  ON m.src = (e.item ->> 'call_group_id')::bigint
			)
			WHERE s.system_id = $1
			  AND EXISTS (
				SELECT 1 FROM jsonb_array_elements(s.evidence) AS e(item)
				JOIN unnest($2::bigint[]) AS m(src) ON m.src = (e.item ->> 'call_group_id')::bigint)
		`, sourceID, srcGroups, dstGroups); err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("remap unit tag suggestion call groups: %w", err)
		}
	}

	// Where the target already has the same (unit, tag) candidate, fold the
	// source into it. Source evidence for a call or call group the target
	// already holds is the same transmission (both systems recorded it, which
	// is usually why they are being merged): it is dropped and not counted
	// again. Like the scanner's own de-duplication this only sees the capped
	// evidence lists, and occurrences assume one mention per duplicate call.
	// Evidence is kept newest first, capped at 10; the target's decision is
	// kept (adopting the source's if the target is still pending), and the
	// current-tag flag comes from whichever row was seen last.
	if _, err := tx.Exec(ctx, `
		UPDATE unit_tag_suggestions t SET
			occurrences = GREATEST(t.occurrences + f.occurrences - f.dups, t.call_count + f.call_count - f.dups),
			call_count  = t.call_count + f.call_count - f.dups,
			matches_current_tag = CASE WHEN f.last_seen > t.last_seen THEN f.matches_current_tag ELSE t.matches_current_tag END,
			tag_at_sighting     = CASE WHEN f.last_seen > t.last_seen THEN f.tag_at_sighting ELSE t.tag_at_sighting END,
			first_seen  = LEAST(t.first_seen, f.first_seen),
			last_seen   = GREATEST(t.last_seen, f.last_seen),
			evidence    = (
				SELECT COALESCE(jsonb_agg(e.item ORDER BY e.at DESC, e.ord), '[]'::jsonb)
				FROM (
					SELECT item, ord, (item ->> 'call_start_time')::timestamptz AS at
					FROM jsonb_array_elements(t.evidence || f.fresh) WITH ORDINALITY AS x(item, ord)
					ORDER BY at DESC, ord LIMIT 10
				) e
			),
			status      = CASE WHEN t.status = 'pending' THEN f.status ELSE t.status END,
			applied_tag = CASE WHEN t.status = 'pending' THEN f.applied_tag ELSE t.applied_tag END,
			previous_tag = CASE WHEN t.status = 'pending' THEN f.previous_tag ELSE t.previous_tag END,
			previous_tag_source = CASE WHEN t.status = 'pending' THEN f.previous_tag_source ELSE t.previous_tag_source END,
			decided_at  = CASE WHEN t.status = 'pending' THEN f.decided_at ELSE t.decided_at END,
			decided_by  = CASE WHEN t.status = 'pending' THEN f.decided_by ELSE t.decided_by END
		FROM (
			SELECT s.*, tt.id AS target_id, d.dups, d.fresh
			FROM unit_tag_suggestions s
			JOIN unit_tag_suggestions tt
			  ON tt.system_id = $1 AND tt.unit_id = s.unit_id AND tt.tag_key = s.tag_key
			CROSS JOIN LATERAL (
				SELECT count(*) FILTER (WHERE x.dup) AS dups,
					COALESCE(jsonb_agg(x.item ORDER BY x.ord) FILTER (WHERE NOT x.dup), '[]'::jsonb) AS fresh
				FROM (
					SELECT e.item, e.ord,
						tt.evidence @> jsonb_build_array(jsonb_build_object('call_id', e.item -> 'call_id'))
						OR (e.item -> 'call_group_id' IS NOT NULL
						    AND tt.evidence @> jsonb_build_array(jsonb_build_object('call_group_id', e.item -> 'call_group_id')))
						AS dup
					FROM jsonb_array_elements(s.evidence) WITH ORDINALITY AS e(item, ord)
				) x
			) d
			WHERE s.system_id = $2
		) f
		WHERE t.id = f.target_id
	`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("merge unit tag suggestions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM unit_tag_suggestions s
		USING unit_tag_suggestions t
		WHERE s.system_id = $2 AND t.system_id = $1
		  AND t.unit_id = s.unit_id AND t.tag_key = s.tag_key
	`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("delete merged unit tag suggestions: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE unit_tag_suggestions SET system_id = $1 WHERE system_id = $2`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move unit tag suggestions: %w", err)
	}

	// Move unit_events
	tag, err = tx.Exec(ctx, `UPDATE unit_events SET system_id = $1 WHERE system_id = $2`, targetID, sourceID)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move unit_events: %w", err)
	}
	eventsMoved = int(tag.RowsAffected())

	// Move trunking_messages
	if _, err := tx.Exec(ctx, `UPDATE trunking_messages SET system_id = $1 WHERE system_id = $2`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move trunking_messages: %w", err)
	}

	// Move decode_rates
	if _, err := tx.Exec(ctx, `UPDATE decode_rates SET system_id = $1 WHERE system_id = $2`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move decode_rates: %w", err)
	}

	// Move sites to target system
	if _, err := tx.Exec(ctx, `UPDATE sites SET system_id = $1 WHERE system_id = $2`, targetID, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("move sites: %w", err)
	}

	// Combine system names
	var targetName, sourceName string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(name,'') FROM systems WHERE system_id = $1`, targetID).Scan(&targetName); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("read target system name: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(name,'') FROM systems WHERE system_id = $1`, sourceID).Scan(&sourceName); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("read source system name: %w", err)
	}
	if sourceName != "" && !strings.Contains(targetName, sourceName) {
		combined := targetName + "/" + sourceName
		if _, err := tx.Exec(ctx, `UPDATE systems SET name = $1 WHERE system_id = $2`, combined, targetID); err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("combine names: %w", err)
		}
	}

	// Soft-delete source system
	if _, err := tx.Exec(ctx, `UPDATE systems SET deleted_at = now() WHERE system_id = $1`, sourceID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("soft-delete source: %w", err)
	}

	// Point API key and anonymous-policy restrictions at the target, so that
	// allow lists keep their data and exclusions keep excluding (§3.2).
	// Exclusions keep their source entries too, for data that still carries
	// the source ID (auth.Restriction.RewriteSystem).
	if _, _, err := rewriteRestrictionsForMerge(ctx, tx, sourceID, targetID); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("rewrite restrictions: %w", err)
	}

	// Log the merge
	if _, err := tx.Exec(ctx, `
		INSERT INTO system_merge_log (source_id, target_id, calls_moved, talkgroups_moved, talkgroups_merged, units_moved, units_merged, events_moved, performed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, sourceID, targetID, callsMoved, tgMoved, tgMerged, unitsMoved, unitsMerged, eventsMoved, performedBy); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("log merge: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("commit merge: %w", err)
	}
	// Cached keys, the anonymous policy and open streams must see the
	// rewritten restrictions.
	auth.Bump()

	return callsMoved, tgMoved, tgMerged, unitsMoved, unitsMerged, eventsMoved, nil
}

// SystemAPI represents a system with embedded sites for API responses.
type SystemAPI struct {
	SystemID   int       `json:"system_id"`
	SystemType string    `json:"system_type"`
	Name       string    `json:"name,omitempty"`
	Sysid      string    `json:"sysid"`
	Wacn       string    `json:"wacn"`
	Sites      []SiteAPI `json:"sites"`
}

// GetSystemByID returns a single system with its sites. A system p may not
// see is pgx.ErrNoRows, like one that doesn't exist (§6.3). A nil p is
// ErrNoPrincipal.
func (db *DB) GetSystemByID(ctx context.Context, p *auth.Principal, systemID int) (*SystemAPI, error) {
	if p == nil {
		return nil, ErrNoPrincipal
	}
	if !p.SystemVisible(systemID) {
		return nil, pgx.ErrNoRows
	}
	row, err := db.Q.GetSystemByID(ctx, systemID)
	if err != nil {
		return nil, err
	}
	s := &SystemAPI{
		SystemID:   row.SystemID,
		SystemType: row.SystemType,
		Name:       row.Name,
		Sysid:      row.Sysid,
		Wacn:       row.Wacn,
	}
	sites, err := db.ListSitesForSystem(ctx, systemID)
	if err != nil {
		return nil, err
	}
	s.Sites = sites
	return s, nil
}

// ListSystemsWithSites returns all active systems p may see with their sites
// (§6.3). A nil p is ErrNoPrincipal.
func (db *DB) ListSystemsWithSites(ctx context.Context, p *auth.Principal) ([]SystemAPI, error) {
	if p == nil {
		return nil, ErrNoPrincipal
	}
	sysRows, err := db.Q.ListActiveSystems(ctx)
	if err != nil {
		return nil, err
	}

	systems := make([]SystemAPI, 0, len(sysRows))
	for _, r := range sysRows {
		if !p.SystemVisible(r.SystemID) {
			continue
		}
		systems = append(systems, SystemAPI{
			SystemID:   r.SystemID,
			SystemType: r.SystemType,
			Name:       r.Name,
			Sysid:      r.Sysid,
			Wacn:       r.Wacn,
		})
	}

	// Load sites for each system
	allSites, err := db.LoadAllSitesAPI(ctx)
	if err != nil {
		return nil, err
	}
	sitesBySystem := make(map[int][]SiteAPI)
	for _, s := range allSites {
		sitesBySystem[s.SystemID] = append(sitesBySystem[s.SystemID], s)
	}
	for i := range systems {
		systems[i].Sites = sitesBySystem[systems[i].SystemID]
		if systems[i].Sites == nil {
			systems[i].Sites = []SiteAPI{}
		}
	}

	return systems, nil
}

// P25SystemAPI represents a P25 system with stats.
type P25SystemAPI struct {
	SystemID       int       `json:"system_id"`
	Name           string    `json:"name,omitempty"`
	Sysid          string    `json:"sysid"`
	Wacn           string    `json:"wacn"`
	Sites          []SiteAPI `json:"sites"`
	TalkgroupCount int       `json:"talkgroup_count"`
	UnitCount      int       `json:"unit_count"`
	Calls24h       int       `json:"calls_24h"`
}

// ListP25Systems returns P25 systems with stats.
func (db *DB) ListP25Systems(ctx context.Context) ([]P25SystemAPI, error) {
	sysRows, err := db.Q.ListP25Systems(ctx)
	if err != nil {
		return nil, err
	}

	systems := make([]P25SystemAPI, len(sysRows))
	for i, r := range sysRows {
		systems[i] = P25SystemAPI{
			SystemID:       r.SystemID,
			Name:           r.Name,
			Sysid:         r.Sysid,
			Wacn:           r.Wacn,
			TalkgroupCount: int(r.TalkgroupCount),
			UnitCount:      int(r.UnitCount),
			Calls24h:       int(r.Calls24h),
		}
	}

	// Load sites
	allSites, err := db.LoadAllSitesAPI(ctx)
	if err != nil {
		return nil, err
	}
	sitesBySystem := make(map[int][]SiteAPI)
	for _, s := range allSites {
		sitesBySystem[s.SystemID] = append(sitesBySystem[s.SystemID], s)
	}
	for i := range systems {
		systems[i].Sites = sitesBySystem[systems[i].SystemID]
		if systems[i].Sites == nil {
			systems[i].Sites = []SiteAPI{}
		}
	}

	return systems, nil
}

// UpdateSystemFields updates mutable system fields. Only non-nil fields are updated.
func (db *DB) UpdateSystemFields(ctx context.Context, systemID int, name, sysid, wacn *string) error {
	nameVal := ""
	if name != nil {
		nameVal = *name
	}
	sysidVal := ""
	if sysid != nil {
		sysidVal = *sysid
	}
	wacnVal := ""
	if wacn != nil {
		wacnVal = *wacn
	}

	return db.Q.UpdateSystemFields(ctx, sqlcdb.UpdateSystemFieldsParams{
		SystemID: systemID,
		Name:     nameVal,
		Sysid:    sysidVal,
		Wacn:     wacnVal,
	})
}

// LoadAllSystems returns all active systems.
func (db *DB) LoadAllSystems(ctx context.Context) ([]System, error) {
	rows, err := db.Q.LoadAllSystems(ctx)
	if err != nil {
		return nil, err
	}
	systems := make([]System, len(rows))
	for i, r := range rows {
		systems[i] = System{
			SystemID:   r.SystemID,
			SystemType: r.SystemType,
			Name:       r.Name,
			Sysid:      r.Sysid,
			Wacn:       r.Wacn,
		}
	}
	return systems, nil
}
