package database

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/snarg/tr-engine/internal/auth"
)

// CallFilter specifies filters for listing calls.
type CallFilter struct {
	SystemIDs   []int
	SiteIDs     []int
	Sysids      []string
	Tgids       []int
	UnitIDs     []int
	Emergency   *bool
	Encrypted   *bool
	Deduplicate bool
	StartTime   *time.Time
	EndTime     *time.Time
	Limit       int
	Offset      int
	Sort        string
}

// CallAPI represents a call for API responses.
type CallAPI struct {
	CallID        int64     `json:"call_id"`
	CallGroupID   *int      `json:"call_group_id,omitempty"`
	SystemID      int       `json:"system_id"`
	SystemName    string    `json:"system_name,omitempty"`
	Sysid         string    `json:"sysid,omitempty"`
	SiteID        *int      `json:"site_id,omitempty"`
	SiteShortName string    `json:"site_short_name,omitempty"`
	Tgid          int       `json:"tgid"`
	TgAlphaTag    string    `json:"tg_alpha_tag,omitempty"`
	TgDescription string    `json:"tg_description,omitempty"`
	TgTag         string    `json:"tg_tag,omitempty"`
	TgGroup       string    `json:"tg_group,omitempty"`
	StartTime     time.Time `json:"start_time"`
	StopTime      *time.Time `json:"stop_time,omitempty"`
	Duration      *float32  `json:"duration,omitempty"`
	AudioURL      *string   `json:"audio_url,omitempty"`
	AudioType     string    `json:"audio_type,omitempty"`
	AudioSize     *int      `json:"audio_size,omitempty"`
	Freq          *int64    `json:"freq,omitempty"`
	FreqError     *int      `json:"freq_error,omitempty"`
	SignalDB      *float32  `json:"signal_db,omitempty"`
	NoiseDB       *float32  `json:"noise_db,omitempty"`
	ErrorCount    *int      `json:"error_count,omitempty"`
	SpikeCount    *int      `json:"spike_count,omitempty"`
	CallState     string    `json:"call_state,omitempty"`
	MonState      string    `json:"mon_state,omitempty"`
	Emergency     bool      `json:"emergency"`
	Encrypted     bool      `json:"encrypted"`
	Analog        bool      `json:"analog"`
	Conventional  bool      `json:"conventional"`
	Phase2TDMA    bool              `json:"phase2_tdma"`
	TDMASlot      *int16            `json:"tdma_slot,omitempty"`
	PatchedTgids  []int32           `json:"patched_tgids,omitempty"`
	SrcList       json.RawMessage   `json:"src_list,omitempty"`
	FreqList      json.RawMessage   `json:"freq_list,omitempty"`
	UnitIDs              []int32         `json:"unit_ids,omitempty"`
	HasTranscription     bool            `json:"has_transcription"`
	TranscriptionStatus  string          `json:"transcription_status,omitempty"`
	TranscriptionText    *string         `json:"transcription_text,omitempty"`
	TranscriptionWordCt  *int            `json:"transcription_word_count,omitempty"`
	MetadataJSON         json.RawMessage `json:"metadata_json,omitempty"`
	IncidentData         json.RawMessage `json:"incident_data,omitempty"`
	CallFilename         string          `json:"-"` // TR's original path, not exposed in JSON; used for audio resolution
}

