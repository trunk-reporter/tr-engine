# Getting Started — Docker Compose

Run tr-engine with Docker Compose: a few setup commands, then one `docker compose up`. Compose handles PostgreSQL, the MQTT broker, and tr-engine — you just need trunk-recorder pointed at the broker.

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

## 1. Download

```bash
mkdir tr-engine && cd tr-engine
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/docker-compose.yml
mkdir -p mosquitto
curl -so mosquitto/mosquitto.conf https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/mosquitto/mosquitto.conf
```

## 2. Create credentials

There are no default passwords. Compose refuses to start until `POSTGRES_PASSWORD` and `MQTT_PASSWORD` are set, and the bundled Mosquitto broker rejects clients that don't log in. Run these once, in the same directory:

```bash
export MQTT_USERNAME=trengine
export MQTT_PASSWORD=$(openssl rand -hex 16)

# .env: database password + MQTT login for tr-engine
cat > .env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
MQTT_USERNAME=$MQTT_USERNAME
MQTT_PASSWORD=$MQTT_PASSWORD
EOF
chmod 600 .env

# mosquitto/passwd: the same MQTT login, hashed for the broker
docker run --rm -e MQTT_USERNAME -e MQTT_PASSWORD -v "$PWD/mosquitto:/mosquitto/config" eclipse-mosquitto:2 \
  sh -c 'mosquitto_passwd -c -b /mosquitto/config/passwd "$MQTT_USERNAME" "$MQTT_PASSWORD" && chown mosquitto:mosquitto /mosquitto/config/passwd'

echo "trunk-recorder MQTT login: $MQTT_USERNAME / $MQTT_PASSWORD"
```

Keep the MQTT password handy; trunk-recorder needs it in step 4. (It is also in `.env`.) The `chown` matters: Mosquitto runs as its own user and can't read a password file owned by root.

## 3. Start

```bash
docker compose up -d postgres mosquitto tr-engine
```

`docker-compose.yml` also defines `tr-dashboard` and `caddy` for HTTPS deployments; they need a Caddyfile and aren't part of this quick start (see the [full stack guide](./docker-full-stack.md)).

On first run:
- PostgreSQL starts and tr-engine auto-applies the database schema
- Mosquitto starts on `127.0.0.1:1883` and requires the login you just created
- tr-engine connects to both and serves the web UI/API on `127.0.0.1:8080`
- tr-engine creates an **admin API key** and prints it once to its log (see [First run: your admin key](#first-run-your-admin-key)). Anonymous access is off: the API and web pages need a key until you decide otherwise.

PostgreSQL is never published on the host, and every published port listens on `127.0.0.1` (this machine only) until you choose otherwise. See [Network exposure](#network-exposure).

Verify it's running (`/health` is public; it needs no key):

```bash
curl http://localhost:8080/api/v1/health
```

### First run: your admin key

On its first start with an empty database, tr-engine creates a key named `bootstrap admin` and prints it once, in a box, to its log:

```bash
docker compose logs tr-engine | grep -A3 "no admin API key"
```

Copy the `tre_...` line into a password manager now; it is never shown again. Check it works:

```bash
curl -H "Authorization: Bearer tre_..." http://localhost:8080/api/v1/whoami
```

Then create a named key for each client and revoke the bootstrap key. The `tr-engine keys` command runs inside the container; `-T` lets you capture its output:

```bash
docker compose exec -T tr-engine tr-engine keys create --name "admin (me)" --scopes admin
docker compose exec -T tr-engine tr-engine keys create --name "web browser" --scopes edit
docker compose exec -T tr-engine tr-engine keys list
docker compose exec -T tr-engine tr-engine keys revoke --prefix tre_xxxxxxxx   # the bootstrap key's prefix
```

Paste a key into the web UI when it asks ("API key…" in the menu). To let anyone who can reach tr-engine listen without a key:

```bash
docker compose exec -T tr-engine tr-engine access set --anonymous listen
```

If you lose every admin key, create a new one the same way: `docker compose exec -T tr-engine tr-engine keys create --name "admin" --scopes admin`. See [auth.md](./auth.md) for keys, scopes, anonymous access and restrictions.

## 4. Point trunk-recorder at the broker

In your trunk-recorder `config.json`, set the MQTT plugin's broker and the login from step 2:

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
      "username": "trengine",
      "password": "YOUR_MQTT_PASSWORD"
    }
  ]
}
```

`tcp://localhost:1883` works when trunk-recorder runs directly on the Docker host. If it runs on another machine, set `MQTT_BIND_IP` in `.env` to the Docker host's LAN (or VPN) address, run `docker compose up -d mosquitto`, and use `tcp://THAT_ADDRESS:1883` as the broker.

