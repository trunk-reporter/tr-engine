# tr-engine Web Frontend — Data Recall & Visualization Techniques Catalog

> **Scope**: All pages in `web/` directory (~25 pages + 5 JS modules)  
> **Generated**: 2026-05-10

---

## 1. External Libraries & Dependencies

| Library | CDN URL | Pages Using It | Purpose |
|---------|---------|----------------|---------|
| **ECharts 5.5.0** | `cdn.jsdelivr.net/npm/echarts@5.5.0/dist/echarts.min.js` | `analytics.html`, `traffic-patterns.html`, `emergency-log.html` | Multi-series charts: line, heatmap, area charts |
| **D3.js 7.9.0** | `cdnjs.cloudflare.com/ajax/libs/d3/7.9.0/d3.min.js` | `stream-graph.html` | Stream graph visualization, stack layouts, force simulation |
| **Chart.js 4.x** | `cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.min.js` | `talkgroup-research.html` | Bar, line, and doughnut charts for deep-dive analytics |
| **CKEditor** (inline) | `cdn.ckeditor.com` | `talkgroup-research.html` | Rich text editing for reports |
| **auth.js** (local) | `web/auth.js` | All pages | API-key auth (`?v=3`): one stored key sent as `Authorization: Bearer` on same-origin `/api/` requests, tickets for `EventSource`/`<audio>`/WebSocket, key prompt on 401 `invalid_key`/`key_required`, `window.trAuth` |
| **theme-config.js** (local) | `web/theme-config.js` | All pages | 10+ themes as CSS custom variables (Crystal, Apple Glass, Obsidian, etc.) |
| **theme-engine.js** (local) | `web/theme-engine.js` | All pages | Applies themes, builds switcher UI, persists to localStorage |
| **audio-engine.js** (local) | `web/audio-engine.js` | `audio-diagnostics.html`, scanner pages | WebSocket audio streaming, per-TG AudioWorklet, jitter tracking |
| **audio-worklet.js** (local) | `web/audio-worklet.js` | `audio-diagnostics.html`, scanner pages | Low-latency PCM playback in AudioWorklet |
| **signal-flow-data.js** (local) | `web/signal-flow-data.js` | `stream-graph.html` | Custom 3-hour time-series data adapter with SSE gapless merge |

---

## 2. Data Recall Techniques

### 2.1 API-Based Recall

| Technique | Description | Endpoints | Pages |
|-----------|-------------|-----------|-------|
| **REST GET** | Standard fetch from `/api/v1/...` with JSON payloads | `/systems`, `/talkgroups`, `/calls`, `/units`, `/units/{id}`, `/talkgroups/{id}/calls`, etc. | Most pages |
| **REST POST (custom SQL)** | `POST /query` with server-side SQL, `$1` parameters, row limits | `/query` | `stream-graph.html`, `signal-flow-data.js` |
| **REST POST (admin)** | Mutations with an `admin` API key | `/admin/transcribe-backfill`, `/admin/maintenance` | N/A (no UI) |
| **Paginated REST** | `/calls?limit=PAGE&offset=OFFSET` with server-side pagination | `/calls`, `/talkgroups`, `/units` | `call-history.html`, `emergency-log.html` |
| **Filtered REST** | Multiple query params: `emergency=true`, `system_id=N`, `start_time`, `end_time`, `sort=-stop_time`, `deduplicate=true` | `/calls`, `/talkgroups` | Most pages |
| **SSE Stream** | `GET /events/stream` with `Last-Event-ID` reconnect, filter params: `systems`, `sites`, `tgids`, `units`, `types`, `emergency_only` | `/events/stream` | `events.html`, `ir-radio-live.html`, `stream-graph.html` |
| **WebSocket Binary** | `ws://.../audio/live` — JSON subscribe/unsubscribe messages, binary frame protocol (12-byte header + PCM) | `/audio/live` | `audio-diagnostics.html`, scanner pages |
| **Polling (interval)** | `setInterval(fetch, 30000)` for periodic refresh | `/unit-affiliations` | `unit-tracker.html` |
| **Lazy-loading** | Initial fetch 50 items, "Load More" button fetches next 50 | `/calls` | `call-history.html` |
| **Backfill + Gapless Merge** | SSE opened BEFORE backfill completes; incoming SSE events buffered; when backfill resolves, SSE events within already-backfilled buckets are discarded, rest are merged | Custom via `POST /query` + SSE | `stream-graph.html`, `signal-flow-data.js` |

