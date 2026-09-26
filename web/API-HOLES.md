# API Holes & Page Workarounds Analysis

> **Methodology**: Cross-referenced all fetch calls, template literals, and apiFetch calls across 26 HTML pages against the OpenAPI 3.0.3 spec.
> **Generated**: 2026-05-10

---

## Executive Summary

The API is well-designed and comprehensive, but there are **5 significant holes** and **7 minor gaps** where pages have to work around the API surface. The most impactful pattern is **client-side aggregation** — pages fetch raw data because the API doesn't provide pre-aggregated views for certain use cases.

---

## 1. Major Holes (Pages Work Around These)

### HOLE 1: No client-side transcript search endpoint
**Page**: `call-history.html`
**Pattern**: 

```js
// Fetches all calls matching base filters, then FILTERS IN MEMORY
calls = calls.filter(function(c) {
  return c.transcription_text && c.transcription_text.toLowerCase().indexOf(q) !== -1;
});
```

**Reason**: The API returns calls with `transcription_text` populated (or empty), but there is **no endpoint** to filter/search calls by transcription content. Pages must fetch all matching calls and filter client-side. This is expensive for large datasets — a call-history page with a search might fetch 500+ calls just to filter 3 that have matching text.

**Proposed fix**: `GET /calls/search?q=...&transcription=true` or `POST /query` with `transcription_text ILIKE $1` on the calls table.

---

### HOLE 2: No per-tg / per-day activity aggregate
**Page**: `talkgroup-research.html`, `systems-overview.html`, `index.html`
**Pattern**: Pages fetch `GET /talkgroups?sort=-calls_1h&limit=10` to get a ranked list, then separately fetch `GET /talkgroups/{id}` and `GET /talkgroups/{id}/calls` for detail. They also build their own "busiest TGs" rankings by fetching all talkgroups and sorting client-side.

**Reason**: The closest server-side aggregate is `/stats/talkgroup-activity` which requires a time window but is useful. However, the index page (Event Horizon) would benefit from a dedicated "top X talkgroups in last N hours" summary that combines with the `/stats` endpoint's `system_activity` array.

**This is working but could be more efficient.** The API provides what it needs; it's just not consolidated into a single response.

---

### HOLE 3: No single-payload "dashboard" endpoint
**Pages**: `index.html` (Event Horizon)
**Pattern**: Event Horizon makes **4 separate fetch calls** in parallel:
- `GET /health` — connection status
- `GET /stats` — call counts
- `GET /talkgroups?sort=-calls_1h&limit=10` — top talkgroups
- `GET /recorders` — recorder state

**Reason**: There's no `GET /dashboard` or `GET /stats/summary` that returns all of this in one response. Pages load faster with parallel requests, but this is still 4 HTTP round-trips. The `/health` and `/stats` endpoints could be combined.

**Not a bug, but an optimization opportunity.** The current approach works well because parallel requests hit the database concurrently anyway.

---

### HOLE 4: No SSE event history endpoint
**Pages**: `irc-radio-live.html`, `omnitrunker.html`
**Pattern**:
- Pages open SSE (`GET /events/stream`) and render events in real-time
- For initial historical context, they make **additional parallel fetches**:
  - `GET /calls?sort=-stop_time&limit=500&start_time=X`
  - `GET /unit-events?sort=-time&limit=500&start_time=X&type=join,off`
  - `GET /talkgroups/...` / `GET /units/...`

**Reason**: The SSE endpoint only pushes new events. There's no `GET /events?limit=50&types=call_start,call_end,unit_event&types=join,off` endpoint that would return recent events in a unified format. Pages must fetch calls and unit-events separately and merge them client-side.

**Proposed fix**: `GET /events/recent?limit=50&types=...` — returns unified event objects similar to SSE payloads but as a paginated list.

---

### HOLE 5: No "co-activation" / related-tg analysis endpoint
**Pages**: `talkgroup-research.html`, `irc-radio-live.html`
**Pattern**: To find related talkgroups (TGs that see calls in the same time windows), pages must:
1. Fetch all calls for a TG over a time window
2. Fetch all calls for other TGs in that window
3. Compare time windows client-side

