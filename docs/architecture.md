# Architecture: Technical Flow-Through

How tr-engine works from startup to shutdown. For what exists and how to use it, see [CLAUDE.md](../CLAUDE.md).

## 1. Startup Sequence

`cmd/tr-engine/main.go` boots the application in this order:

```
1. Parse CLI flags (--listen, --log-level, --database-url, etc.)
2. Load config: .env file → env vars → CLI overrides (config.Load)
3. TR auto-discovery (if TR_DIR set): read config.json + docker-compose.yaml
4. Validate config (cfg.Validate)
5. Initialize zerolog logger
6. Create shutdown context: signal.NotifyContext(SIGINT, SIGTERM)
7. Connect to PostgreSQL (database.Connect)
8. InitSchema — apply schema.sql on fresh DB (no-op if tables exist)
9. Migrate — run incremental migrations (skip already-applied)
9a. Auth setup (internal/database + main.go):
      - one-time legacy import of AUTH_TOKEN/WRITE_TOKEN (data_fixups
        "import-legacy-auth", once per database)
      - one WARN per removed auth variable still set
      - ticket secret: generate 32 random bytes into auth_settings on first start
      - bootstrap admin key (data_fixups "bootstrap-admin-key", once per
        database): if no active admin key exists, create one and print it
        to stderr; on later starts only an ERROR if none is active
10. Initialize audio storage (storage.New)
11. Start storage background services (pruner, reconciler)
12. Start AsyncUploader if S3 async mode (2 workers, 500 queue)
13. Connect MQTT client (if MQTT_BROKER_URL set)
14. Build transcription provider (if STT_PROVIDER set)
15. Create ingest Pipeline (NewPipeline)
16. Start Pipeline (load identity cache → warmup gate → goroutines)
17. Wire MQTT message handler → Pipeline.HandleMessage
18. Import talkgroup/unit CSVs from TR discovery
19. Start FileWatcher (if WATCH_DIR set)
20. Create HTTP server (api.NewServer) and wire all routes
21. Start HTTP server in background goroutine
22. Log "tr-engine ready"
23. Block on shutdown signal or server error
```

## 2. Configuration Layering

Priority (highest wins): **CLI flags > environment variables > .env file > defaults**

```
main.go                      config.go
┌──────────────────┐         ┌──────────────────────────┐
│ flag.Parse()     │         │ godotenv.Load(envFile)    │
│ overrides struct │───────> │ env.Parse(&Config{})      │
│                  │         │ Apply overrides on top    │
└──────────────────┘         │ cfg.Validate()            │
                             └──────────────────────────┘
```

`config.Load()`:
1. `godotenv.Load(envFile)` — loads `.env` (silent if missing)
2. `env.Parse(&cfg)` — struct tags with `envDefault` provide defaults
3. CLI overrides applied field-by-field (non-empty strings only)

The removed auth variables (`AUTH_ENABLED`, `AUTH_TOKEN`, `WRITE_TOKEN`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `JWT_SECRET`, `CORS_ORIGINS`) configure nothing. They are read only by the one-time legacy import and to log a warning on each start. Access control lives in the database: `api_keys`, and `auth_settings` (anonymous access policy, ticket secret, retired public token).

Subcommands (`export`, `import`, `keys`, `access`) are dispatched from `main.go` before the server starts. `keys` and `access` load the same config, connect, run `InitSchema` + `Migrate`, log to stderr at WARN, and print only their result on stdout.

Docker Compose uses `${VAR:-default}` interpolation in `docker-compose.yml` so most settings work without `.env`. Secrets have no defaults: `POSTGRES_PASSWORD` and `MQTT_PASSWORD` use `${VAR:?...}` so compose refuses to start without them.

## 3. Database Lifecycle

### Connect

`database.Connect()` in `internal/database/database.go`:

```
pgxpool.ParseConfig(databaseURL)
  ├── MaxConns = 20
  ├── MinConns = 4
  └── pgxpool.NewWithConfig()
        └── pool.Ping() — fail fast if unreachable
```

Returns `DB{Pool, Q (sqlc queries), log}`.

### InitSchema

