package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/storage"
)

func (p *Pipeline) handleAudio(payload []byte) error {
	var msg AudioMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	meta := &msg.Call.Metadata
	startTime := time.Unix(meta.StartTime, 0)

	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	identity, err := p.identity.Resolve(ctx, msg.InstanceID, meta.ShortName)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	// Find the matching call, or create one from audio metadata
	callID, callStartTime, err := p.db.FindCallForAudio(ctx, identity.SystemID, meta.Talkgroup, startTime)
	if err != nil {
		// No call record yet — create one from audio metadata.
		// call_end will find this record later via FindCallForAudio and update it.
		callID, callStartTime, _, err = p.createCallFromAudio(ctx, identity, meta, startTime)
		if err != nil {
			p.log.Error().Err(err).
				Int("tgid", meta.Talkgroup).
				Str("sys_name", meta.ShortName).
				Msg("failed to create call from audio")
			return nil
		}
	}

	// Decode and save audio file (skip when TR_AUDIO_DIR is set — files served from TR's filesystem)
	var audioPath string
	var audioSize int

	if p.trAudioDir == "" {
		audioData := msg.Call.AudioM4ABase64
		inferredType := "m4a"
		if audioData == "" {
			audioData = msg.Call.AudioWavBase64
			inferredType = "wav"
		}

		audioType := meta.AudioType
		if audioType == "" {
			audioType = inferredType
		}

		if audioData != "" {
			decoded, decErr := base64.StdEncoding.DecodeString(audioData)
			if decErr != nil {
				p.log.Warn().Err(decErr).Msg("failed to decode audio base64")
			} else {
				audioSize = len(decoded)
				filename := buildAudioFilename(meta.Filename, audioType, startTime)
				audioKey := buildAudioRelPath(meta.ShortName, startTime, filename)
				contentType := audioContentType(audioType)

				if err := p.saveAudio(ctx, audioKey, decoded, contentType); err != nil {
					p.log.Error().Err(err).Msg("failed to save audio file")
				} else {
					audioPath = audioKey
				}
			}
		}

		// Decode and save .dvcf file alongside audio (for IMBE ASR)
		if msg.Call.AudioDvcfBase64 != "" && audioPath != "" {
			dvcfDecoded, dvcfErr := base64.StdEncoding.DecodeString(msg.Call.AudioDvcfBase64)
			if dvcfErr != nil {
				p.log.Warn().Err(dvcfErr).Msg("failed to decode dvcf base64")
			} else {
				dvcfExt := filepath.Ext(audioPath)
				tapKey := strings.TrimSuffix(audioPath, dvcfExt) + ".dvcf"
				if err := p.saveAudio(ctx, tapKey, dvcfDecoded, "application/octet-stream"); err != nil {
					p.log.Error().Err(err).Str("tap_key", tapKey).Msg("failed to save tap file")
				} else {
					p.log.Debug().Str("tap_key", tapKey).Int("tap_size", len(dvcfDecoded)).Msg("tap file saved")
				}
			}
		}

		if callID > 0 && audioPath != "" {
			if err := p.db.UpdateCallAudio(ctx, callID, callStartTime, audioPath, audioSize); err != nil {
				p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to update call audio")
			}
		}
	}

	// Build srcList/freqList JSON and update call
	if callID > 0 {
		p.processSrcFreqData(ctx, callID, callStartTime, meta)
	}

	// Enqueue for transcription if audio was saved and call is not encrypted
	if callID > 0 && meta.Encrypted == 0 {
		if meta.Transcript != "" {
			p.insertSourceTranscription(callID, callStartTime, identity.SystemID, meta.Talkgroup, meta)
		} else if p.isIMBEProvider() && msg.Call.AudioDvcfBase64 == "" {
			// IMBE provider needs the .dvcf file. If it wasn't embedded in this audio
			// message, the standalone /dvcf handler will enqueue transcription when
			// the DVCF message arrives.
			p.log.Debug().Int64("call_id", callID).Msg("skipping transcription enqueue — waiting for standalone DVCF message")
		} else {
			p.enqueueTranscription(callID, callStartTime, identity.SystemID, audioPath, meta)
		}
	}

	p.log.Debug().
		Str("sys_name", meta.ShortName).
		Int("tgid", meta.Talkgroup).
		Int("audio_size", audioSize).
		Int("freqs", len(meta.FreqList)).
		Int("srcs", len(meta.SrcList)).
		Msg("audio processed")

	return nil
}

