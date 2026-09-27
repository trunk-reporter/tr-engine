// ═══════════════════════════════════════════════════════════════
// signal-flow-data.js — tr-engine data adapter for Signal Flow
// ═══════════════════════════════════════════════════════════════
//
// Data pipeline:
//
//   1. BACKFILL  — POST /query with server-side date_bin() bucketing
//                  4 parallel queries (calls, airtime, units, affiliations),
//                  ~360 rows each for a 3-hour window / 30-second buckets
//
//   2. ROSTER   — GET /unit-affiliations snapshot, fetched in parallel
//                  with backfill. Stamped into all backfill buckets as
//                  a flat baseline (best approximation of historical state).
//                  Live SSE join/off events track actual changes from
//                  that point forward.
//
//   3. LIVE     — GET /events/stream (SSE) accumulates into the
//                  current bucket, rolls forward every BUCKET_SEC
//
// Gapless handoff strategy:
//   - Start SSE connection BEFORE backfill completes
//   - Buffer incoming SSE events with timestamps
//   - When backfill resolves, merge: discard SSE events that fall
//     within already-backfilled buckets, keep the rest
//   - From that point forward, SSE events feed directly into the
//     current bucket
//
// ═══════════════════════════════════════════════════════════════

export const BUCKET_SEC = 300;
export const WINDOW_SEC = 3 * 3600;
const MAX_BUCKETS = Math.ceil(WINDOW_SEC / BUCKET_SEC);

// ═══════════════════════════════════════════════════════════════
// TYPES
// ═══════════════════════════════════════════════════════════════

/**
 * @typedef {Object} Bucket
 * @property {number} time       - Bucket start as Unix ms
 * @property {Object<string, number>} calls        - tgid → call count
 * @property {Object<string, number>} airtime      - tgid → seconds of audio
 * @property {Object<string, number>} units        - tgid → Set<unitId>.size (stored as number)
 * @property {Object<string, number>} roster       - tgid → affiliated unit count (gauge)
 * @property {Object<string, number>} affiliations - tgid → join event count
 */

/**
 * @typedef {Object} TalkgroupInfo
 * @property {number} tgid
 * @property {string} alpha_tag
 * @property {string} group
 * @property {string} tag
 * @property {string} color      - Assigned by the visualization
 */

// ═══════════════════════════════════════════════════════════════
// STATE
// ═══════════════════════════════════════════════════════════════

/** @type {Bucket[]} */
let timeSeries = [];

/** @type {string[]} tgid keys as strings for the stack */
let tgKeys = [];

/** @type {Map<string, TalkgroupInfo>} */
let talkgroupMap = new Map();

/** @type {EventSource|null} */
let eventSource = null;

/** SSE events buffered during backfill */
let sseBuffer = [];
let isBackfilling = true;
let lastBackfillEdge = 0; // Unix ms — end of the last backfilled bucket

/** Live bucket accumulator — rolls forward every BUCKET_SEC */
let liveBucket = null;
let liveBucketEdge = 0;

/** Per-tgid unit sets for the current live bucket (need Set for distinct count) */
let liveUnitSets = {};

/**
 * Live roster state — tracks current talkgroup affiliations.
 * Map<tgid_string, Set<unitId>> — mirrors /unit-affiliations endpoint.
 * Seeded from REST on init, kept current from SSE join/off events.
 * Snapshotted into each live bucket as roster[tgid] = set.size.
 */
let rosterState = new Map();

/** Reverse lookup: unitId → tgid (for moving units between talkgroups) */
let unitTgMap = new Map();

/** Callback: Signal Flow calls this to get notified of updates */
let onUpdate = null;

// ═══════════════════════════════════════════════════════════════
// PUBLIC API
// ═══════════════════════════════════════════════════════════════