### 2.2 Client-Side Recall

| Technique | Description | Pages |
|-----------|-------------|-------|
| **localStorage persistence** | `tr-engine-api-key` for the API key (admin pages keep an admin key in `sessionStorage` for the tab only); `eh-theme` for theme; `eh-hidden-pages` for nav visibility | `auth.js`, `theme-engine.js` |
| **In-memory state** | Time bucket map (tgid → call count), roster map (tgid → unit IDs), SSE event buffers | `signal-flow-data.js` |
| **Circular buffer** | `_maxDeltas = 500` for jitter tracking, `_maxTransmissions = 100` for transmission log | `audio-engine.js` |
| **Debounced search** | 300ms debounce on search input before re-filtering in-memory data | `unit-tracker.html` |

---

## 3. Visualization Techniques

### 3.1 Chart Libraries

| Library | Chart Types | Pages |
|---------|------------|-------|
| **ECharts** | Heatmap (day×hour call volume), Line chart (daily trends), Bar chart (usage %), Area chart (dual y-axis) | `analytics.html` (bargraphs), `traffic-patterns.html`, `emergency-log.html` |
| **D3** | Stream graph (stacked area with curved top), Area chart | `stream-graph.html` |
| **Chart.js** | Bar chart (usage by recorder), Line chart (decode rates), Doughnut chart (state %), Custom scatter plot (airtime vs duration) | `talkgroup-research.html` |

### 3.2 Table-Based Visualizations

| Technique | Description | Pages |
|-----------|-------------|-------|
| **Data tables** | Header row with uppercase monospace labels, striped alternating rows, hover highlight, responsive horizontal scroll | `analytics.html`, `traffic-patterns.html`, `emergency-log.html`, `unit-tracker.html` |
| **Call history table** | Sticky header, filterable columns, click-to-iframe audio playback, state badges, pagination | `call-history.html` |
| **Affiliation matrix** | Unit→Talkgroup mapping table, sorted by last seen, monospace unit IDs | `unit-tracker.html` |
| **Talkgroup directory** | Paginated directory table with group, tag, mode (D/A/E/M/T) indicators | `talkgroup-directory.html` |
| **Unit grid** | Responsive card grid (grid-template-columns: repeat(auto-fill, minmax(...))) showing unit status as colored badges | `units.html` |
| **Unit cards** | Glass-morphism cards with status dot, alpha tag, last-seen timestamp, affiliation info | `unit-tracker.html` |

### 3.3 Custom Canvas/SVG Visualizations

| Page | Technique |
|------|-----------|
| `stream-graph.html` | **D3 stream graph** — uses `d3.stack()` to create layered area chart representing each talkgroup's traffic over time. Full-viewport, interactive with system selector and time range controls. Uses D3's `area` generator with curved curves. |
| `timeline.html` (Drift) | **Logarithmic timeline** — events drift from "now" into the past using exponentially decreasing temporal spacing. Events organized by hour/day/week/month/year. Full-viewport horizontal layout. SSE-powered for live updates. Very complex (1168 lines of CSS + JS animation). |
| `ir-radio-live.html` | **IRC-style terminal UI** — simulated IRC chat with channels (`#dispatch`, `#patrol`), nick colors, topic lines, join/part messages, live call overlay panel, inline audio player. Simulates an IRC client for radio event display. Largest page at 4796 lines. |
| `scanner.html` / `scanner-classic.html` | **Mobile scanner UI** — full-screen audio player with channel strip, TGID display, alpha tag, audio waveform visualization, touch-friendly controls. Uses `audio-engine.js` for playback. |
| `omnitrunker.html` / `omnitrunker-classic.html` | **Live channel grid** — real-time grid of active voice channels showing frequency, TGID, alpha tags, unit tracking, and call duration. Auto-updates via SSE. Classic variant has a more raw/terminal aesthetic. |
| `events.html` | **SSE event ticker** — monospace vertical feed of raw events with type filtering toolbar, connection status dot, scroll-to-bottom indicator. |

### 3.4 Audio Visualization

