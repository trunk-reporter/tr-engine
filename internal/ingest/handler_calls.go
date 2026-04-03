package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/snarg/tr-engine/internal/database"
)

// upsertAndEnrichTalkgroup upserts a talkgroup, enriches it from the directory,
// and returns the effective alpha tag (respects manual > csv > mqtt priority).
func (p *Pipeline) upsertAndEnrichTalkgroup(ctx context.Context, systemID, tgid int, alphaTag, tag, group, description string, eventTime time.Time) string {
	effectiveTag := alphaTag
	if dbTag, err := p.db.UpsertTalkgroup(ctx, systemID, tgid, alphaTag, tag, group, description, eventTime); err != nil {
		p.log.Warn().Err(err).Int("tgid", tgid).Msg("failed to upsert talkgroup")
	} else if dbTag != "" {
		effectiveTag = dbTag
	}
	// Enrich from directory and read back enriched tag if still empty
	if enriched, _ := p.db.EnrichTalkgroupsFromDirectory(ctx, systemID, tgid); enriched > 0 && effectiveTag == "" {
		if dbTag, err := p.db.GetTalkgroupAlphaTag(ctx, systemID, tgid); err == nil && dbTag != "" {
			effectiveTag = dbTag
		}
	}
	return effectiveTag
}

func (p *Pipeline) handleCallStart(payload []byte) error {
	var msg CallStartMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	call := &msg.Call
	startTime := time.Unix(call.StartTime, 0)

	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	identity, err := p.identity.Resolve(ctx, msg.InstanceID, call.SysName)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	// Upsert talkgroup + enrich from directory — capture effective tag
	effectiveTgTag := call.TalkgroupAlphaTag
	if call.Talkgroup > 0 {
		effectiveTgTag = p.upsertAndEnrichTalkgroup(ctx, identity.SystemID, call.Talkgroup,
			call.TalkgroupAlphaTag, call.TalkgroupTag, call.TalkgroupGroup, call.TalkgroupDescription, startTime)
	}

	// Upsert unit — capture effective tag from DB
	effectiveUnitTag := call.UnitAlphaTag
	if call.Unit > 0 {
		if dbTag, err := p.db.UpsertUnit(ctx, identity.SystemID, call.Unit,
			call.UnitAlphaTag, "call_start", startTime, call.Talkgroup,
		); err != nil {
			p.log.Warn().Err(err).Int("unit", call.Unit).Msg("failed to upsert unit")
		} else if dbTag != "" {
			effectiveUnitTag = dbTag
		}
	}

	// Check if already tracked
	if _, ok := p.activeCalls.Get(call.ID); ok {
		return nil // duplicate call_start
	}

	freq := int64(call.Freq)
	callState := int16(call.CallState)
	monState := int16(call.MonState)
	recState := int16(call.RecState)
	siteID := identity.SiteID

	// Check if the audio handler already created this call (audio can arrive before call_start).
	// If so, enrich the existing record instead of creating a duplicate.
	var callID int64
	existingID, existingST, findErr := p.db.FindCallForAudio(ctx, identity.SystemID, call.Talkgroup, startTime)
	if findErr == nil {
		callID = existingID
		startTime = existingST
		if err := p.db.UpdateCallStartFields(ctx, callID, startTime,
			call.ID, call.CallNum, msg.InstanceID,
			callState, call.CallStateType,
			monState, call.MonStateType,
			recState, call.RecStateType,
		); err != nil {
			p.log.Warn().Err(err).Int64("call_id", callID).Msg("failed to update audio-created call with call_start fields")
		}
		p.log.Debug().
			Str("tr_call_id", call.ID).
			Int64("call_id", callID).
			Msg("call_start matched audio-created call")
	} else {
		duration := float32(call.Length)
		callNum := call.CallNum
		recNum := int16(call.RecNum)
		srcNum := int16(call.SrcNum)
		tdmaSlot := int16(call.TDMASlot)

		row := &database.CallRow{
			SystemID:      identity.SystemID,
			SiteID:        &siteID,
			Tgid:          call.Talkgroup,
			TrCallID:      call.ID,
			CallNum:       &callNum,
			StartTime:     startTime,
			Duration:      &duration,
			Freq:          &freq,
			AudioType:     call.AudioType,
			Phase2TDMA:    call.Phase2TDMA,
			TDMASlot:      &tdmaSlot,
			Analog:        call.Analog,
			Conventional:  call.Conventional,
			Encrypted:     call.Encrypted,
			Emergency:     call.Emergency,
			CallState:     &callState,
			CallStateType: call.CallStateType,
			MonState:      &monState,
			MonStateType:  call.MonStateType,
			RecState:      &recState,
			RecStateType:  call.RecStateType,
			RecNum:        &recNum,
			SrcNum:        &srcNum,
			SystemName:    call.SysName,
			SiteShortName: call.SysName,
			TgAlphaTag:    call.TalkgroupAlphaTag,
			TgDescription: call.TalkgroupDescription,
			TgTag:         call.TalkgroupTag,
			TgGroup:       call.TalkgroupGroup,
			IncidentData:  call.IncidentData,
			InstanceID:    msg.InstanceID,
		}

		// For encrypted calls, store the initiating unit in unit_ids since
		// the audio handler (which normally populates this) will never run.
		if call.Encrypted && call.Unit > 0 {
			row.UnitIDs = []int32{int32(call.Unit)}
		}

		if call.StopTime > 0 {
			st := time.Unix(call.StopTime, 0)
			row.StopTime = &st
		}

		var insertErr error
		callID, insertErr = p.db.InsertCall(ctx, row)
		if insertErr != nil && strings.Contains(insertErr.Error(), "no partition") {
			p.ensurePartitionsFor(startTime)
			callID, insertErr = p.db.InsertCall(ctx, row)
		}
		if insertErr != nil {
			return fmt.Errorf("insert call: %w", insertErr)
		}
	}

	p.activeCalls.Set(call.ID, activeCallEntry{
		CallID:        callID,
		StartTime:     startTime,
		SystemID:      identity.SystemID,
		SystemName:    call.SysName,
		Sysid:         identity.Sysid,
		SiteID:        &siteID,
		SiteShortName: call.SysName,
		Tgid:          call.Talkgroup,
		TgAlphaTag:    effectiveTgTag,
		TgDescription: call.TalkgroupDescription,
		TgTag:         call.TalkgroupTag,
		TgGroup:       call.TalkgroupGroup,
		Unit:          call.Unit,
		UnitAlphaTag:  effectiveUnitTag,
		Freq:          freq,
		Emergency:     call.Emergency,
		Encrypted:     call.Encrypted,
		Analog:        call.Analog,
		Conventional:  call.Conventional,
		Phase2TDMA:    call.Phase2TDMA,
		AudioType:     call.AudioType,
	})

	// Update conventional freq→talkgroup map for AnalogC recorder enrichment
	if (call.Conventional || call.Analog) && freq > 0 && call.Talkgroup > 0 {
		p.conventionalFreqMap.Store(freq, conventionalFreqEntry{
			SystemID:   identity.SystemID,
			Tgid:       call.Talkgroup,
			TgAlphaTag: effectiveTgTag,
		})
	}

	// Create call group
	cgID, err := p.db.UpsertCallGroup(ctx, identity.SystemID, call.Talkgroup, startTime,
		call.TalkgroupAlphaTag, call.TalkgroupDescription, call.TalkgroupTag, call.TalkgroupGroup,
	)
	if err != nil {
		p.log.Warn().Err(err).Msg("failed to upsert call group")
	} else {
		_ = p.db.SetCallGroupID(ctx, callID, startTime, cgID)
		_ = p.db.SetCallGroupPrimary(ctx, cgID, callID)
	}

	p.log.Debug().
		Str("tr_call_id", call.ID).
		Int64("call_id", callID).
		Int("tgid", call.Talkgroup).
		Str("sys_name", call.SysName).
		Msg("call started")

	p.PublishEvent(EventData{
		Type:      "call_start",
		SystemID:  identity.SystemID,
		SiteID:    siteID,
		Tgid:      call.Talkgroup,
		UnitID:    call.Unit,
		Emergency: call.Emergency,
		Payload: map[string]any{
			"call_id":         callID,
			"system_id":       identity.SystemID,
			"tgid":            call.Talkgroup,
			"tg_alpha_tag":    effectiveTgTag,
			"tg_tag":          call.TalkgroupTag,
			"tg_group":        call.TalkgroupGroup,
			"tg_description":  call.TalkgroupDescription,
			"unit":            call.Unit,
			"unit_alpha_tag":  effectiveUnitTag,
			"freq":            freq,
			"start_time":      startTime,
			"emergency":       call.Emergency,
			"encrypted":       call.Encrypted,
			"analog":          call.Analog,
			"conventional":    call.Conventional,
			"phase2_tdma":     call.Phase2TDMA,
			"audio_type":      call.AudioType,
			"incident_data":   call.IncidentData,
		},
	})

	return nil
}

