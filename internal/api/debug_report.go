package api

import (
	"net/url"

	"github.com/snarg/tr-engine/internal/config"
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

		// HTTP server
		"HTTPAddr":     cfg.HTTPAddr,
		"ReadTimeout":  cfg.ReadTimeout.String(),
		"WriteTimeout": cfg.WriteTimeout.String(),
		"IdleTimeout":  cfg.IdleTimeout.String(),

		// Auth
		"AuthEnabled":        cfg.AuthEnabled,
		"AuthToken":          redact(cfg.AuthToken),
		"AuthTokenGenerated": cfg.AuthTokenGenerated,
		"WriteToken":         redact(cfg.WriteToken),
		"RateLimitRPS":       cfg.RateLimitRPS,
		"RateLimitBurst":     cfg.RateLimitBurst,
		"CORSOrigins":        cfg.CORSOrigins,
		"LogLevel":           cfg.LogLevel,

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
		"RetentionRawMessages":  cfg.RetentionRawMessages.String(),
		"RetentionConsoleLogs":  cfg.RetentionConsoleLogs.String(),
		"RetentionPluginStatus": cfg.RetentionPluginStatus.String(),
		"RetentionCheckpoints":  cfg.RetentionCheckpoints.String(),
		"RetentionStaleCalls":   cfg.RetentionStaleCalls.String(),

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
