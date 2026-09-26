package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
)

// sanitizeConfig returns a map of all config fields with secrets redacted
// and URLs stripped of credentials, suitable for inclusion in debug reports.
func sanitizeConfig(cfg *config.Config) map[string]any {
	return map[string]any{
		// Database & MQTT
		"DatabaseURL":    sanitizeURL(cfg.DatabaseURL),
		"MQTTBrokerURL":  sanitizeURL(cfg.MQTTBrokerURL),
		"MQTTTopics":     cfg.MQTTTopics,
		"MQTTInstanceMap": cfg.MQTTInstanceMap,
		"MQTTClientID":   cfg.MQTTClientID,
		"MQTTUsername":    redact(cfg.MQTTUsername),
		"MQTTPassword":   redact(cfg.MQTTPassword),

		// Audio & file paths
		"AudioDir":   cfg.AudioDir,
		"TRAudioDir": cfg.TRAudioDir,

		// File-watch ingest
		"WatchDir":          cfg.WatchDir,
		"WatchInstanceID":   cfg.WatchInstanceID,
		"WatchBackfillDays": cfg.WatchBackfillDays,

		// HTTP upload ingest
		"UploadInstanceID": cfg.UploadInstanceID,

		// Live audio streaming
		"StreamListen":      cfg.StreamListen,
		"StreamInstanceID":  cfg.StreamInstanceID,
		"StreamSampleRate":  cfg.StreamSampleRate,
		"StreamOpusBitrate": cfg.StreamOpusBitrate,
		"StreamMaxClients":  cfg.StreamMaxClients,
		"StreamIdleTimeout": cfg.StreamIdleTimeout.String(),

		// TR auto-discovery
		"TRDir":        cfg.TRDir,
		"CSVWriteback": cfg.CSVWriteback,

		// P25 merging
		"MergeP25Systems": cfg.MergeP25Systems,

		// Unit tag suggestions
		"UnitTagSuggestions":         cfg.UnitTagSuggestions,
		"UnitTagSuggestionsMinCalls": cfg.UnitTagSuggestionsMinCalls,
		"UnitTagSuggestionsMinShare": cfg.UnitTagSuggestionsMinShare,
		"UnitTagSuggestionsInterval": cfg.UnitTagSuggestionsInterval.String(),

		// HTTP server
		"HTTPAddr":     cfg.HTTPAddr,
		"ReadTimeout":  cfg.ReadTimeout.String(),
		"WriteTimeout": cfg.WriteTimeout.String(),
		"IdleTimeout":  cfg.IdleTimeout.String(),

		// Rate limits and proxies (access control itself lives in the
		// database, not in the config)
		"RateLimitRPS":   cfg.RateLimitRPS,
		"RateLimitBurst": cfg.RateLimitBurst,
		"TrustedProxies": cfg.TrustedProxies,
		"LogLevel":       cfg.LogLevel,

		// Raw archival
		"RawStore":         cfg.RawStore,
		"RawIncludeTopics": cfg.RawIncludeTopics,
		"RawExcludeTopics": cfg.RawExcludeTopics,

		// Transcription: Whisper
		"STTProvider":                     cfg.STTProvider,
		"WhisperURL":                      sanitizeURL(cfg.WhisperURL),
		"WhisperAPIKey":                   redact(cfg.WhisperAPIKey),
		"WhisperModel":                    cfg.WhisperModel,
		"WhisperTimeout":                  cfg.WhisperTimeout.String(),
		"WhisperTemperature":              cfg.WhisperTemperature,
		"WhisperLanguage":                 cfg.WhisperLanguage,
		"WhisperPrompt":                   cfg.WhisperPrompt,
		"WhisperHotwords":                 cfg.WhisperHotwords,
		"WhisperBeamSize":                 cfg.WhisperBeamSize,
		"WhisperRepetitionPenalty":        cfg.WhisperRepetitionPenalty,
		"WhisperNoRepeatNgram":            cfg.WhisperNoRepeatNgram,
		"WhisperConditionOnPrev":          cfg.WhisperConditionOnPrev,
		"WhisperNoSpeechThreshold":        cfg.WhisperNoSpeechThreshold,
		"WhisperHallucinationThreshold":   cfg.WhisperHallucinationThreshold,
		"WhisperMaxTokens":               cfg.WhisperMaxTokens,
		"WhisperVadFilter":               cfg.WhisperVadFilter,

		// Transcription: ElevenLabs
		"ElevenLabsAPIKey":   redact(cfg.ElevenLabsAPIKey),
		"ElevenLabsModel":    cfg.ElevenLabsModel,
		"ElevenLabsKeyterms": cfg.ElevenLabsKeyterms,

		// Transcription: DeepInfra
		"DeepInfraAPIKey": redact(cfg.DeepInfraAPIKey),
		"DeepInfraModel":  cfg.DeepInfraModel,

		// LLM post-processing
		"LLMUrl":     sanitizeURL(cfg.LLMUrl),
		"LLMModel":   cfg.LLMModel,
		"LLMTimeout": cfg.LLMTimeout.String(),

		// Metrics
		"MetricsEnabled": cfg.MetricsEnabled,

		// Update checker
		"UpdateCheck":    cfg.UpdateCheck,
		"UpdateCheckURL": cfg.UpdateCheckURL,

		// Audio preprocessing
		"PreprocessAudio": cfg.PreprocessAudio,

		// Retention / maintenance
		"RetentionRawMessages":      cfg.RetentionRawMessages.String(),
		"RetentionConsoleLogs":      cfg.RetentionConsoleLogs.String(),
		"RetentionPluginStatus":     cfg.RetentionPluginStatus.String(),
		"RetentionTrunkingMessages": cfg.RetentionTrunkingMessages.String(),
		"RetentionCheckpoints":      cfg.RetentionCheckpoints.String(),
		"RetentionStaleCalls":       cfg.RetentionStaleCalls.String(),
		"RetentionAuditLog":         cfg.RetentionAuditLog.String(),

		// Transcription worker pool
		"TranscribeWorkers":     cfg.TranscribeWorkers,
		"TranscribeQueueSize":   cfg.TranscribeQueueSize,
		"TranscribeMinDuration": cfg.TranscribeMinDuration,
		"TranscribeMaxDuration": cfg.TranscribeMaxDuration,

		// Transcription talkgroup filtering
		"TranscribeIncludeTGIDs": cfg.TranscribeIncludeTGIDs,
		"TranscribeExcludeTGIDs": cfg.TranscribeExcludeTGIDs,

		// S3
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

		// Debug report forwarding
		"DebugReportURL": cfg.DebugReportURL,
	}
}