**The topic prefix is yours to choose.** tr-engine routes messages based on the trailing segments (e.g. `call_start`, `on`, `message`), not the prefix. Use any prefix you like — `trengine`, `myradio`, `robotastic` — as long as `MQTT_TOPICS` in your `.env` matches with a `/#` wildcard. The default is `#` (all topics), which works fine for a dedicated broker.

Once trunk-recorder connects, systems and talkgroups will auto-populate within seconds.

### MQTT authentication

The bundled broker always requires a login (`allow_anonymous false` in `mosquitto/mosquitto.conf`). tr-engine logs in with `MQTT_USERNAME`/`MQTT_PASSWORD` from `.env`; the broker checks them against `mosquitto/passwd`. To change the password, update `.env`, regenerate the file with the `docker run ... mosquitto_passwd` command from step 2, update trunk-recorder's `config.json`, then:

```bash
docker compose restart mosquitto && docker compose up -d tr-engine
```

To give trunk-recorder its own login, add a user to the existing file (no `-c`, which would overwrite it):

```bash
docker run --rm -v "$PWD/mosquitto:/mosquitto/config" eclipse-mosquitto:2 \
  sh -c 'mosquitto_passwd -b /mosquitto/config/passwd trunk-recorder "NEW_PASSWORD" && chown mosquitto:mosquitto /mosquitto/config/passwd'
docker compose restart mosquitto
```

### Raspberry Pi / ARM64 users

The official `robotastic/trunk-recorder` Docker image supports arm64 but doesn't include the MQTT plugin. If you're running trunk-recorder in Docker on a Pi and need MQTT, use our multi-arch image that bundles the plugin:

```yaml
trunk-recorder:
    image: ghcr.io/trunk-reporter/trunk-recorder-mqtt:latest
```

This is a drop-in replacement — same entrypoint, same config format. It includes trunk-recorder + the MQTT Status plugin pre-compiled for both amd64 and arm64.

If you don't need MQTT, you can skip the plugin entirely and use [file watch mode](#file-watch-mode-watch_dir) instead. You'll lose real-time `call_start` events, unit activity, and recorder state, but call recordings still flow in.

## 5. Access

- **Web UI:** http://localhost:8080
- **API:** http://localhost:8080/api/v1/health
- **API docs:** http://localhost:8080/docs.html

