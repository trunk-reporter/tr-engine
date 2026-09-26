package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	if err := validateUploadMeta(meta, time.Now()); err != nil {
		return nil, fmt.Errorf("%w: %w", api.ErrInvalidUpload, err)
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
		// Auto-create missing partition and retry once. A month outside
		// the window ensurePartitionsFor accepts is the uploader's fault.
		if perr := p.ensurePartitionsFor(startTime); errors.Is(perr, errPartitionOutOfRange) {
			return nil, fmt.Errorf("%w: start time %s: %w", api.ErrInvalidUpload,
				startTime.UTC().Format(time.RFC3339), perr)
		}
		callID, callStartTime, effectiveTgTag, err = p.createCallFromAudio(ctx, identity, meta, startTime)
	}
	if err != nil {
		return nil, fmt.Errorf("create call from upload: %w", err)
	}

	// Save audio file (best-effort — still return success for the call record)
	var audioPath string
	if len(audioData) > 0 {
		ext := uploadAudioExt(meta.AudioType, audioFilename)
		audioKey := p.newUploadAudioKey(ctx, identity.SystemID, startTime, callID, ext)
		contentType := audioContentType(ext)

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

// Limits on what an upload may claim (see validateUploadMeta).
const (
	// maxUploadShortNameLen caps the system short name an upload may
	// create a system under.
	maxUploadShortNameLen = 128
	// maxUploadClockSkew is how far in the future an upload's start time
	// may be. Older start times are bounded by the months
	// ensurePartitionsFor accepts.
	maxUploadClockSkew = 10 * time.Minute
)

// validateUploadMeta refuses uploaded metadata the engine must not act on:
// a talkgroup or start time that isn't positive, a start time more than
// maxUploadClockSkew ahead of now, and a system short name that could be
// read as a path or holds control characters. The caller wraps the error in
// api.ErrInvalidUpload (400).
func validateUploadMeta(meta *AudioMetadata, now time.Time) error {
	if meta.Talkgroup <= 0 {
		return fmt.Errorf("talkgroup must be a positive integer, got %d", meta.Talkgroup)
	}
	if meta.StartTime <= 0 {
		return errors.New("missing or invalid start time (dateTime / start_time, unix seconds)")
	}
	if start := time.Unix(meta.StartTime, 0); start.After(now.Add(maxUploadClockSkew)) {
		return fmt.Errorf("start time %s is in the future", start.UTC().Format(time.RFC3339))
	}
	if err := validateUploadShortName(meta.ShortName); err != nil {
		return err
	}
	return nil
}

// validateUploadShortName refuses a system short name holding a path
// separator, "..", or a control character, or longer than
// maxUploadShortNameLen bytes.
func validateUploadShortName(name string) error {
	if name == "" {
		return errors.New("missing system short name")
	}
	if len(name) > maxUploadShortNameLen {
		return fmt.Errorf("system short name is longer than %d bytes", maxUploadShortNameLen)
	}
	if name == "." || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid system short name %q: no '/', '\\', '..' or control characters", name)
	}
	return nil
}

// uploadAudioExt picks the stored file's extension from the declared audio
// type (an extension or a MIME type), then the uploaded file name's
// extension: m4a, mp3, wav or ogg. Nothing declared means m4a (what the
// upload plugins send); anything else is stored as bin.
func uploadAudioExt(audioType, filename string) string {
	known := map[string]string{
		"m4a": "m4a", "mp4": "m4a", "aac": "m4a",
		"audio/mp4": "m4a", "audio/m4a": "m4a", "audio/x-m4a": "m4a", "audio/aac": "m4a",
		"mp3": "mp3", "audio/mpeg": "mp3", "audio/mp3": "mp3",
		"wav": "wav", "audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/vnd.wave": "wav",
		"ogg": "ogg", "oga": "ogg", "opus": "ogg", "audio/ogg": "ogg", "audio/opus": "ogg",
	}
	fileExt := ""
	if i := strings.LastIndex(filename, "."); i >= 0 {
		fileExt = filename[i+1:]
	}
	declared := false
	for _, c := range []string{audioType, fileExt} {
		c = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c), "."))
		if c == "" {
			continue
		}
		declared = true
		if ext, ok := known[c]; ok {
			return ext
		}
	}
	if !declared {
		return "m4a"
	}
	return "bin"
}

// uploadAudioKey is the storage key of an uploaded call's audio:
// upload/<system_id>/<YYYY-MM-DD>/<call_id>[-<suffix>].<ext>. Every part is
// chosen by the engine, never by the uploader, so an upload can't name, and
// so can't replace, another call's file. (MQTT audio is stored under
// <short_name>/<YYYY-MM-DD>/, whose second segment is never all digits.)
func uploadAudioKey(systemID int, startTime time.Time, callID int64, suffix, ext string) string {
	name := strconv.FormatInt(callID, 10)
	if suffix != "" {
		name += "-" + suffix
	}
	return fmt.Sprintf("upload/%d/%s/%s.%s", systemID, startTime.UTC().Format("2006-01-02"), name, ext)
}

// newUploadAudioKey returns uploadAudioKey for the call, with a random
// suffix if a file is already stored under that key (left over from an
// earlier database, since call IDs are unique): an upload never overwrites
// a stored file.
func (p *Pipeline) newUploadAudioKey(ctx context.Context, systemID int, startTime time.Time, callID int64, ext string) string {
	key := uploadAudioKey(systemID, startTime, callID, "", ext)
	if p.store == nil || !p.store.Exists(ctx, key) {
		return key
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	alt := uploadAudioKey(systemID, startTime, callID, hex.EncodeToString(b[:]), ext)
	p.log.Warn().Str("existing", key).Str("key", alt).Int64("call_id", callID).
		Msg("an audio file is already stored under this upload's key; saving the upload under another name")
	return alt
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
