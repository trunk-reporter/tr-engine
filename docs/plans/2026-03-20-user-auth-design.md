# User Authentication & Authorization Design

**Date:** 2026-03-20
**Status:** Approved
**Contributor:** jodfie (implementation), LumenPrima (design review)

## Summary

Add user accounts, sessions, roles, and API key management to tr-engine. Evolves the existing two-tier token system (`AUTH_TOKEN`/`WRITE_TOKEN`) rather than replacing it. Supports built-in username/password auth and optional OAuth/OIDC providers. Designed for both self-hosted and hosted deployments.

## Goals

- Individual user accounts with role-based access control
- Session-based browser auth (DB-backed, not JWT)
- Named API keys with role inheritance, including service accounts for frontend apps and integrations
- Optional OAuth/OIDC (Google, GitHub, Discord) alongside built-in password auth
- Zero breaking changes to existing deployments
- Architect for future resource-level permissions without requiring a rewrite

## Non-Goals

- Multi-tenancy (multiple orgs sharing one instance)
- Resource-level permissions (per-system, per-talkgroup ACLs) — designed for but not implemented in v1
- Custom OIDC provider configuration (only pre-defined providers in v1)

## Data Model

### `users` Table

| Column | Type | Notes |
|--------|------|-------|
| id | serial PK | |
| email | text UNIQUE | login identifier |
| password_hash | text | bcrypt cost 12, nullable (OAuth-only users) |
| display_name | text | |
| role | text | `viewer`, `operator`, `admin` |
| oauth_provider | text | nullable — `google`, `github`, `discord`, etc. |
| oauth_id | text | nullable — provider's unique user ID |

Constraint: `UNIQUE (oauth_provider, oauth_id) WHERE oauth_provider IS NOT NULL` — prevents duplicate OAuth logins from creating multiple user rows.
| created_at | timestamptz | |
| last_login | timestamptz | |

### `sessions` Table

| Column | Type | Notes |
|--------|------|-------|
| id | serial PK | for admin references (revocation URLs) |
| token_hash | text UNIQUE | SHA-256 of session token (never store plaintext) |
| user_id | int FK → users | |
| created_at | timestamptz | |
| expires_at | timestamptz | default 30 days, sliding window |
| ip | text | for admin visibility |

### `api_keys` Table

| Column | Type | Notes |
|--------|------|-------|
| id | serial PK | |
| key_hash | text UNIQUE | SHA-256 of the key (plaintext shown once at creation) |
| key_prefix | text | first 8 chars for display (`tre_a1b2c3d4...`) |
| user_id | int FK → users | nullable — null for service accounts and legacy keys |
| role | text | permission level of this key |
| label | text | descriptive name ("community-dashboard", "my upload script") |
| is_service_account | bool | true = standalone identity, not tied to a user |
| created_at | timestamptz | |
| last_used_at | timestamptz | |

Service accounts are API keys that act as their own identity rather than inheriting from a user. Frontend apps, bots, and TR upload plugins each get a service account key with an appropriate role.

### Legacy Token Migration

- `AUTH_TOKEN`/`WRITE_TOKEN` env vars continue to work when no users exist (zero-config backward compatibility)
- Once users exist, legacy tokens still work but resolve to mapped roles (`AUTH_TOKEN` → viewer, `WRITE_TOKEN` → admin)
- `WRITE_TOKEN` maps to `admin` (not `operator`) to preserve existing behavior — today `WRITE_TOKEN` can reach all POST/PUT/PATCH/DELETE routes including admin actions like system merge and maintenance triggers
- Admins can create proper API keys to replace legacy tokens at their own pace

## Auth Flow

### Credential Resolution Pipeline

Every request goes through a unified middleware pipeline:

```
Request arrives
  |
Extract credential (priority order):
  1. Authorization: Bearer <token> header
  2. ?token= query param (SSE/EventSource fallback)
  3. Session cookie (tre_session=<token>)
  4. Form field key/api_key (upload compat)
  |
Resolve credential to identity:
  - Matches session token -> load user, get role
  - Matches API key hash -> get role (+ user if linked)
  - Matches legacy AUTH_TOKEN -> role=viewer
  - Matches legacy WRITE_TOKEN -> role=admin
  - No match -> 401
  - AUTH_ENABLED=false -> inject role=admin, auth_method="disabled" (all checks pass)
  |
Inject into request context:
  ctx.role = "viewer" | "operator" | "admin"
  ctx.user_id = nullable (nil for legacy tokens/service accounts)
  ctx.auth_method = "session" | "api_key" | "legacy_token"
  |
Route-level checks:
  - Read endpoints: role >= viewer
  - Write endpoints: role >= operator
  - Admin endpoints: role >= admin
```

