# Audio Jitter Diagnostics Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add always-on jitter measurement at both server and client, a diagnostic page showing per-transmission real-time jitter plots, and a "Send Debug Report" button that ships data to a receiver on case.

**Architecture:** Server-side Welford's stats in `router.go` per active stream, exposed via REST. Client-side jitter tracking in `AudioEngine._handleBinaryFrame()` with transmission segmentation. Standalone diagnostic page with canvas jitter plots. Tiny standalone receiver binary on case.

**Tech Stack:** Go (server jitter + REST + receiver), vanilla JS/Canvas (client jitter + diagnostic page)

---

### Task 1: JitterStats struct (Welford's algorithm)

**Files:**
- Create: `internal/audio/jitter.go`
- Create: `internal/audio/jitter_test.go`

**Step 1: Write the failing test**

In `internal/audio/jitter_test.go`:

```go
package audio

import (
	"math"
	"testing"
)

func TestJitterStatsEmpty(t *testing.T) {
	js := &JitterStats{}
	if js.Count != 0 {
		t.Errorf("Count = %d, want 0", js.Count)
	}
	if js.Mean() != 0 {
		t.Error("Mean should be 0 for empty stats")
	}
	if js.Stddev() != 0 {
		t.Error("Stddev should be 0 for empty stats")
	}
}

func TestJitterStatsAccumulate(t *testing.T) {
	js := &JitterStats{}
	// Simulate 5 deltas: 20, 20, 20, 100, 20 ms
	deltas := []float64{20, 20, 20, 100, 20}
	for _, d := range deltas {
		js.Add(d)
	}

	if js.Count != 5 {
		t.Errorf("Count = %d, want 5", js.Count)
	}
	if js.Min != 20 {
		t.Errorf("Min = %f, want 20", js.Min)
	}
	if js.Max != 100 {
		t.Errorf("Max = %f, want 100", js.Max)
	}
	expectedMean := 36.0
	if math.Abs(js.Mean()-expectedMean) > 0.01 {
		t.Errorf("Mean = %f, want %f", js.Mean(), expectedMean)
	}
	if js.Stddev() <= 0 {
		t.Error("Stddev should be positive for varied data")
	}
}

func TestJitterStatsReset(t *testing.T) {
	js := &JitterStats{}
	js.Add(20)
	js.Add(30)
	js.Reset()
	if js.Count != 0 {
		t.Errorf("Count after reset = %d, want 0", js.Count)
	}
	if js.Min != 0 || js.Max != 0 {
		t.Error("Min/Max should be 0 after reset")
	}
}

func TestJitterStatsSnapshot(t *testing.T) {
	js := &JitterStats{}
	js.Add(20)
	js.Add(30)
	snap := js.Snapshot()
	// Snapshot is a copy — mutating original doesn't affect it
	js.Add(1000)
	if snap.Count != 2 {
		t.Errorf("Snapshot Count = %d, want 2", snap.Count)
	}
	if snap.Max != 30 {
		t.Errorf("Snapshot Max = %f, want 30", snap.Max)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/audio/ -run TestJitterStats -v`
Expected: FAIL — `JitterStats` not defined.

**Step 3: Write the implementation**

In `internal/audio/jitter.go`:

