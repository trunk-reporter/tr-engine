# Debug Report Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** One-click diagnostic report from any tr-engine deployment — browser sends client state, server enriches with config/health/logs, forwards combined bundle to debug-receiver.

**Architecture:** Hidden `debug-report.html` page POSTs client-side data to `POST /api/v1/debug-report`. The server enriches with sanitized config, health data, TR config, console logs, network info, then forwards the combined JSON to the debug-receiver via HTTP POST. No secrets leave the server unsanitized.

**Tech Stack:** Go (API handler), vanilla HTML/JS (page), existing debug-receiver (no changes needed)

---

### Task 1: Add `DEBUG_REPORT_URL` config field

**Files:**
- Modify: `internal/config/config.go`

**Step 1: Add the config field**

In `internal/config/config.go`, add to the `Config` struct (after the `S3` field, before the closing brace):

```go
	// Debug report forwarding (empty string disables)
	DebugReportURL string `env:"DEBUG_REPORT_URL" envDefault:"https://case.luxprimatech.com/debug/report"`
```

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: success

**Step 3: Commit**

```bash
git add internal/config/config.go
git commit -m "feat(config): add DEBUG_REPORT_URL for debug report forwarding"
```

---

### Task 2: Config sanitizer

**Files:**
- Create: `internal/api/debug_report.go`
- Create: `internal/api/debug_report_test.go`

This task builds the config sanitization logic that redacts secrets before including config in debug reports.

**Step 1: Write the test**

Create `internal/api/debug_report_test.go`:

```go
package api

import (
	"testing"

	"github.com/snarg/tr-engine/internal/config"
)

func TestSanitizeConfig(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL:   "postgres://user:secret@db.example.com:5432/trengine",
		MQTTBrokerURL: "mqtt://mqttuser:mqttpass@broker.example.com:1883",
		AuthToken:     "my-secret-token",
		WriteToken:    "my-write-token",
		WhisperAPIKey: "sk-whisper-key",
		ElevenLabsAPIKey: "el-key-123",
		DeepInfraAPIKey:  "di-key-456",
		HTTPAddr:      ":8080",
		LogLevel:      "info",
		StreamListen:  ":9123",
		MQTTUsername:   "mqttuser",
		MQTTPassword:   "mqttpass",
	}
	cfg.S3.AccessKey = "AKIA1234"
	cfg.S3.SecretKey = "supersecret"
	cfg.S3.Bucket = "my-bucket"

	result := sanitizeConfig(cfg)

	// Secrets should be redacted
	if v, ok := result["AuthToken"]; !ok || v != "***" {
		t.Errorf("AuthToken = %q, want '***'", v)
	}
	if v, ok := result["WriteToken"]; !ok || v != "***" {
		t.Errorf("WriteToken = %q, want '***'", v)
	}
	if v, ok := result["WhisperAPIKey"]; !ok || v != "***" {
		t.Errorf("WhisperAPIKey = %q, want '***'", v)
	}
	if v, ok := result["MQTTUsername"]; !ok || v != "***" {
		t.Errorf("MQTTUsername = %q, want '***'", v)
	}
	if v, ok := result["MQTTPassword"]; !ok || v != "***" {
		t.Errorf("MQTTPassword = %q, want '***'", v)
	}

	// URLs should have credentials stripped
	dbURL, _ := result["DatabaseURL"].(string)
	if dbURL == "" || dbURL == cfg.DatabaseURL {
		t.Errorf("DatabaseURL should be sanitized, got %q", dbURL)
	}
	if contains(dbURL, "secret") || contains(dbURL, "user:") {
		t.Errorf("DatabaseURL still contains credentials: %q", dbURL)
	}
	// Host/port/dbname should be preserved
	if !contains(dbURL, "db.example.com") {
		t.Errorf("DatabaseURL should preserve host: %q", dbURL)
	}

	// Non-sensitive fields should be preserved
	if v, _ := result["HTTPAddr"].(string); v != ":8080" {
		t.Errorf("HTTPAddr = %q, want ':8080'", v)
	}
	if v, _ := result["StreamListen"].(string); v != ":9123" {
		t.Errorf("StreamListen = %q, want ':9123'", v)
	}

	// S3 secrets should be redacted
	s3, ok := result["S3"].(map[string]any)
	if !ok {
		t.Fatal("S3 should be a map")
	}
	if v, _ := s3["AccessKey"].(string); v != "***" {
		t.Errorf("S3.AccessKey = %q, want '***'", v)
	}
	if v, _ := s3["Bucket"].(string); v != "my-bucket" {
		t.Errorf("S3.Bucket = %q, want 'my-bucket'", v)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestSanitizeConfig -v`