### First-Run Experience

1. No users exist -> legacy token mode works exactly as today
2. First user created via `POST /api/v1/auth/setup` (one-time, only works when zero users exist) -> becomes admin
3. After that, only admins can create users via `POST /api/v1/auth/users`

### Auth Bootstrap (`/auth-init`) Evolution

Today: `{"token":"..."}`.
With users: `{"token":"..."}` (no session) or `{"token":"...", "user": {"email":"...", "role":"...", "display_name":"..."}}` (valid session). The `user` field is absent — not `null` — when no session exists, so existing clients that only parse `token` are unaffected. Web UI uses the presence of `user` to show login state.

## Role Hierarchy & Permissions

Three roles, strictly ordered: `admin > operator > viewer`.

| Action | viewer | operator | admin |
|--------|--------|----------|-------|
| Read calls, talkgroups, units, systems | yes | yes | yes |
| SSE event stream | yes | yes | yes |
| Live audio (WebSocket) | yes | yes | yes |
| Edit alpha_tags (PATCH talkgroups/units) | no | yes | yes |
| Upload calls | no | yes | yes |
| Merge systems | no | no | yes |
| Trigger maintenance | no | no | yes |
| Create/edit/delete users | no | no | yes |
| Create/revoke API keys (own) | no | yes | yes |
| Create/revoke API keys (any user) | no | no | yes |
| View active sessions | no | no | yes |
| POST /query (ad-hoc SQL) | no | yes | yes |

Roles map to integer levels (`viewer=1`, `operator=2`, `admin=3`). Route groups declare their minimum level. Permission checks are `if level >= required`.

### Future Granularity Path

When resource-level permissions are needed (e.g., "user X can only see system 3"), add a `permissions` JSONB column to `users` and `api_keys`. The middleware contract doesn't change — it gains an additional check after the role check passes. No rewrite needed.

## API Endpoints

All new routes under `/api/v1/auth/`.

### Unauthenticated

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/auth/setup` | Create first admin (only when 0 users exist) |
| POST | `/auth/login` | Email + password -> session cookie |
| POST | `/auth/logout` | Extract session token from cookie, delete that session, return 204 regardless of whether session existed (prevents session-existence oracle). `SameSite=Lax` cookie is the CSRF control |
| GET | `/auth/oauth/{provider}` | Redirect to OAuth flow |
| GET | `/auth/oauth/{provider}/callback` | OAuth callback -> session |

### Operator+ (manage own keys)

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/auth/keys` | List own API keys |
| POST | `/auth/keys` | Create API key (plaintext returned once). Key role capped at caller's own role — an operator cannot create an admin key |
| DELETE | `/auth/keys/{id}` | Revoke own API key |