// ListCalls returns calls matching the filter with a total count, limited to
// the talkgroups p may see (§7.2). It serves GET /calls and the talkgroup and
// unit call lists. The restriction is part of the WHERE shared by the count
// and the rows, separate from the user's filters; patched_tgids is cut down to
// allowed talkgroups. A nil p is ErrNoPrincipal.
func (db *DB) ListCalls(ctx context.Context, p *auth.Principal, filter CallFilter) ([]CallAPI, int, error) {
	// Always include the LEFT JOIN; the dedup condition skips it when not active.
	const fromClause = `FROM calls c
		JOIN systems s ON s.system_id = c.system_id
		LEFT JOIN call_groups cg ON cg.id = c.call_group_id`
	const filterClause = `
		WHERE ($1::timestamptz IS NULL OR c.start_time >= $1)
		  AND ($2::timestamptz IS NULL OR c.start_time < $2)
		  AND ($3::int[] IS NULL OR c.system_id = ANY($3))
		  AND ($4::int[] IS NULL OR c.site_id = ANY($4))
		  AND ($5::text[] IS NULL OR s.sysid = ANY($5))
		  AND ($6::int[] IS NULL OR c.tgid = ANY($6))
		  AND ($7::int[] IS NULL OR c.unit_ids && $7)
		  AND ($8::boolean IS NULL OR c.emergency = $8)
		  AND ($9::boolean IS NULL OR c.encrypted = $9)
		  AND ($10::boolean IS NOT TRUE OR c.call_group_id IS NULL OR c.call_id = cg.primary_call_id OR cg.primary_call_id IS NULL)`
	args := []any{
		filter.StartTime, filter.EndTime,
		pqIntArray(filter.SystemIDs), pqIntArray(filter.SiteIDs),
		pqStringArray(filter.Sysids), pqIntArray(filter.Tgids),
		pqIntArray(filter.UnitIDs), filter.Emergency, filter.Encrypted,
		filter.Deduplicate,
	}
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", len(args)+1)
	if err != nil {
		return nil, 0, err
	}
	whereClause := filterClause + restrict
	args = append(args, restrictArgs...)

	// Count query
	var total int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) "+fromClause+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Sort
	orderBy := "c.start_time DESC"
	if filter.Sort != "" {
		orderBy = filter.Sort
	}

	// Data query
	dataQuery := fmt.Sprintf(`
		SELECT c.call_id, c.call_group_id, c.system_id, COALESCE(c.system_name, ''), COALESCE(s.sysid, ''),
			c.site_id, COALESCE(c.site_short_name, ''),
			c.tgid, COALESCE(c.tg_alpha_tag, ''), COALESCE(c.tg_description, ''),
			COALESCE(c.tg_tag, ''), COALESCE(c.tg_group, ''),
			c.start_time, c.stop_time, c.duration,
			c.audio_file_path, COALESCE(c.audio_type, ''), c.audio_file_size,
			COALESCE(c.call_filename, ''),
			c.freq, c.freq_error, c.signal_db, c.noise_db, c.error_count, c.spike_count,
			COALESCE(c.call_state_type, ''), COALESCE(c.mon_state_type, ''),
			COALESCE(c.emergency, false), COALESCE(c.encrypted, false),
			COALESCE(c.analog, false), COALESCE(c.conventional, false),
			COALESCE(c.phase2_tdma, false), c.tdma_slot,
			c.patched_tgids,
			c.src_list, c.freq_list, c.unit_ids,
			COALESCE(c.has_transcription, false), COALESCE(c.transcription_status, 'none'),
			c.transcription_text, c.transcription_word_count,
			c.metadata_json, c.incidentdata
		%s %s
		ORDER BY %s
		LIMIT %s OFFSET %s
	`, fromClause, whereClause, orderBy, placeholder(len(args)+1), placeholder(len(args)+2))

	rows, err := db.Pool.Query(ctx, dataQuery, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var calls []CallAPI
	for rows.Next() {
		var c CallAPI
		var audioPath *string
		if err := rows.Scan(
			&c.CallID, &c.CallGroupID, &c.SystemID, &c.SystemName, &c.Sysid,
			&c.SiteID, &c.SiteShortName,
			&c.Tgid, &c.TgAlphaTag, &c.TgDescription, &c.TgTag, &c.TgGroup,
			&c.StartTime, &c.StopTime, &c.Duration,
			&audioPath, &c.AudioType, &c.AudioSize,
			&c.CallFilename,
			&c.Freq, &c.FreqError, &c.SignalDB, &c.NoiseDB, &c.ErrorCount, &c.SpikeCount,
			&c.CallState, &c.MonState,
			&c.Emergency, &c.Encrypted, &c.Analog, &c.Conventional,
			&c.Phase2TDMA, &c.TDMASlot,
			&c.PatchedTgids,
			&c.SrcList, &c.FreqList, &c.UnitIDs,
			&c.HasTranscription, &c.TranscriptionStatus,
			&c.TranscriptionText, &c.TranscriptionWordCt,
			&c.MetadataJSON, &c.IncidentData,
		); err != nil {
			return nil, 0, err
		}
		if audioPath != nil && *audioPath != "" {
			url := fmt.Sprintf("/api/v1/calls/%d/audio", c.CallID)
			c.AudioURL = &url
		}
		c.SrcList = NormalizeSrcFreqTimestamps(c.SrcList)
		c.FreqList = NormalizeSrcFreqTimestamps(c.FreqList)
		c.PatchedTgids = allowedPatchedTgids(p, c.SystemID, c.PatchedTgids)
		calls = append(calls, c)
	}
	if calls == nil {
		calls = []CallAPI{}
	}
	return calls, total, rows.Err()
}