// createCallFromAudio creates a call record from audio metadata when no call_start was received.
// The call_end handler will later find this record via FindCallForAudio and enrich it.
// Returns (callID, startTime, effectiveTgAlphaTag, error). The effective tag comes from the DB
// and respects the manual > csv > mqtt priority chain.
func (p *Pipeline) createCallFromAudio(ctx context.Context, identity *ResolvedIdentity, meta *AudioMetadata, startTime time.Time) (int64, time.Time, string, error) {
	// Final dedup check right before INSERT — narrows the TOCTOU race window
	// between concurrent MQTT (handleAudio) and file-watch (processWatchedFile)
	// paths from seconds to sub-millisecond.
	if existingID, existingST, err := p.db.FindCallForAudio(ctx, identity.SystemID, meta.Talkgroup, startTime); err == nil {
		return existingID, existingST, meta.TalkgroupTag, nil
	}

	freq := int64(meta.Freq)
	duration := float32(meta.CallLength)
	signal := float32(meta.Signal)
	noise := float32(meta.Noise)
	tdmaSlot := int16(meta.TDMASlot)
	recNum := int16(meta.RecorderNum)
	srcNum := int16(meta.SourceNum)
	siteID := identity.SiteID
	freqError := meta.FreqError
	encrypted := meta.Encrypted != 0
	emergency := meta.Emergency != 0

	row := &database.CallRow{
		SystemID:      identity.SystemID,
		SiteID:        &siteID,
		Tgid:          meta.Talkgroup,
		StartTime:     startTime,
		Duration:      &duration,
		Freq:          &freq,
		FreqError:     &freqError,
		SignalDB:      &signal,
		NoiseDB:       &noise,
		AudioType:     meta.AudioType,
		Phase2TDMA:    meta.Phase2TDMA != 0,
		TDMASlot:      &tdmaSlot,
		Encrypted:     encrypted,
		Emergency:     emergency,
		RecNum:        &recNum,
		SrcNum:        &srcNum,
		SystemName:    meta.ShortName,
		SiteShortName: meta.ShortName,
		TgAlphaTag:    meta.TalkgroupTag,
		TgDescription: meta.TalkgroupDesc,
		TgTag:         meta.TalkgroupGroupTag,
		TgGroup:       meta.TalkgroupGroup,
		IncidentData:  meta.IncidentData,
	}

	if meta.StopTime > 0 {
		st := time.Unix(meta.StopTime, 0)
		row.StopTime = &st
	}

	callID, err := p.db.InsertCall(ctx, row)
	if err != nil {
		return 0, time.Time{}, "", fmt.Errorf("insert call from audio: %w", err)
	}

	// Upsert talkgroup + enrich from directory — capture effective tag
	effectiveTgTag := meta.TalkgroupTag
	if meta.Talkgroup > 0 {
		effectiveTgTag = p.upsertAndEnrichTalkgroup(ctx, identity.SystemID, meta.Talkgroup,
			meta.TalkgroupTag, meta.TalkgroupGroupTag, meta.TalkgroupGroup, meta.TalkgroupDesc, startTime)
	}

	// Create call group
	cgID, cgErr := p.db.UpsertCallGroup(ctx, identity.SystemID, meta.Talkgroup, startTime,
		meta.TalkgroupTag, meta.TalkgroupDesc, meta.TalkgroupGroupTag, meta.TalkgroupGroup,
	)
	if cgErr == nil {
		_ = p.db.SetCallGroupID(ctx, callID, startTime, cgID)
		_ = p.db.SetCallGroupPrimary(ctx, cgID, callID)
	}

	p.log.Debug().
		Int64("call_id", callID).
		Int("tgid", meta.Talkgroup).
		Str("sys_name", meta.ShortName).
		Msg("call created from audio metadata")

	return callID, startTime, effectiveTgTag, nil
}

