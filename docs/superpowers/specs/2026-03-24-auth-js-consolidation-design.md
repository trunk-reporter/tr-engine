# auth.js Consolidation Design

## Problem

The web UI has inconsistent auth handling across pages. Each page that does write operations re-implements its own 403 handling, write-token prompting, and token storage. Some pages (units.html, irc-radio-live.html) are broken — they don't handle 403 at all or lose the write token on reload. Meanwhile, the JWT user auth system (login/roles/API keys) was added server-side but none of the embedded web pages know about it.

The server-side auth scheme is clean (legacy tokens + JWT + API keys, unified in `JWTOrTokenAuth` middleware). The mess is entirely in the client-side web layer.

## Approach

Centralize all auth logic in `auth.js`. Pages never handle auth errors themselves — they just call `fetch()` and auth.js transparently handles token injection, 403 prompting, and retry.

## Mode Detection

`auth.js` calls `/api/v1/auth-init` on load via synchronous XHR (same as today). The response determines the mode:

- **Legacy mode**: `{ "token": "abc123" }` — no `user` field
- **JWT mode**: `{ "token": "abc123", "user": { "username": "admin", "role": "admin" } }` — `user` present
- **No auth**: auth-init returns no token — mode is `'none'`, no headers injected

If `tr-engine-jwt` exists in localStorage from a previous login, auth.js sends it as a Bearer token in the auth-init XHR. If the server returns `user` info, the JWT is still valid and we're in JWT mode. If not, the JWT has expired — auth.js discards it and falls back to legacy mode. JWT refresh (via httpOnly cookie) is handled lazily on first 401, not at init time, to keep initialization synchronous and fast.

## Token Storage

Three localStorage keys, each with a single purpose:

| Key | Purpose | Set by |
|-----|---------|--------|
| `tr-engine-token` | Read token (legacy) | auth-init response |
| `tr-engine-write-token` | Write token (legacy only) | User prompt on 403 |
| `tr-engine-jwt` | JWT access token | Login form or token refresh |

**Mode logic on load:**
1. If `tr-engine-jwt` exists and auth-init confirms it (returns `user`) → JWT mode
2. If `tr-engine-jwt` exists but auth-init doesn't confirm → discard JWT, legacy mode
3. No JWT → legacy mode (read token from auth-init, write token from localStorage if set)
4. No auth-init response at all → mode `'none'`

## Fetch Patch Behavior

auth.js patches `window.fetch` (same as today, but smarter):

- **GET/HEAD/OPTIONS** → inject read token (legacy) or JWT
- **POST/PATCH/PUT/DELETE** → inject write token if available, else read token (legacy); or JWT (JWT mode)

In JWT mode, one token handles everything — auth.js never injects the legacy read token. This prevents a stale JWT from silently degrading to viewer access via the legacy token fallback in the server middleware.

**Token injection priority:**
1. If caller already set an `Authorization` header → don't override (same as today)
2. JWT mode → use JWT for all requests (never fall back to legacy read token)
3. Legacy mode, mutation → use write token if set, else read token
4. Legacy mode, read → use read token

**Request replay for retry:** The patched fetch captures `(input, init)` arguments. If `input` is a `Request` object, it is cloned via `input.clone()` before the first send so the body is available for replay on retry. If `input` is a string URL, replay uses `(input, init)` directly.

## 401/403 Handling

auth.js intercepts all fetch responses centrally:

### 401 (Unauthorized — no valid token)
- Legacy mode → show token prompt (single password field, "Enter API token")
- JWT mode, no JWT stored → show login form (username + password)
- JWT mode, expired JWT → attempt silent refresh via `POST /api/v1/auth/refresh` (httpOnly cookie); if refresh fails, show login form

