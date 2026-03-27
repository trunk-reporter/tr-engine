# Getting Started — Full Stack (HTTPS + Dashboard)

Production deployment with Caddy (automatic HTTPS), Mosquitto (MQTT broker), tr-dashboard (standalone frontend), and Prometheus metrics — all managed by a single Docker Compose file.

> **Other installation methods:**
> - **[Docker Compose (all-in-one)](./docker.md)** — quick setup with bundled PostgreSQL and MQTT
> - **[Docker with existing MQTT](./docker-external-mqtt.md)** — connect to a broker you already run
> - **[Build from source](./getting-started.md)** — compile everything yourself
> - **[Binary releases](./binary-releases.md)** — download a pre-built binary

## Architecture

```
                    Internet
                       │
            ┌──────────┴──────────┐
            │      Caddy :443     │  ← auto HTTPS (Let's Encrypt / Cloudflare)
            │  reverse proxy      │
            └──┬──────────────┬───┘
               │              │
    api.example.com    dashboard.example.com
               │              │
        ┌──────┴───┐   ┌─────┴──────┐
        │ tr-engine │   │tr-dashboard│
        │  :8080    │   │   :3000    │
        └──┬────┬───┘   └────────────┘
           │    │
    ┌──────┴┐  ┌┴─────────┐
    │Postgres│  │Mosquitto │ ← trunk-recorder publishes here
    │ :5432  │  │  :1883   │
    └────────┘  └──────────┘

    postgres-exporter :9187  ← Prometheus scrapes this
```

| Service | Purpose | Exposed Port |
|---------|---------|-------------|
| **caddy** | Reverse proxy, auto HTTPS | `BIND_IP:80`, `BIND_IP:443` |
| **tr-engine** | API + web UI | internal only (behind Caddy) |
| **tr-dashboard** | Standalone dashboard frontend | internal only (behind Caddy) |
| **postgres** | Database | internal only |
| **mosquitto** | MQTT broker | `BIND_IP:1883` |
| **postgres-exporter** | Prometheus metrics for PostgreSQL | `BIND_IP:9187` |

## Prerequisites