These addresses work on the Docker host. To reach tr-engine from other machines, see [Network exposure](#network-exposure).

## Network exposure

Every published port is bound to `127.0.0.1` by default. Opening one to other machines is an explicit setting in `.env`:

| Variable | Port | Default | When to change it |
|----------|------|---------|-------------------|
| `HTTP_BIND_IP` | tr-engine `8080` | `127.0.0.1` | To use the web UI/API from other machines. Decide on anonymous access first (see [Securing a public-facing instance](#securing-a-public-facing-instance)). |
| `MQTT_BIND_IP` | Mosquitto `1883` | `127.0.0.1` | When trunk-recorder runs on another machine. Login is still required. |
| `BIND_IP` | Caddy `80`/`443` | `127.0.0.1` | Only if you run the `caddy` service; see the [full stack guide](./docker-full-stack.md). |

Use a specific LAN or VPN address (for example `192.168.1.20` or a Tailscale IP) rather than `0.0.0.0` when you can. `0.0.0.0` means every interface, including a public one if the machine has it. PostgreSQL has no host port at all; use `docker compose exec postgres psql -U trengine trengine` to reach it.

After changing any of these, run `docker compose up -d`.

## Data

Data persists across restarts and upgrades in directories next to your `docker-compose.yml`:

| Location | Contents | Path in container |
|----------|----------|-------------------|
| `./pgdata` | PostgreSQL data | `/var/lib/postgresql/data` |
| `./audio` | Call audio files | `/data/audio` |
| `./mosquitto` | Broker config and password file | `/mosquitto/config` |

To back up the database (use your `POSTGRES_USER`/`POSTGRES_DB` if you changed them):

```bash
docker compose exec -T postgres pg_dump -U trengine trengine > backup.sql
```

## Configuration

Apart from the credentials in `.env` (step 2), all defaults are built into `docker-compose.yml`. To customize, add settings to the same `.env` file:

```bash
# Download the reference with all options documented
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/sample.env
# Copy the settings you want from sample.env into .env (don't overwrite .env —
# it holds your passwords), then: docker compose up -d
```

Common settings:

```bash
MQTT_TOPICS=trengine/#          # match your TR plugin's topic prefix (default: #)
LOG_LEVEL=info                  # debug, info, warn, error
# TRUSTED_PROXIES=loopback,private  # proxies whose X-Forwarded-For is believed
# TR_DIR=/tr-config             # auto-discover from TR's config.json (see below)
# WATCH_DIR=/tr-audio           # file watch mode (alternative to MQTT)
```

Docker-specific settings (ignored when running the binary directly):

```bash
POSTGRES_PASSWORD=...           # REQUIRED, no default (openssl rand -hex 24); used by postgres and DATABASE_URL
MQTT_USERNAME=trengine          # login for the bundled broker (default: trengine)
MQTT_PASSWORD=...               # REQUIRED, must match mosquitto/passwd
# POSTGRES_USER=trengine        # database user (default: trengine)
# POSTGRES_DB=trengine
# HTTP_PORT=8080                # host port for the web UI / API
# MQTT_PORT=1883                # host port for the MQTT broker
# HTTP_BIND_IP=127.0.0.1        # see Network exposure
# MQTT_BIND_IP=127.0.0.1
```

See [`sample.env`](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) for all available options with descriptions.

> **Note:** `DATABASE_URL`, `MQTT_BROKER_URL`, and `AUDIO_DIR` are set automatically by `docker-compose.yml` and don't need to appear in `.env`. Database credentials flow from `POSTGRES_*` variables into both the postgres container and `DATABASE_URL` automatically.

Then restart: `docker compose up -d`

Access control is not configured in `.env`: it lives in the database and is managed with API keys (`tr-engine keys`) and the anonymous access policy (`tr-engine access`). See [auth.md](./auth.md).

### Securing a public-facing instance

Before exposing tr-engine beyond your machine, decide what people **without** a key may do:

```bash
docker compose exec -T tr-engine tr-engine access show
# Nobody without a key (the default):
docker compose exec -T tr-engine tr-engine access set --anonymous off
# Public listening, except sensitive talkgroups:
docker compose exec -T tr-engine tr-engine access set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001
```

Anonymous visitors can at most listen; editing needs an `edit` key and administration an `admin` key. Give every client its own key with the least access it needs, and never put an `edit` or `admin` key in a page other people load: a key that reaches other people's browsers is public. Use HTTPS (see the [full stack guide](./docker-full-stack.md)) and set `TRUSTED_PROXIES` if a reverse proxy sits in front. Details: [auth.md](./auth.md).

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

Works with any OpenAI-compatible API.

**OpenAI:**

```bash
STT_PROVIDER=whisper
WHISPER_URL=https://api.openai.com/v1/audio/transcriptions
WHISPER_API_KEY=sk-your_api_key_here
WHISPER_MODEL=gpt-4o-transcribe
WHISPER_LANGUAGE=en
TRANSCRIBE_WORKERS=2
# Optional — local terms, callsigns, place names. Supported by gpt-4o(-mini)-transcribe
# and gpt-transcribe; not by gpt-4o-transcribe-diarize.
# WHISPER_PROMPT=Police dispatch. Engine 7, Medic 23. 10-4, copy, en route.
```

tr-engine picks the request format from the model name (dated snapshots and an `openai/` prefix are recognized):

| `WHISPER_MODEL` | What you get |
|-----------------|--------------|
| `whisper-1` | Word timestamps; each word attributed to the radio unit that said it |
| `gpt-4o-transcribe`, `gpt-4o-mini-transcribe` | Text only — no timestamps, so no per-unit attribution |
| `gpt-transcribe` | Text only; `WHISPER_HOTWORDS` are sent as OpenAI `keywords` |
| `gpt-4o-transcribe-diarize` | Speaker-labelled segments (`A`, `B`, …); word timings are approximated within each segment and still attributed to units. `WHISPER_PROMPT` is not supported by this model |

For the `gpt-*` models, the Whisper-server-only options (`WHISPER_BEAM_SIZE`, the anti-hallucination settings, `WHISPER_VAD_FILTER`, and `WHISPER_HOTWORDS` except on `gpt-transcribe`) are not sent; tr-engine logs a warning once for each one that is set.

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
# STREAM_SOURCE_MAP=172.18.0.1=tr-butco   # sender IP → TR instance_id (see below)
```

`STREAM_SOURCE_MAP` (comma-separated `ip=instance_id`; IPv6 addresses work too) is only needed when several trunk-recorder instances use the same short name for **different** systems. tr-engine can't tell their packets apart by short name, so it drops audio from a sender it can't attribute, with a WARN "dropping live audio: several trunk-recorder instances use this short name" that names the sender's `source_ip` as tr-engine sees it (behind Docker's port mapping that can be a Docker gateway address). Map each sender to its instance ID, or give the systems unique short names. A sender's instance is worked out again for every chunk: real trunk-recorder instances are preferred over the `WATCH_INSTANCE_ID` (file watch / `TR_DIR`) identity, which is preferred over the `UPLOAD_INSTANCE_ID` identity. A sender is dropped with a WARN whenever the preferred instances map its short name to different systems, including when such an instance appears after the sender was attributed (a second trunk-recorder, say), and `STREAM_SOURCE_MAP` is then required. Only `STREAM_SOURCE_MAP` entries take an instance out of other senders' candidates; an attribution tr-engine worked out by itself doesn't.

**Docker port mapping:** Add the UDP port to the `tr-engine` service in `docker-compose.yml`:

```yaml
  tr-engine:
    ports:
      - "${HTTP_BIND_IP:-127.0.0.1}:${HTTP_PORT:-8080}:8080"
      - "${STREAM_BIND_IP:-127.0.0.1}:${STREAM_PORT:-9123}:9123/udp"
```

The simplestream listener is unauthenticated: anything that can reach the port can inject audio. It listens on `127.0.0.1` by default, which works when trunk-recorder runs on the Docker host (use `"address": "127.0.0.1"` in the plugin config). If trunk-recorder is on another machine, set `STREAM_BIND_IP` in `.env` to the Docker host's LAN or VPN address. Set `STREAM_PORT` to change the host port (the container-internal port stays 9123).

Restart with `docker compose up -d`. Verify via the health endpoint with a key that has unrestricted `listen`, `edit` or `admin` (`curl -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/health`) — a new `audio_stream` section appears when streaming is enabled. Without such a key, `/health` shows only `status`, `version` and `checks`.

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

The database persists — your data is safe. If a release includes schema migrations, they'll be noted in the release notes. If you're updating `docker-compose.yml` itself from an older copy, read the next section first.

**Upgrading from a version with `AUTH_TOKEN`, `WRITE_TOKEN` or `ADMIN_PASSWORD`** (before v0.10.0): back up the database and follow [migrating-auth.md](./migrating-auth.md). The first start converts what it can of the old settings into API keys and an anonymous access policy; tr-engine, tr-dashboard and any bind-mounted `web/` must be upgraded together.

### Security defaults changed

Older compose files shipped with a default database password (`trengine`), anonymous MQTT, and ports that could end up listening on every interface. The current files change that:

- **No default database password.** Compose refuses to start until `POSTGRES_PASSWORD` is set in `.env`.
- **PostgreSQL and postgres-exporter are never published** on a public address (no postgres port; exporter on `127.0.0.1` only).
- **MQTT requires a login.** `mosquitto/mosquitto.conf` sets `allow_anonymous false` and reads `mosquitto/passwd`, and compose refuses to start without `MQTT_PASSWORD`.
- **Published ports bind to `127.0.0.1` by default** (`HTTP_BIND_IP`, `MQTT_BIND_IP`, `BIND_IP`). If trunk-recorders on other hosts publish to this broker, set `MQTT_BIND_IP` explicitly or they will stop connecting. In `docker-compose.full.yml`, `BIND_IP` is required.

**Existing databases keep their old password.** PostgreSQL only reads `POSTGRES_PASSWORD` when it initializes an empty data directory. If you never set it, your database password is still `trengine`, and putting a new value in `.env` alone will just lock tr-engine out. Rotate the password inside the database first, then store the new value:

```bash
# 1. With the old stack still running (before replacing docker-compose.yml):
NEW_PG_PASSWORD=$(openssl rand -hex 24)
docker compose exec postgres psql -U trengine -d trengine \
  -c "ALTER ROLE trengine PASSWORD '$NEW_PG_PASSWORD'"
echo "POSTGRES_PASSWORD=$NEW_PG_PASSWORD" >> .env
```

Use your `POSTGRES_USER`/`POSTGRES_DB` in place of `trengine` if you changed them. The command connects over the container's local socket, so it doesn't need the old password.

If you've already replaced `docker-compose.yml` and the old containers are still running, compose now refuses to parse the file without the new variables. Prefix the `docker compose exec` line with `POSTGRES_PASSWORD=placeholder MQTT_PASSWORD=placeholder`; `exec` only runs a command in the existing container and changes nothing else. If the stack is already stopped, bring it back up with your old compose file, rotate, then switch.

Check where your data lives before switching files. Older all-in-one compose files kept the database in the `tr-engine-db` named volume; the current file uses `./pgdata`. If yours used the named volume, keep that mapping in the new file (`- tr-engine-db:/var/lib/postgresql/data` under `postgres`, plus `tr-engine-db:` under the top-level `volumes:`). Otherwise PostgreSQL initializes a new, empty database in `./pgdata`.

```bash
# 2. Create the MQTT login (step 2 of the quick start: MQTT_USERNAME/MQTT_PASSWORD
#    in .env, mosquitto/passwd, and mosquitto/mosquitto.conf from this repo),
#    add "username"/"password" to trunk-recorder's MQTT plugin config, then:
docker compose pull
docker compose up -d        # or: docker compose up -d postgres mosquitto tr-engine (no Caddy)
docker compose logs tr-engine --tail 30   # expect "mqtt connected, subscribing", no database errors
```

Only fall back to keeping the old password (`POSTGRES_PASSWORD=trengine` in `.env`) if you can't rotate right away, and only while PostgreSQL has no published port; rotate it as soon as you can with the `ALTER ROLE` command above.

If you installed with `install.sh`, your generated `tr-engine/docker-compose.yml` is not updated automatically: it still defaults to the `trengine` database password (PostgreSQL is not published there) and publishes port 8080 on all interfaces. Rotate the password as above, then edit that file: replace `${POSTGRES_PASSWORD:-trengine}` with `${POSTGRES_PASSWORD:?Set POSTGRES_PASSWORD in .env}` (two places) and change the port line to `"${HTTP_BIND_IP:-127.0.0.1}:${HTTP_PORT:-8080}:8080"`.

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
