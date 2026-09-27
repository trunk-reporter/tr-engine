# TODO

## Critical

- [x] **Path traversal via MQTT-injected `call_filename`** — `internal/audio/resolve.go`. Added `containedIn()` check on all resolved paths; removed unconstrained absolute path fallback.

- [x] **JSON injection in trunking handler** — `internal/ingest/handler_trunking.go:27`. Replaced manual string concatenation with `json.Marshal(data.Meta)`.

- [x] **Latent SQL injection in `SQLColumn` fallback** — `internal/api/responses.go:107-116`. `SQLColumn()` now falls back to a valid allowlist column instead of returning raw user input.

## Important

- [x] **`ReplaySince` returns zero events when `Last-Event-ID` is evicted** — `internal/ingest/eventbus.go`. If the client's last event ID fell off the ring buffer, `found` stays false and nothing is returned. Should fall back to returning all buffered events. (Fixed; `ReplaySince` and `Subscribe` have since been replaced by `EventBus.SubscribeSince`, which registers and snapshots the ring under the publish lock and replays everything buffered when the ID is gone.)

- [x] **`MergeSystems` ignores TX errors** — `internal/database/systems.go:120-228`. ~15 `tx.Exec` calls discard errors. A failed statement mid-transaction leaves the DB in an inconsistent state. Check every error.

- [x] **Admin merge doesn't update identity cache** — `internal/api/admin.go`. The API merge endpoint calls `db.MergeSystems()` but never calls `identity.RewriteSystemID()`. New MQTT messages continue resolving to the deleted source system until restart.

- [x] **`handleUnitEvent` hardcodes topic depth = 4** — `internal/ingest/handler_units.go:15-18`. Breaks the prefix-agnostic routing design. Should use `parts[len(parts)-1]` instead of `parts[3]`.

- [x] **No upper bound on pagination `limit`** — `internal/api/responses.go:44-68`. `?limit=1000000` on any paginated endpoint causes unbounded memory usage. Add a max (e.g. 1000).

- [x] **`EventBus.Subscribe` never closes channel** — `internal/ingest/eventbus.go` (now `SubscribeSince`). The `cancel()` function deletes from the subscriber map but doesn't close the channel. Not currently exploitable due to `select` with context, but breaks the Go producer-consumer contract.

- [x] **Identity resolution errors silently drop SSE events** — `internal/ingest/handler_calls.go:267-280`. When `idErr != nil` on `call_end`, the SSE event is silently not published and no warning is logged.

## Architectural

- [x] **Pipeline is a God Object** — `internal/ingest/pipeline.go` (1088 lines) holds all state. Consider splitting into focused subsystems.

- [x] **No backpressure between MQTT and DB** — `HandleMessage` is called synchronously from paho's callback. During a DB stall, the paho internal queue grows unboundedly.

- [x] **Affiliation map grows without bound** — No eviction policy. `MarkOff` sets status but doesn't remove entries. Large P25 systems will accumulate stale entries.

- [x] **`WriteTimeout: 0` on all endpoints** — Necessary for SSE but exposes all non-SSE endpoints to slow-client holds. Consider per-handler timeouts.
