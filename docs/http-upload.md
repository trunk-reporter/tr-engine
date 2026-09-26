# HTTP Call Upload

tr-engine can ingest calls via HTTP upload, compatible with trunk-recorder's **rdio-scanner** and **OpenMHz** upload plugins. This is useful when:

- You don't have local access to trunk-recorder's audio directory (no `TR_DIR` or `WATCH_DIR`)
- You're already uploading to another service (OpenMHz, Broadcastify) and want to add tr-engine
- trunk-recorder runs on a different machine with no shared filesystem or MQTT broker

## Quick Setup

1. Create an upload credential in tr-engine.

   Recommended for new installs: enable full auth with an admin password, then create an API key from the admin UI or API. Use that API key as the upload plugin credential.

   Legacy token mode is still supported. If you have not enabled full auth, set `AUTH_TOKEN` in your tr-engine `.env` and use that token:
   ```
   AUTH_TOKEN=your-secret-upload-token
   ```

2. Add the rdio-scanner plugin to your trunk-recorder `config.json`:
   ```json
   {
     "name": "rdioscanner_uploader",
     "library": "librdioscanner_uploader.so",
     "server": "https://your-tr-engine.example.com/api/v1/call-upload",
     "systems": [
       {
         "shortName": "butco",
         "apiKey": "your-secret-upload-token",
         "systemId": 1
       }
     ]
   }
   ```

3. Restart trunk-recorder. Calls will start appearing in tr-engine.

## Authentication

The upload endpoint checks credentials in this order:

1. `Authorization: Bearer <token>` header
2. `?token=<token>` query parameter
3. `key` form field (rdio-scanner convention)
4. `api_key` form field (OpenMHz convention)

**Which credential to use:**

| Auth mode | Upload authenticates with | Web UI / read API uses |
|-----------|--------------------------|------------------------|
| Open mode | No credential required | No credential required |
| Token mode | `AUTH_TOKEN` | `AUTH_TOKEN` entered by the user |
| Full mode | API key (`tre_...`) or `WRITE_TOKEN` — never the public read `AUTH_TOKEN` | Public read token or JWT session |
| Legacy write-token mode | `WRITE_TOKEN` | `AUTH_TOKEN` |

For new public-facing installs, prefer full mode: set `ADMIN_PASSWORD`, log in, and create a service API key for trunk-recorder uploads. `WRITE_TOKEN` remains accepted for backward compatibility, but it is deprecated.

## Choosing a Plugin

**Recommendation: Use the rdio-scanner plugin.** It sends significantly more metadata per upload.

### Metadata Comparison

| Field | rdio-scanner | OpenMHz |
|-------|:---:|:---:|
| System name (`systemLabel`) | yes | no |
| Talkgroup alpha tag | yes | no |
| Talkgroup description | yes | no |
| Talkgroup tag (category) | yes | no |
| Talkgroup group | yes | no |
| Audio file | yes | yes |
| Source list (unit IDs) | yes | yes |
| Frequency list (hops) | yes | yes |
| Start time | yes | yes |
| Stop time | no | yes |
| Emergency flag | yes | yes |
| Encrypted flag | yes | no |
| Audio type (m4a/wav) | yes | no |
| Error count | no | yes |

With the rdio-scanner plugin, talkgroup names and tags show up immediately in tr-engine without needing a CSV import or `TR_DIR` auto-discovery. With OpenMHz, you'll only see raw talkgroup IDs until talkgroups are populated from another source.

### Format Auto-Detection

tr-engine auto-detects the upload format from the form field names — no configuration needed on the tr-engine side:

- **rdio-scanner**: identified by `audio`, `audioName`, or `systemLabel` fields
- **OpenMHz**: identified by `call` or `talkgroup_num` fields

## Sample Configurations

### rdio-scanner Plugin (Recommended)

Add to the `plugins` array in trunk-recorder's `config.json`:

```json
{
  "name": "rdioscanner_uploader",
  "library": "librdioscanner_uploader.so",
  "server": "https://your-tr-engine.example.com/api/v1/call-upload",
  "systems": [
    {
      "shortName": "butco",
      "apiKey": "your-secret-upload-token",
      "systemId": 1
    }
  ]
}
```

