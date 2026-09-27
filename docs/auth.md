# Authentication and Access Control

tr-engine is an API that any client can be written against. It authenticates **client software, not people**: every client (a tr-dashboard deployment, a script, a trunk-recorder upload plugin, a public website's backend, a phone app) holds its own **API key**. There are no user accounts, passwords, logins, sessions or cookies in tr-engine. How a client authenticates *its* users, or whether it has users at all, is up to the client.

This guide has two parts:

- **[Part 1: Operator guide](#part-1-operator-guide)**: running tr-engine, creating keys, public access, reverse proxies, troubleshooting.
- **[Part 2: Client-developer guide](#part-2-client-developer-guide)**: writing a client that uses keys, tickets, streams and audio.

Upgrading from a version with `AUTH_TOKEN`, `WRITE_TOKEN` or `ADMIN_PASSWORD`? Read [migrating-auth.md](migrating-auth.md) first.

---

# Part 1: Operator guide

## Concepts

### API keys

A key looks like `tre_` followed by 64 hex characters. Clients send it in a header:

```
Authorization: Bearer tre_4f1c…
```

tr-engine stores only a SHA-256 hash of each key, so the full key is shown **once**, when it is created. Afterwards you only see its name and its display prefix (`tre_` plus 8 characters). Keys are never accepted in a URL.

Every key has a name that says what it is for ("tr-dashboard at home", "trunk-recorder butco uploads", "prometheus"), and can have an expiry date and a per-key rate limit. Revoking a key takes effect on its next request.

### Scopes

A key's scopes say what it may do:

| Scope | Grants | Implies |
|---|---|---|
| `listen` | Read radio data (every data `GET`), stream events and audio, mint tickets | — |
| `edit` | Change radio metadata: talkgroup and unit tags, transcription corrections and review states, re-transcription, approving or dismissing unit-tag suggestions | `listen` |
| `admin` | Everything else: keys, the anonymous access policy, system and site identity edits, system merges, maintenance, transcription backfill, storage, the SQL query endpoint, saving pages, CSV imports, the debug report, console logs, the audit log | `edit`, `listen` |
| `upload` | `POST /api/v1/call-upload` only | — |

A key holds **at most one** of `listen`, `edit`, `admin`, optionally plus `upload`: `listen`, `edit`, `admin`, `admin,upload` and `upload` are all valid. `listen,edit` is rejected because `edit` already includes `listen`.

### Anonymous access

A request **without** a key is governed by the *anonymous access policy*, which you set:

- `off` (the default on a fresh install): anonymous requests get `401 key_required` everywhere except a few public endpoints (`/api/v1/health`, `/api/v1/whoami`, `/api/v1/openapi.yaml`, `/api/v1/pages` and the static web pages themselves).
- `listen`: anyone can read radio data, stream live events and play audio, optionally only for some systems or talkgroups (a *restriction*).

Anonymous visitors can never edit, administer or upload, and never see trunk-recorder console logs. There is no "public token": making something public is a policy, not a credential you hand out.

### Restrictions

A restriction limits which radio data a listen credential can see. It can be put on a key whose scopes are exactly `listen`, on the anonymous policy, and on tickets. See [Restricting what a credential sees](#restricting-what-a-credential-sees) below.

### Tickets

Browsers cannot attach an `Authorization` header to `EventSource` (live events), `<audio src>` (call audio) or `WebSocket` (live audio). For those three, a client that has a key asks tr-engine for a **ticket**: a short-lived (1 minute to 1 hour), listen-only, signed credential that goes in the URL as `?ticket=`. Tickets work on those three endpoints only. This way no long-lived secret ever appears in a URL, a proxy log or browser history. Operators don't manage tickets; see [Tickets for operators](#tickets-for-operators) for what you need to know.

### Who holds the key

| Client shape | Where the key lives | Who authenticates people | Streams and audio |
|---|---|---|---|
| **Single-user app**: tr-dashboard, the `web/` demo pages, a desktop or phone app, a page you run for yourself | In the app (browser storage for web apps) | Nobody: whoever holds the key is the user | Tickets |
| **Multi-user frontend with a server**: a club site with its own logins, a Discord bot | On the client's server, never in the browser | The client, however it likes | The server calls the API with the header, and mints tickets (optionally narrowed per user) for its users' browsers |
| **Public listening site** | No key | Nobody | Anonymous access policy |

### The one rule: a key that reaches other people's browsers is public

If a key ends up in a web page that other people load (embedded in JavaScript, in a config file served to browsers, or injected by a reverse proxy into anonymous requests), every visitor has that key's access. There is no way for tr-engine to tell them apart.

- To make listening public, use the **anonymous access policy**, not a shared key.
- A multi-user frontend that offers more than public access (editing, or listening to talkgroups that are not public) **needs a server side** that holds the key.
- Never give an `edit` or `admin` key to a page other people use.

## First run: the bootstrap admin key

On the first start with a new database, tr-engine creates one admin key named `bootstrap admin` and prints it **once**, directly to stderr as plain text:

```
================================================================
 tr-engine: no admin API key existed, so one was created:

   tre_4f1c…

 Store it now — it will not be shown again. Paste it into a client
 (tr-dashboard, web/admin.html) or send it as
 "Authorization: Bearer <key>" to create more keys.

 Create or revoke keys later with:
   tr-engine keys --help
   (in Docker: docker compose exec -T tr-engine tr-engine keys --help)
================================================================
```

Where to find it:

| How you run tr-engine | Command |
|---|---|
| Docker Compose | `docker compose logs tr-engine \| grep -A3 "no admin API key"` |
| systemd | `journalctl -u tr-engine \| grep -A3 "no admin API key"` |
| In a terminal | It is in the terminal output |

Copy it into a password manager. Then:

1. Use it to create named keys for your clients ([below](#creating-a-key-for-each-client)), including a named admin key for yourself. Give that admin key no expiry: the API (tr-dashboard's Access page, `web/admin.html`) only lets you revoke the bootstrap key while another admin key lasts at least as long ([last-admin guard](#through-the-api)).
2. Revoke the bootstrap key: `tr-engine keys revoke --prefix <prefix>`, with its whole prefix exactly as `tr-engine keys list` shows it (for example `tre_4f1c2a9b`), or by its ID. tr-dashboard's Access page and `web/admin.html` show a warning while a key named `bootstrap admin` is still active.

This happens **once per database**. If an admin key already exists (for example one imported during an upgrade), no bootstrap key is created. A later start never creates another one: if every admin key has been revoked, has expired or no longer has `admin`, tr-engine logs an ERROR ("no active admin API key exists") with the recovery command, and you create a new admin key from the command line on the host:

```bash
tr-engine keys create --name "admin" --scopes admin
# Docker:
docker compose exec -T tr-engine tr-engine keys create --name "admin" --scopes admin
```

Recovery always needs host access (the command connects to the database directly). The API cannot lock you out of the command line.

## Managing keys

### From the command line

The `keys` and `access` subcommands connect to the database using the same configuration as the server (`.env` in the current directory, environment variables, or `--env-file` / `--database-url`). They work while the server is running.

```
tr-engine keys list   [--all]
tr-engine keys create --name NAME --scopes listen|edit|admin[,upload] | upload
                      [--expires 90d|720h|2026-12-31|2026-12-31T00:00:00Z]
                      [--all-talkgroups] [--systems 1,2] [--talkgroups 1:9178,1:9179]
                      [--exclude-talkgroups 1:5001] [--rate-limit RPS]
tr-engine keys update ID|--prefix PREFIX [--name NAME] [--scopes ...] [--expires ...|--no-expiry]
                      [--rate-limit RPS|--no-rate-limit] [restriction flags|--no-restriction]
tr-engine keys revoke ID|--prefix PREFIX
tr-engine keys import --name NAME --scopes ...     (reads an existing secret from stdin)
tr-engine access show
tr-engine access set  --anonymous off|listen [restriction flags|--no-restriction]
tr-engine access forget-retired-token
```

- A bare number is always a key ID; to pick a key by its prefix, use `--prefix` with the **whole** prefix exactly as `keys list` shows it (`tre_` plus 8 hex characters, such as `tre_4f1c2a9b`, or `legacy_` plus 6 hex characters, such as `legacy_9f2c1a`). A shorter prefix matches nothing ("no key has the prefix ..."), and a prefix shared by several keys fails and lists their IDs; use the ID then.
- `keys create` prints **only the new key** on stdout (all messages go to stderr, including the restriction as stored), so a script can capture it. `keys import` prints only the new key's ID. `keys list` and `access show` print their table on stdout; `update`, `revoke`, `set` and `forget-retired-token` report on stderr.
- `keys list` hides revoked keys; `--all` shows them.
- `--expires` takes a Go duration (`720h`), a number of days (`90d`), a date (`2026-12-31`, midnight UTC) or an RFC 3339 time.
- `--systems`, `--talkgroups` and `--exclude-talkgroups` take comma-separated lists and can also be repeated; repeats add up (`--systems 1 --systems 2` is `--systems 1,2`).
- Restriction flags on `keys update` and `access set` replace the whole restriction. `--exclude-talkgroups` only removes talkgroups from an allow list: combine it with `--all-talkgroups` ("everything except"), `--systems` or `--talkgroups`. A restriction that names a system involved in an earlier merge is rewritten on store, as the merge would have rewritten it ([merges](#system-merges-and-restrictions)).
- `keys import` refuses a secret that is already stored (it names the key, and says whether it is active, revoked or expired), and refuses the pre-upgrade public `AUTH_TOKEN`, both while it is retired and after `access forget-retired-token`: the old engine handed that value to every visitor. Create a new key for a client that still uses it.
- `access forget-retired-token` refuses while an active key holds the retired token's value (a key imported before imports refused it), and names the key to revoke first.
- Every command first brings the database schema up to date, like the server, and names any pending migrations on stderr. On a database from a version before API keys it refuses to apply the irreversible conversion (`convert api_keys to app keys`, `record and drop users`) unless you add `--migrate`: an older engine still running on that database would stop working, and the change can't be undone. The recommended order is to start the new server first ([migrating-auth.md](migrating-auth.md)), which also carries `AUTH_TOKEN`/`WRITE_TOKEN` over.
- `tr-engine export` and `tr-engine import` take `--migrate` too. Without it, on a database from before API keys, they apply only the other migrations, run, and leave the irreversible ones to the server's first start, with a note on stderr (they don't touch keys or settings). `import --dry-run` never applies them, and `import --dry-run --migrate` is refused. To back up before upgrading, use `pg_dump` with the old engine stopped, not `tr-engine export`, which carries only the radio data (no keys, user accounts or settings).
- `keys list` and `access show` print a note on stderr while the one-time legacy import is still to come (an upgraded database the new server hasn't started on yet): the first server start may still import old tokens as keys and set the anonymous policy.
- Exit status: 0 on success, 1 on an error, 2 on a usage mistake (a command without arguments prints its usage and exits 2; `--help` exits 0).
- In Docker, prefix every command with `docker compose exec -T tr-engine`. The `-T` matters when you capture the output:

  ```bash
  KEY=$(docker compose exec -T tr-engine tr-engine keys create --name "tr-dashboard" --scopes edit)
  ```

Changes made with the CLI reach the running server within 30 seconds (its key cache), and open live-event streams and audio WebSockets within 60 seconds.

### Through the API

Any client with an admin key can manage keys (`GET/POST /api/v1/keys`, `GET/PATCH/DELETE /api/v1/keys/{id}`). tr-dashboard's **Access** page and the `web/admin.html` demo page are built on these.

```bash
ADMIN_KEY=tre_...   # an admin key
curl -s -X POST http://localhost:8080/api/v1/keys \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "trunk-recorder butco uploads", "scopes": ["upload"]}'
```

The response includes `"key": "tre_..."`, the only time the full key is shown. Changes through the API take effect at once, including on open streams.

**Last-admin guard.** The API refuses (409) to revoke an active admin key, take `admin` away from it, give it an earlier expiry (or a first one), or set or lower its rate limit below 1 request/second, unless another active admin key lasts at least as long: one with no expiry, or with an expiry no earlier than this key's. Only admin keys that aren't rate-limited below 1 request/second count as that other key, since a key allowed one request an hour can't undo anything. So the only admin key can't be given an expiry, or such a rate limit, through the API; create a second admin key first. If a key did end up with an over-tight limit, `tr-engine keys update ID --no-rate-limit` removes it. The CLI is not subject to this guard (it is the recovery path), but `keys revoke` and `keys update` warn when no admin key is left, or when every remaining one expires within a day.

**Rotating a key.** Keys can't be changed in place. Create a new key, switch the client to it, then revoke the old one. `last_used_at` (in `keys list` and the API) tells you when the old one stopped being used.

## Creating a key for each client

Give every client its own named key, with the least access it needs. Then a leak or a retired client costs you one revocation, and the audit log tells you which client did what.

| Client | Scopes | Notes |
|---|---|---|
| tr-dashboard you use yourself | `edit` | `admin` only if you manage keys or run maintenance from it. Paste the key into the "Connect to tr-engine" screen or Settings → API key. See tr-dashboard's [Authentication](https://github.com/trunk-reporter/tr-dashboard#authentication) section. |
| `web/` demo pages | `listen` or `edit` | Browse with a listen or edit key. `admin.html` and `storage.html` ask for an admin key separately and keep it for the tab only (browser `sessionStorage`, `tr-engine-admin-key`). Saving a page in `playground.html`, sending a debug report (`debug-report.html`) and the CSV import on `talkgroup-directory.html` use the admin key entered on `admin.html` in the same tab; without one they send the stored key, which then needs `admin`. |
| A trunk-recorder upload plugin | `upload` | One key per recorder host. See [Upload plugins](#upload-plugins). |
| Prometheus | `listen` | See [Prometheus](#prometheus). |
| A script or bot | whatever it needs | A reporting script needs `listen`; a tag-sync script needs `edit`. |
| A public website's backend | `listen`, optionally restricted and rate-limited | The key stays on the server. |
| A shared screen in a dispatch room | `listen`, restricted | A browser that others use holds a key, so treat that key as known to everyone who can touch the screen. |

tr-dashboard and the `web/` pages refuse the old full-mode public `AUTH_TOKEN` when it is pasted as a key ("tr-engine ignored this value: it is the retired public AUTH_TOKEN, not an API key"), because the engine treats it as no credential. `web/auth.js` also silently forgets it if an older page had stored it.

Examples:

```bash
tr-engine keys create --name "tr-dashboard at home" --scopes edit
tr-engine keys create --name "trunk-recorder butco uploads" --scopes upload
tr-engine keys create --name "prometheus" --scopes listen
tr-engine keys create --name "club website" --scopes listen \
  --systems 1 --exclude-talkgroups 1:5001,1:5002 --rate-limit 20 --expires 365d
```

## Upload plugins

trunk-recorder's rdio-scanner and OpenMHz upload plugins put the key in a form field (`key` or `api_key`), and tr-engine reads it from there on `POST /api/v1/call-upload`. Create a key with only the `upload` scope and put it in the plugin's `apiKey`:

```json
{
  "name": "rdioscanner_uploader",
  "library": "librdioscanner_uploader.so",
  "server": "https://tr-engine.example.com/api/v1/call-upload",
  "systems": [
    { "shortName": "butco", "apiKey": "tre_...", "systemId": 1 }
  ]
}
```

Only keys with `upload` can upload: anonymous access never allows it, and `listen`/`edit`/`admin` keys without `upload` are refused. When an upload is rejected, tr-engine logs a WARN "call upload rejected" (at most once a minute per client IP) with the IP, the system name from the form, and the reason in a `reason` field (`no key`, `unknown key`, `key #N lacks upload`, `request body too large`, ...). Full details: [http-upload.md](http-upload.md).

A key in a form field is not a header credential, so these uploads are rate-limited **per client IP** like requests without a key (see [Rate limiting](#rate-limiting)). A host that uploads more than `RATE_LIMIT_RPS` calls a second (default 20, bursts of 40), for example while clearing a backlog, gets `429`; send the key as `Authorization: Bearer` instead if the uploader can, or raise `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`.

## Setting the anonymous access policy

Show the current policy:

```bash
tr-engine access show
```

Examples:

```bash
# Nobody without a key (the default)
tr-engine access set --anonymous off

# Anyone can listen to everything
tr-engine access set --anonymous listen --no-restriction

# Anyone can listen, except two sensitive talkgroups
tr-engine access set --anonymous listen --all-talkgroups --exclude-talkgroups 1:5001,1:5002

# Anyone can listen to system 1 and to two talkgroups of system 2
tr-engine access set --anonymous listen --systems 1 --talkgroups 2:9178,2:9179
```

`access set` without restriction flags keeps the stored restriction (so `access set --anonymous off` and back to `listen` restores it); `--no-restriction` clears it. The restriction is kept even while access is `off`, so you can prepare it first. A restriction that names a system already merged into another is rewritten on store ([merges](#system-merges-and-restrictions)); `access show` and the API response show the stored form.

The same through the API (admin key; both fields are required, and `"restriction": null` means unrestricted):

```bash
curl -s -X PUT http://localhost:8080/api/v1/anonymous-access \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"access": "listen", "restriction": {"allow_all": true, "exclude_talkgroups": ["1:5001", "1:5002"]}}'
```

An API change applies at once, including to open anonymous streams (which close if access is now `off`). A CLI change applies within 30 seconds, and to open streams within 60 seconds.

What anonymous listeners get when access is `listen`:

- Everything a `listen` key gets, except `POST /api/v1/tickets` and `/metrics`, which always need a key.
- If the policy is **restricted**: only data for allowed talkgroups, and a 403 `restricted_credential` from the endpoints that can't filter by talkgroup (see [What a restricted credential can use](#what-a-restricted-credential-can-use)).
- Never console logs, never edits, never uploads.

Everyone can see *whether* anonymous access is on and whether it is restricted (`GET /api/v1/whoami`), but only admins can see the lists (`GET /api/v1/anonymous-access`), because the exclude list names the talkgroups you consider sensitive.

## Restricting what a credential sees

A restriction has four fields:

| Field | Meaning |
|---|---|
| `allow_all` | Every talkgroup is allowed (minus `exclude_talkgroups`). Can't be combined with `systems` or `talkgroups`. |
| `systems` | System IDs whose talkgroups are allowed. |
| `talkgroups` | Individual talkgroups, as `system_id:tgid` (for example `1:9178`). |
| `exclude_talkgroups` | Talkgroups never allowed, even when their system is. |

```json
{ "systems": [1, 3], "talkgroups": ["2:9178", "2:9179"], "exclude_talkgroups": ["1:5001"] }
{ "allow_all": true, "exclude_talkgroups": ["1:5001", "1:5002"] }
```

Rules worth knowing:

- **No restriction** (`null`) means everything. **Any** restriction object limits access, and one with nothing allowed (`{}`, or empty `systems` and `talkgroups` without `allow_all`) allows nothing. tr-engine refuses such a restriction on a key ("restriction allows nothing") and on the anonymous policy ("use access: off instead"). Removing the last allowed entry never turns into "everything".
- Data without a talkgroup (unit registrations, recorder state, decode rates, control-channel messages) is **never** shown to a restricted credential.
- IDs don't have to exist yet, so you can prepare a key before its system appears.
- Each list (`systems`, `talkgroups`, `exclude_talkgroups`) holds at most 1000 entries when you set it. System merges can grow a stored exclusion list past that ([below](#system-merges-and-restrictions)); such a stored restriction stays valid, and sending it back unchanged (`PATCH /api/v1/keys/{id}` with the same restriction, `PUT /api/v1/anonymous-access`, or `access set` without restriction flags) is accepted.
- Only `listen` keys can be restricted. An `edit` or `admin` key is always unrestricted.
- A restricted credential never sees data outside its restriction, but some traces of other activity remain. Call IDs are one sequence across all talkgroups, so gaps between the `call_id`s a restricted credential sees show roughly how many calls happened outside its restriction, though never what they were. (Live-event IDs are opaque and reveal nothing.)

### System merges and restrictions

Systems are merged from the admin API (`POST /api/v1/admin/systems/merge`), or automatically when two recorders turn out to watch the same P25 network. In the same transaction every stored restriction (keys and the anonymous policy) is rewritten:

- Allow entries that name the old system (`systems`, and `old:tgid` in `talkgroups`) are rewritten to the surviving system.
- `exclude_talkgroups` are kept **symmetric**: an entry that names either the old or the surviving system is kept, and a copy naming the other one is added (after a chain of merges, every system of the chain), so data that still carries the old ID, or is written under it around the merge, stays excluded. Expect merged-away IDs to remain in stored exclusions, and exclusion lists to grow (possibly past the 1000-entry limit for new input).
- A restriction stored later that names a system involved in a merge (`keys create`/`update`, `access set`, or the same through the API) is rewritten the same way on store; the response or `keys list`/`access show` shows the stored form.
- A ticket narrowing that names a merged-away system anywhere, `exclude_talkgroups` included, is refused when it is minted (`400 invalid_body`). A ticket minted before the merge fails with `401 invalid_ticket`, and a stream it opened closes with `ticket_expired`; clients mint a new one.

A merge never leaves an allow entry pointing at a merged-away system and never voids an exclusion (exclusions keep the old ID next to the surviving one), but it **can widen access**: an allow entry that named either system then covers the whole merged system, including the data the other system already had. That is what you want when both are the same radio network. Before merging systems that are not, check restricted keys (`tr-engine keys list`) and the anonymous policy (`tr-engine access show`).

### What a restricted credential can use

Every endpoint's policy is in [openapi.yaml](../openapi.yaml) as `x-restricted`:

- `enforced`: systems, sites, talkgroups, the talkgroup directory, calls (list, active, details, audio, frequencies, transmissions, transcriptions), call groups, transcription search and batch, live events and live audio. These return only allowed data; asking for a specific call or talkgroup outside the restriction is a 404, as if it didn't exist.
- `deny` (403 `restricted_credential`): units and unit events, affiliations, unit-tag suggestions, P25 systems, encryption stats, talkgroup units, all stats and analytics, recorders, trunking messages, the transcription queue, audio jitter and `/metrics`. Their data either has no talkgroup, or would reveal other talkgroups' activity through totals. Restriction support for these is on the [roadmap](roadmap.md).

## Tickets for operators

Operators rarely need to think about tickets; clients mint them. What to know:

- Only a key with `listen` (a listen, edit or admin key) can mint one, with `POST /api/v1/tickets`. A ticket grants `listen` only, and only on `GET /api/v1/events/stream`, `GET /api/v1/audio/live` and `GET /api/v1/calls/{id}/audio`.
- A ticket lives 1 to 60 minutes (10 by default). It is checked against its key on every use, so revoking or changing the key affects its tickets at once. A live-event stream or audio WebSocket opened with a ticket is closed when the ticket expires, and the client reconnects with a new one.
- A ticket's narrowing can't name a system that was merged into another, not even in `exclude_talkgroups`: minting one is refused (`400 invalid_body`), and an older ticket that does stops working ([merges](#system-merges-and-restrictions)).
- A single ticket can't be revoked. To cut a client off, revoke its key.
- Tickets are signed with a random secret generated on first start and stored in the database (`auth_settings`, row `ticket_secret`). To invalidate every outstanding ticket, delete that row and restart tr-engine; a new secret is generated.
- Tickets appear in access logs (they are in the URL). That is why they are short-lived and listen-only.

## Prometheus

`/metrics` (on unless `METRICS_ENABLED=false`) is served at the engine's root, not under `/api/v1`, and always needs a key with `listen`, whatever the anonymous policy says. Restricted keys are refused.

```bash
tr-engine keys create --name "prometheus" --scopes listen > /etc/prometheus/tr-engine.key
chmod 600 /etc/prometheus/tr-engine.key
```

`prometheus.yml`:

```yaml
scrape_configs:
  - job_name: tr-engine
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/tr-engine.key
    static_configs:
      - targets: ["tr-engine:8080"]
```

## Reverse proxies

A reverse proxy in front of tr-engine should pass requests through **unchanged**. In particular:

- **Never inject an `Authorization` header** into requests. A proxy that adds a key to anonymous requests makes that key public (see [the one rule](#the-one-rule-a-key-that-reaches-other-peoples-browsers-is-public)). Use the anonymous access policy instead. Older setups did this (a Caddy `request_header @no_auth Authorization "Bearer {$AUTH_TOKEN}"` block); remove it. tr-engine treats an injected pre-upgrade public `AUTH_TOKEN` as no credential and logs a WARN at most once an hour; it also treats an empty `Bearer` as no credential, silently (so a Caddy `{$AUTH_TOKEN}` block whose variable is now unset never shows up in the log). Any other injected value is treated as a key: if it is an active key (for example a token-mode `AUTH_TOKEN` or a `WRITE_TOKEN` imported as a legacy key), every visitor silently gets that key's access; otherwise every anonymous request fails with `401 invalid_key`. Either way, remove the block.
- Pass the `Authorization` header through, and don't add CORS headers of your own: tr-engine answers every origin itself.
- Disable response buffering for `/api/v1/events/stream` (tr-engine sends `X-Accel-Buffering: no` for nginx) and allow WebSocket upgrades on `/api/v1/audio/live`.
- Set `TRUSTED_PROXIES` so rate limiting sees real client IPs. tr-engine only believes `X-Forwarded-For` / `X-Real-IP` from peers listed there (default `loopback,private`; comma-separated IPs and CIDRs, or `none`).

Caddy:

```caddy
tr-engine.example.com {
	reverse_proxy tr-engine:8080
}
```

nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header Upgrade $http_upgrade;       # live audio WebSocket
    proxy_set_header Connection $http_connection;
    proxy_buffering off;                          # live events (SSE)
    proxy_read_timeout 1h;
}
```

### HTTP basic auth in front of tr-engine

If your proxy asks for a username and password (`Authorization: Basic ...`), tr-engine treats that header as *no credential*, so it doesn't interfere with anonymous access. It also means a browser can't send a tr-engine key at the same time, since both use the `Authorization` header. Proxy-level passwords work for anonymous viewing behind a private gate, not for key-holding clients.

## Rate limiting

- Requests without a key, with a ticket, with a legacy (imported) key, or with a key tr-engine hasn't seen in the last 30 seconds are limited **per client IP**: `RATE_LIMIT_RPS` (default 20) per second with bursts of `RATE_LIMIT_BURST` (default 40). Guessing keys therefore costs one IP token per guess.
- Other keys sent in the `Authorization` header are not limited unless you set `--rate-limit RPS` on the key (burst twice that).
- An upload that carries its key in the `key`/`api_key` form field (the rdio-scanner and OpenMHz plugins) has no header credential, so it is limited per client IP like a request without a key. It costs two tokens when the key isn't cached, and always for a legacy key (an imported `WRITE_TOKEN` or `AUTH_TOKEN`), so those uploads get half the per-IP rate. For high-volume uploaders, send the key as `Authorization: Bearer` (a legacy key still costs one IP token per request there), replace legacy upload keys with new `upload` keys, or raise `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`.
- Over the limit, tr-engine answers `429 rate_limited` with a `Retry-After` header.

## The audit log

tr-engine records every change made with a key: every request with a key to a real endpoint whose method isn't GET, HEAD or OPTIONS and that the auth layer let through, including refusals decided by the endpoint itself. Each entry has the time, the key's ID and name, the actor, the method, the path, the status the client received and the request ID. The path is stored without its query string and cut at 1024 bytes, with a "…(truncated, N bytes)" marker; the server limits the request line plus headers to 64 KiB plus Go's 4 KiB read slack (a larger request gets `431`; keep them under 64 KiB). Not recorded: requests without a key, requests the auth layer refused (`401`, and `403 insufficient_scope` or `restricted_credential`, such as a listen key trying to edit), call uploads and ticket minting (high volume, no state change worth auditing). Those are only in the access log, which has a line for every request.

- **Read it** in tr-dashboard's Access page, in `web/admin.html`, or with `GET /api/v1/admin/audit-log?limit=50&offset=0&key_id=&since=&until=` (admin).
- **Actor.** A multi-user client can send an `X-Actor` header naming the end user it acted for ("alice@club"). It is recorded, and shown next to the key name in `unit_tag_suggestions.decided_by` and `system_merge_log.performed_by`, but it is informational only: tr-engine can't verify it.
- **Retention:** `RETENTION_AUDIT_LOG` (default `8760h`, one year), purged by the daily maintenance run. Without the environment variable, an admin can also change it at runtime with `PUT /api/v1/admin/maintenance/config` and key `retention_audit_log`.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `401 key_required` | No key, and anonymous access is `off` or doesn't cover this endpoint (edit/admin/upload endpoints, `/tickets`, `/metrics`). | Give the client a key, or set anonymous access to `listen` if the data should be public. |
| `401 invalid_key` | The key is unknown (typo, key from another database), revoked or expired; the message says which when known. Never falls back to anonymous. | `tr-engine keys list --all` shows revoked and expired keys. Create a new key. Check your reverse proxy doesn't inject a header. |
| `401 invalid_ticket` | The ticket is malformed (truncated URL), expired, its key was revoked or re-scoped, the ticket secret was rotated, or it names a merged-away system. | Clients mint a new ticket and retry. If it happens constantly, check the client's clock is not far off and the URL is not being cut. |
| `403 insufficient_scope` | The key is valid but lacks the scope (the message names it), e.g. an upload-only key used to browse, or a listen key used to edit. | Use a key with the right scope, or update the key's scopes. |
| `403 restricted_credential` | The key, ticket or anonymous policy is restricted, and this endpoint can't filter by talkgroup (units, stats, recorders, ...). | Expected. Use an unrestricted key for those pages. |
| `404` for a call or talkgroup you know exists | The credential is restricted and the resource is outside the restriction. | Check the restriction (`keys list`, or `access show`). |
| `429 rate_limited` | Per-IP or per-key limit hit. | Wait `Retry-After` seconds. Raise `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`, check `TRUSTED_PROXIES` (all clients behind one proxy IP share one limit), or give heavy clients their own keys. |
| `503 service_unavailable` on every request with a key | The key lookup couldn't reach the database (error or 2 s timeout). | Check the database. tr-engine never treats this as anonymous. |
| `403 forbidden` "route has no auth policy" | A tr-engine bug: an endpoint without a policy (fails closed). | Report it, with the path. |
| WARN "a request carried the pre-upgrade public AUTH_TOKEN" | A proxy still injects the old public token. | Remove the injection block. Once none is left, `tr-engine access forget-retired-token`. |
| WARN "HTTP uploads were in use but no key can upload now" | After an upgrade, no key has `upload`. | Create an upload key and put it in the trunk-recorder plugin config. |
| WARN "legacy key #N is weak" | An imported `AUTH_TOKEN`/`WRITE_TOKEN` is shorter than 16 characters. | Replace it with a new key and revoke it. |
| ERROR "no active admin API key exists" at startup | Every admin key was revoked, expired or no longer has `admin`. (`tr-engine keys revoke`/`update` also warn "no active admin key is left" when you do that.) | `tr-engine keys create --name "admin" --scopes admin` on the host. |
| WARN "`AUTH_TOKEN` is no longer used" (or another old variable) | Old configuration left in `.env`. | Remove it; see [migrating-auth.md](migrating-auth.md). |
| Uploads rejected; WARN "call upload rejected" with `"reason":"key #N lacks upload"` | The plugin uses a key without `upload`. (Other reasons: `no key`, `unknown key`, `revoked key`, `expired key`, `request body too large`.) | Create an upload key for it. |
| Uploads get `429 rate_limited` | Keys in the upload form field are rate-limited per client IP, at two tokens per upload for a legacy (imported) key. | Send the key as `Authorization: Bearer`, replace a legacy upload key with a new `upload` key, or raise `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`. |
| WARN "dropping live audio: several trunk-recorder instances use this short name" | Two instances use the same short name for different systems, so tr-engine can't tell which one sent a simplestream packet. | Set `STREAM_SOURCE_MAP=ip=instance_id,...`, or give the systems unique short names. |
| Browser shows "This tr-engine doesn't support API keys yet" | tr-dashboard is newer than tr-engine. | Upgrade tr-engine. |

## Security notes

- Treat keys like passwords: store them in a password manager or a secrets file with tight permissions, not in shell history or in pages that other people load.
- Use HTTPS for anything beyond localhost. Keys and tickets travel in headers and URLs.
- `POST /api/v1/query` runs SQL as tr-engine's database role inside a read-only transaction. If that role is a superuser (or can read server files), so can an admin key. Run tr-engine with a dedicated, non-superuser database role.
- tr-engine never sets cookies and answers every origin with `Access-Control-Allow-Origin: *` without credentials. A malicious website can't use a visitor's key, because browsers never send keys automatically.
- An `edit` key can set talkgroup and unit names, which web pages display. Pages must render API text as text; the `web/` pages and tr-dashboard do.

---

# Part 2: Client-developer guide

This part is for people writing software against tr-engine: dashboards, bots, mobile apps, websites, scripts. The API reference is [openapi.yaml](../openapi.yaml), also served by every engine at `/api/v1/openapi.yaml` and browsable at `/docs.html`.

## The model

- Your client gets an **API key** from the operator and sends it as `Authorization: Bearer <key>` on every request. tr-engine has no login endpoint, no users and no cookies.
- Your client may also work **without** a key, if the operator has enabled anonymous access.
- `GET /api/v1/whoami` tells your client what its credential can do.
- For the three browser APIs that can't send headers (`EventSource`, `<audio>`, `WebSocket`), mint a short-lived **ticket** and pass it as `?ticket=`.

## Choose your client shape

1. **Single-user app** (a desktop or mobile app, a web app the user runs for themselves, like tr-dashboard): the user pastes a key into the app, and the app stores it (in a browser, `localStorage`). The key holder is the user. Use tickets for streams and audio.
2. **Multi-user frontend with a server** (a website with its own accounts): the key lives **only on your server**. Your server authenticates its users however it likes and calls tr-engine with the header. For live events and audio, your server mints tickets, optionally narrowed per user, and gives them to the browser, which then connects to tr-engine directly without ever seeing the key.
3. **Public listening site**: no key at all. It works when the operator sets anonymous access to `listen`.

A web page with **no server side** that embeds a key gives that key to every visitor. If you're building a page other people load, either use no key (anonymous access), or add a server.

## Sending the key

```bash
curl -H "Authorization: Bearer $TR_KEY" https://tr-engine.example.com/api/v1/calls?limit=5
```

```js
const res = await fetch(`${BASE}/api/v1/calls?limit=5`, {
  headers: { Authorization: `Bearer ${key}` },
});
```

- Only the `Bearer` scheme counts. Keys in the URL (`?key=`, `?token=`) are ignored everywhere.
- A request with a bad key is always `401 invalid_key`. It never silently falls back to anonymous access, so a revoked key shows up at once.
- Uploads are the one exception: `POST /api/v1/call-upload` also reads the key from the multipart form fields `key` or `api_key`, for trunk-recorder's plugins.
- The engine trims spaces around the value after `Bearer` and keeps everything in between. A minted key is `tre_` plus 64 hex characters, but an imported legacy key (an old `AUTH_TOKEN`/`WRITE_TOKEN`) is whatever the old value was, inner spaces included: `Authorization: Bearer purple monkey dishwasher 42` authenticates if that was the old token.
- **Cleaning a pasted key.** A client that takes keys from users should drop invisible characters (U+00AD, U+200B–U+200F, U+202A–U+202E, U+2060–U+2064, U+FEFF), surrounding whitespace and surrounding quotes (typographic quotes always; straight quotes only around a well-formed `tre_` key, since a legacy value may contain them). Then refuse, with a message, tabs, line breaks, anything outside printable ASCII (U+0020–U+007E), and a space inside a `tre_` key. Send everything else to `GET /whoami` and let the engine decide. tr-dashboard and the `web/` pages (`trAuth.cleanKey`) follow this rule, for these reasons: `fetch()` throws before any request on a header value with a line break or a character above U+00FF, which looks like a network failure; a Latin-1 character such as `é` is sent as a single byte, which never matches the UTF-8 bytes the engine hashed when the value was imported; and tabs, like a space inside a `tre_` key, are refused as paste accidents, although browsers send them. A legacy value with inner tabs or non-ASCII characters still works from curl and scripts, which send its bytes unchanged. To use it from tr-dashboard or the `web/` pages, replace it with a minted key (`tr-engine keys create`).

## Discovering what you can do: `GET /api/v1/whoami`

Call it on startup, and again whenever you get a `401 invalid_key` or an `auth` close signal. It is public (it always answers, with or without a key) and sends `Authorization` if you have one.

```json
{
  "credential": "key",
  "key": {
    "id": 7, "name": "tr-dashboard", "prefix": "tre_1a2b3c4d",
    "scopes": ["edit"], "restriction": null, "expires_at": null, "legacy": false
  },
  "scopes": ["edit", "listen"],
  "restricted": false,
  "anonymous": { "access": "listen", "restricted": false },
  "version": "0.10.0"
}
```

- `scopes` is the **effective** set, implications expanded. Gate features on it: show edit buttons if it has `edit`, admin pages if it has `admin`.
- `restricted: true` means some endpoints will answer 403 `restricted_credential` (see [Restricted credentials in clients](#restricted-credentials-in-clients)).
- Without a key: `credential: "anonymous"`, `key: null`, and `scopes` is `["listen"]` if anonymous access is on, `[]` if not. The response is still `200`.
- With a bad key: `401 invalid_key`. Show your "enter a key" screen, and offer "continue without a key" if `anonymous.access` is `listen`.
- A key whose `scopes` lack `listen` is upload-only; tell the user to use a different key for browsing.
- **Older engines** answer `/whoami` with 404. Show "this tr-engine is too old for API keys", not the key screen.

A typical startup:

```js
async function whoami(key) {
  const res = await fetch(`${BASE}/api/v1/whoami`, {
    headers: key ? { Authorization: `Bearer ${key}` } : {},
  });
  if (res.status === 404) return { state: 'engine-too-old' };
  const body = await res.json().catch(() => ({}));
  if (res.status === 401 && body.code === 'invalid_key') return { state: 'invalid-key' };
  if (!res.ok) return { state: 'error', error: body.error };
  if (!key && body.anonymous.access === 'off') return { state: 'needs-key', whoami: body };
  return { state: 'ready', whoami: body };
}
```

## Errors

Error bodies look like `{"code": "...", "error": "human-readable message", "detail": "optional"}`. Branch on `code`, show `error`.

| Status | `code` | What your client should do |
|---|---|---|
| 401 | `key_required` | No key and anonymous access doesn't cover this. Ask for a key. |
| 401 | `invalid_key` | The key is unknown, revoked or expired. Re-run `whoami` and ask for a new key. Don't retry. |
| 401 | `invalid_ticket` | Mint a new ticket and retry once. |
| 403 | `insufficient_scope` | The key can't do this. Tell the user ("your key can't do this; it needs edit"); don't ask for a new key unprompted. The message always reads "this operation needs the <scope> scope". |
| 403 | `restricted_credential` | The credential is restricted and this endpoint can't filter. Hide the feature. |
| 404 | `not_found` | Also returned for resources outside a restriction. |
| 429 | `rate_limited` | Back off; `Retry-After` says how long. |
| 503 | `service_unavailable` | The engine couldn't check your key. Retry later; this is not a bad key. |

Every 401 carries `WWW-Authenticate: Bearer realm="tr-engine"`. Other messages are informational and may change; branch on `code`. A wrong method on an existing path is `405` with code `bad_request`; a path that doesn't exist (including the removed `/auth/*`, `/users` and `/auth-init`) is `404 not_found` for every method.

## Tickets: live events, audio and WebSockets from a browser

A browser's `EventSource`, `<audio src>` and `WebSocket` can't send an `Authorization` header. Don't put the key in the URL (tr-engine ignores it there anyway). Mint a ticket:

```http
POST /api/v1/tickets
Authorization: Bearer tre_...
Content-Type: application/json

{"ttl_seconds": 600}
```

```json
{ "ticket": "trt_eyJrIjo3LCJlIjoxNzkwMDAwMDAwfQ.bWFjLWJ5dGVzLWhlcmU", "expires_at": "2026-09-26T12:10:00Z" }
```

- Needs a key with `listen` (listen, edit or admin). Anonymous callers get `401 key_required`; **without a key you don't need tickets**, just use plain URLs.
- `ttl_seconds` is clamped to 60–3600 (default 600). The body is optional.
- Use it as `?ticket=` (URL-encode it) on `GET /api/v1/events/stream`, `GET /api/v1/audio/live` and `GET /api/v1/calls/{id}/audio`. It is ignored everywhere else.
- A ticket is **not single-use**: an audio element can make many Range requests with the same URL. It works until it expires.
- It reflects its key's **current** state: revoke or re-scope the key and its tickets stop working.
- Treat the ticket as opaque.
- Live-event IDs are opaque too (currently `<unix_ms>-<32 hex characters>`), unique within one engine process but not sequential. Don't parse or compare them; only send the last one back as `last_event_id`.

Only mint tickets when you have a key. A small cache avoids a mint per request:

```js
let cached = null; // { ticket, expiresAt }

async function getTicket({ minRemaining = 60_000 } = {}) {
  if (cached && cached.expiresAt - Date.now() >= minRemaining) return cached.ticket;
  const res = await fetch(`${BASE}/api/v1/tickets`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ ttl_seconds: 900 }),
  });
  if (!res.ok) throw Object.assign(new Error('ticket'), { status: res.status, body: await res.json().catch(() => ({})) });
  const { ticket, expires_at } = await res.json();
  cached = { ticket, expiresAt: Date.parse(expires_at) };
  return ticket;
}

async function withTicket(path, minRemaining) {
  if (!key) return `${BASE}${path}`;           // anonymous: plain URL
  const sep = path.includes('?') ? '&' : '?';
  return `${BASE}${path}${sep}ticket=${encodeURIComponent(await getTicket({ minRemaining }))}`;
}
```

### Live events (`EventSource`)

Things to handle:

1. **Mint a fresh ticket before every (re)connect.** `EventSource`'s built-in reconnect reuses the old URL, and so the old ticket. Close it on `error` and reconnect yourself.
2. **Resume without gaps.** You can't set `Last-Event-ID` on a new `EventSource`, so pass the last event ID you saw as `last_event_id` in the query string. The server buffers 60 seconds of events.
3. **The `auth` event.** When the stream's credential stops allowing it, the server sends `event: auth` with `{"code": ...}` and closes. On `ticket_expired` (the normal end of a ticket's life, at most an hour; also sent when the ticket's narrowing names a system that has since been merged into another), reconnect with a new ticket. On any other code (`invalid_key` when the key is revoked or reaches its expiry, `key_required`, `insufficient_scope`), **don't** reconnect: re-run `whoami` and show the user what changed. The server re-checks every 60 seconds and at once on changes; if a re-check can't reach the database, the stream stays open with its current access until the next one.
4. **Back off.** A `429` ends an `EventSource` for good; your reconnect loop must wait and retry.

```js
function connectEvents(filters, onEvent) {
  let es = null, lastId = null, stopped = false, delay = 1000;

  async function open() {
    const qs = new URLSearchParams(filters);
    if (lastId) qs.set('last_event_id', lastId);
    let url;
    try {
      url = await withTicket(`/api/v1/events/stream?${qs}`, 60_000);
    } catch (e) {
      if (e.status === 401 || e.status === 403) return stopForAuth(e.body.code);
      return retry();
    }
    es = new EventSource(url);
    es.onopen = () => { delay = 1000; };
    for (const type of ['call_start', 'call_end', 'transcription', 'unit_event']) {
      es.addEventListener(type, (ev) => { lastId = ev.lastEventId || lastId; onEvent(type, JSON.parse(ev.data)); });
    }
    es.addEventListener('auth', (ev) => {
      const { code } = JSON.parse(ev.data);
      es.close();
      if (code === 'ticket_expired') open();   // new ticket, resume from lastId
      else stopForAuth(code);
    });
    es.onerror = () => { es.close(); retry(); };
  }

  function retry() {
    if (stopped) return;
    setTimeout(open, delay);
    delay = Math.min(delay * 2, 30_000);
  }

  function stopForAuth(code) {
    stopped = true;
    // Re-run whoami, then show the key screen or an explanation.
    console.warn('event stream stopped:', code);
  }

  open();
  return () => { stopped = true; es?.close(); };
}
```

The `console` event type is only delivered to admin keys; asking for it with another credential is not an error, the events simply don't arrive.

### Call audio (`<audio>`)

`audio_url` in API responses is a **path on the engine** (`/api/v1/calls/48531/audio`) and never contains a credential. Resolve it against the engine's base URL, not your page's origin (they differ when your app is served elsewhere), and add a ticket if you have a key:

```js
async function playCall(audioEl, callId) {
  const path = `/api/v1/calls/${callId}/audio`;
  // A long-lived element needs a ticket with some life left.
  audioEl.src = await withTicket(path, 5 * 60_000);
  audioEl.onerror = async () => {
    audioEl.onerror = null;                      // retry once
    const t = audioEl.currentTime;
    cached = null;                               // force a new ticket
    audioEl.src = await withTicket(path, 5 * 60_000);
    audioEl.currentTime = t;
    audioEl.play();
  };
  await audioEl.play();
}
```

Don't mint tickets for items that sit in a play queue; mint when playback starts. The `call_end` live event carries `call_id` but no `audio_url`; build the path from `call_id`.

### Live audio (`WebSocket`)

```js
async function connectLiveAudio(onFrame) {
  const url = new URL(await withTicket('/api/v1/audio/live', 60_000), location.href);
  url.protocol = url.protocol.replace('http', 'ws');
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => ws.send(JSON.stringify({ type: 'subscribe', systems: [1], tgids: [9178] }));
  ws.onmessage = (ev) => { if (typeof ev.data !== 'string') onFrame(ev.data); };
  ws.onclose = (ev) => {
    if (ev.code === 4401 && ev.reason === 'ticket_expired') connectLiveAudio(onFrame);
    else if (ev.code === 4401 || ev.code === 4403) console.warn('live audio stopped:', ev.reason); // re-run whoami
    else setTimeout(() => connectLiveAudio(onFrame), 2000);  // network blip
  };
  return ws;
}
```

Close codes: `4401` with reason `invalid_key`, `key_required` or `ticket_expired`, and `4403` with reason `insufficient_scope`, sent in the same situations as the live-event `auth` signal (including key expiry and merged-away narrowings). Only `ticket_expired` should reconnect automatically. Any `Origin` is accepted.

- The server sends a keepalive text message and a WebSocket ping every 15 seconds. `active_streams` in the keepalive counts live talkgroup streams; for a restricted credential, only those on talkgroups it may hear.
- A connection from which the server receives nothing, pongs included, for 60 seconds is closed. Browsers answer pings automatically; other clients must too.
- A client message over 64 KiB closes the connection with `1009`.
- Operators: when several trunk-recorder instances use the same short name for different systems, tr-engine can't tell which instance sent a simplestream packet, and drops that sender's audio with a WARN rather than label it with the wrong system (restrictions depend on it). Map senders with `STREAM_SOURCE_MAP=ip=instance_id,...` (IPv6 addresses work too) or give the systems unique short names. A sender's instance is worked out again for every chunk: real trunk-recorder instances are preferred over the `WATCH_INSTANCE_ID` (file watch / `TR_DIR`) identity, which is preferred over the `UPLOAD_INSTANCE_ID` identity. A sender is dropped with a WARN whenever the preferred instances map its short name to different systems, including when such an instance appears after the sender was attributed (a second trunk-recorder, say), and `STREAM_SOURCE_MAP` is then required. Only `STREAM_SOURCE_MAP` entries take an instance out of other senders' candidates; an attribution tr-engine worked out by itself doesn't.

### Non-browser clients

Anything that can set headers (curl, a server, a native app's HTTP library, Node's `fetch`-based SSE readers) can send `Authorization: Bearer` to the stream and audio endpoints too, and doesn't need tickets. Header-authenticated streams have no ticket timer, but they still get the `auth` signal (`invalid_key`) when the key reaches its `expires_at` or is revoked, and `insufficient_scope` when it loses `listen`.

## Multi-user backends

Your server holds the key. For ordinary API calls, it proxies with the header:

```js
// Express. Your own session middleware has put req.user in place.
app.get('/api/calls', requireLogin, async (req, res) => {
  const r = await fetch(`${ENGINE}/api/v1/calls?${new URLSearchParams(req.query)}`, {
    headers: { Authorization: `Bearer ${process.env.TR_ENGINE_KEY}` },
  });
  res.status(r.status).type('json').send(await r.text());
});
```

For live events and audio, mint a ticket per user and hand it to their browser, which then talks to tr-engine directly. You can **narrow** the ticket to what that user may hear:

```js
app.post('/api/tr-ticket', requireLogin, async (req, res) => {
  const r = await fetch(`${ENGINE}/api/v1/tickets`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${process.env.TR_ENGINE_KEY}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      ttl_seconds: 300,
      restriction: { talkgroups: req.user.allowedTalkgroups },  // e.g. ["1:9178", "1:9179"]
    }),
  });
  res.status(r.status).type('json').send(await r.text());
});
```

- The ticket's access is the **intersection** of your key's access and the narrowing. It can never exceed the key.
- A narrowing may hold at most 100 entries in total (the ticket has to fit in a URL). Narrow by system, or mint several tickets.
- A narrowing that allows nothing is accepted and yields a ticket that sees nothing, which is handy for users with no access.
- A narrowing that names a system that was merged into another, in any list including `exclude_talkgroups`, is refused with `400 invalid_body` ("restriction: names a system that was merged into another; use the system it was merged into"). Update your per-user lists after a merge. Stored key and anonymous restrictions keep merged-away IDs in their exclusions (next to a copy for the surviving system), so if you build narrowings from `GET /api/v1/keys` or `GET /api/v1/anonymous-access`, drop the entries naming a merged-away system and keep their copies for the surviving system.
- To cut one user off, stop giving them tickets; their current ticket works until it expires (so keep TTLs short, like 5 minutes) and their stream is then closed. To cut everyone off at once, revoke the key.
- A restricted narrowing means the browser's stream gets the per-type rules for restricted credentials (see below).

**`X-Actor`.** When your server makes a change on behalf of a user, send `X-Actor: <user name>`. tr-engine records it in the audit log and in "who decided" fields, next to your key's name. It is free text, informational only (tr-engine can't check it), cleaned of control characters and cut to 200 characters.

## Restricted credentials in clients

When `whoami.restricted` is true (a restricted listen key, a narrowed ticket, or a restricted anonymous policy):

- Endpoints marked **`x-restricted: enforced`** in the OpenAPI spec work and return only allowed data. A specific call, call group or talkgroup outside the restriction is 404. Ambiguous talkgroup IDs only consider allowed talkgroups. Counts cover only allowed data too: `total`, and a call group's `call_count`, which counts only the recordings the credential may see.
- Endpoints marked **`x-restricted: deny`** answer `403 restricted_credential`. Don't call them: hide the feature (units, affiliations, stats, recorders, ...), and don't poll them.
- Live events: only `call_start`, `call_update`, `call_end`, `transcription` and `unit_event` events for allowed talkgroups arrive. `recorder_update`, `rate_update`, `trunking_message`, `console` and unit events without a talkgroup (registrations) never do.
- Live audio: only allowed talkgroups' audio is sent, whatever you subscribe to.
- A restriction change reaches open streams in place: tr-engine swaps the stream's credential without closing it (and without an `auth` event) while it keeps `listen`. From then on, events for newly excluded talkgroups are filtered out, including the `call_end` of calls already in progress, so a client that tracks active calls from `call_start`/`call_end` should re-check them against `GET /api/v1/calls/active` (for example on a timer, or when `whoami` shows a changed restriction) rather than wait for a `call_end` that won't come.
- Don't mix `enforced` and `deny` calls in one `Promise.all`; a 403 from one would hide the data from the other. Use `Promise.allSettled`.

### Reading the policy from the spec

Every operation in `openapi.yaml` carries:

- `x-scope`: `public`, `listen`, `edit`, `admin` or `upload`;
- `x-restricted`: `enforced` or `deny`;
- `x-key-required: true` where anonymous access never suffices (`POST /tickets`, `/metrics`).

So you can decide what to show without hard-coding the table:

```js
import yaml from 'js-yaml';
const spec = yaml.load(await (await fetch(`${BASE}/api/v1/openapi.yaml`)).text());

const LEVEL = { public: 0, listen: 1, edit: 2, admin: 3 };
function canCall(method, path, whoami) {
  const op = spec.paths[path]?.[method.toLowerCase()];
  if (!op) return false;
  const scope = op['x-scope'];
  if (scope === 'public') return true;
  if (op['x-key-required'] && whoami.credential !== 'key') return false;
  if (whoami.restricted && op['x-restricted'] === 'deny') return false;
  return whoami.scopes.includes(scope);
}

canCall('GET', '/units', me);   // false for a restricted credential
```

## CORS

tr-engine never uses cookies or other ambient credentials, so it answers requests from **any** origin:

- Every response has `Access-Control-Allow-Origin: *` and never `Access-Control-Allow-Credentials`. Don't use `credentials: 'include'`; there is nothing to include.
- Preflight `OPTIONS` requests get `204` with `Access-Control-Allow-Headers: Authorization, Content-Type, Last-Event-ID, X-Actor, X-Request-ID` and all methods.
- Scripts can read the `X-Request-ID`, `Retry-After` and `WWW-Authenticate` response headers.

A browser app on another origin can therefore call tr-engine directly with its key. `EventSource` and `<audio>` to another origin work without CORS configuration.

## Other details

- **Caching.** `/api/v1` JSON responses are `Cache-Control: no-store` with `Vary: Authorization`; call audio is `Cache-Control: private`.
- **`X-Request-ID`.** Send your own (at most 64 characters of `A-Z a-z 0-9 . _ -`) to correlate logs; tr-engine echoes it, or generates one.
- **Uploads.** Upload clients need an `upload` key; see [http-upload.md](http-upload.md).
- **Rate limits.** Anonymous, ticket and legacy-key requests, and uploads with the key in a form field, are limited per IP; keys in the `Authorization` header only if the operator set a per-key limit. Handle `429` with `Retry-After`.

## Checklist for a new client

- [ ] Call `/whoami` at startup; handle `invalid-key`, `needs-key`, `engine-too-old` and `ready`.
- [ ] Send the key only in the `Authorization` header, and only to the engine's origin.
- [ ] Clean pasted keys as in [Sending the key](#sending-the-key); accept legacy keys with inner spaces.
- [ ] Gate features on `whoami.scopes` and `whoami.restricted` (or on `x-scope`/`x-restricted`).
- [ ] Use tickets (only with a key) for `EventSource`, `<audio>` and `WebSocket`; mint right before connecting.
- [ ] Reconnect event streams yourself with a new ticket and `last_event_id`; handle `event: auth`.
- [ ] Handle WebSocket close codes 4401/4403.
- [ ] Resolve `audio_url` against the engine base URL.
- [ ] Render all API text as text (tags and names can contain anything).
- [ ] If other people use your page, don't embed a key: use anonymous access or a server.