func (p *Pipeline) handleCallEnd(payload []byte) error {
	var msg CallEndMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	call := &msg.Call
	startTime := time.Unix(call.StartTime, 0)

	// 10s budget: slow path may do FindCallByTrCallID + Resolve + FindCallForAudio
	// before the actual UpdateCallEnd, which needs reliable time remaining.
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	// Find active call
	entry, ok := p.activeCalls.Get(call.ID)
	matchedKey := call.ID
	if !ok {
		// Fuzzy match: TR may adjust start_time between call_start and call_end
		// (1-2s for P25, up to ~6s for analog), which changes the ID since it
		// embeds start_time.
		matchedKey, entry, ok = p.activeCalls.FindByTgidAndTime(call.Talkgroup, startTime, 10*time.Second)
	}
	if !ok {
		// Call started before we were running, or duplicate. Try DB lookup.
		var err error
		entry.CallID, entry.StartTime, err = p.db.FindCallByTrCallID(ctx, call.ID, &startTime)
		if err != nil {
			// Not found by tr_call_id — maybe audio handler already created it.
			identity, idErr := p.identity.Resolve(ctx, msg.InstanceID, call.SysName)
			if idErr != nil {
				// Identity resolution failed (transient DB/network error). Return error
				// rather than creating a duplicate call via handleCallStartFromEnd.
				return fmt.Errorf("resolve identity for call_end lookup: %w", idErr)
			}
			entry.CallID, entry.StartTime, err = p.db.FindCallForAudio(ctx, identity.SystemID, call.Talkgroup, startTime)
			if err != nil {
				// Truly not found — insert it fresh with a new context since the
				// current one has been partially consumed by lookup attempts.
				freshCtx, freshCancel := context.WithTimeout(p.ctx, 15*time.Second)
				defer freshCancel()
				return p.handleCallStartFromEnd(freshCtx, &msg)
			}
		}
		matchedKey = "" // came from DB, nothing to delete from active map
	}

	stopTime := time.Unix(call.StopTime, 0)

	err := p.db.UpdateCallEnd(ctx,
		entry.CallID, entry.StartTime,
		stopTime,
		float32(call.Length),
		int64(call.Freq),
		call.FreqError,
		float32(call.Signal),
		float32(call.Noise),
		call.ErrorCount,
		call.SpikeCount,
		int16(call.RecState), call.RecStateType,
		int16(call.CallState), call.CallStateType,
		call.CallFilename,
		int16(call.RetryAttempt),
		float32(call.ProcessCallTime),
	)
	if err != nil {
		return fmt.Errorf("update call end: %w", err)
	}

	if matchedKey != "" {
		p.activeCalls.Delete(matchedKey)
	}

	// Resolve identity for talkgroup upsert and event publishing
	identity, idErr := p.identity.Resolve(ctx, msg.InstanceID, call.SysName)
	effectiveTgTag := call.TalkgroupAlphaTag
	effectiveUnitTag := call.UnitAlphaTag
	if idErr == nil && call.Talkgroup > 0 {
		effectiveTgTag = p.upsertAndEnrichTalkgroup(ctx, identity.SystemID, call.Talkgroup,
			call.TalkgroupAlphaTag, call.TalkgroupTag, call.TalkgroupGroup, call.TalkgroupDescription, startTime)
	}
	if idErr == nil && call.Unit > 0 {
		if dbTag, upsertErr := p.db.UpsertUnit(ctx, identity.SystemID, call.Unit,
			call.UnitAlphaTag, "call_end", startTime, call.Talkgroup,
		); upsertErr == nil && dbTag != "" {
			effectiveUnitTag = dbTag
		}
	}

	p.log.Debug().
		Str("tr_call_id", call.ID).
		Int64("call_id", entry.CallID).
		Float64("duration", call.Length).
		Msg("call ended")

	if idErr == nil {
		p.PublishEvent(EventData{
			Type:      "call_end",
			SystemID:  identity.SystemID,
			SiteID:    identity.SiteID,
			Tgid:      call.Talkgroup,
			Emergency: call.Emergency,
			Payload: map[string]any{
				"call_id":        entry.CallID,
				"system_id":      identity.SystemID,
				"tgid":           call.Talkgroup,
				"tg_alpha_tag":   effectiveTgTag,
				"unit":           call.Unit,
				"unit_alpha_tag": effectiveUnitTag,
				"freq":           int64(call.Freq),
				"start_time":     startTime,
				"stop_time":      stopTime,
				"duration":       call.Length,
				"emergency":      call.Emergency,
				"encrypted":      call.Encrypted,
				"call_filename":  call.CallFilename,
				"incident_data":  call.IncidentData,
			},
		})

		// Enqueue for transcription in TR_AUDIO_DIR mode.
		// When audio comes via MQTT, handleAudio enqueues instead.
		if p.trAudioDir != "" && !call.Encrypted && call.CallFilename != "" {
			meta := &AudioMetadata{
				Talkgroup:         call.Talkgroup,
				CallLength:        int(call.Length),
				Filename:          call.CallFilename,
				TalkgroupTag:      effectiveTgTag,
				TalkgroupDesc:     call.TalkgroupDescription,
				TalkgroupGroupTag: call.TalkgroupTag,
				TalkgroupGroup:    call.TalkgroupGroup,
			}
			p.enqueueTranscription(entry.CallID, entry.StartTime, identity.SystemID, "", meta)
		}

		// Synthesize srcList from unit_event:call records if TR didn't provide one
		// (e.g., encrypted calls). Only writes if src_list is still NULL.
		p.synthesizeSrcList(ctx, entry.CallID, entry.StartTime,
			identity.SystemID, call.Talkgroup, stopTime, float32(call.Length))
	}

	return nil
}