// srcFreqResult holds the pure data transformation output from buildSrcFreqJSON.
type srcFreqResult struct {
	SrcListJSON  json.RawMessage
	FreqListJSON json.RawMessage
	UnitIDs      []int32
}

// buildSrcFreqJSON transforms srcList/freqList metadata into denormalized JSON
// and extracts unique unit IDs. Pure function — no DB or side effects.
func buildSrcFreqJSON(srcList []SrcItem, freqList []FreqItem, callLength int) srcFreqResult {
	var result srcFreqResult

	if len(freqList) > 0 {
		type freqEntry struct {
			Freq       int64   `json:"freq"`
			Time       int64   `json:"time"`
			Pos        float64 `json:"pos"`
			Len        float64 `json:"len"`
			ErrorCount int     `json:"error_count"`
			SpikeCount int     `json:"spike_count"`
		}
		entries := make([]freqEntry, len(freqList))
		for i, f := range freqList {
			entries[i] = freqEntry{
				Freq:       int64(f.Freq),
				Time:       f.Time,
				Pos:        f.Pos,
				Len:        f.Len,
				ErrorCount: f.ErrorCount,
				SpikeCount: f.SpikeCount,
			}
		}
		result.FreqListJSON, _ = json.Marshal(entries)
	}

	unitSet := make(map[int32]struct{})
	if len(srcList) > 0 {
		type srcEntry struct {
			Src          int     `json:"src"`
			Tag          string  `json:"tag,omitempty"`
			Time         int64   `json:"time"`
			Pos          float64 `json:"pos"`
			Duration     float64 `json:"duration,omitempty"`
			Emergency    int     `json:"emergency"`
			SignalSystem string  `json:"signal_system,omitempty"`
		}
		entries := make([]srcEntry, len(srcList))
		for i, s := range srcList {
			var dur float64
			if i+1 < len(srcList) {
				dur = srcList[i+1].Pos - s.Pos
			} else if callLength > 0 {
				dur = float64(callLength) - s.Pos
			}
			entries[i] = srcEntry{
				Src:          s.Src,
				Tag:          s.Tag,
				Time:         s.Time,
				Pos:          s.Pos,
				Duration:     dur,
				Emergency:    s.Emergency,
				SignalSystem: s.SignalSystem,
			}
			unitSet[int32(s.Src)] = struct{}{}
		}
		result.SrcListJSON, _ = json.Marshal(entries)
	}

	result.UnitIDs = make([]int32, 0, len(unitSet))
	for uid := range unitSet {
		result.UnitIDs = append(result.UnitIDs, uid)
	}

	return result
}