/**
 * Initialize the data adapter.
 *
 * @param {Object} opts
 * @param {string} opts.apiBase     - API base URL (e.g., '/api/v1')
 * @param {number} opts.systemId    - System database ID to monitor
 * @param {string} [opts.sysid]     - P25 SYSID for SSE filtering (optional)
 * @param {Function} opts.onUpdate  - Called with (timeSeries, tgKeys, talkgroupMap) on each update
 * @param {Function} [opts.onTalkgroups] - Called once with talkgroup metadata
 */
export async function init(opts) {
  const { apiBase, systemId, sysid } = opts;
  onUpdate = opts.onUpdate;

  // ── Step 1: Fetch talkgroup metadata ─────────────────────
  const talkgroups = await fetchTalkgroups(apiBase, systemId);
  tgKeys = talkgroups.map(tg => String(tg.tgid));
  talkgroups.forEach(tg => talkgroupMap.set(String(tg.tgid), tg));

  if (opts.onTalkgroups) opts.onTalkgroups(talkgroups);

  // ── Step 2: Start SSE BEFORE backfill (buffer events) ────
  startSSE(apiBase, systemId, sysid);

  // ── Step 3: Backfill historical data + fetch roster ──────
  const now = new Date();
  const start = new Date(now.getTime() - WINDOW_SEC * 1000);

  // Align to bucket boundaries
  const startAligned = alignToBucket(start);
  const endAligned = alignToBucket(now);

  // Run backfill and roster fetch in parallel. Either may be unavailable
  // (backfill needs an admin key; the roster is denied to restricted keys),
  // so a failure of one never blocks the other or the live stream.
  const [backfillRes, rosterRes] = await Promise.allSettled([
    backfill(apiBase, systemId, startAligned, endAligned),
    fetchRoster(apiBase, systemId),
  ]);
  if (backfillRes.status === 'rejected') console.warn('[signal-flow-data] Backfill failed, starting from live data:', backfillRes.reason);
  if (rosterRes.status === 'rejected') console.warn('[signal-flow-data] Roster fetch failed, starting empty:', rosterRes.reason);

  // Stamp roster snapshot into all backfill buckets.
  // Roster is a gauge — the current snapshot is the best approximation
  // of historical state since affiliations change slowly. Live SSE
  // events will track actual changes from this point forward.
  const rosterSnapshot = snapshotRoster();
  for (const bucket of timeSeries) {
    Object.assign(bucket.roster, rosterSnapshot);
  }

  lastBackfillEdge = endAligned.getTime();
  isBackfilling = false;

  // ── Step 4: Merge buffered SSE events ────────────────────
  drainSSEBuffer();

  // ── Step 5: Start live bucket roller ─────────────────────
  liveBucketEdge = endAligned.getTime();
  startLiveBucketRoller();

  // Initial render
  notify();
}

/** Clean shutdown */
export function destroy() {
  if (eventSource) {
    eventSource.close();
    eventSource = null;
  }
}

/** Get current state (for external access) */
export function getState() {
  return { timeSeries, tgKeys, talkgroupMap };
}

// ═══════════════════════════════════════════════════════════════
// BACKFILL — POST /query
// ═══════════════════════════════════════════════════════════════
//
// Four parallel queries, one per metric. Each uses date_bin()
// for server-side bucket alignment. The partition indexes on
// (system_id, start_time) / (system_id, tgid, time) ensure
// these hit the right monthly partitions efficiently.
//
// Query result shape: { columns: [...], rows: [[...], ...] }
// We convert to column-indexed lookups for fast assembly.

const EMPTY_RESULT = { columns: [], rows: [] };
let queryAllowed = null;  // Promise<boolean>, decided once per page

// POST /query needs an API key with the admin scope. Only ask when this
// page's own engine is the target and the stored key is an admin key;
// otherwise skip the historical backfill (live data still fills in).
function canQuery(apiBase) {
  if (!queryAllowed) {
    queryAllowed = (async () => {
      const auth = window.trAuth;
      if (!auth) return false;
      if (auth.isSameOriginAPI && !auth.isSameOriginAPI(`${apiBase}/query`)) return false;
      try { await auth.ready(); } catch (e) { return false; }
      return auth.hasScope('admin');
    })();
  }
  return queryAllowed;
}