// GetCallByID returns a single call. A call outside p's restriction is
// pgx.ErrNoRows, like one that doesn't exist (§6.3), and patched_tgids is cut
// down to allowed talkgroups. A nil p is ErrNoPrincipal.
func (db *DB) GetCallByID(ctx context.Context, p *auth.Principal, callID int64) (*CallAPI, error) {
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2)
	if err != nil {
		return nil, err
	}
	var c CallAPI
	var audioPath *string
	err = db.Pool.QueryRow(ctx, `
		SELECT c.call_id, c.call_group_id, c.system_id, COALESCE(c.system_name, ''), COALESCE(s.sysid, ''),
			c.site_id, COALESCE(c.site_short_name, ''),
			c.tgid, COALESCE(c.tg_alpha_tag, ''), COALESCE(c.tg_description, ''),
			COALESCE(c.tg_tag, ''), COALESCE(c.tg_group, ''),
			c.start_time, c.stop_time, c.duration,
			c.audio_file_path, COALESCE(c.audio_type, ''), c.audio_file_size,
			COALESCE(c.call_filename, ''),
			c.freq, c.freq_error, c.signal_db, c.noise_db, c.error_count, c.spike_count,
			COALESCE(c.call_state_type, ''), COALESCE(c.mon_state_type, ''),
			COALESCE(c.emergency, false), COALESCE(c.encrypted, false),
			COALESCE(c.analog, false), COALESCE(c.conventional, false),
			COALESCE(c.phase2_tdma, false), c.tdma_slot,
			c.patched_tgids,
			c.src_list, c.freq_list, c.unit_ids,
			COALESCE(c.has_transcription, false), COALESCE(c.transcription_status, 'none'),
			c.transcription_text, c.transcription_word_count,
			c.metadata_json, c.incidentdata
		FROM calls c
		JOIN systems s ON s.system_id = c.system_id
		WHERE c.call_id = $1`+restrict+`
		ORDER BY c.start_time DESC
		LIMIT 1
	`, append([]any{callID}, restrictArgs...)...).Scan(
		&c.CallID, &c.CallGroupID, &c.SystemID, &c.SystemName, &c.Sysid,
		&c.SiteID, &c.SiteShortName,
		&c.Tgid, &c.TgAlphaTag, &c.TgDescription, &c.TgTag, &c.TgGroup,
		&c.StartTime, &c.StopTime, &c.Duration,
		&audioPath, &c.AudioType, &c.AudioSize,
		&c.CallFilename,
		&c.Freq, &c.FreqError, &c.SignalDB, &c.NoiseDB, &c.ErrorCount, &c.SpikeCount,
		&c.CallState, &c.MonState,
		&c.Emergency, &c.Encrypted, &c.Analog, &c.Conventional,
		&c.Phase2TDMA, &c.TDMASlot,
		&c.PatchedTgids,
		&c.SrcList, &c.FreqList, &c.UnitIDs,
		&c.HasTranscription, &c.TranscriptionStatus,
		&c.TranscriptionText, &c.TranscriptionWordCt,
		&c.MetadataJSON, &c.IncidentData,
	)
	if err != nil {
		return nil, err
	}
	if audioPath != nil && *audioPath != "" {
		url := fmt.Sprintf("/api/v1/calls/%d/audio", c.CallID)
		c.AudioURL = &url
	}
	c.SrcList = NormalizeSrcFreqTimestamps(c.SrcList)
	c.FreqList = NormalizeSrcFreqTimestamps(c.FreqList)
	c.PatchedTgids = allowedPatchedTgids(p, c.SystemID, c.PatchedTgids)
	return &c, nil
}