```go
package audio

import "math"

// JitterStats tracks inter-packet arrival jitter using Welford's online algorithm.
// Not safe for concurrent use — callers must synchronize externally.
type JitterStats struct {
	Count    int     // number of deltas recorded
	Min      float64 // minimum delta (ms)
	Max      float64 // maximum delta (ms)
	Last     float64 // most recent delta (ms)
	mean     float64 // running mean (Welford's M)
	m2       float64 // running sum of squared deviations (Welford's S)
}

// Add records a new inter-packet delta in milliseconds.
func (js *JitterStats) Add(deltaMs float64) {
	js.Count++
	js.Last = deltaMs
	if js.Count == 1 {
		js.Min = deltaMs
		js.Max = deltaMs
		js.mean = deltaMs
		js.m2 = 0
		return
	}
	if deltaMs < js.Min {
		js.Min = deltaMs
	}
	if deltaMs > js.Max {
		js.Max = deltaMs
	}
	// Welford's online algorithm
	delta := deltaMs - js.mean
	js.mean += delta / float64(js.Count)
	delta2 := deltaMs - js.mean
	js.m2 += delta * delta2
}

// Mean returns the running mean delta in ms.
func (js *JitterStats) Mean() float64 {
	return js.mean
}

// Variance returns the population variance.
func (js *JitterStats) Variance() float64 {
	if js.Count < 2 {
		return 0
	}
	return js.m2 / float64(js.Count)
}

// Stddev returns the population standard deviation.
func (js *JitterStats) Stddev() float64 {
	return math.Sqrt(js.Variance())
}

// Reset clears all accumulated stats.
func (js *JitterStats) Reset() {
	*js = JitterStats{}
}

// Snapshot returns a copy of the current stats.
func (js *JitterStats) Snapshot() JitterStats {
	return *js
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/audio/ -run TestJitterStats -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/audio/jitter.go internal/audio/jitter_test.go
git commit -m "feat(audio): add JitterStats with Welford's online algorithm"
```

---

### Task 2: Wire jitter tracking into AudioRouter

**Files:**
- Modify: `internal/audio/router.go` (add jitter fields to `activeStream`, compute in `processChunk`, expose via `GetJitterStats`)
- Modify: `internal/audio/router_test.go` (add jitter test)

**Step 1: Write the failing test**

Append to `internal/audio/router_test.go`:

```go
func TestRouterTracksJitter(t *testing.T) {
	bus := NewAudioBus()
	mock := &mockIdentityLookup{
		systems: map[string]identityResult{
			"butco": {systemID: 1, siteID: 10},
		},
	}
	router := NewAudioRouter(bus, mock, 10*time.Second, 0)

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
	// After 3 chunks, we should have 2 deltas (inter-chunk intervals)
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/audio/ -run TestRouterTracksJitter -v`
Expected: FAIL — `GetJitterStats` not defined.

**Step 3: Implement jitter tracking in router**

In `internal/audio/router.go`:

Add `jitter` and `startedAt` fields to `activeStream`:

```go
type activeStream struct {
	systemID  int
	siteID    int
	shortName string
	lastChunk time.Time
	seq       uint16
	jitter    JitterStats
	startedAt time.Time
}
```

Add a `StreamJitterSnapshot` type for the API response:

```go
// StreamJitterSnapshot is a point-in-time snapshot of jitter stats for one stream.
type StreamJitterSnapshot struct {
	SystemID  int       `json:"system_id"`
	TGID      int       `json:"tgid"`
	Count     int       `json:"count"`
	Min       float64   `json:"min_ms"`
	Max       float64   `json:"max_ms"`
	Mean      float64   `json:"mean_ms"`
	Stddev    float64   `json:"stddev_ms"`
	Last      float64   `json:"last_delta_ms"`
	StartedAt time.Time `json:"started_at"`
}
```

In `processChunk`, after updating `stream.lastChunk`, compute jitter delta. The key change is in the `exists` branch for same-site chunks:

```go
// Same site — update timestamp and increment sequence.
delta := chunk.Timestamp.Sub(stream.lastChunk)
stream.lastChunk = now
stream.seq++
stream.jitter.Add(float64(delta.Microseconds()) / 1000.0) // ms with sub-ms precision
```

For new streams, set `startedAt`:

```go
stream = &activeStream{
	systemID:  systemID,
	siteID:    siteID,
	shortName: chunk.ShortName,
	lastChunk: now,
	seq:       0,
	startedAt: now,
}
```

For site takeover (stale stream), reset jitter:

```go
stream.siteID = siteID
stream.shortName = chunk.ShortName
stream.lastChunk = now
stream.seq = 0
stream.jitter.Reset()
stream.startedAt = now
```

Add `GetJitterStats()` method:

