# OmniTrunker Live Status Page — Design

**Date:** 2026-03-02
**Inspiration:** tr-web's OmniTrunker tab (taclane)

## Overview

A real-time live status page showing active voice channels and a scrolling unit activity feed. Two-panel vertical layout. Theme-integrated, uses existing SSE infrastructure. All data arrives pre-denormalized (TG names, unit aliases embedded in payloads) — no additional API calls needed.

## Layout

### Top Panel: Active Voice Channels

Table of currently active calls. Rows appear on `call_start`, fade out over ~5s after `call_end`, then remove.

**Columns:**
| Column | Source Field | Notes |
|--------|-------------|-------|
| System | system_name or sys_name | From call_start payload |
| Channel | freq | Format as MHz (divide by 1e6, 4 decimal places) |
| Talkgroup | tgid | Numeric |
| Alpha Tag | tg_alpha_tag | Talkgroup name |
| Source | unit | Initiating unit ID |
| Unit Alias | unit_alpha_tag | Unit name |
| Elapsed | (computed) | Live-updating timer from start_time, requestAnimationFrame |
| Mode | audio_type | FDMA, TDMA-0, TDMA-1, Analog |
| Encryption | encrypted | Red badge if true, hidden if false |

**Behavior:**
- `call_start` → insert row, start elapsed timer
- `call_end` → add fade-out CSS class, remove row after 5s
- Emergency calls → highlighted row (accent border or background tint)
- System filter dropdown ("All Systems" default) filters both panels
- Empty state: subtle "No active calls" message

### Bottom Panel: Unit Activity Feed

Reverse-chronological scrolling feed of all unit events.

**Columns:**
| Column | Source Field |
|--------|-------------|
| Time | time / start_time | Format as HH:MM:SS |
| System | system_name |
| Site ID | site_id |
| Unit ID | unit_id |
| Unit Alias | unit_alpha_tag |
| Type | event_type | Color-coded badge |
| Talkgroup | tgid |
| TG Alpha | tg_alpha_tag |

**Type Badge Colors:**
| Event Type | Badge Color | Maps From |
|------------|-------------|-----------|
| GRANT | Blue/teal | unit_event:call or call_start |
| AFFILIATION | Green | unit_event:join |
| REGISTRATION | Yellow/amber | unit_event:on |
| DEREGISTRATION | Red/orange | unit_event:off |
| ACKNOWLEDGE | Purple | unit_event:ackresp |
| DATA_GRANT | Gray/muted | unit_event:data |
| LOCATION | Cyan | unit_event:location |
| CALL END | Muted | unit_event:end or call_end |
| SIGNAL | Magenta | unit_event:signal |

**Controls:**
- Buffer size selector: 50 / 100 / 200 / 500 rows (default 200)
- Type filter dropdown: All Types, or pick specific types
- Auto-scroll toggle (default on)
- Clear button

## Data Flow

Single SSE connection to `/api/v1/events/stream` with **no server-side type filter**. All events arrive; client-side JavaScript filters via the type dropdown. This avoids reconnecting when the user changes filters.

**Event routing:**
- `call_start` → Active Voice Channels (add row) + Unit Activity (as GRANT)
- `call_end` → Active Voice Channels (fade + remove) + Unit Activity (as CALL END)
- `unit_event:*` → Unit Activity feed only
- Other event types (recorder_update, rate_update, trunking_message, console) → ignored

**Reconnect:** `Last-Event-ID` handled automatically by auth.js EventSource wrapper. On reconnect, server replays missed events from 60s ring buffer.

## Tech

- **Integration:** theme-config.js (CSS vars), auth.js (token), theme-engine.js (nav header)
- **Dependencies:** None — vanilla JS, no D3/charting libraries
- **Page registration:** meta tags for nav auto-discovery
- **Suggested card-order:** 3 (after Event Horizon and Live Events)

## File

`web/omnitrunker.html` — single file, theme-integrated.
