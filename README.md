# tr-engine

Backend service that ingests data from [trunk-recorder](https://github.com/robotastic/trunk-recorder) instances and serves it via a REST API with real-time streaming. Handles radio system monitoring data: calls, talkgroups, units, transcriptions, live audio, and recorder state.

Zero configuration for radio systems — tr-engine discovers systems, sites, talkgroups, and units automatically. Point it at a broker, a watch directory, or a trunk-recorder install, give it a database, and it figures out the rest.

> **Note:** This is a ground-up rewrite of the original tr-engine, now archived at [LumenPrima/tr-engine-v0](https://github.com/LumenPrima/tr-engine-v0). The database schema is not compatible. If you're coming from v0, see the **[migration guide](docs/migrating-from-v0.md)**.

## Screenshots

> Live demo: [tr-dashboard.luxprimatech.com](https://tr-dashboard.luxprimatech.com)

### Talkgroup Research — Browse View
Browse all discovered talkgroups with search, system filtering, and sortable columns. Card grid and list views available.

![Browse view — Crystal theme](docs/screenshots/tg-research-browse-crystal.png)

### Talkgroup Research — Detail View
Click any talkgroup to see stats, 24-hour activity timeline, site distribution, and encryption indicator.

![Detail view — Crystal theme](docs/screenshots/tg-research-detail-crystal.png)

### Units Tab — Top Talkers & Network Graph
Horizontal bar chart of most active units plus an interactive SVG network graph showing unit-to-talkgroup relationships.

![Units tab — Crystal theme](docs/screenshots/tg-research-units-crystal.png)

### Dark Theme (Night City)
All pages support 11 switchable themes. Here's the detail view with activity chart and calls tab.

![Detail view — Night City theme](docs/screenshots/tg-research-detail-header.png)
![Units tab — Night City theme](docs/screenshots/tg-research-units-tab.png)

## Tech Stack

- **Go** — multi-core utilization at high message rates
- **PostgreSQL 17+** — partitioned tables, JSONB, denormalized for read performance
- **MQTT + File Watch** — ingests from trunk-recorder via MQTT or filesystem monitoring (or both)
- **REST API** — 80+ endpoints under `/api/v1`, defined in `openapi.yaml`
- **SSE** — real-time event streaming with server-side filtering
- **Live Audio** — UDP simplestream ingest with per-talkgroup Opus encoding, WebSocket delivery
- **Transcription** — pluggable STT providers (Whisper, ElevenLabs, DeepInfra, IMBE ASR)
- **Web UI** — built-in dashboards and companion [tr-dashboard](https://github.com/trunk-reporter/tr-dashboard) React app

## Quick Start

Run this from your trunk-recorder directory (requires [Docker](https://docs.docker.com/get-docker/)):

```bash
curl -sL https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/install.sh | sh
```

That's it. The installer prints your **admin API key** (store it); open http://localhost:8080, paste the key when asked, and call recordings will appear as trunk-recorder captures them.

To remove: `cd tr-engine && docker compose down -v && cd .. && rm -rf tr-engine`

## Other Installation Methods

- **[Docker Compose](docs/docker.md)** — full setup with PostgreSQL, MQTT broker, and tr-engine
- **[Docker with existing MQTT](docs/docker-external-mqtt.md)** — connect to a broker you already run
- **[Docker full stack](docs/docker-full-stack.md)** — PostgreSQL + Mosquitto + tr-engine + tr-dashboard + Caddy
- **[Build from source](docs/getting-started.md)** — compile from source, bring your own PostgreSQL
- **[Binary releases](docs/binary-releases.md)** — download a pre-built binary, just add PostgreSQL
- **[HTTP Upload](docs/http-upload.md)** — ingest calls via trunk-recorder's rdio-scanner or OpenMHz upload plugins (no MQTT or shared filesystem needed)

## Updating

```bash
docker compose pull && docker compose up -d
```

Database and audio files persist in Docker volumes across updates.

> **API keys replace `AUTH_TOKEN` / `WRITE_TOKEN` / `ADMIN_PASSWORD` (v0.10.0).** Back up the database, upgrade tr-engine, tr-dashboard and any bind-mounted `web/` together, and follow the [auth migration guide](docs/migrating-auth.md). The first start imports what it safely can of the old settings.

> **Security defaults changed.** The shipped compose files no longer have a default database password (`POSTGRES_PASSWORD` is required), never publish PostgreSQL, require a login for the bundled MQTT broker, and bind published ports to `127.0.0.1` unless you set `HTTP_BIND_IP` / `MQTT_BIND_IP` / `BIND_IP`. Installs created with the old default database password (`trengine`) must rotate it before switching to the new files — see [Security defaults changed](docs/docker.md#security-defaults-changed).

## Authentication

tr-engine authenticates **client software, not people**. Every client (a tr-dashboard deployment, a script, a trunk-recorder upload plugin, a website's backend) holds its own **API key** and sends it as `Authorization: Bearer tre_...`. There are no user accounts, logins or cookies.

| Scope | Grants |
|-------|--------|
| `listen` | Read radio data, live events and audio |
| `edit` | Also change talkgroup/unit tags, transcriptions and unit-tag suggestions |
| `admin` | Everything: keys, anonymous access, merges, maintenance, SQL query, audit log |
| `upload` | Call upload only (for trunk-recorder's upload plugins) |

- **First run:** tr-engine prints an admin key once to its log. Create a named key per client with `tr-engine keys create` (in Docker: `docker compose exec -T tr-engine tr-engine keys ...`), or from tr-dashboard's Access page.
- **Anonymous access** (no key) is a policy you set: `off` (the default) or `listen`, optionally restricted to some systems or talkgroups (`tr-engine access set --anonymous listen`). Anonymous visitors can never edit.
- **Restrictions:** `listen` keys can be limited to some systems or talkgroups.
- **Tickets:** browsers can't send headers on `EventSource`, `<audio>` and WebSocket, so clients mint short-lived listen-only tickets (`POST /api/v1/tickets`) for those URLs. Keys never go in URLs.
- `GET /api/v1/whoami` tells any client what its credential can do.
- **A key that reaches other people's browsers is public.** Use anonymous access for public listening; never inject keys with a reverse proxy.

Operator and client-developer guide: **[docs/auth.md](docs/auth.md)**. Upgrading from `AUTH_TOKEN`/`WRITE_TOKEN`/`ADMIN_PASSWORD`: **[docs/migrating-auth.md](docs/migrating-auth.md)**.

## Configuration

Configuration is loaded in priority order: **CLI flags > environment variables > .env file > defaults**.

The `.env` file is auto-loaded from the current directory on startup. See `sample.env` for all available fields.

### CLI Flags

```
--listen        HTTP listen address (default :8080)
--log-level     debug, info, warn, error (default info)
--database-url  PostgreSQL connection URL
--mqtt-url      MQTT broker URL
--audio-dir     Audio file directory (default ./audio)
--watch-dir     Watch TR audio directory for new files
--tr-dir        Path to trunk-recorder directory for auto-discovery
--env-file      Path to .env file (default .env)
--version       Print version and exit
```

### Key Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | | PostgreSQL connection string |
| `MQTT_BROKER_URL` | * | | MQTT broker URL (e.g., `tcp://localhost:1883`) |
| `WATCH_DIR` | * | | Watch TR audio directory for new files |
| `TR_DIR` | * | | Path to trunk-recorder directory for auto-discovery |
| `MQTT_TOPICS` | No | `#` | MQTT topic filter (match your TR plugin prefix with `/#`) |
| `HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `RATE_LIMIT_RPS` | No | `20` | Per-IP rate limit (requests/second) for requests without a key, with a ticket or with a legacy key |
| `TRUSTED_PROXIES` | No | `loopback,private` | Reverse proxies whose `X-Forwarded-For` is believed |
| `AUDIO_DIR` | No | `./audio` | Audio file storage directory |
| `STT_PROVIDER` | No | `whisper` | Transcription provider: `whisper`, `elevenlabs`, `deepinfra`, `imbe` |
| `STREAM_LISTEN` | No | | UDP listen address for live audio (e.g., `:9123`) |
| `LOG_LEVEL` | No | `info` | Log level |

\* At least one of `MQTT_BROKER_URL`, `WATCH_DIR`, or `TR_DIR` must be set. All three can run simultaneously.

See `sample.env` for the full list including MQTT credentials, HTTP timeouts, transcription tuning, S3 storage, and retention settings. Access control is not configured with environment variables: API keys and the anonymous access policy live in the database ([docs/auth.md](docs/auth.md)).

### Audio Modes

tr-engine supports two modes for call audio:

- **MQTT audio (default):** trunk-recorder sends base64-encoded audio in MQTT messages. tr-engine decodes and saves the files to `AUDIO_DIR`. Enable with `mqtt_audio: true` in trunk-recorder's MQTT plugin config.

- **Filesystem audio (`TR_AUDIO_DIR`):** trunk-recorder saves audio to its local filesystem. tr-engine serves them directly. Set `TR_AUDIO_DIR` to trunk-recorder's `audioBaseDir`. When using this mode, set `mqtt_audio_type: none` in the TR plugin config to skip base64 encoding.

Both modes can coexist during a transition.

## How It Works

### Ingest Modes

tr-engine supports four ingest modes that can run independently or simultaneously:

- **MQTT** — subscribes to trunk-recorder's MQTT status plugin for real-time call events, unit activity, recorder state, decode rates, trunking messages, and console logs. The richest data source.
- **File Watch** (`WATCH_DIR`) — monitors trunk-recorder's audio output directory for new `.json` metadata files. Only produces `call_end` events. Backfills existing files on startup (`WATCH_BACKFILL_DAYS`).
- **TR Auto-Discovery** (`TR_DIR`) — the simplest setup. Point at trunk-recorder's directory. Auto-discovers capture directory, system names, imports talkgroup and unit CSVs. With `CSV_WRITEBACK=true`, alpha_tag edits are written back to the CSV files.
- **HTTP Upload** (`POST /api/v1/call-upload`) — accepts multipart uploads compatible with trunk-recorder's rdio-scanner and OpenMHz upload plugins. No local audio capture or MQTT broker required. Authenticates with an API key that has the `upload` scope, sent in the plugin's `apiKey` (form field `key`/`api_key`) or as a Bearer header.

### Auto-Discovery

tr-engine builds its model of the radio world automatically from incoming messages:

1. **Identifies systems** by matching P25 `(sysid, wacn)` pairs or conventional `(instance_id, sys_name)`
2. **Discovers sites** within each system — multiple TR instances monitoring the same P25 network auto-merge into one system with separate sites
3. **Tracks talkgroups and units** as they appear in call and unit events

```
System "MARCS" (P25 sysid=348, wacn=BEE00)
  |- Site "butco"  (nac=340, instance=tr-1)
  |- Site "warco"  (nac=34D, instance=tr-2)
  |- Talkgroups (shared across all sites)
  +- Units (shared across all sites)
```

### Data Flow

```
trunk-recorder  ──MQTT──>  broker  ──MQTT──>  tr-engine  ──REST/SSE──>  clients
      |                                            |
      +──audio files──>  fsnotify watcher ─────────+
      |                                            |
      +──HTTP upload──>  POST /call-upload ────────+
      |                                            |
      +──simplestream UDP──> audio router ─────────+──WebSocket──> live audio
                                                   v
                                               PostgreSQL
```

## Real-Time Event Streaming

`GET /api/v1/events/stream` pushes filtered events over SSE.

- **Filter params** (all optional, AND-ed): `systems`, `sites`, `tgids`, `units`, `types`, `emergency_only`
- **8 event types**: `call_start`, `call_update`, `call_end`, `unit_event`, `recorder_update`, `rate_update`, `trunking_message`, `console`
- **Compound type syntax**: `types=unit_event:call` filters by subtype
- **Reconnect**: `Last-Event-ID` header for gapless recovery (60s server-side buffer)

## Live Audio Streaming

`GET /audio/live` delivers real-time radio audio via WebSocket.

- **UDP ingest** from trunk-recorder's simplestream plugin (`STREAM_LISTEN`)
- **Per-talkgroup Opus encoding** (configurable bitrate, PCM passthrough option)
- **Multi-site deduplication** — same call from multiple sites sent once
- **Subscribe/unsubscribe filtering** by system IDs and talkgroup IDs
- **Browser playback** via AudioWorklet (`audio-engine.js` + `audio-worklet.js`)

## Transcription

Pluggable speech-to-text with four providers:

| Provider | Config | Notes |
|----------|--------|-------|
| Whisper | `STT_PROVIDER=whisper` + `WHISPER_URL` | Self-hosted or cloud Whisper-compatible API, including OpenAI `whisper-1`, `gpt-4o-transcribe`, `gpt-4o-mini-transcribe`, `gpt-transcribe`, and `gpt-4o-transcribe-diarize` (see `docs/docker.md`) |
| ElevenLabs | `STT_PROVIDER=elevenlabs` + `ELEVENLABS_API_KEY` | ElevenLabs Scribe API |
| DeepInfra | `STT_PROVIDER=deepinfra` + `DEEPINFRA_STT_API_KEY` | Hosted Whisper models |
| IMBE ASR | `STT_PROVIDER=imbe` + `IMBE_ASR_URL` | Transcribes directly from P25 IMBE codec frames via DVCF |

Features: configurable worker pool, queue size, duration filters, anti-hallucination parameters, `provider_ms` performance tracking, talkgroup include/exclude filtering.

**Unit tag suggestions** (opt-in, `UNIT_TAG_SUGGESTIONS=true`): a background scanner finds units identifying themselves in their own transmissions ("County, Medic 12 on scene") and queues candidate alpha tags in a review queue (`GET /unit-tag-suggestions`) with the transcript excerpt and call audio. Nothing changes a unit until a reviewer approves (optionally editing) a suggestion; dismissed candidates stay dismissed. Only unit designators are extracted ("Medic 12", "P338", "1 Paul 31"), not personal names; a transmission recorded by several sites counts once. See `sample.env` for the review thresholds.

## API

80+ endpoints under `/api/v1`. See `openapi.yaml` for the full specification, or open the built-in Swagger UI at `/docs.html`.

Before marking implementation work complete, follow the testing, API contract, and observability gates in [`docs/quality-gates.md`](docs/quality-gates.md).

### Key Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Service health (public); TR instance status with a key |
| `GET /whoami` | What the caller's credential can do (public) |
| `GET/POST /keys`, `PATCH/DELETE /keys/{id}` | API key management (admin) |
| `GET/PUT /anonymous-access` | Anonymous access policy (admin) |
| `POST /tickets` | Short-lived listen-only ticket for `EventSource`/`<audio>`/WebSocket URLs |
| `GET /admin/audit-log` | Changes made with API keys (admin) |
| `GET /systems` | List radio systems |
| `GET /talkgroups` | List talkgroups (filterable, sortable) |
| `GET /units` | List radio units |
| `GET /calls` | Call recordings (paginated, filterable) |
| `GET /calls/active` | Currently in-progress calls |
| `GET /calls/{id}/audio` | Stream call audio |
| `GET /calls/{id}/transcription` | Call transcription |
| `GET /transcriptions/search` | Full-text search across transcriptions |
| `GET /unit-events` | Unit event queries |
| `GET /unit-affiliations` | Live talkgroup affiliation state |
| `GET /unit-tag-suggestions` | Unit alpha tag review queue (approve/dismiss via POST) |
| `GET /call-groups` | Deduplicated call groups across sites |
| `GET /recorders` | Recorder hardware state |
| `GET /events/stream` | Real-time SSE event stream |
| `GET /audio/live` | Live audio WebSocket |
| `GET /stats` | System statistics |
| `GET /talkgroup-directory` | Talkgroup reference directory |
| `POST /call-upload` | Upload call recording (rdio-scanner/OpenMHz) |
| `POST /query` | Ad-hoc read-only SQL queries (admin) |
| `POST /admin/systems/merge` | Merge duplicate systems (admin) |
| `POST /debug-report` | Submit diagnostic report (admin) |

Every operation in `openapi.yaml` carries `x-scope` (`public`, `listen`, `edit`, `admin`, `upload`) and `x-restricted` (`enforced`, `deny`), so clients can tell what a key may call without hard-coding it.

## Web UI

tr-engine ships with built-in dashboards at `http://localhost:8080`. The index page auto-discovers all pages.

| Page | Description |
|------|-------------|
| **Event Horizon** | Logarithmic timeline — events drift from now into the past |
| **OmniTrunker** | Real-time system overview with active calls, recorders, and decode rates |
| **Live Events** | Real-time SSE event stream with type filtering |
| **Unit Tracker** | Live unit status grid with state colors and group filters |
| **IRC Radio Live** | IRC-style monitor — talkgroups as channels, units as nicks, audio playback |
| **Scanner** | Mobile-friendly radio scanner with auto-play and channel filtering |
| **Talkgroup Research** | Deep-dive analysis — browse, detail charts, unit network graph, call history with audio |
| **Talkgroup Directory** | Browse and import talkgroup reference data from CSV |
| **Call History** | Searchable call log with inline audio playback and transmission timeline |
| **Timeline** | Investigation timeline with talkgroup rows and call blocks |
| **Systems Overview** | System and site health dashboard |
| **Signal Flow** | Stream graph of talkgroup activity over time (D3.js) |
| **Analytics** | System-wide statistics and trends |
| **Admin** | API keys, anonymous access, audit log, maintenance controls (admin key) |
| **API Docs** | Interactive Swagger UI for the REST API |
| **Page Builder** | Generate custom dashboard pages with AI assistance |

Pages are plain HTML with no build step. They ask for an API key when they need one (a `listen` or `edit` key; the "API key…" menu item changes it) and work without one when anonymous access is on. Add new pages by dropping an `.html` file in `web/` with a `<meta name="card-title">` tag — see [CLAUDE.md](CLAUDE.md#web-frontend-page-registration) for the spec.

### tr-dashboard

For a full-featured React dashboard with talkgroup favorites, call playback, unit investigation, and live audio, see **[tr-dashboard](https://github.com/trunk-reporter/tr-dashboard)**. It connects to tr-engine's API with an API key you paste in (or anonymously, when the anonymous access policy allows it) and asks `/api/v1/whoami` what it may do.

## Storage Estimates

Observed with 2 moderately busy counties and 1 trunk-recorder instance:

| Category | Estimated Annual Usage |
|----------|----------------------|
| Database (permanent tables) | ~22 GB/year |
| Database (state + logs overhead) | ~3 GB steady-state |
| Audio files (M4A) | ~140 GB/year |

High-volume tables (calls, unit_events, trunking_messages) are automatically partitioned by month. Partition maintenance runs daily, creating partitions 3 months ahead. State tables are decimated (1/min after 1 week, 1/hour after 1 month). Configurable retention via `RETENTION_*` env vars.

## Project Structure

```
cmd/tr-engine/main.go           Entry point with CLI flag parsing
internal/
  auth/                         Scopes, restrictions, principals, tickets (no tr-engine deps)
  config/config.go              .env + env var + CLI config loading
  database/                     PostgreSQL connection pool + query files
  mqttclient/client.go          MQTT client with auto-reconnect
  ingest/
    pipeline.go                 MQTT message dispatch + batchers
    router.go                   Topic-to-handler routing
    identity.go                 System/site identity resolution + caching
    eventbus.go                 SSE pub/sub with ring buffer replay
    watcher.go                  fsnotify-based file watcher
    handler_*.go                Per-topic message handlers
  audio/
    simplestream.go             UDP listener for trunk-recorder simplestream
    router.go                   Identity resolution, dedup, encoding
    bus.go                      Pub/sub for audio frames
  transcribe/                   STT worker pool + provider implementations
  trconfig/
    trconfig.go                 TR config.json, docker-compose, and CSV parsers
    discover.go                 TR auto-discovery orchestrator
  api/
    server.go                   Chi router + HTTP server
    policy.go                   Route policy table (scope, restrictions, tickets) + SSE per-type scopes
    middleware.go               Auth pipeline, rate limiting, CORS, body limits
    events.go                   SSE event stream endpoint
    audio_stream.go             WebSocket live audio endpoint
    *.go                        Handler files for each resource
web/                            Built-in dashboards (auto-discovered by index)
openapi.yaml                    API specification (source of truth)
schema.sql                      PostgreSQL DDL (auto-applied on first run)
sample.env                      Configuration template
```

## Roadmap

See the [Trunk Reporter Roadmap](https://github.com/orgs/trunk-reporter/projects/1) for the cross-repo project tracker.

## License

MIT
