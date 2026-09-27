# Migrating to API Keys

tr-engine v0.10.0 replaces its old authentication with **API keys**, an **anonymous access policy** and short-lived **tickets**. This is a breaking change: the old configuration (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS`), the open/token/full modes, user accounts, logins and the `?token=` URL parameter are gone.

On the first start of the new version, tr-engine reads your old variables **once** and converts what it safely can, so upload plugins and admin scripts keep working. After that, the old variables do nothing except log a warning.

This guide explains the upgrade step by step. For the new model itself, read [auth.md](auth.md).

## What changed

| Before | After |
|---|---|
| Three modes (open / token / full), derived from which variables were set | One model: every client has an API key; requests without a key follow the **anonymous access policy** (`off` or `listen`, optionally restricted) |
| Open mode: everything public, including writes | Anonymous access can at most **listen**. Edits need an `edit` key, administration an `admin` key |
| `AUTH_TOKEN` in token mode: one shared secret | Imported once as the key `legacy AUTH_TOKEN` (`listen`, plus `upload` if there was no `WRITE_TOKEN`) |
| `AUTH_TOKEN` in full mode: public read token handed out by `/auth-init` | **Not** imported (it was public). Replaced by anonymous access `listen` |
| `WRITE_TOKEN` | Imported once as the key `legacy WRITE_TOKEN` (`admin`, `upload`), unless it was published as the public read token |
| `ADMIN_PASSWORD`, `ADMIN_USERNAME`, `JWT_SECRET`, login, refresh cookies | Gone. There are no people in tr-engine, only clients with keys. A bootstrap admin key is printed on first start when no admin key exists |
| User accounts (`/users`) with roles viewer / editor / admin | Removed. The accounts are listed in the log once and recorded in the database, then dropped |
| API keys (`tre_...`) owned by users, or service accounts | Kept and still valid. Role becomes scope (viewer → `listen`, editor → `edit`, admin → `admin`); see [below](#existing-api-keys-and-user-accounts) |
| `CORS_ORIGINS` | Gone. The API answers every origin, and never uses cookies |
| `?token=` in URLs (live events, audio, WebSocket) | Gone. Clients mint short-lived `?ticket=`s with their key |
| `GET /auth-init`, `/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/setup`, `/auth/me`, `/auth/keys...`, `/users...` | 404. Use `GET /whoami`, `/keys`, `/anonymous-access`, `/tickets` |
| A reverse proxy injecting the read token into anonymous requests | Must be removed |
| `/debug-report` open to anyone, `/metrics` open to anyone | `/debug-report` needs `admin`; `/metrics` needs a key with `listen` |
| Editors could run SQL, merge systems, save pages, import CSVs | Those need `admin` now |

## Before you start

1. **Back up the database.** There is no downgrade path other than restoring this backup.

   ```bash
   # Docker Compose (bundled PostgreSQL)
   docker compose exec -T postgres pg_dump -U trengine trengine > pre-apikey.sql

   # Bare metal / external PostgreSQL
   pg_dump "$DATABASE_URL" > pre-apikey.sql
   ```

   Use your own `POSTGRES_USER`/`POSTGRES_DB` if you changed them from `trengine`.

2. **Note your current `.env`** auth lines (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS`). Don't change them yet: the new version needs to read them once.

3. **Know your clients**: which dashboards, scripts, bots and trunk-recorder upload plugins talk to tr-engine, and which credential each one uses.

## Upgrade step by step

### 1. Back up the database

