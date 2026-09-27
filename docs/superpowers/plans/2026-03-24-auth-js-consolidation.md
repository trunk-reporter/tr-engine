# auth.js Consolidation Implementation Plan

> **Superseded by [2026-09-26-api-key-auth-design.md](../specs/2026-09-26-api-key-auth-design.md).** tr-engine now uses API keys, an anonymous access policy and tickets; the modes, tokens, logins and users described here no longer exist. Kept as a historical record. Current docs: `docs/auth.md`, `docs/migrating-auth.md`.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Centralize all web UI auth handling in `auth.js` so pages never handle auth errors themselves.

**Architecture:** `auth.js` auto-detects legacy token vs JWT mode, patches `fetch()` to inject the right token for reads vs writes, intercepts 401/403 responses with transparent retry after prompting, and owns a single auth modal. Per-page auth code is removed from 7 pages (units, events, talkgroup-directory, playground, omnitrunker, omnitrunker-classic, timeline).

**Tech Stack:** Vanilla JS (no build step), localStorage, existing `/api/v1/auth-init` and `/api/v1/auth/login` endpoints.

**Spec:** `docs/superpowers/specs/2026-03-24-auth-js-consolidation-design.md`

---

### Task 1: Rewrite auth.js — core token management and fetch patch

**Files:**
- Rewrite: `web/auth.js` (168 lines currently, ~300 lines after)

This is the foundation. Everything else depends on it.

- [ ] **Step 1: Start the rewrite (git history serves as backup)**

Write the new `auth.js`. The structure:

```
1. Constants (localStorage keys)
2. State (mode, tokens, role, pendingPrompt)
3. Initialization (sync XHR to auth-init)
4. Fetch patch (token injection + response interception)
5. EventSource patch (token append)
6. Modal UI (create DOM elements via safe DOM API)
7. Prompt logic (show/hide, mode-specific rendering)
8. Login flow (JWT POST)
9. Public API (window.trAuth)
```

Write the initialization and token storage:

```js
(function () {
  'use strict';

  // --- Constants ---
  var STORAGE_READ   = 'tr-engine-token';
  var STORAGE_WRITE  = 'tr-engine-write-token';
  var STORAGE_JWT    = 'tr-engine-jwt';

  // --- State ---
  var mode = 'none';       // 'jwt' | 'legacy' | 'none'
  var readToken = '';
  var writeToken = localStorage.getItem(STORAGE_WRITE) || '';
  var jwtToken = localStorage.getItem(STORAGE_JWT) || '';
  var jwtRole = '';
  var jwtUsername = '';
  var pendingPrompt = null;

  // --- Initialization (synchronous) ---
  try {
    var xhr = new XMLHttpRequest();
    xhr.open('GET', '/api/v1/auth-init', false);
    if (jwtToken) xhr.setRequestHeader('Authorization', 'Bearer ' + jwtToken);
    xhr.send();
    if (xhr.status === 200) {
      var data = JSON.parse(xhr.responseText);
      if (data.token) readToken = data.token;
      if (data.user && jwtToken) {
        mode = 'jwt';
        jwtRole = data.user.role || '';
        jwtUsername = data.user.username || '';
      } else {
        if (jwtToken) { jwtToken = ''; localStorage.removeItem(STORAGE_JWT); }
        if (readToken) mode = 'legacy';
      }
    }
  } catch (e) { /* server may not support auth-init */ }

  if (!readToken && window.__TR_AUTH_TOKEN__) {
    readToken = window.__TR_AUTH_TOKEN__;
    if (mode === 'none') mode = 'legacy';
  }
```

- [ ] **Step 2: Write the fetch patch with method-aware token injection**