### Admin only

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/auth/users` | List all users |
| POST | `/auth/users` | Create user (email, role, optional password) |
| PATCH | `/auth/users/{id}` | Update user role, display_name |
| DELETE | `/auth/users/{id}` | Delete user + sessions + keys |
| GET | `/auth/sessions` | List active sessions |
| DELETE | `/auth/sessions/{id}` | Revoke a specific session (by session ID, not token — tokens must never appear in URLs/logs) |
| GET | `/auth/keys/all` | List all API keys (all users + service accounts) |
| POST | `/auth/keys/service` | Create service account key |
| DELETE | `/auth/keys/{id}/any` | Revoke any API key |

### Updated Existing

| Method | Path | Change |
|--------|------|--------|
| GET | `/auth-init` | Add `user` field when session present |

## Security

- **Password storage:** bcrypt cost 12 via `golang.org/x/crypto/bcrypt`
- **Session tokens:** 32 bytes `crypto/rand`, hex-encoded. Stored as SHA-256 hash (same as API keys — a DB dump should not yield usable credentials). Cookie: `HttpOnly`, `SameSite=Lax`, `Secure` when HTTPS, `Path=/api/`
- **API key format:** `tre_` prefix + 32 random bytes hex. Stored as SHA-256 hash. Plaintext shown once at creation. `key_prefix` stores first 8 chars for admin display
- **Session expiry:** 30 days default (`SESSION_TTL` env var). Sliding window — expiry extended only when current expiry is within 7 days (avoids per-request DB writes for active sessions). Expired sessions cleaned by existing maintenance loop
- **Auth rate limiting:** `/auth/login` and `/auth/setup` limited to 5 attempts per IP per minute, separate from global `RATE_LIMIT_RPS`
- **OAuth CSRF:** State parameter via `crypto/rand`, stored in short-lived cookie, validated on callback
- **Self-destruct prevention:** Admin cannot delete or demote themselves if they are the last admin
- **Audit logging:** Auth events (login, logout, failed login, user/key CRUD) logged via zerolog at info level. Structured log entries, no new table

## Configuration

### New Environment Variables

| Var | Default | Purpose |
|-----|---------|---------|
| `SESSION_TTL` | `720h` (30 days) | Session expiry duration |
| `OAUTH_GOOGLE_CLIENT_ID` | — | Google OAuth (optional) |
| `OAUTH_GOOGLE_CLIENT_SECRET` | — | Google OAuth (optional) |
| `OAUTH_DISCORD_CLIENT_ID` | — | Discord OAuth (optional) |
| `OAUTH_DISCORD_CLIENT_SECRET` | — | Discord OAuth (optional) |
| `OAUTH_GITHUB_CLIENT_ID` | — | GitHub OAuth (optional) |
| `OAUTH_GITHUB_CLIENT_SECRET` | — | GitHub OAuth (optional) |

OAuth providers auto-enable when both client ID and secret are set. No master switch needed.

### Backward Compatibility

| Scenario | Behavior |
|----------|----------|
| No users, `AUTH_TOKEN` set | Exactly as today — legacy token mode |
| No users, no tokens | `AUTH_ENABLED=false` path, fully open |
| Users exist, legacy tokens set | Both work — `AUTH_TOKEN` maps to viewer, `WRITE_TOKEN` maps to admin |
| Users exist, legacy tokens removed | Clean user-only auth |

Zero breaking changes. Every existing `.env` and deployment works without modification.

### Schema Migration

Three new tables via `migrations.go` — idempotent `CREATE TABLE IF NOT EXISTS` with check queries. No changes to existing tables.

## Frontend Integration

This design does not force any frontend to use tr-engine's auth. Three integration paths:

1. **API keys** — frontend has its own auth, uses API keys to talk to tr-engine
2. **Shared OAuth** — frontend and tr-engine use the same OAuth provider, shared identity
3. **tr-engine as auth source** — frontend delegates login to tr-engine's `/auth/` endpoints

Service accounts formalize the machine-to-machine path (what `WRITE_TOKEN` is today).

## Implementation Notes

Key integration points with the existing codebase:

- **`UploadAuth` middleware** (`middleware.go`): currently checks a single static token. Must be adapted to query the DB for API key hash lookup while preserving form field extraction (`key`/`api_key`).
- **`/api/v1/auth-init`** (`server.go`): currently an inline static JSON response. Must be rewritten as a proper handler with DB access to read session cookies.
- **Route registration**: unauthenticated auth endpoints (`/auth/login`, `/auth/setup`, OAuth callbacks) must be registered outside the `r.Group` that applies `BearerAuth`/`WriteAuth` to avoid a bootstrap deadlock (requiring a token to obtain a token).
- **OAuth callback timeout**: `GET /auth/oauth/{provider}/callback` makes an outbound HTTP request to the provider's token endpoint. Its own HTTP client timeout must be less than `HTTP_WRITE_TIMEOUT` (default 30s) to avoid the `ResponseTimeout` middleware firing mid-callback.
- **`POST /query` access level**: assigned to `operator+`. This gives operators arbitrary read-only SQL access to all tables. This is intentional — the query runs in a `BEGIN READ ONLY` transaction with a 30s timeout, and operator-level access aligns with the existing `WRITE_TOKEN` behavior. Can be reconsidered for `admin`-only in the future if needed.
