# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

tr-engine is a backend service that ingests MQTT messages from one or more [trunk-recorder](https://github.com/robotastic/trunk-recorder) instances and serves them via a REST API. It handles radio system monitoring data: calls, talkgroups, units, transcriptions, and recorder state.

Current dev/test uses 2 counties (Butler/Warren, NACs 340/34D).

## Technology Stack

- **Language**: Go
- **Database**: PostgreSQL 17+
- **MQTT**: ingests from trunk-recorder instances
- **Real-time push**: Server-Sent Events (SSE) at `GET /api/v1/events/stream` with server-side filtering (systems, sites, tgids, units, event types). Clients reconnect with `Last-Event-ID` for gapless recovery on filter changes.
- **API**: REST under `/api/v1`, defined in `openapi.yaml`

Go was chosen over Node.js for multi-core utilization and headroom at high message rates.

## Key Files

- `openapi.yaml` — Complete REST API specification (OpenAPI 3.0.3), including SSE event stream endpoint. This is the **source of truth** for API contracts. **IMPORTANT: When adding or modifying API endpoints, request/response schemas, event types, enums, or SSE payload fields, always update `openapi.yaml` to match.** This includes new unit event subtypes, new query parameters, new schema properties, and changes to enum values (e.g., `EventType`, `SystemType`, `SSEEventType`).
- `schema.sql` — PostgreSQL 18 DDL. All tables, indexes, triggers, partitioning, and helper functions. Embedded in the binary via `embed.go` and auto-applied on first startup when the database is empty. Can also be run manually with `psql -f schema.sql`.
- `.env` — Local environment config (gitignored). Contains `DATABASE_URL`, `MQTT_BROKER_URL`, credentials, and `HTTP_ADDR`. Optional for Docker — all defaults are built into `docker-compose.yml`.
- `embed.go` — Go embed directives for `web/*`, `openapi.yaml`, and `schema.sql`. Exposes `WebFiles`, `OpenAPISpec`, and `SchemaSQL` package-level variables.
- `cmd/tr-engine/main.go` — Entry point. Startup order: config → logger → database → schema init → migrations → MQTT → pipeline → HTTP server. Graceful shutdown via SIGINT/SIGTERM with 10s timeout. Version injected via `-ldflags`.
- `cmd/mqtt-dump/` — Dev tool to capture and display live MQTT traffic.
- `cmd/dbcheck/` — DB inspection tool (table counts, call group analysis, cleanup).
- `internal/config/config.go` — Env-based config (`DATABASE_URL`, `MQTT_BROKER_URL`, `HTTP_ADDR`, `AUTH_TOKEN`, `LOG_LEVEL`, timeouts). Uses `caarlos0/env/v11`.
- `internal/database/` — pgxpool wrapper (20 max / 4 min conns, 2s health-check ping) plus query files for all tables: systems, sites, talkgroups, units, calls, call_groups, recorders, stats, etc. `schema.go` handles first-run schema initialization; `migrations.go` handles incremental schema changes.
- `internal/mqttclient/client.go` — Paho MQTT client. Auto-reconnect (5s), QoS 0, `atomic.Bool` connection tracking.
- `internal/ingest/` — Complete MQTT ingestion pipeline. Message routing (`router.go`), identity resolution (`identity.go`), event bus for SSE (`eventbus.go`), batch writers (`batcher.go`), and handlers for all message types (calls, units, recorders, rates, systems, config, audio, status, trunking messages, console logs). Raw archival supports three modes: disabled (`RAW_STORE=false`), allowlist (`RAW_INCLUDE_TOPICS` with `_unknown` for unrecognized topics), or denylist (`RAW_EXCLUDE_TOPICS`). Audio messages have base64 audio data stripped before raw archival since the audio is already saved to disk.
- `internal/api/server.go` — Chi router + HTTP server lifecycle. All endpoints wired via handler `Routes()` methods.
- `internal/api/query.go` — Ad-hoc read-only SQL query handler (`POST /query`). Read-only transaction, 30s statement timeout, row cap, semicolon rejection.
- `internal/database/query.go` — `ExecuteReadOnlyQuery()` — runs SQL in a `BEGIN READ ONLY` transaction with `SET LOCAL statement_timeout = '30s'`.
- `internal/api/upload.go` — HTTP call upload handler (`POST /api/v1/call-upload`). Auto-detects rdio-scanner vs OpenMHz format from form field names. Uses `CallUploader` interface (defined in `live_data.go`) to avoid circular imports with `ingest`.
- `internal/ingest/handler_upload.go` — `ProcessUploadedCall` (full pipeline: identity resolution, dedup, call creation, audio save, SSE publish, transcription enqueue), `ProcessUpload` adapter (implements `api.CallUploader`), `ParseRdioScannerFields`, `ParseOpenMHzFields`.
- `internal/api/middleware.go` — RequestID, structured request Logger (zerolog/hlog), Recoverer (JSON 500), BearerAuth (checks `Authorization: Bearer` header or `?token=` query param; accepts both `AUTH_TOKEN` and `WRITE_TOKEN`), WriteAuth (requires `WRITE_TOKEN` for POST/PUT/PATCH/DELETE when set), UploadAuth (like BearerAuth but also accepts `key`/`api_key` multipart form fields for TR upload plugin compatibility), CORSWithOrigins, RateLimiter (per-IP via `X-Forwarded-For`/`X-Real-IP`, configurable `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`), MaxBodySize (10 MB for API, 50 MB for uploads), ResponseTimeout (wraps non-SSE/audio handlers with `HTTP_WRITE_TIMEOUT`).
- `internal/audio/simplestream.go` — UDP listener for trunk-recorder's simplestream plugin. Parses sendJSON (4-byte LE length + JSON metadata + PCM) and sendTGID (4-byte LE TGID + PCM) packet formats.
- `internal/audio/router.go` — Audio router: identity resolution (short_name → system/site), multi-site deduplication, per-talkgroup encoding, publishes to AudioBus.
- `internal/audio/bus.go` — Pub/sub event bus for audio frames. WebSocket clients subscribe with filters (system IDs, TGIDs).
- `internal/api/audio_stream.go` — WebSocket endpoint (`GET /audio/live`). Clients send JSON subscribe/unsubscribe messages; server sends binary frames (12-byte header + audio data).
- `web/audio-engine.js` — Browser-side audio playback engine. Manages WebSocket connection, audio decoding, and playback via AudioWorklet.
- `web/audio-worklet.js` — AudioWorklet processor for low-latency PCM playback in the browser.
- `web/auth.js` — Shared browser-side auth for all web pages. On load, fetches token from `GET /api/v1/auth-init` (unauthenticated, CDN-safe JSON endpoint). Patches `window.fetch` to inject `Authorization: Bearer` header and `EventSource` to append `?token=` on same-origin `/api/` calls. Falls back to localStorage prompt on 401. Included via `<script src="auth.js?v=1"></script>` in all pages.

## Go Dependencies

| Library | Purpose |
|---------|---------|
| `jackc/pgx/v5` | PostgreSQL driver + connection pool (pgxpool) |
| `go-chi/chi/v5` | HTTP router with composable middleware |
| `eclipse/paho.mqtt.golang` | MQTT client with auto-reconnect |
| `caarlos0/env/v11` | Struct-based env config parsing |
| `rs/zerolog` | Zero-allocation structured JSON logging |

## Data Model: Two-Level System/Site Hierarchy

The core concept that permeates the entire codebase:

- **System** = logical radio network. P25 identified by `(sysid, wacn)`. Conventional identified by `(instance_id, sys_name)`. Talkgroups and units belong at the system level.
- **Site** = recording point within a system. One TR `sys_name` per instance. Multiple sites can monitor the same P25 network from different locations.

```
System 1 (P25 sysid=348, wacn=BEE00)
  ├── Site 1 "butco"  (nac=340, instance=tr-1)
  ├── Site 2 "warco"  (nac=34D, instance=tr-2)
  ├── Talkgroups (shared across all sites)
  └── Units (shared across all sites)
```

Conventional systems are 1:1 with sites.

### Identity Resolution (MQTT Ingest)

- **System**: match `(sysid, wacn)` for P25/smartnet; `(instance_id, sys_name)` for conventional. System types: `p25`, `smartnet`, `conventional`, `conventionalP25`, `conventionalDMR`, `conventionalSIGMF`.
- **Site**: match `(system_id, instance_id, sys_name)` — never use `sys_num` (positional, unstable)
- Two TR instances monitoring the same P25 network auto-merge into one system with separate sites

### ID Formats (API)

- Talkgroup: `{system_id}:{tgid}` (composite) or plain `{tgid}` (409 Conflict if ambiguous)
- Unit: `{system_id}:{unit_id}` (composite) or plain `{unit_id}` (409 if ambiguous)
- Call: plain integer `call_id` (opaque auto-increment)

## Database Design Principles

- **Store everything** — even fields that seem irrelevant now. `metadata_json` JSONB catch-all on calls and unit_events captures unmapped MQTT fields.
- **Denormalize for reads** — `calls` carries `system_name`, `site_short_name`, `tg_alpha_tag`, etc. copied at write time. Avoids JOINs on the hottest query paths.
- **Monthly partitioning** on high-volume tables: `calls`, `call_frequencies`, `call_transmissions`, `unit_events`, `trunking_messages`. Weekly for `mqtt_raw_messages`.
- **Dual-write transmission/frequency data** — `calls.src_list` and `calls.freq_list` JSONB columns for API reads (no JOINs). `call_transmissions` and `call_frequencies` relational tables for ad-hoc SQL queries. `calls.unit_ids` is a denormalized `int[]` with GIN index for fast unit filtering.
- **Call groups** deduplicate recordings: `(system_id, tgid, start_time)` groups duplicate recordings from multiple sites.
- **State tables** (`recorder_snapshots`, `decode_rates`) are append-only with decimation (1/min after 1 week, 1/hour after 1 month). Latest state = `ORDER BY time DESC LIMIT 1`.
- **Audio on filesystem**, not in DB. `calls.audio_file_path` stores relative path.

### Retention Policy

| Category | Tables | Retention |
|----------|--------|-----------|
| Permanent | calls, call_frequencies, call_transmissions, unit_events, transcriptions, talkgroups, units, trunking_messages | Forever (partitioned) |
| Decimated state | recorder_snapshots, decode_rates | Full 1 week → 1/min 1 month → 1/hour |
| Crash recovery | call_active_checkpoints | 7 days |
| Raw archive | mqtt_raw_messages | 7 days |
| Logs | console_messages, plugin_statuses | 30 days |
| Audit | system_merge_log, instance_configs | Forever (low volume) |

## Schema Management

**Auto-apply on startup:** `schema.sql` is embedded in the binary. On connect, `db.InitSchema()` checks if the `systems` table exists in `pg_tables`. If missing (fresh database), it executes the full embedded schema. If present, it's a no-op. This runs before `db.Migrate()`.

**Incremental migrations** (`internal/database/migrations.go`): `db.Migrate()` runs after `InitSchema()` on every startup. Each migration has a `check` query (returns true if already applied) and idempotent `sql`. Migrations handle schema changes that post-date the initial `schema.sql` — adding columns, replacing indexes, etc. To add a new migration, append to the `migrations` slice with a `name`, `sql` (use `IF NOT EXISTS`/`IF EXISTS`), and a `check` query. On failure, `MigrationError` prints the remaining SQL for manual application by a superuser.

**Startup order:** Connect → `InitSchema` (first-run only) → `Migrate` (every startup, skips already-applied) → application boot.

**Manual apply:** `psql -f schema.sql` still works for manual setup or inspection.

The schema creates initial partitions (current month + 3 months ahead). The `create_monthly_partition()` and `create_weekly_partition()` functions handle ongoing partition creation.

## Building & Running

```bash
# Build (injects version, commit hash, and build timestamp via ldflags)
bash build.sh

# Run — auto-loads .env from current directory
./tr-engine.exe

# Override settings via CLI flags
./tr-engine.exe --listen :9090 --log-level debug

# Use a different .env file
./tr-engine.exe --env-file /path/to/production.env

# Print version
./tr-engine.exe --version

# Test health
curl http://localhost:8080/api/v1/health
```

### Configuration

Configuration is loaded in priority order: **CLI flags > environment variables > .env file > defaults**.

The `.env` file is auto-loaded from the current directory on startup (silent if missing). See `sample.env` for all available fields with descriptions.

**CLI flags:**

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--listen` | `HTTP_ADDR` | `:8080` | HTTP listen address |
| `--log-level` | `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--database-url` | `DATABASE_URL` | _(required)_ | PostgreSQL connection URL |
| `--mqtt-url` | `MQTT_BROKER_URL` | _(optional)_ | MQTT broker URL |
| `--audio-dir` | `AUDIO_DIR` | `./audio` | Audio file directory |
| `--watch-dir` | `WATCH_DIR` | — | Watch TR audio directory for new files |
| `--tr-dir` | `TR_DIR` | — | Path to trunk-recorder directory for auto-discovery |
| `--env-file` | — | `.env` | Path to .env file |
| `--version` | — | — | Print version and exit |

**Docker Compose-only settings** (used by `docker-compose.yml` variable interpolation, ignored by the binary): `POSTGRES_USER` (default `trengine`), `POSTGRES_PASSWORD` (default `trengine`), `POSTGRES_DB` (default `trengine`) — configure both the postgres container and `DATABASE_URL` in one place; `HTTP_PORT` (default `8080`), `MQTT_PORT` (default `1883`), `POSTGRES_PORT` (default `5432`) — host port mappings. Docker Compose works with zero `.env` — all defaults are built in.

Additional env-only settings: `MQTT_TOPICS` (comma-separated MQTT topic filters, default `#`; match your TR plugin's `topic`/`unit_topic`/`message_topic` prefixes with `/#` wildcards to limit subscriptions), `MQTT_INSTANCE_MAP` (comma-separated `prefix:instance_id` pairs; rewrites `instance_id` in MQTT payloads based on topic prefix — use when multiple TR instances share the default `instance_id` "trunk-recorder" to prevent identity collisions; e.g. `trdash:trdash,cpg178:cpg178`), `MQTT_CLIENT_ID`, `MQTT_USERNAME`, `MQTT_PASSWORD`, `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT`, `AUTH_TOKEN` (read-only API token; if not set, a random token is auto-generated on each startup and logged — web UI pages receive it transparently via `GET /api/v1/auth-init`), `WRITE_TOKEN` (required for write operations — POST/PATCH/PUT/DELETE require this token; if not set and auth is enabled, the API runs in read-only mode and all mutations are rejected with 403), `CORS_ORIGINS` (comma-separated allowed origins; empty = allow all `*`), `RATE_LIMIT_RPS` (per-IP requests/second, default `20`), `RATE_LIMIT_BURST` (per-IP burst size, default `40`), `RAW_STORE` (bool, default `true` — master switch to disable all raw MQTT archival), `RAW_INCLUDE_TOPICS` (comma-separated allowlist of handler names for raw archival; supports `_unknown` for unrecognized topics; takes priority over `RAW_EXCLUDE_TOPICS`), `RAW_EXCLUDE_TOPICS` (comma-separated denylist of handler names to exclude from raw archival), `WATCH_INSTANCE_ID` (instance ID for file-watched calls, default `file-watch`), `WATCH_BACKFILL_DAYS` (days of existing files to backfill on startup, default `7`; `0` = all, `-1` = none), `CSV_WRITEBACK` (bool, default `false` — when enabled, PATCH edits to talkgroup/unit alpha_tags are written back to TR's CSV files on disk; requires `TR_DIR`), `UPLOAD_INSTANCE_ID` (instance ID for HTTP-uploaded calls, default `http-upload`), `MERGE_P25_SYSTEMS` (bool, default `true` — when enabled, TR instances monitoring the same P25 network (same sysid/wacn) are auto-merged into one system with multiple sites; set to `false` to keep each instance's systems separate), `STT_PROVIDER` (transcription provider: `whisper`, `elevenlabs`, or `deepinfra`; default `whisper`), `DEEPINFRA_STT_API_KEY` (DeepInfra API key, required when `STT_PROVIDER=deepinfra`), `DEEPINFRA_STT_MODEL` (DeepInfra model, default `openai/whisper-large-v3-turbo`), `RETENTION_RAW_MESSAGES` (raw MQTT archive retention, default `168h` / 7 days), `RETENTION_CONSOLE_LOGS` (console log retention, default `720h` / 30 days), `RETENTION_PLUGIN_STATUS` (plugin status retention, default `720h` / 30 days), `RETENTION_CHECKPOINTS` (active call checkpoint retention, default `168h` / 7 days), `RETENTION_STALE_CALLS` (stale incomplete call retention, default `1h`), `STREAM_LISTEN` (UDP listen address for simplestream audio, e.g. `:9123`; streaming disabled if empty), `STREAM_SAMPLE_RATE` (default PCM sample rate, default `8000`; 8000 for P25, 16000 for analog), `STREAM_OPUS_BITRATE` (Opus encoder bitrate in bps, default `16000`; 0 = PCM passthrough), `STREAM_MAX_CLIENTS` (max concurrent WebSocket listeners, default `50`), `STREAM_IDLE_TIMEOUT` (tear down idle per-talkgroup encoders, default `30s`), `DEBUG_REPORT_URL` (debug-receiver endpoint for forwarding diagnostic reports, default `https://case.luxprimatech.com/debug/report`; empty string disables).

**Ingest modes:** At least one of `MQTT_BROKER_URL`, `WATCH_DIR`, or `TR_DIR` must be set. HTTP upload mode is always available when a pipeline is running. Both MQTT and watch mode can run simultaneously. Watch mode only produces `call_end` events (files appear after calls complete). MQTT is the upgrade path for `call_start`, unit events, recorder state, and decode rates.

**TR auto-discovery (`TR_DIR`):** Point at the directory containing trunk-recorder's `config.json`. Auto-discovers `captureDir` (sets `WATCH_DIR` + `TR_AUDIO_DIR`), system names, imports talkgroup CSVs into a `talkgroup_directory` reference table (separate from the main `talkgroups` table which only contains heard talkgroups), and imports unit tag CSVs (`unitTagsFile`) into the `units` table. If a `docker-compose.yaml` is found, container paths are translated to host paths via volume mappings. Browsable via `GET /api/v1/talkgroup-directory?search=...`. When `CSV_WRITEBACK=true`, PATCH edits to alpha_tags are written back to the corresponding CSV files on disk.

## Development Environment

A live environment is available for testing:

- **PostgreSQL**: Deployed instance with real data from ingest testing. Connection details in `.env`.
- **MQTT broker**: Live production server connected to a real trunk-recorder instance. Credentials in `.env`.
- **Config**: Copy `sample.env` to `.env` and fill in credentials. The `.env` file is gitignored and auto-loaded on startup.

## Web Frontend (Page Registration)

HTML pages in `web/` are auto-discovered and listed on the index page via meta tags. No code changes needed — drop an `.html` file in `web/` with the right tags.

**Required meta tag** (without this, the page is invisible to the index):
```html
<meta name="card-title" content="My Page Title">
```

**Optional meta tags:**
```html
<meta name="card-description" content="Short description shown below the title">
<meta name="card-order" content="5">
```

- **card-description** — Gray subtitle text on the card. Omit for title-only.
- **card-order** — Integer sort key, lower = first. Defaults to 0. Current values: 1 (Event Horizon), 2 (Live Events), 3 (Unit Tracker), 10 (API Docs).

**Constraints:**
1. File must be `.html` in the root of `web/` — subdirectories are not scanned.
2. Meta tags must appear in the first 2048 bytes of the file.
3. Attributes must use double quotes in exact order: `name="card-title" content="..."`.

**How it works:**
- `GET /api/v1/pages` (`internal/api/pages.go`) scans `web/*.html`, extracts meta tags, returns sorted JSON.
- `theme-engine.js` injects a sticky header with a nav dropdown that fetches `/api/v1/pages` and renders links.
- **Page visibility**: Users can hide pages from the nav dropdown via an inline "Manage pages" edit mode. Eye icons toggle visibility per page. State persists in `localStorage` key `eh-hidden-pages`. Hidden pages are still accessible by direct URL.
- Dev mode (local `web/` directory on disk): new files picked up on next refresh, no rebuild.
- Production: files embedded via `//go:embed web/*` in `embed.go`, rebuild required. `embed.go` also embeds `openapi.yaml` and `schema.sql`.

**Minimal template:**
```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<script src="auth.js?v=1"></script>
<meta name="card-title" content="My Dashboard">
<meta name="card-description" content="One-line description">
<meta name="card-order" content="4">
<title>My Dashboard — tr-engine</title>
<!-- Theme system -->
<script src="theme-config.js"></script>
</head>
<body>
  <!-- page content -->
  <script src="theme-engine.js?v=2"></script>
</body>
</html>
```

## Documentation (docs/)

- `docs/getting-started.md` — Build from source (bare metal setup)
- `docs/docker.md` — Docker Compose all-in-one (bundled PostgreSQL + Mosquitto + tr-engine)
- `docs/docker-external-mqtt.md` — Docker Compose with an existing external MQTT broker
- `docs/binary-releases.md` — Pre-built binary download
- `docs/migrating-from-v0.md` — Migration guide from tr-engine-v0

## Implementation Status

**Completed:**
- `openapi.yaml` — full REST API spec with SSE event stream endpoint (49 endpoints)
- `schema.sql` — full PostgreSQL 18 DDL (20 tables, partitioning, triggers, helpers)
- MQTT ingestion pipeline — message routing, identity resolution, batch writes, all handler types (calls, units, recorders, rates, systems, config, audio, status, trunking messages, console logs)
- REST API — all 49 endpoints implemented across 13 handler files (systems, talkgroups, units, calls, call_groups, stats, recorders, events/SSE, unit-events, affiliations, transcriptions, admin, query)
- Database layer — complete CRUD and query builders for all tables
- SSE event bus — real-time pub/sub with ring buffer replay, `Last-Event-ID` support, and event publishing wired into all ingest handlers (call_start, call_end, unit_event, recorder_update, rate_update, trunking_message, console)
- Health endpoint — shows database, MQTT, and trunk-recorder instance status (connected/disconnected with last_seen timestamps)
- Dev tools — `cmd/mqtt-dump` (MQTT traffic inspector), `cmd/dbcheck` (DB analysis)
- Security hardening — proxy-aware per-IP rate limiting, 10 MB request body limit, response timeout for non-streaming handlers, CORS origin restrictions, XSS prevention in web UI
- Two-tier auth — read token (`AUTH_TOKEN`, auto-generated if not set) gates all API access; write token (`WRITE_TOKEN`) required for POST/PATCH/PUT/DELETE. When auth is enabled but `WRITE_TOKEN` is not set, the API runs in **read-only mode** — all mutating requests (including uploads) are rejected with 403. `GET /api/v1/auth-init` serves only the read token. Web pages load the read token via `auth.js` for seamless read access. Write operations (tag edits, system merges, transcription corrections, call uploads) require the write token, which is never exposed by any endpoint. When both tokens are empty (`AUTH_ENABLED=false`), all requests pass through with no auth.
- Tag source tracking — `alpha_tag_source` on talkgroups and units (`manual`, `csv`, `mqtt`). Manual edits are preserved across MQTT and CSV re-imports.
- Unit CSV import — loads unit tags from TR's `unitTagsFile` at startup; opt-in writeback on PATCH via `CSV_WRITEBACK`
- Affiliation map eviction — stale entries (>24h) cleaned every 5 minutes
- Warmup gate — buffers non-identity MQTT messages on fresh start until system registration establishes real P25 sysid/wacn, preventing duplicate system creation from early calls. Conventional systems release the gate immediately when their type is detected (no sysid to wait for). 5s timeout fallback if no system info arrives. Skipped on restart when identity cache loads from DB.
- Recorder enrichment — SSE `recorder_update` events and REST recorder cache are enriched with `tgid`, `tg_alpha_tag`, `unit_id`, `unit_alpha_tag` by matching recorder frequency against active calls.
- Theme engine — `theme-config.js` + `theme-engine.js` provide 11 switchable themes, sticky header with nav dropdown, keyboard shortcut (Ctrl+Shift+T), and page visibility management (hide/show pages per-browser via localStorage).
- Transcription pipeline — pluggable STT providers (`STT_PROVIDER`): `whisper` (self-hosted or cloud Whisper-compatible API), `elevenlabs` (ElevenLabs Scribe API), `deepinfra` (DeepInfra hosted Whisper). Configurable workers, queue size, duration filters, anti-hallucination parameters. Performance tracking: `provider_ms` isolates STT call time from total `duration_ms`; queue stats endpoint includes rolling real-time ratio averages.
- Live audio streaming — trunk-recorder simplestream UDP ingest (`STREAM_LISTEN`), per-talkgroup Opus/PCM encoding, multi-site deduplication, WebSocket delivery (`GET /audio/live`) with subscribe/unsubscribe filtering. Browser playback via `audio-engine.js` + `audio-worklet.js` AudioWorklet.
- DB maintenance — automated daily maintenance loop: partition creation (3 months ahead monthly, 3+ weeks ahead weekly), state table decimation (`recorder_snapshots`, `decode_rates`: 1/min after 1 week, 1/hr after 1 month), data purging (configurable retention via `RETENTION_*` env vars), stale call cleanup, orphan call_group cleanup. Admin API: `GET /api/v1/admin/maintenance` (view config + last run results), `POST /api/v1/admin/maintenance` (trigger immediate run). Both require WRITE_TOKEN.

**Not yet done:**
- Test coverage for unit-events and affiliations endpoints

## Real-Time Event Streaming (SSE)

`GET /api/v1/events/stream` pushes filtered events to clients over SSE.

- Filter params (all optional, AND-ed): `systems`, `sites`, `tgids`, `units`, `types`, `emergency_only`
- 8 event types: `call_start`, `call_update`, `call_end`, `unit_event`, `recorder_update`, `rate_update`, `trunking_message`, `console`
- `Last-Event-ID` header for gapless reconnect (60s server-side buffer)
- 15s keepalive comments
- Server sends `X-Accel-Buffering: no` header for nginx compatibility
- To change filters: disconnect and reconnect with new query params

### SSE Filtering Details

All filters are AND-ed. Events carry `SystemID`, `SiteID`, `Tgid`, and `UnitID` metadata for server-side filtering. Events with a zero value for a field (e.g., `recorder_update` has `SystemID=0` since recorders are per-instance, not per-system) pass through that filter dimension.

**Compound type syntax:** The `types` param supports `base:subtype` to filter event subtypes. Currently only `unit_event` has subtypes (on, off, call, end, join, location, ackresp, data). Examples:
- `types=unit_event` — all unit events (any subtype)
- `types=unit_event:call` — only unit call events
- `types=unit_event:call,unit_event:end,call_start` — mix compound and plain

Implementation: `EventData` struct in `eventbus.go` carries `Type`, `SubType`, `SystemID`, `SiteID`, `Tgid`, `UnitID`. All ingest handlers publish via `p.PublishEvent(EventData{...})`. The `matchesFilter()` function in `eventbus.go` handles all filter logic including compound type parsing via `strings.Cut`.

### MQTT Topic → Handler Mapping

The router (`router.go`) is prefix-agnostic — it matches on trailing segments only. Any topic prefix works as long as `MQTT_TOPICS` is set to subscribe to it. The `{topic}`, `{unit_topic}`, and `{message_topic}` below refer to the TR plugin's config fields.

| MQTT Topic | Handler | SSE Event | DB Table | Volume |
|-----------|---------|-----------|----------|--------|
| `{topic}/call_start` | `handleCallStart` | `call_start` | `calls` | Low |
| `{topic}/call_end` | `handleCallEnd` | `call_end` | `calls` | Low |
| `{unit_topic}/{sys_name}/{event}` | `handleUnitEvent` | `unit_event` | `unit_events` | Medium |
| `{topic}/recorders` | `handleRecorders` | `recorder_update` | `recorder_snapshots` | Medium |
| `{topic}/rates` | `handleRates` | `rate_update` | `decode_rates` | Low |
| `{message_topic}/{sys_name}/message` | `handleTrunkingMessage` | `trunking_message` | `trunking_messages` | Very high (batched) |
| `{topic}/trunk_recorder/console` | `handleConsoleLog` | `console` | `console_messages` | Low-medium |
| `{topic}/trunk_recorder/status` | `handleStatus` | _(none)_ | `plugin_statuses` | Very low |

Trunking messages use a `Batcher` for CopyFrom batch inserts (same as raw messages and recorder snapshots). Console logs use simple single-row INSERT. The status handler caches TR instance status in-memory for the `/api/v1/health` endpoint rather than publishing SSE events.

## Deployed Instance

A live v1 instance runs on `tr-dashboard` alongside the existing v0.

### Access

| Service | URL | Port |
|---------|-----|------|
| v1 (this repo) | `https://tr-engine.luxprimatech.com` | 8000 on host |
| v1 Tailnet (direct) | `http://tr-dashboard.pizzly-manta.ts.net:8000` | 8000 on host |
| v0 (legacy) | `https://tr-dashboard.luxprimatech.com` | 8080 on host |
| v0 API direct | `https://tr-api.luxprimatech.com` | 8080 on host |

**For API testing from scripts/CLI, prefer the Tailnet URL** — it bypasses Cloudflare (no WAF blocking, no rate limiting, no bot detection).

### Architecture on tr-dashboard

- **v0** runs natively at `/data/tr-engine` with embedded PostgreSQL and embedded MQTT broker (port 1883)
- **v1** runs via Docker Compose at `/data/tr-engine/v1` with its own PostgreSQL container
- v1 connects to v0's embedded MQTT broker via `host.docker.internal:1883` — both ingest the same feed
- **Caddy** reverse proxies all three domains, with Cloudflare in front (SSL: Full at domain level)
- MQTT credentials: see `.env` on tr-dashboard

### SSH

```bash
ssh root@tr-dashboard
```

### Docker Compose (v1)

```bash
# Location
cd /data/tr-engine/v1

# Status
docker compose ps
docker compose logs tr-engine --tail 20

# Restart
docker compose up -d

# Upgrade to new image
docker compose pull && docker compose up -d
```

### Dev Deploy Script

`deploy-dev.sh` cross-compiles for linux/amd64, stops the container, uploads the binary, restarts, and pushes web files:

```bash
./deploy-dev.sh              # full deploy (binary + web + restart)
./deploy-dev.sh --web-only   # just push web files (no restart needed)
./deploy-dev.sh --binary-only # just push binary + restart
```

### Updating Web Files (No Rebuild)

Web files are embedded in the Go binary via `go:embed`, but the Docker deployment bind-mounts `./web:/opt/tr-engine/web` which overrides the embedded files. When a `web/` directory exists on disk, tr-engine serves from it instead — changes take effect on the next browser request with no restart.

**Push local changes to deployed instance:**
```bash
scp web/*.html web/*.js root@tr-dashboard:/data/tr-engine/v1/web/
```

**Push a single file:**
```bash
scp web/stream-graph.html root@tr-dashboard:/data/tr-engine/v1/web/
```

**Pull latest from GitHub on the server:**
```bash
ssh root@tr-dashboard 'cd /data/tr-engine/v1/web && curl -s https://api.github.com/repos/LumenPrima/tr-engine/contents/web | python3 -c "import json,sys,urllib.request; [urllib.request.urlretrieve(f[\"download_url\"],f[\"name\"]) for f in json.load(sys.stdin) if f[\"type\"]==\"file\"]"'
```

### File Layout on tr-dashboard

```
/data/tr-engine/              # 3.6TB RAID mount
├── config.yaml               # v0 config
├── postgres/                  # v0 embedded PostgreSQL data
├── audio/                     # v0 audio files
├── mqtt/                      # v0 embedded Mosquitto data
└── v1/
    ├── docker-compose.yml     # PostgreSQL + tr-engine (no Mosquitto)
    ├── pgdata/                # PostgreSQL data (bind-mounted into container)
    ├── audio/                 # Call audio files (bind-mounted into container)
    └── web/                   # Bind-mounted into container, overrides embedded UI
        ├── auth.js             # Shared auth (fetches token from /api/v1/auth-init)
        ├── index.html
        ├── irc-radio-live.html
        ├── stream-graph.html
        ├── signal-flow-data.js
        ├── scanner.html
        ├── units.html
        ├── events.html
        ├── timeline.html
        ├── talkgroup-directory.html
        ├── debug-report.html    # Hidden (no card-title), direct URL only
        └── docs.html
```

All persistent data (pgdata, audio, web) is bind-mounted from the RAID into the containers — nothing stored on the 32GB root filesystem.

### Caddy Config

Located at `/etc/caddy/Caddyfile`. To add/modify:

```bash
ssh root@tr-dashboard "vi /etc/caddy/Caddyfile"
# After editing:
ssh root@tr-dashboard "caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy"
```

DNS is managed via Cloudflare (proxied). SSL mode is set globally to Full — no per-domain page rules needed.

**Auth header injection:** The `tr-dashboard.luxprimatech.com` block injects the read token (`AUTH_TOKEN`) into proxied `/api/*` requests — but only when the browser doesn't send its own `Authorization` header. This is critical for two-tier auth:

```
@no_auth not header Authorization *
request_header @no_auth Authorization "Bearer {read_token}"
```

- **No token from browser** → Caddy injects read token → reads work seamlessly via `auth.js`
- **Write token from browser** → Caddy passes it through untouched → writes work

Without the `@no_auth` matcher, Caddy would unconditionally overwrite the header, replacing the browser's write token with the read token and breaking all write operations. The `tr-engine.luxprimatech.com` direct-access domain has no injection — callers provide their own token.

## Known trunk-recorder Issues (Potential Upstream Bug Reports)

### unit_event:end lags call_end by 3-4 seconds
`unit_event:end` arrives 3-4s after `call_end` for the same call. `call_end` fires from the recorder when voice frames stop (immediate), but `unit_event:end` fires from the control channel parser when it sees the deaffiliation message (delayed by the P25 trunking update cycle). TR should be able to detect unit transmission end from the recorder side (voice frames going null) rather than waiting for the control channel. The P25 channel stays allocated during hang time, but the actual voice traffic stops immediately. Event Horizon works around this with a 6s coalesce window.

### call ID shifts between call_start and call_end
The trunk-recorder call ID format `{sys_num}_{tgid}_{start_time}` embeds `start_time`, which can shift by 1-2 seconds between `call_start` and `call_end` messages. This causes the call_end handler to fail exact-match lookup. tr-engine works around this with fuzzy matching by `(tgid, start_time ± 5s)` in the active calls map.