Expected: FAIL (function not defined)

**Step 3: Implement sanitizeConfig**

Create `internal/api/debug_report.go`:

```go
package api

import (
	"net/url"

	"github.com/snarg/tr-engine/internal/config"
)

// sanitizeConfig returns a map representation of the config with secrets redacted.
// URLs have credentials stripped (host/port/dbname preserved). Token/key/password
// fields are replaced with "***".
func sanitizeConfig(cfg *config.Config) map[string]any {
	m := map[string]any{
		"DatabaseURL":       sanitizeURL(cfg.DatabaseURL),
		"MQTTBrokerURL":     sanitizeURL(cfg.MQTTBrokerURL),
		"MQTTTopics":        cfg.MQTTTopics,
		"MQTTInstanceMap":   cfg.MQTTInstanceMap,
		"MQTTClientID":      cfg.MQTTClientID,
		"MQTTUsername":       redact(cfg.MQTTUsername),
		"MQTTPassword":       redact(cfg.MQTTPassword),
		"AudioDir":          cfg.AudioDir,
		"TRAudioDir":        cfg.TRAudioDir,
		"WatchDir":          cfg.WatchDir,
		"WatchInstanceID":   cfg.WatchInstanceID,
		"WatchBackfillDays": cfg.WatchBackfillDays,
		"UploadInstanceID":  cfg.UploadInstanceID,
		"StreamListen":      cfg.StreamListen,
		"StreamInstanceID":  cfg.StreamInstanceID,
		"StreamSampleRate":  cfg.StreamSampleRate,
		"StreamOpusBitrate": cfg.StreamOpusBitrate,
		"StreamMaxClients":  cfg.StreamMaxClients,
		"StreamIdleTimeout": cfg.StreamIdleTimeout.String(),
		"TRDir":             cfg.TRDir,
		"CSVWriteback":      cfg.CSVWriteback,
		"MergeP25Systems":   cfg.MergeP25Systems,
		"HTTPAddr":          cfg.HTTPAddr,
		"ReadTimeout":       cfg.ReadTimeout.String(),
		"WriteTimeout":      cfg.WriteTimeout.String(),
		"IdleTimeout":       cfg.IdleTimeout.String(),
		"AuthEnabled":       cfg.AuthEnabled,
		"AuthToken":         redact(cfg.AuthToken),
		"WriteToken":        redact(cfg.WriteToken),
		"RateLimitRPS":      cfg.RateLimitRPS,
		"RateLimitBurst":    cfg.RateLimitBurst,
		"CORSOrigins":       cfg.CORSOrigins,
		"LogLevel":          cfg.LogLevel,
		"RawStore":          cfg.RawStore,
		"RawIncludeTopics":  cfg.RawIncludeTopics,
		"RawExcludeTopics":  cfg.RawExcludeTopics,
		"STTProvider":       cfg.STTProvider,
		"WhisperURL":        sanitizeURL(cfg.WhisperURL),
		"WhisperAPIKey":     redact(cfg.WhisperAPIKey),
		"WhisperModel":      cfg.WhisperModel,
		"ElevenLabsAPIKey":  redact(cfg.ElevenLabsAPIKey),
		"DeepInfraAPIKey":   redact(cfg.DeepInfraAPIKey),
		"DeepInfraModel":    cfg.DeepInfraModel,
		"LLMUrl":            sanitizeURL(cfg.LLMUrl),
		"LLMModel":          cfg.LLMModel,
		"MetricsEnabled":    cfg.MetricsEnabled,
		"PreprocessAudio":   cfg.PreprocessAudio,
		"TranscribeWorkers":      cfg.TranscribeWorkers,
		"TranscribeQueueSize":    cfg.TranscribeQueueSize,
		"TranscribeMinDuration":  cfg.TranscribeMinDuration,
		"TranscribeMaxDuration":  cfg.TranscribeMaxDuration,
		"TranscribeIncludeTGIDs": cfg.TranscribeIncludeTGIDs,
		"TranscribeExcludeTGIDs": cfg.TranscribeExcludeTGIDs,
		"RetentionRawMessages":   cfg.RetentionRawMessages.String(),
		"RetentionConsoleLogs":   cfg.RetentionConsoleLogs.String(),
		"RetentionPluginStatus":  cfg.RetentionPluginStatus.String(),
		"RetentionCheckpoints":   cfg.RetentionCheckpoints.String(),
		"RetentionStaleCalls":    cfg.RetentionStaleCalls.String(),
		"S3": map[string]any{
			"Bucket":         cfg.S3.Bucket,
			"Endpoint":       sanitizeURL(cfg.S3.Endpoint),
			"Region":         cfg.S3.Region,
			"AccessKey":      redact(cfg.S3.AccessKey),
			"SecretKey":      redact(cfg.S3.SecretKey),
			"Prefix":         cfg.S3.Prefix,
			"PresignExpiry":  cfg.S3.PresignExpiry.String(),
			"LocalCache":     cfg.S3.LocalCache,
			"CacheRetention": cfg.S3.CacheRetention.String(),
			"CacheMaxGB":     cfg.S3.CacheMaxGB,
			"UploadMode":     cfg.S3.UploadMode,
		},
	}
	return m
}

// redact returns "***" for non-empty strings, empty string for empty.
func redact(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// sanitizeURL strips userinfo (credentials) from a URL string.
// Returns the original string if it can't be parsed as a URL.
func sanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestSanitizeConfig -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/api/debug_report.go internal/api/debug_report_test.go
git commit -m "feat(api): add config sanitizer for debug reports"
```