The `shortName` must match the system's `shortName` in your trunk-recorder config. The `apiKey` is your tr-engine API key (`tre_...`) in full auth mode, or `AUTH_TOKEN` in token mode. The deprecated `WRITE_TOKEN` also works during the transition. The `systemId` is sent but not used by tr-engine — identity resolution uses `shortName` instead.

**Multiple systems:**

```json
{
  "name": "rdioscanner_uploader",
  "library": "librdioscanner_uploader.so",
  "server": "https://your-tr-engine.example.com/api/v1/call-upload",
  "systems": [
    { "shortName": "butco", "apiKey": "your-secret-upload-token", "systemId": 1 },
    { "shortName": "warco", "apiKey": "your-secret-upload-token", "systemId": 2 }
  ]
}
```

### OpenMHz Plugin

Add to the `plugins` array in trunk-recorder's `config.json`:

```json
{
  "name": "openmhz_uploader",
  "library": "libopenmhz_uploader.so"
}
```

And set the upload server and per-system API keys at the top level and system level:

```json
{
  "uploadServer": "https://your-tr-engine.example.com/api/v1/call-upload",
  "systems": [
    {
      "shortName": "butco",
      "apiKey": "your-secret-upload-token",
      ...
    }
  ]
}
```

Note: OpenMHz uses a single `uploadServer` for all systems, configured at the root of `config.json`.

## Running Alongside Other Upload Services

trunk-recorder's plugin system loads each entry in the `plugins` array independently. You can run multiple upload plugins simultaneously — the same `.so` library can even be loaded twice with different configurations.

**Example: Upload to both OpenMHz and tr-engine**

If you're already sending to OpenMHz/Broadcastify and want to add tr-engine, keep your existing OpenMHz plugin and add the rdio-scanner plugin:

```json
{
  "uploadServer": "https://api.openmhz.com",
  "plugins": [
    {
      "name": "openmhz_uploader",
      "library": "libopenmhz_uploader.so"
    },
    {
      "name": "rdioscanner_uploader",
      "library": "librdioscanner_uploader.so",
      "server": "https://your-tr-engine.example.com/api/v1/call-upload",
      "systems": [
        { "shortName": "butco", "apiKey": "your-tr-engine-write-token", "systemId": 1 }
      ]
    }
  ],
  "systems": [
    {
      "shortName": "butco",
      "apiKey": "your-openmhz-api-key",
      ...
    }
  ]
}
```

Each call is uploaded to both services independently. The `apiKey` in the `systems` array goes to OpenMHz; the `apiKey` in the rdio-scanner plugin config goes to tr-engine.

## How It Works

1. trunk-recorder finishes recording a call
2. The upload plugin POSTs the audio file + metadata as a multipart form to `/api/v1/call-upload`
3. tr-engine auto-detects the format, parses the metadata, and authenticates via the form `key`/`api_key` field
4. The call goes through the standard ingest pipeline: identity resolution (auto-creates systems/sites), dedup check, call record creation, audio file storage, source/frequency processing, unit upserts, SSE event publishing, and transcription enqueue
5. Returns `201 Created` with the call ID, or `409 Conflict` if the call is a duplicate

## Responses

| Status | Meaning |
|--------|---------|
| `201 Created` | Call ingested successfully. Response body contains `call_id`, `system_id`, `tgid`, `start_time`. |
| `400 Bad Request` | Invalid multipart form, unrecognized format, or missing required fields. |
| `401 Unauthorized` | Missing or invalid auth token. |
| `409 Conflict` | Duplicate call (same system, talkgroup, and start time within 5 seconds). |
| `413 Request Too Large` | Upload exceeds the 50 MB limit. |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_TOKEN` | _(empty)_ | Shared token for token-mode deployments. In full mode, this can act as a public read token. |
| `ADMIN_PASSWORD` | _(empty)_ | Enables full auth mode and JWT/API-key based write access. Recommended for public-facing deployments. |
| `WRITE_TOKEN` | _(empty)_ | Deprecated legacy write token. Still accepted during the transition; prefer API keys. |
| `UPLOAD_INSTANCE_ID` | `http-upload` | Instance ID assigned to uploaded calls for identity resolution. |
