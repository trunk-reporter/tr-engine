-- name: GetTalkgroupByComposite :one
SELECT t.system_id, COALESCE(s.name, '') AS system_name, s.sysid,
    t.tgid, COALESCE(t.alpha_tag, '') AS alpha_tag, COALESCE(t.tag, '') AS tag,
    COALESCE(t."group", '') AS "group", COALESCE(t.description, '') AS description,
    t.mode, t.priority, t.first_seen, t.last_seen,
    (SELECT count(*)::int FROM calls c WHERE c.system_id = t.system_id AND c.tgid = t.tgid AND c.start_time > now() - interval '30 days') AS call_count,
    (SELECT count(*)::int FROM calls c WHERE c.system_id = t.system_id AND c.tgid = t.tgid AND c.start_time > now() - interval '1 hour') AS calls_1h,
    (SELECT count(*)::int FROM calls c WHERE c.system_id = t.system_id AND c.tgid = t.tgid AND c.start_time > now() - interval '24 hours') AS calls_24h,
    GREATEST(
        (SELECT count(DISTINCT unit_rid)::int FROM unit_events ue WHERE ue.system_id = t.system_id AND ue.tgid = t.tgid AND ue.time > now() - interval '30 days'),
        (SELECT count(DISTINCT u)::int FROM calls c, unnest(c.unit_ids) AS u WHERE c.system_id = t.system_id AND c.tgid = t.tgid AND c.start_time > now() - interval '30 days' AND c.unit_ids IS NOT NULL)
    ) AS unit_count
FROM talkgroups t
JOIN systems s ON s.system_id = t.system_id
WHERE t.system_id = $1 AND t.tgid = $2;

-- name: FindTalkgroupSystems :many
SELECT t.system_id, COALESCE(s.name, '') AS system_name, s.sysid
FROM talkgroups t
JOIN systems s ON s.system_id = t.system_id AND s.deleted_at IS NULL
WHERE t.tgid = $1;

-- name: UpdateTalkgroupFields :exec
UPDATE talkgroups SET
    alpha_tag        = CASE WHEN @alpha_tag::text <> '' THEN @alpha_tag ELSE alpha_tag END,
    alpha_tag_source = CASE WHEN @alpha_tag_source::text <> '' THEN @alpha_tag_source ELSE alpha_tag_source END,
    description = CASE WHEN @description::text <> '' THEN @description ELSE description END,
    "group"     = CASE WHEN @tg_group::text <> '' THEN @tg_group ELSE "group" END,
    tag         = CASE WHEN @tag::text <> '' THEN @tag ELSE tag END,
    priority    = CASE WHEN @priority::int >= 0 THEN @priority ELSE priority END
WHERE system_id = @system_id AND tgid = @tgid;

-- name: UpsertTalkgroup :one
INSERT INTO talkgroups (system_id, tgid, alpha_tag, tag, "group", description, first_seen, last_seen)
VALUES (@system_id, @tgid, @alpha_tag, @tag, @tg_group, @description, @event_time, @event_time)
ON CONFLICT (system_id, tgid) DO UPDATE SET
    alpha_tag   = CASE WHEN COALESCE(talkgroups.alpha_tag_source, '') IN ('manual', 'csv') THEN talkgroups.alpha_tag
                       ELSE COALESCE(NULLIF(@alpha_tag, ''), talkgroups.alpha_tag) END,
    tag         = CASE WHEN COALESCE(talkgroups.alpha_tag_source, '') IN ('manual', 'csv') THEN talkgroups.tag
                       ELSE COALESCE(NULLIF(@tag, ''), talkgroups.tag) END,
    "group"     = CASE WHEN COALESCE(talkgroups.alpha_tag_source, '') IN ('manual', 'csv') THEN talkgroups."group"
                       ELSE COALESCE(NULLIF(@tg_group, ''), talkgroups."group") END,
    description = CASE WHEN COALESCE(talkgroups.alpha_tag_source, '') IN ('manual', 'csv') THEN talkgroups.description
                       ELSE COALESCE(NULLIF(@description, ''), talkgroups.description) END,
    first_seen  = LEAST(talkgroups.first_seen, @event_time),
    last_seen   = GREATEST(talkgroups.last_seen, @event_time)
RETURNING COALESCE(alpha_tag, '') AS alpha_tag;

-- name: UpsertTalkgroupDirectory :exec
INSERT INTO talkgroup_directory (system_id, tgid, alpha_tag, mode, description, tag, category, priority)
VALUES (@system_id, @tgid, @alpha_tag, @mode, @description, @tag, @category, @priority)
ON CONFLICT (system_id, tgid) DO UPDATE SET
    alpha_tag   = COALESCE(NULLIF(@alpha_tag, ''), talkgroup_directory.alpha_tag),
    mode        = COALESCE(NULLIF(@mode, ''), talkgroup_directory.mode),
    description = COALESCE(NULLIF(@description, ''), talkgroup_directory.description),
    tag         = COALESCE(NULLIF(@tag, ''), talkgroup_directory.tag),
    category    = COALESCE(NULLIF(@category, ''), talkgroup_directory.category),
    priority    = COALESCE(@priority, talkgroup_directory.priority),
    imported_at = now();

-- name: EnrichTalkgroupsFromDirectory :execrows
UPDATE talkgroups t SET
    alpha_tag   = CASE WHEN COALESCE(t.alpha_tag_source, '') = 'manual' THEN t.alpha_tag
                       ELSE COALESCE(NULLIF(t.alpha_tag, ''), td.alpha_tag) END,
    alpha_tag_source = CASE WHEN COALESCE(t.alpha_tag_source, '') = 'manual' THEN t.alpha_tag_source
                            WHEN COALESCE(t.alpha_tag, '') = '' AND COALESCE(td.alpha_tag, '') <> '' THEN 'csv'
                            ELSE t.alpha_tag_source END,
    tag         = COALESCE(NULLIF(t.tag, ''), td.tag),
    "group"     = COALESCE(NULLIF(t."group", ''), td.category),
    description = COALESCE(NULLIF(t.description, ''), td.description),
    mode        = COALESCE(t.mode, td.mode),
    priority    = COALESCE(t.priority, td.priority)
FROM talkgroup_directory td
WHERE td.system_id = t.system_id AND td.tgid = t.tgid
  AND t.system_id = @system_id
  AND (@tgid::int = 0 OR t.tgid = @tgid);