```js
  function isLocalAPI(url) {
    if (url.startsWith('/api/')) return true;
    try { var u = new URL(url, location.origin); return u.origin === location.origin && u.pathname.startsWith('/api/'); }
    catch (e) { return false; }
  }

  function isMutation(method) {
    return method && ['POST', 'PATCH', 'PUT', 'DELETE'].indexOf(method.toUpperCase()) !== -1;
  }

  function effectiveToken(method) {
    if (mode === 'jwt' && jwtToken) return jwtToken;
    if (mode === 'legacy') {
      if (isMutation(method) && writeToken) return writeToken;
      return readToken;
    }
    return '';
  }

  var _fetch = window.fetch;
  window.fetch = function (input, init) {
    init = init || {};
    var url = typeof input === 'string' ? input : input instanceof Request ? input.url : '';
    var method = (init.method || (input instanceof Request ? input.method : 'GET')).toUpperCase();
    var clonedInput = input instanceof Request ? input.clone() : input;

    if (isLocalAPI(url)) {
      var headers = new Headers(init.headers || {});
      if (!headers.has('Authorization')) {
        var tok = effectiveToken(method);
        if (tok) headers.set('Authorization', 'Bearer ' + tok);
      }
      init.headers = headers;
    }

    return _fetch.call(this, input, init).then(function (resp) {
      if (!isLocalAPI(url)) return resp;
      if (resp.status === 401) return handleAuthError(401, clonedInput, init, method);
      if (resp.status === 403 && isMutation(method)) return handleAuthError(403, clonedInput, init, method);
      return resp;
    });
  };
```

- [ ] **Step 3: Write the auth error handler with debounced prompting and retry**

```js
  function handleAuthError(status, input, init, method) {
    if (pendingPrompt) {
      return pendingPrompt.then(function () { return retryFetch(input, init, method); });
    }
    var resolve;
    pendingPrompt = new Promise(function (r) { resolve = r; });

    return new Promise(function (outerResolve) {
      if (status === 401) {
        if (mode === 'jwt' || jwtToken) {
          silentRefresh().then(function (ok) {
            if (ok) { resolve(); pendingPrompt = null; outerResolve(retryFetch(input, init, method)); }
            else { showModal('login', function () { resolve(); pendingPrompt = null; outerResolve(retryFetch(input, init, method)); }); }
          });
        } else {
          showModal('token', function () { resolve(); pendingPrompt = null; outerResolve(retryFetch(input, init, method)); });
        }
      } else if (status === 403) {
        if (mode === 'jwt') {
          showModal('insufficient', function () { resolve(); pendingPrompt = null;
            outerResolve(new Response(JSON.stringify({ error: 'insufficient permissions' }), { status: 403, headers: { 'Content-Type': 'application/json' } }));
          });
        } else {
          showModal('write-token', function () { resolve(); pendingPrompt = null; outerResolve(retryFetch(input, init, method)); });
        }
      }
    });
  }

  function retryFetch(input, init, method) {
    init = Object.assign({}, init);
    var headers = new Headers(init.headers || {});
    var tok = effectiveToken(method);
    if (tok) headers.set('Authorization', 'Bearer ' + tok);
    init.headers = headers;
    return _fetch.call(window, input, init);
  }

  function silentRefresh() {
    return _fetch.call(window, '/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin' })
      .then(function (r) {
        if (!r.ok) return false;
        return r.json().then(function (data) {
          if (data.access_token) {
            jwtToken = data.access_token;
            localStorage.setItem(STORAGE_JWT, jwtToken);
            if (data.user) { jwtRole = data.user.role || jwtRole; jwtUsername = data.user.username || jwtUsername; }
            return true;
          }
          return false;
        });
      }).catch(function () { return false; });
  }
```

- [ ] **Step 4: Write the EventSource patch**

Same as current but uses `effectiveToken('GET')`:

```js
  var _EventSource = window.EventSource;
  window.EventSource = function (url, opts) {
    if (isLocalAPI(url)) {
      var tok = effectiveToken('GET');
      if (tok) { var sep = url.indexOf('?') !== -1 ? '&' : '?'; url = url + sep + 'token=' + encodeURIComponent(tok); }
    }
    return new _EventSource(url, opts);
  };
  window.EventSource.prototype = _EventSource.prototype;
  window.EventSource.CONNECTING = _EventSource.CONNECTING;
  window.EventSource.OPEN = _EventSource.OPEN;
  window.EventSource.CLOSED = _EventSource.CLOSED;
```