```go
// GetJitterStats returns a snapshot of jitter stats for all active streams.
func (r *AudioRouter) GetJitterStats() map[string]StreamJitterSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]StreamJitterSnapshot, len(r.activeStreams))
	for key, stream := range r.activeStreams {
		snap := stream.jitter.Snapshot()
		result[key] = StreamJitterSnapshot{
			SystemID:  stream.systemID,
			TGID:      extractTGID(key),
			Count:     snap.Count,
			Min:       snap.Min,
			Max:       snap.Max,
			Mean:      snap.Mean(),
			Stddev:    snap.Stddev(),
			Last:      snap.Last,
			StartedAt: stream.startedAt,
		}
	}
	return result
}
```

Add helper `extractTGID`:

```go
func extractTGID(key string) int {
	// key is "systemID:tgid"
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			v := 0
			for _, c := range key[i+1:] {
				v = v*10 + int(c-'0')
			}
			return v
		}
	}
	return 0
}
```

**Important:** The delta must use `chunk.Timestamp` (UDP receive time), not `now` (processing time). Update `processChunk` to compute delta _before_ overwriting `stream.lastChunk`. Currently `stream.lastChunk` is set to `now` (wall-clock at processing time). Change to use `chunk.Timestamp` for jitter calculation:

```go
// Same site — compute jitter from receive timestamps, then update.
delta := chunk.Timestamp.Sub(stream.lastChunk)
stream.lastChunk = chunk.Timestamp  // use receive time, not processing time
stream.seq++
if delta > 0 && delta < 10*time.Second { // sanity bound
	stream.jitter.Add(float64(delta.Microseconds()) / 1000.0)
}
```

Also update new stream creation and the `now` references: for jitter purposes, `stream.lastChunk` should track `chunk.Timestamp` (receive time). The `now` variable is still used for idle timeout checks in dedup (that should still use wall-clock time). So keep `now` for dedup comparisons but use `chunk.Timestamp` for `lastChunk`:

```go
stream.lastChunk = chunk.Timestamp
```

And in the idle check for dedup, compare against `now` using a separate field or just `time.Now()`. Actually, `chunk.Timestamp` and `time.Now()` are nearly identical (microseconds apart), so using `chunk.Timestamp` for `lastChunk` is fine for both idle checks and jitter. The idle timeout is 30s — a few microseconds of skew is irrelevant.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/audio/ -run TestRouterTracksJitter -v`
Expected: PASS

**Step 5: Run all audio tests**

Run: `go test ./internal/audio/ -v`
Expected: All PASS

**Step 6: Commit**

```bash
git add internal/audio/router.go internal/audio/router_test.go
git commit -m "feat(audio): track per-stream jitter stats in router"
```

---

### Task 3: REST endpoint for jitter stats

**Files:**
- Modify: `internal/api/live_data.go` (add `AudioJitterStats` to `AudioStreamer` interface)
- Modify: `internal/api/audio_stream.go` (add `GetJitterStats` handler + route)
- Modify: `internal/ingest/pipeline.go` (implement new interface method)

**Step 1: Add method to AudioStreamer interface**

In `internal/api/live_data.go`, add to the `AudioStreamer` interface:

```go
type AudioStreamer interface {
	SubscribeAudio(filter audio.AudioFilter) (<-chan audio.AudioFrame, func())
	UpdateAudioFilter(ch <-chan audio.AudioFrame, filter audio.AudioFilter)
	AudioStreamEnabled() bool
	AudioStreamStatus() *AudioStreamStatusData
	AudioJitterStats() map[string]audio.StreamJitterSnapshot // new
}
```

**Step 2: Implement on Pipeline**

In `internal/ingest/pipeline.go`, add:

```go
// AudioJitterStats returns per-stream jitter stats from the audio router.
func (p *Pipeline) AudioJitterStats() map[string]audio.StreamJitterSnapshot {
	if p.audioRouter == nil {
		return nil
	}
	return p.audioRouter.GetJitterStats()
}
```

**Step 3: Add handler and route**

In `internal/api/audio_stream.go`, add the handler:

```go
// GetJitterStats returns per-stream audio jitter statistics.
func (h *AudioStreamHandler) GetJitterStats(w http.ResponseWriter, r *http.Request) {
	if h.streamer == nil || !h.streamer.AudioStreamEnabled() {
		WriteError(w, http.StatusNotFound, "live audio streaming is not enabled")
		return
	}
	stats := h.streamer.AudioJitterStats()
	if stats == nil {
		stats = make(map[string]audio.StreamJitterSnapshot)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"streams": stats})
}
```

Update `Routes` in the same file:

```go
func (h *AudioStreamHandler) Routes(r chi.Router) {
	r.Get("/audio/live", h.HandleStream)
	r.Get("/audio/jitter", h.GetJitterStats)
}
```

**Step 4: Build to verify compilation**

Run: `go build ./...`
Expected: Compiles cleanly.

**Step 5: Commit**

```bash
git add internal/api/live_data.go internal/api/audio_stream.go internal/ingest/pipeline.go
git commit -m "feat(api): add GET /audio/jitter endpoint for server-side jitter stats"
```

---

### Task 4: Client-side jitter tracking in AudioEngine

**Files:**
- Modify: `web/audio-engine.js`

**Step 1: Add jitter tracking data structures**

In `AudioEngine` constructor, add after `this._autoPan = true;`:

```javascript
this._jitterTracking = new Map();  // key -> { prevClientTime, prevServerTs, stats, deltas, transmission }
this._transmissionLog = [];        // completed transmission summaries
this._maxTransmissions = 100;      // keep last N
this._transmissionGapMs = 500;     // gap threshold for new transmission
this._maxDeltas = 500;             // circular buffer size per TG
```

**Step 2: Add Welford's stats helper**

Add as a method on AudioEngine (or a standalone class — keep it simple as inline):

```javascript
_newJitterEntry() {
  return {
    prevClientTime: 0,
    prevServerTs: 0,
    stats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
    serverStats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
    networkStats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
    deltas: [],        // circular buffer of { clientDelta, serverDelta, networkJitter, ts }
    deltaIdx: 0,
    transmission: null // current active transmission
  };
}