// handleCallStartFromEnd creates a call record from a call_end message when we missed the call_start.
func (p *Pipeline) handleCallStartFromEnd(ctx context.Context, msg *CallEndMsg) error {
	call := &msg.Call
	startTime := time.Unix(call.StartTime, 0)

	identity, err := p.identity.Resolve(ctx, msg.InstanceID, call.SysName)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	freq := int64(call.Freq)
	duration := float32(call.Length)
	callNum := call.CallNum
	callState := int16(call.CallState)
	monState := int16(call.MonState)
	recState := int16(call.RecState)
	recNum := int16(call.RecNum)
	srcNum := int16(call.SrcNum)
	tdmaSlot := int16(call.TDMASlot)
	siteID := identity.SiteID
	freqError := call.FreqError
	signal := float32(call.Signal)
	noise := float32(call.Noise)
	stopTime := time.Unix(call.StopTime, 0)
	row := &database.CallRow{
		SystemID:      identity.SystemID,
		SiteID:        &siteID,
		Tgid:          call.Talkgroup,
		TrCallID:      call.ID,
		CallNum:       &callNum,
		StartTime:     startTime,
		StopTime:      &stopTime,
		Duration:      &duration,
		Freq:          &freq,
		FreqError:     &freqError,
		SignalDB:      &signal,
		NoiseDB:       &noise,
		ErrorCount:    &call.ErrorCount,
		SpikeCount:    &call.SpikeCount,
		AudioType:     call.AudioType,
		Phase2TDMA:    call.Phase2TDMA,
		TDMASlot:      &tdmaSlot,
		Analog:        call.Analog,
		Conventional:  call.Conventional,
		Encrypted:     call.Encrypted,
		Emergency:     call.Emergency,
		CallState:     &callState,
		CallStateType: call.CallStateType,
		MonState:      &monState,
		MonStateType:  call.MonStateType,
		RecState:      &recState,
		RecStateType:  call.RecStateType,
		RecNum:        &recNum,
		SrcNum:        &srcNum,
		SystemName:    call.SysName,
		SiteShortName: call.SysName,
		TgAlphaTag:    call.TalkgroupAlphaTag,
		TgDescription: call.TalkgroupDescription,
		TgTag:         call.TalkgroupTag,
		TgGroup:       call.TalkgroupGroup,
		IncidentData:  call.IncidentData,
		InstanceID:    msg.InstanceID,
	}

	if call.Encrypted && call.Unit > 0 {
		row.UnitIDs = []int32{int32(call.Unit)}
	}

	// Upsert talkgroup + enrich from directory — capture effective tag
	effectiveTgTag := call.TalkgroupAlphaTag
	if call.Talkgroup > 0 {
		effectiveTgTag = p.upsertAndEnrichTalkgroup(ctx, identity.SystemID, call.Talkgroup,
			call.TalkgroupAlphaTag, call.TalkgroupTag, call.TalkgroupGroup, call.TalkgroupDescription, startTime)
	}

	// Upsert unit — capture effective tag from DB
	effectiveUnitTag := call.UnitAlphaTag
	if call.Unit > 0 {
		if dbTag, upsertErr := p.db.UpsertUnit(ctx, identity.SystemID, call.Unit,
			call.UnitAlphaTag, "call_end", startTime, call.Talkgroup,
		); upsertErr == nil && dbTag != "" {
			effectiveUnitTag = dbTag
		}
	}

	// Retry: audio handler may have created this call concurrently (race on reconnect bursts).
	// By now the audio insert has had time to commit.
	existingID, existingST, findErr := p.db.FindCallForAudio(ctx, identity.SystemID, call.Talkgroup, startTime)
	if findErr == nil {
		// Audio already created the call — update it with call_end data instead of inserting a duplicate.
		err = p.db.UpdateCallEnd(ctx,
			existingID, existingST,
			stopTime,
			duration,
			freq,
			freqError,
			signal,
			noise,
			call.ErrorCount,
			call.SpikeCount,
			recState, call.RecStateType,
			callState, call.CallStateType,
			call.CallFilename,
			int16(call.RetryAttempt),
			float32(call.ProcessCallTime),
		)
		if err != nil {
			return fmt.Errorf("update audio-created call: %w", err)
		}
		p.log.Debug().
			Str("tr_call_id", call.ID).
			Int64("call_id", existingID).
			Msg("call_end matched audio-created call")

		p.PublishEvent(EventData{
			Type:      "call_end",
			SystemID:  identity.SystemID,
			SiteID:    identity.SiteID,
			Tgid:      call.Talkgroup,
			Emergency: call.Emergency,
			Payload: map[string]any{
				"call_id":        existingID,
				"system_id":      identity.SystemID,
				"tgid":           call.Talkgroup,
				"tg_alpha_tag":   effectiveTgTag,
				"unit":           call.Unit,
				"unit_alpha_tag": effectiveUnitTag,
				"freq":           freq,
				"start_time":     startTime,
				"stop_time":      stopTime,
				"duration":       call.Length,
				"emergency":      call.Emergency,
				"encrypted":      call.Encrypted,
				"call_filename":  call.CallFilename,
				"incident_data":  call.IncidentData,
			},
		})

		// Enqueue for transcription in TR_AUDIO_DIR mode
		if p.trAudioDir != "" && !call.Encrypted && call.CallFilename != "" {
			meta := &AudioMetadata{
				Talkgroup:         call.Talkgroup,
				CallLength:        int(call.Length),
				Filename:          call.CallFilename,
				TalkgroupTag:      effectiveTgTag,
				TalkgroupDesc:     call.TalkgroupDescription,
				TalkgroupGroupTag: call.TalkgroupTag,
				TalkgroupGroup:    call.TalkgroupGroup,
			}
			p.enqueueTranscription(existingID, existingST, identity.SystemID, "", meta)
		}

		// Synthesize srcList from unit events if TR didn't provide one
		p.synthesizeSrcList(ctx, existingID, existingST,
			identity.SystemID, call.Talkgroup, stopTime, duration)

		return nil
	}

	callID, err := p.db.InsertCall(ctx, row)
	if err != nil && strings.Contains(err.Error(), "no partition") {
		p.ensurePartitionsFor(startTime)
		callID, err = p.db.InsertCall(ctx, row)
	}
	if err != nil {
		return fmt.Errorf("insert call from end: %w", err)
	}

	// Create call group (same as handleCallStart)
	cgID, cgErr := p.db.UpsertCallGroup(ctx, identity.SystemID, call.Talkgroup, startTime,
		call.TalkgroupAlphaTag, call.TalkgroupDescription, call.TalkgroupTag, call.TalkgroupGroup,
	)
	if cgErr != nil {
		p.log.Warn().Err(cgErr).Msg("failed to upsert call group from call_end backfill")
	} else {
		_ = p.db.SetCallGroupID(ctx, callID, startTime, cgID)
		_ = p.db.SetCallGroupPrimary(ctx, cgID, callID)
	}

	// Update conventional freq→talkgroup map for AnalogC recorder enrichment
	if (call.Conventional || call.Analog) && freq > 0 && call.Talkgroup > 0 {
		p.conventionalFreqMap.Store(freq, conventionalFreqEntry{
			SystemID:   identity.SystemID,
			Tgid:       call.Talkgroup,
			TgAlphaTag: effectiveTgTag,
		})
	}

	p.log.Debug().
		Str("tr_call_id", call.ID).
		Int64("call_id", callID).
		Msg("call inserted from call_end (missed call_start)")

	p.PublishEvent(EventData{
		Type:      "call_end",
		SystemID:  identity.SystemID,
		SiteID:    identity.SiteID,
		Tgid:      call.Talkgroup,
		Emergency: call.Emergency,
		Payload: map[string]any{
			"call_id":        callID,
			"system_id":      identity.SystemID,
			"tgid":           call.Talkgroup,
			"tg_alpha_tag":   effectiveTgTag,
			"unit":           call.Unit,
			"unit_alpha_tag": effectiveUnitTag,
			"freq":           freq,
			"start_time":     startTime,
			"stop_time":      stopTime,
			"duration":       call.Length,
			"emergency":      call.Emergency,
			"encrypted":      call.Encrypted,
			"call_filename":  call.CallFilename,
			"incident_data":  call.IncidentData,
		},
	})

	// Enqueue for transcription in TR_AUDIO_DIR mode
	if p.trAudioDir != "" && !call.Encrypted && call.CallFilename != "" {
		meta := &AudioMetadata{
			Talkgroup:         call.Talkgroup,
			CallLength:        int(call.Length),
			Filename:          call.CallFilename,
			TalkgroupTag:      effectiveTgTag,
			TalkgroupDesc:     call.TalkgroupDescription,
			TalkgroupGroupTag: call.TalkgroupTag,
			TalkgroupGroup:    call.TalkgroupGroup,
		}
		p.enqueueTranscription(callID, startTime, identity.SystemID, "", meta)
	}

	// Synthesize srcList from unit events if TR didn't provide one
	p.synthesizeSrcList(ctx, callID, startTime,
		identity.SystemID, call.Talkgroup, stopTime, duration)

	return nil
}