- **Docker** and **Docker Compose** (v2+)
- A **dedicated IP address** (or host) for binding ports — set as `BIND_IP`
- **Two DNS A records** pointing to that IP (for Caddy to issue HTTPS certificates)
- **trunk-recorder** with the [MQTT Status plugin](https://github.com/TrunkRecorder/trunk-recorder-mqtt-status)

## 1. Download

```bash
mkdir tr-engine && cd tr-engine
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/docker-compose.full.yml
mv docker-compose.full.yml docker-compose.yml
curl -sO https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/sample.env
cp sample.env .env
```

## 2. Configure `.env`

Edit `.env` and set these values:

```bash
# REQUIRED — IP address to bind all external ports to
BIND_IP=203.0.113.10

# REQUIRED — your domain names (replace with your actual domains)
API_DOMAIN=api.example.com
DASHBOARD_DOMAIN=dashboard.example.com

# REQUIRED — MQTT credentials (trunk-recorder must use these too)
MQTT_USERNAME=trengine
MQTT_PASSWORD=your-mqtt-password-here

# STRONGLY RECOMMENDED — API tokens
# AUTH_TOKEN MUST be set explicitly (not auto-generated) because Caddy
# needs the value for its conditional auth header injection.
AUTH_TOKEN=$(openssl rand -base64 32)
WRITE_TOKEN=$(openssl rand -base64 32)

# Optional — match your TR plugin's topic prefix
MQTT_TOPICS=trengine/#
```

Generate secure random tokens:

```bash
# Run these and paste the output into .env
openssl rand -base64 32   # → AUTH_TOKEN
openssl rand -base64 32   # → WRITE_TOKEN
openssl rand -base64 16   # → MQTT_PASSWORD
```

> **Important:** `AUTH_TOKEN` must be set explicitly in `.env` — do not rely on auto-generation. Caddy reads this value via `{$AUTH_TOKEN}` environment variable substitution to inject it into proxied requests. If it's empty, the conditional auth injection won't work and the web UI will prompt for a token on every page load.

Key variables:

| Variable | Required | Description |
|----------|----------|-------------|
| `BIND_IP` | Yes | IP address for all external port bindings |
| `API_DOMAIN` | Yes | Domain for the tr-engine API + built-in web UI |
| `DASHBOARD_DOMAIN` | Yes | Domain for the tr-dashboard frontend |
| `MQTT_USERNAME` | Yes | MQTT broker credentials (shared with trunk-recorder) |
| `MQTT_PASSWORD` | Yes | MQTT broker password |
| `AUTH_TOKEN` | Strongly recommended | Read-only API token (Caddy injects this for browser requests) |
| `WRITE_TOKEN` | Strongly recommended | Write token for mutating operations (POST/PATCH/PUT/DELETE) |

See [`sample.env`](https://github.com/trunk-reporter/tr-engine/blob/master/sample.env) for all available options. Settings like `TR_DIR`, transcription, file watch, and S3 storage are documented in the [Docker Compose guide](./docker.md).

## 3. Set up Mosquitto

Create the config directory and files:

```bash
mkdir -p mosquitto
```

Create `mosquitto/mosquitto.conf`:

```
listener 1883
password_file /mosquitto/config/passwd
allow_anonymous false
```

Generate the password file (use the same password you put in `.env`):

```bash
docker run --rm -v ./mosquitto:/mosquitto/config eclipse-mosquitto:2 \
  mosquitto_passwd -b -c /mosquitto/config/passwd trengine YOUR_MQTT_PASSWORD
```

Replace `YOUR_MQTT_PASSWORD` with the value you set for `MQTT_PASSWORD` in `.env`.

## 4. Set up Caddy

Create the config directory:

```bash
mkdir -p caddy
```

Create `caddy/Caddyfile` (replace the domain names with yours):

```caddyfile
api.example.com {
    # Inject read token for browser requests that don't send their own Authorization header.
    # This makes the web UI work seamlessly — browsers get the read token automatically.
    # When a client sends its own Authorization header (e.g. with WRITE_TOKEN), Caddy
    # passes it through untouched so write operations work.
    @no_auth not header Authorization *
    request_header @no_auth Authorization "Bearer {$AUTH_TOKEN}"

    reverse_proxy tr-engine:8080
}

dashboard.example.com {
    reverse_proxy tr-dashboard:3000
}
```

**Why `@no_auth` matters:** Without this conditional matcher, Caddy would unconditionally overwrite the `Authorization` header on every request — replacing the browser's write token with the read token and breaking all write operations. The `@no_auth` matcher ensures:

- **No token from browser** → Caddy injects the read token → reads work seamlessly
- **Write token from browser** → Caddy passes it through untouched → writes work

## 5. DNS records

Create two A records pointing to your `BIND_IP`:

| Record | Type | Value | Proxy |
|--------|------|-------|-------|
| `api.example.com` | A | `203.0.113.10` | Cloudflare proxied OK |
| `dashboard.example.com` | A | `203.0.113.10` | Cloudflare proxied OK |

**Cloudflare users:** Set SSL/TLS mode to **Full** (not Full Strict, unless you've configured Caddy with Cloudflare origin certificates). Caddy handles the HTTPS certificate on the origin side.

**Non-Cloudflare users:** Caddy automatically obtains Let's Encrypt certificates. Just make sure ports 80 and 443 are reachable from the internet.

If your MQTT broker needs a DNS name (e.g. for trunk-recorder to connect by hostname):

| Record | Type | Value | Proxy |
|--------|------|-------|-------|
| `mqtt.example.com` | A | `203.0.113.10` | DNS only (gray cloud) |

MQTT is raw TCP — it cannot be proxied through Cloudflare.

## 6. Start

```bash
docker compose up -d
```

On first run:
- PostgreSQL initializes and tr-engine auto-applies the database schema
- Caddy obtains HTTPS certificates (may take 30-60 seconds)
- Mosquitto starts with authentication enabled
- tr-engine connects to PostgreSQL and Mosquitto

## 7. Verify

```bash
# Check all services are running
docker compose ps

# Check tr-engine logs
docker compose logs tr-engine --tail 20

# Health check (use the API domain or localhost via docker)
curl -s https://api.example.com/api/v1/health | python3 -m json.tool

# Test MQTT connection from trunk-recorder's perspective
mosquitto_pub -h YOUR_BIND_IP -p 1883 -u trengine -P 'YOUR_MQTT_PASSWORD' -t test -m hello
```

Access your deployment:
- **API + Web UI:** `https://api.example.com`
- **Dashboard:** `https://dashboard.example.com`

## Point trunk-recorder at the broker

In your trunk-recorder `config.json`:

```json
{
  "plugins": [
    {
      "name": "MQTT Status",
      "library": "libmqtt_status_plugin.so",
      "broker": "tcp://YOUR_BIND_IP:1883",
      "topic": "trengine/feeds",
      "unit_topic": "trengine/units",
      "console_logs": true,
      "username": "trengine",
      "password": "YOUR_MQTT_PASSWORD"
    }
  ]
}
```

Replace `YOUR_BIND_IP` and `YOUR_MQTT_PASSWORD` with your values. Systems and talkgroups auto-populate once trunk-recorder connects.

## Configuration

This guide covers the full-stack-specific setup. For shared configuration topics, see the [Docker Compose guide](./docker.md):

- [TR auto-discovery (TR_DIR)](./docker.md#tr-auto-discovery-tr_dir) — auto-import talkgroup names and watch for new calls
- [File watch mode (WATCH_DIR)](./docker.md#file-watch-mode-watch_dir) — ingest calls from TR's audio directory
- [Filesystem audio (TR_AUDIO_DIR)](./docker.md#filesystem-audio-tr_audio_dir) — serve audio directly from TR's filesystem
- [Transcription (STT)](./docker.md#transcription-stt) — automatic speech-to-text for call recordings
- [Custom web UI files](./docker.md#custom-web-ui-files) — override the built-in web UI

To add volume mounts for these features, edit the `tr-engine` service in your `docker-compose.yml` — the commented-out examples are already there.

## Upgrading

```bash
docker compose pull && docker compose up -d
```

All persistent data lives in bind-mounted directories (`pgdata/`, `audio/`) and named volumes (`mosquitto-data`, `caddy-data`). Check release notes for schema migrations.

## Logs

```bash
# All services
docker compose logs -f

# Individual services
docker compose logs -f tr-engine
docker compose logs -f caddy
docker compose logs -f mosquitto
```

## Stopping

```bash
# Stop (data preserved)
docker compose down

# Stop and delete all data (fresh start)
docker compose down -v
```

## Troubleshooting

**Caddy certificate errors:** Check that your DNS records are pointing to `BIND_IP` and that ports 80/443 are reachable. Run `docker compose logs caddy` to see certificate issuance status. If using Cloudflare, ensure SSL mode is set to Full.

**MQTT authentication failures:** Verify that the username/password in your TR `config.json` matches what's in the Mosquitto password file. The password file is generated separately from `.env` — regenerate it if you change `MQTT_PASSWORD`:

```bash
docker run --rm -v ./mosquitto:/mosquitto/config eclipse-mosquitto:2 \
  mosquitto_passwd -b -c /mosquitto/config/passwd trengine NEW_PASSWORD
docker compose restart mosquitto
```

**Write operations failing (403):** If API writes return "write operations require WRITE_TOKEN":
- This is expected when `WRITE_TOKEN` is not set — the API runs in read-only mode when auth is enabled
- To enable writes, set `WRITE_TOKEN` in `.env` and restart: `docker compose up -d`
- If `WRITE_TOKEN` is set but writes still fail, check the `@no_auth` matcher in your Caddyfile — without it, Caddy overwrites the `Authorization` header with the read token

**Dashboard can't reach the API:** The tr-dashboard container connects to tr-engine via the Docker network (`http://tr-engine:8080`). Check that `TR_AUTH_TOKEN` in the compose file matches your `AUTH_TOKEN`.

**Web UI prompts for token:** `AUTH_TOKEN` must be explicitly set in `.env` (not auto-generated) so Caddy can inject it. If you see token prompts, check that `AUTH_TOKEN` is set and that the Caddyfile has the `@no_auth` injection block.