- [ ] **Step 5: Write the modal UI using safe DOM APIs**

Build the modal entirely with `document.createElement` and `textContent` (no innerHTML/XSS-safe). The modal renders differently for each prompt type: login (username + password), token (single password), write-token (single password), and insufficient (message only).

See spec Section "Prompt UI" for the four modal states.

Key implementation notes:
- Use `document.createElement` for all elements
- Use `textContent` for all text (XSS-safe)
- Use `el.style.cssText` for inline styles matching the existing dark theme
- Store `modalCallback` to call on submit — this resolves the pending promise and triggers retry
- `doLogin()` POSTs to `/api/v1/auth/login`, stores access_token in `STORAGE_JWT`
- `submitToken(storageKey)` stores to the appropriate localStorage key and updates in-memory state
- **All modal types get a Cancel/Close button** that dismisses the modal and lets the original 401/403 response propagate to the page's error handler. For "insufficient permissions" the only button is "OK" (dismiss). For token/login prompts, a secondary "Cancel" button sits beside Submit.
- Clicking the overlay background also dismisses (calls the cancel path)

- [ ] **Step 6: Write the public API**

```js
  window.trAuth = {
    getToken: function () { return mode === 'jwt' ? jwtToken : readToken; },
    getWriteToken: function () { return mode === 'jwt' ? jwtToken : (writeToken || readToken); },
    setToken: function (t) {
      readToken = t || '';
      if (readToken) localStorage.setItem(STORAGE_READ, readToken);
      else localStorage.removeItem(STORAGE_READ);
    },
    getMode: function () { return mode; },
    hasWriteAccess: function () {
      if (mode === 'jwt') return jwtRole === 'editor' || jwtRole === 'admin';
      return !!writeToken;
    },
    showPrompt: function () {
      showModal(mode === 'jwt' ? 'login' : 'token', function () { location.reload(); });
    },
    logout: function () {
      jwtToken = ''; jwtRole = ''; jwtUsername = '';
      localStorage.removeItem(STORAGE_JWT);
      mode = readToken ? 'legacy' : 'none';
      _fetch.call(window, '/api/v1/auth/logout', { method: 'POST', credentials: 'same-origin' }).catch(function () {});
      location.reload();
    },
  };
})();
```

- [ ] **Step 7: Verify auth.js loads without errors**

Open any page in browser. Check console for JS errors. Verify:
- `trAuth.getMode()` returns `'legacy'` or `'none'`
- `trAuth.getToken()` returns the read token
- `trAuth.hasWriteAccess()` returns `false` (no write token set)

- [ ] **Step 8: Commit**

```bash
git add web/auth.js
git commit -m "feat: rewrite auth.js with two-token support, JWT mode, and transparent retry

auth.js now auto-detects legacy token vs JWT mode, injects write tokens
for mutations, intercepts 401/403 with a shared modal prompt, and
retries the original fetch transparently. Pages no longer need to
handle auth errors themselves."
```

---

### Task 2: Remove auth code from units.html

**Files:**
- Modify: `web/units.html`

Remove the custom auth prompt (HTML, CSS, JS), `showAuth()`/`hideAuth()`, `authToken` variable, `authHeaders()` function, and 403 handling in `saveUnitTag()`. Replace with reliance on auth.js.

- [ ] **Step 1: Remove auth prompt CSS**

Delete the `.auth-prompt` and `.auth-box` CSS block (lines ~137-159).

- [ ] **Step 2: Remove auth prompt HTML**

Delete the `<div id="auth-prompt" class="auth-prompt">` block (lines ~512-518).

- [ ] **Step 3: Remove auth JS variables and functions**

Remove:
- `let authToken = localStorage.getItem('tr-engine-token') || '';` (line ~569)
- `const authPrompt = ...`, `const authTokenInput = ...`, `const authSubmitBtn = ...` (lines ~592-594)
- `function authHeaders()` (lines ~600-604) — and replace **all 7 call sites** with `{}`: lines ~1015, 1045, 1111, 1172, 1201, 1499, 1554 (search for `authHeaders()` to find them all)
- `function showAuth()` (lines ~1659-1663)
- `function hideAuth()` (lines ~1665-1667)
- `authSubmitBtn.addEventListener(...)` block (lines ~1669-1674)
- `authTokenInput.addEventListener(...)` block (lines ~1676-1678)