// BackfillFilter specifies filters for finding untranscribed calls.
type BackfillFilter struct {
	SystemID    *int
	Tgids       []int
	StartTime   *time.Time
	EndTime     *time.Time
	MinDuration float64
	MaxDuration float64
}

// CountUntranscribedCalls returns the count of calls matching the filter
// that have no transcription and are not encrypted.
func (db *DB) CountUntranscribedCalls(ctx context.Context, filter BackfillFilter) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `
		SELECT count(*)
		FROM calls
		WHERE (has_transcription = false OR has_transcription IS NULL)
		  AND (transcription_status = 'none' OR transcription_status IS NULL)
		  AND (encrypted IS NULL OR encrypted = false)
		  AND audio_file_path IS NOT NULL
		  AND ($1::float8 IS NULL OR duration >= $1)
		  AND ($2::float8 IS NULL OR duration <= $2)
		  AND ($3::int IS NULL OR system_id = $3)
		  AND ($4::int[] IS NULL OR tgid = ANY($4))
		  AND ($5::timestamptz IS NULL OR start_time >= $5)
		  AND ($6::timestamptz IS NULL OR start_time < $6)
	`, nilIfZeroFloat(filter.MinDuration), nilIfZeroFloat(filter.MaxDuration),
		filter.SystemID, pqIntArray(filter.Tgids),
		filter.StartTime, filter.EndTime,
	).Scan(&count)
	return count, err
}

// UntranscribedCallKey identifies a call in backfill order. (start_time,
// call_id) is the calls primary key, so keys are unique and totally ordered,
// which makes them usable as a keyset pagination cursor.
type UntranscribedCallKey struct {
	CallID    int64
	StartTime time.Time
}

// Before reports whether k sorts strictly before other in backfill order
// (start_time DESC, call_id DESC), i.e. whether k is returned earlier.
func (k UntranscribedCallKey) Before(other UntranscribedCallKey) bool {
	if !k.StartTime.Equal(other.StartTime) {
		return k.StartTime.After(other.StartTime)
	}
	return k.CallID > other.CallID
}

