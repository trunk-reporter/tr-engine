-- name: InsertCall :one
INSERT INTO calls (
    system_id, site_id, tgid, tr_call_id, call_num,
    start_time, stop_time, duration, freq, freq_error,
    signal_db, noise_db, error_count, spike_count,
    audio_type, phase2_tdma, tdma_slot, analog, conventional,
    encrypted, emergency,
    call_state, call_state_type, mon_state, mon_state_type,
    rec_state, rec_state_type, rec_num, src_num,
    patched_tgids,
    src_list, freq_list, unit_ids,
    system_name, site_short_name,
    tg_alpha_tag, tg_description, tg_tag, tg_group,
    incidentdata,
    instance_id
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12, $13, $14,
    $15, $16, $17, $18, $19,
    $20, $21,
    $22, $23, $24, $25,
    $26, $27, $28, $29,
    $30,
    $31, $32, $33,
    $34, $35,
    $36, $37, $38, $39,
    $40,
    $41
) RETURNING call_id;

-- name: UpdateCallEnd :exec
UPDATE calls SET
    stop_time = $3,
    duration = $4,
    freq = $5,
    freq_error = $6,
    signal_db = $7,
    noise_db = $8,
    error_count = $9,
    spike_count = $10,
    rec_state = $11,
    rec_state_type = $12,
    call_state = $13,
    call_state_type = $14,
    call_filename = $15,
    retry_attempt = $16,
    process_call_time = $17
WHERE call_id = $1 AND start_time = $2;

-- name: UpdateCallElapsed :exec
UPDATE calls SET
    stop_time = COALESCE($3, stop_time),
    duration = COALESCE($4, duration),
    updated_at = now()
WHERE call_id = $1 AND start_time = $2
    AND (duration IS NULL OR duration = 0);

-- name: UpdateCallStartFields :exec
UPDATE calls SET
    tr_call_id = $3,
    call_num = $4,
    instance_id = $5,
    call_state = $6,
    call_state_type = $7,
    mon_state = $8,
    mon_state_type = $9,
    rec_state = $10,
    rec_state_type = $11
WHERE call_id = $1 AND start_time = $2;

-- name: UpdateCallAudio :exec
UPDATE calls SET
    audio_file_path = $3,
    audio_file_size = $4
WHERE call_id = $1 AND start_time = $2;

-- name: UpdateCallFilename :exec
UPDATE calls SET call_filename = $3
WHERE call_id = $1 AND start_time = $2;

-- name: UpsertCallGroup :one
INSERT INTO call_groups (system_id, tgid, start_time, tg_alpha_tag, tg_description, tg_tag, tg_group)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (system_id, tgid, start_time) DO UPDATE SET
    tg_alpha_tag   = COALESCE(NULLIF($4, ''), call_groups.tg_alpha_tag),
    tg_description = COALESCE(NULLIF($5, ''), call_groups.tg_description)
RETURNING id;

-- name: SetCallGroupID :exec
UPDATE calls SET call_group_id = $3
WHERE call_id = $1 AND start_time = $2;

-- name: SetCallGroupPrimary :exec
UPDATE call_groups SET primary_call_id = $2
WHERE id = $1 AND primary_call_id IS NULL;

-- name: UpdateCallSrcFreq :exec
UPDATE calls SET
    src_list = $3,
    freq_list = $4,
    unit_ids = $5
WHERE call_id = $1 AND start_time = $2;

-- name: InsertCallFrequencies :copyfrom
INSERT INTO call_frequencies (
    call_id, call_start_time, freq,
    "time", pos, len,
    error_count, spike_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertCallTransmissions :copyfrom
INSERT INTO call_transmissions (
    call_id, call_start_time, src,
    "time", pos, duration, emergency,
    signal_system, tag
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertActiveCallCheckpoint :exec
INSERT INTO call_active_checkpoints (instance_id, active_calls, call_count)
VALUES ($1, $2, $3);

-- name: PurgeStaleCalls :execrows
DELETE FROM calls
WHERE rec_state_type = 'RECORDING'
    AND audio_file_path IS NULL
    AND (stop_time IS NULL OR duration IS NULL OR duration = 0)
    AND start_time < $1;

-- name: PurgeOrphanCallGroups :execrows
DELETE FROM call_groups cg
WHERE NOT EXISTS (SELECT 1 FROM calls c WHERE c.call_group_id = cg.id);

-- name: FindCallByTrCallID :one
-- Uses start_time hint ($2) for partition pruning when available.
-- Pass NULL to skip pruning and scan all partitions.
SELECT call_id, start_time FROM calls
WHERE tr_call_id = $1
    AND ($2::timestamptz IS NULL OR start_time BETWEEN $2 - interval '30 seconds' AND $2 + interval '30 seconds')
ORDER BY start_time DESC
LIMIT 1;

-- name: FindCallForAudio :one
SELECT call_id, start_time FROM calls
WHERE system_id = $1 AND tgid = $2
    AND start_time BETWEEN $3::timestamptz - interval '10 seconds' AND $3::timestamptz + interval '10 seconds'
ORDER BY ABS(EXTRACT(EPOCH FROM (start_time - $3::timestamptz)))
LIMIT 1;

-- name: GetCallAudioPath :one
SELECT COALESCE(audio_file_path, '') AS audio_file_path, COALESCE(call_filename, '') AS call_filename
FROM calls WHERE call_id = $1
ORDER BY start_time DESC LIMIT 1;

-- name: GetCallFreqList :one
SELECT freq_list FROM calls WHERE call_id = $1 ORDER BY start_time DESC LIMIT 1;

-- name: GetCallSrcList :one
SELECT src_list FROM calls WHERE call_id = $1 ORDER BY start_time DESC LIMIT 1;

-- name: UpdateCallSrcFreqIfNull :execrows
-- Like UpdateCallSrcFreq but only writes when src_list is NULL (no real data yet).
-- Returns rows affected: 1 = synthesized data written, 0 = real data already present.
UPDATE calls SET
    src_list = $3,
    freq_list = $4,
    unit_ids = $5
WHERE call_id = $1 AND start_time = $2 AND src_list IS NULL;
