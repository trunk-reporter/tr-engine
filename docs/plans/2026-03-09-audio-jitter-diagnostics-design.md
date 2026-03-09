# Audio Jitter Diagnostics

## Problem

Users (e.g. gofaster) experience audio dropouts when TR and tr-engine run on different hosts. UDP jitter from TR's simplestream plugin can spike 100ms+, but there's no visibility into where jitter is introduced (TR→server UDP, server processing, server→client WebSocket) or tools for users to send diagnostic data back to the developer.

## Architecture

Three layers of jitter measurement, always-on, zero-cost when not observed:

```
TR (simplestream UDP) → [1. Server receive jitter] → router/encoder → WS frame
  → [2. Server→client WS jitter] → AudioEngine → [3. End-to-end client jitter]
```

### 1. Server-Side Jitter Tracking (router.go)

Per active stream (`systemID:tgid`), track inter-chunk arrival deltas using `AudioChunk.Timestamp` (set to `time.Now()` in `simplestream.go` at UDP read time — this is the true server receive time).

Stats per stream (Welford's online algorithm):
- Count, min, max, mean, variance/stddev
- Reset on stream idle→active transition (reuse existing `idleTimeout` logic)

Exposed via `GET /api/v1/audio/jitter`:
```json
{
  "streams": {
    "1:9173": {
      "tgid": 9173,
      "system_id": 1,
      "count": 342,
      "min_ms": 18.2,
      "max_ms": 147.3,
      "mean_ms": 20.1,
      "stddev_ms": 8.4,
      "last_delta_ms": 19.8,
      "started_at": "2026-03-09T14:30:00Z"
    }
  }
}
```

No binary protocol changes. The existing WS frame timestamp (offset 6, `time.Since(connStart)`) serves as server send time — the client uses it to isolate WS jitter.

### 2. Client-Side Jitter Tracking (AudioEngine)

Always-on in `_handleBinaryFrame()`. Per TG key, on each frame:

- `clientDelta` = `performance.now() - prevClientTime` (wall-clock inter-frame)
- `serverDelta` = `serverTs - prevServerTs` (from frame header offset 6)
- `networkJitter` = `clientDelta - serverDelta` (WS/network contribution)
- Seq gap detection from frame header offset 10

Data structures:
- Running stats (min/max/mean/count) per TG — same Welford's approach
- Circular buffer of last 500 deltas per TG (for real-time plotting)
- Transmission segmentation: gap > 500ms = new transmission

On transmission end (silence gap detected), snapshot into transmission log:
- TG key, start/end time, duration
- Frame count, seq gaps
- Client jitter summary (min/max/mean/stddev)
- Server delta summary
- Network jitter summary
- Full delta array (for sparkline/plot reconstruction)

Keep last 100 completed transmissions.

New AudioEngine methods:
- `getJitterStats()` → per-TG live stats + circular buffer
- `getTransmissionLog()` → completed transmission summaries

### 3. Diagnostic Page (audio-diagnostics.html)

Standalone page. Connects via AudioEngine to the same WS as scanner.

**Live transmission table**: Each row = one transmission (active or completed).
| TG | Alpha Tag | Duration | Frames | Seq Gaps | Client Jitter (min/avg/max) | Server Jitter | Network Jitter |
Active transmissions update in real-time; completed ones freeze in place.

**Real-time jitter plot per transmission**: Canvas-based scatter/line plot.
- X axis: frame index or elapsed time within transmission
- Y axis: inter-frame delta in ms
- Flat line at ~20ms (expected for 8kHz/160-sample chunks) with spikes visible
- Active transmissions plot live; completed transmissions keep their frozen chart
- Expandable from each transmission row, or inline mini-sparkline

**Summary cards**: Connection uptime, total frames, total drops, overall jitter percentiles.

**"Send Debug Report" button**: Collects:
- Last N completed transmissions with full timing arrays
- Server-side jitter stats (fetched from `GET /api/v1/audio/jitter`)
- Browser UA, connection type, AudioContext state, WS latency
- POSTs JSON to debug report receiver

### 4. Debug Report Receiver (case)

Standalone tiny HTTP server on case:
- Listens on a local port (e.g. 8090), proxied via Caddy
- `POST /report` — accepts JSON body, writes timestamped file to `/data/tr-engine/debug-reports/`
- Rate-limited: 1 req/min per IP
- No auth — payload is diagnostic data only
- CORS: allows all origins (users POST from their own tr-engine domains)

Diagnostic page has `DEBUG_REPORT_URL` default pointing to the case receiver. Users see a clear label that data is sent externally.

## Files to Create/Modify

| File | Change |
|------|--------|
| `internal/audio/router.go` | Add jitter stats to `activeStream`, compute on each chunk |
| `internal/audio/jitter.go` | New file: `JitterStats` struct with Welford's algorithm |
| `internal/api/audio_stream.go` | Add `GET /api/v1/audio/jitter` handler |
| `web/audio-engine.js` | Add jitter tracking in `_handleBinaryFrame`, transmission log, new methods |
| `web/audio-diagnostics.html` | New page: transmission table, real-time jitter plots, send button |
| `cmd/debug-receiver/main.go` | New: tiny HTTP server for case |
| `openapi.yaml` | Add `/audio/jitter` endpoint spec |
