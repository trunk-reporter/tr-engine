# OmniTrunker Page Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a real-time OmniTrunker-style live status page with active voice channels and scrolling unit activity feed.

**Architecture:** Single HTML file (`web/omnitrunker.html`), theme-integrated, SSE-powered. No backend changes needed — all data is already available via `GET /api/v1/events/stream`. Single SSE connection with client-side filtering.

**Tech Stack:** Vanilla JS, CSS variables from theme-config.js, EventSource API.

**Design doc:** `docs/plans/2026-03-02-omnitrunker-page-design.md`

---

### Task 1: Create the HTML skeleton with theme integration

**Files:**
- Create: `web/omnitrunker.html`

**Step 1: Create the file with page structure**

Create `web/omnitrunker.html` with:
- Standard theme-integrated boilerplate (copy pattern from `web/events.html`)
- Meta tags: `card-title="OmniTrunker"`, `card-description="Live voice channels and unit activity feed"`, `card-order="3"`
- Script includes: `auth.js?v=1`, `theme-config.js`, `theme-engine.js?v=2`
- Body structure:
  - `<div class="vignette-overlay"></div>`
  - Toolbar: connection status dot + text, system filter dropdown, event count
  - Top panel: `#active-channels` — "Active Voice Channels" header + table
  - Bottom panel: `#unit-activity` — "Unit Activity" header + controls (buffer size, type filter, auto-scroll, clear) + scrolling table
  - Auth prompt (same pattern as events.html)
  - Theme label

**Step 2: Verify page loads**

Open `https://tr-engine.luxprimatech.com/omnitrunker.html` (or local) and confirm it renders with the theme, shows in nav dropdown.

**Step 3: Commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: add OmniTrunker page skeleton with theme integration"
```

---

### Task 2: CSS for the two-panel layout and type badges

**Files:**
- Modify: `web/omnitrunker.html`

**Step 1: Add CSS for layout**

Two panels stacked vertically. Top panel (Active Voice Channels) has a fixed max-height with the table inside. Bottom panel (Unit Activity) takes remaining space and scrolls.

Key styles:
- `.panel` — shared panel styling with header
- `.panel-header` — "Active Voice Channels" / "Unit Activity" in accent color, uppercase, small
- `#active-channels table` — full-width, fixed layout, themed rows
- `#unit-activity` — flex: 1, overflow-y: auto
- `.controls` — flex row for buffer/type/auto-scroll/clear controls
- `.channel-row` — table row for active calls
- `.channel-row.fading` — opacity transition for call_end fade-out
- `.channel-row.emergency` — accent/danger border highlight
- `.activity-row` — table row for unit events
- `.encrypted-badge` — red background pill

**Step 2: Add CSS for type badges**

Color-coded badge styles using theme CSS variables and `color-mix()`:
```css
.badge { padding: 1px 6px; border-radius: var(--radius-xs); font-size: 11px; font-weight: 600; text-transform: uppercase; text-align: center; display: inline-block; min-width: 90px; }
.badge-grant { background: color-mix(in srgb, var(--info) 20%, transparent); color: var(--info); }
.badge-affiliation { background: color-mix(in srgb, var(--success) 20%, transparent); color: var(--success); }
.badge-registration { background: color-mix(in srgb, var(--warning) 20%, transparent); color: var(--warning); }
.badge-deregistration { background: color-mix(in srgb, var(--danger) 20%, transparent); color: var(--danger); }
.badge-acknowledge { background: color-mix(in srgb, var(--magenta) 20%, transparent); color: var(--magenta); }
.badge-data { background: var(--bg-tile); color: var(--text-muted); }
.badge-location { background: color-mix(in srgb, var(--cyan) 20%, transparent); color: var(--cyan); }
.badge-call-end { background: var(--bg-tile); color: var(--text-faint); }
.badge-signal { background: color-mix(in srgb, var(--magenta) 15%, transparent); color: var(--magenta); }
```

**Step 3: Commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: add OmniTrunker CSS layout and type badge styles"
```

---

### Task 3: SSE connection and event routing

**Files:**
- Modify: `web/omnitrunker.html`

**Step 1: Implement SSE connection**

Follow the pattern from `web/events.html`:
- Connect to `/api/v1/events/stream` with no type filter (receive all events)
- Handle `onopen` → set status connected
- Handle `onerror` → reconnection logic, auth prompt if never connected
- Listen for event types: `call_start`, `call_end`, `unit_event`
- Store `lastEventId` for reconnect
- Auth token from `localStorage` or auth prompt

**Step 2: Implement event routing**

```javascript
// Route events to the appropriate panel handler
eventSource.addEventListener('call_start', (e) => {
  const data = JSON.parse(e.data);
  addActiveChannel(data);
  addActivityRow('grant', data);
});
eventSource.addEventListener('call_end', (e) => {
  const data = JSON.parse(e.data);
  removeActiveChannel(data);
  addActivityRow('call_end', data);
});
eventSource.addEventListener('unit_event', (e) => {
  const data = JSON.parse(e.data);
  addActivityRow(data.event_type, data);
});
```

**Step 3: Verify connection**

Open the page, confirm status dot goes green, confirm events start flowing in the console.

**Step 4: Commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: add SSE connection and event routing for OmniTrunker"
```

