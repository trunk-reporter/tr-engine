# tr-engine Web Frontend — Master Visualization Concepts Index

> **Generated**: 2026-05-10  
> **Total Concepts**: 30 (6 from batch 1 + 24 from batch 2)  
> **Implemented**: 5 pages  
> **Remaining**: 25 concepts as spec-ready blueprints

---

## Implemented (5 pages)

| # | Concept | Technique | Library | File | Lines | Status |
|---|---------|-----------|---------|------|-------|--------|
| 22 | **Talkgroup Bubble Scatter** | Bubble chart (X=logs calls, Y=emrg%, size=avg dur) | ECharts | `web/bubble-scatter.html` | 265 | ✅ Done |
| 9 | **Daily Overview Treemap** | Treemap (systems → talkgroups) | D3.js | `web/daily-overview.html` | 275 | ✅ Done |
| 19 | **Recorder Signal Gauges** | Analogue gauges (signal TG, call rate) | ECharts | `web/recorder-gauges.html` | 224 | ✅ Done |
| 10 | **TG Sunburst** | Sunburst (systems → TGs → hours) | ECharts | `web/tg-sunburst.html` | 218 | ✅ Done |
| 12 | **Daily Overview Calendar Heatmap** | Calendar heatmap (GitHub-style) | ECharts | `web/calendar-heatmap.html` | 208 | ✅ Done |

---

## Batch 1 — Concepts 1–6