async function query(apiBase, sql, params, limit = 10000) {
  if (!(await canQuery(apiBase))) return EMPTY_RESULT;
  const resp = await fetch(`${apiBase}/query`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sql, params, limit }),
  });
  if (!resp.ok) {
    // No key, a key without admin, or /query disabled: degrade gracefully
    if (resp.status === 401 || resp.status === 403) return EMPTY_RESULT;
    throw new Error(`Query failed: ${resp.status} ${await resp.text()}`);
  }
  return resp.json();
}

async function backfill(apiBase, systemId, start, end) {
  const startISO = start.toISOString();
  const endISO = end.toISOString();
  const interval = `${BUCKET_SEC} seconds`;

  // ── Call Rate ────────────────────────────────────────────
  // Hits idx_calls_system_tgid_start → partition pruning on start_time
  const callRateSQL = `
    SELECT
      extract(epoch FROM date_bin('${interval}', start_time, '2000-01-01'::timestamptz))::bigint AS bucket_epoch,
      tgid,
      count(*)::int AS val
    FROM calls
    WHERE system_id = $1
      AND start_time >= $2::timestamptz
      AND start_time <  $3::timestamptz
    GROUP BY 1, tgid
    ORDER BY 1`;

  // ── Airtime ──────────────────────────────────────────────
  // Same index path, sums duration instead of counting
  const airtimeSQL = `
    SELECT
      extract(epoch FROM date_bin('${interval}', start_time, '2000-01-01'::timestamptz))::bigint AS bucket_epoch,
      tgid,
      round(sum(duration)::numeric, 1)::real AS val
    FROM calls
    WHERE system_id = $1
      AND start_time >= $2::timestamptz
      AND start_time <  $3::timestamptz
      AND duration IS NOT NULL
    GROUP BY 1, tgid
    ORDER BY 1`;

  // ── Active Units ─────────────────────────────────────────
  // Counts DISTINCT unit IDs per bucket per talkgroup using
  // the unit_ids int[] column on calls (GIN-indexed).
  const unitsSQL = `
    SELECT
      extract(epoch FROM date_bin('${interval}', c.start_time, '2000-01-01'::timestamptz))::bigint AS bucket_epoch,
      c.tgid,
      count(DISTINCT u)::int AS val
    FROM calls c, unnest(c.unit_ids) AS u
    WHERE c.system_id = $1
      AND c.start_time >= $2::timestamptz
      AND c.start_time <  $3::timestamptz
    GROUP BY 1, c.tgid
    ORDER BY 1`;

  // ── Affiliations ─────────────────────────────────────────
  // unit_events with event_type='join' — these are talkgroup
  // affiliation events. Hits idx_unit_events_system_tgid_time
  // (partial index WHERE tgid IS NOT NULL covers our filter).
  const affSQL = `
    SELECT
      extract(epoch FROM date_bin('${interval}', "time", '2000-01-01'::timestamptz))::bigint AS bucket_epoch,
      tgid,
      count(*)::int AS val
    FROM unit_events
    WHERE system_id = $1
      AND event_type = 'join'
      AND "time" >= $2::timestamptz
      AND "time" <  $3::timestamptz
      AND tgid IS NOT NULL
    GROUP BY 1, tgid
    ORDER BY 1`;

  // Fire all four in parallel
  const [callRes, airRes, unitRes, affRes] = await Promise.all([
    query(apiBase, callRateSQL, [systemId, startISO, endISO]),
    query(apiBase, airtimeSQL, [systemId, startISO, endISO]),
    query(apiBase, unitsSQL, [systemId, startISO, endISO]),
    query(apiBase, affSQL, [systemId, startISO, endISO]),
  ]);

  // ── Assemble into timeSeries ─────────────────────────────
  // Build a map of bucketEpoch → sparse metric objects from each result,
  // then iterate over the full bucket range filling in zeros.

  const callMap = indexResult(callRes);
  const airMap = indexResult(airRes);
  const unitMap = indexResult(unitRes);
  const affMap = indexResult(affRes);

  timeSeries = [];
  const startEpoch = Math.floor(start.getTime() / 1000);
  const endEpoch = Math.floor(end.getTime() / 1000);

  for (let epoch = startEpoch; epoch < endEpoch; epoch += BUCKET_SEC) {
    const bucket = emptyBucket(epoch * 1000);

    // Fill from query results (sparse — only buckets with data appear)
    const cRow = callMap.get(epoch);
    const aRow = airMap.get(epoch);
    const uRow = unitMap.get(epoch);
    const fRow = affMap.get(epoch);

    if (cRow) for (const [tgid, val] of cRow) bucket.calls[tgid] = val;
    if (aRow) for (const [tgid, val] of aRow) bucket.airtime[tgid] = val;
    if (uRow) for (const [tgid, val] of uRow) bucket.units[tgid] = val;
    if (fRow) for (const [tgid, val] of fRow) bucket.affiliations[tgid] = val;

    timeSeries.push(bucket);
  }

  // Trim to max buckets
  if (timeSeries.length > MAX_BUCKETS) {
    timeSeries = timeSeries.slice(-MAX_BUCKETS);
  }
}