`db.InitSchema()` in `internal/database/schema.go`:

```
SELECT EXISTS (SELECT FROM pg_tables
  WHERE schemaname='public' AND tablename='systems')
  │
  ├── true  → no-op (schema already loaded)
  └── false → Exec(embedded schema.sql)
               Creates all 27 tables, indexes, triggers,
               helper functions, initial partitions
```

`main` calls `db.ApplySchemaIfEmpty()`, the same check that also reports whether this process applied the schema. `schema.sql` itself inserts the `data_fixups` row `schema-created-with-api-key-auth`, whichever process runs it (the server, a `tr-engine` subcommand, `psql -f`), and the legacy auth import treats a database carrying it as fresh and imports nothing; databases upgraded from older versions never get it. It and `Migrate` hold a session advisory lock (`schemaLockKey` in `schema.go`), so two engines, or a `tr-engine keys ...` command run while the server starts, never set up the schema at the same time: the second one logs "another tr-engine process is setting up the database schema; waiting for it" and then finds the work done.

### Migrate

`db.Migrate()` in `internal/database/migrations.go`:

```
for each migration in migrations slice:
  ├── run check query → returns true? skip
  └── run SQL (IF NOT EXISTS / IF EXISTS for idempotency)
       └── on failure → return MigrationError with remaining SQL
```

Migrations handle post-`schema.sql` changes (new columns, replaced indexes). Fatal on failure since queries depend on the schema being current. The auth migrations read and drop a `users` table only if it is old tr-engine's (`trEngineUsersSQL`: its `trg_users_updated_at` trigger or viewer/editor role CHECK); another application's `users` table in a shared database is left alone.

### Partition Maintenance

Run by `Pipeline.maintenanceLoop()` — once immediately on startup, then every 24h:

1. Create monthly partitions 3 months ahead (calls, call_frequencies, call_transmissions, unit_events, trunking_messages)
2. Create weekly partitions 3 weeks ahead (mqtt_raw_messages)
3. Decimate state tables (recorder_snapshots, decode_rates): full→1/min after 1 week→1/hour after 1 month
4. Purge expired data (console_messages 30d, plugin_statuses 30d, checkpoints 7d)
5. Drop old weekly partitions (mqtt_raw_messages, 7-day retention)
6. Purge stale RECORDING calls (no call_end/audio after 1 hour)
7. Clean orphaned call_groups
8. Expire stale in-memory active calls (>1 hour old)

On-demand partition creation: if an INSERT fails with "no partition found", `ensurePartitionsFor()` creates the needed partition and the caller retries, for MQTT, file-watch and upload ingest alike. It only does this for months from 12 before to 3 after the current month (partitions are permanent, so a wrong clock or a crafted upload must not create them for arbitrary months); a call outside that window is dropped with a WARN, and an upload gets `400 invalid_body`. The `WATCH_DIR` startup backfill still creates the partitions for its whole date range itself (see §7).

## 4. Audio Storage

### Decision Tree

```
storage.New(cfg.S3, audioDir, log)
  │
  ├── S3 not enabled → LocalStore (filesystem only)
  │
  ├── S3 enabled, LocalCache=false → S3Store (S3 only)
  │
  └── S3 enabled, LocalCache=true → TieredStore
        ├── local: LocalStore (source of truth, fast serving)
        ├── s3: S3Store (backup/durability)
        └── background services:
              ├── CachePruner (if retention or maxGB set)
              │     runs every 1h, verifies S3 before deleting local
              └── UploadReconciler
                    runs every 5min (2min initial delay),
                    re-uploads local files missing from S3
```

### Write Paths

Local disk is always written first. S3 failure is never fatal — the reconciler catches it.

```
Sync mode (S3_UPLOAD_MODE=sync or default):
  TieredStore.Save() → local first (fatal), then S3 (warning on failure)

Async mode (S3_UPLOAD_MODE=async):
  TieredStore.SaveLocal() → local disk immediately
  AsyncUploader.Enqueue() → buffered channel (cap 500)
    └── 2 worker goroutines → S3Store.Save() with 30s timeout
        └── on failure: logged, file safe on local disk
```

