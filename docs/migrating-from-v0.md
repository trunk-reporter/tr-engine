# Migrating from v0

tr-engine v1 is a ground-up rewrite. The database schema is completely different and there is no automated migration path — you need a fresh database.

The good news: v0 used database `tr_engine` and v1 uses `trengine`, so they don't collide. You can run both side-by-side during the transition, or just stop v0 and start v1 on the same machine.

## What changes

| | v0 | v1 |
|---|---|---|
| Database | `tr_engine` (TimescaleDB required) | `trengine` (plain PostgreSQL 17+, no extensions) |
| Config | `config.yaml` (YAML) | `.env` file + env vars + CLI flags |
| MQTT topics | `trunk-recorder/status/#` etc. | Configurable, default `#` (catches everything) |
| Embedded Postgres | Attempted (broken on ARM) | Not supported — use system or Docker Postgres |
| Schema migrations | `golang-migrate` with dirty state tracking | Single `schema.sql` file, run once |
| ARM64 | Not in releases (had to compile from source) | Pre-built `linux-arm64` binary in every release |

## Steps

### 1. Stop v0

```bash
# If running as a systemd service
sudo systemctl stop tr-engine
sudo systemctl disable tr-engine
```

### 2. Create the v1 database

v0's `tr_engine` database stays untouched. Create a new one:

```bash
sudo -u postgres createuser trengine
sudo -u postgres createdb -O trengine trengine
```

Set a password if needed:

```bash
sudo -u postgres psql -c "ALTER USER trengine PASSWORD 'yourpassword';"
```

No TimescaleDB required. No extensions at all — plain PostgreSQL 17+.

Load the schema:

```bash
psql -U trengine -d trengine -f schema.sql
```

### 3. Install v1

Download the binary for your platform from the [releases page](https://github.com/trunk-reporter/tr-engine/releases), or build from source:

```bash
git clone https://github.com/trunk-reporter/tr-engine.git
cd tr-engine
bash build.sh
```

See [binary releases](./binary-releases.md) or [build from source](./getting-started.md) for full instructions.

### 4. Configure

```bash
cp sample.env .env
```

Edit `.env`:

```bash
DATABASE_URL=postgres://trengine:yourpassword@localhost:5432/trengine?sslmode=disable
MQTT_BROKER_URL=tcp://localhost:1883

# New in v1: MQTT is optional if you use file watch or TR auto-discovery instead
# TR_DIR=/path/to/trunk-recorder     # auto-discover from TR's config.json
# WATCH_DIR=/path/to/trunk-recorder/audio  # watch for new files
```

That's it. No YAML, no TimescaleDB tuning, no migration tracker.

### 5. Start v1

```bash
./tr-engine
```

Or set up a systemd service (see [binary releases — run as a service](./binary-releases.md#5-run-as-a-service)).

Data will start populating as soon as trunk-recorder publishes to the MQTT broker. Systems, sites, talkgroups, and units are all auto-discovered from the feed — no manual configuration needed.

### 6. Clean up v0 (optional)

Once you're satisfied v1 is working, you can remove the old database and binaries:

```bash
# Drop the v0 database
sudo -u postgres dropdb tr_engine
sudo -u postgres dropuser tr_engine

# Remove TimescaleDB if nothing else uses it
sudo apt remove timescaledb-2-postgresql-17

# Remove v0 binary and config
rm -rf /path/to/old/tr-engine
```

## What about my old data?

v0 call recordings and history cannot be imported into v1. The table structures, identity model, and partitioning strategy are all different. If you need to keep old data for reference, leave the `tr_engine` database in place — it won't interfere with v1.

Audio files from v0 are also not compatible — v1 uses a different directory structure and naming convention. Keep your old audio directory around if you want access to historical recordings.