/**
 * Convert /query result into Map<bucketEpoch, Map<tgid, value>>
 * Expected columns: [bucket_epoch, tgid, val]
 */
function indexResult(result) {
  const map = new Map();
  for (const row of result.rows) {
    const [epoch, tgid, val] = row;
    if (!map.has(epoch)) map.set(epoch, new Map());
    map.get(epoch).set(String(tgid), val);
  }
  return map;
}

// ═══════════════════════════════════════════════════════════════
// LIVE — SSE via GET /events/stream
// ═══════════════════════════════════════════════════════════════
//
// We subscribe to:
//   call_end          → call rate + airtime
//   unit_event:call   → active unit tracking (distinct units per tg per bucket)
//   unit_event:join   → affiliations count + roster state tracking
//   unit_event:off    → roster state tracking (unit deregistration)
//
// Active units are tracked from unit_event:call (unit keyed up on a talkgroup)
// rather than call_end, because the SSE call_end payload does not include
// the units array. unit_event:call gives real-time per-transmission tracking.
//
// Roster is a gauge maintained from join/off events:
//   join: move unit from old tg to new tg in rosterState
//   off:  remove unit from rosterState entirely
// Each mutation triggers a roster snapshot into the live bucket.

function startSSE(apiBase, systemId, sysid) {
  const params = new URLSearchParams();
  params.set('systems', String(systemId));
  params.set('types', 'call_end,unit_event:call,unit_event:join,unit_event:off');

  const url = `${apiBase}/events/stream?${params}`;
  eventSource = new EventSource(url);

  // call_end → call rate + airtime
  eventSource.addEventListener('call_end', (e) => {
    const call = JSON.parse(e.data);
    const event = {
      type: 'call_end',
      time: new Date(call.stop_time || call.start_time).getTime(),
      tgid: String(call.tgid),
      duration: call.duration || 0,
    };

    if (isBackfilling) {
      sseBuffer.push(event);
    } else {
      applyEvent(event);
    }
  });

  // unit_event → active units, affiliations, roster tracking
  eventSource.addEventListener('unit_event', (e) => {
    const evt = JSON.parse(e.data);

    // unit_event:call — unit keyed up on a talkgroup
    if (evt.event_type === 'call' && evt.tgid) {
      const event = {
        type: 'unit_call',
        time: new Date(evt.time).getTime(),
        tgid: String(evt.tgid),
        unitId: evt.unit_id,
      };

      if (isBackfilling) {
        sseBuffer.push(event);
      } else {
        applyEvent(event);
      }
    }

    // unit_event:join — affiliation + roster
    if (evt.event_type === 'join' && evt.tgid) {
      const event = {
        type: 'join',
        time: new Date(evt.time).getTime(),
        tgid: String(evt.tgid),
        unitId: evt.unit_id,
      };

      if (isBackfilling) {
        sseBuffer.push(event);
      } else {
        applyEvent(event);
      }
    }

    // unit_event:off — roster removal
    if (evt.event_type === 'off') {
      const event = {
        type: 'off',
        time: new Date(evt.time).getTime(),
        unitId: evt.unit_id,
      };

      if (isBackfilling) {
        sseBuffer.push(event);
      } else {
        applyEvent(event);
      }
    }
  });

  eventSource.onopen = () => {
    if (window.ehSetNetworkStatus) window.ehSetNetworkStatus('connected');
  };
  eventSource.onerror = (err) => {
    console.warn('[signal-flow-data] SSE error, will auto-reconnect:', err);
    if (window.ehSetNetworkStatus) window.ehSetNetworkStatus('connecting');
    // EventSource auto-reconnects. On reconnect, pass Last-Event-ID
    // for gapless recovery (server buffers 60s of events).
  };
}

