package database

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/snarg/tr-engine/internal/database/sqlcdb"
)

// CallExport contains all non-derived call fields for export.
type CallExport struct {
	SystemID      int
	SiteID        *int
	Tgid          int
	StartTime     time.Time
	StopTime      *time.Time
	Duration      *float32
	Freq          *int64
	FreqError     *int
	SignalDB      *float32
	NoiseDB       *float32
	ErrorCount    *int
	SpikeCount    *int
	AudioType     string
	AudioFilePath string
	AudioFileSize *int
	Phase2TDMA    bool
	TDMASlot      *int16
	Analog        bool
	Conventional  bool
	Encrypted     bool
	Emergency     bool
	PatchedTgids  []int32
	SrcList       json.RawMessage
	FreqList      json.RawMessage
	UnitIDs       []int32
	MetadataJSON  json.RawMessage
	IncidentData  json.RawMessage
	InstanceID    string
}

// ExportCalls returns all calls for the given systems and optional time range.
func (db *DB) ExportCalls(ctx context.Context, systemIDs []int, start, end *time.Time) ([]CallExport, error) {
	// Check if incidentdata column exists (migration may not have been applied)
	incidentCol := "incidentdata"
	if !db.columnExists(ctx, "calls", "incidentdata") {
		incidentCol = "NULL::jsonb"
	}
	query := `
		SELECT system_id, site_id, tgid, start_time, stop_time, duration,
			freq, freq_error, signal_db, noise_db, error_count, spike_count,
			COALESCE(audio_type, ''), COALESCE(audio_file_path, ''),
			audio_file_size, COALESCE(phase2_tdma, false), tdma_slot,
			COALESCE(analog, false), COALESCE(conventional, false),
			COALESCE(encrypted, false), COALESCE(emergency, false),
			patched_tgids, src_list, freq_list, unit_ids,
			metadata_json, ` + incidentCol + `, COALESCE(instance_id, '')
		FROM calls
		WHERE ($1::int[] IS NULL OR system_id = ANY($1))
		  AND ($2::timestamptz IS NULL OR start_time >= $2)
		  AND ($3::timestamptz IS NULL OR start_time < $3)
		ORDER BY start_time ASC
	`
	var startArg, endArg any
	if start != nil {
		startArg = *start
	}
	if end != nil {
		endArg = *end
	}

	rows, err := db.Pool.Query(ctx, query, pqIntArray(systemIDs), startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CallExport
	for rows.Next() {
		var c CallExport
		if err := rows.Scan(
			&c.SystemID, &c.SiteID, &c.Tgid, &c.StartTime, &c.StopTime, &c.Duration,
			&c.Freq, &c.FreqError, &c.SignalDB, &c.NoiseDB, &c.ErrorCount, &c.SpikeCount,
			&c.AudioType, &c.AudioFilePath,
			&c.AudioFileSize, &c.Phase2TDMA, &c.TDMASlot,
			&c.Analog, &c.Conventional, &c.Encrypted, &c.Emergency,
			&c.PatchedTgids, &c.SrcList, &c.FreqList, &c.UnitIDs,
			&c.MetadataJSON, &c.IncidentData, &c.InstanceID,
		); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// Conversion helpers for the calls domain.
func ptrIntToInt32(v *int) *int32 {
	if v == nil {
		return nil
	}
	i := int32(*v)
	return &i
}

func int32sToInts(s []int32) []int {
	if s == nil {
		return nil
	}
	r := make([]int, len(s))
	for i, v := range s {
		r[i] = int(v)
	}
	return r
}

func pgtz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func pgtzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

type CallRow struct {
	SystemID      int
	SiteID        *int
	Tgid          int
	TrCallID      string
	CallNum       *int
	StartTime     time.Time
	StopTime      *time.Time
	Duration      *float32
	Freq          *int64
	FreqError     *int
	SignalDB      *float32
	NoiseDB       *float32
	ErrorCount    *int
	SpikeCount    *int
	AudioType     string
	Phase2TDMA    bool
	TDMASlot      *int16
	Analog        bool
	Conventional  bool
	Encrypted     bool
	Emergency     bool
	CallState     *int16
	CallStateType string
	MonState      *int16
	MonStateType  string
	RecState      *int16
	RecStateType  string
	RecNum        *int16
	SrcNum        *int16
	PatchedTgids  []int32
	SrcList       json.RawMessage
	FreqList      json.RawMessage
	UnitIDs       []int32
	SystemName    string
	SiteShortName string
	TgAlphaTag    string
	TgDescription string
	TgTag         string
	TgGroup       string
	IncidentData  json.RawMessage
	InstanceID    string
}

// InsertCall inserts a new call and returns its call_id.
func (db *DB) InsertCall(ctx context.Context, c *CallRow) (int64, error) {
	return db.Q.InsertCall(ctx, sqlcdb.InsertCallParams{
		SystemID:      c.SystemID,
		SiteID:        ptrIntToInt32(c.SiteID),
		Tgid:          c.Tgid,
		TrCallID:      &c.TrCallID,
		CallNum:       ptrIntToInt32(c.CallNum),
		StartTime:     pgtz(c.StartTime),
		StopTime:      pgtzPtr(c.StopTime),
		Duration:      c.Duration,
		Freq:          c.Freq,
		FreqError:     ptrIntToInt32(c.FreqError),
		SignalDb:       c.SignalDB,
		NoiseDb:        c.NoiseDB,
		ErrorCount:    ptrIntToInt32(c.ErrorCount),
		SpikeCount:    ptrIntToInt32(c.SpikeCount),
		AudioType:     &c.AudioType,
		Phase2Tdma:    &c.Phase2TDMA,
		TdmaSlot:      c.TDMASlot,
		Analog:        &c.Analog,
		Conventional:  &c.Conventional,
		Encrypted:     &c.Encrypted,
		Emergency:     &c.Emergency,
		CallState:     c.CallState,
		CallStateType: &c.CallStateType,
		MonState:      c.MonState,
		MonStateType:  &c.MonStateType,
		RecState:      c.RecState,
		RecStateType:  &c.RecStateType,
		RecNum:        c.RecNum,
		SrcNum:        c.SrcNum,
		PatchedTgids:  int32sToInts(c.PatchedTgids),
		SrcList:       c.SrcList,
		FreqList:      c.FreqList,
		UnitIds:       int32sToInts(c.UnitIDs),
		SystemName:    &c.SystemName,
		SiteShortName: &c.SiteShortName,
		TgAlphaTag:    &c.TgAlphaTag,
		TgDescription: &c.TgDescription,
		TgTag:         &c.TgTag,
		TgGroup:       &c.TgGroup,
		Incidentdata:  c.IncidentData,
		InstanceID:    &c.InstanceID,
	})
}

// UpdateCallEnd updates a call with end-of-call data.
func (db *DB) UpdateCallEnd(ctx context.Context, callID int64, startTime time.Time,
	stopTime time.Time, duration float32, freq int64, freqError int,
	signalDB, noiseDB float32, errorCount, spikeCount int,
	recState int16, recStateType string, callState int16, callStateType string,
	callFilename string, retryAttempt int16, processCallTime float32,
) error {
	fe := int32(freqError)
	ec := int32(errorCount)
	sc := int32(spikeCount)
	return db.Q.UpdateCallEnd(ctx, sqlcdb.UpdateCallEndParams{
		CallID:          callID,
		StartTime:       pgtz(startTime),
		StopTime:        pgtz(stopTime),
		Duration:        &duration,
		Freq:            &freq,
		FreqError:       &fe,
		SignalDb:        &signalDB,
		NoiseDb:         &noiseDB,
		ErrorCount:      &ec,
		SpikeCount:      &sc,
		RecState:        &recState,
		RecStateType:    &recStateType,
		CallState:       &callState,
		CallStateType:   &callStateType,
		CallFilename:    &callFilename,
		RetryAttempt:    &retryAttempt,
		ProcessCallTime: &processCallTime,
	})
}

// UpdateCallElapsed updates a call's running duration from calls_active elapsed data.
func (db *DB) UpdateCallElapsed(ctx context.Context, callID int64, startTime time.Time, stopTime *time.Time, duration *float32) error {
	return db.Q.UpdateCallElapsed(ctx, sqlcdb.UpdateCallElapsedParams{
		CallID:    callID,
		StartTime: pgtz(startTime),
		StopTime:  pgtzPtr(stopTime),
		Duration:  duration,
	})
}

// UpdateCallStartFields enriches an audio-created call with fields from call_start.
func (db *DB) UpdateCallStartFields(ctx context.Context, callID int64, startTime time.Time,
	trCallID string, callNum int, instanceID string,
	callState int16, callStateType string,
	monState int16, monStateType string,
	recState int16, recStateType string,
) error {
	cn := int32(callNum)
	return db.Q.UpdateCallStartFields(ctx, sqlcdb.UpdateCallStartFieldsParams{
		CallID:        callID,
		StartTime:     pgtz(startTime),
		TrCallID:      &trCallID,
		CallNum:       &cn,
		InstanceID:    &instanceID,
		CallState:     &callState,
		CallStateType: &callStateType,
		MonState:      &monState,
		MonStateType:  &monStateType,
		RecState:      &recState,
		RecStateType:  &recStateType,
	})
}

// UpdateCallAudio updates a call with audio file path and size.
func (db *DB) UpdateCallAudio(ctx context.Context, callID int64, startTime time.Time, audioPath string, audioSize int) error {
	as := int32(audioSize)
	return db.Q.UpdateCallAudio(ctx, sqlcdb.UpdateCallAudioParams{
		CallID:        callID,
		StartTime:     pgtz(startTime),
		AudioFilePath: &audioPath,
		AudioFileSize: &as,
	})
}

// UpdateCallFilename sets the call_filename field (TR's original audio file path).
func (db *DB) UpdateCallFilename(ctx context.Context, callID int64, startTime time.Time, callFilename string) error {
	return db.Q.UpdateCallFilename(ctx, sqlcdb.UpdateCallFilenameParams{
		CallID:       callID,
		StartTime:    pgtz(startTime),
		CallFilename: &callFilename,
	})
}

// UpsertCallGroup creates or finds a call group and returns its id.
func (db *DB) UpsertCallGroup(ctx context.Context, systemID, tgid int, startTime time.Time,
	tgAlphaTag, tgDescription, tgTag, tgGroup string,
) (int, error) {
	return db.Q.UpsertCallGroup(ctx, sqlcdb.UpsertCallGroupParams{
		SystemID:      systemID,
		Tgid:          tgid,
		StartTime:     pgtz(startTime),
		TgAlphaTag:    &tgAlphaTag,
		TgDescription: &tgDescription,
		TgTag:         &tgTag,
		TgGroup:       &tgGroup,
	})
}

// SetCallGroupID sets the call_group_id on a call.
func (db *DB) SetCallGroupID(ctx context.Context, callID int64, startTime time.Time, callGroupID int) error {
	cg := int32(callGroupID)
	return db.Q.SetCallGroupID(ctx, sqlcdb.SetCallGroupIDParams{
		CallID:      callID,
		StartTime:   pgtz(startTime),
		CallGroupID: &cg,
	})
}

// SetCallGroupPrimary sets the primary_call_id on a call group.
func (db *DB) SetCallGroupPrimary(ctx context.Context, callGroupID int, callID int64) error {
	return db.Q.SetCallGroupPrimary(ctx, sqlcdb.SetCallGroupPrimaryParams{
		ID:            callGroupID,
		PrimaryCallID: &callID,
	})
}

// UpdateCallSrcFreq updates a call with srcList, freqList, and unit_ids JSONB columns.
func (db *DB) UpdateCallSrcFreq(ctx context.Context, callID int64, startTime time.Time,
	srcList json.RawMessage, freqList json.RawMessage, unitIDs []int32) error {
	return db.Q.UpdateCallSrcFreq(ctx, sqlcdb.UpdateCallSrcFreqParams{
		CallID:    callID,
		StartTime: pgtz(startTime),
		SrcList:   srcList,
		FreqList:  freqList,
		UnitIds:   int32sToInts(unitIDs),
	})
}

// UpdateCallSrcFreqIfNull conditionally updates srcList/freqList/unit_ids only when
// src_list IS NULL (no real data from trunk-recorder). Returns rows affected:
// 1 = synthesized data written, 0 = real data already present (no-op).
func (db *DB) UpdateCallSrcFreqIfNull(ctx context.Context, callID int64, startTime time.Time,
	srcList json.RawMessage, freqList json.RawMessage, unitIDs []int32) (int64, error) {
	return db.Q.UpdateCallSrcFreqIfNull(ctx, sqlcdb.UpdateCallSrcFreqIfNullParams{
		CallID:    callID,
		StartTime: pgtz(startTime),
		SrcList:   srcList,
		FreqList:  freqList,
		UnitIds:   int32sToInts(unitIDs),
	})
}

type CallFrequencyRow struct {
	CallID        int64
	CallStartTime time.Time
	Freq          int64
	Time          *time.Time
	Pos           *float32
	Len           *float32
	ErrorCount    *int
	SpikeCount    *int
}

// InsertCallFrequencies batch-inserts call frequency records.
func (db *DB) InsertCallFrequencies(ctx context.Context, rows []CallFrequencyRow) (int64, error) {
	params := make([]sqlcdb.InsertCallFrequenciesParams, len(rows))
	for i, r := range rows {
		params[i] = sqlcdb.InsertCallFrequenciesParams{
			CallID:        r.CallID,
			CallStartTime: pgtz(r.CallStartTime),
			Freq:          r.Freq,
			Time:          pgtzPtr(r.Time),
			Pos:           r.Pos,
			Len:           r.Len,
			ErrorCount:    ptrIntToInt32(r.ErrorCount),
			SpikeCount:    ptrIntToInt32(r.SpikeCount),
		}
	}
	return db.Q.InsertCallFrequencies(ctx, params)
}

type CallTransmissionRow struct {
	CallID        int64
	CallStartTime time.Time
	Src           int
	Time          *time.Time
	Pos           *float32
	Duration      *float32
	Emergency     int16
	SignalSystem  string
	Tag           string
}

// InsertCallTransmissions batch-inserts call transmission records.
func (db *DB) InsertCallTransmissions(ctx context.Context, rows []CallTransmissionRow) (int64, error) {
	params := make([]sqlcdb.InsertCallTransmissionsParams, len(rows))
	for i, r := range rows {
		params[i] = sqlcdb.InsertCallTransmissionsParams{
			CallID:        r.CallID,
			CallStartTime: pgtz(r.CallStartTime),
			Src:           r.Src,
			Time:          pgtzPtr(r.Time),
			Pos:           r.Pos,
			Duration:      r.Duration,
			Emergency:     &r.Emergency,
			SignalSystem:  &r.SignalSystem,
			Tag:           &r.Tag,
		}
	}
	return db.Q.InsertCallTransmissions(ctx, params)
}

// InsertActiveCallCheckpoint stores a snapshot of active calls for crash recovery.
func (db *DB) InsertActiveCallCheckpoint(ctx context.Context, instanceID string, activeCalls []byte, callCount int) error {
	cc := int32(callCount)
	return db.Q.InsertActiveCallCheckpoint(ctx, sqlcdb.InsertActiveCallCheckpointParams{
		InstanceID:  &instanceID,
		ActiveCalls: activeCalls,
		CallCount:   &cc,
	})
}

// PurgeStaleCalls deletes RECORDING calls older than maxAge that never received
// audio or a call_end. These are orphaned call_start records. Returns the number deleted.
func (db *DB) PurgeStaleCalls(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	return db.Q.PurgeStaleCalls(ctx, pgtz(cutoff))
}

// PurgeOrphanCallGroups deletes call_groups with no remaining calls. Returns count deleted.
func (db *DB) PurgeOrphanCallGroups(ctx context.Context) (int64, error) {
	return db.Q.PurgeOrphanCallGroups(ctx)
}

// FindCallByTrCallID finds a call by its trunk-recorder call ID.
// startTimeHint enables partition pruning (±30s window); pass nil to scan all partitions.
func (db *DB) FindCallByTrCallID(ctx context.Context, trCallID string, startTimeHint *time.Time) (int64, time.Time, error) {
	row, err := db.Q.FindCallByTrCallID(ctx, sqlcdb.FindCallByTrCallIDParams{
		TrCallID: &trCallID,
		Column2:  pgtzPtr(startTimeHint),
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return row.CallID, row.StartTime.Time, nil
}

// FindCallForAudio finds a call matching the audio metadata.
// Uses fuzzy start_time matching (±10s) to handle trunk-recorder shifting
// start_time between call_start/call_end and audio messages. Analog systems
// can have up to ~6s delta between recording start and MQTT call_start.
func (db *DB) FindCallForAudio(ctx context.Context, systemID, tgid int, startTime time.Time) (int64, time.Time, error) {
	row, err := db.Q.FindCallForAudio(ctx, sqlcdb.FindCallForAudioParams{
		SystemID: systemID,
		Tgid:     tgid,
		Column3:  pgtz(startTime),
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return row.CallID, row.StartTime.Time, nil
}

// FindCallFuzzy checks if a call exists matching (system_id, tgid, start_time ± 10s).
// Returns call_id and start_time if found, or 0/zero-time/ErrNoRows if not found.
func (db *DB) FindCallFuzzy(ctx context.Context, systemID, tgid int, startTime time.Time) (int64, time.Time, error) {
	return db.FindCallForAudio(ctx, systemID, tgid, startTime)
}

// DvcfCallMatch holds the call fields needed to enqueue IMBE transcription
// from a standalone DVCF message (which lacks instance_id for identity resolution).
type DvcfCallMatch struct {
	CallID        int64
	StartTime     time.Time
	SystemID      int
	Tgid          int
	AudioFilePath string
	CallFilename  string
	Duration      float32
	SrcList       json.RawMessage
	TgAlphaTag    string
	TgDescription string
	TgTag         string
	TgGroup       string
}

// FindCallBySystemName finds a call by system_name + tgid + start_time (±10s).
// Used by the DVCF handler which has short_name but no instance_id.
func (db *DB) FindCallBySystemName(ctx context.Context, systemName string, tgid int, startTime time.Time) (*DvcfCallMatch, error) {
	row, err := db.Q.FindCallBySystemName(ctx, sqlcdb.FindCallBySystemNameParams{
		SystemName: systemName,
		Tgid:       tgid,
		Column3:    pgtz(startTime),
	})
	if err != nil {
		return nil, err
	}
	return &DvcfCallMatch{
		CallID:        row.CallID,
		StartTime:     row.StartTime.Time,
		SystemID:      row.SystemID,
		Tgid:          row.Tgid,
		AudioFilePath: row.AudioFilePath,
		CallFilename:  row.CallFilename,
		Duration:      row.Duration,
		SrcList:       row.SrcList,
		TgAlphaTag:    row.TgAlphaTag,
		TgDescription: row.TgDescription,
		TgTag:         row.TgTag,
		TgGroup:       row.TgGroup,
	}, nil
}

// GetCallAudioPath returns the audio file path and call_filename for a call.
// audio_file_path is the tr-engine managed path; call_filename is TR's original absolute path.
func (db *DB) GetCallAudioPath(ctx context.Context, callID int64) (audioPath string, callFilename string, err error) {
	row, err := db.Q.GetCallAudioPath(ctx, callID)
	if err != nil {
		return "", "", err
	}
	return row.AudioFilePath, row.CallFilename, nil
}

// GetCallFrequencies returns frequency entries for a call by reading the freq_list JSONB column.
func (db *DB) GetCallFrequencies(ctx context.Context, callID int64) ([]CallFrequencyAPI, error) {
	raw, err := db.Q.GetCallFreqList(ctx, callID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return []CallFrequencyAPI{}, nil
	}
	normalized := NormalizeSrcFreqTimestamps(raw)
	var freqs []CallFrequencyAPI
	if err := json.Unmarshal(normalized, &freqs); err != nil {
		return nil, err
	}
	if freqs == nil {
		freqs = []CallFrequencyAPI{}
	}
	return freqs, nil
}

// GetCallTransmissions returns transmission entries for a call by reading the src_list JSONB column.
func (db *DB) GetCallTransmissions(ctx context.Context, callID int64) ([]CallTransmissionAPI, error) {
	raw, err := db.Q.GetCallSrcList(ctx, callID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return []CallTransmissionAPI{}, nil
	}
	normalized := NormalizeSrcFreqTimestamps(raw)
	var txs []CallTransmissionAPI
	if err := json.Unmarshal(normalized, &txs); err != nil {
		return nil, err
	}
	if txs == nil {
		txs = []CallTransmissionAPI{}
	}
	return txs, nil
}