---

### Task 4: Active Voice Channels panel

**Files:**
- Modify: `web/omnitrunker.html`

**Step 1: Implement addActiveChannel(data)**

- Maintain a Map of active calls keyed by `call_id`
- On `call_start`: create/update a table row with columns:
  - System (from system_name or instance_id)
  - Channel (freq / 1e6, toFixed(4) + " MHz")
  - Talkgroup (tgid)
  - Alpha Tag (tg_alpha_tag)
  - Source (unit ID, colored like tr-web)
  - Unit Alias (unit_alpha_tag)
  - Elapsed (live-updating, computed from start_time)
  - Mode (audio_type — map to FDMA/TDMA-0/TDMA-1/Analog)
  - Encryption (red "ENCRYPTED" badge if encrypted=true)
- Emergency rows get `.emergency` class

**Step 2: Implement removeActiveChannel(data)**

- On `call_end`: find row by call_id, add `.fading` class
- After 5s transition, remove the row and delete from Map
- If call_id not found, try fuzzy match by tgid (same workaround as backend)

**Step 3: Implement elapsed timer**

- `requestAnimationFrame` loop that updates all elapsed cells every second
- Format as `Xs` for <60s, `M:SS` for <1h, `H:MM:SS` for longer
- Only update visible rows

**Step 4: System filter dropdown**

- Populate from seen systems as events arrive
- "All Systems" default
- Filters both Active Voice Channels and Unit Activity

**Step 5: Verify**

Confirm calls appear, elapsed ticks up, calls fade out after ending, encryption badge shows on encrypted calls.

**Step 6: Commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: implement Active Voice Channels panel with live elapsed timer"
```

---

### Task 5: Unit Activity feed

**Files:**
- Modify: `web/omnitrunker.html`

**Step 1: Implement addActivityRow(eventType, data)**

- Prepend a row to the unit activity table
- Columns: Time (HH:MM:SS), System, Site ID, Unit ID, Unit Alias, Type (color-coded badge), Talkgroup, TG Alpha
- Map event_type to badge class:
  - `call` / call_start → badge-grant, label "GRANT"
  - `join` → badge-affiliation, label "AFFILIATION"
  - `on` → badge-registration, label "REGISTRATION"
  - `off` → badge-deregistration, label "DEREGISTRATION"
  - `ackresp` → badge-acknowledge, label "ACKNOWLEDGE"
  - `data` → badge-data, label "DATA_GRANT"
  - `location` → badge-location, label "LOCATION"
  - `end` / call_end → badge-call-end, label "CALL END"
  - `signal` → badge-signal, label "SIGNAL"
- Trim excess rows based on buffer size setting

**Step 2: Implement controls**

- Buffer size: `<select>` with 50/100/200/500, default 200. On change, trim excess.
- Type filter: `<select>` with "All Types" + each type. Client-side filter — hide rows that don't match, or skip adding them.
- Auto-scroll: checkbox (default checked). When on, scroll to top on new row (since prepending).
- Clear: button that empties the feed.

**Step 3: Verify**

Confirm events stream in with correct badges, type filter works, buffer size trims, auto-scroll works.

**Step 4: Commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: implement Unit Activity feed with type badges and controls"
```

---

### Task 6: Polish and deploy

**Files:**
- Modify: `web/omnitrunker.html`

**Step 1: Empty states**

- Active Voice Channels: "No active calls" centered message when table is empty
- Unit Activity: "Waiting for events..." message on initial load

**Step 2: Mobile responsiveness**

- Stack panels vertically on narrow screens (already natural)
- Reduce table font size
- Hide less-important columns (Site ID, Mode) on mobile

**Step 3: XSS safety**

- Ensure all dynamic content uses text escaping (same `esc()` helper from events.html)
- No `innerHTML` with unescaped user data

**Step 4: Deploy to live instance**

```bash
scp web/omnitrunker.html root@tr-dashboard:/data/tr-engine/v1/web/
```

**Step 5: Final commit**

```bash
git add web/omnitrunker.html
git commit -m "feat: polish OmniTrunker page with empty states and mobile support"
```

---

## Reference Files

- Pattern to follow: `web/events.html` (SSE connection, theme integration, event handling)
- Theme system: `web/theme-config.js` (CSS variables), `web/theme-engine.js` (nav header)
- Auth: `web/auth.js` (token injection)
- SSE endpoint: `GET /api/v1/events/stream` (see `openapi.yaml`)
- Design doc: `docs/plans/2026-03-02-omnitrunker-page-design.md`
- SSE event payloads: `internal/ingest/eventbus.go`, handler files in `internal/ingest/`
