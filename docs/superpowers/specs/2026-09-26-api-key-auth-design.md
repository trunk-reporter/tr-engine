# API-Key Auth Design

**Date:** 2026-09-26
**Status:** Approved
**Supersedes:** `2026-03-28-auth-simplification-design.md` (open/token/full modes) and `2026-03-24-auth-js-consolidation-design.md`
**Breaking:** Yes. Backwards compatibility with the old auth configuration is intentionally dropped; a one-time migration keeps existing upload plugins and admin scripts working (see [Upgrade and legacy migration](#upgrade-and-legacy-migration)).

---

## 1. Problem

tr-engine's auth grew to six environment variables (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, plus `CORS_ORIGINS`), three modes (open/token/full), seven ways to present a credential (JWT, refresh cookie, API key, `AUTH_TOKEN`, `WRITE_TOKEN`, `?token=`, upload form fields), two browser implementations (`web/auth.js`, tr-dashboard) and a reverse-proxy header-injection trick. It confused operators and was a blocker for some users, and the complexity produced real holes:

- In full mode `AUTH_TOKEN` switched from a secret to a value `/auth-init` hands to every visitor — and that public token was accepted for call uploads.
- Open-mode uploads were rejected, the login rate limiter could be bypassed with a spoofed `X-Forwarded-For`, `JWT_SECRET` without `ADMIN_PASSWORD` opened an unauthenticated first-run setup endpoint (fixed in `98b5d2b`).
- Guests were bounced to `/login` in tr-dashboard; live events and audio only worked for guests because Caddy injected a token (fixed in tr-dashboard `393f4a7`).
- `?token=` put long-lived secrets in URLs; CORS reflected any origin *with* credentials; `/debug-report` let anyone make the server ship its (partly unsanitized) configuration to a third party; editors could run SQL over password hashes, write same-origin HTML (`POST /pages`), merge systems and purge tables.

tr-engine is meant to be **an API that any client can be written against**, not one half of a joined whole with tr-dashboard. tr-dashboard is one client; the pages in `web/` are demos of what a client can do.

## 2. Principles

1. **The engine authenticates client software, not people.** Each client (a dashboard deployment, a script, an upload plugin, a public website's backend, a phone app) holds an API key. How a client authenticates *its* users — or whether it has users — is the client developer's business.
2. **One credential type: the API key**, sent in the `Authorization: Bearer` header. The only exception is call upload, which also accepts the key in the `key`/`api_key` multipart fields because trunk-recorder's upload plugins put it there.
3. **Access without a key is a policy, not a token.** The *anonymous access policy* says what a request with no key may do: nothing (default) or listen, optionally restricted to certain systems/talkgroups. No credential is ever handed out to make something "public".
4. **No long-lived secret in a URL.** Browsers can't attach headers to `EventSource`, `<audio src>` or `WebSocket`; for those a client mints a short-lived, listen-only, signed **ticket** and puts that in `?ticket=`.
5. **Fail closed.** Every route has an explicit policy; a route without one is denied. A credential restricted to some systems/talkgroups can only reach endpoints that enforce the restriction; everything else denies it.
6. **No ambient credentials.** No cookies, no sessions. Because nothing is sent automatically by browsers, the API can safely answer cross-origin requests from any site (`Access-Control-Allow-Origin: *`, never with credentials). There is no CSRF surface and no `CORS_ORIGINS` to configure.

### Who holds the key (client shapes)

| Shape | Where the key lives | Who authenticates people | Streams and audio |
|---|---|---|---|
| **Multi-user frontend with a server** (e.g. a club site with Discord login) | On the client's server | The client, however it likes | The server uses headers everywhere. It can mint tickets (optionally narrowed) and give them to its browsers so they fetch audio/streams straight from the engine without ever seeing the key. |
| **Single-user app** (desktop, mobile, a web page you run for yourself, tr-dashboard, the `web/` demo pages) | In the app (browser `localStorage` for web apps) | Nobody — the key holder is the user | Tickets for `EventSource`, `<audio>` and WebSocket. |
| **Public listening site** | No key | Nobody | Anonymous access policy. |

**The one rule clients must follow:** a key that reaches other people's browsers is public. A multi-user page with no server side and an embedded key gives that key's access to every visitor. Multi-user frontends that offer more than public access need a server side. This is stated in the operator and developer docs, and key creation asks what the key is for.

## 3. Concepts

### 3.1 Scopes

| Scope | Grants | Implies |
|---|---|---|
| `listen` | Read radio data (all data `GET`s), stream events and audio, mint tickets | — |
| `edit` | Change radio metadata: talkgroup/unit tags, transcription corrections and review states, re-transcription, unit-tag-suggestion approve/dismiss | `listen` |
| `admin` | Everything else: keys, anonymous policy, system/site identity edits, merges, maintenance, backfill, storage, SQL query, page saving, CSV imports, debug report, console logs, audit log | `edit`, `listen` |
| `upload` | `POST /api/v1/call-upload` only | — (independent) |

A key's `scopes` is a non-empty set containing **at most one** of `listen`/`edit`/`admin`, plus optionally `upload`. Examples: `["listen"]`, `["edit"]`, `["admin","upload"]`, `["upload"]`. `["listen","edit"]` is rejected (redundant; `edit` implies `listen`).

### 3.2 Restrictions

A restriction limits *which radio data* a listen credential can see.

```json
{
  "systems": [1, 3],
  "talkgroups": ["2:9178", "2:9179"],
  "exclude_talkgroups": ["1:5001"]
}
```

- `systems`: system IDs whose talkgroups are allowed.
- `talkgroups`: individual talkgroups (composite `system_id:tgid`) that are allowed.
- `exclude_talkgroups`: talkgroups that are never allowed, even if their system is.
- All three are optional arrays. A restriction whose three arrays are all empty is normalized to **no restriction** (`null`).

**Semantics.** A (system, talkgroup) pair is allowed when:

```
allow_part  = (systems is empty AND talkgroups is empty)       -- exclude-only restriction: everything is a candidate
              OR system ∈ systems
              OR (system, tgid) ∈ talkgroups
allowed     = allow_part AND (system, tgid) ∉ exclude_talkgroups
```

A **system is visible** (its metadata may be shown) when `allow_part` could be true for some talkgroup of it: `systems` and `talkgroups` both empty, or the system is in `systems`, or some entry of `talkgroups` is in that system.

Data that has no talkgroup (NULL tgid) is **not allowed** under any restriction that has a non-empty `talkgroups` or `exclude_talkgroups` list, and is only allowed under a systems-only restriction when its system is listed. (In practice, endpoints whose rows lack a talkgroup are simply denied for restricted credentials — see §6.)

**Where restrictions may appear:**
- On a key **only when its scopes are exactly `["listen"]`**. Keys with `edit`, `admin` or `upload` are never restricted (their endpoints are global operations). Creating or patching a key that violates this is a 400.
- On the anonymous policy (which is listen-only by construction).
- On a ticket, to narrow the minting key's access further.

A request's **effective restriction** is the intersection of every restriction that applies to it (key ∩ ticket). Implementation: a principal carries a list of restrictions; a pair is allowed only if *every* restriction allows it. An empty list means unrestricted.

Restriction limits: at most 1000 entries per array; talkgroup strings must parse as `<positive int>:<non-negative int>`; system IDs must be positive. IDs are not required to exist (a key can be prepared before a system appears).

### 3.3 Principals

Every request is resolved to exactly one principal before routing:

| Kind | From | Scopes | Restrictions |
|---|---|---|---|
| `key` | `Authorization: Bearer <key>` (or upload form field on the upload route) | the key's scopes | the key's restriction, if any |
| `ticket` | `?ticket=` on a ticket-enabled route (GET/HEAD only) | `listen` only, and only if the minting key still exists, is active and has `listen` (via `listen`/`edit`/`admin`) | key's restriction ∩ ticket's restriction |
| `anonymous` | no credential | `listen` if the anonymous policy is `listen`, else none | the anonymous policy's restriction, if any |

If an `Authorization` header is present it wins; a `?ticket=` alongside it is ignored. A **presented but invalid** credential (unknown, revoked or expired key; bad/expired ticket) is always a 401 — it never falls back to anonymous. This makes a mistyped or revoked key obvious instead of silently downgrading to public access.

### 3.4 API keys

Format (unchanged): `tre_` + 64 hex characters (32 random bytes). Stored as SHA-256 hex (`key_hash`); the first 12 characters (`tre_` + 8 hex) are kept as `prefix` for display. The plaintext is returned exactly once, at creation.

Legacy tokens imported during upgrade (§11) are stored the same way (hash of the old value) with `legacy = true`; they do not have the `tre_` prefix. Credential lookup therefore hashes whatever bearer value is presented; the `tre_` prefix is a convention, not a lookup requirement.

Key fields:

| Field | Type | Notes |
|---|---|---|
| `id` | int | |
| `name` | string, 1–100 chars, required | "tr-dashboard at home", "trunk-recorder butco uploads" |
| `prefix` | string | first 12 chars of the key |
| `scopes` | string[] | §3.1 |
| `restriction` | Restriction \| null | §3.2, only with `["listen"]` |
| `expires_at` | RFC3339 \| null | null = never; must be in the future when set |
| `rate_limit_rps` | number \| null | null = not rate limited (§8) |
| `legacy` | bool | true for keys imported from `AUTH_TOKEN`/`WRITE_TOKEN` |
| `created_at` | RFC3339 | |
| `last_used_at` | RFC3339 \| null | updated at most once per minute per key |
| `revoked_at` | RFC3339 \| null | revocation is a soft delete |
| `status` | `active` \| `expired` \| `revoked` | computed |

Only `admin` keys can list, create, change or revoke keys. The API refuses (409 `conflict`) to revoke, expire (set `expires_at` in the past is already a 400), or remove `admin` from the **last active admin key**; the CLI can still do it (and the engine will mint a new bootstrap key on next start, §10).

### 3.5 Tickets

A ticket is a stateless, HMAC-signed, short-lived credential for URLs.

- **Format:** `trt_` + base64url(payload JSON) + `.` + base64url(HMAC-SHA256(ticket_secret, payload bytes)). Payload: `{"k": <key id>, "e": <expiry unix seconds>, "r": <Restriction or omitted>}`.
- **Minting:** `POST /api/v1/tickets` with a key that has `listen` (via `listen`, `edit` or `admin`). Anonymous callers and upload-only keys get 401/403. Body (all optional): `{"ttl_seconds": 600, "restriction": {...}}`. `ttl_seconds` is clamped to 60–3600, default 600. Response `200 {"ticket": "trt_...", "expires_at": "..."}`.
- **Use:** `?ticket=` is honoured only on routes marked ticket-enabled (§6): `GET /api/v1/events/stream`, `GET /api/v1/audio/live`, `GET /api/v1/calls/{id}/audio`. On any other route a `?ticket=` parameter is ignored (the request is treated as if it had no credential).
- **Validation:** signature (constant-time), expiry, then the key is re-resolved by ID: it must still exist, not be revoked or expired, and have `listen`. So revoking a key kills its tickets immediately.
- **Not single-use:** audio elements issue repeated Range requests and `EventSource` may reconnect to the same URL; a ticket stays valid until it expires. Tickets only authorize the stream's *connection*; an established SSE or WebSocket connection is not cut when its ticket expires (it is cut when its key is revoked — see §7.4).
- **Secret:** 32 random bytes generated on first start and stored in `auth_settings` (`ticket_secret`). Not configurable; rotating it (deleting the row and restarting) invalidates all outstanding tickets.

### 3.6 Anonymous access policy

```json
{ "access": "off", "restriction": null }
{ "access": "listen", "restriction": { "exclude_talkgroups": ["1:5001", "1:5002"] } }
```

- `access`: `off` (default on a fresh install) or `listen`. It can never be anything else — anonymous callers can never edit, administer or upload.
- `restriction`: optional (§3.2). Stored even when `access` is `off`, so an operator can prepare it.
- Stored in the database (`auth_settings.anonymous_access`), changed with `PUT /api/v1/anonymous-access` (admin) or `tr-engine access set` (CLI). There is **no environment variable** for it, so there is exactly one source of truth. Changes apply to new requests immediately (the running server reads it through a cache that `PUT` invalidates and that expires after 30 s, so CLI changes apply within 30 s).

## 4. HTTP API

All paths are under `/api/v1`. All responses are JSON unless stated. Errors use the existing `{"code","error","detail?"}` body.

### 4.1 `GET /whoami` — public

Tells any client what it can do. Always reachable; with an invalid credential it returns 401 (`invalid_key` / `invalid_ticket`) so a client learns its key is bad.

```json
{
  "credential": "key",                         // "key" | "ticket" | "anonymous"
  "key": {                                      // null unless credential is key or ticket
    "id": 7, "name": "tr-dashboard", "prefix": "tre_1a2b3c4d",
    "scopes": ["edit"], "restriction": null, "expires_at": null
  },
  "scopes": ["edit", "listen"],                 // effective scopes, implications expanded, sorted: admin, edit, listen, upload
  "restricted": false,                           // true if any restriction applies
  "restrictions": [],                            // every restriction that applies (key, ticket, or anonymous policy); all must allow a talkgroup
  "anonymous": { "access": "listen", "restriction": null },
  "version": "1.0.0"
}
```

`whoami` replaces `/auth-init` and `/auth/me`.

### 4.2 Keys — `admin`

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/keys?include_revoked=false` | — | `200 {"keys": [APIKey...], "total": n}` ordered by id |
| POST | `/keys` | `{name, scopes, restriction?, expires_at?, rate_limit_rps?}` | `201 APIKey + {"key": "tre_..."}` (plaintext once) |
| GET | `/keys/{id}` | — | `200 APIKey` / 404 |
| PATCH | `/keys/{id}` | any of `{name, scopes, restriction, expires_at, rate_limit_rps}`; `null` clears `restriction`/`expires_at`/`rate_limit_rps` | `200 APIKey`; 409 if revoked or last-admin guard; 400 on validation |
| DELETE | `/keys/{id}` | — | `204`; revokes (sets `revoked_at`); idempotent; 409 on last-admin guard |

Validation errors are 400 `invalid_body`/`invalid_parameter` with a message naming the field. Patching scopes away from `["listen"]` while a restriction exists is a 400 (clear the restriction in the same request).

### 4.3 Anonymous access — `admin`

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/anonymous-access` | — | `200 {"access","restriction","updated_at"}` |
| PUT | `/anonymous-access` | `{"access": "off"\|"listen", "restriction": Restriction\|null}` | `200` same shape |

(The *current* policy is also visible to everyone in `GET /whoami`.)

### 4.4 Tickets — `listen` key (not anonymous)

`POST /tickets` — see §3.5.

### 4.5 Audit log — `admin`

`GET /admin/audit-log?limit=50&offset=0&key_id=&since=&until=` → `200 {"entries": [...], "total": n}`, newest first. Entry: `{id, time, key_id, key_name, actor, method, path, status, request_id}`. §9.

### 4.6 Removed endpoints

`/auth-init`, `/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/setup` (GET, POST), `/auth/me`, `/auth/keys` and all its sub-paths, `/users` and all its sub-paths. They are simply gone (404). The `?token=` query parameter is no longer read anywhere.

### 4.7 Error codes and status codes

New codes (added to `internal/api/responses.go` and the OpenAPI `Error.code` enum):

| Status | Code | When |
|---|---|---|
| 401 | `key_required` | No credential and the anonymous policy doesn't allow this (policy `off`, or the route needs more than `listen`). |
| 401 | `invalid_key` | Presented key is unknown, revoked or expired. Message says which of revoked/expired when known. |
| 401 | `invalid_ticket` | Bad signature, malformed, expired, or the minting key is gone/revoked/expired/lacks listen. |
| 403 | `insufficient_scope` | Valid credential without the scope the route needs. Message names the needed scope. |
| 403 | `restricted_credential` | Credential is restricted to some systems/talkgroups and this endpoint can't enforce that. |
| 404 | `not_found` | Also used when a restricted credential asks for a specific resource outside its restriction (so existence isn't revealed). |

Every 401 carries `WWW-Authenticate: Bearer realm="tr-engine"`. Existing codes (`unauthorized`, `forbidden`) remain in the enum but the auth layer no longer emits them.

## 5. Transport rules

- **Keys:** `Authorization: Bearer <key>` only. Keys in the query string are ignored. The upload route additionally accepts form fields `key` and `api_key` (in that order after the header).
- **Tickets:** `?ticket=` only, only on ticket-enabled routes, only for GET/HEAD.
- **CORS:** every response gets `Access-Control-Allow-Origin: *`. Preflight (`OPTIONS`) answers 204 with `Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE, OPTIONS`, `Access-Control-Allow-Headers: Authorization, Content-Type, Last-Event-ID, X-Actor, X-Request-ID`, `Access-Control-Max-Age: 600`. Responses expose `X-Request-ID, Retry-After, WWW-Authenticate` via `Access-Control-Expose-Headers`. `Access-Control-Allow-Credentials` is never sent. `CORS_ORIGINS` is removed.
- **WebSocket** `/audio/live`: any `Origin` is accepted (auth is by header or ticket; there are no ambient credentials).
- **SSE `Last-Event-ID`:** accepted from the `Last-Event-ID` header or, because a client that re-creates an `EventSource` with a fresh ticket can't set that header, from a `last_event_id` query parameter (header wins).

## 6. Route policy

A single table in `internal/api/policy.go` maps every route pattern (`"METHOD /pattern"`, patterns as chi reports them) to a policy:

```go
type RoutePolicy struct {
    Scope      auth.Scope // "" (public), listen, edit, admin, upload
    Restricted Mode       // Deny (default) or Enforced: handler applies the principal's restriction
    Ticket     bool       // ?ticket= accepted
}
```

An `Authorize` middleware (after principal resolution) finds the matched route pattern with `chi.Mux.Match` on the root router, looks up its policy, and:

1. unknown pattern → 403 `forbidden` + error log "route has no auth policy" (fail closed);
2. public → pass;
3. principal lacks the scope → 401 `key_required` (anonymous) or 403 `insufficient_scope` (key/ticket);
4. principal is restricted and `Restricted == Deny` → 403 `restricted_credential`;
5. otherwise pass; for `Enforced` routes the handler must apply the restriction (§7).

A unit test walks every registered route with `chi.Walk` and fails if any lacks a policy, and another asserts the table has no stale entries. A third test asserts that every route with `Restricted: Enforced` is in the list of handlers covered by restriction tests.

### 6.1 The table

**Public** (no credential needed, anonymous policy irrelevant): `GET /api/v1/health`, `GET /api/v1/whoami`, `GET /api/v1/openapi.yaml`, `GET /api/v1/pages`, `GET /favicon.ico`, `GET /*` (static files).

`/health` returns `status`, `version`, and per-check `status` only, unless the principal has unrestricted `listen` (or better), which gets today's full body (TR instance list, pool stats, stream listen address, update check).

**`listen`, restricted = Enforced** (handler applies the restriction):

| Route | Enforcement |
|---|---|
| `GET /systems` | keep systems visible to the restriction |
| `GET /systems/{id}` | 404 unless visible |
| `GET /sites/{id}` | 404 unless its system is visible |
| `GET /talkgroups` | restriction clause in the shared WHERE (count + rows) |
| `GET /talkgroups/{id}` | plain-id ambiguity resolution only considers allowed talkgroups; 404 unless allowed |
| `GET /talkgroups/{id}/calls` | as above, then restricted `ListCalls` |
| `GET /talkgroup-directory` | restriction clause in the shared WHERE |
| `GET /calls` | restriction clause inside `database.ListCalls` (also covers talkgroup/unit call lists); `patched_tgids` filtered to allowed |
| `GET /calls/active` | filter in the loop (always, not only when user filters are present) |
| `GET /calls/{id}` | 404 unless allowed; `patched_tgids` filtered |
| `GET /calls/{id}/audio` (ticket) | the audio-path lookup also returns `system_id`, `tgid`; 404 unless allowed |
| `GET /calls/{id}/frequencies` | lookup returns `system_id`, `tgid`; 404 unless allowed |
| `GET /calls/{id}/transmissions` | same |
| `GET /calls/{id}/transcription` | `GetCallForTranscription` first; 404 unless allowed |
| `GET /calls/{id}/transcriptions` | same |
| `GET /call-groups` | restriction clause in the shared WHERE |
| `GET /call-groups/{id}` | 404 unless the group's (system, tgid) is allowed; member calls filtered defensively |
| `GET /transcriptions/search` | restriction clause on the joined `calls` |
| `GET /transcriptions/batch` | join `calls`; silently omit disallowed call IDs |
| `GET /events/stream` (ticket) | restricted matcher (§7.3) |
| `GET /audio/live` (ticket) | server-side frame check (§7.4) |
| `POST /tickets` | the ticket carries the minting key's restriction plus the requested narrowing |

**`listen`, restricted = Deny:** `GET /p25-systems`, `GET /talkgroups/encryption-stats`, `GET /talkgroups/{id}/units`, `GET /units`, `GET /units/{id}`, `GET /units/{id}/calls`, `GET /units/{id}/events`, `GET /unit-events`, `GET /unit-affiliations`, `GET /unit-tag-suggestions`, `GET /unit-tag-suggestions/{id}`, `GET /stats`, `GET /stats/rates`, `GET /stats/talkgroup-activity`, `GET /stats/call-volume`, `GET /stats/daily-overview`, `GET /stats/category-breakdown`, `GET /stats/call-heatmap`, `GET /analytics/recorder-utilization`, `GET /analytics/decode-rates`, `GET /trunking-messages`, `GET /recorders`, `GET /transcriptions/queue`, `GET /audio/jitter`, `GET /metrics`.

(Units, stats and recorder data either have no talkgroup dimension or leak other talkgroups' activity through aggregates and `last_event_*` fields; supporting them for restricted credentials is future work.)

**`edit`:** `PATCH /talkgroups/{id}`, `PATCH /units/{id}`, `PUT /calls/{id}/transcription`, `POST /calls/{id}/transcribe`, `POST /calls/{id}/transcription/verify`, `POST /calls/{id}/transcription/reject`, `POST /calls/{id}/transcription/exclude`, `POST /unit-tag-suggestions/{id}/approve`, `POST /unit-tag-suggestions/{id}/dismiss`.

**`admin`:** `GET /console-messages`, `PATCH /systems/{id}`, `PATCH /sites/{id}`, `POST /talkgroup-directory/import`, `POST /unit-tags/import`, `POST /admin/systems/merge`, `GET /admin/maintenance`, `POST /admin/maintenance`, `PUT /admin/maintenance/config`, `DELETE /admin/maintenance/config/{key}`, `POST /admin/transcribe-backfill`, `GET /admin/transcribe-backfill`, `DELETE /admin/transcribe-backfill`, `DELETE /admin/transcribe-backfill/{id}`, `GET /admin/storage/stats`, `POST /admin/storage/purge/{table}`, `POST /query`, `POST /pages`, `POST /debug-report`, `GET /keys`, `POST /keys`, `GET /keys/{id}`, `PATCH /keys/{id}`, `DELETE /keys/{id}`, `GET /anonymous-access`, `PUT /anonymous-access`, `GET /admin/audit-log`.

**`upload`:** `POST /call-upload`.

(`edit`/`admin`/`upload` routes are never reachable by a restricted principal, because only `["listen"]` keys, tickets and the anonymous policy can carry restrictions.)

Notable changes from today: system merge, `GET /admin/maintenance`, `GET /admin/transcribe-backfill`, CSV imports, `POST /query`, `POST /pages` and system/site PATCH move from "editor"/"any reader" to `admin`; `POST /debug-report` and `GET /metrics` stop being unauthenticated; `GET /console-messages` becomes admin.

## 7. Restriction enforcement

### 7.1 Shared helpers (`internal/auth`)

New dependency-free package `internal/auth`:

- `Scope`, `Scopes` (validation, implication, `Has`, normalization/sort).
- `TG{SystemID, Tgid}` with `ParseTG("1:9178")`, `String()`, JSON as the composite string.
- `Restriction{Systems []int; Talkgroups []TG; ExcludeTalkgroups []TG}` with `Normalize() *Restriction` (nil when empty; dedupe; sort), `Validate() error`, `AllowsTG(sys, tg int) bool`, `SystemVisible(sys int) bool`.
- `Principal{Kind; KeyID int; KeyName string; Scopes Scopes; Restrictions []Restriction; Actor string}` with `Has(scope)`, `Restricted()`, `AllowsTG`, `SystemVisible` (all restrictions must agree), and
- `(*Principal) SQL(sysCol, tgCol string, firstArg int) (clause string, args []any)` returning `""` for unrestricted principals and otherwise `" AND (...)"` with positional args starting at `$firstArg`. For each restriction it emits only the parts that are non-empty:
  - allow part: `(sysCol = ANY($a::int[]) OR (sysCol, tgCol) IN (SELECT s, t FROM unnest($b::int[], $c::int[]) AS x(s, t)))` (dropping whichever side is empty; omitted entirely for exclude-only restrictions);
  - exclude part: `AND tgCol IS NOT NULL AND NOT EXISTS (SELECT 1 FROM unnest($d::int[], $e::int[]) AS x(s, t) WHERE x.s = sysCol AND x.t = tgCol)`;
  - whenever the restriction has any talkgroup-level list, `AND tgCol IS NOT NULL`.
  Arrays are always passed as non-NULL Go slices (never through the existing `pqIntArray` helpers, which turn empty slices into NULL = "no filter" — the fail-open hazard found during scouting).
- `Ticket` sign/verify (`SignTicket(secret, payload)`, `VerifyTicket(secret, token, now)`).
- `auth` has no dependencies on other tr-engine packages, so `database`, `ingest`, `audio` and `api` can all import it. The request-context accessor (`api.PrincipalFrom(r)`) lives in `internal/api`.

Everything above is table-tested, including the SQL helper against the scratch PostgreSQL (NULL tgids, exclude-only, systems-only, talkgroups-only, multiple restrictions, empty-intersection results).

### 7.2 Database queries

Query-builder filters that serve Enforced routes gain a `Restrict *auth.Principal` (or equivalent) field whose clause is ANDed into the **shared** WHERE so counts and rows agree. User-supplied filters stay separate and ANDed — never "intersect then pass as the user filter", because an empty intersection would become NULL = unfiltered. Single-resource lookups load `system_id`/`tgid` (extending `GetCallAudioPath`, `GetCallFreqList`, `GetCallSrcList` or calling `GetCallForTranscription`) and return 404 when not allowed.

### 7.3 SSE

`EventFilter` gains `Principal *auth.Principal`. For a restricted principal, `matchesFilter` first applies a strict matcher that does **not** honour the zero-value pass-through:

- allowed types: `call_start`, `call_update`, `call_end`, `transcription`, `unit_event` — and only when the event's `SystemID != 0`, `Tgid != 0` and `AllowsTG(SystemID, Tgid)`;
- every other type (`recorder_update`, `rate_update`, `trunking_message`, `console`, and any future type) is dropped.

Then the client's own filters apply as today. Both `Publish` and `ReplaySince` go through `matchesFilter`, so replay is covered.

Also fixed while here: the handler replays before it subscribes, losing events published in between. It must subscribe first, then replay, and skip live events whose ID was already sent by the replay.

### 7.4 Live audio WebSocket

The subscriber stores the principal. `matchesAudioFilter` additionally requires `AllowsTG(frame.SystemID, frame.TGID)` for a restricted subscriber, regardless of what the client's `subscribe` message says (the client can't overwrite it). Keys are re-checked every 60 s for long-lived connections (SSE and WebSocket): if the principal's key has been revoked or has expired, the connection is closed.

## 8. Rate limiting

- Anonymous requests and requests whose credential failed authentication are limited **per client IP** (`RATE_LIMIT_RPS` / `RATE_LIMIT_BURST`, IP from `TrustedProxies.ClientIP`, `TRUSTED_PROXIES`).
- Requests authenticated with a key or ticket are **not** IP-limited. If the key has `rate_limit_rps`, they are limited per key (burst = 2 × rps, minimum 1). A multi-user frontend's traffic all comes from one IP, which is why keyed traffic isn't IP-limited.
- Key resolution is cached in memory (positive and negative results, 30 s TTL, bounded LRU); changing or revoking a key through the API invalidates its cache entry immediately. So the cost of guessing keys is a cache/indexed lookup and a per-IP rate limit.
- `AuthRateLimiter` (the login limiter) is removed.

## 9. Audit log

Table `audit_log (id bigserial PK, time timestamptz default now(), key_id int NULL, key_name text NOT NULL, actor text NULL, method text, path text, status int, request_id text)`, index on `(time DESC)` and `(key_id, time DESC)`.

- Middleware records, after the handler returns, every request with a method other than GET/HEAD/OPTIONS, **except** `POST /api/v1/call-upload` and `POST /api/v1/tickets` (high volume, no state change of note). The path is recorded without the query string.
- `X-Actor` request header: optional, free text (trimmed, control characters removed, max 200 chars) naming the end user a multi-user client acted for. Recorded, never used for authorization.
- `unit_tag_suggestions.decided_by` and `system_merge_log.merged_by` store the key name (plus ` / <actor>` when present) instead of a username / the literal `"api"`.
- Retention: `RETENTION_AUDIT_LOG` (default `8760h` = 1 year), purged by the daily maintenance run.

## 10. Bootstrap and CLI

### 10.1 Bootstrap admin key

After migrations, on every start: if there is no active (not revoked, not expired) key with `admin` scope, create one named `bootstrap admin` with scopes `["admin"]` and print it once, prominently, at WARN level:

```
================================================================
 No admin API key exists, so one was created:

   tre_4f1c…

 Store it now — it will not be shown again. Use it as a Bearer
 token (or paste it into a client) to create more keys.
 Create or revoke keys later with:  tr-engine keys --help
================================================================
```

A lost admin key is recovered by revoking/creating keys with the CLI, or by revoking all admin keys and restarting. Both require access to the host, which is the point.

### 10.2 CLI

New subcommands, following the `export`/`import` pattern in `cmd/tr-engine` (own FlagSet, `--env-file`/`--database-url`, `config.Load`, `database.Connect`, `InitSchema`+`Migrate`):

```
tr-engine keys list [--all]
tr-engine keys create --name NAME --scopes listen|edit|admin[,upload] | upload
                      [--expires 90d|720h|2026-12-31|2026-12-31T00:00:00Z]
                      [--systems 1,2] [--talkgroups 1:9178,1:9179] [--exclude-talkgroups 1:5001]
                      [--rate-limit RPS]
tr-engine keys revoke ID|PREFIX
tr-engine access show
tr-engine access set --anonymous off|listen [--systems ...] [--talkgroups ...] [--exclude-talkgroups ...]
```

`keys create` prints the plaintext key once on stdout (and nothing else on stdout, so it can be captured by scripts); messages go to stderr. `keys revoke` accepts a numeric ID or a unique prefix. The CLI is not subject to the last-admin guard. `--expires` accepts a Go duration, a `d` suffix (days), a date or an RFC3339 time.

## 11. Upgrade and legacy migration

### 11.1 Schema

Migrations (appended to `internal/database/migrations.go`; the old "create users table" and "add display_name and last_login to users" entries are **removed** so fresh databases never create `users`; the old "create api_keys table" entry is changed to create the new shape directly):

1. **api_keys → app-key model** (check: column `scopes` exists). In one statement block: add `scopes text[]`, `restriction jsonb`, `expires_at timestamptz`, `revoked_at timestamptz`, `rate_limit_rps real`, `legacy boolean NOT NULL DEFAULT false`; rename `label` → `name`; fill `name` with `COALESCE(NULLIF(label,''), 'key ' || key_prefix)` and, when the key belonged to a user and the `users` table exists, append ` (<username>)`; map `role` → scopes (`viewer`→`{listen}`, `editor`→`{edit}`, `admin`→`{admin}`) and add `upload` for `is_service_account` keys (service keys were the documented upload credential; other keys lose the upload ability they only had through a bug); set `scopes NOT NULL`; drop `user_id`, `role`, `is_service_account`, `idx_api_keys_user_id`. Add `CHECK (cardinality(scopes) > 0)`.
2. **drop users table** (check: `users` doesn't exist): `DROP TABLE IF EXISTS users CASCADE`.
3. **auth_settings** (`name text PRIMARY KEY, value jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()`), also added to `schema.sql`.
4. **audit_log** (§9), also added to `schema.sql`.

### 11.2 Legacy environment variables

The engine reads the old variables **only** to migrate and to warn; they no longer configure anything.

On the first start of the new version (claimed once per database in `data_fixups` as `import-legacy-auth`, detail records what was done):

| Old configuration | What happens |
|---|---|
| `WRITE_TOKEN` set | Imported as key `legacy WRITE_TOKEN`, scopes `["admin","upload"]`, `legacy=true`. Scripts and upload plugins using it keep working. |
| `AUTH_TOKEN` set **without** `ADMIN_PASSWORD` (token mode: a private shared secret) | Imported as key `legacy AUTH_TOKEN`, scopes `["listen","upload"]` (token mode used it for reads and uploads), `legacy=true`. |
| `AUTH_TOKEN` set **with** `ADMIN_PASSWORD` (full mode: `AUTH_TOKEN` was the public read token) | Not imported (it was public). Anonymous policy set to `listen`. |
| Neither `AUTH_TOKEN` nor `ADMIN_PASSWORD` set, or `AUTH_ENABLED=false`, **and the database already had data** (`systems` not empty — an upgrade from open mode) | Anonymous policy set to `listen`, so a previously open instance stays readable. Writes now need a key (bootstrap key printed). |
| Fresh database | Nothing imported; anonymous policy stays `off`. |

An imported token whose hash matches an existing key is not duplicated. The import happens before the bootstrap check, so an imported `WRITE_TOKEN` counts as the admin key (no bootstrap key is printed in that case).

On **every** start, each old variable that is still set produces one WARN line: `AUTH_TOKEN is no longer used (imported as API key #3 "legacy AUTH_TOKEN" on 2026-09-26) — remove it from your configuration; see docs/migrating-auth.md`, or for variables that were never imported: `ADMIN_PASSWORD is no longer used — tr-engine has no user accounts; clients use API keys. See docs/auth.md`. `CORS_ORIGINS` → "no longer needed: the API allows all origins and never uses cookies".

### 11.3 What operators must do

Covered step by step in `docs/migrating-auth.md`: copy the bootstrap key from the log (if printed), check `tr-engine keys list`, replace legacy keys with named per-client keys and revoke the legacy ones, set the anonymous policy deliberately, remove the Caddy token-injection block, remove old variables from `.env`, and give each upload plugin its own `upload` key.

## 12. Clients

### 12.1 tr-dashboard (single-user app + public viewer)

- **State:** `useAuthStore` becomes `{ apiKey (persisted, localStorage), whoami, status: 'loading' | 'ready' | 'needs-key' | 'invalid-key' | 'error', error }` with helpers `hasScope(scope)`, `canEdit()`, `isAdmin()`, `restricted`.
- **Startup (`RequireAuth` → `AuthGate`):** `GET /whoami` with the stored key. 401 → `invalid-key` (key-entry screen explaining the key was rejected, with a "continue without a key" option when anonymous access allows listening). No key and anonymous `off` → `needs-key` screen. Otherwise `ready` (anonymous visitors browse read-only).
- **Key entry:** a full-page "Connect to tr-engine" form (paste key → validate with `/whoami` → store) and an "API key" card in Settings (show name/prefix/scopes from `whoami`, replace, forget). No login page, no users page.
- **Requests:** `Authorization: Bearer <key>` from `request()` when a key is stored; nothing otherwise. No refresh logic. 401 → re-run `whoami`, show key screen if the key is now invalid. 403 `insufficient_scope`/`restricted_credential` → a clear message ("Your key can't do this: needs edit scope").
- **Write gating:** edit buttons (talkgroup/unit edit, unit-tag suggestions) require `edit`; Admin page, key management and anonymous policy require `admin`. Restricted/anonymous visitors don't see widgets backed by Deny endpoints (units, stats, recorders…) or see a "not available for this key" placeholder instead of an error.
- **Tickets:** `getTicket()` caches one ticket per key until 60 s before expiry; `withTicket(url)` is async. SSE: when a key is stored, mint a ticket right before every (re)connect; on `error`, close and reconnect with a fresh ticket and `last_event_id`. Audio: `AudioPlayer` builds the URL from `API_BASE` + `/calls/{id}/audio` (not the root-relative `audio_url`, which breaks with an absolute `VITE_API_BASE`) and appends a freshly minted ticket right before setting `src` (queued items must not carry tickets minted at enqueue time).
- **Access page** (`/access`, admin): keys list/create/edit/revoke (plaintext shown once with a copy button, and a warning box: "keys used in web pages other people load are public"), anonymous policy editor, recent audit log.
- **Removed:** `Login.tsx`, `Users.tsx`, `/login`, `/users`, `login/refresh/logout/setup` client functions, the `readToken`/`writeToken`/`accessToken`/`jwtEnabled`/`user` state, the guest state, `credentials: 'include'`.
- **Dev proxy:** `TR_AUTH_TOKEN` becomes `TR_API_KEY`, injected only when the browser request has no `Authorization` header.
- **Types:** regenerate `src/api/generated.ts` from the new `openapi.yaml`.

### 12.2 Engine demo pages (`web/`)

- **`auth.js`** rewritten around one stored key (`localStorage['tr-engine-api-key']`, every access in try/catch; old `tr-engine-token`/`-write-token`/`-jwt` entries removed on load):
  - `fetch` patch: same-origin `/api/` URLs only; adds `Authorization` when a key is stored and the caller didn't set one; preserves `Request` headers. 401 → key prompt (paste key) and one retry; 403 `insufficient_scope`/`restricted_credential` → explanatory modal with a "use a different key" button.
  - `EventSource` replacement: a wrapper that, when a key is stored, mints a ticket (async), connects with `?ticket=`, and on error re-mints and reconnects with `last_event_id`; it re-attaches listeners and mirrors `readyState`, `onopen`/`onmessage`/`onerror`, `addEventListener`, `close`, and the static constants. Without a key it behaves like the native `EventSource`.
  - Public API `window.trAuth`: `ready()`, `getKey()`, `setKey(k)`, `clearKey()`, `whoami()` (cached), `hasScope(s)`, `showKeyPrompt()`, `mediaUrl(url)` (sync; appends the cached ticket for same-origin API URLs when a key is stored; the cache is refreshed in the background), `ticketUrl(url)` (async).
  - No synchronous XHR.
- `admin.html`: key management + anonymous policy + audit log (replacing users/setup/login UI). `storage.html`: gate on `whoami` admin scope.
- The five `<audio>` sites (`call-history`, `talkgroup-research`, `irc-radio-live`, `scanner`, `scanner-classic`) use `trAuth.mediaUrl(url)`; the key/ticket is only attached to same-origin URLs (fixes the "token sent to any `?api=` origin" bug). `audio-engine.js` connects with `await trAuth.ticketUrl(...)`.
- `units.html` calls `trAuth.showKeyPrompt()` (the old `showAuth()` didn't exist). `emergency-log.html` uses `audio_url` (it read a field the API never returns). `irc-radio-live.html` loses the captured Cloudflare beacon script (third-party JS on the origin that stores the key). `playground.html`'s generated-page instructions describe keys and tickets. `theme-engine.js` adds an "API key…" item to the nav menu when `trAuth` is present.
- All pages bump `auth.js?v=3`.

## 13. Configuration summary

| Variable | Status |
|---|---|
| `AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS` | **Removed.** Read only for the one-time import and to warn (§11.2). |
| `TRUSTED_PROXIES` | Kept (default `loopback,private`). |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | Kept; now apply to anonymous/failed requests only. |
| `RETENTION_AUDIT_LOG` | New, default `8760h`. |

Docker Compose files drop the `TR_AUTH_TOKEN` variable passed to tr-dashboard; `caddy/Caddyfile` drops the token-injection block.

## 14. Security properties (acceptance criteria)

1. No endpoint other than those in §6 "Public" responds with data to a request without a valid credential unless the anonymous policy is `listen`, and then only listen routes.
2. The anonymous policy can't grant more than `listen`.
3. A revoked or expired key stops working within one request cycle when revoked through the API (≤30 s via CLI), including its tickets and its open SSE/WebSocket connections (≤60 s).
4. No key is accepted from a URL. Tickets are accepted only on the three ticket routes, only for GET/HEAD, grant only `listen`, and never outlive 1 hour.
5. A restricted principal gets 403 `restricted_credential` on every non-Enforced route and never receives rows, events or audio frames outside its restriction on Enforced routes (including counts/totals and `patched_tgids`).
6. `Access-Control-Allow-Credentials` never appears; no cookies are set.
7. Every registered route has a policy (test); unknown routes fail closed.
8. Upload requires a key with `upload`; nothing else can upload.
9. `/debug-report`, `/query`, `/pages` (POST), `/metrics`, merges, maintenance, storage, CSV imports and system/site identity edits require `admin`.
10. The login rate limiter no longer exists; per-IP limits use `TRUSTED_PROXIES` (already shipped in `98b5d2b`).

## 15. Bugs fixed alongside

Found while mapping the code for this design; fixed in the same change because they are auth-adjacent or touched by it:

- `ListTalkgroupUnits` joined `units` on `unit_id` only, returning same-numbered units from other systems (cross-system leak, wrong totals).
- `POST /admin/storage/purge/{table}` accepted zero/negative `older_than`, deleting every row (or dropping every `mqtt_raw_messages` partition including the current one); its error text suggested `7d`, which Go durations reject. Now requires a positive duration ≥ 1h and accepts a `d` suffix.
- SSE replay-before-subscribe gap (§7.3).
- `PUT /calls/{id}/transcription` with an invalid `source` returned 500 (DB CHECK) instead of 400.
- `POST /calls/{id}/transcribe` for a nonexistent call returned 503 instead of 404.
- `GET /talkgroup-directory` and `GET /recorders` returned `null` instead of `[]` when empty.
- `GET /calls/{id}/frequencies` and `/transmissions` ignored pagination-parameter errors.
- `GET /unit-affiliations` `summary.talkgroup_counts` was keyed by tgid only, merging systems (now keyed `system_id:tgid`).
- `debug-report` shipped the unsanitized trunk-recorder `config.json` (upload API keys, MQTT credentials); it now redacts keys named like `*key*`, `*token*`, `*password*`, `*secret*` at any depth, and requires `admin`.
- web: token appended to cross-origin `?api=` audio URLs; `auth.js` dropped `Request` headers; token-mode users could never enter a token (login form shown instead); `units.html` called an undefined `showAuth()`; `emergency-log.html` read a non-existent field.
- tr-dashboard: `/login` spun forever on direct load; token mode had no credential for SSE/audio; `canWrite()` over-reported in token mode; audio URLs ignored an absolute `VITE_API_BASE`; URL credentials were frozen at enqueue time.

**Found but out of scope** (tracked in `docs/roadmap.md`): `GetTalkgroupByComposite`/`GetUnitByComposite` don't exclude soft-deleted systems (sqlc queries); `PATCH /sites/{id}` doesn't invalidate the ingest identity cache; `POST /query` runs as the application DB role, which can `pg_read_file` if that role is privileged (now admin-only; a dedicated read-only role is recommended in the docs); CDN scripts without SRI in `web/`; `audio-diagnostics.html` posts to a hard-coded external URL.

## 16. Implementation outline

1. `internal/auth` package + tests.
2. Database: migrations, `api_keys.go` rewrite, `auth_settings` (anonymous policy, ticket secret), `audit_log`, legacy import fixup; delete `users.go`.
3. API: principal resolution middleware, route policy + `Authorize`, CORS, rate limiting, audit middleware; endpoints `whoami`, keys, anonymous-access, tickets, audit-log; upload auth; delete `auth.go`, `setup.go`, `users.go`, old middleware and tests; `server.go` wiring; config cleanup; `main.go` bootstrap key, legacy warnings, `keys`/`access` CLI; drop `golang-jwt`.
4. Restriction enforcement on every Enforced route, SSE, WebSocket, audio; §15 engine bug fixes.
5. `web/` demo pages.
6. Docs: `openapi.yaml`, new `docs/auth.md`, rewritten `docs/migrating-auth.md`, `docs/http-upload.md`, README, CLAUDE.md/AGENTS.md, `sample.env`, compose files, Caddyfile, install script, `web/*.md`.
7. tr-dashboard rewrite (§12.1), regenerated types, README/AGENTS/CLAUDE/CHANGELOG.
8. Verification: unit + DB integration tests, route-policy tests, an end-to-end run (engine against a scratch PostgreSQL, curl matrix over every route × principal kind, Playwright on tr-dashboard and the demo pages), adversarial security review.