---

### Task 3: Server environment collector

**Files:**
- Modify: `internal/api/debug_report.go`
- Modify: `internal/api/debug_report_test.go`

Collects server-side environment info: container detection, network interfaces, hostname.

**Step 1: Write the test**

Add to `internal/api/debug_report_test.go`:

```go
func TestCollectEnvironment(t *testing.T) {
	env := collectEnvironment()

	if env["hostname"] == nil {
		t.Error("hostname should be set")
	}
	// network_interfaces should be a slice
	if _, ok := env["network_interfaces"]; !ok {
		t.Error("network_interfaces should be present")
	}
	// in_container should be a bool
	if _, ok := env["in_container"].(bool); !ok {
		t.Error("in_container should be a bool")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestCollectEnvironment -v`
Expected: FAIL

**Step 3: Implement collectEnvironment**

Add to `internal/api/debug_report.go`:

```go
import (
	"net"
	"os"
	"runtime"
)

// collectEnvironment gathers host/container environment info.
func collectEnvironment() map[string]any {
	env := map[string]any{
		"go_version": runtime.Version(),
		"go_os":      runtime.GOOS,
		"go_arch":    runtime.GOARCH,
		"num_cpu":    runtime.NumCPU(),
	}

	if hostname, err := os.Hostname(); err == nil {
		env["hostname"] = hostname
	}

	// Container detection
	_, dockerEnv := os.Stat("/.dockerenv")
	env["in_container"] = dockerEnv == nil

	// Network interfaces
	ifaces, err := net.Interfaces()
	if err == nil {
		var netInfo []map[string]any
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			addrStrs := make([]string, 0, len(addrs))
			for _, a := range addrs {
				addrStrs = append(addrStrs, a.String())
			}
			if len(addrStrs) > 0 {
				netInfo = append(netInfo, map[string]any{
					"name":  iface.Name,
					"flags": iface.Flags.String(),
					"addrs": addrStrs,
				})
			}
		}
		env["network_interfaces"] = netInfo
	}

	return env
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestCollectEnvironment -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/api/debug_report.go internal/api/debug_report_test.go
git commit -m "feat(api): add environment collector for debug reports"
```

