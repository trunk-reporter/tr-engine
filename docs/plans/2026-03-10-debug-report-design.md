# Debug Report Feature Design

## Goal

One-click diagnostic report from any tr-engine deployment, accessible via a hidden page. Collects client-side browser state and server-side config/health, forwards the combined bundle to the debug-receiver for developer review.

## Architecture

```
Browser (debug-report.html)
  → POST /api/v1/debug-report  {client-side data + user notes}
    → tr-engine enriches with server config, health, TR config, logs
    → tr-engine redacts secrets
    → tr-engine forwards combined bundle to DEBUG_REPORT_URL
  ← 200 OK with success/failure status
```

The debug-receiver is a separate service that stores reports and optionally notifies via Discord webhook. tr-engine acts as the aggregation point — the browser never sees server secrets.

## Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| `DEBUG_REPORT_URL` | `https://case.luxprimatech.com/debug/report` | Debug-receiver endpoint. Empty string disables. |

## Page UI (`debug-report.html`)

Hidden page — no `card-title` meta tag, not listed in nav. Accessed by direct URL only (shareable in Discord for troubleshooting).

- **"What's wrong?"** — short text input
- **"Additional context"** — large textarea for pasting docker-compose.yml, config snippets, error messages, etc.
- **Category summary** — plain-language list of what auto-collected data will be sent (no data preview to avoid exposing secrets)
- **Consent message** — "This sends system diagnostics to the tr-engine developer."
- **"Send Report" button**
- **Status feedback** — success/error after submission

## Data Collected

### Client-Side (browser → tr-engine)

| Category | Fields |
|----------|--------|
| Browser | userAgent, platform, screen dimensions |
| Network | navigator.connection (effectiveType, downlink, rtt) |
| Audio | AudioContext state, sampleRate, baseLatency |
| Page | current URL, theme |
| Console errors | last ~50 console.error entries (ring buffer) |
| Audio engine | connection state, active TGs, buffer stats (if audio-engine.js loaded) |
| User input | "What's wrong?" note, pasted additional context |

### Server-Side (tr-engine enriches before forwarding)

| Category | Details |
|----------|---------|
| Config | Full config struct, secrets redacted (passwords, tokens, API keys → `***`). Database URL with credentials stripped (keep host/port/dbname). |
| TR config | trunk-recorder config.json contents (if `TR_DIR` set) |
| Health | DB, MQTT, streaming status, TR instances, version, uptime |
| Audio jitter | Server-side jitter stats from audio router |
| Identity cache | System count, site count, total cache entries |
| Container env | /.dockerenv detection, hostname, network interfaces, listening addresses |
| Console logs | `console_messages` table, last hour, warn/error level only |
| MQTT | Connection state, subscribed topics |

## Secret Redaction

Server-side before forwarding. Fields matching `PASSWORD`, `TOKEN`, `KEY`, `SECRET` are masked as `***`. `DATABASE_URL` and `MQTT_BROKER_URL` have credentials stripped but preserve host/port/dbname structure.

## Existing Debug Infrastructure (unchanged)

The audio-diagnostics page's inline report logic (PCM captures, detailed jitter deltas, direct POST to debug-receiver) is left as-is. It serves a different purpose — detailed audio quality analysis with captured samples.