**Reason**: The API has no relationship analysis endpoint. `GET /talkgroups/{id}/calls` returns calls on a single TG. There's no server-side correlation like `"talkgroups active within 5s of: [list]"`.

This is inherently expensive (requires cross-tg joins), so it may be better suited as a batch/pre-computed thing. But for pages that need it, there's no shortcut.

---

## 2. Minor Gaps (Small Inefficiencies)

### GAP 6: Unit events client-side grouping
**Page**: `irc-radio-live.html`

```js
// Groups consecutive same-type events client-side
for (var i = 0; i < events.length; i++) {
  var prev = grouped[grouped.length - 1];
  if (prev && prev.event_type === ev.event_type && prev.tgid === ev.tgid) {
    prev.count++;
    prev.time = ev.time;
    continue;
  }
  grouped.push(ev);
}
```

**Reason**: The API returns raw event stream (1 event per line), but the IRC UI wants to collapse consecutive same `(event_type, tgid)` events into a single display item (e.g., "call ×3"). There's no `GROUP BY event_type, tgid, TIME_BUCKET(...)` variant on the unit-events endpoint.

---

### GAP 7: Transcription batch fetch fallback
**Page**: `irc-radio-live.html`

```js
// First try batch endpoint
fetch(`/transcriptions/batch?call_ids=...`)
// Then fall back to individual fetch with throttling
batch.map(id => fetch(`/calls/${id}/transcription`).then(...))
```

**Reason**: The batch endpoint (`GET /transcriptions/batch?call_ids=...`) exists and is documented, but pages must still implement a fallback because the batch endpoint may be disabled or rate-limited. The fallback of fetching 50-200 calls individually with throttling is wasteful.

---

### GAP 8: No bulk transcription for call lists
**Pages**: `irc-radio-live.html`, `call-history.html`
**Pattern**: The IRC page fetches transcriptions for a list of calls using either batch or individual fetch. `call-history.html` doesn't seem to bulk-fetch transcriptions at all — it fetches `/calls/{id}/transcription` when a user expands a call row.

**Reason**: The batch endpoint exists but isn't universally adopted. Each page decides independently whether to fetch transcription eagerly or lazily.

---

### GAP 9: Audio file streaming not chunked
**Pages**: scanner pages, call-history.html

```js
const audioSrc = `${API_BASE}/calls/${callId}/audio`;
```

**Reason**: The audio endpoint returns the full audio file. There's no support for HTTP Range requests (byte-range seeking), so users can't seek within a recording or start playback before the full file downloads. If audio files are large (multi-minute recordings), this creates a noticeable delay.

---

### GAP 10: No "recent TGs" endpoint
**Pages**: `scanner.html`, `irc-radio-live.html`, `talkgroup-research.html`

**Pattern**: Pages fetch `GET /talkgroups?sort=alpha_tag&limit=1000` to get all talkgroups, then the UI shows a list. With thousands of talkgroups, this is a large response.

**Reason**: While `limit` and `pagination` parameters exist, there's no `GET /talkgroups/recent?hours=1` that returns only talkgroups with recent activity (similar to `/stats/talkgroup-activity` but as a direct list, not an aggregated response).

---

### GAP 11: /query endpoint needs an admin key
**Pages**: `stream-graph.html`, `signal-flow-data.js`

```js
if (!trAuth.hasScope('admin')) return { columns: [], rows: [] }; // /query unavailable
```

**Reason**: `POST /query` needs the `admin` scope (401/403 otherwise). That is deliberate: raw SQL can read every table and can't honour a key's system/talkgroup restriction. Pages check `trAuth.hasScope('admin')` before calling it and degrade on 401/403, but there's no read-only endpoint that provides the same analytical data to `listen` keys or anonymous visitors.

---

### GAP 12: No heatmap for specific systems/TGs
**Pages**: `traffic-patterns.html` (new)