### Read Path

Local disk first, S3 fallback with cache-on-read. When S3 serves a file that's
missing locally (e.g. after cache pruning), it's saved back to local disk so
subsequent requests hit the fast path.

```
TieredStore.Open()
  ├── local.Open() → found? return local file (fast path)
  └── s3.Open() → fallback to S3
        └── on success: save copy to local disk (best-effort)
              → next request hits local fast path

GetCallAudio (API):
  1. LocalPath → serve directly (fastest)
  2. Open → stream via TieredStore (triggers cache-on-read)
  3. TR_AUDIO_DIR fallback (file watch mode)
```

### LocalStore Safety

All writes use atomic temp-file-then-rename. `safePath()` rejects path traversal attempts.

## 5. Ingest Pipeline

### Construction

`NewPipeline()` in `internal/ingest/pipeline.go` creates:

| Component | Purpose |
|-----------|---------|
| `IdentityResolver` | Maps (instance_id, sys_name) → (system_id, site_id) |
| `activeCallMap` | Tracks in-flight calls: tr_call_id → call metadata |
| `affiliationMap` | Tracks unit→talkgroup affiliations |
| `EventBus(4096)` | SSE pub/sub with ring buffer for replay |
| `rawBatcher` | Batch-inserts mqtt_raw_messages (100 items / 2s) |
| `recorderBatcher` | Batch-inserts recorder_snapshots (100 items / 2s) |
| `trunkingBatcher` | Batch-inserts trunking_messages (100 items / 2s) |
| `transcriber` | Optional WorkerPool for STT (if configured) |

### Start Sequence

`Pipeline.Start()`:

```
1. identity.LoadCache() — pre-populate from DB (all sites)
2. Warmup gate decision:
   ├── cache non-empty → skip warmup (not a fresh DB)
   └── cache empty → activate warmup gate
         buffer non-identity messages for up to 5s
         until system registration establishes sysid/wacn
3. backfillAffiliations() — load recent join events from DB
4. Spawn background goroutines:
   ├── statsLoop (60s interval: log msg counts, active calls)
   ├── maintenanceLoop (24h: partitions, decimation, purges)
   ├── talkgroupStatsLoop (5min: refresh cached TG stats)
   ├── dedupCleanupLoop (10s: sweep expired unit event dedup entries)
   └── affiliationEvictionLoop (5min: evict entries >24h stale)
5. Start transcriber WorkerPool (if configured)
```

### Batcher

Generic `Batcher[T]` in `internal/ingest/batcher.go`:

```
Add(item)
  ├── items >= maxSize? → flush immediately (async goroutine)
  └── first item? → start timer (interval)
        └── timer fires → flush (async goroutine)

Stop() → flush remaining → wg.Wait() for in-flight flushes
```

All three batchers use maxSize=100, interval=2s. Flush functions use `CopyFrom` batch inserts with 10s timeout.

## 6. MQTT Message Flow

### Entry Point

```
mqttclient.Client
  └── SetMessageHandler(pipeline.HandleMessage)
```

### HandleMessage

`pipeline.go:HandleMessage()`:

```
topic + payload arrive
  │
  ├── msgCount.Add(1)
  ├── ParseTopic(topic) → Route{Handler, SysName}
  │     router.go: match on trailing segments only (prefix-agnostic)
  │     ├── .../trunk_recorder/status → "status"
  │     ├── .../call_start → "call_start"
  │     ├── .../{sys_name}/message → "trunking_message"
  │     ├── .../{sys_name}/{event} → "unit_event"
  │     └── nil → unknown topic
  │
  ├── json.Unmarshal → Envelope{InstanceID}
  ├── archiveRaw(handler, topic, payload, instanceID)
  │     check RAW_STORE, RAW_INCLUDE_TOPICS, RAW_EXCLUDE_TOPICS
  │     strip base64 audio data before archival
  │     add to rawBatcher
  │
  ├── UpdateTRInstanceStatus(instanceID, "connected", now)
  │
  └── dispatch(route, topic, payload, env)
        │
        ├── warmup gate check:
        │     if !warmupDone && handler not in {systems,system,config,status}
        │       → buffer message, return
        │
        └── switch route.Handler:
              status → handleStatus
              systems → handleSystems (triggers completeWarmup)
              call_start → handleCallStart
              call_end → handleCallEnd
              recorders → handleRecorders
              unit_event → handleUnitEvent
              trunking_message → handleTrunkingMessage
              console → handleConsoleLog
              ... (14 handlers total)
```

