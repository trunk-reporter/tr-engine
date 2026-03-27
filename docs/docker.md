# Getting Started — Docker Compose

Run tr-engine with a single command. Docker Compose handles PostgreSQL, the MQTT broker, and tr-engine — you just need trunk-recorder pointed at the broker.

> **Don't have the MQTT plugin?** Run this from your trunk-recorder directory — no setup needed:
> ```bash
> curl -sL https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/install.sh | sh
> ```
> This installs everything (including PostgreSQL) and starts watching your call recordings automatically.

> **Other installation methods:**
> - **[Docker with existing MQTT](./docker-external-mqtt.md)** — connect to a broker you already run instead of bundling one
> - **[Full stack (HTTPS + Dashboard)](./docker-full-stack.md)** — production deployment with Caddy, Mosquitto, tr-dashboard, and Prometheus metrics
> - **[Build from source](./getting-started.md)** — compile everything yourself from scratch
> - **[Binary release](./binary-releases.md)** — download a pre-built binary, just add PostgreSQL and MQTT

## Prerequisites

- Docker and Docker Compose
- A running trunk-recorder instance with the [MQTT Status plugin](https://github.com/TrunkRecorder/trunk-recorder-mqtt-status)

## 1. Download and start

```bash
mkdir tr-engine && cd tr-engine
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/docker-compose.yml
docker compose up -d
```

That's it — one file, one command. On first run:
- PostgreSQL starts and tr-engine auto-applies the database schema
- Mosquitto starts on port **1883** (anonymous access)
- tr-engine connects to both and starts listening
- An API auth token is auto-generated and logged (see [Configuration](#configuration) to set a persistent one)

Verify it's running:

```bash
curl http://localhost:8080/api/v1/health
```

## 3. Point trunk-recorder at the broker

In your trunk-recorder `config.json`, set the MQTT plugin's broker to your Docker host:

```json
{
  "plugins": [
    {
      "name": "MQTT Status",
      "library": "libmqtt_status_plugin.so",
      "broker": "tcp://YOUR_DOCKER_HOST:1883",
      "topic": "trengine/feeds",
      "unit_topic": "trengine/units",
      "console_logs": true
    }
  ]
}
```

Replace `YOUR_DOCKER_HOST` with the IP or hostname of the machine running Docker. If trunk-recorder runs on the same machine, use `localhost`.

**The topic prefix is yours to choose.** tr-engine routes messages based on the trailing segments (e.g. `call_start`, `on`, `message`), not the prefix. Use any prefix you like — `trengine`, `myradio`, `robotastic` — as long as `MQTT_TOPICS` in your `.env` matches with a `/#` wildcard. The default is `#` (all topics), which works fine for a dedicated broker.

Once trunk-recorder connects, systems and talkgroups will auto-populate within seconds.

### MQTT authentication

By default, the bundled Mosquitto broker allows anonymous connections. To require authentication, set `MQTT_USERNAME` and `MQTT_PASSWORD` in your `.env`:

```bash
MQTT_USERNAME=myuser
MQTT_PASSWORD=mypassword
```

The same credentials configure both the broker and tr-engine's connection to it. When set, Mosquitto creates a password file at startup and rejects anonymous connections. Update your trunk-recorder plugin config to match:

```json
"broker": "tcp://YOUR_DOCKER_HOST:1883",
"mqtt_username": "myuser",
"mqtt_password": "mypassword"
```

### Raspberry Pi / ARM64 users

The official `robotastic/trunk-recorder` Docker image supports arm64 but doesn't include the MQTT plugin. If you're running trunk-recorder in Docker on a Pi and need MQTT, use our multi-arch image that bundles the plugin:

```yaml
trunk-recorder:
    image: ghcr.io/trunk-reporter/trunk-recorder-mqtt:latest
```

This is a drop-in replacement — same entrypoint, same config format. It includes trunk-recorder + the MQTT Status plugin pre-compiled for both amd64 and arm64.

If you don't need MQTT, you can skip the plugin entirely and use [file watch mode](#file-watch-mode-watch_dir) instead. You'll lose real-time `call_start` events, unit activity, and recorder state, but call recordings still flow in.

## 4. Access

- **Web UI:** http://localhost:8080
- **API:** http://localhost:8080/api/v1/health
- **API docs:** http://localhost:8080/docs.html

## Data

Two named volumes persist across restarts and upgrades:

| Volume | Contents | Path in container |
|--------|----------|-------------------|
| `tr-engine-db` | PostgreSQL data | `/var/lib/postgresql/data` |
| `tr-engine-audio` | Call audio files | `/data/audio` |

To back up the database (use your `POSTGRES_USER`/`POSTGRES_DB` if you changed them):

```bash
docker compose exec postgres pg_dump -U trengine trengine > backup.sql
```

## Configuration

tr-engine works with zero configuration — all defaults are built into `docker-compose.yml`. To customize, create a `.env` file next to your `docker-compose.yml`:

```bash
# Download the reference with all options documented
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/sample.env
cp sample.env .env
# Edit .env with your settings, then: docker compose up -d
```

Common settings:

```bash
AUTH_TOKEN=my-secret            # persistent API token (auto-generated if not set)
MQTT_TOPICS=trengine/#          # match your TR plugin's topic prefix (default: #)
# WRITE_TOKEN=my-write-secret   # separate token for write operations (see below)
# CORS_ORIGINS=https://example.com  # restrict CORS (empty = allow all)
LOG_LEVEL=info                  # debug, info, warn, error
# TR_DIR=/tr-config             # auto-discover from TR's config.json (see below)
# WATCH_DIR=/tr-audio           # file watch mode (alternative to MQTT)
```

Docker-specific settings (ignored when running the binary directly):

```bash
# POSTGRES_USER=trengine        # database credentials (default: trengine)
# POSTGRES_PASSWORD=trengine    # used by both postgres container and DATABASE_URL
# POSTGRES_DB=trengine
# HTTP_PORT=8080                # host port for the web UI / API
# MQTT_PORT=1883                # host port for the MQTT broker
```

See [`sample.env`](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) for all available options with descriptions.

> **Note:** `DATABASE_URL`, `MQTT_BROKER_URL`, and `AUDIO_DIR` are set automatically by `docker-compose.yml` and don't need to appear in `.env`. Database credentials flow from `POSTGRES_*` variables into both the postgres container and `DATABASE_URL` automatically.

Then restart: `docker compose up -d`

### Securing a public-facing instance

If your tr-engine instance is accessible from the internet, you should **always** set `WRITE_TOKEN` in your `.env`:

```bash
AUTH_TOKEN=my-read-token         # used by the web UI (served via /auth-init)
WRITE_TOKEN=my-write-secret      # required for all write operations
```

**Why this matters:** `AUTH_TOKEN` is automatically served to every browser that loads the web UI (via `GET /api/v1/auth-init`). When auth is enabled (the default), the API runs in **read-only mode** unless `WRITE_TOKEN` is set — all POST, PUT, PATCH, and DELETE requests are rejected with a 403 error. This includes call uploads from trunk-recorder.

Setting `WRITE_TOKEN` unlocks write operations for clients that present it. The web UI continues to work normally for viewing data using `AUTH_TOKEN`.

Use a strong, random value for `WRITE_TOKEN` and do **not** reuse your `AUTH_TOKEN`.

### TR auto-discovery (TR_DIR)

The simplest setup if trunk-recorder's directory is accessible. Add `TR_DIR` to your `.env` and bind-mount TR's directory in `docker-compose.yml`:

In `.env`:
```bash
TR_DIR=/tr-config
```

In `docker-compose.yml`, add a volume to the `tr-engine` service:
```yaml
    volumes:
      - /path/to/trunk-recorder:/tr-config:ro
      # If TR's audio is in a separate location, mount that too:
      # - /path/to/trunk-recorder/audio:/tr-audio:ro
```

This auto-discovers `captureDir` from `config.json` (sets `WATCH_DIR` + `TR_AUDIO_DIR`), system names, and imports talkgroup and unit tag CSVs. If TR runs in Docker, container paths are translated to host paths via volume mappings in `docker-compose.yaml`.

### File watch mode (WATCH_DIR)

To watch TR's audio directory for new files without the full auto-discovery, add to `.env`:

```bash
WATCH_DIR=/tr-audio
# WATCH_BACKFILL_DAYS=7  # days to backfill on startup (0=all, -1=none)
```

And add a volume in `docker-compose.yml`:
```yaml
    volumes:
      - /path/to/trunk-recorder/audio:/tr-audio:ro
```

Watch mode only produces `call_end` events. For `call_start`, unit events, and recorder state, add MQTT. Both modes can run simultaneously.

### Filesystem audio (TR_AUDIO_DIR)

Instead of receiving audio over MQTT as base64, tr-engine can serve audio files directly from trunk-recorder's filesystem. This avoids the encoding overhead and eliminates duplicate files.

To enable it, add to `.env`:
```bash
TR_AUDIO_DIR=/tr-audio
```

And bind-mount TR's audio directory in `docker-compose.yml`:
```yaml
    volumes:
      - /path/to/trunk-recorder/audio:/tr-audio:ro
```

When `TR_AUDIO_DIR` is set, tr-engine skips saving audio from MQTT and instead resolves files using the `call_filename` path that trunk-recorder reports at call_end. In your TR plugin config, keep `mqtt_audio: true` but set `mqtt_audio_type: none` — this sends the call metadata (frequencies, transmissions, unit list) without the base64 audio payload, saving encoding CPU and MQTT bandwidth.

Both modes coexist during a transition — existing MQTT-ingested audio still serves from `AUDIO_DIR`.

### Transcription (STT)

Transcription is optional. Add STT settings to your `.env` to enable automatic transcription of call recordings. Three provider options:

**Local Whisper (self-hosted):**

```bash
STT_PROVIDER=whisper
WHISPER_URL=http://whisper-server:8000/v1/audio/transcriptions
WHISPER_MODEL=deepdml/faster-whisper-large-v3-turbo-ct2
WHISPER_LANGUAGE=en
WHISPER_TEMPERATURE=0.1
TRANSCRIBE_WORKERS=2
# Optional — can improve recognition of domain terms but may cause
# hallucinations (Whisper repeats prompt words even in silence).
# Test with your audio before enabling in production.
# WHISPER_PROMPT=Police dispatch. Engine 7, Medic 23. 10-4, copy, en route.
# WHISPER_HOTWORDS=Medic,Engine,Ladder,Rescue,10-4
```

Requires an OpenAI-compatible Whisper server (e.g., [speaches-ai](https://github.com/speaches-ai/speaches)). See `tools/whisper-server/` for a ready-made Docker Compose.

**Remote Whisper (Groq, OpenAI, etc.):**

```bash
STT_PROVIDER=whisper
WHISPER_URL=https://api.groq.com/openai/v1/audio/transcriptions
WHISPER_API_KEY=gsk_your_api_key_here
WHISPER_MODEL=whisper-large-v3-turbo
WHISPER_LANGUAGE=en
WHISPER_TEMPERATURE=0.1
TRANSCRIBE_WORKERS=2
# Optional — see note above about hallucination risk.
# WHISPER_PROMPT=Police dispatch. Engine 7, Medic 23. 10-4, copy, en route.
```

Works with any OpenAI-compatible API. For OpenAI, use `https://api.openai.com/v1/audio/transcriptions` and model `whisper-1`.

**ElevenLabs:**

```bash
STT_PROVIDER=elevenlabs
ELEVENLABS_API_KEY=sk_your_api_key_here
ELEVENLABS_MODEL=scribe_v2
TRANSCRIBE_WORKERS=2
# Optional — boosts recognition of specific terms.
# Less prone to hallucination than Whisper prompts, but test first.
# ELEVENLABS_KEYTERMS=Medic,Engine,Ladder,Rescue,10-4
```

**Common tuning (all providers):**

```bash
TRANSCRIBE_QUEUE_SIZE=500       # max queued jobs (dropped when full)
TRANSCRIBE_MIN_DURATION=1.0     # skip calls shorter than 1s
TRANSCRIBE_MAX_DURATION=300     # skip calls longer than 5min
# PREPROCESS_AUDIO=true         # bandpass filter + normalize (requires sox)
```

Transcription auto-triggers on every `call_end` within the min/max duration range. See `sample.env` for the full list of Whisper tuning parameters including anti-hallucination options.

### Live Audio Streaming

Live audio streaming lets browser clients hear radio traffic in real time via the OmniTrunker and Scanner web pages. It uses trunk-recorder's simplestream plugin to send raw PCM audio over UDP, which tr-engine encodes and serves to browsers over WebSocket.

> **Secure context required:** Live audio uses the Web Audio API (`AudioContext` + `AudioWorklet`), which browsers only allow in [secure contexts](https://developer.mozilla.org/en-US/docs/Web/Security/Secure_Contexts). This means it works on:
> - `localhost` / `127.0.0.1` (always treated as secure)
> - Any `https://` URL
>
> It will **not** work over plain `http://` to a remote host — the browser silently blocks `AudioContext` creation. If you're accessing tr-engine from another machine, put a reverse proxy with TLS in front (Caddy, nginx + Let's Encrypt, Cloudflare Tunnel, etc.). See the [full stack guide](./docker-full-stack.md) for a production HTTPS setup.

**trunk-recorder side:** You must enable audio streaming globally **and** add the simplestream plugin. In your trunk-recorder `config.json`, set `"audioStreaming": true` at the top level:

```json
{
  "audioStreaming": true
}
```

Without this, the simplestream plugin silently does nothing.

Then add the simplestream plugin:

```json
{
  "plugins": [
    {
      "name": "simplestream",
      "library": "libsimplestream.so",
      "streams": [
        {
          "address": "YOUR_DOCKER_HOST",
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

Set `address` to the IP or hostname of the machine running Docker. Use `TGID: 0` for all talkgroups, `shortName: ""` for all systems.

**tr-engine side:** Add to your `.env`:

```bash
STREAM_LISTEN=:9123              # enables the UDP listener (disabled if not set)
# STREAM_SAMPLE_RATE=8000        # 8000 for P25, 16000 for analog
# STREAM_OPUS_BITRATE=16000      # Opus encoder bitrate (bps)
# STREAM_MAX_CLIENTS=50          # max concurrent WebSocket listeners
# STREAM_IDLE_TIMEOUT=30s        # tear down idle per-talkgroup encoders
```

**Docker port mapping:** Add the UDP port to the `tr-engine` service in `docker-compose.yml`:

```yaml
  tr-engine:
    ports:
      - "${HTTP_PORT:-8080}:8080"
      - "${STREAM_PORT:-9123}:9123/udp"
    environment:
      - STREAM_LISTEN=:9123
```

Or set `STREAM_PORT` in `.env` to change the host port mapping (the container-internal port stays 9123).

Restart with `docker compose up -d`. Verify via the health endpoint — a new `audio_stream` section appears when streaming is enabled.

> **Note:** Streaming works alongside MQTT, not as a replacement. MQTT provides call metadata, talkgroup names, unit events, etc. Simplestream adds live audio on top.

### Custom web UI files

The web UI is embedded in the binary, but you can override it by mounting a local directory:

```yaml
volumes:
  - ./web:/opt/tr-engine/web
```

When a `web/` directory exists on disk, tr-engine serves from it instead of the embedded files. Changes take effect on the next browser request — no restart needed. This is useful for iterating on the UI without rebuilding the Docker image.

To pull the latest web UI files from GitHub without rebuilding:

**Linux/Mac:**
```bash
mkdir -p web && cd web && curl -s https://api.github.com/repos/trunk-reporter/tr-engine/contents/web | python3 -c "import json,sys,urllib.request; [urllib.request.urlretrieve(f['download_url'],f['name']) for f in json.load(sys.stdin) if f['type']=='file']"
```

**Windows (PowerShell):**
```powershell
mkdir -Force web; (irm https://api.github.com/repos/trunk-reporter/tr-engine/contents/web) | ? type -eq file | % { iwr $_.download_url -Out "web/$($_.name)" }
```

Run from the directory containing your `docker-compose.yml`. Changes take effect on the next browser refresh — no restart needed.

## Upgrading

```bash
docker compose pull && docker compose up -d
```

The database volume persists — your data is safe. If a release includes schema migrations, they'll be noted in the release notes.

## Logs

```bash
# All services
docker compose logs -f

# Just tr-engine
docker compose logs -f tr-engine
```

## Stopping

```bash
# Stop (data preserved)
docker compose down

# Stop and delete all data (fresh start)
docker compose down -v
```
