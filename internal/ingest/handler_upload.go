package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/snarg/tr-engine/internal/api"
)

// UploadResult holds the outcome of a successfully processed uploaded call.
type UploadResult struct {
	CallID        int64
	SystemID      int
	Tgid          int
	StartTime     time.Time
	AudioFilePath string
}

// ProcessUpload implements api.CallUploader. It bridges the API layer to the
// pipeline by parsing form fields into AudioMetadata based on format, then
// delegating to ProcessUploadedCall.
func (p *Pipeline) ProcessUpload(ctx context.Context, instanceID string, format string, fields map[string]string, audioData []byte, audioFilename string) (*api.UploadCallResult, error) {
	var meta *AudioMetadata
	var err error

	switch format {
	case "rdio-scanner":
		meta, err = ParseRdioScannerFields(fields)
	case "openmhz":
		meta, err = ParseOpenMHzFields(fields)
	default:
		return nil, fmt.Errorf("%w: unsupported upload format: %s", api.ErrInvalidUpload, format)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s fields: %w", api.ErrInvalidUpload, format, err)
	}

	// OpenMHz doesn't always include short_name — use instanceID as fallback
	if meta.ShortName == "" {
		meta.ShortName = instanceID
	}

	result, err := p.ProcessUploadedCall(ctx, instanceID, meta, audioData, audioFilename)
	if err != nil {
		return nil, err
	}

	return &api.UploadCallResult{
		CallID:        result.CallID,
		SystemID:      result.SystemID,
		Tgid:          result.Tgid,
		StartTime:     result.StartTime,
		AudioFilePath: result.AudioFilePath,
	}, nil
}