_addJitterSample(stats, value) {
  stats.count++;
  stats.last = value;
  if (stats.count === 1) {
    stats.min = value;
    stats.max = value;
    stats.mean = value;
    stats.m2 = 0;
    return;
  }
  if (value < stats.min) stats.min = value;
  if (value > stats.max) stats.max = value;
  var delta = value - stats.mean;
  stats.mean += delta / stats.count;
  var delta2 = value - stats.mean;
  stats.m2 += delta * delta2;
}

_jitterStddev(stats) {
  if (stats.count < 2) return 0;
  return Math.sqrt(stats.m2 / stats.count);
}

_resetJitterStats(stats) {
  stats.count = 0;
  stats.min = Infinity;
  stats.max = 0;
  stats.mean = 0;
  stats.m2 = 0;
  stats.last = 0;
}
```

**Step 3: Add jitter measurement in `_handleBinaryFrame`**

In `_handleBinaryFrame`, after parsing the header and before the format detection, add jitter tracking. The server timestamp is at offset 6 (uint32 BE, ms since connection start) and seq is at offset 10 (uint16 BE):

```javascript
var serverTs = view.getUint32(6);
var seq = view.getUint16(10);

// --- Jitter tracking ---
var now = performance.now();
var entry = this._jitterTracking.get(key);
if (!entry) {
  entry = this._newJitterEntry();
  this._jitterTracking.set(key, entry);
}

// Detect transmission boundary
if (entry.prevClientTime > 0) {
  var gap = now - entry.prevClientTime;
  if (gap > this._transmissionGapMs && entry.transmission) {
    // End current transmission — snapshot and archive
    entry.transmission.endTime = entry.prevClientTime;
    entry.transmission.duration = entry.transmission.endTime - entry.transmission.startTime;
    this._transmissionLog.push(entry.transmission);
    if (this._transmissionLog.length > this._maxTransmissions) {
      this._transmissionLog.shift();
    }
    this.emit('transmission_end', entry.transmission);
    entry.transmission = null;
    // Reset per-transmission stats
    this._resetJitterStats(entry.stats);
    this._resetJitterStats(entry.serverStats);
    this._resetJitterStats(entry.networkStats);
    entry.deltas = [];
    entry.deltaIdx = 0;
  }
}

