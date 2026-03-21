package audio

import (
	"context"
	"testing"
	"time"
)

// mockIdentityLookup implements IdentityLookup for testing.
type mockIdentityLookup struct {
	systems map[string]identityResult
}

type identityResult struct {
	systemID int
	siteID   int
}

func (m *mockIdentityLookup) LookupByShortName(instanceID, shortName string) (systemID, siteID int, ok bool) {
	r, found := m.systems[shortName]
	if !found {
		return 0, 0, false
	}
	return r.systemID, r.siteID, true
}

func (m *mockIdentityLookup) LookupByShortNameAny(shortName string, exclude map[string]bool) (systemID, siteID int, instanceID string, ok bool) {
	r, found := m.systems[shortName]
	if !found {
		return 0, 0, "", false
	}
	return r.systemID, r.siteID, "default", true
}

func makeChunk(shortName string, tgid, unitID int) AudioChunk {
	return AudioChunk{
		ShortName:  shortName,
		TGID:       tgid,
		UnitID:     unitID,
		Format:     AudioFormatPCM,
		SampleRate: 8000,
		Data:       []byte{0x01, 0x02, 0x03, 0x04},
		Timestamp:  time.Now(),
	}
}

func TestRouterResolvesIdentity(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}
	router := NewAudioRouter(bus, mock, "", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	router.Input() <- makeChunk("butco", 1001, 500)

	select {
	case frame := <-ch:
		if frame.SystemID != 1 {
			t.Errorf("SystemID = %d, want 1", frame.SystemID)
		}
		if frame.TGID != 1001 {
			t.Errorf("TGID = %d, want 1001", frame.TGID)
		}
		if frame.UnitID != 500 {
			t.Errorf("UnitID = %d, want 500", frame.UnitID)
		}
		if frame.Seq != 0 {
			t.Errorf("Seq = %d, want 0 (first frame)", frame.Seq)
		}
		if frame.Format != AudioFormatPCM {
			t.Errorf("Format = %v, want PCM", frame.Format)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
	}
}

func TestRouterDropsUnknownSystem(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}
	router := NewAudioRouter(bus, mock, "", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	router.Input() <- makeChunk("unknown", 1001, 500)

	select {
	case <-ch:
		t.Fatal("received frame for unknown system, expected drop")
	case <-time.After(200 * time.Millisecond):
		// Expected: no frame received
	}
}

func TestRouterDeduplicatesMultiSite(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
			"warco": {systemID: 1, siteID: 20},
		},
	}
	router := NewAudioRouter(bus, mock, "", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	// butco sends first — claims the stream for TG 1001
	router.Input() <- makeChunk("butco", 1001, 500)

	select {
	case frame := <-ch:
		if frame.SystemID != 1 || frame.TGID != 1001 {
			t.Errorf("first frame: SystemID=%d TGID=%d, want 1/1001", frame.SystemID, frame.TGID)
		}
		if frame.Seq != 0 {
			t.Errorf("first frame Seq = %d, want 0", frame.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first frame")
	}

	// warco sends for the same TGID — should be dropped (dedup)
	router.Input() <- makeChunk("warco", 1001, 501)

	select {
	case <-ch:
		t.Fatal("received frame from warco, expected dedup drop")
	case <-time.After(200 * time.Millisecond):
		// Expected: dropped
	}

	// butco sends again — should succeed, seq incremented
	router.Input() <- makeChunk("butco", 1001, 500)

	select {
	case frame := <-ch:
		if frame.Seq != 1 {
			t.Errorf("third frame Seq = %d, want 1", frame.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for third frame")
	}
}

func TestRouterIdleStreamRelease(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
			"warco": {systemID: 1, siteID: 20},
		},
	}
	// Use 100ms idle timeout for fast test
	router := NewAudioRouter(bus, mock, "", 100*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	// butco claims TG 1001
	router.Input() <- makeChunk("butco", 1001, 500)

	select {
	case <-ch:
		// Consume the frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for butco frame")
	}

	// Wait for idle timeout + cleanup tick (cleanup runs every 5s, but we
	// need to wait for the stream to go idle). Since cleanup runs on a ticker,
	// we sleep a bit longer than the idle timeout to ensure the next tick
	// catches it. The router's cleanup ticker is 5s, which is too long for
	// tests. We'll rely on the fact that processChunk also checks staleness
	// OR we wait long enough. Actually, looking at the spec, cleanup runs
	// every 5s which is too slow. Let's sleep 200ms (past the 100ms idle)
	// and then send from warco. If processChunk checks staleness when the
	// existing stream's site differs, the test works. Otherwise we need a
	// shorter cleanup interval.
	//
	// The simplest approach: sleep past idle timeout, then warco claims.
	// processChunk should notice the existing stream is stale and allow takeover.
	time.Sleep(200 * time.Millisecond)

	// warco sends for the same TG — should succeed because butco's stream is idle
	router.Input() <- makeChunk("warco", 1001, 501)

	select {
	case frame := <-ch:
		if frame.UnitID != 501 {
			t.Errorf("UnitID = %d, want 501 (warco's unit)", frame.UnitID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for warco frame after idle release")
	}
}

func TestRouterTracksJitter(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}
	router := NewAudioRouter(bus, mock, "", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	// Send 3 chunks with controlled timestamps
	for i := 0; i < 3; i++ {
		chunk := makeChunk("butco", 1001, 500)
		chunk.Timestamp = time.Now()
		router.Input() <- chunk
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stats := router.GetJitterStats()
	s, ok := stats["1:1001"]
	if !ok {
		t.Fatal("no jitter stats for 1:1001")
	}
	// After 3 chunks, we should have 2 deltas
	if s.Count != 2 {
		t.Errorf("jitter Count = %d, want 2", s.Count)
	}
	if s.Min <= 0 {
		t.Errorf("jitter Min = %f, should be > 0", s.Min)
	}
	if s.SystemID != 1 || s.TGID != 1001 {
		t.Errorf("SystemID=%d TGID=%d, want 1/1001", s.SystemID, s.TGID)
	}
}

func TestRouterActiveStreams(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}
	router := NewAudioRouter(bus, mock, "", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	// Initially no active streams
	if n := router.ActiveStreamCount(); n != 0 {
		t.Errorf("ActiveStreamCount = %d before audio, want 0", n)
	}

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	router.Input() <- makeChunk("butco", 1001, 500)

	// Wait for the frame to be processed
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
	}

	if n := router.ActiveStreamCount(); n != 1 {
		t.Errorf("ActiveStreamCount = %d after one TG, want 1", n)
	}
}

// instanceScopedLookup implements IdentityLookup with instance-scoped cache keys.
type instanceScopedLookup struct {
	entries map[string]identityResult // key: "instanceID:shortName"
}

func (m *instanceScopedLookup) LookupByShortName(instanceID, shortName string) (systemID, siteID int, ok bool) {
	if instanceID != "" {
		r, found := m.entries[instanceID+":"+shortName]
		if !found {
			return 0, 0, false
		}
		return r.systemID, r.siteID, true
	}
	// Fallback: scan all (mirrors IdentityResolver behavior)
	for _, r := range m.entries {
		return r.systemID, r.siteID, true
	}
	return 0, 0, false
}

func (m *instanceScopedLookup) LookupByShortNameAny(shortName string, exclude map[string]bool) (systemID, siteID int, instanceID string, ok bool) {
	for key, r := range m.entries {
		// Extract instanceID from key "instanceID:shortName"
		instID := key[:len(key)-len(shortName)-1]
		if key[len(instID)+1:] != shortName {
			continue
		}
		if exclude[instID] {
			continue
		}
		return r.systemID, r.siteID, instID, true
	}
	return 0, 0, "", false
}

func TestRouterInstanceIDResolvesCorrectSystem(t *testing.T) {
	bus := NewAudioBus()
	// Two instances registered the same short_name "conv" but with different system IDs
	mock := &instanceScopedLookup{
		entries: map[string]identityResult{
			"instance-a:conv": {systemID: 2, siteID: 10},
			"instance-b:conv": {systemID: 5, siteID: 20},
		},
	}
	// Router configured with instance-a — should always resolve to system 2
	router := NewAudioRouter(bus, mock, "instance-a", 10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.Run(ctx)

	ch, unsub := bus.Subscribe(AudioFilter{})
	defer unsub()

	// Send 10 chunks — all should resolve to system 2 (not randomly to 5)
	for i := 0; i < 10; i++ {
		router.Input() <- makeChunk("conv", 3, 100)
	}

	for i := 0; i < 10; i++ {
		select {
		case frame := <-ch:
			if frame.SystemID != 2 {
				t.Errorf("frame %d: SystemID = %d, want 2 (instance-a)", i, frame.SystemID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for frame %d", i)
		}
	}
}