// ProcessUploadedCall ingests a call submitted via HTTP upload (rdio-scanner or
// OpenMHz format). It mirrors processWatchedFile: identity resolution, dedup,
// call creation, audio save, src/freq processing, unit upserts, SSE publish,
// and transcription enqueue.
func (p *Pipeline) ProcessUploadedCall(ctx context.Context, instanceID string, meta *AudioMetadata, audioData []byte, audioFilename string) (*UploadResult, error) {
	startTime := time.Unix(meta.StartTime, 0)

	// Resolve identity (auto-creates system/site if needed)
	identity, err := p.identity.Resolve(ctx, instanceID, meta.ShortName)
	if err != nil {
		return nil, fmt.Errorf("resolve identity: %w", err)
	}

	// Dedup check — reject if this call already exists
	if existingID, _, findErr := p.db.FindCallForAudio(ctx, identity.SystemID, meta.Talkgroup, startTime); findErr == nil {
		return nil, fmt.Errorf("duplicate call: call_id=%d already exists for system=%d tgid=%d start_time=%d",
			existingID, identity.SystemID, meta.Talkgroup, meta.StartTime)
	}

	// Create call from audio metadata
	callID, callStartTime, effectiveTgTag, err := p.createCallFromAudio(ctx, identity, meta, startTime)
	if err != nil && strings.Contains(err.Error(), "no partition") {
		// Auto-create missing partition and retry once
		p.ensurePartitionsFor(startTime)
		callID, callStartTime, effectiveTgTag, err = p.createCallFromAudio(ctx, identity, meta, startTime)
	}
	if err != nil {
		return nil, fmt.Errorf("create call from upload: %w", err)
	}

	// Save audio file (best-effort — still return success for the call record)
	var audioPath string
	if len(audioData) > 0 {
		audioType := meta.AudioType
		if audioType == "" {
			if idx := strings.LastIndex(audioFilename, "."); idx >= 0 {
				audioType = audioFilename[idx+1:]
			}
		}
		if audioType == "" {
			audioType = "m4a"
		}

		filename := buildAudioFilename(audioFilename, audioType, startTime)
		audioKey := buildAudioRelPath(meta.ShortName, startTime, filename)
		contentType := audioContentType(audioType)

		if err := p.saveAudio(ctx, audioKey, audioData, contentType); err != nil {
			p.log.Error().Err(err).Int64("call_id", callID).Msg("failed to save uploaded audio file")
		} else {
			audioPath = audioKey
			if updateErr := p.db.UpdateCallAudio(ctx, callID, callStartTime, audioPath, len(audioData)); updateErr != nil {
				p.log.Warn().Err(updateErr).Int64("call_id", callID).Msg("failed to update call audio path")
			}
		}
	}

	// Process srcList/freqList
	p.processSrcFreqData(ctx, callID, callStartTime, meta)

	// Upsert units from srcList
	for _, s := range meta.SrcList {
		if s.Src > 0 {
			_, _ = p.db.UpsertUnit(ctx, identity.SystemID, s.Src,
				s.Tag, s.TagOTA, "upload", startTime, meta.Talkgroup,
			)
		}
	}

	// Publish call_end SSE event (uploaded calls are always complete)
	stopTime := startTime
	if meta.StopTime > 0 {
		stopTime = time.Unix(meta.StopTime, 0)
	}
	p.PublishEvent(EventData{
		Type:      "call_end",
		SystemID:  identity.SystemID,
		SiteID:    identity.SiteID,
		Tgid:      meta.Talkgroup,
		Emergency: meta.Emergency != 0,
		Payload: map[string]any{
			"call_id":        callID,
			"system_id":      identity.SystemID,
			"tgid":           meta.Talkgroup,
			"tg_alpha_tag":   effectiveTgTag,
			"freq":           int64(meta.Freq),
			"start_time":     startTime,
			"stop_time":      stopTime,
			"duration":       float64(meta.CallLength),
			"emergency":      meta.Emergency != 0,
			"encrypted":      meta.Encrypted != 0,
			"audio_file_path": audioPath,
			"source":         "upload",
		},
	})

	// Enqueue for transcription if not encrypted
	if meta.Encrypted == 0 {
		if meta.Transcript != "" {
			p.insertSourceTranscription(callID, callStartTime, identity.SystemID, meta.Talkgroup, meta)
		} else {
			p.enqueueTranscription(callID, callStartTime, identity.SystemID, audioPath, meta)
		}
	}

	p.log.Info().
		Int64("call_id", callID).
		Int("tgid", meta.Talkgroup).
		Str("sys_name", meta.ShortName).
		Str("instance_id", instanceID).
		Str("audio_path", audioPath).
		Msg("call created from HTTP upload")

	return &UploadResult{
		CallID:        callID,
		SystemID:      identity.SystemID,
		Tgid:          meta.Talkgroup,
		StartTime:     startTime,
		AudioFilePath: audioPath,
	}, nil
}

