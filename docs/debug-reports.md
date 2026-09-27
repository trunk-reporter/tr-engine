# Debug Reports — Developer Guide

How to set up the debug-receiver, what reports look like, and how to parse them.

## Architecture

```
User's browser (debug-report.html, with an admin API key)
  → POST /api/v1/debug-report  {client-side JSON}   (needs the admin scope)
    → tr-engine enriches with server config, health, logs
    → tr-engine forwards combined JSON to DEBUG_REPORT_URL
      → debug-receiver saves .json.gz + notifies Discord

Audio diagnostics page (audio-diagnostics.html)
  → POST directly to debug-receiver  {multipart: report JSON + PCM audio files}
    → debug-receiver saves .json.gz + audio_N.pcm + notifies Discord
```

Two report types arrive at the debug-receiver:

| Type | Source | Format | Contains Audio? |
|------|--------|--------|-----------------|
| `debug_report` | debug-report.html → tr-engine → receiver | JSON body | No |
| Audio diagnostic | audio-diagnostics.html → receiver directly | Multipart (JSON + PCM) | Yes |

## Setting Up the Debug-Receiver

### Build

```bash
# From the repo root
GOOS=linux GOARCH=amd64 go build -o debug-receiver-linux ./cmd/debug-receiver/
```

### Deploy

The debug-receiver runs on `case` in a Docker container:

```bash
# Stop, replace binary, start
ssh root@case "docker stop tr-engine-debug-receiver-1"
scp debug-receiver-linux root@case:/data/tr-engine/debug-receiver
ssh root@case "docker start tr-engine-debug-receiver-1"
```

Cannot `scp` over the binary while the container is running (file busy). Must stop first.

### Environment Variables

| Var | Default | Description |
|-----|---------|-------------|
| `LISTEN_ADDR` | `:8090` | HTTP listen address |
| `DEBUG_REPORT_DIR` | `/data/tr-engine/debug-reports` | Where `.json.gz` reports and PCM files are saved |
| `UPLOAD_DIR` | `/data/tr-engine/debug-uploads` | Where standalone file uploads are saved |
| `DISCORD_WEBHOOK_URL` | _(empty)_ | Discord webhook for notifications (pings `@drew`) |

### Caddy Routing

On `case`, Caddy reverse-proxies `https://case.luxprimatech.com/debug/*` to the debug-receiver on port 8090. The `/debug/report` path maps to the receiver's `/report` handler.

### Rate Limiting

The debug-receiver rate-limits to **1 report per IP per minute**. Duplicate submissions within the window are rejected.

## Report Types

### 1. Debug Report (`report_type: "debug_report"`)

Sent by the debug-report.html page via tr-engine's `POST /api/v1/debug-report` endpoint. The browser sends client-side data; tr-engine enriches with server-side diagnostics before forwarding.

Because it makes the server send its configuration to a third party, the endpoint needs an API key with the **`admin`** scope. Anonymous visitors and `listen`/`edit` keys get 401/403; the page asks for an admin key.

**Top-level structure:**

```json
{
  "timestamp": "2026-03-10T17:50:00Z",
  "report_type": "debug_report",
  "version": "v0.8.29-10-gecb18c6 (commit=ecb18c6, built=...)",
  "uptime_seconds": 3600,
  "client": { ... },
  "server": { ... }
}
```

