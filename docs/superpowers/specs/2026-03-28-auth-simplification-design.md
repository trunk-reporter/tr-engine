# Auth Simplification Design

> **Superseded by [2026-09-26-api-key-auth-design.md](2026-09-26-api-key-auth-design.md).** tr-engine now uses API keys, an anonymous access policy and tickets; the modes, tokens, logins and users described here no longer exist. Kept as a historical record. Current docs: `docs/auth.md`, `docs/migrating-auth.md`.

**Date:** 2026-03-28
**Status:** Approved
**Supersedes:** 2026-03-24-auth-js-consolidation-design.md (partially — auth.js changes are subsumed by this)

## Problem

The current auth system has 5 overlapping mechanisms (AUTH_TOKEN, WRITE_TOKEN, JWT, API keys, Caddy proxy injection) that create configuration confusion for users deploying tr-engine + tr-dashboard. The dashboard silently fails when Caddy isn't injecting the read token, and users see "not connecting" with no clear error.

## Goals

1. Dashboard handles all auth client-side — no proxy injection required
2. Single clear config surface: `AUTH_TOKEN` for simple/public, `ADMIN_PASSWORD` for multi-user
3. Remove `WRITE_TOKEN` (replaced by JWT roles)
4. Remove auto-generated `AUTH_TOKEN` (explicit config only)
5. Maintain backward compatibility with a deprecation path
6. Log loud startup warning when running in open mode (no auth)

## Auth Modes

Three modes, determined by engine configuration:

| Config | Mode | Behavior |
|--------|------|----------|
| No `AUTH_TOKEN`, no `ADMIN_PASSWORD` | **open** | No auth required. All requests pass through. |
| `AUTH_TOKEN` set, no `ADMIN_PASSWORD` | **token** | Single shared token required. User enters it in dashboard, stored in localStorage. |
| `ADMIN_PASSWORD` set | **full** | JWT login for write access. If `AUTH_TOKEN` also set, it serves as a public read token (handed out by auth-init). If not set, login required for everything. |

## auth-init Endpoint Changes

### Current response (broken)
```json
{
  "token": "abc123",
  "guest_access": true,
  "user": { "username": "admin", "role": "admin" }
}
```

### New response
```json
{
  "mode": "open" | "token" | "full",
  "read_token": "abc123" | null,
  "jwt_enabled": true
}
```

Field semantics:
- `mode` — which auth mode the engine is running in
- `read_token` — present only in `full` mode when `AUTH_TOKEN` is set (public read access). Never present in `token` mode (user must provide their own). Never present in `open` mode (not needed).
- `jwt_enabled` — true when `ADMIN_PASSWORD` is set and JWT login endpoints are available

The `user` field is removed from auth-init. Authenticated user info comes from `/api/v1/auth/me` after login.

## Engine Changes (tr-engine)

### auth-init handler (`server.go`)

Rewrite to return new response shape:

```go
// IMPORTANT: auth-init must always be registered (currently gated on AuthToken != "").
// Move outside the AuthToken != "" conditional so all modes are discoverable.
r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
    resp := map[string]any{
        "jwt_enabled": opts.Config.AdminPassword != "",
    }
    switch {
    case opts.Config.AuthToken == "" && opts.Config.AdminPassword == "":
        resp["mode"] = "open"
    case opts.Config.AuthToken != "" && opts.Config.AdminPassword == "":
        resp["mode"] = "token"
        // Do NOT include read_token — user must enter it themselves
    default:
        // AdminPassword is set — full mode (JWT login available)
        resp["mode"] = "full"
        if opts.Config.AuthToken != "" {
            resp["read_token"] = opts.Config.AuthToken
        }
    }
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Cache-Control", "no-store")
    b, _ := json.Marshal(resp)
    w.Write(b)
})
```

**Required change:** auth-init must be unconditionally registered (remove the `if opts.Config.AuthToken != ""` gate). The dashboard depends on it to detect all three modes including `open`.

### Remove auto-generated AUTH_TOKEN

Currently in `config.go`, if `AUTH_TOKEN` is empty and `AUTH_ENABLED` is not explicitly false, a random token is generated and logged. Remove this behavior. If `AUTH_TOKEN` is not set, there is no token auth.

**Open mode startup warning:** When neither `AUTH_TOKEN` nor `ADMIN_PASSWORD` is set, log a prominent warning at startup: `"WARNING: running in open mode — API is completely unprotected. Set AUTH_TOKEN or ADMIN_PASSWORD to enable authentication."` This prevents users from accidentally running unprotected.

### Deprecate WRITE_TOKEN

