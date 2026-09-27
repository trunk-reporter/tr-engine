# API-Key Auth Design

**Date:** 2026-09-26
**Status:** Approved (revision 2, after security / client-developer / operator / implementability review)
**Supersedes:** `2026-03-28-auth-simplification-design.md` (open/token/full modes) and `2026-03-24-auth-js-consolidation-design.md`
**Breaking:** Yes. Backwards compatibility with the old auth configuration is intentionally dropped. A one-time migration keeps existing upload plugins and admin scripts working where that is safe (§11).

---

## 1. Problem

tr-engine's auth grew to seven environment variables (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS`), three modes (open/token/full), seven ways to present a credential (JWT, refresh cookie, API key, `AUTH_TOKEN`, `WRITE_TOKEN`, `?token=`, upload form fields), two browser implementations (`web/auth.js`, tr-dashboard) and a reverse-proxy header-injection trick. Operators found it confusing and it blocked some users. The complexity also produced real holes:

- In full mode `AUTH_TOKEN` stopped being a secret: `/auth-init` handed it to every visitor. That public token was accepted for call uploads.
- Open-mode uploads were rejected.
- The login rate limiter could be bypassed with a spoofed `X-Forwarded-For`.
- `JWT_SECRET` without `ADMIN_PASSWORD` opened an unauthenticated first-run setup endpoint.

These four were fixed in `98b5d2b`.

- tr-dashboard sent guests to `/login`.
- Live events and audio worked for guests only because Caddy injected a token.

These two were fixed in tr-dashboard `393f4a7`.

Still open:
- `?token=` put long-lived secrets in URLs.
- CORS reflected any origin *with* credentials.
- `/debug-report` let anyone make the server send its configuration, including the raw trunk-recorder config, to a third party.
- Editors could run SQL over password hashes, write same-origin HTML (`POST /pages`), merge systems and purge tables.

tr-engine is meant to be **an API that any client can be written against**, not one half of a joined whole with tr-dashboard. tr-dashboard is one client; the pages in `web/` are demos of what a client can do.

## 2. Principles

1. **The engine authenticates client software, not people.** Each client holds an API key: a dashboard deployment, a script, an upload plugin, a public website's backend, a phone app. How a client authenticates *its* users, or whether it has users at all, is up to the client developer.
2. **One credential type: the API key**, sent as `Authorization: Bearer`. The only exception is call upload, which also reads the key from the `key`/`api_key` multipart fields, because trunk-recorder's upload plugins put it there.
3. **Access without a key is a policy, not a token.** The *anonymous access policy* says what a request with no key may do: nothing (the default), or listen, optionally restricted to certain systems or talkgroups. The engine never hands out a credential to make something "public".
4. **No long-lived secret in a URL.** Browsers can't attach headers to `EventSource`, `<audio src>` or `WebSocket`. For those, a client mints a short-lived, listen-only, signed **ticket** and puts it in `?ticket=`.
5. **Fail closed.** Every matched route has an explicit policy, and a matched route without one is denied. A credential restricted to some systems or talkgroups can only reach endpoints that enforce the restriction; everything else denies it. An empty allow list means "nothing", never "everything".
6. **No ambient credentials.** No cookies, no sessions. Browsers send nothing automatically, so the API can safely answer cross-origin requests from any site (`Access-Control-Allow-Origin: *`, never with credentials). There is no CSRF surface and no `CORS_ORIGINS` to configure.

### Who holds the key (client shapes)

| Shape | Where the key lives | Who authenticates people | Streams and audio |
|---|---|---|---|
| **Multi-user frontend with a server** (e.g. a club site with Discord login) | On the client's server | The client, however it likes | The server uses headers everywhere. It can mint tickets (optionally narrowed per user) and hand them to its browsers, which then fetch audio and streams directly from the engine without ever seeing the key. |
| **Single-user app** (desktop, mobile, a web page you run for yourself, tr-dashboard, the `web/` demo pages) | In the app (browser storage for web apps) | Nobody: the key holder is the user | Tickets for `EventSource`, `<audio>` and WebSocket. |
| **Public listening site** | No key | Nobody | Anonymous access policy. |

**The one rule clients must follow:** a key that reaches other people's browsers is public. A multi-user page with no server side and an embedded key gives that key's access to every visitor. So does a reverse proxy that injects a key into anonymous requests. Multi-user frontends that offer more than public access need a server side. The operator and developer docs state this, and key creation asks what the key is for.

## 3. Concepts

### 3.1 Scopes

| Scope | Grants | Implies |
|---|---|---|
| `listen` | Read radio data (all data `GET`s), stream events and audio, mint tickets | — |
| `edit` | Change radio metadata: talkgroup/unit tags, transcription corrections and review states, re-transcription, unit-tag-suggestion approve/dismiss | `listen` |
| `admin` | Everything else: keys, anonymous policy, system/site identity edits, merges, maintenance, backfill, storage, SQL query, page saving, CSV imports, debug report, console logs, audit log | `edit`, `listen` |
| `upload` | `POST /api/v1/call-upload` only | — (independent) |

A key's `scopes` is a non-empty set. It holds **at most one** of `listen`/`edit`/`admin`, optionally plus `upload`. Valid examples: `["listen"]`, `["edit"]`, `["admin","upload"]`, `["upload"]`. `["listen","edit"]` is rejected with 400, because `edit` already implies `listen`. Unknown scope strings are rejected with 400. Scopes are stored sorted (`admin`, `edit`, `listen`, `upload`).

### 3.2 Restrictions

A restriction limits *which radio data* a listen credential can see.

```json
{ "systems": [1, 3], "talkgroups": ["2:9178", "2:9179"], "exclude_talkgroups": ["1:5001"] }
{ "allow_all": true, "exclude_talkgroups": ["1:5001", "1:5002"] }
```

| Field | Type | Meaning |
|---|---|---|
| `allow_all` | bool, default false | Every talkgroup is a candidate. It may not be combined with non-empty `systems` or `talkgroups` (400). |
| `systems` | int[] | Systems whose talkgroups are allowed. |
| `talkgroups` | string[] (`"system_id:tgid"`) | Individual allowed talkgroups. |
| `exclude_talkgroups` | string[] | Talkgroups that are never allowed, even when their system is. |

**Only `restriction: null` (or an omitted field) means unrestricted.** A restriction object is never turned into null. If `allow_all` is false and `systems` and `talkgroups` are both empty, the restriction **allows nothing**. `{}` is such a restriction.

**Semantics.** A (system, talkgroup) pair is allowed when:

```
allowed = tgid > 0 AND system > 0          (NULL fails both)
          AND (allow_all OR system ∈ systems OR (system, tgid) ∈ talkgroups)
          AND (system, tgid) ∉ exclude_talkgroups
```

Data with a NULL, zero or negative talkgroup or system ID is **never** allowed for a restricted principal, whatever the restriction says.

A **system is visible**, meaning its metadata may be shown, when `allow_all` is true, or the system is in `systems`, or at least one entry in `talkgroups` belongs to it.

**Where restrictions may appear:**

| On | Rules |
|---|---|
| A key | Only when its scopes are exactly `["listen"]`; any other key is 400. A key restriction that allows nothing is rejected with 400 "restriction allows nothing". |
| The anonymous policy | It is listen-only by construction. A restriction that allows nothing is rejected with 400 "use access: off instead". |
| A ticket | Used to narrow the minting key's access further (§3.5). A narrowing that allows nothing is accepted; the ticket then sees nothing. This lets a multi-user backend hand a no-access user a harmless ticket. |

A request's **effective restriction** is the intersection of every restriction that applies to it (key ∩ ticket narrowing). In the implementation, a principal carries a list of restrictions, and a pair is allowed only if *every* restriction allows it. An empty list means unrestricted.

**Validation:**
- Each array of a **new** restriction (key create/PATCH, anonymous PUT, the CLI) holds at most 1000 entries (tickets: see §3.5). The limit applies to input only: merge rewrites keep every exclusion and add copies (below), so a stored `exclude_talkgroups` can grow past 1000. Such a stored restriction stays valid, and sending it back unchanged (`PATCH /keys/{id}` with the stored restriction, `PUT /anonymous-access`, `access set` without restriction flags) is accepted.
- Talkgroup strings must parse as `<positive int>:<positive int>`.
- System IDs must be positive.
- IDs are not required to exist, so a key can be prepared before its system appears.
- Duplicates are removed and arrays are stored sorted.

**System merges.** `MergeSystems` runs both from the admin API and automatically at ingest when P25 identity matches. In the same transaction it rewrites every restriction reference to the source system: `systems` entries and `source:tgid` entries in `talkgroups` become the target's; `exclude_talkgroups` is kept **symmetric** across the two IDs: every entry naming the source or the target is kept and gets a copy naming the other one, and replaying this over the logged merges until nothing changes makes an exclusion naming any system of a chain of merges (a into b, b into c) name all of them. Data still carrying an old ID (written or buffered around the merge, and never moved) thus stays excluded, whichever ID the exclusion named. It does this for every row of `api_keys.restriction` and for `auth_settings.anonymous_access`, dedupes, and then bumps the auth generation (§7.5). A restriction stored later (key create/PATCH, anonymous PUT, the CLI) that names a system appearing in `system_merge_log` is rewritten the same way on store (allow entries moved, exclusions made symmetric), under an advisory lock shared with the merge. A ticket narrowing that names a merged-away system in any array, `exclude_talkgroups` included, is refused at mint time (400 `invalid_body`); a ticket minted before the merge is rejected as `invalid_ticket` on use (and its open stream closes with `ticket_expired`); the client simply mints a new one. Stored key and anonymous restrictions keep merged-away IDs in their exclusions, so a backend that builds narrowings from them must drop those entries (their copies for the surviving system stay).

A merge never leaves an allow entry (`systems`, `talkgroups`) pointing at a merged-away system and never voids an exclusion (exclusions keep the old ID next to the surviving one), but it can widen access: an allow entry that named either system then covers the whole merged system, including data the other system already held. That is intended when both are one radio network; an admin merging unrelated systems must review restricted keys and the anonymous policy first.

### 3.3 Principals and credential presentation

Every request resolves to exactly one principal:

