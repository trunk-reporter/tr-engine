package audio

import (
	"context"
	"testing"
	"time"
)

func TestEndToEndPCMFlow(t *testing.T) {
	// Simulates: SimplestreamSource → AudioRouter → AudioBus → subscriber
	bus := NewAudioBus()
	lookup := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}

	router := NewAudioRouter(bus, lookup, "", 30*time.Second, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	// Subscribe to all audio
	ch, cancelSub := bus.Subscribe(AudioFilter{}, unrestricted())
	defer cancelSub()

	// Simulate 5 consecutive 20ms audio chunks (100ms of audio)
	for i := 0; i < 5; i++ {
		router.Input() <- AudioChunk{
			ShortName:  "butco",
			TGID:       1001,
			UnitID:     305,
			Format:     AudioFormatPCM,
			SampleRate: 8000,
			Data:       make([]byte, 320), // 160 samples * 2 bytes
			Timestamp:  time.Now(),
		}
	}

	// Should receive all 5 frames with incrementing sequence numbers
	for i := 0; i < 5; i++ {
		select {
		case frame := <-ch:
			if frame.TGID != 1001 {
				t.Errorf("frame %d: TGID = %d, want 1001", i, frame.TGID)
			}
			if int(frame.Seq) != i {
				t.Errorf("frame %d: Seq = %d, want %d", i, frame.Seq, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for frame %d", i)
		}
	}

	// Active stream count should be 1
	if n := router.ActiveStreamCount(); n != 1 {
		t.Errorf("ActiveStreamCount = %d, want 1", n)
	}
}