---

### Task 4: Debug report HTTP handler

**Files:**
- Modify: `internal/api/debug_report.go`
- Modify: `internal/api/server.go`

The handler receives client-side data, enriches with server-side data, and forwards to the debug-receiver.

**Step 1: Add the handler struct and route wiring**

Add to `internal/api/debug_report.go`:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// DebugReportHandler handles debug report submission and forwarding.
type DebugReportHandler struct {
	db             *database.DB
	cfg            *config.Config
	live           LiveDataSource
	audioStreamer   AudioStreamer
	log            zerolog.Logger
	version        string
	startTime      time.Time
	mqtt           MQTTStatus
	forwardURL     string
	trConfigPath   string // path to TR config.json (empty if TR_DIR not set)
}

// MQTTStatus provides MQTT connection state.
type MQTTStatus interface {
	IsConnected() bool
}

// DebugReportOptions holds the dependencies for the debug report handler.
type DebugReportOptions struct {
	DB           *database.DB
	Config       *config.Config
	Live         LiveDataSource
	AudioStreamer AudioStreamer
	MQTT         MQTTStatus
	Log          zerolog.Logger
	Version      string
	StartTime    time.Time
}

func NewDebugReportHandler(opts DebugReportOptions) *DebugReportHandler {
	trConfigPath := ""
	if opts.Config.TRDir != "" {
		trConfigPath = filepath.Join(opts.Config.TRDir, "config.json")
	}
	return &DebugReportHandler{
		db:           opts.DB,
		cfg:          opts.Config,
		live:         opts.Live,
		audioStreamer: opts.AudioStreamer,
		log:          opts.Log,
		version:      opts.Version,
		startTime:    opts.StartTime,
		mqtt:         opts.MQTT,
		forwardURL:   opts.Config.DebugReportURL,
		trConfigPath: trConfigPath,
	}
}

func (h *DebugReportHandler) Routes(r chi.Router) {
	r.Post("/debug-report", h.Submit)
}