| Kind | From | Scopes | Restrictions |
|---|---|---|---|
| `key` | `Authorization: Bearer <key>`, or on the upload route the `key`/`api_key` form field | the key's scopes | the key's restriction, if any |
| `ticket` | `?ticket=` on a ticket-enabled route | `listen` only, and only if the minting key still exists, is active and has `listen` (via `listen`, `edit` or `admin`) | the key's *current* restriction ∩ the ticket's narrowing |
| `anonymous` | no credential | `listen` if the anonymous policy is `listen`, otherwise none | the anonymous policy's restriction, if any |

**What counts as a presented credential:**
- `Authorization` is a credential only if its scheme is `Bearer` (compared case-insensitively) **and** the token is non-empty after trimming. Any other scheme (for example `Basic` from a proxy's HTTP auth), or an empty `Bearer`, is treated as **no credential**.
- A bearer token whose SHA-256 equals `auth_settings.retired_public_token` (the old full-mode public `AUTH_TOKEN`, §11.2) is treated as **no credential**. The engine logs a WARN at most once an hour: "a request carried the pre-upgrade public AUTH_TOKEN — a reverse proxy is probably still injecting it; remove the injection".
- On a ticket-enabled route, a `?ticket=` takes precedence over an `Authorization` header, because a URL ticket is always the client's own choice while a header may have been injected by a proxy. On other routes `?ticket=` is ignored completely.
- A **presented but invalid** credential is always a 401 and never falls back to anonymous. Invalid means an unknown, revoked or expired key, or a bad, expired or orphaned ticket. This way a mistyped or revoked key shows up immediately instead of silently getting public access.

### 3.4 API keys

**Format** (unchanged): `tre_` followed by 64 hex characters (32 random bytes). Only the SHA-256 hex digest is stored (`key_hash`). `prefix` is the first 12 characters (`tre_` + 8 hex) and is kept for display. The plaintext is returned exactly once, at creation.

**Legacy keys** are imported from `AUTH_TOKEN`/`WRITE_TOKEN` (§11) or with `tr-engine keys import`. They are stored as the hash of the old value with `legacy = true`. Their `prefix` is `legacy_` followed by the first 6 hex characters of `key_hash`, so no character of the old secret is ever stored or shown. Credential lookup hashes whatever bearer value is presented; the `tre_` prefix is a convention, not a lookup requirement.

**Fields:**

| Field | Type | Notes |
|---|---|---|
| `id` | int | |
| `name` | string, 1–100 characters, required | e.g. "tr-dashboard at home", "trunk-recorder butco uploads" |
| `prefix` | string | see above |
| `scopes` | string[] | §3.1 |
| `restriction` | Restriction \| null | §3.2; only allowed with `["listen"]` |
| `expires_at` | RFC3339 \| null | null means never; must be in the future when set |
| `rate_limit_rps` | number \| null | null means no per-key limit (§8); must be > 0 when set |
| `legacy` | bool | true for imported legacy tokens |
| `created_at` | RFC3339 | |
| `last_used_at` | RFC3339 \| null | updated at most once per minute per key |
| `revoked_at` | RFC3339 \| null | revocation is a soft delete |
| `status` | `active` \| `expired` \| `revoked` | computed |

**Management.** Only `admin` keys can list, create, change or revoke keys.

**Last-admin guard.** The API refuses with 409 `conflict` to revoke an active admin key, to change its scopes so it loses `admin`, to move its `expires_at` earlier (including giving it one), or to set or lower its `rate_limit_rps` below 1 request/second, unless another active admin key lasts at least as long (no expiry, or one no earlier than this key's current expiry). Only admin keys not rate-limited below 1 request/second count as that other key, for revoke, demote, earlier expiry and throttle alike. Otherwise a near-future expiry, a limit of one request an hour, or shortening one admin key and then revoking the other, would leave no usable admin key as surely as revoking the last one; in particular the only admin key can't be given an expiry, or such a rate limit, through the API. The throttle case has its own message ("a rate limit below 1 request/second on this admin key would leave no admin key that can lift it: create another admin key without a rate limit first"). A request that sets `expires_at` in the past is already a 400. The guard runs in one transaction that takes `pg_advisory_xact_lock` on a fixed constant before counting, so concurrent requests can't both pass. The CLI is not subject to the guard; `tr-engine keys update ID --no-rate-limit` undoes an over-tight limit.

### 3.5 Tickets

A ticket is a stateless, HMAC-signed, short-lived credential for URLs.

**Format:** `trt_<payload>.<mac>`.
- `<payload>` is unpadded base64url of a compact JSON object `{"k": <key id>, "e": <expiry, unix seconds>, "n": <narrowing Restriction, omitted if none>}`.
- `<mac>` is unpadded base64url of `HMAC-SHA256(ticket_secret, ASCII bytes of <payload>)`. The MAC covers the encoded segment, JWS-style.

**Verification order:**
1. Strict base64url for both parts: no padding, no non-canonical trailing bits.
2. Constant-time MAC check, done **before** decoding the JSON.
3. Decode the JSON.
4. `e > now`, and `e ≤ now + 3600 + 60` (60 s of clock skew).
5. Resolve the key by ID. It must exist, not be revoked or expired, and have `listen`.
6. Reject a narrowing that references a merged-away system in any array, `exclude_talkgroups` included (§3.2).

Any failure is 401 `invalid_ticket`.

**Minting:** `POST /api/v1/tickets` with a key that has `listen` (via `listen`, `edit` or `admin`). The route is `KeyRequired`: anonymous callers get 401 `key_required` and upload-only keys get 403 `insufficient_scope`. The body is optional: `{"ttl_seconds": 600, "restriction": {...}}`.
- `ttl_seconds` is clamped to 60–3600; the default is 600.
- `restriction` is the requested **narrowing**. It is validated like any restriction, but may hold **at most 100 entries in total**, and the encoded ticket must be ≤ 2048 bytes. Anything larger is 400 `invalid_body` "ticket restriction too large — narrow by system or mint several tickets".
- The payload stores only the narrowing. The key's current restriction is applied at every verification, so a key PATCH takes effect on outstanding tickets.
- Response: `200 {"ticket": "trt_...", "expires_at": "..."}`.

**Use:**
- `?ticket=` is honoured only on the ticket-enabled routes: `GET /api/v1/events/stream`, `GET /api/v1/audio/live` and `GET /api/v1/calls/{id}/audio`, plus `HEAD` on the same paths.
- Tickets are **not single-use**. Audio elements issue repeated Range requests, so a ticket stays valid until it expires.
- An SSE or WebSocket connection authenticated by a ticket is **closed when the ticket expires**, with the `ticket_expired` signal (§7.5). The client mints a new ticket and reconnects; SSE resumes gaplessly via `last_event_id`. This bounds what a leaked ticket can do to its lifetime, and lets a multi-user backend cut one user off by not issuing another ticket.
- A multi-user backend can't revoke one individual ticket. It can stop minting, wait up to the TTL, or revoke the whole key.

**Secret:** 32 random bytes, generated on first start and stored in `auth_settings` (`ticket_secret`). It is not configurable. Deleting the row and restarting rotates it and invalidates every outstanding ticket.

### 3.6 Anonymous access policy

```json
{ "access": "off", "restriction": null }
{ "access": "listen", "restriction": { "allow_all": true, "exclude_talkgroups": ["1:5001", "1:5002"] } }
```

- `access` is `off` (the default on a fresh install) or `listen`. It can never be anything else: anonymous callers can never edit, administer or upload.
- `restriction` is optional (§3.2). It is stored even when `access` is `off`, so an operator can prepare it in advance.
- The policy lives in the database (`auth_settings.anonymous_access`). It is changed with `PUT /api/v1/anonymous-access` (admin) or `tr-engine access set` (CLI). There is **no environment variable** for it, so there is exactly one source of truth.
- A `PUT` must include both fields; a missing `restriction` key is 400, and an explicit `null` clears it. A CLI `access set` without restriction flags keeps the stored restriction, and `--no-restriction` clears it.
- Changes take effect for new requests at once through the API: `PUT` invalidates the cache and bumps the auth generation. Through the CLI they take effect within 30 s, when the cache expires. Open streams are re-checked (§7.5).
- `GET /whoami` shows every caller only `{access, restricted: bool}`. The lists themselves, and in particular `exclude_talkgroups`, which names the talkgroups the operator considers sensitive, are available only through the admin `GET /anonymous-access`.

## 4. HTTP API

All paths are under `/api/v1` unless shown otherwise. Error bodies keep the existing shape `{"code","error","detail?"}`. Every `/api/v1` JSON response carries `Cache-Control: no-store` and `Vary: Authorization`. Call audio carries `Cache-Control: private`.

### 4.1 `GET /whoami` (public)

Tells any client what its credential can do.
- It is not ticket-enabled.
- With an invalid key it returns 401 `invalid_key`, so a client learns the key is bad.
- Under policy `off` an anonymous caller still gets 200 with `scopes: []`.

```json
{
  "credential": "key",                         // "key" | "anonymous"
  "key": {                                      // null for anonymous
    "id": 7, "name": "tr-dashboard", "prefix": "tre_1a2b3c4d",
    "scopes": ["edit"], "restriction": null, "expires_at": null, "legacy": false
  },
  "scopes": ["edit", "listen"],                 // effective, implications expanded, sorted
  "restricted": false,                           // true if any restriction applies to this caller
  "anonymous": { "access": "listen", "restricted": false },
  "version": "1.0.0"
}
```

For an anonymous caller, `restricted` reflects the policy, but the lists are never shown. For a key caller, `key.restriction` is the key's own restriction.

`whoami` replaces `/auth-init` and `/auth/me`.

### 4.2 Keys (`admin`)

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/keys?include_revoked=false` | — | `200 {"keys": [APIKey...], "total": n}`, ordered by id |
| POST | `/keys` | `{name, scopes, restriction?, expires_at?, rate_limit_rps?}` | `201`: APIKey + `"key": "tre_..."` (plaintext, shown once) |
| GET | `/keys/{id}` | — | `200 APIKey`, or 404 |
| PATCH | `/keys/{id}` | any of `{name, scopes, restriction, expires_at, rate_limit_rps}` | `200 APIKey`; 409 if the key is revoked or the last-admin guard applies; 400 on validation errors |
| DELETE | `/keys/{id}` | — | `204`: revokes the key (sets `revoked_at`); idempotent; 409 if the last-admin guard applies |

**PATCH semantics:**
- An **absent** field is left unchanged; an explicit `null` clears `restriction`, `expires_at` or `rate_limit_rps`. The implementation must tell the two apart (decode into `map[string]json.RawMessage` or an optional-field type).
- To change scopes away from `["listen"]` on a key that has a restriction, clear the restriction in the same request; otherwise the PATCH is 400.
- A PATCH or DELETE invalidates the key's cache entry and bumps the auth generation.

Validation errors are 400 `invalid_body` or `invalid_parameter`, with a message naming the field.

### 4.3 Anonymous access (`admin`)

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/anonymous-access` | — | `200 {"access","restriction","updated_at"}` |
| PUT | `/anonymous-access` | `{"access": "off"\|"listen", "restriction": Restriction\|null}` (both keys required) | `200`, same shape |

### 4.4 Tickets (key with `listen`)

`POST /tickets`: see §3.5.

### 4.5 Audit log (`admin`)

`GET /admin/audit-log?limit=50&offset=0&key_id=&since=&until=` returns `200 {"entries": [...], "total": n}`, newest first. Each entry is `{id, time, key_id, key_name, actor, method, path, status, request_id}`. See §9.

### 4.6 Removed endpoints

These are gone and answer 404 `not_found`:
- `/auth-init`
- `/auth/login`, `/auth/refresh`, `/auth/logout`
- `/auth/setup` (GET and POST)
- `/auth/me`
- `/auth/keys` and all its sub-paths
- `/users` and all its sub-paths

The `?token=` query parameter is no longer read anywhere.

### 4.7 Error codes and status codes

New codes, added to `internal/api/responses.go` and to the OpenAPI `Error.code` enum:

| Status | Code | When |
|---|---|---|
| 401 | `key_required` | No credential, and the anonymous policy doesn't allow this: the policy is `off`, the route needs more than `listen`, or the route is `KeyRequired`. |
| 401 | `invalid_key` | The presented key is unknown, revoked or expired. The message says "revoked" or "expired" when known. |
| 401 | `invalid_ticket` | The ticket is malformed, has a bad MAC, has expired, is orphaned (its key is gone, revoked, expired or lacks listen), or references a merged system. |
| 403 | `insufficient_scope` | A valid credential without the scope the route needs. The message names the needed scope. |
| 403 | `restricted_credential` | The credential is restricted to some systems or talkgroups, and this endpoint can't enforce that. |
| 404 | `not_found` | Also returned when a restricted credential asks for a specific resource outside its restriction, so that existence isn't revealed. |
| 503 | `service_unavailable` | The credential lookup failed (DB error or timeout). Never cached, never treated as anonymous. |

Every 401 carries `WWW-Authenticate: Bearer realm="tr-engine"`. The existing codes `unauthorized` and `forbidden` stay in the enum, but the auth layer emits `forbidden` only for "matched route has no policy".

## 5. Transport rules

- **Keys:** only in `Authorization: Bearer <key>` (§3.3). Keys in the query string are ignored everywhere. On `POST /api/v1/call-upload` without a Bearer header, the upload middleware reads the multipart fields `key` and then `api_key`. It reads them from the parsed multipart body only, never from `FormValue` or the URL. It does this after the route's `MaxBodySize(50 MB)`. A present but invalid value is 401 `invalid_key`, with no fallthrough to the other field.
- **Tickets:** only in `?ticket=`, only on ticket-enabled routes, only for GET and HEAD.
- **CORS:**
  - Every response gets `Access-Control-Allow-Origin: *`.
  - `OPTIONS` is answered with 204 **before** any auth or routing, with:
    - `Access-Control-Allow-Methods: GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS`
    - `Access-Control-Allow-Headers: Authorization, Content-Type, Last-Event-ID, X-Actor, X-Request-ID`
    - `Access-Control-Max-Age: 600`
  - Responses expose `X-Request-ID, Retry-After, WWW-Authenticate` through `Access-Control-Expose-Headers`.
  - `Access-Control-Allow-Credentials` is never sent, no cookies are ever set, and `CORS_ORIGINS` is removed.
- **WebSocket** `/audio/live`: any `Origin` is accepted. Auth is by header or ticket, and there are no ambient credentials.
- **SSE `Last-Event-ID`:** accepted from the `Last-Event-ID` header, or from a `last_event_id` query parameter (the header wins). The parameter exists because a client that re-creates an `EventSource` with a fresh ticket can't set the header.
- **HEAD:** the root router uses chi's `middleware.GetHead`, so a HEAD request is served by the GET handler, and policy lookup uses the GET route (§6).
- **`X-Request-ID`:** a client value is kept only if it is at most 64 characters of `[A-Za-z0-9._-]`; otherwise a new one is generated. This applies globally.
- **Header size:** the server sets `http.Server.MaxHeaderBytes` to 64 KiB for the request line plus headers. net/http reads up to `MaxHeaderBytes` + 4 KiB of slack before it gives up, so the effective limit is about 68 KiB; a larger request gets `431 Request Header Fields Too Large` before any middleware runs. Clients should stay under 64 KiB.

## 6. Route policy and the request pipeline

### 6.1 Policy table

A single table in `internal/api/policy.go` maps every route (`"METHOD /full/pattern"`, written exactly as `chi.Mux.Find` returns it, including the `/api/v1` prefix) to a policy:

```go
type RoutePolicy struct {
    Scope       auth.Scope // "" = public; otherwise listen, edit, admin, upload
    Restricted  Mode       // Deny (zero value) or Enforced: the handler applies the principal's restrictions
    Ticket      bool       // ?ticket= honoured (GET/HEAD only)
    KeyRequired bool       // anonymous principals never pass, whatever the anonymous policy says
    FormKey     bool       // upload: key may come from multipart fields; checked by the upload middleware
}
```

Routes are registered **flat**, not with `r.Route(...).Get("/")`, so that `chi.Walk` and `Find` agree on the pattern strings. Static files are registered with `r.Get("/*", ...)` only; there is no `r.Handle`.

### 6.2 Pipeline

This is the root router's middleware order:

1. **RequestID** (sanitized, §5)
2. **CORS**: sets headers; `OPTIONS` → 204 and stop
3. **GetHead** (§5), then **Logger**, then **Recoverer** inside it (so a panic is logged through the request's logger with its stack, method and path, and the access line records the 500; `http.ErrAbortHandler` is re-raised), then **APIHeaders** (`Cache-Control: no-store`, `Vary: Authorization` on `/api/v1`)
4. **Match**:
   - `path := r.URL.RawPath`, or `r.URL.Path` if RawPath is empty. This is exactly what chi routes on.
   - `pattern := root.Find(chi.NewRouteContext(), method, path)`, where HEAD falls back to GET when there is no HEAD route. Always use a *fresh* route context; `Find` mutates the context it is given.
   - Store `(pattern, policy)` in the request context.
   - `pattern == ""`: chi will answer 404 or 405 without running any handler. Skip steps 5–7 (no principal, no log) and let the router reply. This is still fail-closed, because no handler runs.
   - Pattern found but not in the table: 403 `forbidden` plus an ERROR log "route has no auth policy".
5. **Resolve principal** (§3.3): on routes with `policy.Ticket` and method GET/HEAD, a `?ticket=` is resolved first and wins over the `Authorization` header; otherwise the Bearer header; otherwise anonymous. Rate limiting is interleaved with this step (§8).
6. **Authorize**:
   - Public: pass.
   - `FormKey` and no header principal: pass to the upload middleware, which decides.
   - Anonymous and (`KeyRequired`, or policy `off`, or `Scope` > listen): 401 `key_required`.
   - Principal lacks `Scope`: 403 `insufficient_scope`.
   - Principal restricted and `Restricted == Deny`: 403 `restricted_credential`.
   - Otherwise: pass.
7. **Audit** (§9), wrapping only non-GET/HEAD requests.
8. Route-group middleware (MaxBodySize, ResponseTimeout, the upload middleware), then the handler.

Audit sits **outside** ResponseTimeout, so it records the status the client actually received.

**Construction for tests.** Router construction moves into `buildRouter(opts) *chi.Mux`. Given stub dependencies, it registers every conditional route (metrics, upload, live audio, jitter). Prometheus collector registration moves out of router construction (into `NewServer`, or onto a private registry), so the router can be built more than once in tests.

**Required tests:**
- `chi.Walk(buildRouter(allFeatures))`: every route has a table entry, and every table entry is a real route (no stale entries).
- For every route, a synthesized path's `Find` result equals the Walk pattern.
- Every `{param}` route with a `%2F` value in its parameter gets the same policy decision as the route chi actually dispatches to.
- `GET /api/v1/auth-init` → 404; `HEAD /api/v1/health` → 200.
- A check that `openapi.yaml` and the table agree (§6.4).

### 6.3 The table

**Public** (no credential needed; the anonymous policy doesn't matter):
- `GET /api/v1/health`
- `GET /api/v1/whoami`
- `GET /api/v1/openapi.yaml`
- `GET /api/v1/pages`
- `GET /favicon.ico`
- `GET /*` (static files)

`/health` returns only `status`, `version` and each check's `status`. The full body is returned only to a `key` principal with unrestricted `listen` or better; it contains the TR instance list, pool stats, the stream listen address and the update check.

**`listen`, Restricted = Enforced** (the handler applies the restriction):

| Route (`/api/v1` prefix omitted) | Enforcement |
|---|---|
| `GET /systems` | Only systems visible to the restriction. |
| `GET /systems/{id}` | 404 unless the system is visible. |
| `GET /sites/{id}` | 404 unless its system is visible. |
| `GET /talkgroups` | Restriction clause in the shared WHERE (count + rows). |
| `GET /talkgroups/{id}` | Resolving a plain (ambiguous) id considers only allowed talkgroups, so the 409 body never lists forbidden systems. 404 unless allowed. |
| `GET /talkgroups/{id}/calls` | As above, then the restricted `ListCalls`. |
| `GET /talkgroup-directory` | Restriction clause in the shared WHERE. |
| `GET /calls` | Restriction clause inside `database.ListCalls` (which also serves the talkgroup and unit call lists). `patched_tgids` is filtered to allowed talkgroups. |
| `GET /calls/active` | Filtered in the loop, always, not only when user filters are present. |
| `GET /calls/{id}` | 404 unless allowed; `patched_tgids` filtered. |
| `GET /calls/{id}/audio` (Ticket) | `GetCallAccess` first; 404 unless allowed. |
| `GET /calls/{id}/frequencies` | `GetCallAccess` first; 404 unless allowed. |
| `GET /calls/{id}/transmissions` | Same. |
| `GET /calls/{id}/transcription` | Same. |
| `GET /calls/{id}/transcriptions` | Same. |
| `GET /call-groups` | Restriction clause in the shared WHERE. |
| `GET /call-groups/{id}` | 404 unless the group's (system, tgid) is allowed; member calls are also filtered defensively. |
| `GET /transcriptions/search` | Restriction clause on the joined `calls`. |
| `GET /transcriptions/batch` | Join `calls`; disallowed call IDs are silently omitted. |
| `GET /events/stream` (Ticket) | Per-type scope and restricted matcher (§7.3). |
| `GET /audio/live` (Ticket) | Server-side frame check (§7.4). |
| `POST /tickets` (KeyRequired) | The ticket carries only the requested narrowing; the key's restriction is applied when the ticket is verified. |

**`listen`, Restricted = Deny:**
- Systems and talkgroups: `GET /p25-systems`, `GET /talkgroups/encryption-stats`, `GET /talkgroups/{id}/units`
- Units: `GET /units`, `GET /units/{id}`, `GET /units/{id}/calls`, `GET /units/{id}/events`, `GET /unit-events`, `GET /unit-affiliations`, `GET /unit-tag-suggestions`, `GET /unit-tag-suggestions/{id}`
- Stats: `GET /stats`, `GET /stats/rates`, `GET /stats/talkgroup-activity`, `GET /stats/call-volume`, `GET /stats/daily-overview`, `GET /stats/category-breakdown`, `GET /stats/call-heatmap`
- Analytics: `GET /analytics/recorder-utilization`, `GET /analytics/decode-rates`
- Other: `GET /trunking-messages`, `GET /recorders`, `GET /transcriptions/queue`, `GET /audio/jitter`
- **`GET /metrics`** is at the root, not under `/api/v1`, and exists only when `METRICS_ENABLED`. It is also KeyRequired, so a Prometheus scrape key needs only `listen`, and `/metrics` is never public.

These routes are denied because their data either has no talkgroup dimension, or leaks other talkgroups' activity through aggregates and `last_event_*` fields. Supporting restricted credentials on them is future work (`docs/roadmap.md`).

**`edit`:**
- `PATCH /talkgroups/{id}`, `PATCH /units/{id}`
- `PUT /calls/{id}/transcription`, `POST /calls/{id}/transcribe`
- `POST /calls/{id}/transcription/verify`, `POST /calls/{id}/transcription/reject`, `POST /calls/{id}/transcription/exclude`
- `POST /unit-tag-suggestions/{id}/approve`, `POST /unit-tag-suggestions/{id}/dismiss`

**`admin`:**
- System and data management: `GET /console-messages`, `PATCH /systems/{id}`, `PATCH /sites/{id}`, `POST /talkgroup-directory/import`, `POST /unit-tags/import`, `POST /admin/systems/merge`
- Maintenance: `GET /admin/maintenance`, `POST /admin/maintenance`, `PUT /admin/maintenance/config`, `DELETE /admin/maintenance/config/{key}`
- Backfill: `POST /admin/transcribe-backfill`, `GET /admin/transcribe-backfill`, `DELETE /admin/transcribe-backfill`, `DELETE /admin/transcribe-backfill/{id}`
- Storage: `GET /admin/storage/stats`, `POST /admin/storage/purge/{table}`
- Other: `POST /query`, `POST /pages`, `POST /debug-report`
- Keys: `GET /keys`, `POST /keys`, `GET /keys/{id}`, `PATCH /keys/{id}`, `DELETE /keys/{id}`
- Anonymous access and audit: `GET /anonymous-access`, `PUT /anonymous-access`, `GET /admin/audit-log`

**`upload` (FormKey):** `POST /call-upload`.

A restricted principal can never reach an `edit`, `admin` or `upload` route: only `["listen"]` keys, tickets and the anonymous policy can carry restrictions.

**Changes from today:** these move from "editor" or "any reader" to `admin`: system merge, `GET /admin/maintenance`, `GET /admin/transcribe-backfill`, the CSV imports, `POST /query`, `POST /pages`, and system/site PATCH. `POST /debug-report` stops being unauthenticated and needs `admin`. `GET /metrics` needs a `listen` key. `GET /console-messages` becomes `admin`.

### 6.4 Machine-readable policy in OpenAPI

So that clients don't have to hard-code §6.3, `openapi.yaml` gets:
- Per-operation `security`:
  - public operations: `security: []`;
  - the ticket routes: both `bearerAuth` and a new `ticketAuth` scheme (`apiKey` in query, name `ticket`);
  - upload: `bearerAuth` plus a description of the form fields.
- On every operation, `x-scope: public|listen|edit|admin|upload` and `x-restricted: enforced|deny`; `x-key-required: true` where applicable.
- A per-type scope and restricted-visibility description on the `SSEEventType` enum (§7.3).

A Go test (under `internal/api`, reading the embedded spec) asserts that every table entry has a matching operation with the same `x-scope`/`x-restricted`, and vice versa.

## 7. Enforcement

### 7.1 Shared helpers (`internal/auth`)

A new package with no dependencies on other tr-engine packages, so `database`, `ingest`, `audio` and `api` can all import it:

- `Scope`, `Scopes`: validation, implication, `Has`, normalization and sorting.
- `TG{SystemID, Tgid}`: `ParseTG("1:9178")`, `String()`, and JSON as the composite string.
- `Restriction{AllowAll bool; Systems []int; Talkgroups []TG; ExcludeTalkgroups []TG}`:
  - `Normalize()` dedupes and sorts, and never returns nil for a non-nil input.
  - `Validate(kind)`: kind is key, anonymous or ticket, applying the "allows nothing" rules of §3.2.
  - `AllowsNothing()`, `AllowsTG(sys, tg int) bool`, `SystemVisible(sys int) bool`, and `RewriteSystem(from, to int)` for merges.
- `Principal{Kind; KeyID int; KeyName string; Scopes Scopes; Restrictions []Restriction; Actor string; TicketExpiry time.Time}`:
  - `Has(scope)`, `Restricted()`, and `AllowsTG`/`SystemVisible`, which require every restriction to agree.
  - `Internal`, a package-level unrestricted principal for internal callers.
- `(*Principal) SQL(sysCol, tgCol string, firstArg int) (clause string, args []any)`:
  - Returns `""` for unrestricted principals.
  - Otherwise returns `" AND (...)"` with positional args starting at `$firstArg`. For every restriction it emits `tgCol > 0 AND sysCol > 0` (so NULL, zero and negative talkgroup or system IDs are never visible to a restricted principal), then:
    - allow part:
      - `allow_all`: nothing.
      - Allow-nothing: `FALSE`.
      - Otherwise `(sysCol = ANY($a::int[]) OR (sysCol, tgCol) IN (SELECT s, t FROM unnest($b::int[], $c::int[]) AS x(s, t)))`, dropping whichever side is empty.
    - exclude part (only when non-empty): `AND NOT EXISTS (SELECT 1 FROM unnest($d::int[], $e::int[]) AS x(s, t) WHERE x.s = sysCol AND x.t = tgCol)`.
  - Array args are always non-nil `[]int32` slices. **pgx encodes a nil slice as SQL NULL**, which is why the existing `pqIntArray` helpers (empty → NULL → "no filter") must never be used for restrictions.
- Tickets: `SignTicket(secret, payload)` and `VerifyTicket(secret, token, now) (payload, error)`, following the §3.5 rules.
- The auth generation: an `atomic.Uint64` with `Generation()` and `Bump()`. It is bumped by key PATCH/DELETE, anonymous-policy PUT and system merges. Caches and long-lived connections watch it.

Everything above is table-tested. The SQL helper is also tested against a real PostgreSQL in `internal/database`, gated on `TEST_DATABASE_URL`, with a fresh database per test as `internal/unittags` does. The cases include:
- NULL, 0 and negative tgids, and 0 and negative system IDs;
- allow_all + exclude, systems-only, talkgroups-only, and allow-nothing;
- multiple restrictions, and an empty intersection;
- a test that removing the last allowed entry never widens access.

### 7.2 Database queries

Every query function behind an Enforced route takes the principal as a **required positional parameter**, e.g. `ListCalls(ctx, p *auth.Principal, f CallFilter)`:
- A nil `p` returns an error, and internal callers pass `auth.Internal`.
- The restriction clause is ANDed into the **shared** WHERE, so counts and rows agree.
- User-supplied filters stay separate and ANDed. Never intersect them with the restriction and pass the result as the user filter: an empty intersection would become NULL, which means unfiltered.
- Placeholder numbering must be recomputed wherever a query hard-codes `$n`. `ListCalls` uses `LIMIT $11 OFFSET $12` and `SearchTranscriptions` uses `$8/$9`.

Single-resource routes call a new hand-written `GetCallAccess(ctx, callID) (systemID, tgid int, err error)` in `internal/database/calls.go` before doing anything else. It uses pgx directly: the sqlc-generated files are not hand-edited, and sqlc isn't required.

Each Enforced list route gets a DB integration test with seeded data. It asserts that a restricted principal's rows **and totals** contain only allowed (system, tgid) pairs, and covers the empty-intersection case.

### 7.3 SSE

**Per-type minimum scope** (applies to every principal):

| Event type | Needs | Restricted principals |
|---|---|---|
| `console` | `admin` | never (admin is never restricted) |
| `call_start`, `call_update`, `call_end`, `transcription` | `listen` | only if `SystemID != 0`, `Tgid != 0` and `AllowsTG` |
| `unit_event` | `listen` | only if `SystemID != 0`, `Tgid != 0` and `AllowsTG` (so on/off/registration events are dropped) |
| `recorder_update`, `rate_update`, `trunking_message` | `listen` | dropped |
| any other or future type | `admin` until classified | dropped |

The map sits next to the route table in `policy.go`, is tested, and is documented in the `SSEEventType` OpenAPI description. `matchesFilter` applies it before the client's own filters. `Publish` and replay both go through `matchesFilter`.

The principal is stored on the **subscriber** at subscribe time, separately from the client's `EventFilter`, and is swappable atomically (§7.5).

**Replay without a gap.** A new `EventBus.SubscribeSince(lastID, filter, principal) (replay []SSEEvent, ch <-chan SSEEvent, cancel func())` is exposed through `LiveDataSource`. It registers the subscriber and snapshots the ring under the same lock that `Publish` holds while adding to the ring and distributing. Live events are deduplicated by numeric sequence (only those greater than the highest replayed sequence are sent). The channel buffer is `max(64, len(replay)+64)`. This replaces the current replay-then-subscribe sequence, which loses events published in between.

### 7.4 Live audio WebSocket

The principal is stored on the audio subscriber at `Subscribe` time, separately from the client-updatable `AudioFilter`. `UpdateAudioFilter` never touches it. For a restricted subscriber, `matchesAudioFilter` also requires `AllowsTG(frame.SystemID, frame.TGID)`, whatever the client's subscribe message says.

**Connection limits.** The server sends a keepalive message and a WebSocket ping every 15 s (the keepalive's `active_streams` counts only allowed talkgroups for a restricted principal). A connection from which nothing arrives for 60 s, pongs included, is closed. A client message over 64 KiB closes the connection with `1009`.

**Attribution.** Clamping by restriction is only as good as the `SystemID` on each frame, which the audio router derives from the simplestream sender. A sender mapped by `STREAM_SOURCE_MAP` (`ip=instance_id` pairs) uses that instance. Otherwise the instance is worked out again for every chunk from the instances that have the packet's short name: real trunk-recorder instances are preferred over the `WATCH_INSTANCE_ID` (file watch / `TR_DIR`) identity, which is preferred over the `UPLOAD_INSTANCE_ID` identity, and instances the source map gives to other senders are left out (only source-map entries do that). When the preferred instances map the short name to different systems, including when such an instance appears after the sender was attributed, the router drops the sender's audio with a WARN (at most every 5 minutes) instead of guessing; `STREAM_SOURCE_MAP` is then required.

### 7.5 Long-lived connections: re-check and close signals

Each SSE and WebSocket connection re-resolves its principal **every 60 s**, **immediately when the auth generation changes** (also when it changed while the connection was starting), and **when its key's `expires_at` passes** (a timer armed from the key as last read, for key and ticket connections, and re-armed after every re-check):
- key: re-read by ID, bypassing the cache;
- ticket: the key re-read plus the ticket's narrowing, and the ticket's expiry;
- anonymous: the current policy.

Outcomes:
- If the new principal no longer has `listen` (key revoked, expired or re-scoped; anonymous policy now `off`), the connection is closed with a signal.
- If a ticket connection's ticket has expired, it is closed with `ticket_expired`. A separate timer closes it at the ticket's expiry, whatever the re-checks find. A ticket whose narrowing names a system that has since been merged into another (§3.2) also closes its connection with `ticket_expired`, since a new ticket minted for the merged system fixes it.
- If scopes or restrictions changed but `listen` remains, the subscriber's principal is swapped atomically; no reconnect is needed.
- If the re-check can't reach the database (the key, ticket or policy lookup fails), the connection stays open with its current principal, a WARN is logged, and the next re-check (the 60 s tick or a generation change) tries again. The expiry timer is not re-armed for a time that has already passed, so a failing lookup is not retried in a loop.

Close signals, so clients can tell auth loss from a network blip:
- **SSE:** send `event: auth` with `data: {"code":"invalid_key"|"key_required"|"insufficient_scope"|"ticket_expired"}`, then end the response.
- **WebSocket:** close with code `4401` (`invalid_key`, `key_required`, `ticket_expired`) or `4403` (`insufficient_scope`), with the code string as the reason.

On `ticket_expired`, clients mint a new ticket and reconnect, SSE with `last_event_id`. On any other code they **must not** auto-reconnect; they re-run `whoami` and show the key screen or an explanation.

API changes reach open streams at once. CLI changes, which happen in another process and can't bump the in-process generation, reach them within 60 s, because the periodic re-check reads the database directly.

## 8. Rate limiting and the key cache

**Order of operations:**
1. **No credential:** per-IP limiter (`RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`, IP from `TrustedProxies.ClientIP`).
2. **Bearer credential found in the positive cache:** key principal. If the key has `rate_limit_rps`, apply the per-key limiter (burst = 2 × rps, at least 1); otherwise no limit. Legacy keys (`legacy = true`) are always IP-limited instead: they may have been effectively public.
3. **Bearer credential not in the positive cache:** take a per-IP token first; if none is left, return 429 without touching the database. Then look the key up: first in the negative cache, otherwise in the database with a 2 s timeout.
   - Found: cache it, then continue as in step 2.
   - Not found, revoked or expired: negative-cache it, then 401.
   - Lookup error: 503, not cached, and never treated as anonymous.
4. **Ticket:** verifying the MAC and expiry costs no database access; resolving the key goes through the cache as above. Ticket requests are **per-IP limited, like anonymous requests**, and are not charged to the key's per-key limit. Tickets live in many browsers, each with its own IP.
5. **Upload with the key in a form field** (no Bearer header): the request is first resolved as having no credential (step 1, one per-IP token); the upload middleware then resolves the form key as in steps 2–3. That costs a second per-IP token when the key isn't in the positive cache, and on every request for a legacy key, so such uploads get half the per-IP rate.

**Caches:**
- Positive and negative results live in **separate** bounded LRUs, so random guesses can't evict valid keys.
- The TTL is 30 s, and entries are keyed by SHA-256 hash; the positive cache is also indexed by key ID, for tickets.
- `expires_at` and `revoked_at` are checked against the cached record on every hit.
- API PATCH/DELETE invalidates the key's entries, and a generation bump clears both caches.
- `last_used_at` is written at most once per minute per key, asynchronously.

`AuthRateLimiter`, the old login limiter, is removed.

A 429 on `/events/stream` ends an `EventSource` for good. Clients must back off and reconnect themselves (the dashboard's SSE manager and the `auth.js` wrapper do this).

## 9. Audit log

The table is `audit_log`:

| Column | Type |
|---|---|
| `id` | bigserial primary key |
| `time` | timestamptz, default `now()` |
| `key_id` | int NOT NULL |
| `key_name` | text NOT NULL |
| `actor` | text NULL |
| `method` | text |
| `path` | text |
| `status` | int |
| `request_id` | text |

It has indexes on `(time DESC)` and `(key_id, time DESC)`.

**What is recorded:** the middleware runs after Authorize and records only requests that meet all of these:
- the principal is a **key**;
- the route matched;
- the method is not GET, HEAD or OPTIONS.

Denials the handler itself decides (401/403 from inside handlers) are included. The status recorded is the one the client received.

**What is skipped:** anonymous and failed-auth attempts; these stay in the access log. `POST /api/v1/call-upload` and `POST /api/v1/tickets` are also skipped, as high-volume routes with no state change worth auditing.

**Mechanics:**
- The path is stored without its query string, and capped at 1024 bytes (cut at a UTF-8 boundary) with a "…(truncated, N bytes)" marker (`database.TruncateAuditPath`).
- The response writer is wrapped only for audited methods, using a wrapper with `Unwrap`, `Flush` and `Hijack`, like `metrics.statusWriter`.

**`X-Actor` request header:** optional free text naming the end user on whose behalf a multi-user client acted. It is trimmed, Unicode categories Cc and Cf are removed, and it is capped at 200 characters. It is recorded but never used for authorization.

**Other records of who did it:** `unit_tag_suggestions.decided_by` and `system_merge_log.performed_by` now store the key name, plus ` / <actor>` when present, instead of a username or the literal `"api"`.

**Retention:** `RETENTION_AUDIT_LOG` (default `8760h`, one year), purged by the daily maintenance run and shown in `GET /admin/maintenance`.

Admin UIs (web/admin.html and the tr-dashboard Access page) render `actor`, `key_name`, `path` and `request_id` as text only.

## 10. Bootstrap and CLI

### 10.1 Bootstrap admin key

On startup, after migrations and the legacy import, the engine claims `bootstrap-admin-key` in `data_fixups`, inside a transaction holding an advisory lock. The claim is made **once per database**. At that moment:
- If an active admin key already exists (for example an imported `WRITE_TOKEN`), no key is minted; the claim only records that.
- Otherwise it creates a key named `bootstrap admin` with scopes `["admin"]`.

The plaintext is printed **directly to stderr as plain text**, bypassing zerolog so it is readable and log shippers don't index it as a structured field:

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

The Docker line is shown only when `/.dockerenv` exists. A separate structured WARN log line records only `key_id` and `prefix`.

On every later start, if no active admin key exists (all revoked, or all expired), the engine does **not** mint one. It logs an ERROR with the recovery command, `tr-engine keys create --name "admin" --scopes admin`. Recovery always goes through the CLI, which means host access.

The tr-dashboard Access page and `web/admin.html` show a warning while a key named `bootstrap admin` is still active ("replace this key with a named one and revoke it").

### 10.2 CLI

New subcommands follow the `export`/`import` pattern in `cmd/tr-engine`: their own FlagSet, `--env-file`/`--database-url`, `config.Load`, `database.Connect`, `InitSchema` + `Migrate`. They differ in two ways: **logs go to stderr at WARN level**, and a `Migrate` failure is fatal (for `export`/`import` it is only a WARN).

All four commands (`keys`, `access`, `export`, `import`) name pending migrations on stderr before they run, and none of them applies the irreversible ones (`convert api_keys to app keys`, `record and drop users`, §11.1) on a database from before API keys unless `--migrate` is given, since an older engine still running on that database would stop working. Without `--migrate`:
- `keys` and `access` refuse such a database (they read and write the new auth tables), with a message naming the migrations and the two ways forward;
- `export` and `import` apply the other migrations with `database.MigrateReversible` and run, leaving the irreversible ones to the server's first start, with a note on stderr (they don't touch the auth tables). `import --dry-run` never applies them, and `import --dry-run --migrate` is refused.

The recommended order is to stop the old engine, back up the database with `pg_dump` (not `tr-engine export`, which doesn't carry the auth tables), and start the new server first, which also runs the legacy import. `keys list` and `access show` print a stderr note while that one-time import is still to come.

```
tr-engine keys list   [--all]
tr-engine keys create --name NAME --scopes listen|edit|admin[,upload] | upload
                      [--expires 90d|720h|2026-12-31|2026-12-31T00:00:00Z]
                      [--all-talkgroups] [--systems 1,2] [--talkgroups 1:9178,1:9179] [--exclude-talkgroups 1:5001]
                      [--rate-limit RPS]
tr-engine keys update ID|--prefix PREFIX [--name NAME] [--scopes ...] [--expires ...|--no-expiry]
                      [--rate-limit RPS|--no-rate-limit] [restriction flags|--no-restriction]
tr-engine keys revoke ID|--prefix PREFIX
tr-engine keys import --name NAME --scopes ...      (reads an existing secret from stdin; stores its hash, legacy=true)
tr-engine access show
tr-engine access set  --anonymous off|listen [--all-talkgroups] [--systems ...] [--talkgroups ...]
                      [--exclude-talkgroups ...] [--no-restriction]
tr-engine access forget-retired-token
```

**Arguments and output:**
- A numeric argument is always an ID; prefixes need `--prefix`.
- `keys create` and `keys import` print the plaintext key (create) or the new key's ID (import) on stdout, and nothing else, so scripts can capture it (`docker compose exec -T ...`). All messages go to stderr; `keys create` reports there the restriction as stored (after any merge rewrite).

**Restriction flags:**
- `--systems`, `--talkgroups` and `--exclude-talkgroups` take comma-separated values and may be repeated; repeats add up.
- `--systems ""` or `--talkgroups ""` means an *empty* allow list, not "absent".
- `--all-talkgroups` sets `allow_all`.
- `access set` without restriction flags keeps the stored restriction.

`keys import` enforces the legacy strength rules (§11.2). It also refuses the retired public token (§11.2), both while it is stored and after `access forget-retired-token` (whose hash is kept in `auth_settings` `forgotten_public_tokens` for this), and a secret that is already stored. `access forget-retired-token` refuses while an active key has the retired token's hash, naming that key.

The CLI is not subject to the last-admin guard; `keys revoke`/`update` warn when no active admin key is left or every remaining one expires within a day.

`--expires` accepts:
- a Go duration;
- a number with a `d` suffix (days);
- a date (`YYYY-MM-DD`, midnight UTC);
- an RFC3339 time.

## 11. Upgrade and legacy migration

### 11.1 Schema

Migrations are appended to `internal/database/migrations.go`. **`Migrate` evaluates every `check` before applying anything**, so no migration may rely on the effects of another migration pending in the same run. `Migrate` (and `InitSchema`) hold the session advisory lock `schemaLockKey` throughout, so a second process starting on the same database (another engine, or a CLI command run while the server starts) waits and then finds the migrations applied, instead of evaluating the same ones as pending and failing half-way.

**Which `users` table is tr-engine's.** The old "create users table" migration skipped creating the table when any `users` table already existed, and then used it, so a database shared with another application may hold that application's `users`. Both migrations below therefore act on `users` only when `trEngineUsersSQL` holds: the relation `users` resolves to (unqualified, as the old engine resolved it) has the columns `username`, `role` and `enabled`, **and** either the trigger `trg_users_updated_at` or a CHECK constraint naming `'viewer'` and `'editor'`, both created with tr-engine's table. Any other `users` table is never read as tr-engine's user list and never dropped.

**Irreversible migrations.** The two migrations below are marked `irreversible`: they convert the old API keys and drop the old user accounts, which an older engine still running on the same database can't survive, and they can't be undone. The server applies them on its first start (`Migrate`). The CLI applies them only with `--migrate` (§10.2); without it, `keys`/`access` refuse such a database, and `export`/`import` apply the other migrations with `database.MigrateReversible` and leave these two to the server's first start.

- **Fresh databases** get the new tables from `schema.sql`: `api_keys` in its new shape, `auth_settings` and `audit_log`. The old "create api_keys table" migration is changed to create the new shape too; it only matters for databases that predate `api_keys` and were never initialized from the current `schema.sql`. The old "create users table" and "add display_name and last_login to users" migrations are **removed**, so `users` is never created again.
- **Migration: api_keys → app-key model.** Its check is: column `scopes` exists **and** column `label` does not. It is a single `DO $$ ... $$` block, and every statement that touches old columns goes through `EXECUTE`. That keeps it parseable on databases without `users` or without the old columns, and keeps it usable as the manual SQL that `MigrationError` prints. When `api_keys.label` exists, it:
  1. adds `scopes text[]`, `restriction jsonb`, `expires_at timestamptz`, `revoked_at timestamptz`, `rate_limit_rps real` and `legacy boolean NOT NULL DEFAULT false`;
  2. if `users` is tr-engine's (`trEngineUsersSQL`, above):
     - revokes (`revoked_at = now()`) keys whose owner has `enabled = false`;
     - computes a user-owned key's role as the **lower** of the key's role and the owner's current role (viewer < editor < admin);
     - appends ` (<username>)` or ` (<username>, disabled)` to the name;
  3. maps role to scopes: `viewer` → `{listen}`, `editor` → `{edit}`, `admin` → `{admin}`, and adds `upload` for `is_service_account` keys. Service keys were the documented upload credential; other keys lose the upload ability they only had through a bug;
  4. renames `label` to `name`, filling empty names with `'key ' || key_prefix`;
  5. sets `scopes NOT NULL` and adds `CHECK (cardinality(scopes) > 0)`;
  6. drops `user_id`, `role`, `is_service_account` and `idx_api_keys_user_id`.
- **Migration: record and drop users.** Its check: `users` is not tr-engine's (`NOT trEngineUsersSQL`: it doesn't exist, or it is another application's, which is left alone). It is also a DO block that returns at once unless `trEngineUsersSQL` holds. It first inserts into `data_fixups` (name `removed-user-accounts`, `ON CONFLICT DO NOTHING`) a JSON array of every user (`username, role, enabled, last_login`), then runs `DROP TABLE users CASCADE`. At startup, the engine logs a one-time summary from that row: "removed 3 user accounts: alice (admin), bob (editor), carol (viewer, disabled) — give each person or their client an API key".
- `auth_settings` (`name text PRIMARY KEY, value jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()`) and `audit_log` also get `IF NOT EXISTS` migrations, for databases created before this version.

**Rollback is not supported** other than by restoring a backup. The migration guide's first step is a database backup, with the Docker command (`docker compose exec postgres pg_dump -U trengine trengine > pre-apikey.sql`).

**Required DB integration tests:**
- a fresh database;
- a database from before users and api_keys existed;
- a current-version database with users (enabled and disabled; admin, editor and viewer), user-owned and service keys;
- a database whose `users` table belongs to another application (never read or dropped);
- running `Migrate` twice.

### 11.2 Legacy environment variables

The old variables are read **only** to migrate and to warn; they no longer configure anything.

**One-time import.** On the first start of the new version, the engine claims `import-legacy-auth` in `data_fixups` (once per database) and follows this procedure. The `detail` column records the derived old mode and every decision, and one summary line is logged.

1. **Derive the old effective configuration** exactly as the old `config.Load` did. `AUTH_ENABLED=false` clears every credential (old mode: open). `JWT_SECRET` without `ADMIN_PASSWORD` is dropped. The old mode is then:
   - `open`: none of `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_PASSWORD` set;
   - `write-token-only`: only `WRITE_TOKEN` set;
   - `token`: `AUTH_TOKEN` set, `ADMIN_PASSWORD` not set;
   - `full`: `ADMIN_PASSWORD` set.
2. **Fresh database** (the schema was created by this version): import nothing, and leave the anonymous policy `off`. The per-variable warnings still apply. `schema.sql` itself inserts the `data_fixups` row `schema-created-with-api-key-auth` (only when `systems` is empty), so this holds whichever process ran it: the server, `tr-engine import`/`export`/`keys`/`access`, or `psql -f schema.sql`. Migrations never insert it, so a database upgraded from an older version doesn't carry it.
3. **`WRITE_TOKEN`** is imported as the key `legacy WRITE_TOKEN`, scopes `["admin","upload"]`. **Exception:** in `full` mode, if `WRITE_TOKEN` equals `AUTH_TOKEN`, nothing is imported and an ERROR is logged: "WRITE_TOKEN was published by /auth-init as the public read token; not imported — create a new admin key". The bootstrap key is then printed.
4. **`AUTH_TOKEN` in `token` mode** is imported as the key `legacy AUTH_TOKEN`, scopes `["listen"]`, plus `upload` only if `WRITE_TOKEN` is unset (the old upload auth preferred `WRITE_TOKEN`). If it equals `WRITE_TOKEN`, it was already imported in step 3 and is not duplicated.
5. **`AUTH_TOKEN` in `full` mode** is not imported, because it was public. Its SHA-256 is stored as `auth_settings.retired_public_token` (§3.3).
6. **Anonymous policy** is set to `listen` if the old mode is `open` and the `systems` table is not empty (an upgraded open instance stays readable), or if the old mode is `full` with `AUTH_TOKEN` set (the public demo). Otherwise it stays `off`. When it stays `off` and the old mode was `write-token-only`, an INFO line names `tr-engine access set --anonymous listen` for operators who want public reads.
7. **Strength rules for imported values.** A value containing `$(` (an unexpanded shell substitution copied from old docs) is **refused**, with an ERROR naming the variable. A value shorter than 16 characters is imported, with a WARN on every start: "legacy key #N is weak (N characters) — replace it". An imported value whose hash already exists is not duplicated.
8. **Uploads.** If calls from `UPLOAD_INSTANCE_ID` exist in the last 7 days and no active key has `upload` after the import, the engine logs a WARN: "HTTP uploads were in use but no key can upload now — create one with `tr-engine keys create --name 'uploads' --scopes upload` and configure it in trunk-recorder". It also lists migrated keys without `upload` that were used in the last 7 days.

**Every start.** Each old variable that is still set produces one WARN line:
- For `AUTH_TOKEN` and `WRITE_TOKEN`, the WARN says what this process's value is now (the import's record is used only when it describes this value):
  - the key the import recorded: "AUTH_TOKEN is no longer used (imported as API key #3 'legacy AUTH_TOKEN' on 2026-09-26) — remove it from your configuration; see docs/migrating-auth.md". When that key is revoked or expired, "; that key is revoked (expired), so clients sending it get 401 invalid_key" is added inside the parentheses;
  - another stored key (for example one registered with `tr-engine keys import` after a rotation): "its value is API key #N", with the same note;
  - the retired public token while it is still stored as retired: "treated as anonymous". After `access forget-retired-token`, the WARN says that requests carrying it get 401 `invalid_key`;
  - a value that matches no stored key and no retired or forgotten public token: the value was never imported (clients sending it get 401 `invalid_key`), with a suggestion to register it with `tr-engine keys import`.
- For the others: "ADMIN_PASSWORD is no longer used — tr-engine has no user accounts; clients use API keys. See docs/auth.md".
- `CORS_ORIGINS`: "no longer needed: the API allows all origins and never uses cookies".

**Rejected uploads.** When the upload route rejects a request, the engine logs a WARN at most once a minute per client IP, with the IP, the form's `system`/`shortName`, and the reason: no key, unknown key, or key #N lacks `upload`.

### 11.3 What operators must do

`docs/migrating-auth.md` covers each step in detail:

1. **Back up the database.**
2. **Upgrade the engine, tr-dashboard and any bind-mounted `web/` together**, with pinned versions. When copying the new `web/` into a bind-mounted directory, keep any pages you saved yourself.
3. **Start the new version once with the old `.env` unchanged**, so the import can read it.
4. **Copy the bootstrap key** from `docker compose logs tr-engine` (stderr) if one was printed.
5. **Remove any reverse-proxy block that injects `Authorization`** (the Caddy block in `caddy/Caddyfile` and on gerty; the nginx and Caddy examples in the old tr-dashboard README). The engine tolerates the old public token (logging a WARN at most hourly) and empty bearers (silently), but any other injected value makes every anonymous request 401. Operators check the proxy config itself, since an empty injection never logs.
6. Check `tr-engine keys list`, and **give every client its own named key**: dashboards, scripts, each trunk-recorder upload host (`upload` scope), and Prometheus (`listen`). Revoke the legacy keys and the bootstrap key afterwards.
7. **Set the anonymous policy deliberately** (`access show`/`access set`).
8. **Remove the old variables** from `.env`.
9. If the import missed a secret that is still in use, **register it** with `tr-engine keys import`.

## 12. Clients

### 12.1 tr-dashboard (single-user app + public viewer)

**State.** `useAuthStore` becomes:
- `{ apiKey, whoami, status: 'loading' | 'ready' | 'needs-key' | 'invalid-key' | 'engine-too-old' | 'error', error }`;
- `apiKey` is persisted in localStorage; the store version moves to 3;
- the migration from v2 turns a stored `writeToken` into a *candidate* `apiKey`, kept only if `/whoami` accepts it, since it may be an imported legacy token;
- helpers: `hasScope(scope)`, `canEdit()`, `isAdmin()`, `restricted`.

**Startup** (`AuthGate`, which replaces `RequireAuth`) calls `GET /whoami` with the stored key:
- 401 `invalid_key`: `invalid-key`. The key screen explains that the key was rejected, and offers "continue without a key" when `whoami.anonymous.access` is `listen`.
- 404, or a 401 with any other code: `engine-too-old`, shown as "This tr-engine doesn't support API keys yet — upgrade tr-engine". Never the key screen.
- No key and anonymous access `off`: `needs-key`.
- Otherwise `ready`. Anonymous visitors browse read-only.

**Key entry:**
- A full-page "Connect to tr-engine" form: paste a key, validate it with `/whoami`, store it.
- A key whose effective scopes lack `listen` is rejected with "this key can only upload; use a listen, edit or admin key".
- An "API key" card in Settings shows name, prefix and scopes, with replace and forget.
- There is no login page and no users page.

**Requests:**
- `request()` sends `Authorization: Bearer <key>` when a key is stored, and nothing otherwise. There is no refresh logic.
- 401 `invalid_key`: re-run `whoami` and show the key screen.
- 403 `insufficient_scope`/`restricted_credential`: a clear message, e.g. "Your key can't do this (needs edit)".

**Gating** is decided on `whoami.scopes` and `whoami.restricted`:
- Edit buttons (talkgroup and unit edit, unit-tag suggestions) require `edit`. The Access page and the Admin page require `admin`.
- When `restricted`:
  - API functions for Deny endpoints short-circuit with a typed "unavailable" result instead of calling the engine;
  - polling of Deny endpoints stops;
  - the Units, Affiliations, Recorders and unit-suggestion nav items are hidden;
  - unit results are left out of the command palette and GoTo menu.
- `Promise.all` sites that mix Enforced and Deny calls are converted to `Promise.allSettled`, or split, so the Enforced data still renders: `Dashboard.tsx` (stats/recorders with calls), `SystemDetail.tsx`, `TalkgroupAnalytics.tsx`, `CommandPalette.tsx` and `GoToMenu.tsx`.

**SSE:**
- `SSEManager` subscribes to `apiKey` and reconnects whenever the key is set, replaced or forgotten.
- With a key, it mints a ticket right before every (re)connect.
- On `error` it closes and reconnects with a fresh ticket and `last_event_id`, with backoff.
- On `event: auth`, it reconnects only for `ticket_expired`; otherwise it stops and re-runs `whoami`.
- The `console` subscription is kept; the engine only delivers it to admin keys.

**Audio:**
- `AudioPlayer` builds the URL from `API_BASE` + `/calls/{id}/audio`, not from the root-relative `audio_url`, which breaks with an absolute `VITE_API_BASE`. With a key, it appends a ticket right before setting `src`.
- The ticket helper `getTicket({minRemaining})` returns the cached ticket if at least `minRemaining` is left, otherwise it mints a new one. Media uses `minRemaining = 5 min`; SSE uses 60 s.
- On a media `error`, the player mints a new ticket once and restores `currentTime`.
- Queued items never carry tickets.

**Access page** (`/access`, admin only):
- keys list, create, edit and revoke; the plaintext is shown once with a copy button, next to a warning box: "keys used in web pages that other people load are public";
- a bootstrap-key warning;
- the anonymous policy editor, with `allow_all` + exclude lists and system/talkgroup pickers; it can't save an allow-nothing policy;
- the recent audit log, rendered as text.

**Removed:**
- `Login.tsx`, `Users.tsx`, the `/login` and `/users` routes;
- the `login`/`refresh`/`logout`/`setup` client functions and the users API;
- the `readToken`/`writeToken`/`accessToken`/`jwtEnabled`/`user` state and the guest state;
- `credentials: 'include'`.

**Dev proxy:** `TR_AUTH_TOKEN` becomes `TR_API_KEY`, injected only when the browser request has no `Authorization` header. The dev server listens on `0.0.0.0`, so the key is added only to same-origin requests from a loopback client (`mayAddDevKey`: not other devices on the network, nor other sites open in the developer's browser, checked with `Sec-Fetch-Site`/`Origin`), and `vite preview` never adds it. `TR_API_KEY` should be a `listen` key, never the bootstrap admin key. The README and `examples/` lose every proxy-injection example.

**Types:** regenerate `src/api/generated.ts` from the new `openapi.yaml`.

**Smoke check:** `scripts/smoke-auth-audio.mjs` is updated to the ticket model, or removed.

### 12.2 Engine demo pages (`web/`)

**`auth.js`** is rewritten around one stored key, `localStorage['tr-engine-api-key']`, with every storage access in try/catch. The old entries `tr-engine-token`, `tr-engine-write-token` and `tr-engine-jwt` are removed on load. There is no synchronous XHR. `whoami` is fetched asynchronously, and the `fetch` patch waits for it.

**`fetch` patch:**
- It applies to **same-origin `/api/` URLs only**. It adds `Authorization` when a key is stored and the caller didn't set one, and preserves a `Request` object's own headers.
- It **prompts only** on 401 `invalid_key`, or on 401 `key_required` when no key is stored and `whoami.anonymous.access` is `off`. The prompt is a paste-key modal, followed by one retry.
- Every other 401/403 on a GET is passed through untouched.
- For mutations, 403 `insufficient_scope` shows an explanatory modal with a "use a different key" button.

**`EventSource` replacement:**
- It is a wrapper that re-attaches listeners, mirrors `readyState` and `onopen`/`onmessage`/`onerror`, and supports `addEventListener`, `close` and the static constants.
- **For same-origin `/api/` URLs with a stored key**, it mints a ticket (async), connects with `?ticket=`, and handles `event: auth` (§7.5). On error, it mints a new ticket and reconnects with `last_event_id` and backoff.
- Cross-origin URLs (pages opened with `?api=`) and key-less use get the native `EventSource`, unchanged.

**Public API, `window.trAuth`:**
- `ready()`: resolves after `whoami`, and after the first ticket when a key is stored.
- `getKey()`, `setKey(k)`, `clearKey()`, `whoami()` (cached), `hasScope(s)`, `showKeyPrompt()`.
- `mediaUrl(url)`: synchronous. For same-origin API URLs with a stored key, it appends the cached ticket, but never one with less than 60 s left; in that case it returns the plain URL and triggers a refresh. The cache is refreshed when less than 5 min remain, and on `visibilitychange`.
- `ticketUrl(url)`: async, same-origin only.
- Deprecated shims that `console.warn`: `getToken()`/`getWriteToken()` return `''`, `hasWriteAccess()` → `hasScope('edit')`, `showPrompt()` → `showKeyPrompt()`, `getMode()` → `'key'|'anonymous'`, `logout()` → `clearKey()`. These keep pages built from old templates, or saved with `POST /pages`, working.

**Pages:**
- **`admin.html`:** key management, the anonymous policy, the audit log and the bootstrap warning replace the users/setup/login UI. The admin key is entered **on this page and kept in `sessionStorage`** for the tab only, and sent explicitly (the `fetch` patch doesn't override an explicit header). docs/auth.md advises browsing the other demo pages with a listen or edit key.
- **`storage.html`:** gated on the admin scope via `whoami`, with the same session-only admin key.
- **The five `<audio>` sites** (`call-history`, `talkgroup-research`, `irc-radio-live`, `scanner`, `scanner-classic`): use `trAuth.mediaUrl(url)`, and retry once on media `error` with `await trAuth.ticketUrl(url)`. Tickets are only attached to same-origin URLs, which fixes the "token sent to any `?api=` origin" bug.
- **`audio-engine.js`:** connects with `await trAuth.ticketUrl(...)` for same-origin paths, and handles close codes 4401/4403.
- **HTML escaping:** every page renders API-provided strings with `textContent` or the shared escape helper; known raw `innerHTML` sinks are in `unit-tracker.html` and `emergency-log.html`. This matters because an `edit` key can set a talkgroup or unit alpha tag to HTML, which would run on the origin where keys are stored.
- **`Promise.all` sites that mix Enforced and Deny calls** (`systems-overview.html`, `irc-radio-live.html`) become `allSettled`. `signal-flow-data.js` skips `/query` unless `trAuth.hasScope('admin')`, and degrades on 401/403.
- **Other page fixes:**
  - `units.html` calls `trAuth.showKeyPrompt()`; the old `showAuth()` didn't exist.
  - `emergency-log.html` uses `audio_url`; it read a field the API never returns.
  - `irc-radio-live.html` drops the captured Cloudflare beacon script, which was third-party JS on the origin that stores the key.
  - `playground.html`'s generated-page instructions describe keys and tickets.
  - `theme-engine.js` adds an "API key…" item to the nav menu when `trAuth` is present.
- All pages bump to `auth.js?v=3`.

## 13. Configuration summary

| Variable | Status |
|---|---|
| `AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS` | **Removed.** Read only for the one-time import and to warn (§11.2). |
| `TRUSTED_PROXIES` | Kept (default `loopback,private`). |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | Kept. They now apply to anonymous, ticket, legacy-key and key-cache-miss requests, and to uploads with the key in a form field (§8). |
| `RETENTION_AUDIT_LOG` | New; default `8760h`. |
| `STREAM_SOURCE_MAP` | New; comma-separated `ip=instance_id` pairs attributing simplestream senders to trunk-recorder instances. Needed when instances share a short name for different systems: that audio is otherwise dropped with a WARN, since live-audio restrictions depend on attribution (§7.4). |

The Docker Compose files drop the `TR_AUTH_TOKEN` variable passed to tr-dashboard. `caddy/Caddyfile` drops the token-injection block. `.githooks/pre-commit` adds a `tre_[0-9a-f]{64}` pattern and keeps the old ones.

## 14. Security properties (acceptance criteria)

1. Without a valid credential, no endpoint outside §6.3 "Public" returns data. The only exception is `listen` routes when the anonymous policy is `listen`, and then never KeyRequired routes.
2. The anonymous policy can't grant more than `listen`, can't see `console` events, and can't be configured to allow nothing.
3. **Revocation and changes take effect:**
   - An API revoke, expiry, scope change or restriction change takes effect on the next request. A CLI change takes effect within 30 s.
   - Open SSE/WebSocket connections pick up API changes at once, and CLI changes within 60 s. They close with a signal, or swap the principal when `listen` remains.
   - A ticket connection is closed when its ticket expires.
4. **URLs:**
   - No key is accepted from a URL.
   - Tickets are accepted only on the three ticket routes, only for GET/HEAD, and grant only `listen`.
   - A ticket never outlives 1 hour, and neither does a connection it opened.
5. **Restrictions:**
   - A restricted principal gets 403 `restricted_credential` on every non-Enforced route.
   - On Enforced routes it never receives rows, totals, events or audio frames outside its restriction, and never `patched_tgids` or 409 ambiguity details for disallowed talkgroups.
   - An empty allow list never widens access.
   - System merges never leave an allow entry naming a merged-away system and never void an exclusion (exclusions keep the old ID next to the surviving one, symmetric along chains of merges). (They can widen allow lists to the whole merged system, §3.2.)
   - SSE event IDs are opaque (the publish sequence encrypted per process), so a restricted subscriber can't count the events it wasn't shown. Known limitation: call IDs are one sequence across all talkgroups, so gaps in the `call_id`s a restricted credential sees reveal how much activity happened outside its restriction, though never what it was.
6. **No cookies:** `Access-Control-Allow-Credentials` never appears, and no cookies are set.
7. **Route coverage:** every registered route has a policy (tested), and a matched route without a policy fails closed. Unmatched routes get 404/405 with no handler run.
8. **Uploads:** upload requires a key with `upload`; nothing else can upload. Form-field keys are read only from the multipart body, after the size limit.
9. **Admin-only routes:** `/debug-report`, `/query`, `POST /pages`, merges, maintenance, storage, CSV imports, system/site identity edits and `console` events require `admin`. `/metrics` requires a key with `listen`.
10. **Legacy imports:** no legacy value that was ever public (the full-mode `AUTH_TOKEN`, including a `WRITE_TOKEN` equal to it) is imported as a key. No character of a legacy secret is stored in plaintext.
11. **Guessing keys** costs one per-IP rate-limit token per attempt and can't evict valid keys from the cache.
12. The login rate limiter no longer exists. Per-IP limits use `TRUSTED_PROXIES` (already shipped in `98b5d2b`).

## 15. Bugs fixed alongside

These were found while mapping the code for this design. They are fixed in the same change because they are auth-adjacent or in code the change touches:
- `ListTalkgroupUnits` joined `units` on `unit_id` only. It returned same-numbered units from other systems (a cross-system leak) and wrong totals.
- `POST /admin/storage/purge/{table}` accepted a zero or negative `older_than`, which deleted every row, or dropped every `mqtt_raw_messages` partition including the current one. Its error text suggested `7d`, which Go durations reject. It now requires a duration of at least 1h, and accepts a `d` suffix.
- The SSE replay-before-subscribe gap (§7.3).
- `PUT /calls/{id}/transcription` with an invalid `source` returned 500 (the DB CHECK) instead of 400.
- `POST /calls/{id}/transcribe` for a nonexistent call returned 503 instead of 404.
- `GET /talkgroup-directory` and `GET /recorders` returned `null` instead of `[]` when empty.
- `GET /calls/{id}/frequencies` and `/transmissions` ignored pagination-parameter errors.
- `summary.talkgroup_counts` in `GET /unit-affiliations` was keyed by tgid only, merging systems. It is now keyed `system_id:tgid`.
- **debug-report** sent the unsanitized trunk-recorder `config.json` (upload API keys, MQTT credentials). Redaction now:
  - matches key names case-insensitively on `key|token|pass|secret|auth|credential`;
  - strips userinfo and query strings from every URL-valued string, at any depth.

  It is tested against a realistic TR config (rdioscanner/openmhz `apiKey`, broadcastify, MQTT plugin), and the endpoint requires `admin`.
- web:
  - the token was appended to cross-origin `?api=` audio URLs;
  - `auth.js` dropped `Request` headers;
  - token-mode users could never enter a token, because the login form was shown instead;
  - `units.html` called an undefined `showAuth()`;
  - `emergency-log.html` read a non-existent field;
  - unescaped `innerHTML` sinks.
- tr-dashboard:
  - `/login` spun forever on direct load;
  - token mode had no credential for SSE or audio;
  - `canWrite()` over-reported in token mode;
  - audio URLs ignored an absolute `VITE_API_BASE`;
  - URL credentials were frozen at enqueue time;
  - Deny and Enforced calls shared `Promise.all`.

**Found but out of scope** (tracked in `docs/roadmap.md`):
- `GetTalkgroupByComposite`/`GetUnitByComposite` don't exclude soft-deleted systems (sqlc queries).
- `PATCH /sites/{id}` doesn't invalidate the ingest identity cache.
- `POST /query` runs as the application DB role, which can `pg_read_file` if that role is privileged. It is now admin-only, and the docs recommend a dedicated read-only role.
- CDN scripts in `web/` have no SRI.
- `audio-diagnostics.html` posts to a hard-coded external URL.
- Restriction support for the units, stats and recorder endpoints.

## 16. Implementation outline

Each step leaves the tree building, with its tests passing, and fail-closed.

1. **`internal/auth` package + tests.**
2. **Database and API core in one step:**
   - migrations and `schema.sql`;
   - `api_keys.go` rewrite; `auth_settings` (anonymous policy, ticket secret, retired public token); `audit_log`; legacy import; bootstrap claim; restriction rewrite on merge;
   - delete `database/users.go`;
   - `buildRouter` refactor; Match/resolve/authorize/audit pipeline; policy table with **every listen route Restricted = Deny** at this point; CORS; rate limiting and caches;
   - endpoints `whoami`, keys, anonymous-access, tickets, audit-log; upload middleware;
   - delete `auth.go`, `setup.go`, `users.go` and the old middleware and tests;
   - config cleanup; `main.go` (legacy import and warnings, bootstrap banner, `keys`/`access` CLI); drop `golang-jwt` (`go mod tidy`).
3. **Restriction enforcement, one route family at a time.** Each family switches its table entries to Enforced in the same change as its enforcement and its integration test. Also: SSE per-type scopes, `SubscribeSince`, WebSocket, audio, the long-lived connection re-check and close signals, and the §15 engine bug fixes.
4. **`web/` demo pages.**
5. **Docs:**
   - `openapi.yaml`: new endpoints, removed endpoints, per-operation security and `x-scope`/`x-restricted`, `ticketAuth`, error codes, the `SSEEventType` per-type scopes, `audio_url` semantics ("a path on the engine origin; never contains a credential; resolve against the engine base URL and append `?ticket=` when needed"), and the fixed `call_end` description;
   - new `docs/auth.md` (operator guide + client-developer guide);
   - rewritten `docs/migrating-auth.md`;
   - `docs/http-upload.md`, `docs/docker.md` (first run: where the bootstrap key is, `docker compose exec -T tr-engine tr-engine keys ...`), `docs/docker-full-stack.md`, `docs/docker-external-mqtt.md`, `docs/getting-started.md`, `docs/binary-releases.md` (verify steps use `/health` or a key header), `docs/architecture.md`, `docs/building-pages.md` (templates use the key header and tickets), `docs/debug-reports.md`, `docs/quality-gates.md`, `docs/roadmap.md`;
   - README, CLAUDE.md and AGENTS.md (including the Deployed Instance and Caddy sections), `sample.env`, compose files, `caddy/Caddyfile`, `install.sh`, `.githooks/pre-commit`, `web/*.md`.
6. **tr-dashboard** (§12.1): regenerated types; README, AGENTS.md, CLAUDE.md, CHANGELOG.md, `.env.example`, `examples/`, and the docs under `docs/` that describe auth.
7. **Verification:**
   - unit tests and DB integration tests (`TEST_DATABASE_URL`);
   - the route-policy and openapi-agreement tests;
   - an end-to-end run: the engine against a scratch PostgreSQL, and a curl matrix over every route × principal kind (anonymous off/listen/restricted, listen/edit/admin/upload/restricted/legacy/revoked/expired keys, tickets, retired public token);
   - legacy-import scenarios built from old-shape databases;
   - Playwright on tr-dashboard and on every `web/` page, with anonymous access off, listen and restricted: no modal and no uncaught error for visitors, and working key entry, SSE, audio and key management;
   - an adversarial security review.