| Technique | Description | Pages |
|-----------|-------------|-------|
| **AudioWorklet** | Custom GLSL-like processor running in AudioWorklet thread for low-latency PCM playback | `audio-worklet.js`, `audio-diagnostics.html`, scanner pages |
| **Per-TG Audio Nodes** | `AudioEngine` creates per-talkgroup node chains: Source → Gain → Compressor → Panner → Destination | `audio-engine.js` |
| **Jitter analysis** | Tracks delta between audio frames, logs to circular buffer, visualizes via stats cards | `audio-diagnostics.html` |
| **Transmission log** | Log of completed transmissions with timestamps, duration, TGID | `audio-diagnostics.html` |
| **WebSocket binary decode** | 12-byte header (type + length) + PCM audio data, decoded from ArrayBuffers | `audio-engine.js` |

---

## 4. Interactive Patterns

| Pattern | Description | Pages |
|---------|-------------|-------|
| **System selector** | Dropdown to filter data by `system_id`, triggers re-fetch on change | Most pages |
| **Date range filter** | `<input type="date">` inputs for `start_time`/`end_time` | `call-history.html`, `emergency-log.html` |  
| **Search filter** | Debounced text search in in-memory data | `unit-tracker.html`, `call-history.html` |
| **Affiliation filter** | Status toggle (all/affiliated/inactive) | `unit-tracker.html` |
| **Auto-refresh** | 30-second polling with countdown display | `unit-tracker.html` |
| **SSE reconnect** | `Last-Event-ID` header for gapless reconnect on network change | `events.html`, `ir-radio-live.html` |
| **Pagination** | Page numbers with Prev/Next, active page highlighting | `call-history.html`, `emergency-log.html` |
| **Lazy-load** | "Load more" button appends to scrollable list | `call-history.html` |
| **Click-to-play** | Audio file opens in iframe with playback controls | `call-history.html` |
| **Audio toggle** | Play/pause with icon swap | `call-history.html`, scanner pages |
| **Sorting** | `sort=-stop_time` (desc), `sort=start_time` (asc) | `call-history.html`, `emergency-log.html` |
| **Export** | CSV download of unit records | `unit-export.html` |
| **CSV import** | File picker → CSV parsing → API POST | `talkgroup-directory.html` |

---

## 5. Styling & Layout Techniques

| Technique | Description | Pages |
|-----------|-------------|-------|
| **CSS custom properties (all colors)** | 30+ variables: `--bg`, `--text`, `--accent`, `--success`, `--danger`, `--glass-bg`, `--tile-bg`, etc. All colors come from theme config | ALL pages |
| **Glass-morphism** | `backdrop-filter: blur(var(--glass-blur))` on cards and headers | Most pages |
| **Grid overlay** | `body::before` with `linear-gradient` grid pattern from CSS vars | All pages |
| **Scanlines** | `body::after` with repeating-linear-gradient scanline effect | All pages |
| **Vignette** | Optional radial/linear gradient overlay for depth | Most pages |
| **Responsive grid** | `grid-template-columns: repeat(auto-fill, minmax(..., 1fr))` | `unit-tracker.html`, `scanner.html` |
| **Sticky headers** | `position: sticky; top: 60px` on table headers | `call-history.html` |
| **Full-viewport scrolling** | `height: 100vh; overflow: hidden; display: flex; flex-direction: column` | `events.html`, `scanner.html`, `omnitrunker.html` |
| **Mobile-only layout** | `user-scalable=no`, `touch-action: manipulation`, reduced font sizes | `scanner.html` |
| **Monospace-first design** | `font-family: var(--font-mono)` for all data content | `events.html`, `call-history.html`, `omnitrunker.html` |
| **Display font** | `Playfair Display` for headings on Event Horizon | `index.html` |

---

## 6. Page Summary