### call_start Walkthrough

```
handleCallStart(payload)
  │
  ├── json.Unmarshal → CallStartMessage
  ├── identity.Resolve(instanceID, sysName)
  │     ├── fast path: RLock → cache hit → return
  │     └── slow path: Lock → double-check → upsert instance
  │           → FindOrCreateSystem → FindOrCreateSite → cache
  │
  ├── db.InsertCall() → call_id (or conflict → update)
  ├── activeCalls.Set(trCallID, entry)
  │
  └── PublishEvent(EventData{Type: "call_start", ...})
        → EventBus.Publish() → ring buffer + subscribers
```

### call_end Walkthrough

```
handleCallEnd(payload)
  │
  ├── json.Unmarshal → CallEndMessage (includes audio metadata)
  ├── identity.Resolve(instanceID, sysName)
  │
  ├── Find active call:
  │     ├── activeCalls.Get(trCallID) — exact match
  │     └── activeCalls.FindByTgidAndTime(tgid, startTime, ±5s)
  │           fuzzy match handles TR's start_time shift
  │
  ├── Save audio (if audio data present):
  │     ├── store.Save(key, data, contentType) — or SaveToCache+Enqueue
  │     └── update call record with audio_file_path
  │
  ├── db.UpdateCallEnd() — set duration, freq_list, src_list, etc.
  ├── activeCalls.Delete(trCallID)
  ├── Assign call_group (dedup across sites)
  ├── Enqueue transcription (if configured, duration in range)
  │
  └── PublishEvent(EventData{Type: "call_end", ...})
```

### Identity Resolution

`internal/ingest/identity.go`:

```
Resolve(ctx, instanceID, sysName)
  │
  ├── Fast path (RLock):
  │     cache[instanceID:sysName] → hit? return immediately
  │     (hot path — most messages resolve here)
  │
  └── Slow path (Lock):
        ├── Double-check cache (another goroutine may have added it)
        ├── UpsertInstance(instanceID) → instance DB ID
        ├── FindOrCreateSystem(instanceID, sysName)
        │     P25: match on (sysid, wacn)
        │     Conventional: match on (instance_id, sys_name)
        │     Creates new system if no match
        ├── FindOrCreateSite(systemID, instanceID, sysName)
        │     Match on (system_id, instance_id, sys_name)
        │     Creates new site if no match
        └── Cache the resolved identity
```

### Raw Archival

```
archiveRaw(handler, topic, payload, instanceID)
  │
  ├── RAW_STORE=false? → return (disabled)
  ├── RAW_INCLUDE_TOPICS set? → allowlist check
  ├── RAW_EXCLUDE_TOPICS set? → denylist check
  ├── handler=="audio"? → stripAudioBase64(payload)
  │     removes audio_m4a_base64 / audio_wav_base64 from JSON
  │     (audio already saved to disk, ~60KB savings per message)
  └── rawBatcher.Add(RawMessageRow{topic, payload, time, instanceID})
```

## 7. File Watch Flow

`internal/ingest/watcher.go` — alternative to MQTT for users without the MQTT plugin.

```
FileWatcher.Start()
  │
  ├── fsnotify.NewWatcher()
  ├── WalkDir(watchDir) — add all existing directories
  ├── spawn watchLoop goroutine
  └── spawn backfill goroutine (if backfillDays >= 0)

watchLoop:
  fsnotify event (Create|Write)
    │
    ├── directory? → addDirRecursive (watch new subdirs, process .json)
    └── .json file? → scheduleProcess(path)
          debounce 500ms (coalesce Create+Write events)
            └── processJSONFile(path)
                  ├── ReadFile → json.Unmarshal → AudioMetadata
                  ├── skip if tgid <= 0
                  └── pipeline.processWatchedFile(instanceID, meta, path)
                        same pipeline as MQTT call_end:
                        identity resolve → insert call → assign group
                        → save audio → transcribe → publish SSE

backfill:
  ├── WalkDir → collect all .json files
  ├── Filter by cutoff (backfillDays)
  ├── Sort oldest-first
  ├── Ensure partitions for full date range
  └── Process with 8 worker goroutines
        progress logged every 5000 files
```

