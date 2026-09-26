# Getting Started — Binary Release

Download a pre-built binary and get running in minutes. You need PostgreSQL and at least one ingest source (MQTT broker, TR's audio directory, or TR's install directory).

> **Other installation methods:**
> - **[Build from source](./getting-started.md)** — compile everything yourself from scratch
> - **[Docker Compose](./docker.md)** — single `docker compose up` with everything pre-configured
> - **[Docker with existing MQTT](./docker-external-mqtt.md)** — Docker Compose connecting to a broker you already run
> - **[Full stack (HTTPS + Dashboard)](./docker-full-stack.md)** — production deployment with Caddy, Mosquitto, tr-dashboard, and Prometheus metrics

## Prerequisites

- **PostgreSQL 17+**
- A running **trunk-recorder** instance with one of:
  - The [MQTT Status plugin](https://github.com/TrunkRecorder/trunk-recorder-mqtt-status) and an MQTT broker
  - An accessible audio output directory (for file watch mode)
  - An accessible install directory (for TR auto-discovery)

If you don't have these yet, see the [build from source guide](./getting-started.md) for setup instructions on each component.

## 1. Download

Grab the latest release for your platform from the [releases page](https://github.com/trunk-reporter/tr-engine/releases):

| Platform | File |
|----------|------|
| Linux (amd64) | `tr-engine-linux-amd64.tar.gz` |
| Linux (arm64) | `tr-engine-linux-arm64.tar.gz` |
| Windows (amd64) | `tr-engine-windows-amd64.zip` |
| macOS (amd64) | `tr-engine-darwin-amd64.tar.gz` |
| macOS (arm64) | `tr-engine-darwin-arm64.tar.gz` |

```bash
# Example: Linux amd64
curl -LO https://github.com/trunk-reporter/tr-engine/releases/latest/download/tr-engine-linux-amd64.tar.gz
tar xzf tr-engine-linux-amd64.tar.gz
chmod +x tr-engine
```

The archive contains:
- `tr-engine` (or `tr-engine.exe` on Windows) — the server binary
- `schema.sql` — database schema
- `sample.env` — configuration template

## 2. Set up the database

Create a database and user, then load the schema:

```bash
sudo -u postgres psql -c "CREATE USER trengine WITH PASSWORD 'your_password_here';"
sudo -u postgres psql -c "CREATE DATABASE trengine OWNER trengine;"
psql -U trengine -d trengine -f schema.sql
```

This creates all tables, indexes, partitions, and triggers. Takes a few seconds.

## 3. Configure

```bash
cp sample.env .env
```

Edit `.env` — you need `DATABASE_URL` plus at least one ingest source:

```env
DATABASE_URL=postgres://trengine:your_password_here@localhost:5432/trengine?sslmode=disable

# Option A: MQTT ingest (richest data — call_start, units, recorders, rates)
MQTT_BROKER_URL=tcp://localhost:1883
MQTT_TOPICS=trengine/#

# Option B: Auto-discover from trunk-recorder's directory (simplest setup)
# TR_DIR=/path/to/trunk-recorder

# Option C: Watch TR's audio directory for new files
# WATCH_DIR=/path/to/trunk-recorder/audio
```

All three modes can run simultaneously. See `sample.env` for all available options (HTTP port, log level, audio directory, backfill settings, raw archival, rate limits). Access control is not configured here: tr-engine uses API keys (next step).

## 4. Run

```bash
./tr-engine
```

You should see:

```
{"level":"info","version":"...","message":"tr-engine starting"}
{"level":"info","component":"database","message":"database connected"}
{"level":"info","component":"mqtt","message":"mqtt connected, subscribing"}
{"level":"info","listen":":8080","message":"tr-engine ready"}
```

On the first start with an empty database, tr-engine also prints an **admin API key**, once, in a box on stderr ("no admin API key existed, so one was created"). Copy the `tre_...` line into a password manager; it is never shown again. Create named keys for your clients with it and revoke it afterwards:

```bash
./tr-engine keys create --name "admin (me)" --scopes admin
./tr-engine keys create --name "my browser" --scopes edit
./tr-engine keys list
./tr-engine keys revoke --prefix tre_xxxxxxxx   # the bootstrap key's prefix
```

Anonymous access is off on a new install. To let anyone who can reach tr-engine listen without a key: `./tr-engine access set --anonymous listen`. See [auth.md](auth.md).

## 5. Verify

```bash
# Health check (public, no key needed)
curl http://localhost:8080/api/v1/health

KEY=tre_...   # your admin key, or a key from `./tr-engine keys create`
curl -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/whoami

# List discovered systems (populated once TR sends data)
curl -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/systems

# Watch live events
curl -N -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/events/stream
```

Open `http://localhost:8080/irc-radio-live.html` in a browser for the live web UI, and paste a key when it asks.

## Running as a service

### systemd (Linux)

Create `/etc/systemd/system/tr-engine.service`:

```ini
[Unit]
Description=tr-engine
After=network.target postgresql.service mosquitto.service

[Service]
Type=simple
User=trengine
WorkingDirectory=/opt/tr-engine
ExecStart=/opt/tr-engine/tr-engine
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now tr-engine
sudo journalctl -u tr-engine -f
```

### Windows

Use [NSSM](https://nssm.cc/) or run as a scheduled task:

```powershell
nssm install tr-engine C:\tr-engine\tr-engine.exe
nssm set tr-engine AppDirectory C:\tr-engine
nssm start tr-engine
```

## Upgrading

1. Stop tr-engine
2. Replace the binary with the new version
3. Check the release notes for any schema migrations
4. If a migration is needed: `psql -U trengine -d trengine -f migrations/xxx.sql`
5. Start tr-engine

The schema is designed to be additive — new versions add tables/columns but don't break existing data. `schema.sql` is always safe to re-run on a fresh database.