See [above](#before-you-start).

### 2. Upgrade tr-engine, tr-dashboard and `web/` together

The new engine, tr-dashboard and the `web/` demo pages only work with each other. Upgrade them in one go, and pin the versions (not `latest`) so they don't drift apart later.

```bash
# docker-compose.yml
#   tr-engine:    image: ghcr.io/trunk-reporter/tr-engine:v0.10.0
#   tr-dashboard: image: ghcr.io/trunk-reporter/tr-dashboard:<matching version>
docker compose pull tr-engine tr-dashboard
```

If you bind-mount a `web/` directory into the container (`./web:/opt/tr-engine/web`), copy the new `web/` files over it. **Keep pages you saved yourself** (with `POST /api/v1/pages` or by hand); copy only the files that ship with tr-engine:

```bash
rsync -av --exclude='my-*.html' tr-engine-src/web/ ./web/   # adjust the exclude to your own pages
```

Pages built from old templates keep working: `auth.js` still answers the old function names, with a console warning.

Bare metal: stop the service, replace the binary, keep `.env`.

### 3. Start once with the old `.env` unchanged

```bash
docker compose up -d tr-engine
docker compose logs tr-engine --tail 100
```

On this first start tr-engine runs its database migrations and the **one-time legacy import** ([what it does](#what-the-one-time-import-does)). It logs one summary line, for example:

```
legacy auth import: old mode "token"; imported AUTH_TOKEN as key #3 'legacy AUTH_TOKEN' (listen, upload); anonymous access stays off
```

It also logs, once, the user accounts it removed:

```
removed 3 user accounts: alice (admin), bob (editor), carol (viewer, disabled) — give each person or their client an API key
```

### 4. Copy the bootstrap key, if one was printed

If no admin key exists after the import (for example you had token mode without `WRITE_TOKEN`, or open mode), tr-engine creates a key named `bootstrap admin` and prints it **once** to stderr, in a box:

```bash
docker compose logs tr-engine | grep -A3 "no admin API key"
```

Store it in a password manager. You need an admin key for the next steps. (No bootstrap key is printed if an admin key already exists, such as an imported `WRITE_TOKEN` or an existing admin-owned `tre_` key.)

### 5. Remove reverse-proxy token injection

If your reverse proxy adds an `Authorization` header to requests that don't have one, **remove that block now**. In the new model that turns the injected token into a public key, or breaks every anonymous request.

The old Caddy pattern (this repository's `caddy/Caddyfile`, the Deployed Instance notes, and the old tr-dashboard README) was:

```caddy
handle /api/* {
	@no_auth not header Authorization *
	request_header @no_auth Authorization "Bearer {$AUTH_TOKEN}"
	reverse_proxy tr-engine:8080
}
```

Replace it with a plain proxy:

```caddy
handle /api/* {
	reverse_proxy tr-engine:8080
}
```

The old nginx pattern (`set $auth "Bearer ..."; ... proxy_set_header Authorization $auth;`) goes too; just `proxy_pass`. Then reload the proxy (`docker compose restart caddy` for a bind-mounted Caddyfile).

tr-engine is tolerant during the switch: it ignores an injected *old full-mode public `AUTH_TOKEN`* and an empty `Bearer` (treats them as no credential, and logs "a request carried the pre-upgrade public AUTH_TOKEN — a reverse proxy is probably still injecting it" at most once an hour). Any **other** injected value (for example a token-mode `AUTH_TOKEN`, which was imported as a key) is used as a key, so every anonymous visitor gets that key's access; remove it.

Once no WARN has appeared for a while, forget the old public token:

```bash
docker compose exec -T tr-engine tr-engine access forget-retired-token
```

`CORS_ORIGINS` is no longer needed either: tr-engine answers every origin, without cookies.

### 6. Give every client its own key

List what exists now:

```bash
docker compose exec -T tr-engine tr-engine keys list
```

Then create one named key per client, with the least access it needs ([which scopes](auth.md#creating-a-key-for-each-client)):

```bash
docker compose exec -T tr-engine tr-engine keys create --name "admin (alice)" --scopes admin
docker compose exec -T tr-engine tr-engine keys create --name "tr-dashboard at home" --scopes edit
docker compose exec -T tr-engine tr-engine keys create --name "trunk-recorder butco uploads" --scopes upload
docker compose exec -T tr-engine tr-engine keys create --name "prometheus" --scopes listen
```

Each `keys create` prints only the new key. Put each key into its client:

- **tr-dashboard**: open it and paste the key into the "Connect to tr-engine" screen (or Settings → API key). A write token saved by an older dashboard is tried once as a key and kept only if the engine accepts it (an imported legacy token will be).
- **`web/` pages**: use the "API key…" menu item, or the prompt that appears when a page needs a key. `admin.html` asks for an admin key separately and keeps it for the browser tab only.
- **Upload plugins**: put the key in `apiKey`. See [Upload plugins](#upload-plugins).
- **Scripts**: send `Authorization: Bearer <key>`. Replace any `?token=` in URLs.
- **Prometheus**: see [auth.md](auth.md#prometheus).
- **People who had user accounts**: an admin creates a key for each person's client (or, better, each client they run). Keys they created themselves under their account still work ([see below](#existing-api-keys-and-user-accounts)).

When every client has its own key, revoke the legacy keys and the bootstrap key:

```bash
docker compose exec -T tr-engine tr-engine keys list          # find their IDs or prefixes
docker compose exec -T tr-engine tr-engine keys revoke 3
docker compose exec -T tr-engine tr-engine keys revoke --prefix legacy_9f2c1a
```

`last_used_at` in `keys list` shows whether a legacy key is still in use before you revoke it.

### 7. Set the anonymous access policy deliberately

The import chose a policy for you (`listen` for an upgraded open instance with data and for a full-mode public demo; `off` otherwise). Check it and decide:

```bash
docker compose exec -T tr-engine tr-engine access show

# Public listening, everything:
docker compose exec -T tr-engine tr-engine access set --anonymous listen --no-restriction
# Public listening, except sensitive talkgroups:
docker compose exec -T tr-engine tr-engine access set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001,1:5002
# Private instance:
docker compose exec -T tr-engine tr-engine access set --anonymous off
```

Remember: in the old open mode, anyone could also *edit*. Anonymous access can now only listen; editing needs a key.

### 8. Remove the old variables

Delete these lines from `.env` (and from any `environment:` block in your compose file):

```
AUTH_ENABLED=
AUTH_TOKEN=
WRITE_TOKEN=
ADMIN_USERNAME=
ADMIN_PASSWORD=
JWT_SECRET=
CORS_ORIGINS=
```

Also remove `TR_AUTH_TOKEN` (and the dead `TR_ENGINE_URL`) from the `tr-dashboard` service. Then recreate the container so it picks up the change (`docker compose restart` does not re-read `.env`):

```bash
docker compose up -d tr-engine
```

Until you do, every start logs one WARN per leftover variable, for example "AUTH_TOKEN is no longer used (imported as API key #3 'legacy AUTH_TOKEN' on 2026-09-26) — remove it from your configuration" or "ADMIN_PASSWORD is no longer used — tr-engine has no user accounts; clients use API keys".

### 9. Register a secret the import missed (optional)

If a client still uses an old secret that the import skipped (for example a `WRITE_TOKEN` you had already removed from `.env`, or a token that lived only in a script), and you can't change that client yet, register the secret as a legacy key. It is read from stdin, so it doesn't end up in your shell history:

```bash
docker compose exec -T tr-engine tr-engine keys import --name "legacy: nightly export script" --scopes listen < old-token.txt
```

Only the hash is stored. The same strength rules as the automatic import apply. Plan to replace it with a real key.

## What the one-time import does

The import runs on the first start of the new version, **once per database** (it is recorded in the `data_fixups` table as `import-legacy-auth`, with every decision in its `detail` column). It first works out your old mode exactly as the old version did:

- `AUTH_ENABLED=false` cleared every credential: old mode **open**.
- `JWT_SECRET` without `ADMIN_PASSWORD` is ignored (it never enabled logins).
- Then: **open** if none of `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_PASSWORD` is set; **write-token-only** if only `WRITE_TOKEN` is set; **token** if `AUTH_TOKEN` is set without `ADMIN_PASSWORD`; **full** if `ADMIN_PASSWORD` is set.

| Old mode | Keys created | Anonymous access after upgrade | Bootstrap key printed? |
|---|---|---|---|
| **open** | none | `listen` if the database already has systems (so an upgraded public instance stays readable), otherwise `off` | Yes, unless an admin API key already exists |
| **write-token-only** | `legacy WRITE_TOKEN` (`admin`, `upload`) | `off`. An INFO line suggests `tr-engine access set --anonymous listen` if you want public reads | No |
| **token** | `legacy AUTH_TOKEN` (`listen`, plus `upload` if `WRITE_TOKEN` was not set). If `WRITE_TOKEN` was also set: `legacy WRITE_TOKEN` (`admin`, `upload`) | `off` (everyone used to need the token) | Yes if there was no `WRITE_TOKEN` and no admin API key |
| **full** | `legacy WRITE_TOKEN` (`admin`, `upload`) if set. `AUTH_TOKEN` is **not** imported: `/auth-init` gave it to every visitor | `listen` if `AUTH_TOKEN` was set (the public-demo setup), otherwise `off` | Only if no admin-owned API key and no `WRITE_TOKEN` exist |

Special cases:

- **Fresh database** (the schema was created on this start): nothing is imported, and anonymous access stays `off`. You still get the bootstrap key and the per-variable warnings.
- **`WRITE_TOKEN` equal to `AUTH_TOKEN` in full mode**: the token was published by `/auth-init` as the public read token, so it is **not** imported. tr-engine logs an ERROR ("WRITE_TOKEN was published by /auth-init as the public read token; not imported — create a new admin key") and prints a bootstrap key.
- **`AUTH_TOKEN` equal to `WRITE_TOKEN` in token mode**: imported once, as `legacy WRITE_TOKEN`.
- **Full-mode `AUTH_TOKEN`**: its SHA-256 is kept as the *retired public token*, so a proxy that still injects it is tolerated ([step 5](#5-remove-reverse-proxy-token-injection)).
- **Strength rules**: a value containing `$(` (a shell command copied literally from old docs, such as `AUTH_TOKEN=$(openssl rand -base64 32)`, which Compose never ran) is **refused**, with an ERROR naming the variable. A value shorter than 16 characters is imported, but every start logs "legacy key #N is weak (N characters) — replace it". A value already imported is not imported twice.
- **Legacy keys** are marked `legacy`, show a prefix of `legacy_` plus 6 hex characters of their hash (no character of the old secret is stored), and are always rate-limited per client IP (they may have been effectively public).
- **Uploads**: if calls arrived through HTTP upload (`UPLOAD_INSTANCE_ID`) in the last 7 days and, after the import, no active key has `upload`, tr-engine logs "HTTP uploads were in use but no key can upload now — create one with `tr-engine keys create --name 'uploads' --scopes upload` and configure it in trunk-recorder", and lists migrated keys without `upload` that were used in the last 7 days.

To see what happened later:

```bash
docker compose logs tr-engine | grep -i -E "legacy|bootstrap|removed .* user accounts"
docker compose exec -T tr-engine tr-engine keys list --all
docker compose exec -T postgres psql -U trengine trengine -c \
  "SELECT name, applied_at, detail FROM data_fixups WHERE name IN ('import-legacy-auth', 'removed-user-accounts', 'bootstrap-admin-key')"
```

### Existing API keys and user accounts

Keys created by the old version (`tre_...`, from the admin page or `/auth/keys`) keep working with the same secret:

- A key's scope is its old role, but never more than its owner's **current** role (viewer → `listen`, editor → `edit`, admin → `admin`). A key created by an editor who was later demoted to viewer becomes a `listen` key.
- Keys of **disabled** users are revoked.
- The owner's username is appended to the key's name: "home dashboard (alice)", or "... (alice, disabled)".
- **Service-account** keys (the documented upload credential) also get `upload`. Other keys lose the ability to upload, which they only had through a bug; the upload warning above tells you if that matters.
- Keys with an empty label are named `key <prefix>`.

User accounts themselves are removed: the migration records every account (username, role, enabled, last login) in `data_fixups` (`removed-user-accounts`), logs the one-line summary, and drops the `users` table. Nobody logs in anymore; give each person's client a key.

## Upload plugins

Upload plugins keep working when the import gave their credential `upload`:

| What the plugin sent | After the upgrade |
|---|---|
| `WRITE_TOKEN` (any mode) | Works: imported with `admin`, `upload`. Replace it with an `upload`-only key soon: an upload plugin should not hold an admin key |
| `AUTH_TOKEN` in token mode without `WRITE_TOKEN` | Works: imported with `listen`, `upload` |
| `AUTH_TOKEN` in token mode with `WRITE_TOKEN` also set | **Stops working**: the old version preferred `WRITE_TOKEN` for uploads, so `AUTH_TOKEN` is imported as `listen` only |
| A service-account `tre_` key | Works: service keys get `upload` |
| A user's own `tre_` key | **Stops working**: only service keys get `upload` |
| The full-mode public `AUTH_TOKEN` | **Stops working**: it was public and is not imported |
| Nothing (old open mode) | **Stops working**: anonymous uploads are never allowed |

For each trunk-recorder host, create an upload-only key and put it in the plugin config:

```bash
docker compose exec -T tr-engine tr-engine keys create --name "trunk-recorder butco uploads" --scopes upload
```

```json
{ "shortName": "butco", "apiKey": "tre_...", "systemId": 1 }
```

Restart trunk-recorder. Rejected uploads are logged by tr-engine with the client IP, the system name and the reason (no key, unknown key, or "key #N lacks upload"). See [http-upload.md](http-upload.md).

## Example: a public demo in full mode (gerty-style)

A public instance with a guest view and an admin login had:

```env
AUTH_TOKEN=<public read token>
ADMIN_PASSWORD=<admin password>
CORS_ORIGINS=https://tr-dashboard.example.com,https://tr-engine.example.com
```

and a Caddy block on the dashboard domain that injected `AUTH_TOKEN` into `/api/*` requests.

After the upgrade:

- Anonymous access is `listen`, unrestricted: guests see what they saw before, without any token. Consider excluding sensitive talkgroups (`access set --anonymous listen --all-talkgroups --exclude-talkgroups ...`).
- `AUTH_TOKEN` is retired, not imported. Browsers and proxies that still send it are treated as anonymous.
- The admin user account is gone. If admins had created their own `tre_` keys, those keep working; otherwise the bootstrap key is printed. Create one named admin key per person who administers the instance.
- tr-dashboard shows guests the read-only view; admins paste an admin key (or an edit key for day-to-day use) under Settings → API key.

Do this:

```bash
cd /docker/tr-engine
docker compose exec -T postgres pg_dump -U trengine trengine > pre-apikey.sql
# pin both images in docker-compose.yml, copy the new web/ files (keep your own pages)
docker compose pull tr-engine tr-dashboard && docker compose up -d tr-engine tr-dashboard
docker compose logs tr-engine | grep -A3 "no admin API key"          # bootstrap key, if printed
# remove the @no_auth / request_header block from the Caddyfile, then:
docker compose restart caddy
docker compose exec -T tr-engine tr-engine keys create --name "admin (you)" --scopes admin
docker compose exec -T tr-engine tr-engine access show
# delete AUTH_TOKEN, ADMIN_PASSWORD, CORS_ORIGINS (and TR_AUTH_TOKEN) from .env / compose, then:
docker compose up -d tr-engine tr-dashboard
docker compose exec -T tr-engine tr-engine keys revoke --prefix <bootstrap key prefix>
docker compose exec -T tr-engine tr-engine access forget-retired-token   # once the injection WARN is gone
```

## Example: a private token-mode instance

```env
AUTH_TOKEN=a-long-shared-secret
```

After the upgrade the shared secret is the key `legacy AUTH_TOKEN` (`listen`, `upload`), anonymous access is `off`, and a bootstrap admin key is printed. Everything that used the shared secret for reading or uploading keeps working. Changes (tag edits, merges), which the shared secret never allowed, need an `edit` or `admin` key: create one with the bootstrap key. Then create named keys, move each client over, and revoke `legacy AUTH_TOKEN`.

## Example: an open instance on a home LAN

No auth variables were set. After the upgrade anonymous access is `listen` (if you had data), a bootstrap admin key is printed, and editing needs a key. Paste an `edit` key into your own dashboard; guests on your LAN keep read access. If you would rather keep the instance fully private, `access set --anonymous off`.

## Rolling back

Rollback is only possible by **restoring the backup** from step 1: the migration rewrites the `api_keys` table and drops `users`.

```bash
docker compose stop tr-engine
# switch the images back to the old versions in docker-compose.yml
docker compose exec -T postgres psql -U trengine -c "DROP DATABASE trengine" postgres
docker compose exec -T postgres psql -U trengine -c "CREATE DATABASE trengine" postgres
docker compose exec -T postgres psql -U trengine trengine < pre-apikey.sql
docker compose up -d tr-engine tr-dashboard
```

Restore the old `.env` lines and the proxy injection block too, if you had removed them. Calls ingested between the upgrade and the rollback are lost.