- If `WRITE_TOKEN` is set at startup, log a warning: `"WRITE_TOKEN is deprecated — use ADMIN_PASSWORD for write access control. WRITE_TOKEN will be ignored in a future release."`
- For backward compatibility, `WRITE_TOKEN` continues to work in `JWTOrTokenAuth` middleware during the deprecation period. No code removal yet.

### Deprecate AUTH_ENABLED

- `AUTH_ENABLED` was an implicit flag (true when either token is set). Remove it as a concept. Auth mode is derived from `AUTH_TOKEN` and `ADMIN_PASSWORD` presence.
- If `AUTH_ENABLED=false` is explicitly set in env, log a warning: `"AUTH_ENABLED is deprecated — remove AUTH_TOKEN and ADMIN_PASSWORD to disable auth."`
- **During deprecation:** preserve the existing clearing behavior (`AUTH_ENABLED=false` → clear `AuthToken` and `WriteToken`). This ensures old `.env` files with `AUTH_ENABLED=false` continue to result in open mode rather than accidentally enabling auth from leftover token values.

### JWTOrTokenAuth middleware

No changes needed. It already handles:
1. JWT tokens (two dots → parse as JWT)
2. API keys (`tre_` prefix → DB lookup)
3. Legacy tokens (constant-time compare against WRITE_TOKEN, AUTH_TOKEN)
4. No auth configured → pass through

### WriteAuth middleware

**This needs changes** (the current pass-through logic is unsafe in new modes).

Currently, `WriteAuth` short-circuits with `next.ServeHTTP()` when both `writeToken` and `authToken` are empty. In `full` mode without `AUTH_TOKEN`, this means writes pass through with no check — a security hole.

Fix: `WriteAuth` should also consider whether JWT auth is configured. New logic:
1. If `WRITE_TOKEN` is set (deprecated path) and token matches → allow (admin role)
2. If `AUTH_TOKEN` is set and token matches → reject writes (read-only token)
3. If JWT auth is configured → require editor+ role from JWT claims (already in context from `JWTOrTokenAuth`)
4. If no auth is configured at all → pass through (open mode)

The key change: instead of checking `writeToken == "" && authToken == ""` to detect "no auth," pass a `jwtEnabled bool` parameter so WriteAuth knows JWT is in play even when both legacy tokens are empty.

### UploadAuth middleware

**Gap:** Upload plugins (rdio-scanner, OpenMHz) send auth via `key`/`api_key` form fields, not JWT. With `WRITE_TOKEN` deprecated, uploads need an alternative auth path.

Fix: Extend `UploadAuth` to also accept API keys (`tre_` prefix) from form fields. Flow:
1. Check `Authorization` header (JWT or legacy token) — existing behavior
2. Check `key`/`api_key` form fields — if value starts with `tre_`, resolve via DB
3. Check `key`/`api_key` form fields — constant-time compare against `AUTH_TOKEN` (for simple deployments in `token` mode where the shared token grants full access)
4. If `WRITE_TOKEN` is set (deprecated) — existing constant-time compare

This lets upload plugins use API keys created via the management endpoints, which is the intended long-term path.

## Dashboard Changes (tr-dashboard)

### RequireAuth.tsx

Rewrite the auth detection flow:

```typescript
// 1. Fetch auth-init to determine mode
const res = await fetch('/api/v1/auth-init')
const data = await res.json()

switch (data.mode) {
  case 'open':
    // No auth needed
    setAuthMode('open')
    break

  case 'token':
    // Check localStorage for saved token
    const saved = localStorage.getItem('tr-api-token')
    if (saved) {
      useAuthStore.getState().setApiToken(saved)
      setAuthMode('token')
    } else {
      // Show token prompt
      setNeedsToken(true)
    }
    break

  case 'full':
    if (data.read_token) {
      // Public read access — store read token
      useAuthStore.getState().setApiToken(data.read_token)
      setAuthMode('guest-read')
    }
    // Try JWT refresh for authenticated session
    const result = await refreshAuth()
    if (result) {
      setAuth(result.access_token, result.user)
      setAuthMode('authenticated')
    } else if (!data.read_token) {
      // No public read, no JWT session — must login
      setAuthMode('login-required')
    }
    break
}
```

### Auth store changes

```typescript
interface AuthState {
  // JWT auth
  accessToken: string
  user: AuthUser | null
  isAuthenticated: boolean

  // API token (shared token or public read token)
  apiToken: string

  // Auth mode from engine
  authMode: 'open' | 'token' | 'guest-read' | 'authenticated' | 'login-required'
}
```

Remove `writeToken`. Add `apiToken` (replaces both the old read token and user-entered shared token).