func (h *DebugReportHandler) Submit(w http.ResponseWriter, r *http.Request) {
	if h.forwardURL == "" {
		http.Error(w, `{"error":"debug reports disabled"}`, http.StatusServiceUnavailable)
		return
	}

	// Read client payload
	var clientData map[string]any
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit for client data
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &clientData); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
	}
	if clientData == nil {
		clientData = make(map[string]any)
	}

	// Build combined report
	report := map[string]any{
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"report_type": "debug_report",
		"version":     h.version,
		"uptime_seconds": int64(time.Since(h.startTime).Seconds()),
		"client":      clientData,
		"server":      h.collectServerData(r.Context()),
	}

	// Forward to debug-receiver
	reportJSON, err := json.Marshal(report)
	if err != nil {
		h.log.Error().Err(err).Msg("failed to marshal debug report")
		http.Error(w, `{"error":"failed to build report"}`, http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.forwardURL, bytes.NewReader(reportJSON))
	if err != nil {
		h.log.Error().Err(err).Msg("failed to create forward request")
		http.Error(w, `{"error":"failed to forward report"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.log.Error().Err(err).Str("url", h.forwardURL).Msg("failed to forward debug report")
		http.Error(w, `{"error":"failed to forward report"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.log.Warn().Int("status", resp.StatusCode).Msg("debug-receiver returned non-200")
		http.Error(w, `{"error":"debug-receiver rejected report"}`, http.StatusBadGateway)
		return
	}

	h.log.Info().Str("remote_addr", r.RemoteAddr).Msg("debug report forwarded")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// collectServerData gathers all server-side diagnostic data.
func (h *DebugReportHandler) collectServerData(ctx context.Context) map[string]any {
	data := map[string]any{
		"config":      sanitizeConfig(h.cfg),
		"environment": collectEnvironment(),
	}

	// Health-like data
	data["mqtt_connected"] = h.mqtt != nil && h.mqtt.IsConnected()

	// TR instance status
	if h.live != nil {
		data["tr_instances"] = h.live.TRInstanceStatus()
		data["ingest_metrics"] = h.live.IngestMetrics()
		data["watcher_status"] = h.live.WatcherStatus()
		data["transcription_status"] = h.live.TranscriptionStatus()
		data["transcription_queue"] = h.live.TranscriptionQueueStats()
		data["maintenance"] = h.live.MaintenanceStatus()
	}

	// Audio streaming
	if h.audioStreamer != nil {
		data["audio_stream"] = h.audioStreamer.AudioStreamStatus()
		data["audio_jitter"] = h.audioStreamer.AudioJitterStats()
	}

	// Database pool stats
	if h.db != nil {
		stat := h.db.Pool.Stat()
		data["database_pool"] = map[string]any{
			"max_conns":       stat.MaxConns(),
			"total_conns":     stat.TotalConns(),
			"acquired_conns":  stat.AcquiredConns(),
			"idle_conns":      stat.IdleConns(),
			"empty_acquires":  stat.EmptyAcquireCount(),
		}

		// Console messages (last hour, warn/error only)
		data["console_messages"] = h.queryConsoleLogs(ctx)
	}

	// TR config.json
	if h.trConfigPath != "" {
		if raw, err := os.ReadFile(h.trConfigPath); err == nil {
			var trCfg any
			if json.Unmarshal(raw, &trCfg) == nil {
				data["tr_config"] = trCfg
			}
		}
	}

	return data
}

// queryConsoleLogs fetches warn/error console messages from the last hour.
func (h *DebugReportHandler) queryConsoleLogs(ctx context.Context) any {
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	warnLevel := "warn"
	msgs, _, err := h.db.ListConsoleMessages(ctx, database.ConsoleMessageFilter{
		Severity:  &warnLevel,
		StartTime: &oneHourAgo,
		Limit:     200,
	})
	if err != nil {
		h.log.Warn().Err(err).Msg("failed to query console messages for debug report")
		return nil
	}

	// Also get errors
	errorLevel := "error"
	errorMsgs, _, err := h.db.ListConsoleMessages(ctx, database.ConsoleMessageFilter{
		Severity:  &errorLevel,
		StartTime: &oneHourAgo,
		Limit:     200,
	})
	if err == nil {
		msgs = append(msgs, errorMsgs...)
	}

	return msgs
}
```

**Step 2: Check how ListConsoleMessages severity filter works**

The `Severity` filter in `ConsoleMessageFilter` may filter for exact match or prefix. Check `internal/database/console_messages.go` to see if it supports querying both `warn` and `error` in one call, or if two queries are needed. If the schema has a severity column that's a simple text match, two queries (as shown) is correct. If there's a way to do `severity IN ('warn', 'error')`, that's better but requires a schema check.

**Step 3: Wire the handler in server.go**

In `internal/api/server.go`, add the debug report handler. It should be registered **outside** the authenticated routes group (like health), since we want it accessible without write token. Place it after the health endpoint registration (around line 78):

```go
	// Debug report endpoint (unauthenticated — user sees consent dialog on the page)
	if opts.Config.DebugReportURL != "" {
		debugReport := NewDebugReportHandler(DebugReportOptions{
			DB:           opts.DB,
			Config:       opts.Config,
			Live:         opts.Live,
			AudioStreamer: opts.AudioStreamer,
			MQTT:         opts.MQTT,
			Log:          opts.Log,
			Version:      opts.Version,
			StartTime:    opts.StartTime,
		})
		r.Post("/api/v1/debug-report", debugReport.Submit)
	}
```

Note: The `MQTTStatus` interface needs `IsConnected() bool`. The existing `*mqttclient.Client` already has this method, so it satisfies the interface. If MQTT is nil (not configured), the handler handles nil gracefully.

**Step 4: Verify it compiles**

Run: `go build ./...`
Expected: success

**Step 5: Commit**

```bash
git add internal/api/debug_report.go internal/api/server.go
git commit -m "feat(api): add POST /api/v1/debug-report endpoint"
```

---

### Task 5: Console message severity query fix

**Files:**
- Modify: `internal/database/console_messages.go` (check only)

**Step 1: Verify how severity filter works**

Read `internal/database/console_messages.go` and check how the `Severity` filter is applied. If it does an exact match (`severity = $N`), the two-query approach in Task 4 is correct. If it already supports array or `>=` filtering, adjust `queryConsoleLogs` accordingly.

The key question: does the console_messages table store severity as `info`, `warn`, `error` strings? Or numeric levels? Check the schema and adjust the query logic if needed.

**Step 2: Test with the actual query**

Run: `go test ./internal/api/ -run TestSanitizeConfig -v`
Expected: PASS (existing tests still work)

---

### Task 6: Frontend — debug-report.html

**Files:**
- Create: `web/debug-report.html`

This is the hidden page (no `card-title` meta tag). Accessible by direct URL only.

**Step 1: Create the page**

Create `web/debug-report.html`. The page should:

1. **Not have** a `card-title` meta tag (stays hidden from nav)
2. Include `auth.js` and `theme-config.js` / `theme-engine.js` for consistent styling
3. Show a brief explanation of what data will be sent
4. Provide:
   - "What's wrong?" text input
   - "Additional context" textarea with placeholder "Paste your docker-compose.yml, config snippets, error messages, etc."
   - Category checklist (display only, not toggleable) showing what auto-collected data is included
   - "Send Debug Report" button
   - Consent text: "This sends system diagnostics to the tr-engine developer."
5. On submit:
   - Collect client-side data (browser info, audio state, console errors)
   - POST JSON to `/api/v1/debug-report`
   - Show success/error feedback

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<script src="auth.js?v=1"></script>
<title>Debug Report — tr-engine</title>
<script src="theme-config.js"></script>
<style>
  body { font-family: system-ui, sans-serif; max-width: 640px; margin: 80px auto 40px; padding: 0 20px; }
  h1 { font-size: 1.4em; margin-bottom: 0.3em; }
  .subtitle { color: #888; margin-bottom: 2em; font-size: 0.9em; }
  label { display: block; font-weight: 600; margin: 1.2em 0 0.3em; font-size: 0.95em; }
  input[type="text"], textarea {
    width: 100%; box-sizing: border-box; padding: 8px 10px;
    border: 1px solid var(--border, #333); border-radius: 4px;
    background: var(--bg-secondary, #1a1a1a); color: var(--text, #e0e0e0);
    font-family: inherit; font-size: 0.9em;
  }
  textarea { min-height: 120px; resize: vertical; }
  .categories { margin: 1.5em 0; padding: 1em; border: 1px solid var(--border, #333); border-radius: 6px; background: var(--bg-secondary, #1a1a1a); }
  .categories h3 { margin: 0 0 0.6em; font-size: 0.95em; }
  .categories ul { margin: 0; padding-left: 1.4em; font-size: 0.85em; color: #aaa; line-height: 1.8; }
  .consent { margin: 1.5em 0; font-size: 0.85em; color: #999; }
  button {
    padding: 10px 24px; border: none; border-radius: 4px; cursor: pointer;
    font-size: 1em; font-weight: 600;
    background: var(--accent, #4a9eff); color: #fff;
  }
  button:disabled { opacity: 0.5; cursor: not-allowed; }
  .status { margin-top: 1em; padding: 10px; border-radius: 4px; font-size: 0.9em; display: none; }
  .status.success { display: block; background: #1a3a1a; color: #4caf50; border: 1px solid #2a5a2a; }
  .status.error { display: block; background: #3a1a1a; color: #f44336; border: 1px solid #5a2a2a; }
</style>
</head>
<body>
  <h1>Send Debug Report</h1>
  <p class="subtitle">Help troubleshoot issues by sending system diagnostics to the developer.</p>

  <label for="problem">What's wrong?</label>
  <input type="text" id="problem" placeholder="Brief description of the issue">

  <label for="context">Additional context</label>
  <textarea id="context" placeholder="Paste your docker-compose.yml, config snippets, error messages, etc."></textarea>

  <div class="categories">
    <h3>The following data will be collected automatically:</h3>
    <ul>
      <li>Browser info (user agent, screen, network type)</li>
      <li>Audio playback state (if active)</li>
      <li>Recent browser console errors</li>
      <li>Server configuration (passwords and API keys are redacted)</li>
      <li>trunk-recorder config (if available)</li>
      <li>System health (database, MQTT, streaming status)</li>
      <li>Audio streaming jitter statistics</li>
      <li>Recent trunk-recorder warnings and errors (last hour)</li>
      <li>Server environment (hostname, network interfaces, container detection)</li>
    </ul>
  </div>

  <p class="consent">This sends system diagnostics to the tr-engine developer. Secrets (passwords, API keys, tokens) are automatically redacted before transmission.</p>

  <button id="btnSend" onclick="sendReport()">Send Debug Report</button>
  <div id="status" class="status"></div>

  <script>
  // Capture console errors in a ring buffer
  const consoleErrors = [];
  const MAX_ERRORS = 50;
  const origError = console.error;
  console.error = function() {
    consoleErrors.push({
      time: new Date().toISOString(),
      args: Array.from(arguments).map(a => {
        try { return typeof a === 'object' ? JSON.stringify(a) : String(a); }
        catch { return String(a); }
      })
    });
    if (consoleErrors.length > MAX_ERRORS) consoleErrors.shift();
    origError.apply(console, arguments);
  };

  function collectClientData() {
    const data = {
      timestamp: new Date().toISOString(),
      userAgent: navigator.userAgent,
      platform: navigator.platform,
      language: navigator.language,
      screen: { width: screen.width, height: screen.height, pixelRatio: devicePixelRatio },
      page: { url: location.href, referrer: document.referrer },
      consoleErrors: consoleErrors.slice(),
    };

    // Network info
    if (navigator.connection) {
      const c = navigator.connection;
      data.network = { type: c.type, effectiveType: c.effectiveType, downlink: c.downlink, rtt: c.rtt };
    }

    // AudioContext support
    try {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (AC) {
        const ctx = new AC();
        data.audioSupport = { state: ctx.state, sampleRate: ctx.sampleRate, baseLatency: ctx.baseLatency };
        ctx.close();
      }
    } catch (e) {
      data.audioSupport = { error: e.message };
    }

    // Audio engine state (if loaded on another page and accessible)
    if (window.audioEngine) {
      try {
        data.audioEngine = {
          connected: window.audioEngine.isConnected(),
          activeTGs: window.audioEngine.getActiveTGs(),
        };
      } catch (e) { /* ignore */ }
    }

    // Theme
    if (window.ThemeEngine) {
      data.theme = window.ThemeEngine.current();
    }

    return data;
  }

  async function sendReport() {
    const btn = document.getElementById('btnSend');
    const status = document.getElementById('status');
    btn.disabled = true;
    btn.textContent = 'Sending...';
    status.className = 'status';
    status.style.display = 'none';

    try {
      const clientData = collectClientData();
      clientData.problem = document.getElementById('problem').value;
      clientData.additionalContext = document.getElementById('context').value;

      const resp = await fetch('/api/v1/debug-report', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(clientData),
      });

      if (resp.ok) {
        status.textContent = 'Report sent successfully. Thank you!';
        status.className = 'status success';
      } else {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        status.textContent = 'Failed to send: ' + (err.error || resp.statusText);
        status.className = 'status error';
      }
    } catch (e) {
      status.textContent = 'Failed to send: ' + e.message;
      status.className = 'status error';
    } finally {
      btn.disabled = false;
      btn.textContent = 'Send Debug Report';
    }
  }
  </script>
  <script src="theme-engine.js?v=2"></script>
</body>
</html>
```

**Step 2: Test manually**

1. Run tr-engine locally
2. Navigate to `http://localhost:8080/debug-report.html`
3. Verify the page loads, shows categories, accepts input
4. Click "Send Debug Report" and verify it reaches the debug-receiver (check the Discord webhook notification)

**Step 3: Commit**

```bash
git add web/debug-report.html
git commit -m "feat(web): add debug report page for one-click diagnostics"
```

---

### Task 7: Integration test

**Files:**
- Modify: `internal/api/debug_report_test.go`

**Step 1: Write integration test for the submit handler**

Add a test that:
1. Starts a mock debug-receiver (httptest.Server) that records what it receives
2. Creates a DebugReportHandler with the mock URL
3. POSTs client data to the handler
4. Verifies the forwarded payload contains both client and server sections
5. Verifies secrets are redacted in the forwarded payload

```go
func TestDebugReportSubmitForwards(t *testing.T) {
	var received []byte
	mockReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockReceiver.Close()

	cfg := &config.Config{
		DatabaseURL:    "postgres://user:pass@localhost:5432/test",
		AuthToken:      "secret-token",
		DebugReportURL: mockReceiver.URL,
	}

	h := NewDebugReportHandler(DebugReportOptions{
		Config:    cfg,
		Log:       zerolog.Nop(),
		Version:   "test-v0.0.1",
		StartTime: time.Now(),
	})

	body := `{"problem":"audio not working","userAgent":"TestBrowser/1.0"}`
	req := httptest.NewRequest("POST", "/api/v1/debug-report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Submit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify forwarded report structure
	var report map[string]any
	if err := json.Unmarshal(received, &report); err != nil {
		t.Fatalf("failed to parse forwarded report: %v", err)
	}
	if report["report_type"] != "debug_report" {
		t.Errorf("report_type = %v, want 'debug_report'", report["report_type"])
	}

	// Client data preserved
	client, ok := report["client"].(map[string]any)
	if !ok {
		t.Fatal("client section missing")
	}
	if client["problem"] != "audio not working" {
		t.Errorf("client.problem = %v", client["problem"])
	}

	// Server data present with redacted secrets
	server, ok := report["server"].(map[string]any)
	if !ok {
		t.Fatal("server section missing")
	}
	cfg2, ok := server["config"].(map[string]any)
	if !ok {
		t.Fatal("server.config missing")
	}
	if cfg2["AuthToken"] != "***" {
		t.Errorf("AuthToken not redacted: %v", cfg2["AuthToken"])
	}
}

func TestDebugReportDisabledReturns503(t *testing.T) {
	cfg := &config.Config{
		DebugReportURL: "", // disabled
	}
	h := NewDebugReportHandler(DebugReportOptions{
		Config:    cfg,
		Log:       zerolog.Nop(),
		StartTime: time.Now(),
	})

	req := httptest.NewRequest("POST", "/api/v1/debug-report", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.Submit(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
```

**Step 2: Run tests**

Run: `go test ./internal/api/ -run TestDebugReport -v`
Expected: PASS

**Step 3: Commit**

```bash
git add internal/api/debug_report_test.go
git commit -m "test(api): add integration tests for debug report handler"
```

---

### Task 8: Update CLAUDE.md and sample.env

**Files:**
- Modify: `CLAUDE.md` — add `DEBUG_REPORT_URL` to env-only settings documentation
- Modify: `sample.env` — add `DEBUG_REPORT_URL` with description

**Step 1: Add to CLAUDE.md env-only settings section**

Find the section listing env-only settings and add:

```
`DEBUG_REPORT_URL` (debug-receiver endpoint for diagnostic reports, default `https://case.luxprimatech.com/debug/report`; empty string disables)
```

**Step 2: Add to sample.env**

```env
# Debug report forwarding (empty to disable)
# DEBUG_REPORT_URL=https://case.luxprimatech.com/debug/report
```

**Step 3: Add debug-report.html to the web files list in CLAUDE.md**

In the file layout section, add `debug-report.html` to the web files list.

**Step 4: Commit**

```bash
git add CLAUDE.md sample.env
git commit -m "docs: add DEBUG_REPORT_URL to config documentation"
```
