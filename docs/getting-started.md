# Getting Started — Build from Source

This guide walks through setting up tr-engine from scratch on bare metal: installing each component yourself, building from source, and wiring everything together.

> **Other installation methods:**
> - **[Docker Compose](./docker.md)** — single `docker compose up` with everything pre-configured
> - **[Docker with existing MQTT](./docker-external-mqtt.md)** — Docker Compose connecting to a broker you already run
> - **[Full stack (HTTPS + Dashboard)](./docker-full-stack.md)** — production deployment with Caddy, Mosquitto, tr-dashboard, and Prometheus metrics
> - **[Binary releases](./binary-releases.md)** — download a pre-built binary, just add PostgreSQL and MQTT

## Architecture

```
trunk-recorder  ──MQTT──>  broker  ──MQTT──>  tr-engine  ──REST/SSE──>  clients
   (radio)      ──files──>  audio dir ──fsnotify──>  |                  (web UI)
                                                     v
                                                 PostgreSQL
```

trunk-recorder captures radio traffic from SDR hardware. tr-engine can ingest data via **MQTT** (real-time events), **file watching** (monitoring TR's audio output directory), or both. Data is stored in PostgreSQL and exposed via a REST API with real-time SSE streaming.

## 1. MQTT Broker (optional)

> **MQTT is optional.** If you only need call recordings, you can skip MQTT entirely and use file watch mode or TR auto-discovery instead — see sections 4 and 5. MQTT provides richer data: real-time `call_start` events, unit activity, recorder state, and decode rates.

tr-engine needs an MQTT broker between it and trunk-recorder. Mosquitto is the simplest choice.

### Install

```bash
# Debian/Ubuntu
sudo apt install mosquitto mosquitto-clients

# macOS
brew install mosquitto

# Docker
docker run -d --name mosquitto -p 1883:1883 eclipse-mosquitto
```

### Configure

For a local setup, the default config works fine (anonymous access on port 1883). For remote access, create `/etc/mosquitto/conf.d/listener.conf`:

```
listener 1883
allow_anonymous true
```

Restart with `sudo systemctl restart mosquitto`.

### Verify

```bash
# In one terminal, subscribe to all topics:
mosquitto_sub -t '#' -v

# In another, publish a test message:
mosquitto_pub -t 'test' -m 'hello'
```

## 2. PostgreSQL

tr-engine requires PostgreSQL 17+. It uses partitioned tables, JSONB, and GIN indexes.

### Install

```bash
# Debian/Ubuntu (via official PostgreSQL apt repo)
sudo apt install postgresql-17

# macOS
brew install postgresql@17

# Docker
docker run -d --name postgres -p 5432:5432 -e POSTGRES_PASSWORD=secret postgres:17
```

### Create database and user

```bash
sudo -u postgres psql
```

```sql
CREATE USER trengine WITH PASSWORD 'your_password_here';
CREATE DATABASE trengine OWNER trengine;
\q
```

### Load the schema

tr-engine auto-applies `schema.sql` on first startup when it detects an empty database — no manual step needed.

To apply it manually instead (e.g., to inspect the schema before starting):

```bash
psql -U trengine -d trengine -f schema.sql
```

This creates all tables, indexes, triggers, partition functions, and initial partitions (current month + 3 months ahead). Partition maintenance runs automatically within tr-engine after that.

## 3. trunk-recorder

[trunk-recorder](https://github.com/robotastic/trunk-recorder) captures P25/SmartNet/conventional radio traffic using SDR hardware (RTL-SDR, HackRF, etc.). Full setup is covered in the [trunk-recorder docs](https://trunkrecorder.com/docs/Install). This section covers only the MQTT plugin configuration that tr-engine needs.

### Install the MQTT plugin

The MQTT Status plugin is a separate plugin, not built into trunk-recorder core.

```bash
# Install MQTT libraries
sudo apt install libpaho-mqtt-dev libpaho-mqttpp-dev

# Clone the plugin into your trunk-recorder source tree
cd trunk-recorder/user_plugins/
git clone https://github.com/TrunkRecorder/trunk-recorder-mqtt-status.git

# Rebuild trunk-recorder (from the build directory)
make install
```

A Docker image with the plugin pre-integrated is also available:
```bash
docker pull thegreatcodeholio/trunk-recorder-mqtt:latest
```

### Configure the plugin

Add the plugin to your trunk-recorder `config.json`:

```json
{
  "plugins": [
    {
      "name": "MQTT Status",
      "library": "libmqtt_status_plugin.so",
      "broker": "tcp://localhost:1883",
      "topic": "trengine/feeds",
      "unit_topic": "trengine/units",
      "console_logs": true,
      "instanceId": "my-site"
    }
  ]
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `broker` | Yes | MQTT broker URL (`tcp://host:1883` or `ssl://host:8883`) |
| `topic` | Yes | Base topic for call/recorder/system events |
| `unit_topic` | No | Topic prefix for unit events (on/off/call/join). Recommended. |
| `message_topic` | No | Topic prefix for trunking messages. **Very high volume** — omit unless you specifically need trunking message data. |
| `console_logs` | No | Publish TR console output over MQTT (default: false) |
| `mqtt_audio` | No | Send audio as base64 in MQTT messages (default: false). Set to `true` with `mqtt_audio_type: none` when using `TR_AUDIO_DIR` (see below). |
| `mqtt_audio_type` | No | Which audio format to include: `wav`, `m4a`, `both`, or `none` (default: `wav`). Set to `none` to send only call metadata without base64 audio — saves encoding overhead and bandwidth. |
| `instanceId` | No | Identifier for this TR instance (default: `trunk-recorder`) |
| `username` | No | MQTT broker credentials |
| `password` | No | MQTT broker credentials |

**The topic prefix is yours to choose.** The values above use `trengine/feeds` and `trengine/units`, but you can use any prefix — `myradio/feeds`, `robotastic/feeds`, etc. tr-engine routes messages based on the trailing segments (`call_start`, `on`, `message`, etc.), not the prefix. Just make sure `MQTT_TOPICS` in your tr-engine config matches with a `/#` wildcard (e.g. `MQTT_TOPICS=trengine/#`).

**Topic structure produced:**

With the config above, the plugin publishes to:
- `{topic}/call_start`, `{topic}/call_end`, `{topic}/recorders`, `{topic}/rates`, etc.
- `{unit_topic}/{sys_name}/on`, `{unit_topic}/{sys_name}/call`, `{unit_topic}/{sys_name}/join`, etc.

### Multiple trunk-recorder instances

If you have multiple TR instances monitoring the same P25 network from different sites, point them all at the same MQTT broker with the same topic prefix. tr-engine will auto-merge them into a single system with separate sites based on the P25 system ID (sysid/wacn).

## 4. tr-engine

### Build

```bash
git clone https://github.com/trunk-reporter/tr-engine.git
cd tr-engine
bash build.sh
```

Or manually:
```bash
go build -o tr-engine ./cmd/tr-engine
```

### Configure

```bash
cp sample.env .env
```

Edit `.env` with your values:

```env
# Required
DATABASE_URL=postgres://trengine:your_password_here@localhost:5432/trengine?sslmode=disable

# Ingest mode — at least one of these must be set:
MQTT_BROKER_URL=tcp://localhost:1883     # MQTT ingest (richest data)
# WATCH_DIR=/path/to/trunk-recorder/audio  # File watch (call recordings only)
# TR_DIR=/path/to/trunk-recorder           # Auto-discover from TR's config.json

# Match your TR plugin's topic prefix + wildcard (only needed for MQTT)
MQTT_TOPICS=trengine/#

# Optional
HTTP_ADDR=:8080
LOG_LEVEL=info
AUDIO_DIR=./audio

# To serve audio directly from trunk-recorder's filesystem instead of
# receiving it over MQTT, set TR_AUDIO_DIR to TR's audioBaseDir:
# TR_AUDIO_DIR=/path/to/trunk-recorder/audio

# Authentication
# AUTH_TOKEN=my-secret           # API token (auto-generated if not set)
# WRITE_TOKEN=my-write-secret    # separate token for write operations
```

> **Public-facing instances:** When auth is enabled (the default), the API runs in **read-only mode** unless `WRITE_TOKEN` is set — all POST, PUT, PATCH, and DELETE requests are rejected with 403. This prevents the browser-visible `AUTH_TOKEN` (served via `/api/v1/auth-init`) from granting write access. Set `WRITE_TOKEN` to a strong, random value to enable writes for trusted services (trunk-recorder upload plugins, admin scripts).

`MQTT_TOPICS` must match the topic prefixes from your TR plugin config. If all your TR topics share a common root (e.g. `topic: "trengine/feeds"`, `unit_topic: "trengine/units"`), a single wildcard like `trengine/#` covers everything. If they differ, comma-separate them: `MQTT_TOPICS=prefix1/#,prefix2/#`.

### TR Auto-Discovery (simplest setup)

If trunk-recorder runs on the same machine (or its directory is accessible), set `TR_DIR` to the directory containing TR's `config.json`:

```env
TR_DIR=/home/radio/trunk-recorder
```

This auto-discovers:
- **`captureDir`** from `config.json` — sets `WATCH_DIR` and `TR_AUDIO_DIR` automatically
- **System names** — from `config.json` system entries
- **Talkgroup and unit names** — imports TR's talkgroup CSV and unit tag CSV files
- **Docker volume mappings** — if `docker-compose.yaml` is found, translates container paths to host paths

`TR_DIR` replaces `WATCH_DIR`, `TR_AUDIO_DIR`, and manual talkgroup import. All three ingest modes (MQTT, watch, TR_DIR) can run simultaneously.

### File Watch Mode

To use file watching without the full auto-discovery, set `WATCH_DIR` directly:

```env
WATCH_DIR=/path/to/trunk-recorder/audio
WATCH_INSTANCE_ID=my-site          # default: file-watch
WATCH_BACKFILL_DAYS=7              # days to backfill on startup (0=all, -1=none)
```

Watch mode monitors TR's audio output directory for new `.json` metadata files. It only produces `call_end` events — for `call_start`, unit events, recorder state, and decode rates, add MQTT.

### Run

```bash
./tr-engine
```

tr-engine auto-loads `.env` from the current directory. You can also use CLI flags:

```bash
./tr-engine --listen :9090 --log-level debug --database-url postgres://...
```

### Verify

```bash
# Health check — shows database, MQTT, and TR instance status
curl http://localhost:8080/api/v1/health

# List systems (populated after TR connects and sends data)
curl http://localhost:8080/api/v1/systems

# List talkgroups
curl http://localhost:8080/api/v1/talkgroups?limit=10

# Watch live events
curl -N http://localhost:8080/api/v1/events/stream
```

### Web UI

tr-engine serves static files from a `web/` directory in dev mode. Open `http://localhost:8080/irc-radio-live.html` for an IRC-style live radio monitor.

## 5. Live Audio Streaming (optional)

Live audio streaming lets browser clients hear radio traffic in real time via the OmniTrunker and Scanner web pages. It works alongside MQTT — MQTT provides call metadata, simplestream provides raw audio.

> **Secure context required:** Live audio uses the Web Audio API (`AudioContext` + `AudioWorklet`), which browsers only allow in [secure contexts](https://developer.mozilla.org/en-US/docs/Web/Security/Secure_Contexts). This means it works on:
> - `localhost` / `127.0.0.1` (always treated as secure)
> - Any `https://` URL
>
> It will **not** work over plain `http://` to a remote host — the browser silently blocks `AudioContext` creation. If you're accessing tr-engine from another machine, you need HTTPS (e.g., a reverse proxy with TLS, or Cloudflare Tunnel).

### trunk-recorder side

**Important:** You must enable audio streaming globally in trunk-recorder's `config.json` **and** add the simplestream plugin. Both are required.

**1. Enable audioStreaming** in the top-level config:

```json
{
  "audioStreaming": true
}
```

Without this, the simplestream plugin silently does nothing.

**2. Add the simplestream plugin:**

```json
{
  "plugins": [
    {
      "name": "simplestream",
      "library": "libsimplestream.so",
      "streams": [
        {
          "address": "YOUR_TR_ENGINE_HOST",
          "port": 9123,
          "TGID": 0,
          "sendJSON": true,
          "shortName": ""
        }
      ]
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `address` | IP or hostname of the machine running tr-engine |
| `port` | Must match the port in `STREAM_LISTEN` (default: 9123) |
| `talkgroup` | `0` = stream all talkgroups |
| `sendJSON` | `true` = include talkgroup/unit metadata with each audio packet (recommended) |
| `shortName` | `""` = stream all systems. Set to a specific `sys_name` to limit to one system. |

### tr-engine side

Add to your `.env`:

```env
# Enable live audio streaming (disabled if not set)
STREAM_LISTEN=:9123

# Optional tuning
# STREAM_SAMPLE_RATE=8000       # 8000 for P25, 16000 for analog
# STREAM_OPUS_BITRATE=16000     # Opus encoder bitrate (bps), 0 = PCM passthrough
# STREAM_MAX_CLIENTS=50         # max concurrent WebSocket listeners
# STREAM_IDLE_TIMEOUT=30s       # tear down idle per-talkgroup encoders
```

Restart tr-engine after changing `.env`.

### Verify

Check the health endpoint — a new `audio_stream` section appears when streaming is enabled:

```bash
curl http://localhost:8080/api/v1/health
```

> **Note:** simplestream sends raw PCM audio over UDP. This is separate from the MQTT feed — you still need MQTT (or file watch) for call metadata, talkgroup names, unit events, etc. Streaming adds live audio on top of the existing data pipeline.

## What happens on first run

1. tr-engine connects to PostgreSQL (and MQTT if configured)
2. If `TR_DIR` is set, reads TR's `config.json` and imports talkgroup CSVs into the reference directory
3. If `WATCH_DIR` is set (directly or via `TR_DIR`), backfills existing audio files and starts watching for new ones
4. If MQTT is configured, subscribes to the configured topics
5. Systems, sites, talkgroups, and units auto-populate as data flows
6. The SSE event stream (`/api/v1/events/stream`) begins pushing events to connected clients

There's no manual system/site/talkgroup configuration needed — everything is discovered automatically. Talkgroup names come from TR's CSV files (via `TR_DIR` auto-import or CSV upload at `/api/v1/talkgroup-directory/import`) and from call metadata in the MQTT feed.

## Troubleshooting

**No systems appearing (MQTT mode):** Check that trunk-recorder is running, connected to the MQTT broker, and publishing messages. Use `mosquitto_sub -t '#' -v` to verify messages are flowing.

**No systems appearing (watch mode):** Check that `WATCH_DIR` points to TR's audio output directory (the one containing per-system subdirectories like `butco/`, `warco/`). Verify `.json` metadata files exist alongside the audio files.

**MQTT connection failing:** Verify `MQTT_BROKER_URL` matches your broker's address and port. Check firewall rules if the broker is remote.

**Database errors on startup:** Ensure `schema.sql` was loaded successfully. Check that the database user has ownership of all tables.

**Audio playback not working:** tr-engine serves audio from `AUDIO_DIR` (for MQTT-ingested audio) or `TR_AUDIO_DIR` (for filesystem audio). If using MQTT audio, ensure `mqtt_audio: true` is set in the TR plugin config. If using filesystem audio, set `TR_AUDIO_DIR` to trunk-recorder's `audioBaseDir` and keep `mqtt_audio: true` with `mqtt_audio_type: none` in the TR plugin config. Ensure tr-engine can read the audio files — on the same machine, just point to the directory; in Docker, bind-mount TR's audio directory into the container.
