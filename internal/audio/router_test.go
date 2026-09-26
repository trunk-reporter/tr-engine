package audio

import (
	"context"
	"slices"
	"strings"
	"sync"
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

func (m *mockIdentityLookup) InstancesByShortName(shortName string) []string {
	if _, found := m.systems[shortName]; !found {
		return nil
	}
	return []string{"default"}
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

func (m *instanceScopedLookup) InstancesByShortName(shortName string) []string {
	var ids []string
	for key := range m.entries {
		instID, name, _ := strings.Cut(key, ":")
		if name == shortName {
			ids = append(ids, instID)
		}
	}
	slices.Sort(ids)
	return ids
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

	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
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

// collectFrames returns the frames that arrive on ch within d.
func collectFrames(ch <-chan AudioFrame, d time.Duration) []AudioFrame {
	var out []AudioFrame
	deadline := time.After(d)
	for {
		select {
		case f := <-ch:
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}

// A sender whose short name matches several instances is not guessed: its
// audio is dropped rather than labelled with one of the systems' IDs, which
// restrictions on live audio rely on (r1-04). A configured source map, or
// the other sender being attributed first, resolves it.
func TestRouterAmbiguousShortNameFromSender(t *testing.T) {
	lookup := &instanceScopedLookup{
		entries: map[string]identityResult{
			"tr-a:county": {systemID: 3, siteID: 30},
			"tr-b:county": {systemID: 4, siteID: 40},
			"tr-c:solo":   {systemID: 5, siteID: 50},
		},
	}
	chunkFrom := func(ip, shortName string, tgid int) AudioChunk {
		c := makeChunk(shortName, tgid, 1)
		c.SourceAddr = ip
		return c
	}

	t.Run("ambiguous sender is dropped", func(t *testing.T) {
		bus := NewAudioBus()
		router := NewAudioRouter(bus, lookup, "trunk-recorder", 10*time.Second, 0)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go router.Run(ctx)
		ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
		defer unsub()

		for i := 0; i < 5; i++ {
			router.Input() <- chunkFrom("127.0.0.4", "county", 500)
		}
		// An unambiguous short name from another sender still works.
		router.Input() <- chunkFrom("127.0.0.9", "solo", 600)
		frames := collectFrames(ch, 300*time.Millisecond)
		if len(frames) != 1 || frames[0].SystemID != 5 {
			t.Errorf("frames = %+v, want only the unambiguous system 5 one", frames)
		}
	})

	t.Run("instances sharing a short name on one system are not ambiguous", func(t *testing.T) {
		same := &instanceScopedLookup{entries: map[string]identityResult{
			"file-watch:county":     {systemID: 3, siteID: 30},
			"trunk-recorder:county": {systemID: 3, siteID: 31},
		}}
		bus := NewAudioBus()
		router := NewAudioRouter(bus, same, "trunk-recorder", 10*time.Second, 0)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go router.Run(ctx)
		ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
		defer unsub()

		router.Input() <- chunkFrom("127.0.0.4", "county", 500)
		frames := collectFrames(ch, 300*time.Millisecond)
		if len(frames) != 1 || frames[0].SystemID != 3 {
			t.Errorf("frames = %+v, want one on system 3", frames)
		}
	})

	t.Run("source map attributes the sender", func(t *testing.T) {
		bus := NewAudioBus()
		router := NewAudioRouter(bus, lookup, "trunk-recorder", 10*time.Second, 0)
		m, err := ParseSourceMap(" 127.0.0.4 = tr-b ,")
		if err != nil {
			t.Fatal(err)
		}
		router.SetSourceMap(m)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go router.Run(ctx)
		ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
		defer unsub()

		router.Input() <- chunkFrom("127.0.0.4", "county", 500)
		// tr-b is claimed, so the other county sender can only be tr-a.
		router.Input() <- chunkFrom("127.0.0.3", "county", 700)
		frames := collectFrames(ch, 300*time.Millisecond)
		got := map[int]int{}
		for _, f := range frames {
			got[f.TGID] = f.SystemID
		}
		if len(frames) != 2 || got[500] != 4 || got[700] != 3 {
			t.Errorf("frames = %+v, want tg 500 on system 4 (tr-b) and tg 700 on system 3 (tr-a)", frames)
		}
	})
}

// lockedLookup is an instanceScopedLookup that may change while the router
// runs, like the identity cache does.
type lockedLookup struct {
	mu sync.RWMutex
	instanceScopedLookup
}

func (m *lockedLookup) LookupByShortName(instanceID, shortName string) (int, int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instanceScopedLookup.LookupByShortName(instanceID, shortName)
}

func (m *lockedLookup) InstancesByShortName(shortName string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instanceScopedLookup.InstancesByShortName(shortName)
}

func (m *lockedLookup) add(key string, r identityResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = r
}

// runRouter starts a router over lookup with the default ingest-only
// instances, subscribed to everything.
func runRouter(t *testing.T, lookup IdentityLookup, configure func(r *AudioRouter)) (*AudioRouter, <-chan AudioFrame) {
	t.Helper()
	bus := NewAudioBus()
	router := NewAudioRouter(bus, lookup, "trunk-recorder", 10*time.Second, 0)
	router.SetIngestOnlyInstances("file-watch", "http-upload")
	if configure != nil {
		configure(router)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go router.Run(ctx)
	ch, unsub := bus.Subscribe(AudioFilter{}, unrestricted())
	t.Cleanup(unsub)
	return router, ch
}

func senderChunk(ip, shortName string, tgid int) AudioChunk {
	c := makeChunk(shortName, tgid, 1)
	c.SourceAddr = ip
	return c
}

// systemsByTG maps each frame's talkgroup to the systems it arrived on.
func systemsByTG(frames []AudioFrame) map[int][]int {
	out := map[int][]int{}
	for _, f := range frames {
		if !slices.Contains(out[f.TGID], f.SystemID) {
			out[f.TGID] = append(out[f.TGID], f.SystemID)
		}
	}
	return out
}

// File-watch / TR_DIR and upload identities share short names with the
// trunk-recorder instances whose calls they ingest, on systems of their own,
// but never send simplestream audio: a real instance wins over them, the
// watch instance over the upload instance, and one alone is used (r2-08).
func TestRouterIngestOnlyInstances(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries map[string]identityResult
		want    int
	}{
		{"trunk-recorder and file watch", map[string]identityResult{
			"tr-1:livesys": {systemID: 2, siteID: 20}, "file-watch:livesys": {systemID: 1, siteID: 10}}, 2},
		{"trunk-recorder and upload", map[string]identityResult{
			"tr-1:livesys": {systemID: 2, siteID: 20}, "http-upload:livesys": {systemID: 7, siteID: 70}}, 2},
		{"all three", map[string]identityResult{"tr-1:livesys": {systemID: 2, siteID: 20},
			"file-watch:livesys": {systemID: 1, siteID: 10}, "http-upload:livesys": {systemID: 7, siteID: 70}}, 2},
		{"file watch only", map[string]identityResult{"file-watch:livesys": {systemID: 1, siteID: 10}}, 1},
		{"file watch and upload", map[string]identityResult{
			"file-watch:livesys": {systemID: 1, siteID: 10}, "http-upload:livesys": {systemID: 7, siteID: 70}}, 1},
		{"upload only", map[string]identityResult{"http-upload:livesys": {systemID: 7, siteID: 70}}, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			router, ch := runRouter(t, &instanceScopedLookup{entries: c.entries}, nil)
			router.Input() <- senderChunk("127.0.0.3", "livesys", 500)
			router.Input() <- senderChunk("127.0.0.4", "livesys", 501)
			got := systemsByTG(collectFrames(ch, 300*time.Millisecond))
			if len(got) != 2 || !slices.Equal(got[500], []int{c.want}) || !slices.Equal(got[501], []int{c.want}) {
				t.Errorf("systems by talkgroup = %v, want both on system %d", got, c.want)
			}
		})
	}
}

// An attribution is derived again for every chunk: when an instance with
// the same short name on another system appears after a sender was
// attributed, the sender is no longer guessed, and nothing is labelled with
// the wrong system; STREAM_SOURCE_MAP then attributes both (r2-09).
func TestRouterReattributesWhenInstancesChange(t *testing.T) {
	lookup := &lockedLookup{instanceScopedLookup: instanceScopedLookup{entries: map[string]identityResult{
		"tr-a:county": {systemID: 3, siteID: 30},
	}}}
	router, ch := runRouter(t, lookup, nil)

	router.Input() <- senderChunk("10.0.0.2", "county", 501)
	if got := systemsByTG(collectFrames(ch, 300*time.Millisecond)); !slices.Equal(got[501], []int{3}) {
		t.Fatalf("before tr-b: %v, want tg 501 on system 3", got)
	}

	lookup.add("tr-b:county", identityResult{systemID: 4, siteID: 40})
	for i := 0; i < 3; i++ {
		router.Input() <- senderChunk("10.0.0.2", "county", 501)
		router.Input() <- senderChunk("10.0.0.1", "county", 502)
	}
	if frames := collectFrames(ch, 300*time.Millisecond); len(frames) != 0 {
		t.Errorf("after tr-b appeared: frames %v, want none (the senders can't be told apart)", systemsByTG(frames))
	}

	// Mapping one sender attributes it, and the other by elimination.
	router.SetSourceMap(map[string]string{"10.0.0.2": "tr-b"})
	router.Input() <- senderChunk("10.0.0.2", "county", 501)
	router.Input() <- senderChunk("10.0.0.1", "county", 502)
	got := systemsByTG(collectFrames(ch, 300*time.Millisecond))
	if !slices.Equal(got[501], []int{4}) || !slices.Equal(got[502], []int{3}) {
		t.Errorf("with the source map: %v, want tg 501 on system 4 (tr-b) and tg 502 on system 3 (tr-a)", got)
	}
}

func TestParseSourceMap(t *testing.T) {
	m, err := ParseSourceMap("10.0.0.5=tr-1, ::ffff:10.0.0.6=tr-2,fe80::1=tr-3")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"10.0.0.5": "tr-1", "10.0.0.6": "tr-2", "fe80::1": "tr-3"}
	if len(m) != len(want) {
		t.Fatalf("ParseSourceMap = %v, want %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("ParseSourceMap[%s] = %q, want %q", k, m[k], v)
		}
	}
	for _, bad := range []string{"10.0.0.5", "10.0.0.5=", "host=tr-1", "10.0.0.5:tr-1"} {
		if _, err := ParseSourceMap(bad); err == nil {
			t.Errorf("ParseSourceMap(%q) accepted", bad)
		}
	}
}