// processSrcFreqData builds srcList/freqList JSON, updates the call's denormalized
// JSONB columns, and inserts into the relational call_frequencies/call_transmissions tables.
func (p *Pipeline) processSrcFreqData(ctx context.Context, callID int64, callStartTime time.Time, meta *AudioMetadata) {
	if len(meta.SrcList) == 0 && len(meta.FreqList) == 0 {
		return
	}

	sf := buildSrcFreqJSON(meta.SrcList, meta.FreqList, meta.CallLength)

	if err := p.db.UpdateCallSrcFreq(ctx, callID, callStartTime, sf.SrcListJSON, sf.FreqListJSON, sf.UnitIDs); err != nil {
		p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to update call src/freq data")
	}

	// Insert into relational tables for ad-hoc queries
	if len(meta.FreqList) > 0 {
		freqRows := make([]database.CallFrequencyRow, 0, len(meta.FreqList))
		for _, f := range meta.FreqList {
			ft := time.Unix(f.Time, 0)
			pos := float32(f.Pos)
			length := float32(f.Len)
			ec := f.ErrorCount
			sc := f.SpikeCount
			freqRows = append(freqRows, database.CallFrequencyRow{
				CallID:        callID,
				CallStartTime: callStartTime,
				Freq:          int64(f.Freq),
				Time:          &ft,
				Pos:           &pos,
				Len:           &length,
				ErrorCount:    &ec,
				SpikeCount:    &sc,
			})
		}
		if _, err := p.db.InsertCallFrequencies(ctx, freqRows); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to insert call frequencies")
		}
	}
	if len(meta.SrcList) > 0 {
		txRows := make([]database.CallTransmissionRow, 0, len(meta.SrcList))
		for i, s := range meta.SrcList {
			st := time.Unix(s.Time, 0)
			pos := float32(s.Pos)
			var dur *float32
			if i+1 < len(meta.SrcList) {
				d := float32(meta.SrcList[i+1].Pos - s.Pos)
				dur = &d
			} else if meta.CallLength > 0 {
				d := float32(float64(meta.CallLength) - s.Pos)
				dur = &d
			}
			txRows = append(txRows, database.CallTransmissionRow{
				CallID:        callID,
				CallStartTime: callStartTime,
				Src:           s.Src,
				Time:          &st,
				Pos:           &pos,
				Duration:      dur,
				Emergency:     int16(s.Emergency),
				SignalSystem:  s.SignalSystem,
				Tag:           s.Tag,
			})
		}
		if _, err := p.db.InsertCallTransmissions(ctx, txRows); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to insert call transmissions")
		}
	}
}

// processWatchedFile handles a JSON metadata file from the file watcher.
// It creates a call record, processes srcList/freqList, sets the audio path,
// and publishes a call_end SSE event.
func (p *Pipeline) processWatchedFile(instanceID string, meta *AudioMetadata, jsonPath string) error {
	startTime := time.Unix(meta.StartTime, 0)

	ctx, cancel := context.WithTimeout(p.ctx, 60*time.Second)
	defer cancel()

	// Resolve identity (auto-creates system/site if needed)
	identity, err := p.identity.Resolve(ctx, instanceID, meta.ShortName)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	// Check for existing call (dedup against MQTT ingest or prior backfill)
	if existingID, _, findErr := p.db.FindCallForAudio(ctx, identity.SystemID, meta.Talkgroup, startTime); findErr == nil {
		p.log.Debug().
			Int64("call_id", existingID).
			Str("path", jsonPath).
			Msg("watched file already in DB, skipping")
		return nil
	}

	// Create call from audio metadata
	callID, callStartTime, effectiveTgTag, err := p.createCallFromAudio(ctx, identity, meta, startTime)
	if err != nil && strings.Contains(err.Error(), "no partition") {
		// Auto-create missing partition and retry once
		p.ensurePartitionsFor(startTime)
		callID, callStartTime, effectiveTgTag, err = p.createCallFromAudio(ctx, identity, meta, startTime)
	}
	if err != nil {
		return fmt.Errorf("create call from watched file: %w", err)
	}

	// Set call_filename to the companion audio file next to the .json.
	// Try common extensions in preference order.
	base := strings.TrimSuffix(jsonPath, ".json")
	var audioPath string
	for _, ext := range []string{".m4a", ".wav", ".mp3"} {
		if _, statErr := os.Stat(base + ext); statErr == nil {
			audioPath = base + ext
			break
		}
	}
	if audioPath != "" {
		if err := p.db.UpdateCallFilename(ctx, callID, callStartTime, audioPath); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to set call_filename from watched file")
		}
		meta.Filename = audioPath // pass to transcription job
	}

	// Process srcList/freqList
	p.processSrcFreqData(ctx, callID, callStartTime, meta)

	// Upsert units from srcList
	for _, s := range meta.SrcList {
		if s.Src > 0 {
			_, _ = p.db.UpsertUnit(ctx, identity.SystemID, s.Src,
				s.Tag, "file_watch", startTime, meta.Talkgroup,
			)
		}
	}

	// Publish call_end SSE event (file appears after call is complete)
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
			"call_id":       callID,
			"system_id":     identity.SystemID,
			"tgid":          meta.Talkgroup,
			"tg_alpha_tag":  effectiveTgTag,
			"freq":          int64(meta.Freq),
			"start_time":    startTime,
			"stop_time":     stopTime,
			"duration":      float64(meta.CallLength),
			"emergency":     meta.Emergency != 0,
			"encrypted":     meta.Encrypted != 0,
			"call_filename": audioPath,
			"source":        "file_watch",
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

	p.log.Debug().
		Int64("call_id", callID).
		Int("tgid", meta.Talkgroup).
		Str("sys_name", meta.ShortName).
		Str("path", jsonPath).
		Msg("call created from watched file")

	return nil
}