// ListUntranscribedCalls returns up to limit calls matching the filter that
// have no transcription and are not encrypted, ordered newest first
// (start_time DESC, call_id DESC).
//
// Pagination is keyset-based: when after is non-nil, only calls that sort
// strictly after that key are returned. Paging therefore never revisits a
// call, even one that stays untranscribed (e.g. its transcription failed),
// which an OFFSET-based scan over this shrinking result set cannot guarantee.
func (db *DB) ListUntranscribedCalls(ctx context.Context, filter BackfillFilter, after *UntranscribedCallKey, limit int) ([]UntranscribedCallKey, error) {
	var afterTime *time.Time
	var afterID *int64
	if after != nil {
		afterTime = &after.StartTime
		afterID = &after.CallID
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT call_id, start_time
		FROM calls
		WHERE (has_transcription = false OR has_transcription IS NULL)
		  AND (transcription_status = 'none' OR transcription_status IS NULL)
		  AND (encrypted IS NULL OR encrypted = false)
		  AND audio_file_path IS NOT NULL
		  AND ($1::float8 IS NULL OR duration >= $1)
		  AND ($2::float8 IS NULL OR duration <= $2)
		  AND ($3::int IS NULL OR system_id = $3)
		  AND ($4::int[] IS NULL OR tgid = ANY($4))
		  AND ($5::timestamptz IS NULL OR start_time >= $5)
		  AND ($6::timestamptz IS NULL OR start_time < $6)
		  -- keyset cursor; start_time <= $7 is implied by the row comparison but
		  -- lets the planner prune partitions and range-scan on start_time
		  AND ($7::timestamptz IS NULL OR start_time <= $7)
		  AND ($7::timestamptz IS NULL OR (start_time, call_id) < ($7, $8::bigint))
		ORDER BY start_time DESC, call_id DESC
		LIMIT $9
	`, nilIfZeroFloat(filter.MinDuration), nilIfZeroFloat(filter.MaxDuration),
		filter.SystemID, pqIntArray(filter.Tgids),
		filter.StartTime, filter.EndTime,
		afterTime, afterID,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []UntranscribedCallKey
	for rows.Next() {
		var k UntranscribedCallKey
		if err := rows.Scan(&k.CallID, &k.StartTime); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CallFrequencyAPI represents a frequency entry for API responses.
type CallFrequencyAPI struct {
	Freq       int64    `json:"freq"`
	Time       *string  `json:"time,omitempty"`
	Pos        *float32 `json:"pos,omitempty"`
	Len        *float32 `json:"len,omitempty"`
	ErrorCount *int     `json:"error_count,omitempty"`
	SpikeCount *int     `json:"spike_count,omitempty"`
}

// CallTransmissionAPI represents a transmission entry for API responses.
type CallTransmissionAPI struct {
	Src          int      `json:"src"`
	Tag          string   `json:"tag,omitempty"`
	Time         *string  `json:"time,omitempty"`
	Pos          *float32 `json:"pos,omitempty"`
	Duration     *float32 `json:"duration,omitempty"`
	Emergency    int16    `json:"emergency"`
	SignalSystem string   `json:"signal_system,omitempty"`
}

// CallGroupFilter specifies filters for listing call groups.
type CallGroupFilter struct {
	Sysids    []string
	Tgids     []int
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	Offset    int
}

// CallGroupAPI represents a call group for API responses.
type CallGroupAPI struct {
	ID                  int        `json:"id"`
	SystemID            int        `json:"system_id"`
	SystemName          string     `json:"system_name,omitempty"`
	Sysid               string     `json:"sysid,omitempty"`
	SiteID              *int       `json:"site_id,omitempty"`
	SiteShortName       string     `json:"site_short_name,omitempty"`
	Tgid                int        `json:"tgid"`
	TgAlphaTag          string     `json:"tg_alpha_tag,omitempty"`
	TgDescription       string     `json:"tg_description,omitempty"`
	TgTag               string     `json:"tg_tag,omitempty"`
	TgGroup             string     `json:"tg_group,omitempty"`
	StartTime           time.Time  `json:"start_time"`
	StopTime            *time.Time `json:"stop_time,omitempty"`
	Duration            *float32   `json:"duration,omitempty"`
	CallCount           int        `json:"call_count"`
	PrimaryCallID       *int64     `json:"primary_call_id,omitempty"`
	HasTranscription    bool       `json:"has_transcription"`
	TranscriptionStatus string     `json:"transcription_status,omitempty"`
	TranscriptionText   *string    `json:"transcription_text,omitempty"`
}

// ListCallGroups returns call groups matching the filter, limited to the
// talkgroups p may see: the restriction is part of the WHERE shared by the
// count and the rows (§7.2). A nil p is ErrNoPrincipal.
func (db *DB) ListCallGroups(ctx context.Context, p *auth.Principal, filter CallGroupFilter) ([]CallGroupAPI, int, error) {
	const fromClause = `FROM call_groups cg
		JOIN systems s ON s.system_id = cg.system_id
		LEFT JOIN calls pc ON pc.call_id = cg.primary_call_id AND pc.start_time >= cg.start_time - interval '10 seconds'`
	const filterClause = `
		WHERE ($1::timestamptz IS NULL OR cg.start_time >= $1)
		  AND ($2::timestamptz IS NULL OR cg.start_time < $2)
		  AND ($3::text[] IS NULL OR s.sysid = ANY($3))
		  AND ($4::int[] IS NULL OR cg.tgid = ANY($4))`
	args := []any{filter.StartTime, filter.EndTime, pqStringArray(filter.Sysids), pqIntArray(filter.Tgids)}
	restrict, restrictArgs, err := restrictSQL(p, "cg.system_id", "cg.tgid", len(args)+1)
	if err != nil {
		return nil, 0, err
	}
	whereClause := filterClause + restrict
	args = append(args, restrictArgs...)

	var total int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) "+fromClause+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// call_count counts only the member calls p may see, like the member
	// list of GetCallGroupByID.
	memberRestrict, memberArgs, err := restrictSQL(p, "c.system_id", "c.tgid", len(args)+1)
	if err != nil {
		return nil, 0, err
	}
	dataArgs := slices.Concat(args, memberArgs, []any{filter.Limit, filter.Offset})

	dataQuery := `
		SELECT cg.id, cg.system_id, COALESCE(s.name, ''), COALESCE(s.sysid, ''),
			pc.site_id, COALESCE(pc.site_short_name, ''),
			cg.tgid, COALESCE(cg.tg_alpha_tag, ''), COALESCE(cg.tg_description, ''),
			COALESCE(cg.tg_tag, ''), COALESCE(cg.tg_group, ''),
			cg.start_time, cg.primary_call_id,
			(SELECT count(*) FROM calls c WHERE c.call_group_id = cg.id` + memberRestrict + `),
			COALESCE(cg.transcription_text IS NOT NULL, false),
			COALESCE(cg.transcription_status, 'none'),
			cg.transcription_text
		` + fromClause + whereClause + `
		ORDER BY cg.start_time DESC
		LIMIT ` + placeholder(len(dataArgs)-1) + ` OFFSET ` + placeholder(len(dataArgs))

	rows, err := db.Pool.Query(ctx, dataQuery, dataArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var groups []CallGroupAPI
	for rows.Next() {
		var g CallGroupAPI
		if err := rows.Scan(
			&g.ID, &g.SystemID, &g.SystemName, &g.Sysid,
			&g.SiteID, &g.SiteShortName,
			&g.Tgid, &g.TgAlphaTag, &g.TgDescription, &g.TgTag, &g.TgGroup,
			&g.StartTime, &g.PrimaryCallID, &g.CallCount,
			&g.HasTranscription, &g.TranscriptionStatus, &g.TranscriptionText,
		); err != nil {
			return nil, 0, err
		}
		groups = append(groups, g)
	}
	if groups == nil {
		groups = []CallGroupAPI{}
	}
	return groups, total, rows.Err()
}

// GetCallGroupByID returns a call group with its individual recordings. A
// group whose (system, talkgroup) p may not see is pgx.ErrNoRows, like one
// that doesn't exist (§6.3). Member calls share the group's talkgroup, but
// are filtered by the restriction too, and their patched_tgids are cut down
// to allowed talkgroups. A nil p is ErrNoPrincipal.
func (db *DB) GetCallGroupByID(ctx context.Context, p *auth.Principal, id int) (*CallGroupAPI, []CallAPI, error) {
	groupRestrict, groupArgs, err := restrictSQL(p, "cg.system_id", "cg.tgid", 2)
	if err != nil {
		return nil, nil, err
	}
	// call_count counts only the member calls p may see, like the list below.
	countRestrict, countArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2+len(groupArgs))
	if err != nil {
		return nil, nil, err
	}
	callRestrict, callArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2)
	if err != nil {
		return nil, nil, err
	}
	var g CallGroupAPI
	err = db.Pool.QueryRow(ctx, `
		SELECT cg.id, cg.system_id, COALESCE(s.name, ''), COALESCE(s.sysid, ''),
			pc.site_id, COALESCE(pc.site_short_name, ''),
			cg.tgid, COALESCE(cg.tg_alpha_tag, ''), COALESCE(cg.tg_description, ''),
			COALESCE(cg.tg_tag, ''), COALESCE(cg.tg_group, ''),
			cg.start_time, cg.primary_call_id,
			(SELECT count(*) FROM calls c WHERE c.call_group_id = cg.id`+countRestrict+`),
			COALESCE(cg.transcription_text IS NOT NULL, false),
			COALESCE(cg.transcription_status, 'none'),
			cg.transcription_text
		FROM call_groups cg
		JOIN systems s ON s.system_id = cg.system_id
		LEFT JOIN calls pc ON pc.call_id = cg.primary_call_id AND pc.start_time >= cg.start_time - interval '10 seconds'
		WHERE cg.id = $1`+groupRestrict+`
	`, slices.Concat([]any{id}, groupArgs, countArgs)...).Scan(
		&g.ID, &g.SystemID, &g.SystemName, &g.Sysid,
		&g.SiteID, &g.SiteShortName,
		&g.Tgid, &g.TgAlphaTag, &g.TgDescription, &g.TgTag, &g.TgGroup,
		&g.StartTime, &g.PrimaryCallID, &g.CallCount,
		&g.HasTranscription, &g.TranscriptionStatus, &g.TranscriptionText,
	)
	if err != nil {
		return nil, nil, err
	}

	// Fetch calls in this group
	rows, err := db.Pool.Query(ctx, `
		SELECT c.call_id, c.call_group_id, c.system_id, COALESCE(c.system_name, ''), COALESCE(s.sysid, ''),
			c.site_id, COALESCE(c.site_short_name, ''),
			c.tgid, COALESCE(c.tg_alpha_tag, ''), COALESCE(c.tg_description, ''),
			COALESCE(c.tg_tag, ''), COALESCE(c.tg_group, ''),
			c.start_time, c.stop_time, c.duration,
			c.audio_file_path, COALESCE(c.audio_type, ''), c.audio_file_size,
			COALESCE(c.call_filename, ''),
			c.freq, c.freq_error, c.signal_db, c.noise_db, c.error_count, c.spike_count,
			COALESCE(c.call_state_type, ''), COALESCE(c.mon_state_type, ''),
			COALESCE(c.emergency, false), COALESCE(c.encrypted, false),
			COALESCE(c.analog, false), COALESCE(c.conventional, false),
			COALESCE(c.phase2_tdma, false), c.tdma_slot,
			c.patched_tgids,
			c.src_list, c.freq_list, c.unit_ids,
			COALESCE(c.has_transcription, false), COALESCE(c.transcription_status, 'none'),
			c.transcription_text, c.transcription_word_count,
			c.metadata_json, c.incidentdata
		FROM calls c
		JOIN systems s ON s.system_id = c.system_id
		WHERE c.call_group_id = $1`+callRestrict+`
		ORDER BY c.start_time DESC
	`, append([]any{id}, callArgs...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var calls []CallAPI
	for rows.Next() {
		var c CallAPI
		var audioPath *string
		if err := rows.Scan(
			&c.CallID, &c.CallGroupID, &c.SystemID, &c.SystemName, &c.Sysid,
			&c.SiteID, &c.SiteShortName,
			&c.Tgid, &c.TgAlphaTag, &c.TgDescription, &c.TgTag, &c.TgGroup,
			&c.StartTime, &c.StopTime, &c.Duration,
			&audioPath, &c.AudioType, &c.AudioSize,
			&c.CallFilename,
			&c.Freq, &c.FreqError, &c.SignalDB, &c.NoiseDB, &c.ErrorCount, &c.SpikeCount,
			&c.CallState, &c.MonState,
			&c.Emergency, &c.Encrypted, &c.Analog, &c.Conventional,
			&c.Phase2TDMA, &c.TDMASlot,
			&c.PatchedTgids,
			&c.SrcList, &c.FreqList, &c.UnitIDs,
			&c.HasTranscription, &c.TranscriptionStatus,
			&c.TranscriptionText, &c.TranscriptionWordCt,
			&c.MetadataJSON, &c.IncidentData,
		); err != nil {
			return nil, nil, err
		}
		if audioPath != nil && *audioPath != "" {
			url := fmt.Sprintf("/api/v1/calls/%d/audio", c.CallID)
			c.AudioURL = &url
		}
		c.SrcList = NormalizeSrcFreqTimestamps(c.SrcList)
		c.FreqList = NormalizeSrcFreqTimestamps(c.FreqList)
		c.PatchedTgids = allowedPatchedTgids(p, c.SystemID, c.PatchedTgids)
		calls = append(calls, c)
	}
	if calls == nil {
		calls = []CallAPI{}
	}
	return &g, calls, rows.Err()
}