// Start new transmission if needed
if (!entry.transmission) {
  entry.transmission = {
    key: key,
    tgid: tgid,
    systemId: systemId,
    startTime: now,
    endTime: 0,
    duration: 0,
    frameCount: 0,
    seqGaps: 0,
    lastSeq: seq,
    deltas: [],
    clientStats: null,
    serverStats: null,
    networkStats: null,
  };
  this.emit('transmission_start', entry.transmission);
}

entry.transmission.frameCount++;

// Seq gap detection
if (entry.transmission.frameCount > 1) {
  var expectedSeq = (entry.transmission.lastSeq + 1) & 0xFFFF;
  if (seq !== expectedSeq) {
    entry.transmission.seqGaps++;
  }
}
entry.transmission.lastSeq = seq;

// Compute deltas (skip first frame — no previous to compare)
if (entry.prevClientTime > 0 && entry.prevServerTs > 0) {
  var clientDelta = now - entry.prevClientTime;
  var serverDelta = serverTs - entry.prevServerTs;
  var networkJitter = clientDelta - serverDelta;

  this._addJitterSample(entry.stats, clientDelta);
  this._addJitterSample(entry.serverStats, serverDelta);
  this._addJitterSample(entry.networkStats, networkJitter);

  var sample = { clientDelta: clientDelta, serverDelta: serverDelta, networkJitter: networkJitter, ts: now };
  if (entry.deltas.length < this._maxDeltas) {
    entry.deltas.push(sample);
  } else {
    entry.deltas[entry.deltaIdx] = sample;
    entry.deltaIdx = (entry.deltaIdx + 1) % this._maxDeltas;
  }

  // Also accumulate into the transmission's delta array (unbounded for the transmission lifetime)
  entry.transmission.deltas.push(sample);

  this.emit('jitter_sample', { key: key, sample: sample });
}

entry.prevClientTime = now;
entry.prevServerTs = serverTs;
// --- End jitter tracking ---
```

**Step 4: Add public methods**

```javascript
getJitterStats() {
  var result = {};
  var self = this;
  this._jitterTracking.forEach(function(entry, key) {
    result[key] = {
      client: {
        count: entry.stats.count,
        min: entry.stats.count > 0 ? entry.stats.min : 0,
        max: entry.stats.max,
        mean: entry.stats.mean,
        stddev: self._jitterStddev(entry.stats),
        last: entry.stats.last
      },
      server: {
        count: entry.serverStats.count,
        min: entry.serverStats.count > 0 ? entry.serverStats.min : 0,
        max: entry.serverStats.max,
        mean: entry.serverStats.mean,
        stddev: self._jitterStddev(entry.serverStats),
        last: entry.serverStats.last
      },
      network: {
        count: entry.networkStats.count,
        min: entry.networkStats.count > 0 ? entry.networkStats.min : 0,
        max: entry.networkStats.max,
        mean: entry.networkStats.mean,
        stddev: self._jitterStddev(entry.networkStats),
        last: entry.networkStats.last
      },
      deltas: entry.deltas.slice(),
      activeTransmission: entry.transmission
    };
  });
  return result;
}