- [ ] **Step 4: Simplify SSE connection**

In `connectSSE()` (~line 1606-1608), remove the manual token param:
```js
// Before:
if (authToken) params.set('token', authToken);
// After: remove this line — auth.js patches EventSource
```

- [ ] **Step 5: Remove 401/403 handling from fetch calls**

In the bootstrap `fetchJSON()` function (~line 1498-1507), remove the 401/403 check:
```js
// Remove:
if (resp.status === 401 || resp.status === 403) {
  showAuth();
  throw new Error('auth');
}
```

Similarly remove the 401/403 check in the talkgroup units fetch (~line 1555).

Note: `saveUnitTag()` (~line 1108) has no custom 403 handling — it just does `if (!r.ok) throw new Error(...)` which is fine as-is. auth.js intercepts the 403 before the page sees it.

- [ ] **Step 6: Remove SSE auth code**

In EventSource `onopen` handler (~line 1617), remove the `hideAuth()` call.
In EventSource `onerror` handler (~line 1622-1627), remove the `showAuth()` call. Keep the `eventSource.close()`.

- [ ] **Step 7: Test in browser**

Open units.html. Verify:
- Page loads, units display
- Click a unit, edit name, save -> auth.js prompts for write token -> retry succeeds
- Page reload -> write token persists

- [ ] **Step 8: Commit**

```bash
git add web/units.html
git commit -m "refactor: remove per-page auth handling from units.html

auth.js now handles all token injection, 401/403 prompting, and retry."
```

---

### Task 3: Remove auth code from events.html

**Files:**
- Modify: `web/events.html`

- [ ] **Step 1: Remove auth prompt CSS**

Delete `.auth-prompt` and `.auth-box` CSS (lines ~81-104).

- [ ] **Step 2: Remove auth prompt HTML**

Delete `<div id="auth-prompt" class="auth-prompt">` block (lines ~185-195 approx).

- [ ] **Step 3: Remove auth JS variables and functions**

Remove:
- `const authPrompt = ...`, `const authTokenInput = ...` (lines ~201-202)
- `let authToken = localStorage.getItem('tr-engine-token') || '';` (line ~208)
- `function showAuth()` (line ~298)
- `function hideAuth()` (line ~304)
- Auth submit event listener (lines ~362-368)
- Manual token params in EventSource URL construction (~lines 318-319)

- [ ] **Step 4: Remove SSE error auth fallback**

In EventSource onerror (~line 339), remove `showAuth()` call.

- [ ] **Step 5: Test in browser**

Open events.html. Verify SSE connects and events stream.

- [ ] **Step 6: Commit**

```bash
git add web/events.html
git commit -m "refactor: remove per-page auth handling from events.html

auth.js now handles all token injection and 401 prompting."
```

---

### Task 4: Remove write-token code from talkgroup-directory.html

**Files:**
- Modify: `web/talkgroup-directory.html`

- [ ] **Step 1: Remove write-token CSS**

Delete `.write-token-btn`, `.write-token-row`, and related CSS (lines ~92-132).

- [ ] **Step 2: Remove write-token HTML**

Delete the write-token button (line ~298) and write-token-row div (lines ~303-308).

- [ ] **Step 3: Remove write-token JS**

Remove:
- `function getAuthHeaders()` (lines ~393-396) — duplicates auth.js's fetch patch
- `function getWriteHeaders()` (lines ~397-401)
- Write token UI variables and event listeners (lines ~404-440)

- [ ] **Step 4: Simplify all fetch calls**

Replace all `{ headers: getAuthHeaders() }` calls (lines ~445, 474) with `{}`.
Replace `headers: getWriteHeaders()` in the CSV import fetch (~line 635) with `{}`.
Search for any remaining `getAuthHeaders` or `getWriteHeaders` references.