```js
// Only takes system_id param
GET /stats/call-heatmap?hours=720&system_id=1
```

**Reason**: The call-heatmap endpoint only supports `system_id` filter, but pages might want `tgid` filter too (e.g., "visualize traffic patterns for this specific talkgroup"). No per-talkgroup heatmap endpoint exists.

---

## 3. Workaround Patterns by Page

| Page | Workaround | Efficiency Impact |
|------|-----------|-------------------|
| **call-history.html** | Client-side transcript search | High — fetches all calls, filters in memory |
| **irc-radio-live.html** | Client-side event grouping (consecutive same-type) | Low-Medium — groups in-memory after fetch |
| **irc-radio-live.html** | Batch + fallback transcription fetching | Medium — double-fetch path if batch fails |
| **irc-radio-live.html** | Separate calls + unit-events fetch + merge | Medium — no unified event history |
| **scanner.html** | Fetches all unit records (5000 records) on load | High — large payload, mostly stale data |
| **scanner.html** | No "recent TGs" pre-aggregation for initial display | Medium — must wait for full fetch |
| **scanner.html** | Individual call fetch for audio metadata | Low — cached by browser |

---

## 4. High-Impact Recommendations (Priority Order)

### Priority 1: `GET /calls/search?q=...` or `POST /calls?q_search=...`
**Impact**: call-history.html is the most used dashboard page. The transcript search workaround is the single biggest inefficiency.

**Alternative**: Add `transcription` as a text search parameter on `GET /calls`:
```
GET /calls?limit=50&offset=0&q=dispatch&search_fields=transcription_text
```

### Priority 2: `GET /events/recent?limit=50&types=...`
**Impact**: irc-radio-live.html, omnitrunker.html, timeline.html all fetch calls + unit-events separately. This would unify the two into one endpoint with a consistent event format matching SSE payloads.

### Priority 3: `POST /call-heatmap` with `tgid` filter + `GET /stats/talkgroup-activity?top=50`
**Impact**: Pages want per-TG heatmaps and "top talkgroups" lists in a single response. Currently they make 3-4 calls.

### Priority 4: Purpose-built analytics endpoints instead of `POST /query`
**Impact**: signal-flow-data.js (stream-graph.html) and any future analytical pages need aggregate data that today only `/query` (admin) provides. `/query` stays admin-only (raw SQL can't enforce restrictions), so these pages need dedicated `listen` endpoints that apply the caller's restriction.

### Priority 5: Add `Range:` header support to audio endpoints
**Impact**: Enables seeking in long recordings. Low-effort server change, high user-value.

---

## 5. What's Not Actually Missing

The API already provides most of what's needed — these are mostly surface-level gaps:

- ✅ `/stats/call-volume` — good for call trends
- ✅ `/stats/call-heatmap` — good for day/hour patterns  
- ✅ `/stats/daily-overview` — good for daily totals
- ✅ `/stats/talkgroup-activity` — good for TG rankings
- ✅ `/query` — powerful but admin-only
- ✅ `/transcriptions/batch` — exists and works
- ✅ `/unit-affiliations` — complete real-time snapshot
- ✅ `/events/stream` — full-featured with filtering

The holes are mostly about **aggregation convenience** (combining related data in fewer requests) and **search capabilities** (searching transcription text, finding related talkgroups).

---

## 6. Client-Side vs Server-Side Tradeoffs

| Feature | Currently | Why Client-Side |
|---------|-----------|-----------------|
| Transcript search | Client filter | No text search index on transcription_text |
| Event grouping | Client group | Raw stream semantics — no grouping needed for SSE |
| Co-activation analysis | Client analysis | Requires cross-TG JOINs — expensive on demand |
| Recent TGs list | Full fetch + client | No "hot TGs" materialized view |
| Audio seeking | Not supported | FLAC/PCM files on disk, not in DB |

All of these could be moved server-side, but they'd require different database access patterns (full-text search indexes, materialized views, byte-range serving).