### 403 (Forbidden — valid token, insufficient permissions)
- Legacy mode → show write-token prompt ("This action requires a write token")
- JWT mode → show message "Your account doesn't have write access" (informational, no input — role is server-determined, entering a different token won't help)

### Transparent Retry

After the user submits a token or logs in:
1. Store the new credential in the appropriate localStorage key
2. Re-fire the original failed fetch with the new credential (using captured input/init)
3. Return the retried response to the original caller's promise chain

The page's `.then()` chain never sees the 403. If the retry also fails, the error propagates normally.

**Debounce/serialization:** A single `pendingPrompt` promise is maintained. When a 401/403 triggers a prompt, if `pendingPrompt` is already active, subsequent handlers await the same promise instead of creating new prompts. When the user submits, all waiters get the new credential and retry.

### JWT Login Flow

When the login form is shown (401 in JWT mode):
1. User enters username + password
2. auth.js POSTs to `/api/v1/auth/login` with `{ username, password }`
3. Server returns `{ "access_token": "...", "user": { "id", "username", "role" } }` and sets httpOnly refresh cookie
4. auth.js stores `access_token` in `tr-engine-jwt`, caches role in memory
5. Retry the original failed fetch with the new JWT

## Prompt UI

auth.js owns a single modal element injected into the DOM. It renders differently based on mode and error type:

| Mode | 401 | 403 |
|------|-----|-----|
| Legacy | "Enter API token" (password field) | "Enter write token" (password field) |
| JWT (no session) | "Log in" (username + password) | N/A (can't get 403 without 401 first) |
| JWT (active session) | Silent refresh → login form if fails | "Insufficient permissions" (message) |

The modal replaces all page-level auth prompts. Pages that currently have their own auth prompt HTML/CSS/JS will have it removed.

## Page API

```js
window.trAuth.getToken()          // current effective token (read or JWT)
window.trAuth.getWriteToken()     // write token (legacy) or JWT
window.trAuth.setToken(t)         // set read token manually
window.trAuth.getMode()           // 'jwt' | 'legacy' | 'none'
window.trAuth.hasWriteAccess()    // true if JWT with editor+ role, or write token is set
window.trAuth.showPrompt()        // force-show the auth prompt (backward compat)
window.trAuth.logout()            // clear JWT + call POST /api/v1/auth/logout, revert to legacy mode
```

`hasWriteAccess()` in JWT mode reads the role from the cached login/auth-init response (not by decoding the JWT client-side).

## Changes to Existing Pages

### Pages that lose auth code:
- **talkgroup-directory.html** — remove write-token button, row, `getWriteHeaders()`, `WRITE_TOKEN_KEY`, related CSS (~50 lines)
- **playground.html** — remove write-token prompt div, `getWriteToken()`, retry button, related CSS (~40 lines)
- **units.html** — remove custom `showAuth()`, auth prompt HTML/CSS, 403 handling (~80 lines)
- **events.html** — remove `showAuth()`/`hideAuth()`, `#auth-prompt` div + CSS. Currently auth.js defers to this page's prompt via `document.getElementById('auth-prompt')` — that check goes away when auth.js owns the modal.

### Pages that work with zero changes:
- **irc-radio-live.html** — never had write-token handling; auth.js now covers its PATCH calls
- Read-only pages (scanner.html, timeline.html, etc.) — auth.js's read-token injection continues to work
- debug-report.html, audio-diagnostics.html — their POSTs get 403→prompt handling for free

### EventSource (SSE):
SSE is read-only, so it only needs the read token (legacy) or JWT. auth.js already patches EventSource to append `?token=`. In JWT mode, the JWT is appended instead. Note: if the JWT expires mid-stream (1h expiry), EventSource will disconnect and reconnect. On reconnect, auth.js should reconstruct the URL with the current token (which may have been refreshed since the original connection).

## Server-Side Changes

None. The auth-init endpoint already returns `user` when a valid JWT is present. The JWT login/refresh/logout endpoints exist. The `JWTOrTokenAuth` middleware handles all token types. The web UI just needs to use what's already there.

## Edge Cases

- **Caddy header injection**: Caddy injects the read token when no Authorization header is present. auth.js's fetch patch sets Authorization before the request reaches Caddy, so Caddy's `@no_auth` matcher correctly passes it through. No change to Caddy config.
- **Multiple 403s**: Debounced via shared `pendingPrompt` promise — one prompt, all waiters retry when resolved.
- **Token refresh race**: If multiple fetches hit 401 simultaneously (expired JWT), only one refresh attempt fires. Others await the same promise.
- **Backward compat**: `trAuth.getToken()` and `trAuth.showPrompt()` continue to work. Pages that reference `tr-engine-token` directly in localStorage still get the read token.
- **No-auth mode**: When auth-init returns no token and no JWT exists, mode is `'none'`. No headers injected, no prompts shown, all requests pass through (matches server behavior when auth is disabled).