// synthesizeSrcList builds srcList/freqList from unit_event:call records when
// trunk-recorder didn't provide them (e.g., encrypted calls). Only writes if the
// call's src_list is still NULL — never clobbers real data from the audio handler.
func (p *Pipeline) synthesizeSrcList(ctx context.Context, callID int64, callStartTime time.Time,
	systemID, tgid int, stopTime time.Time, callLength float32) {
	// Widen the window slightly to catch events near call boundaries
	windowStart := callStartTime.Add(-2 * time.Second)
	windowEnd := stopTime.Add(5 * time.Second)

	events, err := p.db.GetUnitEventsForCall(ctx, systemID, tgid, windowStart, windowEnd)
	if err != nil {
		p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to query unit events for srcList synthesis")
		return
	}
	if len(events) == 0 {
		return
	}

	// Build srcList entries from unit_event:call rows
	type srcEntry struct {
		Src       int     `json:"src"`
		Time      int64   `json:"time"`
		Pos       float64 `json:"pos"`
		Emergency int     `json:"emergency"`
		Tag       string  `json:"tag,omitempty"`
		Duration  float64 `json:"duration,omitempty"`
	}
	srcEntries := make([]srcEntry, 0, len(events))
	unitSet := make(map[int32]struct{})

	for _, ev := range events {
		emergency := 0
		if ev.Emergency {
			emergency = 1
		}
		dur := float64(ev.Length)
		srcEntries = append(srcEntries, srcEntry{
			Src:       ev.UnitRID,
			Time:      ev.Time.Unix(),
			Pos:       float64(ev.Position),
			Emergency: emergency,
			Tag:       ev.UnitAlphaTag,
			Duration:  dur,
		})
		unitSet[int32(ev.UnitRID)] = struct{}{}
	}

	srcListJSON, _ := json.Marshal(srcEntries)

	// Build freqList from distinct frequencies
	type freqEntry struct {
		Freq      int64   `json:"freq"`
		Time      int64   `json:"time"`
		Pos       float64 `json:"pos"`
		Len       float64 `json:"len"`
		ErrorCount int    `json:"error_count"`
		SpikeCount int    `json:"spike_count"`
	}
	freqMap := make(map[int64]bool)
	var freqEntries []freqEntry
	for _, ev := range events {
		if ev.Freq > 0 && !freqMap[ev.Freq] {
			freqMap[ev.Freq] = true
			freqEntries = append(freqEntries, freqEntry{
				Freq: ev.Freq,
				Time: ev.Time.Unix(),
				Pos:  float64(ev.Position),
				Len:  float64(callLength),
			})
		}
	}
	var freqListJSON json.RawMessage
	if len(freqEntries) > 0 {
		freqListJSON, _ = json.Marshal(freqEntries)
	}

	unitIDs := make([]int32, 0, len(unitSet))
	for uid := range unitSet {
		unitIDs = append(unitIDs, uid)
	}

	// Conditional update: only writes if src_list IS NULL
	updated, err := p.db.UpdateCallSrcFreqIfNull(ctx, callID, callStartTime, srcListJSON, freqListJSON, unitIDs)
	if err != nil {
		p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to write synthesized srcList")
		return
	}
	if updated == 0 {
		return // real data already present
	}

	// Insert into relational tables for ad-hoc queries
	txRows := make([]database.CallTransmissionRow, 0, len(srcEntries))
	for _, s := range srcEntries {
		t := time.Unix(s.Time, 0)
		pos := float32(s.Pos)
		var dur *float32
		if s.Duration > 0 {
			d := float32(s.Duration)
			dur = &d
		}
		txRows = append(txRows, database.CallTransmissionRow{
			CallID:        callID,
			CallStartTime: callStartTime,
			Src:           s.Src,
			Time:          &t,
			Pos:           &pos,
			Duration:      dur,
			Emergency:     int16(s.Emergency),
			Tag:           s.Tag,
		})
	}
	if len(txRows) > 0 {
		if _, err := p.db.InsertCallTransmissions(ctx, txRows); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to insert synthesized call transmissions")
		}
	}

	if len(freqEntries) > 0 {
		fRows := make([]database.CallFrequencyRow, 0, len(freqEntries))
		for _, f := range freqEntries {
			ft := time.Unix(f.Time, 0)
			pos := float32(f.Pos)
			length := float32(f.Len)
			fRows = append(fRows, database.CallFrequencyRow{
				CallID:        callID,
				CallStartTime: callStartTime,
				Freq:          f.Freq,
				Time:          &ft,
				Pos:           &pos,
				Len:           &length,
			})
		}
		if _, err := p.db.InsertCallFrequencies(ctx, fRows); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to insert synthesized call frequencies")
		}
	}

	p.log.Debug().
		Int64("call_id", callID).
		Int("srcs", len(srcEntries)).
		Int("freqs", len(freqEntries)).
		Int("units", len(unitIDs)).
		Msg("synthesized srcList from unit events")
}