/**
 * After backfill completes, drain buffered SSE events.
 * Discard any that fall within already-backfilled time range.
 */
function drainSSEBuffer() {
  for (const event of sseBuffer) {
    if (event.time >= lastBackfillEdge) {
      applyEvent(event);
    }
    // else: already covered by backfill, discard
  }
  sseBuffer = [];
}

/**
 * Apply a single SSE-derived event to the current live bucket.
 */
function applyEvent(event) {
  ensureLiveBucket(event.time);

  if (event.type === 'call_end') {
    const tg = event.tgid;
    liveBucket.calls[tg] = (liveBucket.calls[tg] || 0) + 1;
    liveBucket.airtime[tg] = (liveBucket.airtime[tg] || 0) + event.duration;
  }

  if (event.type === 'unit_call') {
    // Track distinct units per talkgroup via Set
    const tg = event.tgid;
    if (!liveUnitSets[tg]) liveUnitSets[tg] = new Set();
    liveUnitSets[tg].add(event.unitId);
    liveBucket.units[tg] = liveUnitSets[tg].size;
  }

  if (event.type === 'join') {
    const tg = event.tgid;
    liveBucket.affiliations[tg] = (liveBucket.affiliations[tg] || 0) + 1;

    // Update roster state: move unit from old talkgroup to new one
    if (event.unitId != null) {
      const oldTg = unitTgMap.get(event.unitId);
      if (oldTg && oldTg !== tg && rosterState.has(oldTg)) {
        rosterState.get(oldTg).delete(event.unitId);
      }
      if (!rosterState.has(tg)) rosterState.set(tg, new Set());
      rosterState.get(tg).add(event.unitId);
      unitTgMap.set(event.unitId, tg);
    }

    // Snapshot roster into live bucket
    Object.assign(liveBucket.roster, snapshotRoster());
  }

  if (event.type === 'off') {
    // Unit deregistered — remove from roster
    if (event.unitId != null) {
      const oldTg = unitTgMap.get(event.unitId);
      if (oldTg && rosterState.has(oldTg)) {
        rosterState.get(oldTg).delete(event.unitId);
      }
      unitTgMap.delete(event.unitId);

      // Snapshot roster into live bucket
      Object.assign(liveBucket.roster, snapshotRoster());
    }
  }

  notify();
}

// ═══════════════════════════════════════════════════════════════
// LIVE BUCKET MANAGEMENT
// ═══════════════════════════════════════════════════════════════

/**
 * Ensure the live bucket covers the given timestamp.
 * If the event is beyond the current bucket edge, roll forward:
 * finalize the current bucket into timeSeries and start a new one.
 */