| # | Page | card-order | Data Recall Methods | Visualization Methods | Libraries | Lines |
|---|------|-----------|-------------------|----------------------|-----------|-------|
| 1 | **Event Horizon** (`index.html`) | 1 | REST summary endpoints, SSE health | Summary tiles, health pills, nav grid | ECharts (health stats), CSS grid | ~714 |
| 2 | **Live Events** (`events.html`) | 2 | SSE event stream | Monospace event feed, filter bar | CSS flex layout | ~380 |
| 3 | **OmniTrunker** | 3 | SSE event stream, REST API | Real-time channel grid, live audio tiles | CSS grid, SSE | ~450 |
| 4 | **Signal Flow** (`stream-graph.html`) | 4 | POST /query (backfill), SSE, /unit-affiliations | D3 stream graph | D3.js 7.9, signal-flow-data.js | ~420 |
| 5 | **Analytics** (`analytics.html`) | 8 | REST /stats (decode rates), recorder snapshots | ECharts bar charts, line charts, combo charts | ECharts 5.5, ECharts gauge | ~480 |
| 6 | **Call History** (`call-history.html`) | 6 | Paginated REST /calls, iframe audio | Filterable table, state badges, pagination | CSS table, inline frame | ~600 |
| 7 | **Talkgroup Research** | 7 | REST /talkgroups/{id}/calls, /talkgroups/{id}/units, REST /query | Chart.js bar/line/doughnut charts, deep-dive table | Chart.js 4, CKEditor (reports) | ~2982 |
| 8 | **Talkgroup Directory** | 5 | REST /talkgroups, REST /query | Paginated directory table, CSV import | CSS table, file picker | ~567 |
| 9 | **Unit Tracker** | 7 (new) | Polling /unit-affiliations, /units | Card grid, affiliation matrix table | CSS grid | ~484 |
| 10 | **Traffic & Patterns** | 5 (new) | /stats/* endpoints | ECharts heatmap, line chart, data table | ECharts 5.5 | ~500 |
| 11 | **Emergency Log** | 9 (new) | Paginated /calls?emergency=true | Timeline cards, ECharts area chart | ECharts 5.5, pagination | ~530 |
| 12 | **Drift** (`timeline.html`) | 1 | SSE stream, REST /calls | Logarithmic time layout, animated event drift | Custom CSS animations, SSE | ~1168 |
| 13 | **IRC Radio** (`irc-radio-live.html`) | 0 | SSE event stream, REST, WebSocket audio | IRC terminal UI with channels/nicks, call overlay | CSS monospace terminal | ~4796 |
| 14 | **Audio Diagnostics** | 12 | WebSocket /audio/live, /api/v1/audio/jitter | Jitter stats, transmission log, audio waveform | audio-engine.js, audio-worklet.js, Web Audio API | ~1157 |
| 15 | **Scanner** (`scanner.html`) | 1 | WebSocket /audio/live, SSE | Full-screen audio player, channel strip | audio-engine.js, AudioWorklet | ~900 |
| 16 | **Scanner Classic** (`scanner-classic.html`) | — | WebSocket /audio/live | Classic-style audio scanner | audio-engine.js | ~800 |
| 17 | **Storage** (`storage.html`) | 21 | REST /admin/maintenance, /stats | Storage stats cards, retention config UI | CSS grid, forms | ~501 |
| 18 | **Drift** (`timeline.html`) | 1 | SSE + REST | Logarithmic timeline animation | Custom JS animation | ~1168 |
| 19 | **Audio Diagnostics** | 12 | WebSocket, SSE | Jitter analysis, waveform display | Web Audio API | ~1157 |
| 20 | **Talkgroup Research** | 7 | REST, custom SQL | Multi-chart deep analysis | Chart.js + CKEditor | ~2982 |
| 21 | **Talkgroup Directory** | 5 | REST, CSV | Paginated table, import UI | — | ~567 |
| 22 | **Page Builder** (`playground.html`) | 11 | AI prompt → HTML generation | Form input → preview render | — | ~769 |
| 23 | **API Docs** (`docs.html`) | 10 | Fetch openapi.yaml | Rendered API spec display | — | ~380 |
| 24 | **Admin** (`admin.html`) | — | Admin REST endpoints | Admin controls, manual maintenance trigger | — | ~400 |
| 25 | **Debug Report** (`debug-report.html`) | hidden | REST POST /debug-report | Form-based report submission | — | ~351 |
| 26 | **Unit Export** (`unit-export.html`) | — | REST /units | CSV export | — | ~200 |

---

## 7. Data Architectures & Pipelines

### 7.1 Signal Flow (Most Complex Data Pipeline)

```
                    ┌─────────────────┐
                    │   BACKFILL      │
                    │ POST /query     │
                    │ date_bin() bucket│
                    │ 4 parallel queries│
                    │ (calls, airtime, │
                    │  units, affiliat.│
                    └────────┬─────────┘
                             │
                    ┌────────▼─────────┐
                    │   ROSTER          │
                    │ GET /unit-affili. │
                    │ → timestamped     │
                    │ baseline          │
                    └────────┬─────────┘
                             │
                    ┌────────▼─────────┐
                    │   LIVE (SSE)      │
                    │ gapless merge     │
                    │ into buckets      │
                    └────────┬─────────┘
                             │
                    ┌────────▼─────────┐
                    │  D3 stream graph │
                    │ visualization     │
                    └──────────────────┘
```

- **BUCKET_SEC = 300** (5-minute buckets), **WINDOW_SEC = 10800** (3 hours)
- Backfill uses `DATE_BIN('300 seconds', start_time)` on PostgreSQL
- SSE gapless handoff: buffer events, merge on backfill completion, discard overlap

### 7.2 Audio Streaming Pipeline

```
                    ┌──────────────────┐
                    │  WebSocket /audio/live │
                    │ binary frames (12-byte header) │
                    └────────┬──────────────┘
                             │
                    ┌────────▼────────────┐
                    │ AudioEngine          │
                    │ per-TG nodes:        │
                    │ Source → Gain → Comp │
                    │ → Panner → Dest      │
                    └────────┬─────────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
        ┌─────▼─────┐ ┌─────▼─────┐ ┌─────▼────────┐
        │ Jitter log │ │ Trans. log │ │ AudioWorklet │
        │ (500 deltas)││ (100 max)  │ │ (PCM playback)│
        └────────────┘ └────────────┘ └──────────────┘
```

### 7.3 SSE Event Pipeline

```
                  ┌──────────────────┐
                  │ GET /events/stream│
                  │ ?systems=&sites=  │
                  │ &tgids=&units=    │
                  │ &types=           │
                  │ &emergency_only=  │
                  └────────┬─────────┘
                           │
                  ┌────────▼─────────┐
                  │ Client-side filter│
                  │ by event type     │
                  │ and filter params │
                  └────────┬─────────┘
                           │
                  ┌────────▼─────────┐
                  │ Render as:       │
                  │ - Event feed     │
                  │ - IRC terminal   │
                  │ - Channel grid   │
                  └──────────────────┘
```

---

## 8. Page Registration Mechanism

Pages are auto-discovered by `GET /api/v1/pages`:
1. Scans `web/*.html` on the server
2. Extracts meta tags from first 2048 bytes: `card-title`, `card-description`, `card-order`
3. Returns sorted JSON array
4. `theme-engine.js` injects a sticky header with a nav dropdown from this list

**Rules:**
- Must be `.html` in root of `web/` (no subdirectories)
- Meta tags must appear within first 2048 bytes
- Attributes must use double quotes: `name="card-title" content="..."`
- Omit `card-title` meta to hide from nav (e.g., `debug-report.html`)

---

## 9. Unique/Notable Approaches

| Page | Notable Technique |
|------|------------------|
| **Signal Flow** | Custom in-memory time-bucket architecture with gapless SSE merge — the most sophisticated data pipeline |
| **IRC Radio** | Full IRC UI simulation (channels, nicks, joins, topics) for radio events — 4796 lines, likely the most complex single page |
| **Drift** | Logarithmic timeline where events spread exponentially from "now" to the past — CSS animations + SSE for live updates |
| **Talkgroup Research** | Only page using two chart libraries (Chart.js + custom canvas), plus CKEditor for report generation |
| **Scanner** | Mobile-first with `user-scalable=no`, touch-optimized, full-screen audio player — the only truly mobile-optimized page |
| **Audio Diagnostics** | Only page using Web Audio API (AudioWorklet + compression + jitter tracking) |
| **Event Horizon** (index) | The navigation hub — uses ECharts for system health visualization, acts as central nav |
| **OmniTrunker** | Real-time channel grid with SSE-powered live updates — the "always-on" monitoring page |

---

## 10. Missing/Missing-Noticed Techniques

| Gap | Could benefit from |
|-----|-------------------|
| No page-level dark mode | Currently relies on ThemeEngine for dark themes (Obsidian, etc.) |
| No caching layer | Every page re-fetches data on load (except in-memory filters) |
| No Web Worker usage | All JS runs on main thread, even for large datasets |
| No service worker | No offline capability or background sync |
| Minimal graph/network visualization | Unit relationships, talkgroup co-activation could use a force-directed graph |
| Map visualization | No geographic map — trunk-recorder instance locations exist but aren't displayed |
| No date-fns dayjs | Custom date formatting scattered across pages |
