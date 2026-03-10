# OmniTrunker Multi-Select Filter Dropdowns — Design

**Goal:** Replace the single-select System and Unit Activity Type dropdowns in omnitrunker.html with inline multi-select checkbox dropdowns, allowing users to pick one or more systems and one or more event types simultaneously.

**Architecture:** Build a reusable `MultiSelectDropdown` widget in vanilla JS/CSS within omnitrunker.html. Replace both `<select>` elements with instances of this widget. Persist selections to localStorage. Migrate existing single-value system filter persistence to array format.

## Component: MultiSelectDropdown

- Button displays summary text + chevron
- Click opens a floating checkbox list positioned below the button
- Top row: "All" / "None" toggle buttons
- Each item: checkbox + label
- Click outside or Escape closes the list
- Styled to match existing `<select>` elements (same font, colors, border, radius)
- `z-index` above panels so list doesn't get clipped

### Summary Label Logic

- All selected → custom "all" label (e.g., "All Systems", "All Types")
- One selected → that item's label
- Multiple but not all → count + noun (e.g., "2 systems", "3 types")
- None selected → same as all (treat empty selection as "show everything")

## System Filter

- Location: toolbar (replaces `<select id="system-filter">`)
- Items: dynamically populated as systems are discovered
- Default: all selected
- Filters: Active Voice Channels table AND Unit Activity table
- Persistence: `localStorage` key `omni-system-filter` (migrate from single string to JSON array)

## Type Filter

- Location: activity controls bar (replaces `<select id="type-filter">`)
- Items: 9 hardcoded types (grant, affiliation, registration, deregistration, acknowledge, data, location, call-end, signal)
- Default: all selected
- Filters: Unit Activity table only
- Persistence: `localStorage` key `omni-type-filter` (new)

## Filtering Logic

- Both filters AND together: a row must match a selected system AND a selected type
- New rows checked on arrival in `addActivityRow()`
- Existing DOM rows shown/hidden when filter selection changes
- Active Voice Channels table filtered by system selection only

## Out of Scope

- No changes to audio, TG selection, frequency monitor, or SSE subscriptions
- No search box (item counts too small to warrant it)
