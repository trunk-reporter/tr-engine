# Quality Gates

This baseline applies to every `tr-engine` implementation issue. An issue is not complete until it names the commands or manual checks it used, or documents why a gate could not run.

## Required Local Gates

Run these before opening review for backend changes:

```bash
bash build.sh
go test ./...
go vet ./...
```

For a narrower inner loop, run package-level tests first, for example:

```bash
go test ./internal/api
go test ./internal/ingest
go test ./internal/audio
```

The final verification for shared backend work should still include `go test ./...` unless a blocker is documented.

## API Contract Gates

`openapi.yaml` is the source of truth for the public REST, SSE, and live-audio contract. Any change that adds, removes, renames, or changes behavior for an endpoint, request, response, schema, auth requirement, SSE event, or audio route must include:

- The corresponding `openapi.yaml` update in the same change, including the operation's `security`, `x-scope`, `x-restricted` and (where it applies) `x-key-required`, which must match the route's entry in the policy table (`internal/api/policy.go`); a Go test checks the two agree.
- Focused endpoint tests in `internal/api/*_test.go` for status code, auth behavior, error shape, and response body.
- Regeneration or downstream verification for clients that consume the contract, especially `tr-dashboard`'s `npm run api:generate` when dashboard types are affected.
- A diff check that the OpenAPI change matches the implementation and does not remove unrelated paths or schemas.

Run an OpenAPI validator and include the command in the verification block, for example `npx -y @redocly/cli lint openapi.yaml` (the spec should validate; the known warnings are the WebSocket operation's lack of a 2xx response, `/openapi.yaml`'s lack of a 4xx response, and the prose-only `SSEAuthSignal` schema). The minimum contract gate is implementation tests, the policy/OpenAPI agreement test, and review of the `openapi.yaml` diff.

## Backend Test Expectations

Add or update tests for:

- New or changed HTTP endpoints, middleware, pagination, filtering, and error responses.
- Every new route gets a policy-table entry (the route/policy coverage test fails otherwise). An `Enforced` route needs a DB integration test showing that a restricted principal's rows **and totals** contain only allowed (system, tgid) pairs, including the empty-intersection case.
- Auth changes: principal resolution, scopes, restrictions, tickets, the anonymous policy, rate limiting and caches, with the documented error codes (`key_required`, `invalid_key`, `invalid_ticket`, `insufficient_scope`, `restricted_credential`).
- SSE event publishing, filtering, replay, reconnect assumptions, and event payload shape.
- Live audio ingest, routing, deduplication, and encoding behavior, especially failures that should surface to clients.
- Ingest handlers, identity resolution, deduplication, backfill behavior, and file or MQTT parsing.
- Database query behavior when SQL, migrations, or sqlc-generated code changes.

Prefer focused package tests for the changed boundary, then run the broader suite before completion.

## Manual Verification Checklist

Use the smallest checklist that covers the changed behavior. Include the checked items in the issue or PR.

- Auth: verify the change with no key under anonymous access `off`, `listen` and restricted `listen`, and with `listen`, `edit`, `admin`, `upload`, restricted, revoked and expired keys and a ticket, as relevant; failed auth must return the documented error code and `WWW-Authenticate` header.
- SSE: verify `/api/v1/events/stream` emits the changed event, honors filters, sends replay IDs, and behaves through the intended reverse proxy without buffering.
- Live audio: verify `/audio/live` upgrade behavior, subscription messages, disabled-stream errors, and client-visible close/error behavior.
- Reverse proxy: verify `/api/*`, `/audio/*`, `/health/*`, `/docs.html`, and `/openapi.yaml` route correctly in the deployment shape being changed.
- OpenAPI: verify Swagger UI or `/openapi.yaml` exposes the updated contract.
- Dashboard compatibility: verify affected dashboard calls compile or regenerate cleanly when the contract changes.

## Observability Baseline

New operationally significant work should leave enough information to diagnose failures without reproducing the full environment:

- Structured logs with stable event names and useful IDs for auth, ingest, SSE, live audio, transcription, storage, and proxy-facing failures.
- User-visible error responses using the documented API error envelope instead of opaque 500s where the failure is expected.
- Health or debug-report data for connection state, stream status, build/version metadata, and recent subsystem errors when practical.
- Metrics or counters for high-volume paths where regressions are otherwise invisible, such as ingest drops, SSE subscribers, live-audio clients, queue depth, and transcription failures.
- Debug report links or collected fields that let maintainers correlate browser reports with backend logs.

## Completion Rule

Every implementation issue should end with a verification block like:

```text
Verification:
- bash build.sh
- go test ./...
- go vet ./...
- Manual: /api/v1/events/stream emits call_end through Caddy with buffering disabled.
```

If a gate cannot run, record the blocker, the risk, and the narrower check that was run instead.