- [ ] **Step 5: Test in browser**

Open talkgroup-directory.html. Verify talkgroups load and CSV import triggers write-token prompt from auth.js on 403.

- [ ] **Step 6: Commit**

```bash
git add web/talkgroup-directory.html
git commit -m "refactor: remove per-page write-token UI from talkgroup-directory.html

auth.js now handles write-token prompting centrally on 403."
```

---

### Task 5: Remove write-token code from playground.html

**Files:**
- Modify: `web/playground.html`

- [ ] **Step 1: Remove write-token CSS**

Delete `.write-token-prompt` and related CSS (lines ~237-256).

- [ ] **Step 2: Remove write-token HTML**

Delete the `<div class="write-token-prompt" id="writeTokenPrompt">` block (lines ~356-362).

- [ ] **Step 3: Remove write-token JS**

Remove:
- `WRITE_TOKEN_KEY`, `writeTokenPrompt`, `writeTokenInput`, `writeTokenRetry` variables (lines ~617-620)
- `function getWriteToken()` (lines ~622-624)
- Write token header injection in `doSave()` (lines ~660-661)
- 401/403 handling in `doSave()` result check (lines ~679-683)
- `writeTokenRetry` and `writeTokenInput` event listeners (lines ~705-714)
- References to `writeTokenRetry.disabled` (lines ~655, 673, 693)

- [ ] **Step 4: Simplify doSave()**

Replace headers block:
```js
// Before:
var headers = { 'Content-Type': 'application/json' };
var wt = getWriteToken();
if (wt) headers['Authorization'] = 'Bearer ' + wt;
// After:
var headers = { 'Content-Type': 'application/json' };
```

Remove the 401/403 branch from the response handler.

- [ ] **Step 5: Test in browser**

Open playground.html. Verify page save triggers write-token prompt from auth.js on 403.

- [ ] **Step 6: Commit**

```bash
git add web/playground.html
git commit -m "refactor: remove per-page write-token UI from playground.html

auth.js now handles write-token prompting centrally on 403."
```

---

### Task 6: Remove auth code from omnitrunker.html

**Files:**
- Modify: `web/omnitrunker.html`

Same pattern as units.html — has `.auth-prompt` CSS, auth-prompt HTML, `showAuth()`, `authToken`, manual SSE token injection.

- [ ] **Step 1: Remove auth prompt CSS**

Delete `.auth-prompt` and `.auth-box` CSS (lines ~84-85 and surrounding block).

- [ ] **Step 2: Remove auth prompt HTML**

Delete `<div id="auth-prompt" class="auth-prompt">` block (line ~901 area).

- [ ] **Step 3: Remove auth JS**

Remove `authToken` variable (~line 1202), `showAuth()` function (~line 1916), manual token SSE param (~lines 1946-1947), `showAuth()` in SSE onerror (~line 1967). Remove `authHeaders()` and replace all call sites with `{}`.

- [ ] **Step 4: Test in browser**

Open omnitrunker.html. Verify page loads and SSE connects.

- [ ] **Step 5: Commit**

```bash
git add web/omnitrunker.html
git commit -m "refactor: remove per-page auth handling from omnitrunker.html"
```

---

### Task 7: Remove auth code from omnitrunker-classic.html

**Files:**
- Modify: `web/omnitrunker-classic.html`

Same pattern — auth-prompt CSS/HTML, `showAuth()`, `authToken`, manual SSE token.

- [ ] **Step 1: Remove auth prompt CSS**

Delete `.auth-prompt` CSS (lines ~84-85 area).

- [ ] **Step 2: Remove auth prompt HTML**

Delete `<div id="auth-prompt">` block (~line 427 area).

- [ ] **Step 3: Remove auth JS**

Remove `authToken` (~line 460), `showAuth()` (~line 888), manual SSE token param (~lines 918-919), `showAuth()` in onerror (~line 939). Remove `authHeaders()` and replace all call sites with `{}`.

- [ ] **Step 4: Test in browser**

Open omnitrunker-classic.html. Verify page loads.