Persistence: `apiToken` persists to localStorage only in `token` mode (user-entered). In `guest-read` mode it's fetched fresh from auth-init on each page load.

### request() function

Update token selection logic:

```typescript
// Priority: JWT > API token > nothing
if (accessToken) {
  headers['Authorization'] = `Bearer ${accessToken}`
} else if (apiToken) {
  headers['Authorization'] = `Bearer ${apiToken}`
}
// No more writeToken logic
```

### SSE connection (`eventsource.ts`)

`SSEManager.buildUrl()` currently only injects `accessToken`. Update to use the same priority:

```typescript
const { accessToken } = useAuthStore.getState()
const { apiToken } = useAuthStore.getState()
const token = accessToken || apiToken
if (token) params.set('token', token)
```

Also update the token-change subscription to watch `apiToken` in addition to `accessToken`.

### Token prompt UI

New component for `token` mode: simple input field where user pastes their API token. Validates by calling `/api/v1/health` with the token. On success, stores in localStorage. On failure, shows error.

### Login form

Existing login form works as-is for `full` mode. Add a visual indicator when user is in guest-read mode (e.g., "Viewing as guest — log in for full access").

## Embedded Web Pages (auth.js)

The existing `auth.js` that patches `window.fetch` can be simplified to match the same flow:

1. Fetch `auth-init`
2. If `mode: "open"` — do nothing
3. If `mode: "token"` — use lazy-prompt-on-401 pattern (current behavior: synchronous XHR blocks page load, so prompt cannot happen during init; instead, set `mode = 'none'` and let 401 interception trigger the localStorage prompt)
4. If `mode: "full"` with `read_token` — inject it into all requests
5. If `mode: "full"` without `read_token` — lazy-prompt-on-401 (same as token mode)

**Backward compat:** If auth-init response has no `mode` field (old engine), fall back to current behavior (use `token` field if present, probe login endpoint).

## Config Summary (After)

| Env Var | Purpose | Required |
|---------|---------|----------|
| `AUTH_TOKEN` | Shared API token for simple deployments OR public read token in full mode | No |
| `ADMIN_PASSWORD` | Seeds first admin user, enables JWT login | No |
| `JWT_SECRET` | Auto-generated when `ADMIN_PASSWORD` is set | No (auto) |
| ~~`WRITE_TOKEN`~~ | Deprecated, log warning if set | No |
| ~~`AUTH_ENABLED`~~ | Deprecated, derived from above | No |

## Migration Path

### For existing users

1. **`AUTH_TOKEN` only (no ADMIN_PASSWORD):** No change. Dashboard will now prompt for the token instead of relying on proxy injection.
2. **`AUTH_TOKEN` + `ADMIN_PASSWORD`:** No change in behavior. Public read works via auth-init read_token. JWT login for writes.
3. **`WRITE_TOKEN` users:** Log deprecation warning. Works during transition. Guide users to set `ADMIN_PASSWORD` and create user accounts instead.
4. **`AUTH_ENABLED=false`:** Log deprecation warning. Remove both token vars to disable auth.
5. **Caddy injection users:** The `@no_auth` / `request_header` injection block in Caddyfile is now unnecessary but **harmless to leave in place** during migration. The new dashboard ignores injected tokens (it fetches its own from auth-init). Removing the Caddy injection block is optional cleanup, not a required step.

### Version compatibility

Dashboard v-next detects the new auth-init shape by checking for the `mode` field. If `mode` is absent (old engine), falls back to legacy behavior (check `guest_access`, probe login endpoint).

## Security Notes

- **read_token in auth-init is intentionally public:** In `full` mode with `AUTH_TOKEN` set, auth-init returns the read token to unauthenticated callers. This is by design — `AUTH_TOKEN` in this configuration is a "public read key," not a secret. Anyone who can reach the API gets read access. This matches the current behavior.
- **Token mode keeps the token secret:** In `token` mode, auth-init does NOT return the token. The user must know it out-of-band.
- **API keys in open mode are inert:** When no auth is configured, `JWTOrTokenAuth` short-circuits before reaching the API key check. API keys still validate but aren't required. This is correct — open mode means no auth needed.

## What This Does NOT Change

- API key auth (`tre_` prefix) — unchanged
- JWT token format and claims — unchanged
- Auth endpoint paths (`/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/me`, `/auth/setup`) — unchanged
- `JWTOrTokenAuth` middleware — unchanged (still handles JWT, API keys, legacy tokens, and no-auth pass-through)
- CORS configuration — unchanged
- Rate limiting on auth endpoints — unchanged