getTransmissionLog() {
  return this._transmissionLog.slice();
}
```

**Step 5: Verify AudioEngine still loads**

Test manually by opening scanner.html in browser with dev tools open — no JS errors.

**Step 6: Commit**

```bash
git add web/audio-engine.js
git commit -m "feat(audio-engine): add always-on jitter tracking and transmission log"
```

---

### Task 5: Debug report receiver (standalone binary)

**Files:**
- Create: `cmd/debug-receiver/main.go`

**Step 1: Write the receiver**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	outputDir  = "/data/tr-engine/debug-reports"
	listenAddr = ":8090"
	maxBody    = int64(1 << 20) // 1 MB
)

// Simple per-IP rate limiter: 1 request per minute
type rateLimiter struct {
	mu    sync.Mutex
	times map[string]time.Time
}

func (rl *rateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	last, ok := rl.times[ip]
	if ok && time.Since(last) < time.Minute {
		return false
	}
	rl.times[ip] = time.Now()
	return true
}

func extractIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func main() {
	if dir := os.Getenv("DEBUG_REPORT_DIR"); dir != "" {
		outputDir = dir
	}
	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		listenAddr = addr
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("failed to create output dir %s: %v", outputDir, err)
	}

	rl := &rateLimiter{times: make(map[string]time.Time)}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ip := extractIP(r)
		if !rl.Allow(ip) {
			http.Error(w, "rate limited — try again in 1 minute", http.StatusTooManyRequests)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		// Validate it's JSON
		if !json.Valid(body) {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		// Write to file
		ts := time.Now().UTC().Format("2006-01-02T15-04-05")
		filename := fmt.Sprintf("%s_%s.json", ts, strings.ReplaceAll(ip, ":", "-"))
		path := filepath.Join(outputDir, filename)

		if err := os.WriteFile(path, body, 0644); err != nil {
			log.Printf("failed to write report: %v", err)
			http.Error(w, "write error", http.StatusInternalServerError)
			return
		}

		log.Printf("saved report from %s → %s (%d bytes)", ip, filename, len(body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	log.Printf("debug-receiver listening on %s, writing to %s", listenAddr, outputDir)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
```

**Step 2: Build to verify compilation**

Run: `go build ./cmd/debug-receiver/`
Expected: Compiles cleanly.

**Step 3: Commit**

```bash
git add cmd/debug-receiver/main.go
git commit -m "feat: add standalone debug report receiver for case"
```

---

### Task 6: Diagnostic page (audio-diagnostics.html)

**Files:**
- Create: `web/audio-diagnostics.html`

This is the largest task. The page needs:

1. Theme engine integration (standard tr-engine page boilerplate)
2. AudioEngine connection + subscription controls
3. Live transmission table (active + completed)
4. Per-transmission real-time jitter plot (canvas)
5. Summary cards
6. Send Debug Report button

**Step 1: Create the page with boilerplate + AudioEngine connection**

Use the standard page template from CLAUDE.md. Include `auth.js`, `theme-config.js`, `theme-engine.js`, `audio-engine.js`. Add card meta tags. Subscribe to all TGs/systems (diagnostics should see everything).

The page structure:

```
┌─────────────────────────────────────────────┐
│ Summary cards (uptime, frames, drops, etc.) │
├─────────────────────────────────────────────┤
│ [Send Debug Report]  [Subscribe controls]   │
├─────────────────────────────────────────────┤
│ Active Transmissions                        │
│ ┌─ TG 9173 ──── 4.2s ── 210 frames ──────┐ │
│ │ [===== jitter scatter plot ===========] │ │
│ │ client: 20/21/147ms  server: 20/20/98  │ │
│ └─────────────────────────────────────────┘ │
│ ┌─ TG 1234 ──── 1.8s ── 90 frames ───────┐ │
│ │ [===== jitter scatter plot ===========] │ │
│ └─────────────────────────────────────────┘ │
├─────────────────────────────────────────────┤
│ Completed Transmissions (scrollable log)    │
│ Row: TG | Duration | Frames | Gaps | Jitter│
│ [expandable sparkline per row]              │
└─────────────────────────────────────────────┘
```

Key implementation details:

- **Jitter plot**: Canvas element per active transmission. X = elapsed time (ms), Y = inter-frame delta (ms). Draw a horizontal reference line at the expected delta (~20ms for 8kHz/160-sample). Points above are late, below are early. Color code: green < 30ms, yellow 30-60ms, red > 60ms.

- **Active transmissions**: Listen to `transmission_start` and `jitter_sample` events from AudioEngine. On `transmission_start`, create a new card with canvas. On `jitter_sample`, append point to canvas. On `transmission_end`, freeze the card and move to completed section.

- **Completed transmissions**: Table with sparkline. Sparkline is a tiny canvas (200x30) drawn from the transmission's `deltas` array on `transmission_end`.