// buildAudioFilename returns the filename to use for saving audio.
// If filename is empty, generates one from the start time and audio type.
func buildAudioFilename(filename, audioType string, startTime time.Time) string {
	if filename != "" {
		return filename
	}
	ext := ".wav"
	if audioType != "" {
		if audioType[0] != '.' {
			ext = "." + audioType
		} else {
			ext = audioType
		}
	}
	return fmt.Sprintf("%d%s", startTime.Unix(), ext)
}

// buildAudioRelPath constructs the relative path for an audio file:
// {sysName}/{YYYY-MM-DD}/{filename}
func buildAudioRelPath(sysName string, startTime time.Time, filename string) string {
	dateDir := startTime.Format("2006-01-02")
	return filepath.Join(sysName, dateDir, filename)
}

// saveAudio writes audio data through the storage abstraction.
// For tiered stores in async mode: writes to local cache synchronously,
// then enqueues S3 upload in the background.
func (p *Pipeline) saveAudio(ctx context.Context, key string, data []byte, contentType string) error {
	if p.uploader != nil {
		// Async mode: save locally first, then background S3 upload
		if tiered, ok := p.store.(*storage.TieredStore); ok {
			if err := tiered.SaveLocal(ctx, key, data, contentType); err != nil {
				return err
			}
			p.uploader.Enqueue(key, data, contentType)
			return nil
		}
	}
	// Sync mode or non-tiered store
	return p.store.Save(ctx, key, data, contentType)
}

// audioContentType returns the MIME type for an audio type string.
func audioContentType(audioType string) string {
	switch audioType {
	case "m4a":
		return "audio/mp4"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}
