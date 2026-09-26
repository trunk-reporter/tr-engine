# AGENTS.md

This file provides guidance to Codex and other coding agents when working with code in this repository. Legacy CLAUDE.md files may remain during migration; prefer AGENTS.md for active instructions.

## What This Project Is

tr-engine is a backend service that ingests MQTT messages from one or more [trunk-recorder](https://github.com/robotastic/trunk-recorder) instances and serves them via a REST API. It handles radio system monitoring data: calls, talkgroups, units, transcriptions, and recorder state.

Current dev/test uses 2 counties (Butler/Warren, NACs 340/34D).

## Technology Stack

- **Language**: Go
- **Database**: PostgreSQL 17+
- **MQTT**: ingests from trunk-recorder instances
- **Real-time push**: Server-Sent Events (SSE) at `GET /api/v1/events/stream` with server-side filtering (systems, sites, tgids, units, event types). Clients reconnect with `Last-Event-ID` (header) or `last_event_id` (query) for gapless recovery on filter changes.
- **Auth**: API keys (`Authorization: Bearer tre_...`) with scopes `listen`/`edit`/`admin`/`upload`, an anonymous access policy for requests without a key, and short-lived `?ticket=`s for `EventSource`/`<audio>`/WebSocket. No users, logins or cookies. See `docs/auth.md`.
- **API**: REST under `/api/v1`, defined in `openapi.yaml`

Go was chosen over Node.js for multi-core utilization and headroom at high message rates.

## Key Files

- `openapi.yaml` — Complete REST API specification (OpenAPI 3.0.3), including SSE event stream endpoint. This is the **source of truth** for API contracts. **IMPORTANT: When adding or modifying API endpoints, request/response schemas, event types, enums, or SSE payload fields, always update `openapi.yaml` to match.** This includes new unit event subtypes, new query parameters, new schema properties, and changes to enum values (e.g., `EventType`, `SystemType`, `SSEEventType`).
- `schema.sql` — PostgreSQL 18 DDL. All tables, indexes, triggers, partitioning, and helper functions. Embedded in the binary via `embed.go` and auto-applied on first startup when the database is empty. Can also be run manually with `psql -f schema.sql`.
- `.env` — Local environment config (gitignored). Contains `DATABASE_URL`, `MQTT_BROKER_URL`, credentials, and `HTTP_ADDR`. Required for Docker (`POSTGRES_PASSWORD`, `MQTT_PASSWORD`); everything else has defaults in `docker-compose.yml`.
- `embed.go` — Go embed directives for `web/*`, `openapi.yaml`, and `schema.sql`. Exposes `WebFiles`, `OpenAPISpec`, and `SchemaSQL` package-level variables.
- `cmd/tr-engine/main.go` — Entry point. Startup order: config → logger → database → schema init → migrations → auth setup (one-time legacy import of `AUTH_TOKEN`/`WRITE_TOKEN`, warnings for removed auth variables, ticket secret, bootstrap admin key printed to stderr once per database) → MQTT → pipeline → HTTP server. Graceful shutdown via SIGINT/SIGTERM with 10s timeout. Version injected via `-ldflags`. Subcommands: `export`, `import`, `keys` (`list`, `create`, `update`, `revoke`, `import`) and `access` (`show`, `set`, `forget-retired-token`); `keys`/`access` log to stderr at WARN and print only their result on stdout (`docker compose exec -T tr-engine tr-engine keys ...`).
- `cmd/mqtt-dump/` — Dev tool to capture and display live MQTT traffic.
- `cmd/dbcheck/` — DB inspection tool (table counts, call group analysis, cleanup).
- `internal/config/config.go` — Env-based config (`DATABASE_URL`, `MQTT_BROKER_URL`, `HTTP_ADDR`, `LOG_LEVEL`, rate limits, `TRUSTED_PROXIES`, timeouts). Uses `caarlos0/env/v11`. The removed auth variables (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS`) configure nothing; they are read only by the one-time legacy import and to warn.
- `internal/auth/` — Auth primitives with no dependencies on other tr-engine packages: `Scope`/`Scopes` (validation, implication `admin > edit > listen`, `upload` independent), `TG` (`"system_id:tgid"`), `Restriction` (`allow_all`/`systems`/`talkgroups`/`exclude_talkgroups`; `null` = unrestricted, an empty allow list allows nothing), `Principal` (`key`/`ticket`/`anonymous`/`internal`; `Principal.SQL(sysCol, tgCol, firstArg)` builds the restriction clause with non-nil `int4[]` args — never use the `pqIntArray` helpers for restrictions, pgx encodes nil as NULL), `SignTicket`/`VerifyTicket` (`trt_<payload>.<mac>`, HMAC-SHA256, ≤1 h), and the auth generation (`Generation()`/`Bump()`, bumped by key PATCH/DELETE, anonymous-policy PUT and system merges).
- `internal/database/` — pgxpool wrapper (20 max / 4 min conns, 2s health-check ping) plus query files for all tables: systems, sites, talkgroups, units, calls, call_groups, recorders, stats, etc. `schema.go` handles first-run schema initialization; `migrations.go` handles incremental schema changes.
- `internal/unittags/` — Unit alpha tag suggestion scanner (opt-in, `UNIT_TAG_SUGGESTIONS`). `extract.go` is a pure, positional extractor of a unit's self-identification ("County, Medic 12 on scene") from its own attributed speech; `scanner.go` walks `transcriptions` in id order from a `scan_cursors` cursor and accumulates candidates and capped evidence in `unit_tag_suggestions`. Units change only through `POST /unit-tag-suggestions/{id}/approve` (`internal/api/unit_tag_suggestions.go`; SQL in `internal/database/unit_tag_suggestions.go`). `db_integration_test.go` runs against a real PostgreSQL only when `TEST_DATABASE_URL` is set.
- `internal/mqttclient/client.go` — Paho MQTT client. Auto-reconnect (5s), QoS 0, `atomic.Bool` connection tracking.
- `internal/ingest/` — Complete MQTT ingestion pipeline. Message routing (`router.go`), identity resolution (`identity.go`), event bus for SSE (`eventbus.go`), batch writers (`batcher.go`), and handlers for all message types (calls, units, recorders, rates, systems, config, audio, status, trunking messages, console logs). Raw archival supports three modes: disabled (`RAW_STORE=false`), allowlist (`RAW_INCLUDE_TOPICS` with `_unknown` for unrecognized topics), or denylist (`RAW_EXCLUDE_TOPICS`). Audio messages have base64 audio data stripped before raw archival since the audio is already saved to disk.
- `internal/api/server.go` — Chi router + HTTP server lifecycle. `buildRouter(opts)` registers every route **flat** (no `r.Route(...)` sub-routers) so `chi.Walk` and `Mux.Find` agree on pattern strings.
- `internal/api/policy.go` — The route policy table: every `"METHOD /full/pattern"` → `RoutePolicy{Scope, Restricted (Deny|Enforced), Ticket, KeyRequired, FormKey}`, plus the SSE per-type scope map. A matched route without an entry fails closed (403 + ERROR log); tests check the table against `chi.Walk` and against `openapi.yaml`'s `x-scope`/`x-restricted`. **Every new route needs an entry here and matching `x-scope`/`x-restricted` in `openapi.yaml`.** Restricted principals can only reach `Enforced` routes, whose handlers must apply the restriction (DB queries take the principal as a required parameter; single resources call `GetCallAccess` first and 404 outside the restriction).
- Auth endpoints (`internal/api`): `GET /whoami` (public), `/keys` CRUD, `GET/PUT /anonymous-access`, `GET /admin/audit-log` (admin), `POST /tickets` (key with listen). SQL in `internal/database/api_keys.go` plus the `auth_settings` (anonymous policy, ticket secret, retired public token) and `audit_log` tables.
- `internal/api/query.go` — Ad-hoc read-only SQL query handler (`POST /query`). Read-only transaction, 30s statement timeout, row cap, semicolon rejection.
- `internal/database/query.go` — `ExecuteReadOnlyQuery()` — runs SQL in a `BEGIN READ ONLY` transaction with `SET LOCAL statement_timeout = '30s'`.
- `internal/api/upload.go` — HTTP call upload handler (`POST /api/v1/call-upload`). Auto-detects rdio-scanner vs OpenMHz format from form field names. Uses `CallUploader` interface (defined in `live_data.go`) to avoid circular imports with `ingest`.
- `internal/ingest/handler_upload.go` — `ProcessUploadedCall` (full pipeline: identity resolution, dedup, call creation, audio save, SSE publish, transcription enqueue), `ProcessUpload` adapter (implements `api.CallUploader`), `ParseRdioScannerFields`, `ParseOpenMHzFields`.
- `internal/api/middleware.go` — Root pipeline in order: RequestID (client value kept only if ≤64 chars of `[A-Za-z0-9._-]`), CORS (`Access-Control-Allow-Origin: *` on every response, never credentials; `OPTIONS` → 204 before auth/routing), Recoverer (JSON 500) + Logger (zerolog/hlog), Match (`root.Find` with a fresh route context; unmatched → chi 404/405), Resolve principal (`Authorization: Bearer` only; `?ticket=` only on ticket routes for GET/HEAD and it wins; invalid credential → 401, never anonymous), Authorize (public / `key_required` / `insufficient_scope` / `restricted_credential`), Audit (key principals, non-GET/HEAD/OPTIONS). Rate limiting is interleaved with Resolve: per-IP for no credential, tickets, legacy keys and key-cache misses (client IP from `TrustedProxies.ClientIP` in `clientip.go`, which only believes `X-Forwarded-For`/`X-Real-IP` from `TRUSTED_PROXIES` peers and walks the chain right-to-left); per-key when the key has `rate_limit_rps`. Key cache: separate positive/negative LRUs, 30 s TTL. Then route-group middleware: MaxBodySize (10 MB for API, 50 MB for uploads), the upload middleware (key from multipart `key`/`api_key` when no Bearer header), ResponseTimeout (wraps non-SSE/audio handlers with `HTTP_WRITE_TIMEOUT`).
- `internal/audio/simplestream.go` — UDP listener for trunk-recorder's simplestream plugin. Parses sendJSON (4-byte LE length + JSON metadata + PCM) and sendTGID (4-byte LE TGID + PCM) packet formats.
- `internal/audio/router.go` — Audio router: identity resolution (short_name → system/site), multi-site deduplication, per-talkgroup encoding, publishes to AudioBus.
- `internal/audio/bus.go` — Pub/sub event bus for audio frames. WebSocket clients subscribe with filters (system IDs, TGIDs).
- `internal/api/audio_stream.go` — WebSocket endpoint (`GET /audio/live`). Clients send JSON subscribe/unsubscribe messages; server sends binary frames (12-byte header + audio data).
- `web/audio-engine.js` — Browser-side audio playback engine. Manages WebSocket connection, audio decoding, and playback via AudioWorklet.
- `web/audio-worklet.js` — AudioWorklet processor for low-latency PCM playback in the browser.
- `web/auth.js` — Shared browser-side auth for all web pages (`auth.js?v=3`). Keeps one API key in `localStorage['tr-engine-api-key']` (old `tr-engine-token`/`-write-token`/`-jwt` entries are removed) and fetches `GET /api/v1/whoami` asynchronously. Patches `window.fetch` for **same-origin `/api/` URLs only** to add `Authorization: Bearer` when a key is stored; prompts for a key only on 401 `invalid_key`, or `key_required` with no key and anonymous access off. Replaces `EventSource` for same-origin API URLs with a key by a wrapper that mints a ticket (`POST /api/v1/tickets`), connects with `?ticket=`, handles `event: auth`, and reconnects with a fresh ticket and `last_event_id`. `window.trAuth`: `ready()`, `getKey()`, `setKey()`, `clearKey()`, `whoami()`, `hasScope()`, `showKeyPrompt()`, `mediaUrl(url)` (sync, cached ticket), `ticketUrl(url)` (async), plus deprecated shims for old pages. `admin.html`/`storage.html` take an admin key per tab (`sessionStorage`).

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
| Permanent | calls, call_frequencies, call_transmissions, unit_events, transcriptions, talkgroups, units | Forever (partitioned) |
| Configurable retention | trunking_messages | Configurable (default 720h / 30d, partitioned) |
| Decimated state | recorder_snapshots, decode_rates | Full 1 week → 1/min 1 month → 1/hour |
| Crash recovery | call_active_checkpoints | 7 days |
| Raw archive | mqtt_raw_messages | 7 days |
| Logs | console_messages, plugin_statuses | 30 days |
| Audit | system_merge_log, instance_configs | Forever (low volume) |
| Review queue | unit_tag_suggestions, scan_cursors | Forever (low volume) |
| Access control | api_keys (revoked keys kept, soft delete), auth_settings | Forever (low volume) |
| Audit | audit_log | `RETENTION_AUDIT_LOG` (default 8760h / 1 year) |

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

**Subcommands:** `tr-engine keys list|create|update|revoke|import` and `tr-engine access show|set|forget-retired-token` manage API keys and the anonymous access policy directly in the database (they take `--env-file`/`--database-url` like the server); `tr-engine export`/`import` move data between instances. In Docker: `docker compose exec -T tr-engine tr-engine keys ...`.

**Access control is not configured with environment variables.** API keys and the anonymous access policy (`off` by default on a fresh install) live in the database. On the first start with an empty database the engine prints a `bootstrap admin` key to stderr once. See `docs/auth.md` (operator + client-developer guide) and `docs/migrating-auth.md` (upgrading from `AUTH_TOKEN`/`WRITE_TOKEN`/`ADMIN_PASSWORD`).

**Docker Compose-only settings** (used by `docker-compose.yml` variable interpolation, ignored by the binary): `POSTGRES_USER` (default `trengine`), `POSTGRES_PASSWORD` (**required, no default** — `${POSTGRES_PASSWORD:?...}`), `POSTGRES_DB` (default `trengine`) — configure both the postgres container and `DATABASE_URL` in one place; `MQTT_USERNAME` (default `trengine`) / `MQTT_PASSWORD` (required; bundled Mosquitto has `allow_anonymous false` + `mosquitto/passwd`); `HTTP_PORT` (default `8080`), `MQTT_PORT` (default `1883`) — host port mappings; `HTTP_BIND_IP`, `MQTT_BIND_IP`, `BIND_IP` (Caddy) — host bind addresses, all default `127.0.0.1` (`BIND_IP` is required in `docker-compose.full.yml`). PostgreSQL is never published. Never reintroduce default passwords, anonymous MQTT, or unbound (`0.0.0.0`) port mappings in shipped compose files.

Additional env-only settings: `MQTT_TOPICS` (comma-separated MQTT topic filters, default `#`; match your TR plugin's `topic`/`unit_topic`/`message_topic` prefixes with `/#` wildcards to limit subscriptions), `MQTT_INSTANCE_MAP` (comma-separated `prefix:instance_id` pairs; rewrites `instance_id` in MQTT payloads based on topic prefix — use when multiple TR instances share the default `instance_id` "trunk-recorder" to prevent identity collisions; e.g. `trdash:trdash,cpg178:cpg178`), `MQTT_CLIENT_ID`, `MQTT_USERNAME`, `MQTT_PASSWORD`, `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT`, `RATE_LIMIT_RPS` (per-IP requests/second, default `20`; applies to requests without a key, with a ticket, with a legacy key, and to key-cache misses — other keys are limited only by their own `rate_limit_rps`), `RATE_LIMIT_BURST` (per-IP burst size, default `40`), `TRUSTED_PROXIES` (reverse proxies whose `X-Forwarded-For`/`X-Real-IP` are believed; comma-separated IPs/CIDRs plus keywords `loopback`, `private`, `none`; default `loopback,private`), `RAW_STORE` (bool, default `true` — master switch to disable all raw MQTT archival), `RAW_INCLUDE_TOPICS` (comma-separated allowlist of handler names for raw archival; supports `_unknown` for unrecognized topics; takes priority over `RAW_EXCLUDE_TOPICS`), `RAW_EXCLUDE_TOPICS` (comma-separated denylist of handler names to exclude from raw archival), `WATCH_INSTANCE_ID` (instance ID for file-watched calls, default `file-watch`), `WATCH_BACKFILL_DAYS` (days of existing files to backfill on startup, default `7`; `0` = all, `-1` = none), `CSV_WRITEBACK` (bool, default `false` — when enabled, PATCH edits to talkgroup/unit alpha_tags are written back to TR's CSV files on disk; requires `TR_DIR`), `UPLOAD_INSTANCE_ID` (instance ID for HTTP-uploaded calls, default `http-upload`), `MERGE_P25_SYSTEMS` (bool, default `true` — when enabled, TR instances monitoring the same P25 network (same sysid/wacn) are auto-merged into one system with multiple sites; set to `false` to keep each instance's systems separate), `STT_PROVIDER` (transcription provider: `whisper`, `elevenlabs`, or `deepinfra`; default `whisper`), `DEEPINFRA_STT_API_KEY` (DeepInfra API key, required when `STT_PROVIDER=deepinfra`), `DEEPINFRA_STT_MODEL` (DeepInfra model, default `openai/whisper-large-v3-turbo`), `RETENTION_RAW_MESSAGES` (raw MQTT archive retention, default `168h` / 7 days), `RETENTION_CONSOLE_LOGS` (console log retention, default `720h` / 30 days), `RETENTION_PLUGIN_STATUS` (plugin status retention, default `720h` / 30 days), `RETENTION_CHECKPOINTS` (active call checkpoint retention, default `168h` / 7 days), `RETENTION_STALE_CALLS` (stale incomplete call retention, default `1h`), `RETENTION_AUDIT_LOG` (audit log retention, default `8760h` / 1 year), `STREAM_LISTEN` (UDP listen address for simplestream audio, e.g. `:9123`; streaming disabled if empty), `STREAM_SAMPLE_RATE` (default PCM sample rate, default `8000`; 8000 for P25, 16000 for analog), `STREAM_OPUS_BITRATE` (Opus encoder bitrate in bps, default `16000`; 0 = PCM passthrough), `STREAM_MAX_CLIENTS` (max concurrent WebSocket listeners, default `50`), `STREAM_IDLE_TIMEOUT` (tear down idle per-talkgroup encoders, default `30s`), `DEBUG_REPORT_URL` (debug-receiver endpoint for forwarding diagnostic reports, default `https://case.luxprimatech.com/debug/report`), `DEBUG_REPORT_DISABLE` (bool, default `false` — set to `true` to disable debug report submissions; `POST /debug-report` needs an admin key), `UNIT_TAG_SUGGESTIONS` (bool, default `false` — background scanner in `internal/unittags` that extracts unit self-identifications (designators like `Medic 12`/`P338`/`1 Paul 31`, not personal names) from attributed, primary transcriptions into the `unit_tag_suggestions` review queue, counting each call group (multi-site recordings of one transmission) once; resumable via `scan_cursors`, backfills on first enable; serializes with `MergeSystems` via a transaction advisory lock; never writes `units` — only `POST /unit-tag-suggestions/{id}/approve` does, via the same path as `PATCH /units/{id}` with `alpha_tag_source=manual`), `UNIT_TAG_SUGGESTIONS_MIN_CALLS` (default `3`) / `UNIT_TAG_SUGGESTIONS_MIN_SHARE` (default `0.2`) (review gate for listing pending candidates), `UNIT_TAG_SUGGESTIONS_INTERVAL` (scan pause once caught up, default `60s`).

**Ingest modes:** At least one of `MQTT_BROKER_URL`, `WATCH_DIR`, or `TR_DIR` must be set. HTTP upload mode is always available when a pipeline is running. Both MQTT and watch mode can run simultaneously. Watch mode only produces `call_end` events (files appear after calls complete). MQTT is the upgrade path for `call_start`, unit events, recorder state, and decode rates.

**TR auto-discovery (`TR_DIR`):** Point at the directory containing trunk-recorder's `config.json`. Auto-discovers `captureDir` (sets `WATCH_DIR` + `TR_AUDIO_DIR`), system names, imports talkgroup CSVs into a `talkgroup_directory` reference table (separate from the main `talkgroups` table which only contains heard talkgroups), and imports unit tag CSVs (`unitTagsFile`) into the `units` table; both CSVs are polled (30s, `trconfig.FileWatch`) and re-imported when they change. If a `docker-compose.yaml` is found, container paths are translated to host paths via volume mappings. Browsable via `GET /api/v1/talkgroup-directory?search=...`. When `CSV_WRITEBACK=true`, PATCH edits to alpha_tags are written back to the corresponding CSV files on disk.

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

**Auth in pages:** include `auth.js?v=3` first; plain `fetch('/api/v1/...')` and `new EventSource('/api/v1/events/stream?...')` then work with or without a stored key. Use `trAuth.mediaUrl(url)` for `<audio src>` (retry once on `error` with `await trAuth.ticketUrl(url)`), `await trAuth.ticketUrl(...)` for WebSocket URLs, and `trAuth.hasScope('edit'|'admin')` to gate features. Render API strings with `textContent` (an `edit` key can set tags to HTML). Use `Promise.allSettled` when mixing restriction-enforced and restriction-denied endpoints.

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
<script src="auth.js?v=3"></script>
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
- `docs/docker-full-stack.md` — Docker Compose with Caddy (HTTPS) and tr-dashboard
- `docs/binary-releases.md` — Pre-built binary download
- `docs/auth.md` — API keys, scopes, anonymous access, restrictions, tickets: operator guide and client-developer guide
- `docs/migrating-auth.md` — Upgrading from `AUTH_TOKEN`/`WRITE_TOKEN`/`ADMIN_PASSWORD` (legacy import, step-by-step)
- `docs/http-upload.md` — HTTP call upload (rdio-scanner/OpenMHz plugins, `upload` keys)
- `docs/architecture.md` — Technical flow: startup, ingest, HTTP pipeline, SSE
- `docs/building-pages.md` — Prompt templates for custom `web/` pages
- `docs/migrating-from-v0.md` — Migration guide from tr-engine-v0

## Implementation Status

**Completed:**
- `openapi.yaml` — full REST API spec with SSE event stream endpoint (49 endpoints)
- `schema.sql` — full PostgreSQL 18 DDL (23 tables, partitioning, triggers, helpers)
- MQTT ingestion pipeline — message routing, identity resolution, batch writes, all handler types (calls, units, recorders, rates, systems, config, audio, status, trunking messages, console logs)
- REST API — all 49 endpoints implemented across 13 handler files (systems, talkgroups, units, calls, call_groups, stats, recorders, events/SSE, unit-events, affiliations, transcriptions, admin, query)
- Database layer — complete CRUD and query builders for all tables
- SSE event bus — real-time pub/sub with ring buffer replay, `Last-Event-ID` support, and event publishing wired into all ingest handlers (call_start, call_end, unit_event, recorder_update, rate_update, trunking_message, console)
- Health endpoint — shows database, MQTT, and trunk-recorder instance status (connected/disconnected with last_seen timestamps)
- Dev tools — `cmd/mqtt-dump` (MQTT traffic inspector), `cmd/dbcheck` (DB analysis)
- Security hardening — proxy-aware per-IP rate limiting (`TRUSTED_PROXIES`), 10 MB request body limit, response timeout for non-streaming handlers, CORS `*` without credentials (no cookies anywhere), XSS prevention in web UI
- API-key auth — The engine authenticates client software, not people. **Keys** (`tre_` + 64 hex, SHA-256 stored, plaintext shown once) carry scopes: at most one of `listen`/`edit`/`admin` (`admin` ⊃ `edit` ⊃ `listen`), optionally plus `upload` (`POST /call-upload` only). **Anonymous access policy** (`auth_settings.anonymous_access`): `off` (default) or `listen`, optionally restricted; set with `PUT /anonymous-access` or `tr-engine access set`. **Restrictions** (`allow_all`/`systems`/`talkgroups`/`exclude_talkgroups`) on `["listen"]` keys, the anonymous policy and ticket narrowings; `null` = unrestricted, empty allow list = nothing, NULL/0 talkgroups never allowed; system merges rewrite them. **Tickets** (`POST /tickets`, `trt_...`, 60 s–1 h, HMAC with `auth_settings.ticket_secret`) are accepted only as `?ticket=` on `/events/stream`, `/audio/live`, `/calls/{id}/audio` (GET/HEAD), grant `listen` only, reflect the minting key's current state, and their streams close at expiry. Presented-but-invalid credentials are always 401 (`invalid_key`/`invalid_ticket`), never anonymous. Error codes: `key_required`, `invalid_key`, `invalid_ticket` (401), `insufficient_scope`, `restricted_credential` (403), `service_unavailable` (503, key lookup failed); every 401 has `WWW-Authenticate: Bearer realm="tr-engine"`. `GET /whoami` replaces `/auth-init` and `/auth/me`; `/auth/*`, `/users` and `?token=` are gone (404). Last-admin guard on the API (not the CLI). Bootstrap admin key printed once per database. One-time legacy import (`data_fixups` `import-legacy-auth`): `WRITE_TOKEN` → `legacy WRITE_TOKEN` (admin, upload) unless it equals the full-mode public `AUTH_TOKEN`; token-mode `AUTH_TOKEN` → `legacy AUTH_TOKEN` (listen, +upload without `WRITE_TOKEN`); full-mode `AUTH_TOKEN` retired (its hash is ignored when presented); anonymous policy `listen` for upgraded open instances with data and full-mode public demos. The `users` table is recorded in `data_fixups` and dropped; user-owned keys keep the lower of key and owner role.
- Audit log — `audit_log` records every key-authenticated non-GET/HEAD/OPTIONS request to a matched route (except call-upload and tickets) with key name, optional `X-Actor`, method, path, status and request ID; `GET /admin/audit-log`; `RETENTION_AUDIT_LOG`. `unit_tag_suggestions.decided_by` and `system_merge_log.performed_by` store the key name (plus ` / <actor>`).
- Tag source tracking — `alpha_tag_source` on talkgroups and units (`manual`, `csv`, `mqtt`; MQTT-discovered rows are NULL). Priority manual > csv > mqtt: PATCH edits mark `manual` and are never overwritten; CSV tags (talkgroup directory via `EnrichTalkgroupsFromDirectory`, unit tags via `ImportUnitTags`, one set-based statement per file) replace MQTT tags and earlier CSV tags; `UpsertTalkgroup`/`UpsertUnit` never change the tags of manual/csv rows (`alpha_tag`, plus a talkgroup's `tag`/`group`/`description`), and `MergeSystems` combines tags by the same priority. Directory enrichment runs on CSV import, at startup, and on every call/unit event (`upsertAndEnrichTalkgroup`), writing only rows that change. Calls and call groups denormalize the effective (post-enrichment) talkgroup alpha tag (`tg_alpha_tag`); their `tg_tag`/`tg_group`/`tg_description` are still the reporting TR instance's values. `talkgroup_directory` holds only imported CSV values: PATCH copies an edit into it only when `CSV_WRITEBACK` also wrote it to TR's CSV. On the first start after upgrading, `KeepPreUpgradeTagEdits` (once per DB, recorded in `data_fixups` with the pinned IDs in `detail`) marks `manual` every csv-sourced talkgroup/unit whose tag isn't confirmed by the TR_DIR CSV about to be imported, since older versions left edits marked `csv` (and copied talkgroup edits into `talkgroup_directory`); a TR_DIR CSV that fails to load then confirms nothing, which is logged per file. CSV imports by `system_name` only target existing systems (404 otherwise): ingest finds systems through sites, so a system created by an import would never get traffic.
- Unit CSV import — loads unit tags from TR's `unitTagsFile` at startup, or via upload at `POST /api/v1/unit-tags/import`; opt-in writeback on PATCH via `CSV_WRITEBACK`
- Unit tag observations — `units.recorder_alpha_tag` (latest tag TR reported, kept even when a manual/csv `alpha_tag` wins) and `units.ota_alpha_tag` (raw over-the-air alias from optional MQTT field `unit_alpha_tag_ota`, or `srcList[].tag_ota` that trunk-recorder 5.2+ writes into call JSON (file-watch/upload), with `ota_alpha_tag_first_seen`/`_last_seen`). Written in the same `UpsertUnit` statement; empty values are no-ops, older events never overwrite newer observations, and neither column ever changes `alpha_tag`. Export/import and system merge combine them with the same rules (`unitObservationMergeSQL`). Exposed read-only on unit API responses, SSE `unit_event` (`unit_ota_alpha_tag`), and export archives.
- Affiliation map eviction — stale entries (>24h) cleaned every 5 minutes
- Warmup gate — buffers non-identity MQTT messages on fresh start until system registration establishes real P25 sysid/wacn, preventing duplicate system creation from early calls. Conventional systems release the gate immediately when their type is detected (no sysid to wait for). 5s timeout fallback if no system info arrives. Skipped on restart when identity cache loads from DB.
- Recorder enrichment — SSE `recorder_update` events and REST recorder cache are enriched with `tgid`, `tg_alpha_tag`, `unit_id`, `unit_alpha_tag` by matching recorder frequency against active calls.
- Theme engine — `theme-config.js` + `theme-engine.js` provide 11 switchable themes, sticky header with nav dropdown, keyboard shortcut (Ctrl+Shift+T), and page visibility management (hide/show pages per-browser via localStorage).
- Transcription pipeline — pluggable STT providers (`STT_PROVIDER`): `whisper` (self-hosted or cloud Whisper-compatible API), `elevenlabs` (ElevenLabs Scribe API), `deepinfra` (DeepInfra hosted Whisper), `imbe` (IMBE ASR — transcribes directly from P25 IMBE codec frames). Configurable workers, queue size, duration filters, anti-hallucination parameters. Performance tracking: `provider_ms` isolates STT call time from total `duration_ms`; queue stats endpoint includes rolling real-time ratio averages.
- IMBE ASR integration — when `STT_PROVIDER=imbe`, transcription is enqueued by `handleDvcf` (not `handleAudio` or other handlers). The DVCF plugin (`tr-plugin-dvcf`) publishes `.dvcf` files (SymbolStream v2 binary format) on a separate MQTT topic (`{topic}/dvcf`) without the standard TR envelope (no `instance_id`). The MQTT payload is `{"audio_dvcf_base64": "...", "metadata": {...}}` where metadata includes talkgroup info, signal quality (`signal`, `noise`, `freq_error`), call timing, emergency/priority flags, TDMA state, and `srcList`. `handleDvcf` saves the file, finds the matching call via `FindCallBySystemName(system_name, tgid, start_time ±5s)`, and enqueues transcription. All other `enqueueTranscription()` call sites are no-ops when IMBE is active. **Known limitation:** IMBE only handles P25 calls. Analog calls on the same system get no transcription. Future fix: dual-provider routing (IMBE for P25, Whisper fallback for analog).
- DVCF binary format — SymbolStream Protocol v2. 8-byte header (`SY` magic + version + msg_type + payload_len), message types: CODEC_FRAME (0x01, raw IMBE/AMBE params), CALL_START (0x02), CALL_END (0x03), HEARTBEAT (0x04), CALL_METADATA (0x05, JSON with talkgroup tags, signal quality, src_list). `.dvcf` files are self-contained: CALL_START + codec frames + CALL_METADATA + CALL_END. Full spec at `tr-plugin-dvcf/DVCF_SPEC.md`.
- Live audio streaming — trunk-recorder simplestream UDP ingest (`STREAM_LISTEN`), per-talkgroup Opus/PCM encoding, multi-site deduplication, WebSocket delivery (`GET /audio/live`) with subscribe/unsubscribe filtering. Browser playback via `audio-engine.js` + `audio-worklet.js` AudioWorklet.
- DB maintenance — automated daily maintenance loop: partition creation (3 months ahead monthly, 3+ weeks ahead weekly), state table decimation (`recorder_snapshots`, `decode_rates`: 1/min after 1 week, 1/hr after 1 month), data purging (configurable retention via `RETENTION_*` env vars), stale call cleanup, orphan call_group cleanup. Admin API: `GET /api/v1/admin/maintenance` (view config + last run results), `POST /api/v1/admin/maintenance` (trigger immediate run). Both require the `admin` scope.

**Not yet done:**
- Test coverage for unit-events and affiliations endpoints

## Real-Time Event Streaming (SSE)

`GET /api/v1/events/stream` pushes filtered events to clients over SSE.

- Filter params (all optional, AND-ed): `systems`, `sites`, `tgids`, `units`, `types`, `emergency_only`
- 9 event types: `call_start`, `call_update`, `call_end`, `transcription`, `unit_event`, `recorder_update`, `rate_update`, `trunking_message`, `console`, plus the `auth` control event
- `Last-Event-ID` header or `last_event_id` query param (header wins) for gapless reconnect (60s server-side buffer); `EventBus.SubscribeSince` registers and snapshots the ring under the publish lock, so nothing is lost between replay and live
- Auth: `Authorization: Bearer` key, `?ticket=` (browser `EventSource`), or anonymous under the anonymous policy. The stream re-checks its principal every 60 s and on every auth-generation bump: it swaps the principal if `listen` remains, otherwise sends `event: auth` with `{"code": invalid_key|key_required|insufficient_scope|ticket_expired}` and closes. Ticket streams close when the ticket expires. Clients reconnect with a fresh ticket only on `ticket_expired`.
- 15s keepalive comments
- Server sends `X-Accel-Buffering: no` header for nginx compatibility
- To change filters: disconnect and reconnect with new query params

### SSE Filtering Details

All filters are AND-ed. Events carry `SystemID`, `SiteID`, `Tgid`, and `UnitID` metadata for server-side filtering. In the **client's** filters, events with a zero value for a field (e.g., `recorder_update` has `SystemID=0` since recorders are per-instance, not per-system) pass through that filter dimension.

Before the client's filters, `matchesFilter()` applies the subscriber's **principal** (stored on the subscriber, separate from its `EventFilter`), per event type (map in `internal/api/policy.go`, documented on `SSEEventType` in `openapi.yaml`): `console` needs `admin`; `call_*`, `transcription` and `unit_event` need `listen`, and restricted principals get them only when `SystemID != 0`, `Tgid != 0` and the restriction allows the pair (so unit on/off events are dropped); `recorder_update`, `rate_update`, `trunking_message` need `listen` and are dropped for restricted principals; any new type is `admin`-only until classified. Zero values never pass the restriction check. **When adding an SSE event type, classify it in that map and in `openapi.yaml`.**

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
| `{topic}/dvcf` | `handleDvcf` | _(none)_ | filesystem (`.dvcf`) | Low |

Trunking messages use a `Batcher` for CopyFrom batch inserts (same as raw messages and recorder snapshots). Console logs use simple single-row INSERT. The status handler caches TR instance status in-memory for the `/api/v1/health` endpoint rather than publishing SSE events.

## Deployed Instance

Production runs on `gerty` via Docker Compose.

### Access

| Service | URL | Port |
|---------|-----|------|
| tr-dashboard (UI) | `https://tr-dashboard.luxprimatech.com` | 80/443 (Caddy) |
| tr-engine API (direct) | `https://tr-engine.luxprimatech.com` | 80/443 (Caddy) |
| tr-engine API (Tailnet) | `http://gerty.pizzly-manta.ts.net:8080` | 8080 on host |

**For API testing from scripts/CLI, prefer the Tailnet URL** — it bypasses Cloudflare (no WAF blocking, no rate limiting, no bot detection).

### Architecture on gerty

- **tr-engine** runs via Docker Compose at `/docker/tr-engine` with its own PostgreSQL and Mosquitto containers
- **tr-dashboard** runs as a separate container (`ghcr.io/trunk-reporter/tr-dashboard:latest`), served by Caddy
- **Caddy** reverse proxies both domains, with Cloudflare in front (SSL: Full)
- **Auth model (public demo):** anonymous access policy `listen` (the legacy import set it from the old full-mode `AUTH_TOKEN` + `ADMIN_PASSWORD` config), so guests browse read-only with no key. Admins paste an `admin` or `edit` key into tr-dashboard (Settings → API key). No proxy injects anything. Manage keys and the policy on the host: `docker compose exec -T tr-engine tr-engine keys list` / `access show`.

### SSH

```bash
ssh root@gerty
```

### Docker Compose

```bash
# Location
cd /docker/tr-engine

# Status
docker compose ps
docker compose logs tr-engine --tail 20

# Restart (picks up new images, NOT .env changes)
docker compose up -d

# To pick up .env changes, must recreate:
docker compose up -d tr-engine   # recreates with new env

# Upgrade dashboard to new image
docker compose pull tr-dashboard && docker compose up -d --force-recreate tr-dashboard
```

**Important:** `docker compose restart` does NOT re-read `.env` — always use `docker compose up -d <service>` to recreate the container when env vars change.

### Dev Deploy Script

`deploy-dev.sh` cross-compiles a static binary (`CGO_ENABLED=0`) for linux/amd64, stops the container, uploads the binary, restarts, and pushes web files:

```bash
./deploy-dev.sh              # full deploy (binary + web + restart)
./deploy-dev.sh --web-only   # just push web files (no restart needed)
./deploy-dev.sh --binary-only # just push binary + restart
```

The binary must be statically linked (`CGO_ENABLED=0`) because the container is Alpine (musl, not glibc).

### Updating Web Files (No Rebuild)

Web files are embedded in the Go binary via `go:embed`, but the Docker deployment bind-mounts `./web:/opt/tr-engine/web` which overrides the embedded files. When a `web/` directory exists on disk, tr-engine serves from it instead — changes take effect on the next browser request with no restart.

```bash
scp web/*.html web/*.js root@gerty:/docker/tr-engine/web/
```

### File Layout on gerty

```
/docker/tr-engine/
├── docker-compose.yml     # PostgreSQL + Mosquitto + tr-engine + tr-dashboard + Caddy + debug-receiver
├── .env                   # All config (POSTGRES_PASSWORD, MQTT, STT, etc.; no auth variables)
├── Caddyfile              # Reverse proxy config (bind-mounted into caddy container)
├── tr-engine              # Binary (bind-mounted into container at /usr/local/bin/tr-engine)
├── debug-receiver         # Binary (bind-mounted into debug-receiver container)
├── data/
│   ├── db/                # PostgreSQL data
│   └── audio/             # Call audio files
└── web/                   # Bind-mounted into tr-engine container, overrides embedded UI
```

### Caddy Config

Located at `/docker/tr-engine/Caddyfile`, bind-mounted into the Caddy container.

```bash
ssh root@gerty "vi /docker/tr-engine/Caddyfile"
# After editing — must restart container (reload reads cached copy):
ssh root@gerty "cd /docker/tr-engine && docker compose restart caddy"
```

DNS is managed via Cloudflare (proxied). SSL mode is set globally to Full.

**No auth header injection.** The Caddyfile only proxies: `/api/*` on `tr-dashboard.luxprimatech.com` goes to `tr-engine:8080` unchanged, everything else to `tr-dashboard:3000`, and `tr-engine.luxprimatech.com` goes straight to tr-engine. Never add a `request_header ... Authorization` block: a key injected into visitors' requests is a key every visitor has. Public read access comes from the anonymous access policy. (The pre-upgrade injection of the old public `AUTH_TOKEN` is tolerated and logged as a WARN at most hourly; remove it, then `tr-engine access forget-retired-token`.)

```
tr-dashboard.luxprimatech.com {
    handle /api/* {
        reverse_proxy tr-engine:8080
    }
    handle {
        reverse_proxy tr-dashboard:3000
    }
}
```

**CORS:** nothing to configure. tr-engine answers every origin with `Access-Control-Allow-Origin: *` and never uses cookies or credentials, so `CORS_ORIGINS` no longer exists (a leftover value only logs a warning).

## Known trunk-recorder Issues (Potential Upstream Bug Reports)

### unit_event:end lags call_end by 3-4 seconds
`unit_event:end` arrives 3-4s after `call_end` for the same call. `call_end` fires from the recorder when voice frames stop (immediate), but `unit_event:end` fires from the control channel parser when it sees the deaffiliation message (delayed by the P25 trunking update cycle). TR should be able to detect unit transmission end from the recorder side (voice frames going null) rather than waiting for the control channel. The P25 channel stays allocated during hang time, but the actual voice traffic stops immediately. Event Horizon works around this with a 6s coalesce window.

### call ID shifts between call_start and call_end
The trunk-recorder call ID format `{sys_num}_{tgid}_{start_time}` embeds `start_time`, which can shift by 1-2 seconds between `call_start` and `call_end` messages. This causes the call_end handler to fail exact-match lookup. tr-engine works around this with fuzzy matching by `(tgid, start_time ± 5s)` in the active calls map.