**`client` section** (from the user's browser):

| Field | Type | Description |
|-------|------|-------------|
| `problem` | string | User's "What's wrong?" input |
| `additionalContext` | string | Pasted docker-compose, config, errors |
| `timestamp` | string | Client-side ISO timestamp |
| `userAgent` | string | Browser user agent |
| `platform` | string | `navigator.platform` |
| `language` | string | Browser language |
| `screen` | object | `{ width, height, pixelRatio }` |
| `page` | object | `{ url, referrer }` |
| `network` | object | `{ type, effectiveType, downlink, rtt }` (if available) |
| `audioSupport` | object | `{ state, sampleRate, baseLatency }` or `{ error }` |
| `audioEngine` | object | `{ connected, activeTGs }` (if audio-engine.js is loaded) |
| `theme` | string | Current theme key |
| `consoleErrors` | array | Last 50 `console.error` entries with timestamps |

**`server` section** (enriched by tr-engine):

| Field | Type | Description |
|-------|------|-------------|
| `config` | object | Full config with secrets redacted (see below) |
| `environment` | object | `{ hostname, in_container, go_version, go_os, go_arch, num_cpu, network_interfaces }` |
| `mqtt_connected` | bool | MQTT broker connection state (`false` on an install without MQTT: no `MQTT_BROKER_URL`; the endpoint works there too) |
| `tr_instances` | array | `[{ instance_id, status, last_seen }]` |
| `ingest_metrics` | object | `{ MsgCount, ActiveCalls, HandlerCounts, SSESubscribers }` |
| `watcher_status` | object | File watcher state (if active) |
| `transcription_status` | object | STT provider status |
| `transcription_queue` | object | Queue depth, completed/failed counts |
| `maintenance` | object | Last maintenance run results |
| `audio_stream` | object | `{ enabled, listen, active_encoders, connected_clients }` |
| `audio_jitter` | object | Per-stream jitter stats from UDP ingest |
| `database_pool` | object | pgxpool stats (max/total/acquired/idle conns) |
| `console_messages` | array | TR console warn/error messages from last hour |
| `tr_config` | object | Raw trunk-recorder `config.json` (if TR_DIR set) |

**Config redaction rules** (applied to tr-engine's config and to `tr_config`, at any depth):
- Fields whose names contain `key`, `token`, `pass`, `secret`, `auth` or `credential` (case-insensitive) → `"***"`. This covers trunk-recorder's upload plugin `apiKey`s, Broadcastify keys and MQTT plugin passwords.
- Every URL-valued string → user info and query string stripped, host/port/path preserved
- Empty secret fields → `""` (not `"***"`)

The report never contains API keys: tr-engine only stores their hashes, and access control is not part of its config.

### 2. Audio Diagnostic Report

Sent directly from audio-diagnostics.html to the debug-receiver as multipart form data. Contains detailed jitter analysis and captured PCM audio samples.

**Multipart fields:**
- `report` — JSON blob (see below)
- `audio_0.pcm`, `audio_1.pcm`, ... — Raw PCM audio captures (16-bit LE, typically 8000 Hz)

**Report JSON structure:**

```json
{
  "timestamp": "2026-03-10T17:50:00Z",
  "reportClickTime": 12345.67,
  "userAgent": "...",
  "connection": { "type", "effectiveType", "downlink", "rtt" },
  "audioContext": { "state", "sampleRate", "baseLatency" },
  "uptime": 60000,
  "totalFrames": 500,
  "totalGaps": 3,
  "totalTransmissions": 12,
  "totalUnderruns": 0,
  "clientJitter": { "<systemId:tgid>": { "min", "max", "mean", "stddev", "count" } },
  "serverJitter": { ... },
  "bufferStats": { ... },
  "transmissions": [
    {
      "key": "1:1001",
      "tgid": 1001,
      "systemId": 1,
      "startTime": 1710000000000,
      "endTime": 1710000005000,
      "duration": 5000,
      "frameCount": 250,
      "seqGaps": 0,
      "deltas": [{ "clientDelta", "serverDelta", "networkJitter", "ts" }],
      "clientStats": { "min", "max", "mean", "stddev" },
      "serverStats": { ... },
      "networkStats": { ... },
      "audio": { "file": "audio_0.pcm", "sampleRate": 8000, "samples": 40000 }
    }
  ]
}
```

## Reading Reports

Reports are gzip-compressed JSON files saved to `DEBUG_REPORT_DIR`.

### Listing reports

```bash
ssh root@case "ls -lt /data/tr-engine/debug-reports/ | head -20"
```

### Reading a report

```bash
ssh root@case "zcat /data/tr-engine/debug-reports/2026-03-10T17-50-00_192-168-1-100.json.gz | jq ."
```

### Quick triage with jq

```bash
# What's the problem?
zcat report.json.gz | jq '.client.problem'

# What version and uptime?
zcat report.json.gz | jq '{ version, uptime_seconds }'

# Is MQTT connected? How many TR instances?
zcat report.json.gz | jq '.server | { mqtt_connected, tr_instances }'

# What's the streaming config?
zcat report.json.gz | jq '.server.config | { StreamListen, StreamInstanceID, StreamSampleRate, StreamOpusBitrate }'

# Any console errors from TR?
zcat report.json.gz | jq '.server.console_messages'

# Database pool health?
zcat report.json.gz | jq '.server.database_pool'

# Docker or bare metal?
zcat report.json.gz | jq '.server.environment | { hostname, in_container, network_interfaces }'

# User's pasted context (docker-compose, etc.)?
zcat report.json.gz | jq -r '.client.additionalContext'

# Audio jitter stats?
zcat report.json.gz | jq '.server.audio_jitter'

# Full TR config?
zcat report.json.gz | jq '.server.tr_config'
```

### Playing back captured PCM audio

Audio diagnostic reports include raw PCM captures. Play with:

```bash
# Copy from server
scp root@case:/data/tr-engine/debug-reports/*audio_0.pcm ./

# Play (16-bit LE mono, usually 8000 Hz)
ffplay -f s16le -ar 8000 -ac 1 audio_0.pcm

# Convert to WAV for easier handling
ffmpeg -f s16le -ar 8000 -ac 1 -i audio_0.pcm output.wav
```

Check the report's `transmissions[].audio.sampleRate` field — it's usually 8000 (P25) but could be 16000 (analog).

## Common Debugging Scenarios

### "Audio isn't working"

1. Check `server.config.StreamListen` — should be `:9123` (with colon). Missing colon = Go can't bind.
2. Check `server.audio_stream.enabled` — false means `STREAM_LISTEN` not configured.
3. Check `server.environment.in_container` — if true, verify UDP port is exposed as `/udp` in docker-compose.
4. Check `client.additionalContext` for their docker-compose.
5. Check `server.audio_stream.active_encoders` — 0 means no UDP packets arriving.

### "Talkgroups showing on wrong system"

1. Check `server.config.StreamInstanceID` — should match the TR instance sending simplestream.
2. Check `server.tr_config.systems` — how many systems, what short_names.
3. Check `server.config.MergeP25Systems` — if true, P25 systems auto-merge by sysid/wacn.

### "Transcription not working"

1. Check `server.transcription_status` — `"not_configured"` means no STT provider set.
2. Check `server.config.STTProvider` and `server.config.WhisperURL` (URL is sanitized but host preserved).
3. Check `server.transcription_queue` — high `failed` count suggests provider issues.

### "Can't connect / 404 errors"

1. Check `client.consoleErrors` for 401/403/404 responses, and their error `code`: `key_required` (no key and anonymous access is off), `invalid_key` (revoked, expired or mistyped key, or a proxy injecting a header), `insufficient_scope`, `restricted_credential`. See [auth.md](auth.md#troubleshooting).
2. Check `client.page.url` — are they hitting the right hostname?
3. CORS is never the cause of an auth failure: tr-engine allows every origin.

### "General health check"

```bash
zcat report.json.gz | jq '{
  version,
  uptime_h: (.uptime_seconds / 3600 | floor),
  mqtt: .server.mqtt_connected,
  db_pool: .server.database_pool,
  tr_instances: [.server.tr_instances[].instance_id],
  streaming: .server.audio_stream.enabled,
  transcription: .server.transcription_status.status,
  in_docker: .server.environment.in_container,
  console_errors: (.server.console_messages | length)
}'
```

## tr-engine Configuration

Users control debug reports with two env vars:

| Var | Default | Description |
|-----|---------|-------------|
| `DEBUG_REPORT_URL` | `https://case.luxprimatech.com/debug/report` | Where to forward reports |
| `DEBUG_REPORT_DISABLE` | `false` | Set `true` to disable submissions entirely |

The endpoint `POST /api/v1/debug-report` is always registered and needs an `admin` key. When disabled, it returns 503.
