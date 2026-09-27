# HTTP Call Upload

tr-engine can ingest calls via HTTP upload, compatible with trunk-recorder's **rdio-scanner** and **OpenMHz** upload plugins. This is useful when:

- You don't have local access to trunk-recorder's audio directory (no `TR_DIR` or `WATCH_DIR`)
- You're already uploading to another service (OpenMHz, Broadcastify) and want to add tr-engine
- trunk-recorder runs on a different machine with no shared filesystem or MQTT broker

## Quick Setup

1. Create an API key with the `upload` scope, one per trunk-recorder host:
   ```bash
   tr-engine keys create --name "trunk-recorder butco uploads" --scopes upload
   # Docker:
   docker compose exec -T tr-engine tr-engine keys create --name "trunk-recorder butco uploads" --scopes upload
   ```
   It prints the key (`tre_...`) once. You can also create it from tr-dashboard's Access page, `web/admin.html`, or `POST /api/v1/keys` with an admin key. See [auth.md](auth.md).

2. Add the rdio-scanner plugin to your trunk-recorder `config.json`:
   ```json
   {
     "name": "rdioscanner_uploader",
     "library": "librdioscanner_uploader.so",
     "server": "https://your-tr-engine.example.com/api/v1/call-upload",
     "systems": [
       {
         "shortName": "butco",
         "apiKey": "tre_your-upload-key",
         "systemId": 1
       }
     ]
   }
   ```

3. Restart trunk-recorder. Calls will start appearing in tr-engine.

## Authentication

Uploads need an API key with the **`upload`** scope. Nothing else can upload: anonymous access never allows it (whatever the anonymous access policy says), and `listen`, `edit` and `admin` keys without `upload` are refused with `403 insufficient_scope`.

The endpoint reads the key from, in order:

1. the `Authorization: Bearer <key>` header;
2. the `key` form field (rdio-scanner convention);
3. the `api_key` form field (OpenMHz convention).

Form fields are read from the multipart body only, never from the URL query string. A field that holds an unknown, revoked or expired key is rejected with `401 invalid_key`; the other field is not tried.

Give each trunk-recorder host its own `upload`-only key, so you can revoke one without touching the others, and so a leaked plugin config can't read or change anything. When an upload is rejected, tr-engine logs a WARN "call upload rejected" (at most once a minute per client IP) with the IP, the system name from the form, and the reason in its `reason` field: `no key`, `unknown key`, `key #N lacks upload`, `request body too large`, ...

**Rate limits.** A key in the `key`/`api_key` form field is not a header credential, so these uploads are rate-limited **per client IP** like requests without a key (`RATE_LIMIT_RPS`, default 20 per second with bursts of `RATE_LIMIT_BURST`, default 40), and cost two tokens when the key isn't in the engine's 30-second key cache, and always for a legacy (imported) key such as an imported `WRITE_TOKEN`, so those uploads get half the per-IP rate. A trunk-recorder host that uploads faster than that, for example while clearing a backlog, gets `429 rate_limited`. If the uploader can send `Authorization: Bearer <key>` instead, a normal key isn't IP-limited (unless it has its own `--rate-limit`), while a legacy key still costs one IP token per upload; otherwise raise `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`. Replace legacy upload keys with a new `upload` key (see [migrating-auth.md](migrating-auth.md#upload-plugins)).

Upgrading from a version that used `AUTH_TOKEN` or `WRITE_TOKEN` for uploads? See [migrating-auth.md](migrating-auth.md#upload-plugins) for which old credentials still work.

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
      "apiKey": "tre_your-upload-key",
      "systemId": 1
    }
  ]
}
```

The `shortName` must match the system's `shortName` in your trunk-recorder config. The `apiKey` is a tr-engine API key with the `upload` scope (`tre_...`). The `systemId` is sent but not used by tr-engine — identity resolution uses `shortName` instead.

**Multiple systems:**

```json
{
  "name": "rdioscanner_uploader",
  "library": "librdioscanner_uploader.so",
  "server": "https://your-tr-engine.example.com/api/v1/call-upload",
  "systems": [
    { "shortName": "butco", "apiKey": "tre_your-upload-key", "systemId": 1 },
    { "shortName": "warco", "apiKey": "tre_your-upload-key", "systemId": 2 }
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
      "apiKey": "tre_your-upload-key",
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
        { "shortName": "butco", "apiKey": "tre_your-upload-key", "systemId": 1 }
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
3. tr-engine checks the key (`upload` scope), auto-detects the format and parses the metadata
4. The call goes through the standard ingest pipeline: identity resolution (auto-creates systems/sites), dedup check, call record creation, audio file storage, source/frequency processing, unit upserts, SSE event publishing, and transcription enqueue
5. Returns `201 Created` with the call ID, or `409 Conflict` if the call is a duplicate

### Where the audio is stored

Uploaded audio is saved under the audio directory (or storage backend) as `upload/<system_id>/<YYYY-MM-DD>/<call_id>.<ext>`, with the UTC date of the call's start time. `ext` is `m4a`, `mp3`, `wav`, `ogg` or `bin`, taken from `audioType` or, failing that, the uploaded file's name (nothing declared means `m4a`; anything unrecognized is stored as `bin`). Every part of the path is chosen by tr-engine, never by the uploader, and an upload never overwrites a file: if one already exists under that name (left over from an earlier database), the upload is saved as `<call_id>-<random hex>.<ext>` instead, with a WARN.

### What an upload may claim

tr-engine refuses, with `400 invalid_body` and a message saying why:

- a system short name (`systemLabel`, `system` or `short_name`) containing `/`, `\`, `..` or control characters, or longer than 128 bytes;
- a talkgroup that is not a positive integer;
- a missing or non-positive start time (`dateTime` / `start_time`);
- a start time more than 10 minutes in the future (check the uploader's clock);
- a start time in a month that has no database partition yet and lies outside the months tr-engine creates partitions for on demand: from 12 months before the current month to 3 months after it. (Partitions are permanent, so a bogus timestamp must not create them for arbitrary months.) To load older calls, use `WATCH_DIR` backfill, which creates older partitions itself.

## Responses

| Status | Meaning |
|--------|---------|
| `201 Created` | Call ingested successfully. Response body contains `call_id`, `system_id`, `tgid`, `start_time` and, when the audio was saved, `audio_file_path` (`upload/<system_id>/<YYYY-MM-DD>/<call_id>.<ext>`). |
| `400 Bad Request` | Unrecognized format (`bad_request`), missing or malformed required fields, a refused short name, talkgroup or start time ([above](#what-an-upload-may-claim)), or (with a Bearer key) an invalid multipart form (`invalid_body`). |
| `401 Unauthorized` | `key_required` (no key; also a body that isn't multipart without a Bearer header, or the old public `AUTH_TOKEN` in the form) or `invalid_key` (unknown, revoked or expired key). |
| `403 Forbidden` | `insufficient_scope`: the key is valid but lacks the `upload` scope. |
| `409 Conflict` | Duplicate call (same system, talkgroup, and start time within 5 seconds). |
| `413 Request Too Large` | Upload exceeds the 50 MB limit. |
| `429 Too Many Requests` | `rate_limited`: the per-IP limit for form-field keys (see above); wait `Retry-After` seconds. |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `UPLOAD_INSTANCE_ID` | `http-upload` | Instance ID assigned to uploaded calls for identity resolution. |

Upload credentials are API keys, managed with `tr-engine keys` or the keys API, not environment variables.