// ParseRdioScannerFields parses rdio-scanner trunk-recorder plugin form fields
// into an AudioMetadata struct.
//
// Expected fields:
//   - talkgroup (int, required)
//   - frequency/freq (int, Hz)
//   - dateTime (int64, unix epoch seconds)
//   - systemLabel/system (string, system short name)
//   - sources (JSON array of source objects)
//   - frequencies (JSON array of frequency objects)
//   - talkgroupLabel (string, alpha tag)
//   - talkgroupName (string, description)
//   - talkgroupTag (string, tag)
//   - talkgroupGroup (string, group)
//   - audioType (string, e.g. "m4a")
//   - emergency (bool/int)
//   - encrypted (bool/int)
func ParseRdioScannerFields(fields map[string]string) (*AudioMetadata, error) {
	meta := &AudioMetadata{}

	// talkgroup (required)
	tgStr := firstNonEmpty(fields, "talkgroup")
	if tgStr == "" {
		return nil, fmt.Errorf("missing required field: talkgroup")
	}
	tg, err := strconv.Atoi(tgStr)
	if err != nil {
		return nil, fmt.Errorf("invalid talkgroup %q: %w", tgStr, err)
	}
	meta.Talkgroup = tg

	// frequency
	if freqStr := firstNonEmpty(fields, "frequency", "freq"); freqStr != "" {
		freq, err := strconv.ParseFloat(freqStr, 64)
		if err == nil {
			meta.Freq = freq
		}
	}

	// dateTime → StartTime
	if dtStr := firstNonEmpty(fields, "dateTime"); dtStr != "" {
		dt, err := strconv.ParseInt(dtStr, 10, 64)
		if err == nil {
			meta.StartTime = dt
		}
	}

	// stopTime
	if stStr := firstNonEmpty(fields, "stopTime", "stop_time"); stStr != "" {
		st, err := strconv.ParseInt(stStr, 10, 64)
		if err == nil {
			meta.StopTime = st
		}
	}

	// systemLabel → ShortName
	meta.ShortName = firstNonEmpty(fields, "systemLabel", "system", "short_name")

	// Talkgroup metadata
	meta.TalkgroupTag = firstNonEmpty(fields, "talkgroupLabel", "talkgroupAlphaTag")
	meta.TalkgroupDesc = firstNonEmpty(fields, "talkgroupName", "talkgroupDescription")
	meta.TalkgroupGroupTag = firstNonEmpty(fields, "talkgroupTag")
	meta.TalkgroupGroup = firstNonEmpty(fields, "talkgroupGroup")

	// audioType
	meta.AudioType = firstNonEmpty(fields, "audioType", "audio_type")

	// emergency
	if emStr := firstNonEmpty(fields, "emergency"); emStr != "" {
		meta.Emergency = parseBoolInt(emStr)
	}

	// encrypted
	if encStr := firstNonEmpty(fields, "encrypted"); encStr != "" {
		meta.Encrypted = parseBoolInt(encStr)
	}

	// callLength / duration
	if clStr := firstNonEmpty(fields, "callLength", "call_length"); clStr != "" {
		cl, err := strconv.Atoi(clStr)
		if err == nil {
			meta.CallLength = cl
		}
	}

	// sources JSON → SrcList
	if srcJSON := firstNonEmpty(fields, "sources", "source_list"); srcJSON != "" {
		meta.SrcList, _ = parseRdioSources(srcJSON)
	}

	// frequencies JSON → FreqList
	if freqJSON := firstNonEmpty(fields, "frequencies", "freq_list"); freqJSON != "" {
		meta.FreqList, _ = parseRdioFrequencies(freqJSON)
	}

	// Compute callLength from stop-start if not provided
	if meta.CallLength == 0 && meta.StopTime > 0 && meta.StartTime > 0 {
		meta.CallLength = int(meta.StopTime - meta.StartTime)
	}

	return meta, nil
}