func (p *Pipeline) handleCallsActive(payload []byte) error {
	var msg CallsActiveMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}

	// Store as checkpoint for crash recovery. Use a longer timeout because this
	// handler iterates over all active calls with individual DB updates.
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	if err := p.db.InsertActiveCallCheckpoint(ctx, msg.InstanceID, payload, len(msg.Calls)); err != nil {
		p.log.Warn().Err(err).Msg("failed to insert active call checkpoint")
	}

	// Build set of TR call IDs still active in this snapshot
	activeIDs := make(map[string]CallData, len(msg.Calls))
	for _, c := range msg.Calls {
		activeIDs[c.ID] = c
	}

	// Update elapsed time for active calls and detect ended calls.
	// Calls present in our map but absent from the active list have ended.
	// This is the only end signal for encrypted/monitor-only calls.
	snapshot := p.activeCalls.All()
	for trCallID, entry := range snapshot {
		if activeCall, ok := activeIDs[trCallID]; ok {
			// Still active — update elapsed duration for call_update events
			if activeCall.Elapsed > 0 {
				elapsed := float32(activeCall.Elapsed)
				stopTime := entry.StartTime.Add(time.Duration(activeCall.Elapsed) * time.Second)
				_ = p.db.UpdateCallElapsed(ctx, entry.CallID, entry.StartTime, &stopTime, &elapsed)
			}
			continue
		}

		// Call disappeared from active list — it's ended.
		// Synthesize an ending for any call that vanishes. If a proper call_end
		// MQTT message arrives later with richer metadata (signal, noise, filename),
		// handleCallEnd does a DB lookup and overwrites these placeholder values.
		p.log.Debug().
			Str("tr_call_id", trCallID).
			Int64("call_id", entry.CallID).
			Int("tgid", entry.Tgid).
			Bool("encrypted", entry.Encrypted).
			Msg("call ended (disappeared from calls_active)")

		// Estimate stop time as now (the call ended sometime between the last
		// calls_active that included it and this one)
		stopTime := time.Now()
		duration := float32(stopTime.Sub(entry.StartTime).Seconds())

		if err := p.db.UpdateCallEnd(ctx,
			entry.CallID, entry.StartTime,
			stopTime, duration,
			entry.Freq,
			0,    // freq_error
			0, 0, // signal, noise (unknown — call_end may overwrite)
			0, 0, // error_count, spike_count
			0, "COMPLETED", // rec_state
			0, "COMPLETED", // call_state
			"",             // call_filename (no recording)
			0,              // retry_attempt
			0,              // process_call_time
		); err != nil {
			p.log.Warn().Err(err).Int64("call_id", entry.CallID).Msg("failed to close stale call")
		}

		p.activeCalls.Delete(trCallID)

		siteID := 0
		if entry.SiteID != nil {
			siteID = *entry.SiteID
		}

		p.PublishEvent(EventData{
			Type:      "call_end",
			SystemID:  entry.SystemID,
			SiteID:    siteID,
			Tgid:      entry.Tgid,
			UnitID:    entry.Unit,
			Emergency: entry.Emergency,
			Payload: map[string]any{
				"call_id":        entry.CallID,
				"system_id":      entry.SystemID,
				"tgid":           entry.Tgid,
				"tg_alpha_tag":   entry.TgAlphaTag,
				"unit":           entry.Unit,
				"unit_alpha_tag": entry.UnitAlphaTag,
				"freq":           entry.Freq,
				"start_time":     entry.StartTime,
				"stop_time":      stopTime,
				"duration":       duration,
				"emergency":      entry.Emergency,
				"encrypted":      entry.Encrypted,
			},
		})

		// Synthesize srcList from unit events if TR didn't provide one
		p.synthesizeSrcList(ctx, entry.CallID, entry.StartTime,
			entry.SystemID, entry.Tgid, stopTime, duration)
	}

	p.log.Debug().
		Int("active_calls", len(msg.Calls)).
		Str("instance_id", msg.InstanceID).
		Msg("calls_active checkpoint stored")

	return nil
}