// redact returns "***" for non-empty strings, empty string for empty.
func redact(s string) string {
	if s != "" {
		return "***"
	}
	return ""
}

// sanitizeURL parses a URL and strips user credentials (username/password).
// Returns the original string if empty or unparseable.
func sanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// secretFieldPattern matches the names of fields whose values are secrets
// (§15): API keys, tokens, passwords, secrets, auth settings, credentials.
var secretFieldPattern = regexp.MustCompile(`(?i)key|token|pass|secret|auth|credential`)

// redactReport returns a copy of a decoded JSON value (maps, slices,
// strings, numbers, booleans) that is safe to send to a third party, at any
// depth: every field whose name matches secretFieldPattern has a non-empty
// value replaced by "***" (booleans are kept), and every string that is a
// URL loses its userinfo, query and fragment.
func redactReport(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretFieldPattern.MatchString(k) {
				out[k] = redactSecretValue(val)
			} else {
				out[k] = redactReport(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactReport(val)
		}
		return out
	case string:
		return stripURLSecrets(t)
	}
	return v
}

// redactSecretValue hides the value of a secret-named field. Empty and null
// values stay as they are, so a report still shows that a secret is unset.
func redactSecretValue(v any) any {
	switch t := v.(type) {
	case nil, bool:
		return t
	case string:
		if t == "" {
			return ""
		}
	}
	return "***"
}

// stripURLSecrets removes userinfo, query and fragment from a string that is
// an absolute URL (scheme and host); other strings are returned unchanged.
func stripURLSecrets(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// MQTTStatus provides MQTT connection state for the debug report handler.
type MQTTStatus interface {
	IsConnected() bool
}

// DebugReportHandler handles POST /api/v1/debug-report requests.
type DebugReportHandler struct {
	db            *database.DB
	cfg           *config.Config
	live          LiveDataSource
	audioStreamer AudioStreamer
	log           zerolog.Logger
	version       string
	startTime     time.Time
	mqtt          MQTTStatus
	forwardURL    string
	disabled      bool
	trConfigPath  string
}

// DebugReportOptions configures a DebugReportHandler.
type DebugReportOptions struct {
	DB            *database.DB
	Config        *config.Config
	Live          LiveDataSource
	AudioStreamer AudioStreamer
	MQTT          MQTTStatus
	Log           zerolog.Logger
	Version       string
	StartTime     time.Time
}

// NewDebugReportHandler creates a new debug report handler.
func NewDebugReportHandler(opts DebugReportOptions) *DebugReportHandler {
	h := &DebugReportHandler{
		db:           opts.DB,
		cfg:          opts.Config,
		live:         opts.Live,
		audioStreamer: opts.AudioStreamer,
		log:          opts.Log,
		version:      opts.Version,
		startTime:    opts.StartTime,
		mqtt:         opts.MQTT,
		forwardURL:   opts.Config.DebugReportURL,
		disabled:     opts.Config.DebugReportDisable,
	}
	if opts.Config.TRDir != "" {
		h.trConfigPath = filepath.Join(opts.Config.TRDir, "config.json")
	}
	return h
}

// Submit handles POST /api/v1/debug-report.
func (h *DebugReportHandler) Submit(w http.ResponseWriter, r *http.Request) {
	if h.disabled || h.forwardURL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"debug reports disabled"}`))
		return
	}

	// Read client body with 1MB limit
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"failed to read request body"}`))
		return
	}

	var clientData map[string]any
	if err := json.Unmarshal(body, &clientData); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid JSON"}`))
		return
	}

	// Build combined report
	report := map[string]any{
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
		"report_type":    "debug_report",
		"version":        h.version,
		"uptime_seconds": int64(time.Since(h.startTime).Seconds()),
		"client":         clientData,
		"server":         h.collectServerData(r.Context()),
	}

	reportJSON, err := redactedJSON(report)
	if err != nil {
		h.log.Error().Err(err).Msg("failed to marshal debug report")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to build report"}`))
		return
	}

	// Forward to debug receiver
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.forwardURL, bytes.NewReader(reportJSON))
	if err != nil {
		h.log.Error().Err(err).Msg("failed to create forward request")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"failed to forward report"}`))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.log.Error().Err(err).Str("url", h.forwardURL).Msg("failed to forward debug report")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"failed to forward report"}`))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.log.Warn().Int("status", resp.StatusCode).Msg("debug receiver returned non-2xx")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"debug receiver rejected report"}`))
		return
	}

	h.log.Info().Msg("debug report forwarded successfully")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"ok":true}`))
}