// ParseOpenMHzFields parses OpenMHz trunk-recorder plugin form fields
// into an AudioMetadata struct.
//
// Expected fields:
//   - talkgroup_num (int, required)
//   - freq (int, Hz)
//   - start_time (int64, unix epoch seconds)
//   - stop_time (int64, unix epoch seconds)
//   - source_list (JSON array of source objects)
//   - freq_list (JSON array of frequency objects)
//   - emergency (bool/int)
//   - error_count (int)
//   - call_length (int, seconds)
//   - short_name (string, system short name — often empty)
func ParseOpenMHzFields(fields map[string]string) (*AudioMetadata, error) {
	meta := &AudioMetadata{}

	// talkgroup_num (required)
	tgStr := firstNonEmpty(fields, "talkgroup_num")
	if tgStr == "" {
		return nil, fmt.Errorf("missing required field: talkgroup_num")
	}
	tg, err := strconv.Atoi(tgStr)
	if err != nil {
		return nil, fmt.Errorf("invalid talkgroup_num %q: %w", tgStr, err)
	}
	meta.Talkgroup = tg

	// freq
	if freqStr := firstNonEmpty(fields, "freq"); freqStr != "" {
		freq, err := strconv.ParseFloat(freqStr, 64)
		if err == nil {
			meta.Freq = freq
		}
	}

	// start_time
	if stStr := firstNonEmpty(fields, "start_time"); stStr != "" {
		st, err := strconv.ParseInt(stStr, 10, 64)
		if err == nil {
			meta.StartTime = st
		}
	}

	// stop_time
	if stStr := firstNonEmpty(fields, "stop_time"); stStr != "" {
		st, err := strconv.ParseInt(stStr, 10, 64)
		if err == nil {
			meta.StopTime = st
		}
	}

	// short_name (often empty for OpenMHz — API handler fills it in)
	meta.ShortName = firstNonEmpty(fields, "short_name")

	// emergency
	if emStr := firstNonEmpty(fields, "emergency"); emStr != "" {
		meta.Emergency = parseBoolInt(emStr)
	}

	// encrypted
	if encStr := firstNonEmpty(fields, "encrypted"); encStr != "" {
		meta.Encrypted = parseBoolInt(encStr)
	}

	// error_count → FreqError (approximate — OpenMHz sends a single error_count)
	if ecStr := firstNonEmpty(fields, "error_count"); ecStr != "" {
		ec, err := strconv.Atoi(ecStr)
		if err == nil {
			meta.FreqError = ec
		}
	}

	// call_length
	if clStr := firstNonEmpty(fields, "call_length"); clStr != "" {
		cl, err := strconv.Atoi(clStr)
		if err == nil {
			meta.CallLength = cl
		}
	}

	// source_list JSON → SrcList
	if srcJSON := firstNonEmpty(fields, "source_list"); srcJSON != "" {
		meta.SrcList, _ = parseOpenMHzSources(srcJSON)
	}

	// freq_list JSON → FreqList
	if freqJSON := firstNonEmpty(fields, "freq_list"); freqJSON != "" {
		meta.FreqList, _ = parseOpenMHzFrequencies(freqJSON)
	}

	// Compute callLength from stop-start if not provided
	if meta.CallLength == 0 && meta.StopTime > 0 && meta.StartTime > 0 {
		meta.CallLength = int(meta.StopTime - meta.StartTime)
	}

	return meta, nil
}

// firstNonEmpty returns the first non-empty value from the fields map for any
// of the given keys.
func firstNonEmpty(fields map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := fields[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// parseBoolInt converts "true"/"1" to 1, anything else to 0.
func parseBoolInt(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "true" || s == "1" {
		return 1
	}
	return 0
}

// parseRdioSources parses the rdio-scanner "sources" JSON field.
// Rdio-scanner sends: [{"src": 12345, "time": 1700000000, "pos": 0.0, "emergency": 0, "signal_system": "", "tag": ""}]
func parseRdioSources(raw string) ([]SrcItem, error) {
	var items []SrcItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("parse rdio sources: %w", err)
	}
	return items, nil
}

// parseRdioFrequencies parses the rdio-scanner "frequencies" JSON field.
// Rdio-scanner sends: [{"freq": 851000000, "time": 1700000000, "pos": 0.0, "len": 1.5, "error_count": 0, "spike_count": 0}]
func parseRdioFrequencies(raw string) ([]FreqItem, error) {
	var items []FreqItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("parse rdio frequencies: %w", err)
	}
	return items, nil
}

// parseOpenMHzSources parses the OpenMHz "source_list" JSON field.
// OpenMHz sends: [{"src": 12345, "time": 1700000000, "pos": 0.0, "emergency": 0, "signal_system": "", "tag": ""}]
func parseOpenMHzSources(raw string) ([]SrcItem, error) {
	var items []SrcItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("parse openmhz sources: %w", err)
	}
	return items, nil
}

// parseOpenMHzFrequencies parses the OpenMHz "freq_list" JSON field.
// OpenMHz sends: [{"freq": 851000000, "time": 1700000000, "pos": 0.0, "len": 1.5, "error_count": 0, "spike_count": 0}]
func parseOpenMHzFrequencies(raw string) ([]FreqItem, error) {
	var items []FreqItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("parse openmhz frequencies: %w", err)
	}
	return items, nil
}