Watch mode only produces `call_end` events (files appear after calls complete). MQTT is the upgrade path for `call_start`, unit events, recorder state, and decode rates.

## 8. HTTP Upload Flow

`internal/api/upload.go` + `internal/ingest/handler_upload.go`

```
POST /api/v1/call-upload
  │
  ├── Auth pipeline (policy: scope upload, FormKey) — a Bearer header is
  │     resolved here; without one the request passes to the upload middleware
  ├── MaxBodySize(50 MB)
  ├── Upload middleware — no header principal: read multipart field "key",
  │     then "api_key" (body only, never the URL); invalid → 401 invalid_key,
  │     no upload scope → 403 insufficient_scope; rejections logged per IP
  │
  ├── ParseMultipartForm
  │     auto-detect format from field names:
  │     ├── "audioFile" field → rdio-scanner format
  │     │     ParseRdioScannerFields(form) → CallUploadData
  │     └── "audio" field → OpenMHz format
  │           ParseOpenMHzFields(form) → CallUploadData
  │
  ├── validateUploadMeta → 400 invalid_body for a short name with '/', '\',
  │     '..' or control characters or over 128 bytes, a talkgroup <= 0, a
  │     missing/non-positive start time, or a start time > 10 min ahead
  │
  └── pipeline.ProcessUploadedCall(ctx, data)
        ├── identity.Resolve(uploadInstanceID, sysName)
        ├── Dedup check: FindCallByTgidStartTime
        ├── InsertCall (status=COMPLETED); a month with no partition outside
        │     the on-demand window (12 back to 3 ahead) → 400 invalid_body
        ├── Save audio file (store.Save) as upload/<system_id>/<YYYY-MM-DD>/
        │     <call_id>.<ext> (m4a/mp3/wav/ogg/bin from audioType or the file
        │     name; never overwrites: an existing file gets a random suffix)
        ├── Assign call_group
        ├── Enqueue transcription
        └── PublishEvent("call_end")
```

## 9. HTTP Request Flow

### Middleware Stack