- [ ] **Step 5: Commit**

```bash
git add web/omnitrunker-classic.html
git commit -m "refactor: remove per-page auth handling from omnitrunker-classic.html"
```

---

### Task 8: Remove auth code from timeline.html

**Files:**
- Modify: `web/timeline.html`

Same pattern — auth-prompt CSS/HTML, `showAuth()`, `authToken`, manual SSE token.

- [ ] **Step 1: Remove auth prompt CSS**

Delete `.auth-prompt` CSS (lines ~303-304 area).

- [ ] **Step 2: Remove auth prompt HTML**

Delete `<div id="auth-prompt">` block (~line 394 area).

- [ ] **Step 3: Remove auth JS**

Remove `authToken` (~line 479), `showAuth()` (~line 1138), manual SSE token param (~line 1146). Remove `authHeaders()` and replace all call sites with `{}`.

- [ ] **Step 4: Test in browser**

Open timeline.html. Verify page loads and timeline renders.

- [ ] **Step 5: Commit**

```bash
git add web/timeline.html
git commit -m "refactor: remove per-page auth handling from timeline.html"
```

---

### Task 9: Verify irc-radio-live.html works without changes

**Files:**
- Read (no modify): `web/irc-radio-live.html`

irc-radio-live.html has two PATCH calls (lines ~1697, ~2549) that rely on auth.js's patched fetch. It never had write-token handling, so auth.js's transparent retry should cover it.

- [ ] **Step 1: Test unit tag edit**

Open irc-radio-live.html. Click a user nick, edit the unit tag, save. Expect:
- With write token already stored: save succeeds silently
- Without write token: auth.js shows write-token prompt, retry succeeds

- [ ] **Step 2: Test talkgroup tag edit**

Click the channel name edit button, change the talkgroup name, save. Same behavior.

- [ ] **Step 3: Note any issues**

If irc-radio-live.html does anything that bypasses the patched fetch, note for follow-up.

---

### Task 10: Final verification sweep

- [ ] **Step 1: Verify no page has `id="auth-prompt"` anymore**

```bash
grep -r 'id="auth-prompt"' web/
```

Expected: no matches.

- [ ] **Step 2: Verify no page references `tr-engine-token` directly**

```bash
grep -r 'tr-engine-token' web/ --include='*.html'
```

Expected: no matches (all token access goes through `trAuth` API now).

- [ ] **Step 3: Verify no page has `getWriteHeaders` or `getWriteToken`**

```bash
grep -r 'getWriteHeaders\|getWriteToken\|WRITE_TOKEN_KEY' web/ --include='*.html'
```

Expected: no matches.

- [ ] **Step 4: Commit any cleanup**

```bash
git add -A web/
git commit -m "chore: verify no page-level auth prompts remain"
```

---

### Task 11: End-to-end testing

- [ ] **Step 1: Test legacy mode — read-only access**

Set `AUTH_TOKEN` in `.env`, no `WRITE_TOKEN`. Restart. Verify:
- Pages load, data displays
- Write operations get 403, auth.js shows "Write Access Required" prompt
- Read-only pages work normally

- [ ] **Step 2: Test legacy mode — read + write access**

Set both `AUTH_TOKEN` and `WRITE_TOKEN`. Enter write token when prompted. Verify:
- Write ops succeed after entering write token
- Write token persists across reload (stored in `tr-engine-write-token`)
- Read operations use the read token

- [ ] **Step 3: Test JWT mode (if JWT_SECRET configured)**

Set `JWT_SECRET` and `ADMIN_PASSWORD`. Verify:
- Login form appears on 401
- After login, reads and writes work with JWT
- `trAuth.getMode()` returns `'jwt'`
- `trAuth.hasWriteAccess()` returns `true` for admin

- [ ] **Step 4: Test no-auth mode**

Clear `AUTH_TOKEN`. Verify pages load without prompts. `trAuth.getMode()` returns `'none'`.

- [ ] **Step 5: Deploy to dev**

```bash
./deploy-dev.sh --web-only
```

Test on live instance.
