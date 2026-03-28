# trunk-reporter Roadmap

Internal priority tracker across the trunk-reporter organization. Items are selected from here for the public roadmap.

Last updated: 2026-03-28

## Organization Repos

| Repo | Purpose | Status |
|------|---------|--------|
| **tr-engine** | Backend API, MQTT ingest, transcription, SSE streaming | Active, pre-1.0 (v0.9.7) |
| **tr-dashboard** | Web frontend for tr-engine | Active, pre-1.0 (v1.0.0-pre5) |
| **tr-stack** | All-in-one Docker Compose (TR + engine + dashboard + ASR) | Active |
| **imbe-asr** | IMBE codec → text transcription (no audio decode step) | Active, deployed on eddie |
| **tr-plugin-dvcf** | TR plugin: captures IMBE frames, publishes .dvcf via MQTT | Active, deployed on eddie |
| **tr-docker** | Pre-built trunk-recorder Docker image with plugins | Maintained |
| **trunk-recorder** | Fork of trunk-recorder | Maintained |
| **qwen3-asr-server** | OpenAI-compatible ASR server (Qwen3, fine-tuned on P25) | Experimental |
| **p25-training-pipeline** | Fine-tune ASR models on P25 audio | Research |
| **tr-bot** | Discord bot for troubleshooting + debug reports | Active |
| **symbolstream** | DVCF binary format library | Supporting |
| **tr-update-worker** | Update notification worker | Supporting |

---

## 1.0 Blockers

Items that must ship before tr-engine and tr-dashboard can be tagged 1.0.

### Dual-provider transcription routing
**Repo:** tr-engine
**Why:** IMBE provider only handles P25. Analog/conventional calls get no transcription when `STT_PROVIDER=imbe`. Any user with mixed systems hits this immediately.
**Scope:** Two worker pools (IMBE + fallback), routing by presence of .dvcf file. Config: `STT_PROVIDER=imbe`, `STT_FALLBACK_PROVIDER=whisper`.
**Depends on:** Nothing — self-contained in tr-engine.

### Backfill manager IMBE guard
**Repo:** tr-engine
**Why:** Backfill feeds old calls (no .dvcf files) to the IMBE provider, causing log spam and wasted cycles. Confirmed on eddie.
**Scope:** Small fix — check provider name in `backfill.go:enqueueCall()`, skip calls without .dvcf when IMBE is active.
**Depends on:** Nothing.

### Test coverage gaps
**Repo:** tr-engine
**Why:** Unit-events and affiliations endpoints have no tests. Auth system has tests but coverage is thin on edge cases. Can't confidently tag 1.0 without this.
**Scope:** Test files for unit-events, affiliations, and auth edge cases.

### Dashboard guest + auth UX
**Repo:** tr-dashboard
**Why:** Auth flow is fragile — `guest_access` field was a band-aid. Dashboard needs a clean auth state machine: detect open/guest/login-required, handle gracefully without Caddy workarounds.
**Scope:** Refactor RequireAuth component, test against all tr-engine auth configurations.
**Depends on:** tr-engine auth-init contract (stable as of v0.9.7).

### Documentation for users
**Repo:** tr-engine, tr-stack
**Why:** Getting started docs exist but assume significant prior knowledge. First-time users need: install guide, configuration reference, troubleshooting. tr-stack needs a proper README.
**Scope:** Consolidate and expand `docs/`, write tr-stack README with quickstart.

### Deprecate `STREAM_INSTANCE_ID`
**Repo:** tr-engine
**Why:** Temporary config workaround from v0.8.29 for non-deterministic simplestream identity resolution. Confuses users. Real fix: cache first successful short_name→systemID mapping in audio router.
**Scope:** Small — audio router change + remove config field.

---

## 1.0 Nice-to-Haves

Ship if time allows, otherwise defer to 1.1.

### Transcription search
**Repo:** tr-engine
**Why:** Users want to find calls by what was said. Data is there, no search endpoint exists.
**Scope:** `pg_tsvector` index on `transcriptions.text`, new endpoint `GET /api/v1/transcriptions/search?q=...`.

### Glossary post-correction
**Repo:** tr-engine
**Why:** ASR consistently mangles street names, unit callsigns, landmarks, 10-codes. Research done (`docs/glossary-research.md`). Dramatically improves transcription quality for users who populate a glossary.
**Scope:** Glossary table, pg_trgm + fuzzystrmatch lookup, LLM correction pipeline, API for glossary CRUD.

### Notification / alerting
**Repo:** tr-engine or separate worker
**Why:** Users want push alerts on keywords, talkgroups, emergency calls. Natural extension of the SSE event bus.
**Scope:** Webhook dispatch (Discord, Telegram, generic HTTP). Configurable rules (keyword match, talkgroup, emergency flag). Could be a separate worker consuming SSE.

### S3 audio storage
**Repo:** tr-engine
**Why:** Long-running deployments accumulate audio. Storage abstraction exists (`internal/storage/`), tiered store started but incomplete.
**Scope:** Complete S3 lifecycle — upload, serve, expiry. Local cache with background sync.

### Admin UI in dashboard
**Repo:** tr-dashboard
**Why:** Admin API exists (maintenance, system merge, retention) but no UI. Admins currently use curl or the query endpoint.
**Scope:** Admin pages for maintenance status, system management, retention config.

---

## Post-1.0

Tracked for planning, not blocking any release.

### Incident grouping
Auto-detect related calls across talkgroups (same timeframe, overlapping units, dispatch + tactical channels). Builds on existing `call_groups` and `incident_data` fields.

### Call replay / timeline
Play back a sequence of calls on a talkgroup over a time range. Audio + transcriptions + unit attributions in a timeline view.

### Analytics / reporting
System utilization, busiest talkgroups, response time metrics, unit activity patterns. Aggregate queries over partitioned tables.

### Multi-instance dashboard
tr-dashboard pointing at multiple tr-engine backends (different counties/systems). Unified view with per-instance filtering.

### User roles and permissions
Beyond admin + guest: per-user access control, API key scoping by system/talkgroup. Foundation exists in JWT auth system.

### Federation
Multiple tr-engine instances sharing data across organizations. Cross-county coordination view.

### Mobile app
Push notifications + live audio on mobile. WebSocket audio already works in browser.

### qwen3-asr integration
Alternative ASR provider using Qwen3 fine-tuned on P25 audio. Experimental — dependent on training pipeline maturity and quality comparison vs Whisper/IMBE.

---

## Cross-cutting Concerns

These apply across multiple items:

- **DVCF plugin maturity:** tr-plugin-dvcf is deployed on eddie but not widely tested. Every call should produce a DVCF message — current ratio is low (~1:3 vs audio). Investigate whether the plugin has filtering or errors.
- **tr-stack as the default install path:** Most users will install via tr-stack, not individual repos. The stack's docker-compose.yml, defaults, and docs need to be first-class.
- **CI/CD:** tr-engine has GitHub Actions (build on tag, release with binaries). tr-dashboard has Docker image builds. Both need automated testing in CI before 1.0.
- **Versioning alignment:** tr-engine and tr-dashboard version independently but ship together. Consider whether they should share a version or have a compatibility matrix.