- **Send Debug Report**: On click, collect `engine.getTransmissionLog()`, fetch `GET /api/v1/audio/jitter` for server-side stats, gather browser info (`navigator.userAgent`, `navigator.connection`, AudioContext state), POST to `DEBUG_REPORT_URL`.

- **`DEBUG_REPORT_URL`**: Read from a `<meta>` tag or hardcode default. For now, hardcode `https://case.luxprimatech.com/debug/report` (the Caddy-proxied path to the receiver).

- **Security**: Use safe DOM methods only (createElement, textContent, appendChild). No innerHTML with data. The security hook will reject innerHTML.

- **Frontend design**: Use the `frontend-design` skill aesthetic guidelines. Industrial/utilitarian tone — this is a diagnostic tool. Dark theme, monospace numbers, green/yellow/red signal colors. Compact information density.

**Step 2: Verify page loads**

Open in browser, confirm no JS errors, confirm it connects to WebSocket, confirm transmissions appear.

**Step 3: Commit**

```bash
git add web/audio-diagnostics.html
git commit -m "feat: add audio diagnostics page with real-time jitter plots"
```

---

### Task 7: OpenAPI spec update

**Files:**
- Modify: `openapi.yaml`

**Step 1: Add the `/audio/jitter` endpoint**

Add under paths:

```yaml
  /audio/jitter:
    get:
      summary: Get audio jitter stats
      description: Returns per-stream inter-packet arrival jitter statistics for all active audio streams.
      operationId: getAudioJitter
      tags: [Audio]
      responses:
        '200':
          description: Jitter statistics
          content:
            application/json:
              schema:
                type: object
                properties:
                  streams:
                    type: object
                    additionalProperties:
                      $ref: '#/components/schemas/StreamJitterSnapshot'
        '404':
          description: Live audio streaming is not enabled
```

Add to components/schemas:

```yaml
    StreamJitterSnapshot:
      type: object
      properties:
        system_id:
          type: integer
        tgid:
          type: integer
        count:
          type: integer
        min_ms:
          type: number
          format: double
        max_ms:
          type: number
          format: double
        mean_ms:
          type: number
          format: double
        stddev_ms:
          type: number
          format: double
        last_delta_ms:
          type: number
          format: double
        started_at:
          type: string
          format: date-time
```

**Step 2: Commit**

```bash
git add openapi.yaml
git commit -m "docs(openapi): add /audio/jitter endpoint spec"
```

---

### Task 8: Deploy and test end-to-end

**Step 1: Build and deploy**

```bash
bash deploy-dev.sh
```

Wait for health check to pass.

**Step 2: Deploy debug receiver on case**

```bash
GOOS=linux GOARCH=amd64 go build -o debug-receiver ./cmd/debug-receiver/
scp debug-receiver root@case:/data/tr-engine/
ssh root@case 'cd /data/tr-engine && nohup ./debug-receiver > debug-receiver.log 2>&1 &'
```

Add Caddy reverse proxy rule for `/debug/*` → `localhost:8090`:

```bash
ssh root@case "cat >> /etc/caddy/Caddyfile.d/debug.conf << 'EOF'
# in the case.luxprimatech.com block, add:
# handle /debug/* {
#     uri strip_prefix /debug
#     reverse_proxy localhost:8090
# }
EOF"
```

(Exact Caddy config TBD based on existing Caddyfile structure.)

**Step 3: Test jitter endpoint**

```bash
curl -s https://case.luxprimatech.com/api/v1/audio/jitter | python3 -m json.tool
```

Expected: JSON with `streams` object (may be empty if no active audio).

**Step 4: Test diagnostic page**

Open `https://case.luxprimatech.com/audio-diagnostics.html` in browser. Verify:
- Page loads with theme
- WebSocket connects
- When audio is active, transmissions appear with live jitter plots
- "Send Debug Report" button sends to receiver
- Check `ssh root@case ls /data/tr-engine/debug-reports/` for the saved report

**Step 5: Commit any fixups**

```bash
git add -A && git commit -m "fix: deployment adjustments for audio diagnostics"
```