`internal/api/server.go:buildRouter()` — exact order as wired. The code is split across `middleware.go` (RequestID, CORS, Recoverer, Logger, APIHeaders, MaxBodySize, ResponseTimeout), `pipeline.go` (Match, Resolve, Authorize, Audit, UploadAuth), `authn.go` (key and ticket resolution, rate limits), `keycache.go` (key cache, limiter sets) and `streamauth.go` (re-checks of open streams, see [SSE Handler](#sse-handler)).

Router construction lives in `buildRouter(opts) *chi.Mux`. Every route is registered flat (no `r.Route(...)` sub-routers), so `chi.Walk` and `Mux.Find` agree on pattern strings, and every route has an entry in the policy table in `internal/api/policy.go`. HEAD requests are served by the GET handler (`middleware.GetHead`).

```
Root middleware, in order:
  1. RequestID   — keep a client X-Request-ID only if ≤64 chars of [A-Za-z0-9._-]
  2. CORS        — Access-Control-Allow-Origin: * on every response, never
                   Allow-Credentials; OPTIONS → 204 here, before auth or routing
     GetHead     — HEAD requests are served by the GET handler
  3. Recoverer   — catch panics → JSON 500
     Logger      — one access line (zerolog/hlog) for every request, refusals
                   included; a WebSocket upgrade is logged as 101 when the
                   socket closes
     APIHeaders  — Cache-Control: no-store and Vary: Authorization on /api/v1
  4. Match       — path = URL.RawPath or URL.Path; pattern = root.Find(fresh
                   route context, method, path), HEAD falling back to GET
                   ├── no pattern → chi answers 404/405, no handler runs
                   └── pattern without a policy → 403 forbidden + ERROR log
  5. Resolve     — principal from Authorization: Bearer (non-empty, Bearer only);
                   ?ticket= only on Ticket routes for GET/HEAD (and it wins);
                   no credential → anonymous. Invalid credential → 401, never
                   anonymous. Rate limiting interleaved (see below).
  6. Authorize   — public: pass
                   FormKey without a header principal: pass to upload middleware
                   anonymous and (KeyRequired, policy off, or scope > listen): 401 key_required
                   lacks scope: 403 insufficient_scope
                   restricted and Restricted == Deny: 403 restricted_credential
  7. Audit       — key principals, non-GET/HEAD/OPTIONS, except call-upload and
                   tickets; records the status the client received (runs after
                   Authorize, so auth-layer refusals are only in the access
                   log); path without query, capped at 1024 bytes
  8. Route-group middleware: MaxBodySize (10 MB API, 50 MB upload),
     [InstrumentHandler if metrics enabled], upload middleware,
     ResponseTimeout (http.TimeoutHandler; skips SSE + audio) → handler
```

Rate limiting (step 5): no credential → per-IP limiter (this includes uploads whose key is in the multipart form: Resolve sees no header credential and charges the IP; UploadAuth then resolves the form key, charging a second IP token on a cache miss) (`RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`, IP from `TrustedProxies.ClientIP`). A bearer found in the positive key cache → the key's own `rate_limit_rps` limiter if set, otherwise none (legacy keys: per-IP). A bearer not in the positive cache → take a per-IP token first (429 without touching the database if none), then the negative cache, then the database (2 s timeout; errors → 503, never cached). Tickets: MAC and expiry checked without the database, key resolved through the cache, per-IP limited.

Key cache: separate bounded LRUs for positive and negative results (so guesses can't evict valid keys), 30 s TTL, keyed by SHA-256 of the presented value, positive entries also indexed by key ID for tickets. `expires_at`/`revoked_at` are re-checked on every hit. API PATCH/DELETE invalidates the key's entries; an auth-generation bump clears both caches. `last_used_at` is written at most once a minute per key, asynchronously.

### Auth Model

```
Principal (internal/auth.Principal) — exactly one per request:
  │
  ├── key        Authorization: Bearer <key> (or, on /call-upload, form key/api_key)
  │                scopes: the key's; restrictions: the key's restriction, if any
  │
  ├── ticket     ?ticket=trt_<payload>.<mac> on /events/stream, /audio/live,
  │                /calls/{id}/audio (GET/HEAD)
  │                scopes: listen, if the minting key is still active with listen
  │                restrictions: key's current restriction ∩ ticket narrowing
  │
  └── anonymous  no credential
                   scopes: listen if auth_settings.anonymous_access is "listen", else none
                   restrictions: the policy's restriction, if any

Scopes: listen < edit < admin (each implies the lower ones); upload is independent.
Route policy: RoutePolicy{Scope, Restricted (Deny|Enforced), Ticket, KeyRequired, FormKey}.
Restricted principals only pass Enforced routes; those handlers apply
Principal.SQL(sysCol, tgCol, $n) in the shared WHERE, or GetCallAccess +
AllowsTG for single resources (404 outside the restriction).
```

The auth generation (`auth.Generation()`, an `atomic.Uint64`) is bumped by key PATCH/DELETE, anonymous-policy PUT and system merges. Caches and long-lived connections watch it. See [auth.md](auth.md) for the operator and client view.

### HTTP Server Config

```go
http.Server{
    Addr:         cfg.HTTPAddr,       // default :8080
    ReadTimeout:  cfg.ReadTimeout,    // default 5s
    IdleTimeout:  cfg.IdleTimeout,    // default 120s
    WriteTimeout: 0,                  // disabled for SSE
    MaxHeaderBytes: 64 << 10,         // request line + headers; larger → 431
}
```

`WriteTimeout=0` allows long-lived SSE connections. Non-streaming handlers are bounded by `ResponseTimeout` middleware (default 30s) and DB query timeouts.

## 10. SSE Event System

### EventBus

`internal/ingest/eventbus.go`:

```
EventBus
  ├── subscribers: map[uint64]subscriber
  │     each has: chan SSEEvent (buffered 64), EventFilter
  ├── ring buffer: []SSEEvent (4096 slots, ~60s at high rate)
  ├── seq: publish sequence number (orders the ring, dedupes replay vs live)
  ├── idCipher: per-process AES key that turns seq into the public event ID
  └── one mutex: numbering, the ring and distribution happen under it
```

### Publish

```
EventBus.Publish(EventData)
  │
  ├── JSON marshal payload
  └── under eb.mu (publishLocked):
        ├── seq++; ID = "{unix_millis}-{32 hex}": seq encrypted with idCipher,
        │     so IDs are opaque, unique per process and not sequential
        │     (a restricted subscriber can't count the events it wasn't shown)
        ├── Write to ring buffer
        │     ring[ringHead] = event   (evicting the oldest once full)
        │     ringHead = (ringHead + 1) % ringSize
        └── Distribute to subscribers
              for each subscriber:
                matchesFilter(event, sub.filter, sub.principal)?
                  ├── yes → non-blocking send to sub.ch
                  │         (drop if subscriber is slow)
                  └── no  → skip
  then, outside the lock: WARN about ring evictions (at most once a minute)
  and slow-subscriber drops (one line per publish)
```

### Subscribe

```
EventBus.SubscribeSince(lastEventID, filter, principal)
  → returns (replay []SSEEvent, <-chan SSEEvent, cancelFn)
  channel buffered to max(64, len(replay)+64) events
  cancel removes subscriber and closes channel
```

`LiveDataSource` (the interface `internal/api` sees) exposes only `SubscribeSince`; the old `Subscribe`/`ReplaySince` pair is gone (see [Replay](#replay-last-event-id)). `principal` is an `*atomic.Pointer[auth.Principal]` that the stream's `streamGuard` swaps when the credential changes. The live-audio side works the same way: `AudioStreamer.SubscribeAudio(filter, principal)` → `AudioBus.Subscribe(filter, principal)`.

After a system merge, `EventBus.RewriteSystemID(old, new)` relabels buffered events of the merged-away system, so a replay checks them against the (rewritten) restrictions under the ID their data now has.

### Filter Logic

`matchesFilter()` first applies the subscriber's **principal** (stored on the subscriber at subscribe time, separately from the client's `EventFilter`, and swappable atomically), then the client's own filters:

```
0. access (fail closed; zero values never pass):
   console                                         → needs admin
   call_start, call_update, call_end, transcription → needs listen; restricted
       principals only if SystemID != 0, Tgid != 0 and AllowsTG
   unit_event                                      → same (so on/off/registration
       events, which have no talkgroup, are dropped for restricted principals)
   recorder_update, rate_update, trunking_message  → needs listen; dropped for
       restricted principals
   any other type                                  → admin until classified
1. emergency_only: skip non-emergency events
2. types: match event Type (or Type:SubType for compound filters)
   "unit_event" matches all unit events
   "unit_event:call" matches only unit call events
3. systems: match SystemID (zero SystemID passes through)
4. sites: match SiteID (zero SiteID passes through)
5. tgids: match Tgid (zero Tgid passes through)
6. units: match UnitID (zero UnitID passes through)
```

In the client's own filters (steps 3–6), zero-value fields pass through their filter dimension, so events like `recorder_update` (no SystemID) reach subscribers filtering by system. That convenience never applies to step 0: a restriction only lets through events whose system and talkgroup it allows. The per-type map lives next to the route table in `policy.go` and is documented on `SSEEventType` in `openapi.yaml`.

### Replay (Last-Event-ID)

```
SubscribeSince(lastID, filter, principal) → (replay, ch, cancel)
  │
  ├── Under the same lock Publish holds while appending to the ring and
  │   distributing: register the subscriber and snapshot the ring
  │     find lastID → replay = filtered events after it
  │     lastID not found (ring wrapped)? → replay = ALL available filtered events
  │     (better to replay duplicates than miss events)
  │
  └── Live events are deduplicated by sequence: only those newer than the
      highest replayed sequence are sent. Channel buffer max(64, len(replay)+64).
```

This replaces replay-then-subscribe, which lost events published in between. The last event ID comes from the `Last-Event-ID` header or, for clients that re-create an `EventSource` with a fresh ticket, the `last_event_id` query parameter (the header wins).

### SSE Handler

`GET /api/v1/events/stream`:

```
1. Principal already resolved by the auth pipeline (key, ticket or anonymous)
2. Parse filter params from query string
3. SubscribeSince(Last-Event-ID header || last_event_id param, filter, principal)
   → send replay, then stream from the channel
4. Loop:
   ├── event from channel → write SSE frame
   ├── 15s ticker → write ": keepalive" comment
   ├── 60s ticker, auth generation changed, or the key reached expires_at
   │     → re-resolve the principal (streamGuard, internal/api/streamauth.go)
   │     (key re-read by ID bypassing the cache; ticket: key + narrowing + expiry;
   │      anonymous: current policy)
   │     ├── still has listen → swap the subscriber's principal atomically
   │     ├── database unreachable → keep the stream as it is, retry next time
   │     └── lost listen, key expired/revoked, or ticket expired →
   │           "event: auth" / data {"code": invalid_key|key_required|
   │           insufficient_scope|ticket_expired}, then end the response
   │           (a ticket whose narrowing names a merged-away system →
   │            ticket_expired: a new ticket fixes it)
   └── client disconnect → cancel subscription
Headers: Content-Type: text/event-stream, X-Accel-Buffering: no
```

`GET /api/v1/audio/live` (WebSocket) does the same re-check. Its principal is stored on the audio subscriber separately from the client-updatable `AudioFilter`; for a restricted principal `matchesAudioFilter` also requires `AllowsTG(frame.SystemID, frame.TGID)`. On auth loss it closes with code 4401 (`invalid_key`, `key_required`, `ticket_expired`) or 4403 (`insufficient_scope`), the code string as the reason. CLI changes (another process) reach open connections through the 60 s re-check, which reads the database directly. The WebSocket also sends a ping with each 15 s keepalive (whose `active_streams` counts only allowed talkgroups for a restricted principal), closes a peer it hasn't heard from (pongs included) for 60 s, and closes with 1009 on a client message over 64 KiB.

Live audio relies on correct system attribution of simplestream packets. The audio router maps a sender IP to a TR instance (`STREAM_SOURCE_MAP`, or worked out again for every chunk from the instances that have the packet's short name: real TR instances before the `WATCH_INSTANCE_ID` identity, before the `UPLOAD_INSTANCE_ID` identity, leaving out instances the source map gives to other senders); when the preferred instances use the short name for different systems, including when such an instance appears after the sender was attributed, it drops the sender's audio with a WARN instead of guessing.

## 11. Shutdown

Triggered by SIGINT or SIGTERM. The `defer` ordering in `main.go` determines teardown sequence (defers execute LIFO):

```
Signal received → ctx.Done()
  │
  ├── srv.Shutdown(10s timeout)      — stop accepting, drain HTTP connections
  │
  ├── pipeline.Stop()                — (defer, runs after HTTP shutdown)
  │     ├── warmupTimer.Stop()
  │     ├── watcher.Stop()           — close fsnotify
  │     ├── transcriber.Stop()       — drain transcription queue
  │     ├── uploader.Stop()          — drain async S3 uploads
  │     ├── rawBatcher.Stop()        — flush + wait
  │     ├── recorderBatcher.Stop()   — flush + wait
  │     ├── trunkingBatcher.Stop()   — flush + wait
  │     └── cancel()                 — cancel pipeline context
  │                                    stops all background goroutines
  │
  ├── mqtt.Close()                   — (defer) disconnect MQTT client
  │
  ├── storage services Stop()        — (defer) stop pruner, reconciler
  │
  └── db.Close()                     — (defer) close pgxpool
```

The 10-second timeout context bounds the entire shutdown. Pipeline batchers flush remaining items before stopping. Background goroutines exit via `<-p.ctx.Done()` checks in their ticker loops.
