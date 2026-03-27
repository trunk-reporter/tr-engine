# Building Custom Pages

tr-engine's REST API and SSE event stream make it easy to build custom dashboards and visualizations. You can generate pages with an AI assistant like Claude — just describe what you want and provide the API spec.

## Quick Start: Page Builder

The fastest way: open the [Page Builder](/playground.html) on your tr-engine instance, describe what you want, and copy the generated prompt into Claude.

Live demo: [tr-engine.luxprimatech.com/playground.html](https://tr-engine.luxprimatech.com/playground.html)

## Two Modes

### Integrated (recommended for tr-engine users)

Pages live in tr-engine's `web/` directory and use the built-in theme system and auto-auth. They appear in the nav dropdown automatically.

### Standalone

Self-contained HTML files that work from anywhere. The page connects to your tr-engine instance by URL and bootstraps auth via the `/auth-init` endpoint. Good for sharing, embedding, or running locally.

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
<script src="auth.js?v=1"></script>

This automatically patches fetch() and EventSource to include auth headers. No manual auth code needed — just use fetch('/api/v1/...') normally.

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
Event types: call_start, call_end, unit_event, recorder_update, rate_update

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
The page needs to connect to a tr-engine instance. Include this auth bootstrap at the top of the page:

1. Show a config bar with an API URL input (default: window.location.origin) and a "Connect" button
2. On connect, fetch {apiUrl}/api/v1/auth-init to get the Bearer token (this endpoint requires no auth)
3. If successful, store the token and show a green connected indicator
4. If it fails (CORS or network), show a manual "Auth Token" input field as fallback
5. Create a helper function that wraps fetch to include the Authorization header:

function apiFetch(path, opts = {}) {
  return fetch(API_URL + path, {
    ...opts,
    headers: { ...opts.headers, 'Authorization': 'Bearer ' + TOKEN }
  });
}

Use apiFetch('/api/v1/...') for all API calls.

## SSE Real-Time Events
For live updates:
const url = API_URL + '/api/v1/events/stream?types=call_start,call_end&token=' + encodeURIComponent(TOKEN);
const es = new EventSource(url);
es.onmessage = (e) => { const data = JSON.parse(e.data); /* handle event */ };

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
