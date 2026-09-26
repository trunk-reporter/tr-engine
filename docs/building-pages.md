# Building Custom Pages

tr-engine's REST API and SSE event stream make it easy to build custom dashboards and visualizations. You can generate pages with an AI assistant like Claude — just describe what you want and provide the API spec.

## Quick Start: Page Builder

The fastest way: open the [Page Builder](/playground.html) on your tr-engine instance, describe what you want, and copy the generated prompt into Claude.

Live demo: [tr-engine.luxprimatech.com/playground.html](https://tr-engine.luxprimatech.com/playground.html)

## Two Modes

### Integrated (recommended for tr-engine users)

Pages live in tr-engine's `web/` directory and use the built-in theme system and `auth.js`, which handles API keys and tickets for them. They appear in the nav dropdown automatically.

### Standalone

Self-contained HTML files that work from anywhere. The page connects to your tr-engine instance by URL, checks what it may do with `GET /api/v1/whoami`, and uses an API key the user pastes in (or none, if the instance allows anonymous listening). Good for running locally or for your own use.

> **A key in a page other people load is public.** Don't embed a key in a page you share. Shared or public pages should use no key (the operator's anonymous access policy decides what they can read), or talk to a server of your own that holds the key. See [auth.md](auth.md#the-one-rule-a-key-that-reaches-other-peoples-browsers-is-public).

## Manual Prompt Template

If you prefer to copy-paste directly, here are the prompt skeletons. Replace `{DESCRIPTION}` with what you want built.

### Integrated Mode

~~~
Build a single-file HTML page that will be served from tr-engine's web/ directory.

## API Specification
Read the full API spec here: https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/openapi.yaml

Use any endpoints you need. All endpoints are under /api/v1. Responses use {items, total, limit, offset} pagination.

## Authentication
Include this as the FIRST script in <head>:
<script src="auth.js?v=3"></script>

auth.js keeps the user's API key (if any) and handles auth for same-origin /api/ URLs:
- fetch('/api/v1/...') gets the Authorization header automatically; call it normally.
- new EventSource('/api/v1/events/stream?...') is replaced by a wrapper that mints a
  short-lived ticket, reconnects with a fresh one and resumes without gaps.
- For <audio>, set src to trAuth.mediaUrl('/api/v1/calls/' + id + '/audio'); if the
  element fires 'error', retry once with: el.src = await trAuth.ticketUrl(url).
- For WebSocket URLs, use: await trAuth.ticketUrl('/api/v1/audio/live').
- await trAuth.ready() before the first request if the page needs to know who it is;
  trAuth.hasScope('edit') / trAuth.hasScope('admin') gate edit and admin features.
- If the page takes a key in its own input, store it with trAuth.setKey(value) (it
  cleans the value and rejects one that can't be a key, error code
  invalid_key_format), or run it through trAuth.cleanKey(value) and show .error
  instead of sending it. Never put the raw input in an Authorization header:
  fetch() throws on characters like curly quotes or zero-width spaces, which looks
  like a network failure.
- Never put a key or token in a URL yourself, and never call /api/v1/auth-init
  (it no longer exists).

Handle errors: 401 key_required (no key and anonymous access is off: auth.js
prompts for a key), 403 insufficient_scope (hide the feature), 403
restricted_credential (the credential only sees some talkgroups; that endpoint is
unavailable, so use Promise.allSettled when mixing calls).
Render all API text with textContent, never innerHTML: names and tags can contain HTML.

## Theme System
Include these scripts:
<script src="theme-config.js"></script>  (in <head>, after auth.js)
<script src="theme-engine.js?v=2"></script>  (before </body>)

The theme engine injects a sticky header with nav and theme switcher. Use these CSS variables for styling:

Background: --bg, --bg-surface, --bg-elevated, --bg-tile
Text: --text, --text-mid, --text-muted, --text-faint
Accent: --accent, --accent-light, --accent-dim, --accent-glow
Status: --success, --warning, --danger, --info
Glass: --glass-bg, --glass-border, --glass-shine, --glass-blur
Typography: --font-display, --font-body, --font-mono
Borders: --border, --border-hover, --radius, --radius-sm
Shadows: --shadow-panel, --shadow-panel-hover

## Page Registration
Add these meta tags so the page appears in tr-engine's nav:
<meta name="card-title" content="YOUR PAGE TITLE">
<meta name="card-description" content="Short description">

## SSE Real-Time Events
For live updates, connect to /api/v1/events/stream with filter params:
const es = new EventSource('/api/v1/events/stream?types=call_start,call_end');
es.onmessage = (e) => { const data = JSON.parse(e.data); /* handle event */ };

Filter options: systems, sites, tgids, units, types, emergency_only (all optional, AND-ed).
Event types: call_start, call_end, transcription, unit_event, recorder_update, rate_update
(listen these with es.addEventListener('call_end', ...); console events reach admin keys only).
call_end carries call_id but no audio_url: the audio is at /api/v1/calls/{call_id}/audio.

## What to Build
{DESCRIPTION}
~~~

### Standalone Mode

~~~
Build a self-contained single-file HTML page that connects to a tr-engine REST API instance.

## API Specification
Read the full API spec here: https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/openapi.yaml

Use any endpoints you need. All endpoints are under /api/v1. Responses use {items, total, limit, offset} pagination.

## Authentication
tr-engine authenticates with API keys sent as "Authorization: Bearer <key>". There are
no logins. CORS allows any origin (no cookies), so the page can call the engine directly.

1. Show a config bar with an API URL input (default: window.location.origin), an optional
   "API key" password field (keep the key in localStorage only if the user ticks
   "remember"), and a "Connect" button. Clean the pasted key first: drop invisible
   characters (U+00AD, U+200B-U+200F, U+202A-U+202E, U+2060-U+2064, U+FEFF) and surrounding
   whitespace and curly quotes. Then refuse, with a clear message, tabs, line breaks and
   anything outside printable ASCII (U+0020-U+007E), and a space inside a key that
   starts with tre_. fetch() throws on a header value with a line break or a character
   above U+00FF, which looks like a network failure; other non-ASCII characters (é) are
   sent as single bytes that never match a key; tabs and a space inside a tre_ key are
   paste accidents. Send everything else to /whoami: an imported legacy key (an old
   AUTH_TOKEN/WRITE_TOKEN) may contain inner spaces, and the engine accepts it.
2. On connect, GET {apiUrl}/api/v1/whoami, with the Authorization header if a key was
   entered. 200 → show a green indicator plus whoami.scopes. 401 invalid_key → "key
   rejected". 404 → "this tr-engine is too old". If there is no key and
   whoami.anonymous.access is "off", ask for a key.
3. Wrap fetch:

function apiFetch(path, opts = {}) {
  const headers = { ...opts.headers };
  if (KEY) headers['Authorization'] = 'Bearer ' + KEY;
  return fetch(API_URL + path, { ...opts, headers });
}

Use apiFetch('/api/v1/...') for all API calls. Never put the key in a URL.

4. Browsers can't send headers on EventSource, <audio> or WebSocket. With a key, mint a
   ticket first and put it in ?ticket= (tickets are short-lived and listen-only):

async function ticketUrl(path) {
  if (!KEY) return API_URL + path;               // anonymous: plain URL
  const r = await apiFetch('/api/v1/tickets', { method: 'POST' });
  const { ticket } = await r.json();
  return API_URL + path + (path.includes('?') ? '&' : '?') + 'ticket=' + encodeURIComponent(ticket);
}

Error codes: 401 key_required / invalid_key / invalid_ticket, 403 insufficient_scope /
restricted_credential (the key only sees some talkgroups; use Promise.allSettled when
mixing calls). Render all API text with textContent, never innerHTML.

## SSE Real-Time Events
For live updates, create the EventSource yourself and reconnect with a fresh ticket:

let lastId = null;
async function connect() {
  let path = '/api/v1/events/stream?types=call_start,call_end';
  if (lastId) path += '&last_event_id=' + encodeURIComponent(lastId);
  const es = new EventSource(await ticketUrl(path));
  es.addEventListener('call_end', (e) => { lastId = e.lastEventId; const data = JSON.parse(e.data); /* handle */ });
  es.addEventListener('auth', (e) => {           // the server is closing the stream
    es.close();
    if (JSON.parse(e.data).code === 'ticket_expired') connect();   // otherwise ask for a key
  });
  es.onerror = () => { es.close(); setTimeout(connect, 3000); };
}
connect();

call_end carries call_id but no audio_url: play API_URL + '/api/v1/calls/{call_id}/audio'
through ticketUrl() when a key is set.

## Styling
Use a clean, modern dark theme. No external CSS frameworks needed — inline styles are fine.
Suggested palette: #0a0a0f background, #e0e0e8 text, #00d4ff accent.

## What to Build
{DESCRIPTION}
~~~

## Example Descriptions

These work well as-is or as starting points:

- **Live call feed with audio**: "A live dashboard showing incoming calls via SSE. Each call shows talkgroup name, duration, unit count, and a play button for audio. Auto-scrolls as new calls arrive. Include a pause button to stop auto-scroll."

- **Talkgroup leaderboard**: "A leaderboard showing the busiest talkgroups in the last hour. Horizontal bar chart with talkgroup names on the y-axis and call count on the x-axis. Auto-refreshes every 60 seconds. Use Chart.js via CDN."

- **Unit activity tracker**: "A grid of unit cards showing all active units. Each card shows unit ID, alpha tag, last event type with a colored badge, and the talkgroup they're on. Updates live via SSE. Cards pulse briefly when their unit has new activity."

## API Reference

Full API specification: [openapi.yaml](https://raw.githubusercontent.com/trunk-reporter/tr-engine/master/openapi.yaml)

Interactive docs: [API Docs](/docs.html) on your tr-engine instance

Key endpoints:
- `GET /api/v1/calls` — recorded calls (paginated, filterable)
- `GET /api/v1/calls/active` — in-progress calls
- `GET /api/v1/talkgroups` — all talkgroups with activity stats
- `GET /api/v1/units` — radio units
- `GET /api/v1/events/stream` — real-time SSE event stream
- `GET /api/v1/stats` — system statistics

See the [README](../README.md) for the full endpoint list.
