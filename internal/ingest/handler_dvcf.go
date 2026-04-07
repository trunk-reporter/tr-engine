package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/snarg/tr-engine/internal/transcribe"
)

func (p *Pipeline) handleDvcf(payload []byte) error {
	var msg DvcfMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("unmarshal dvcf message: %w", err)
	}

	if msg.AudioDvcfBase64 == "" {
		p.log.Warn().Msg("dvcf message has no audio_dvcf_base64, skipping")
		return nil
	}

	meta := &msg.Metadata
	if meta.Filename == "" {
		p.log.Warn().Int("tgid", meta.Talkgroup).Msg("dvcf message has no filename in metadata, skipping")
		return nil
	}

	// Decode the base64 DVCF data
	dvcfData, err := base64.StdEncoding.DecodeString(msg.AudioDvcfBase64)
	if err != nil {
		return fmt.Errorf("decode dvcf base64: %w", err)
	}

	// Derive the save path: same structure as audio files but with .dvcf extension
	startTime := time.Unix(meta.StartTime, 0)
	audioFilename := meta.Filename
	ext := filepath.Ext(audioFilename)
	dvcfFilename := strings.TrimSuffix(audioFilename, ext) + ".dvcf"
	dvcfKey := buildAudioRelPath(meta.ShortName, startTime, dvcfFilename)

	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	if err := p.saveAudio(ctx, dvcfKey, dvcfData, "application/octet-stream"); err != nil {
		return fmt.Errorf("save dvcf file: %w", err)
	}

	p.log.Debug().
		Str("dvcf_key", dvcfKey).
		Int("dvcf_size", len(dvcfData)).
		Int("tgid", meta.Talkgroup).
		Str("sys_name", meta.ShortName).
		Msg("dvcf file saved")

	// Enqueue transcription if IMBE provider is active
	if !p.isIMBEProvider() {
		return nil
	}

	// Find the matching call by system_name + tgid + start_time
	call, err := p.db.FindCallBySystemName(ctx, meta.ShortName, meta.Talkgroup, startTime)
	if err != nil {
		p.log.Warn().
			Err(err).
			Str("sys_name", meta.ShortName).
			Int("tgid", meta.Talkgroup).
			Int64("start_time", meta.StartTime).
			Msg("dvcf: no matching call found, transcription will rely on backfill")
		return nil
	}

	// Check duration and talkgroup filters
	if call.Duration < float32(p.transcriber.MinDuration()) || call.Duration > float32(p.transcriber.MaxDuration()) {
		return nil
	}
	if !p.shouldTranscribeTG(call.SystemID, call.Tgid) {
		return nil
	}

	job := transcribe.Job{
		CallID:        call.CallID,
		CallStartTime: call.StartTime,
		SystemID:      call.SystemID,
		Tgid:          call.Tgid,
		Duration:      call.Duration,
		AudioFilePath: dvcfKey,
		CallFilename:  meta.Filename,
		SrcList:       call.SrcList,
		TgAlphaTag:    call.TgAlphaTag,
		TgDescription: call.TgDescription,
		TgTag:         call.TgTag,
		TgGroup:       call.TgGroup,
	}
	if !p.transcriber.Enqueue(job) {
		p.log.Warn().Int64("call_id", call.CallID).Msg("dvcf: transcription queue full, skipping")
	}

	return nil
}
