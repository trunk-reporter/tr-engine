package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
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

	return nil
}