function ensureLiveBucket(eventTimeMs) {
  if (!liveBucket) {
    liveBucket = emptyBucket(liveBucketEdge);
    Object.assign(liveBucket.roster, snapshotRoster());
    liveUnitSets = {};
  }

  // Roll forward if event is past the current bucket window
  while (eventTimeMs >= liveBucketEdge + BUCKET_SEC * 1000) {
    // Finalize current bucket into timeSeries
    timeSeries.push(liveBucket);
    if (timeSeries.length > MAX_BUCKETS) timeSeries.shift();

    // Advance
    liveBucketEdge += BUCKET_SEC * 1000;
    liveBucket = emptyBucket(liveBucketEdge);
    // Carry roster state forward — it's a gauge, not a counter
    Object.assign(liveBucket.roster, snapshotRoster());
    liveUnitSets = {};
  }
}

/**
 * Periodic roller: even if no events arrive, we need to push
 * empty buckets so the visualization keeps scrolling forward.
 * Runs every BUCKET_SEC.
 */
function startLiveBucketRoller() {
  setInterval(() => {
    ensureLiveBucket(Date.now());
    notify();
  }, BUCKET_SEC * 1000);
}

// ═══════════════════════════════════════════════════════════════
// TALKGROUP METADATA
// ═══════════════════════════════════════════════════════════════

async function fetchTalkgroups(apiBase, systemId) {
  const resp = await fetch(
    `${apiBase}/talkgroups?system_id=${systemId}&limit=25&sort=-calls_24h`
  );
  if (!resp.ok) throw new Error(`Talkgroup fetch failed: ${resp.status}`);
  const data = await resp.json();
  return data.talkgroups;
}

/**
 * Fetch current affiliation state from GET /unit-affiliations.
 * Seeds rosterState and unitTgMap for live tracking.
 */
async function fetchRoster(apiBase, systemId) {
  const resp = await fetch(
    `${apiBase}/unit-affiliations?system_id=${systemId}&status=affiliated&limit=1000`
  );
  if (!resp.ok) {
    console.warn(`[signal-flow-data] Roster fetch failed: ${resp.status}, starting empty`);
    return;
  }
  const data = await resp.json();

  // Build roster state from affiliation list
  rosterState.clear();
  unitTgMap.clear();
  for (const aff of (data.affiliations || [])) {
    const tg = String(aff.tgid);
    if (!rosterState.has(tg)) rosterState.set(tg, new Set());
    rosterState.get(tg).add(aff.unit_id);
    unitTgMap.set(aff.unit_id, tg);
  }

  console.log(`[signal-flow-data] Roster seeded: ${unitTgMap.size} units across ${rosterState.size} talkgroups`);
}

// ═══════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════

function emptyBucket(timeMs) {
  const bucket = { time: timeMs, calls: {}, airtime: {}, units: {}, roster: {}, affiliations: {} };
  // Initialize all known talkgroups to 0 so the stack layout is stable
  for (const k of tgKeys) {
    bucket.calls[k] = 0;
    bucket.airtime[k] = 0;
    bucket.units[k] = 0;
    bucket.roster[k] = 0;
    bucket.affiliations[k] = 0;
  }
  return bucket;
}

/**
 * Snapshot current rosterState into a plain { tgid: count } object.
 * Used to stamp roster values into buckets.
 */
function snapshotRoster() {
  const snapshot = {};
  for (const k of tgKeys) {
    const set = rosterState.get(k);
    snapshot[k] = set ? set.size : 0;
  }
  return snapshot;
}

function alignToBucket(date) {
  const epoch = Math.floor(date.getTime() / 1000);
  const aligned = Math.floor(epoch / BUCKET_SEC) * BUCKET_SEC;
  return new Date(aligned * 1000);
}

function notify() {
  if (onUpdate) {
    // Include the live bucket as the last entry for rendering
    const series = liveBucket
      ? [...timeSeries, liveBucket]
      : timeSeries;
    onUpdate(series, tgKeys, talkgroupMap);
  }
}