// redactedJSON encodes a report after passing all of it, client part
// included, through redactReport. A JSON round trip first turns every
// struct into maps, so redaction reaches every field at every depth.
func redactedJSON(report map[string]any) ([]byte, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(redactReport(decoded))
}

// collectServerData gathers server-side diagnostics for the debug report.
func (h *DebugReportHandler) collectServerData(ctx context.Context) map[string]any {
	data := map[string]any{
		"config":         sanitizeConfig(h.cfg),
		"environment":    collectEnvironment(),
		"mqtt_connected": h.mqtt != nil && h.mqtt.IsConnected(),
	}

	if h.live != nil {
		data["tr_instances"] = h.live.TRInstanceStatus()
		data["ingest_metrics"] = h.live.IngestMetrics()
		data["watcher_status"] = h.live.WatcherStatus()
		data["transcription_status"] = h.live.TranscriptionStatus()
		data["transcription_queue"] = h.live.TranscriptionQueueStats()
		data["maintenance"] = h.live.MaintenanceStatus()
	}

	if h.audioStreamer != nil {
		data["audio_stream"] = h.audioStreamer.AudioStreamStatus()
		data["audio_jitter"] = h.audioStreamer.AudioJitterStats()
	}

	if h.db != nil {
		stat := h.db.Pool.Stat()
		data["database_pool"] = map[string]any{
			"max_conns":           stat.MaxConns(),
			"total_conns":         stat.TotalConns(),
			"acquired_conns":     stat.AcquiredConns(),
			"idle_conns":         stat.IdleConns(),
			"empty_acquire_count": stat.EmptyAcquireCount(),
		}
		data["console_messages"] = h.queryConsoleLogs(ctx)
	}

	if h.trConfigPath != "" {
		if trCfg := h.readTRConfig(); trCfg != nil {
			data["tr_config"] = trCfg
		}
	}

	return data
}

// queryConsoleLogs fetches recent warn/error console messages from the last hour.
func (h *DebugReportHandler) queryConsoleLogs(ctx context.Context) any {
	oneHourAgo := time.Now().Add(-1 * time.Hour)

	warnSev := "warn"
	warnMsgs, _, err := h.db.ListConsoleMessages(ctx, database.ConsoleMessageFilter{
		Severity:  &warnSev,
		StartTime: &oneHourAgo,
		Limit:     200,
	})
	if err != nil {
		h.log.Debug().Err(err).Msg("failed to query warn console messages")
		return nil
	}

	errorSev := "error"
	errorMsgs, _, err := h.db.ListConsoleMessages(ctx, database.ConsoleMessageFilter{
		Severity:  &errorSev,
		StartTime: &oneHourAgo,
		Limit:     200,
	})
	if err != nil {
		h.log.Debug().Err(err).Msg("failed to query error console messages")
		return nil
	}

	combined := make([]database.ConsoleMessageAPI, 0, len(warnMsgs)+len(errorMsgs))
	combined = append(combined, warnMsgs...)
	combined = append(combined, errorMsgs...)

	if len(combined) == 0 {
		return nil
	}
	return combined
}

// readTRConfig reads and parses the trunk-recorder config.json file.
func (h *DebugReportHandler) readTRConfig() any {
	data, err := os.ReadFile(h.trConfigPath)
	if err != nil {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return parsed
}
