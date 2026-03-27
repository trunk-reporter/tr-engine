# Getting Started — Docker with Existing MQTT Broker

Run tr-engine via Docker Compose, connecting to an MQTT broker you already have running (e.g., the one your trunk-recorder is already publishing to). No repo clone required.

> **Other installation methods:**
> - **[Docker Compose (all-in-one)](./docker.md)** — includes its own MQTT broker
> - **[Full stack (HTTPS + Dashboard)](./docker-full-stack.md)** — production deployment with Caddy, Mosquitto, tr-dashboard, and Prometheus metrics
> - **[Build from source](./getting-started.md)** — compile everything yourself
> - **[Binary releases](./binary-releases.md)** — download a pre-built binary

## Prerequisites

- Docker and Docker Compose
- A running **MQTT broker** that trunk-recorder is already publishing to (or use file watch mode instead — see [Configuration](#other-settings))
- The broker's address, port, and credentials (if any)

## 1. Create a project directory

```bash
mkdir tr-engine && cd tr-engine
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/docker-compose.yml
```

## 2. Configure your MQTT broker

Create a `.env` file with your broker details:

```bash
MQTT_BROKER_URL=tcp://192.168.1.50:1883
MQTT_USERNAME=
MQTT_PASSWORD=
MQTT_TOPICS=trengine/#
```

| Variable | What to set |
|----------|-------------|
| `MQTT_BROKER_URL` | Your broker's address — e.g. `tcp://192.168.1.50:1883` |
| `MQTT_USERNAME` | Broker credentials (leave empty for anonymous) |
| `MQTT_PASSWORD` | Broker credentials (leave empty for anonymous) |
| `MQTT_TOPICS` | Must match your TR plugin's topic prefix with `/#`. If your TR plugin uses `topic: "myradio/feeds"`, set this to `myradio/#` |

See [`sample.env`](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) for all available options.

## 3. Edit `docker-compose.yml`

Remove the `mosquitto` service and the `depends_on` reference to it — you don't need a bundled broker.

Here's what the file should look like after editing:

```yaml
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: ${POSTGRES_USER:-trengine}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-trengine}
      POSTGRES_DB: ${POSTGRES_DB:-trengine}
    volumes:
      - ./pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-trengine}"]
      interval: 5s
      timeout: 3s
      retries: 5

  tr-engine:
    image: ghcr.io/trunk-reporter/tr-engine:latest
    ports:
      - "${HTTP_PORT:-8080}:8080"
    env_file:
      - path: ./.env
        required: false
    environment:
      # Docker-internal: DATABASE_URL is built from POSTGRES_* vars above
      DATABASE_URL: postgres://${POSTGRES_USER:-trengine}:${POSTGRES_PASSWORD:-trengine}@postgres:5432/${POSTGRES_DB:-trengine}?sslmode=disable
      AUDIO_DIR: /data/audio
      # MQTT_BROKER_URL comes from .env — no override needed here
    volumes:
      - ./audio:/data/audio
      # Uncomment to live-edit web UI without rebuilding:
      # - ./web:/opt/tr-engine/web
    depends_on:
      postgres:
        condition: service_healthy
```

> **Note:** `DATABASE_URL` is built automatically from the `POSTGRES_*` variables — set credentials in one place via `.env`. `MQTT_BROKER_URL` is intentionally absent from the `environment` block so it picks up your value from `.env`.

### Broker on the Docker host

If the MQTT broker runs on the same machine as Docker, set `MQTT_BROKER_URL=tcp://host.docker.internal:1883` in your `.env` and add `extra_hosts` to the tr-engine service in `docker-compose.yml`:

```yaml
  tr-engine:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

On macOS and Windows, `host.docker.internal` works without `extra_hosts`. On Linux, the `extra_hosts` line is required.

## 4. Start

```bash
docker compose up -d
```

On first run, PostgreSQL starts with an empty database. tr-engine auto-applies the schema on first connect (takes a few seconds).

## 5. Verify

```bash
# Check logs — look for "mqtt connected" and "subscribing"
docker compose logs tr-engine --tail 30

# Health check — database and mqtt should both show "connected"
curl http://localhost:8080/api/v1/health

# Watch live events (Ctrl-C to stop)
curl -N http://localhost:8080/api/v1/events/stream
```

Open http://localhost:8080 for the web UI. Systems and talkgroups auto-populate as trunk-recorder sends data — no manual configuration needed.

## Data

All data is stored in bind-mounted directories next to your `docker-compose.yml`:

| Directory | Contents |
|-----------|----------|
| `./pgdata` | PostgreSQL data |
| `./audio` | Call audio files |

To back up the database:

```bash
docker compose exec postgres pg_dump -U trengine trengine > backup.sql
```

## Other settings

Everything else is optional and has sensible defaults. Add any variable from [sample.env](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) to your `.env` file:

```bash
AUTH_TOKEN=my-secret                # API authentication token
# WRITE_TOKEN=my-write-secret       # separate token for write operations (recommended for public instances)
# CORS_ORIGINS=https://example.com  # restrict CORS (empty = allow all)
LOG_LEVEL=debug                     # more verbose logging
RAW_STORE=false                     # disable raw MQTT archival (saves disk)
RAW_EXCLUDE_TOPICS=trunking_message # exclude high-volume raw archival
# TR_DIR=/tr-config                 # auto-discover from TR's config.json
# WATCH_DIR=/tr-audio               # file watch mode (alternative to MQTT)
# WATCH_BACKFILL_DAYS=7             # days to backfill on startup
```

Then restart: `docker compose up -d`

### File watch mode and TR auto-discovery

MQTT is optional — you can ingest calls by watching TR's audio directory instead (or in addition to MQTT). Add to `.env` and bind-mount TR's directory in `docker-compose.yml`:

In `.env`:
```bash
TR_DIR=/tr-config              # auto-discover everything from TR's config
# or just: WATCH_DIR=/tr-audio  # watch audio directory only
```

In `docker-compose.yml`:
```yaml
    volumes:
      - /path/to/trunk-recorder:/tr-config:ro
```

`TR_DIR` reads TR's `config.json`, auto-sets `WATCH_DIR` and `TR_AUDIO_DIR`, and imports talkgroup and unit tag CSVs. See [sample.env](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) for all available options.

### Filesystem audio (TR_AUDIO_DIR)

If trunk-recorder runs on the same machine (or its audio directory is accessible via a network mount), you can serve audio files directly from TR's filesystem instead of receiving them over MQTT as base64. This avoids encoding overhead and duplicate files.

Add to `.env`:
```bash
TR_AUDIO_DIR=/tr-audio
```

And bind-mount TR's audio directory in `docker-compose.yml`:
```yaml
    volumes:
      - ./audio:/data/audio
      - /path/to/trunk-recorder/audio:/tr-audio:ro
```

When `TR_AUDIO_DIR` is set, tr-engine skips saving audio files from MQTT and resolves them using the `call_filename` path reported at call_end. In your TR plugin config, keep `mqtt_audio: true` but set `mqtt_audio_type: none` — this sends the call metadata (frequencies, transmissions, unit list) without the base64 audio payload, saving encoding CPU and MQTT bandwidth.

## Custom web UI

Mount a local `web/` directory to override the embedded UI files without rebuilding:

```yaml
    volumes:
      - ./audio:/data/audio
      - ./web:/opt/tr-engine/web
```

Changes take effect on the next browser request — no restart needed. See [Updating Web Files](./docker.md#custom-web-ui-files) for how to pull the latest UI files from GitHub.

## Upgrading

```bash
docker compose pull && docker compose up -d
```

Database and audio files persist in the bind-mounted directories. Check the release notes for any schema migrations.

## Troubleshooting

**MQTT not connecting:** Check that the broker address is reachable from inside the container. Run `docker compose logs tr-engine` and look for connection errors. If the broker is on `localhost`, use `host.docker.internal` instead (see above).

**No data appearing:** Verify trunk-recorder is publishing with `mosquitto_sub -h your-broker -t '#' -v`. Check that `MQTT_TOPICS` matches the TR plugin's topic prefix.