### 1: Talkgroup Co-activation Network
- **Type**: Force-directed graph (network diagram)
- **Library**: D3.js 7.9.0 + d3-force
- **Description**: Interactive force-directed graph showing which talkgroups are co-activated in the same calls. Nodes = TGs, edges = co-activation frequency.
- **Data**: `unit_ids[]` from calls or call overlap analysis; `GET /api/v1/calls`
- **Feasibility**: High (~200 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 30–85

### 2: Temporal Activity Hex Heatmap
- **Type**: Hexagonal bin heatmap (hour × day-of-week density)
- **Library**: ECharts 5.5.0
- **Description**: Hex bins showing call density across hours of day vs days of week. Reveals peak hours and anomalies.
- **Data**: Calls grouped by hour of day and day of week
- **Feasibility**: High (~50 LOC — adapting existing analytics heatmap)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 88–124

### 3: Emergency Response Flow Diagram
- **Type**: Sankey diagram
- **Library**: ECharts built-in Sankey or D3 + d3-sankey
- **Description**: Flow diagram showing emergency call origin TGs → responding TGs within 60s. Reveals response chains (dispatch → engines → command).
- **Data**: Calls with `emergency=true` overlapping by 60s
- **Feasibility**: Medium-High (~150 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 127–186

### 4: Encryption & Priority Radar
- **Type**: Radar/spider chart (5 metrics per system)
- **Library**: ECharts or Chart.js radar
- **Description**: Radar chart comparing systems on 5 normalized metrics: call count, emergency rate, encryption rate, active TGs, avg duration. One polygon per system.
- **Data**: Daily stats grouped by system
- **Feasibility**: Very High (~100 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 189–238

### 5: Signal Quality Timeline
- **Type**: Scatter + regression line plot
- **Library**: ECharts 5.5.0
- **Description**: Scatter plot of signal_db vs noise_db over time with regression line and shaded "usable" band. Reveals degradation trends.
- **Data**: Calls with `signal_db`, `noise_db`, `freq_error`
- **Feasibility**: High (~70 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 241–280

### 6: Call Duration Distribution
- **Type**: Histogram + ECDF overlay
- **Library**: ECharts or Chart.js
- **Description**: Call duration histogram with percentile markers and cumulative distribution line. Reveals distribution shape (short bursts vs long tails).
- **Data**: Duration stats grouped by system
- **Feasibility**: Very High (~60 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v6.md` lines 283–339

---

## Batch 2 — Concepts 7–30

### 7: Call Metrics Parallel Coordinates
- **Type**: Parallel coordinates (multidimensional)
- **Library**: D3.js 7.9.0 + d3.parcoords plugin
- **Description**: Calls as lines on 8 parallel axes (duration, signal, noise, freq, freq_error, errors, spikes). Brushing filters across all dimensions.
- **Data**: Calls with signal_db, noise_db, freq_error, error_count, spike_count, duration
- **Feasibility**: Medium (~200 LOC with plugin, ~50 LOC without)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 16–89

### 8: Signal Quality Scatterplot Matrix
- **Type**: Scatterplot matrix (SPLOM) with brushing
- **Library**: D3.js 7.9.0
- **Description**: 6×6 grid of pairwise scatterplots. Brushing any cell highlights matching calls across ALL other cells.
- **Data**: Calls with signal_db, noise_db, freq_error, duration, emergency
- **Feasibility**: Medium (~300 LOC with brushing)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 92–140

### 9: **Daily Overview Treemap** — ✅ Implemented
- **File**: `web/daily-overview.html` (275 lines)
- **Status**: ✅ Done

### 10: **TG Sunburst** — ✅ Implemented
- **File**: `web/tg-sunburst.html` (218 lines)
- **Status**: ✅ Done

### 11: Talkgroup Correlation Matrix
- **Type**: Correlation heatmap + linked scatter plot
- **Library**: ECharts 5.5.0 (matrix coordinate)
- **Description**: Left panel = correlation heatmap (which TGs have correlated activity), right panel = scatter plot for clicked cell pair.
- **Data**: Daily call counts per TG across 30+ days
- **Feasibility**: High (~150 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 245–300

### 12: **Daily Overview Calendar Heatmap** — ✅ Implemented
- **File**: `web/calendar-heatmap.html` (208 lines)
- **Status**: ✅ Done

### 13: Signal Strength Waterfall Display
- **Type**: Waterfall (time-frequency spectrogram)
- **Library**: D3.js 7.9.0 + d3-waterfall plugin
- **Description**: Classic RF waterfall — time (scrolling), frequency (log scale), color = signal strength. Pan/zoom through 24h window.
- **Data**: Calls with freq, signal_db, noise_db, start_time, stop_time
- **Feasibility**: Medium (~200 LOC with d3-waterfall)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 343–393

### 14: Emergency Frequency Breakdown (Stacked Area)
- **Type**: Stacked area chart
- **Library**: ECharts 5.5.0
- **Description**: Emergency call rate per hour stacked by system. Shows overlapping emergency surges.
- **Data**: Emergency calls grouped by hour and system
- **Feasibility**: Very High (~40 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 396–430

### 15: Transmission Duration Dot Plot
- **Type**: Dot plot (horizontal jittered)
- **Library**: ECharts or D3.js
- **Description**: X-axis = duration, Y-axis = time-of-day, jittered dots per TG. Shows distribution shape not visible in histograms.
- **Data**: Calls with duration, start_time, tgid
- **Feasibility**: Medium (~100 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 433–479

### 16: Transcription Word Cloud
- **Type**: Word cloud (font size = frequency)
- **Library**: D3.js 7.9.0 + d3-cloud plugin
- **Description**: Interactive word cloud from call transcriptions. Filterable by time window and emergency flag.
- **Data**: Calls with transcription_text
- **Feasibility**: Medium (~150 LOC + d3-cloud 3KB)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 482–530

### 17: Frequency Spectrum Treemap
- **Type**: Treemap (frequency-banded)
- **Library**: D3.js 7.9.0
- **Description**: Treemap organized by frequency band (P25, Analog, SmartNet) and systems → TGs sized by call volume.
- **Data**: Calls with freq and analog flags
- **Feasibility**: Medium-High (~200 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 533–585

### 18: Unit Call Correlation Matrix
- **Type**: Matrix heatmap (unit ↔ TG correlation)
- **Library**: ECharts 5.5.0 (matrix coordinate)
- **Description**: Rows = TGs, cols = units, cell brightness = affiliation count. Reveals unit-to-TG patterns.
- **Data**: Calls with unit_ids[]
- **Feasibility**: Medium (~100 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 588–630

### 19: **Recorder Signal Gauges** — ✅ Implemented
- **File**: `web/recorder-gauges.html` (224 lines)
- **Status**: ✅ Done

### 20: Talkgroup Activity Slope Chart
- **Type**: Slope/bump chart (before/after trend)
- **Library**: Chart.js or D3.js
- **Description**: Lines connecting two time periods. Upward slope = TGs increasing; downward = declining. "What changed?" at a glance.
- **Data**: TG call counts for two time windows
- **Feasibility**: Very High (~60 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 685–730

### 21: Call Event Ridge Plot
- **Type**: Ridgeline plot (density curves with jittered overlap)
- **Library**: D3.js 7.9.0
- **Description**: Each TG = one KDE-smoothed density curve for call duration. Stacked vertically. Shows bimodal, long-tail distributions.
- **Data**: Calls with duration, tgid, date
- **Feasibility**: Medium (~200 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 774–820

### 22: **Talkgroup Bubble Scatter** — ✅ Implemented
- **File**: `web/bubble-scatter.html` (265 lines)
- **Status**: ✅ Done

### 23: Emissions Timeline (Stacked Bar Chart)
- **Type**: Stacked bar chart (event types)
- **Library**: Chart.js 4.x
- **Description**: Calls, unit_events, trunking_messages per day/week, stacked by type. Shows event relationship dynamics.
- **Data**: Calls, unit_events, trunking_messages counts by time
- **Feasibility**: Very High (~30 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 823–870

### 24: Unit Activity Clock
- **Type**: Circular Gantt chart
- **Library**: D3.js 7.9.0
- **Description**: Radial wheel — one band per unit, arc length = active time. Shows 24h rhythm across all units.
- **Data**: Unit events (on/off/join) with timestamps
- **Feasibility**: Medium (~250 LOC — manual trigonometry for radial layout)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 874–920

### 25: Decibel Radar Ring (Per-Call Profile)
- **Type**: Radar chart (per-call 5-axis profile)
- **Library**: ECharts 5.5.0
- **Description**: Radar chart of 5 call metrics: signal, noise, freq_error, errors, spikes. Compare calls by overlaying profiles.
- **Data**: Single call signal metrics
- **Feasibility**: Very High (~120 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 923–968

### 26: Barycenter Activity Map
- **Type**: Force-directed layout (pure spatial clustering)
- **Library**: D3.js 7.9.0
- **Description**: TGs positioned by co-occurrence frequency — no edges, just positions. Cluster reveals natural network regions.
- **Data**: TG co-occurrence adjacency (same as Concept 1, but no edges)
- **Feasibility**: Medium-High (~150 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 971–1016

### 27: Encryption Timeline (Band Chart)
- **Type**: Band chart (encrypted % over time)
- **Library**: ECharts 5.5.0
- **Description**: Band chart showing proportion of encrypted calls over time. Spikes during sensitive operations.
- **Data**: Calls grouped by minute with encrypted flag
- **Feasibility**: Very High (~60 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 1019–1065

### 28: Frequency Proximity Network
- **Type**: Frequency-constrained force layout
- **Library**: D3.js 7.9.0
- **Description**: Hybrid layout — TGs positioned on Y-axis proportional to frequency, pulled horizontally by co-occurrence. Best of both worlds.
- **Data**: TG frequencies + co-occurrence adjacency
- **Feasibility**: Medium-High (~200 LOC)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 1068–1115

### 29: Sentiment Timeline
- **Type**: Sentiment line chart (keyword-based)
- **Library**: ECharts or Chart.js
- **Description**: Time series of transcription sentiment scores (-1 to +1). Keywords colored green/red. Shows tense vs calm operations.
- **Data**: Calls with transcription_text (client-side keyword scoring)
- **Feasibility**: Medium (~150 LOC + 50 LOC keyword dict)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 1118–1163

### 30: TG Affiliation Heatmap
- **Type**: Time-evolving matrix heatmap
- **Library**: ECharts 5.5.0 (matrix coordinate)
- **Description**: 24-column matrix of TGs × hours, cells colored by affiliated units. Shows shifting unit-TG relationships during incidents.
- **Data**: Calls with unit_ids[] grouped by TG, hour
- **Feasibility**: Medium (~200 LOC with custom cell rendering)
- **Status**: ⬜ Not implemented
- **Full spec**: `web/CONCEPTS-v7.md` lines 1166–1210

---

## Summary by Library

### ECharts (5.5.0) — Already Loaded
| Page | File | Lines | Chart Type |
|------|------|-------|------------|
| **✅ Done** | `bubble-scatter.html` | 265 | Scatter + visualMap |
| **✅ Done** | `recorder-gauges.html` | 224 | Gauge (multiple) |
| **✅ Done** | `tg-sunburst.html` | 218 | Sunburst |
| **✅ Done** | `calendar-heatmap.html` | 208 | Calendar heatmap |

### ECharts Remaining
| # | Concept | Type | Feasibility | Est LOC |
|---|---------|------|-------------|---------|
| 2 | Hex bin heatmap | ECharts custom | High | ~50 |
| 3 | Emergency Sankey | ECharts Sankey | Med-High | ~150 |
| 4 | Encryption Radar | ECharts Radar | Very High | ~100 |
| 5 | Signal Timeline | Scatter + regression | High | ~70 |
| 6 | Duration Distribution | Histogram + ECDF | Very High | ~60 |
| 11 | Correlation Matrix | ECharts matrix | High | ~150 |
| 14 | Emergency Stacked Area | Stacked area | Very High | ~40 |
| 17 | Frequency Spectrum Treemap | Treemap | Med-High | ~200 |
| 18 | Unit TG Correlation | Matrix heatmap | Medium | ~100 |
| 20 | Slope Chart | Slope/Bump | Very High | ~60 |
| 23 | Emissions Timeline | Stacked bar | Very High | ~30 |
| 25 | Decibel Radar Ring | Radar (per-call) | Very High | ~120 |
| 27 | Encryption Band | Band chart | Very High | ~60 |

### D3.js (7.9.0) — Already Loaded
| # | Concept | Type | Feasibility | Est LOC |
|---|---------|------|-------------|---------|
| 1 | Co-activation Network | Force-directed graph | High | ~200 |
| 8 | Signal SPLOM | Scatterplot matrix | Medium | ~300 |
| 11 | Correlation Matrix (alt) | Correlation + scatter | High | ~150 |
| 16 | Word Cloud | Word cloud (+ d3-cloud 3KB) | Medium | ~150 |
| 21 | Ridge Plot | Ridgeline density | Medium | ~200 |
| 24 | Unit Activity Clock | Circular Gantt | Medium | ~250 |
| 26 | Barycenter Map | Force layout (no edges) | Med-High | ~150 |
| 28 | Frequency Network | Freq-constrained force | Med-High | ~200 |

### D3.js Remaining with Plugins
| # | Concept | Type + Plugin | Est LOC |
|---|---------|--------------|---------|
| 7 | Parallel Coordinates | + d3.parcoords plugin | ~50-200 |
| 13 | Waterfall | + d3-waterfall plugin | ~200 |
| 16 | Word Cloud | + d3-cloud (3KB) | ~150 |

### Chart.js (4.x) — Already Loaded
| # | Concept | Type | Feasibility | Est LOC |
|---|---------|------|-------------|---------|
| 20 | Slope Chart | Bar/line combo | Very High | ~60 |
| 23 | Emissions Timeline | Stacked bar | Very High | ~30 |

---

## Prioritized Implementation Order

### Tier 1: Very High Feasibility, Under 100 LOC (do these first)
1. **#6** — Duration Distribution histogram (~60 LOC)
2. **#23** — Emissions stacked bar (~30 LOC)
3. **#14** — Emergency Stacked Area (~40 LOC)
4. **#27** — Encryption Band chart (~60 LOC)
5. **#20** — Slope Chart (~60 LOC)
6. **#4** — Encryption Radar chart (~100 LOC)
7. **#3** — Emergency Sankey (~150 LOC)

### Tier 2: Medium Feasibility (50–200 LOC)
8. **#2** — Hex bin heatmap (~50 LOC)
9. **#5** — Signal Quality Timeline (~70 LOC)
10. **#25** — Decibel Radar Ring (~120 LOC)
11. **#8** — SPLOM (~300 LOC)
12. **#11** — Correlation Matrix (~150 LOC)
13. **#28** — Frequency Proximity Network (~200 LOC)
14. **#1** — Co-activation Network (~200 LOC)

### Tier 3: Requires Plugin or Complex Layout (200+ LOC)
15. **#17** — Frequency Spectrum Treemap (~200 LOC)
16. **#7** — Parallel Coordinates (~200 LOC + plugin)
17. **#21** — Ridge Plot (~200 LOC)
18. **#16** — Word Cloud (~150 LOC + plugin)
19. **#26** — Barycenter Map (~150 LOC)
20. **#24** — Unit Activity Clock (~250 LOC)
21. **#29** — Sentiment Timeline (~150 LOC)
22. **#13** — Waterfall Display (~200 LOC + plugin)
23. **#30** — TG Affiliation Heatmap (~200 LOC)
24. **#18** — Unit TG Correlation (~100 LOC)

---

## Implementation Notes

### Common patterns across all pages (established by existing pages):
- Include `auth.js?v=3` first — adds the stored API key to same-origin `/api/` fetches, mints tickets for `EventSource`, prompts for a key only when needed (`trAuth.mediaUrl(url)` for `<audio>`, `await trAuth.ticketUrl(url)` for WebSocket)
- Include `theme-config.js` — CSS variable definitions (var(--bg), --text, --tile-bg, etc.)
- Include `theme-engine.js?v=2` at bottom — applies themes, persists user preferences
- Use CSS custom variables for ALL colors — do NOT use hardcoded #fff
- Chart must respond to window resize (ResizeObserver + resize() call)
- Data fetched via: `fetch('/api/v1/...')` — auth.js adds the key; render API strings with `textContent`; use `Promise.allSettled` when mixing endpoints restricted keys may be denied
- Meta card tags required: card-title, card-description, card-order

### API endpoints used across concepts:
- `GET /api/v1/systems` — all concepts need this to map system_id → name
- `GET /api/v1/calls?system_id={id}&limit={N}&sort=-start_time` — most concepts
- `GET /api/v1/talkgroups?system_id={id}` — concepts needing alpha tags
- `GET /api/v1/recorders` — recorder gauges only
- `GET /api/v1/stats` — emergency timeline, signal quality concepts
- `POST /query` (custom SQL) — concepts needing aggregation not available via GET

### Libraries already loaded in existing pages:
- **ECharts 5.5.0**: loaded in analytics.html, traffic-patterns.html, emergency-log.html
- **D3.js 7.9.0**: loaded in stream-graph.html
- **Chart.js 4.x**: loaded in talkgroup-research.html
- **All concepts use existing libraries** — no new dependencies required
