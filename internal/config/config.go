package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL   string `env:"DATABASE_URL,required"`
	MQTTBrokerURL string `env:"MQTT_BROKER_URL"`
	MQTTTopics       string `env:"MQTT_TOPICS" envDefault:"#"`
	MQTTInstanceMap  string `env:"MQTT_INSTANCE_MAP"` // "prefix:instance_id,prefix:instance_id"
	MQTTClientID  string `env:"MQTT_CLIENT_ID" envDefault:"tr-engine"`
	MQTTUsername  string `env:"MQTT_USERNAME"`
	MQTTPassword  string `env:"MQTT_PASSWORD"`

	AudioDir   string `env:"AUDIO_DIR" envDefault:"./audio"`
	TRAudioDir string `env:"TR_AUDIO_DIR"`

	// File-watch ingest mode (alternative to MQTT)
	WatchDir          string `env:"WATCH_DIR"`
	WatchInstanceID   string `env:"WATCH_INSTANCE_ID" envDefault:"file-watch"`
	WatchBackfillDays int    `env:"WATCH_BACKFILL_DAYS" envDefault:"7"`

	// HTTP upload ingest mode (rdio-scanner / OpenMHz compatible)
	UploadInstanceID string `env:"UPLOAD_INSTANCE_ID" envDefault:"http-upload"`

	// Live audio streaming (simplestream UDP ingest → WebSocket relay)
	StreamListen      string        `env:"STREAM_LISTEN"`                              // UDP listen address, e.g. ":9123". Feature disabled if empty.
	StreamInstanceID  string        `env:"STREAM_INSTANCE_ID" envDefault:"trunk-recorder"` // TR instance ID for identity resolution
	StreamSampleRate  int           `env:"STREAM_SAMPLE_RATE" envDefault:"8000"`        // Default PCM sample rate (8000 P25, 16000 analog)
	StreamOpusBitrate int           `env:"STREAM_OPUS_BITRATE" envDefault:"16000"`      // Opus encoder bitrate in bps
	StreamMaxClients  int           `env:"STREAM_MAX_CLIENTS" envDefault:"50"`          // Max concurrent WebSocket listeners
	StreamIdleTimeout time.Duration `env:"STREAM_IDLE_TIMEOUT" envDefault:"30s"`        // Tear down per-TG encoder after idle

	// TR auto-discovery (reads trunk-recorder's config.json + docker-compose.yaml)
	TRDir        string `env:"TR_DIR"`
	CSVWriteback bool   `env:"CSV_WRITEBACK" envDefault:"false"` // write edits back to TR's CSV files on disk

	// P25 system merging: when true (default), systems with the same sysid/wacn
	// are auto-merged into one system with multiple sites. Set to false to keep
	// each TR instance's systems separate even if they share sysid/wacn.
	MergeP25Systems bool `env:"MERGE_P25_SYSTEMS" envDefault:"true"`

	HTTPAddr     string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadTimeout  time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"5s"`
	WriteTimeout time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	IdleTimeout  time.Duration `env:"HTTP_IDLE_TIMEOUT" envDefault:"120s"`

	AuthEnabled        bool   `env:"AUTH_ENABLED" envDefault:"true"` // set to false to disable all API auth
	AuthToken          string `env:"AUTH_TOKEN"`
	AuthTokenGenerated bool   // true when auto-generated (not from env/config)
	WriteToken         string `env:"WRITE_TOKEN"` // separate token for write operations; if not set, writes use AuthToken

	// User authentication (optional — disabled when ADMIN_PASSWORD is empty)
	JWTSecret     string `env:"JWT_SECRET"`      // HMAC key for JWT signing; auto-generated if empty (sessions lost on restart)
	AdminUsername string `env:"ADMIN_USERNAME" envDefault:"admin"` // default admin username seeded on first run
	AdminPassword string `env:"ADMIN_PASSWORD"` // if set, seeds admin user on first run and enables JWT auth
	RateLimitRPS   float64 `env:"RATE_LIMIT_RPS" envDefault:"20"`
	RateLimitBurst int     `env:"RATE_LIMIT_BURST" envDefault:"40"`
	CORSOrigins string `env:"CORS_ORIGINS"` // comma-separated allowed origins; empty = allow all (*)
	LogLevel    string `env:"LOG_LEVEL" envDefault:"info"`

	RawStore         bool   `env:"RAW_STORE" envDefault:"true"`
	RawIncludeTopics string `env:"RAW_INCLUDE_TOPICS"`
	RawExcludeTopics string `env:"RAW_EXCLUDE_TOPICS"`

	// Transcription (optional — disabled when no STT provider is configured)
	STTProvider        string `env:"STT_PROVIDER" envDefault:"whisper"`
	WhisperURL         string        `env:"WHISPER_URL"`
	WhisperAPIKey      string        `env:"WHISPER_API_KEY"`
	WhisperModel       string        `env:"WHISPER_MODEL"`
	WhisperTimeout     time.Duration `env:"WHISPER_TIMEOUT" envDefault:"30s"`
	WhisperTemperature float64       `env:"WHISPER_TEMPERATURE" envDefault:"0.1"`
	WhisperLanguage    string        `env:"WHISPER_LANGUAGE" envDefault:"en"`
	WhisperPrompt      string        `env:"WHISPER_PROMPT"`
	WhisperHotwords    string        `env:"WHISPER_HOTWORDS"`
	WhisperBeamSize    int           `env:"WHISPER_BEAM_SIZE" envDefault:"0"`

	// Anti-hallucination parameters (require custom whisper-server or compatible endpoint)
	WhisperRepetitionPenalty          float64 `env:"WHISPER_REPETITION_PENALTY" envDefault:"0"`
	WhisperNoRepeatNgram              int     `env:"WHISPER_NO_REPEAT_NGRAM" envDefault:"0"`
	WhisperConditionOnPrev            *bool   `env:"WHISPER_CONDITION_ON_PREV"`
	WhisperNoSpeechThreshold          float64 `env:"WHISPER_NO_SPEECH_THRESHOLD" envDefault:"0"`
	WhisperHallucinationThreshold     float64 `env:"WHISPER_HALLUCINATION_THRESHOLD" envDefault:"0"`
	WhisperMaxTokens                  int     `env:"WHISPER_MAX_TOKENS" envDefault:"0"`
	WhisperVadFilter                  bool    `env:"WHISPER_VAD_FILTER" envDefault:"false"`

	// ElevenLabs STT (alternative to Whisper; used when STT_PROVIDER=elevenlabs)
	ElevenLabsAPIKey   string `env:"ELEVENLABS_API_KEY"`
	ElevenLabsModel    string `env:"ELEVENLABS_MODEL" envDefault:"scribe_v2"`
	ElevenLabsKeyterms string `env:"ELEVENLABS_KEYTERMS"`

	// DeepInfra STT (alternative to Whisper; used when STT_PROVIDER=deepinfra)
	DeepInfraAPIKey string `env:"DEEPINFRA_STT_API_KEY"`
	DeepInfraModel  string `env:"DEEPINFRA_STT_MODEL" envDefault:"openai/whisper-large-v3-turbo"`

	// IMBE ASR (alternative to Whisper; used when STT_PROVIDER=imbe)
	IMBEAsrURL   string `env:"IMBE_ASR_URL"`
	IMBEAsrModel string `env:"IMBE_ASR_MODEL" envDefault:"imbe"`

	// LLM post-processing (optional — disabled when LLM_URL is empty; not yet implemented)
	LLMUrl     string        `env:"LLM_URL"`
	LLMModel   string        `env:"LLM_MODEL"`
	LLMTimeout time.Duration `env:"LLM_TIMEOUT" envDefault:"30s"`

	// Prometheus metrics endpoint at /metrics (enabled by default)
	MetricsEnabled bool `env:"METRICS_ENABLED" envDefault:"true"`

	// Update checker (enabled by default — set UPDATE_CHECK=false to disable)
	UpdateCheck    bool   `env:"UPDATE_CHECK" envDefault:"true"`
	UpdateCheckURL string `env:"UPDATE_CHECK_URL" envDefault:"https://updates.luxprimatech.com/check"`

	// Audio preprocessing (requires sox in PATH)
	PreprocessAudio bool `env:"PREPROCESS_AUDIO" envDefault:"false"`

	// Retention / maintenance
	RetentionRawMessages  time.Duration `env:"RETENTION_RAW_MESSAGES" envDefault:"168h"`   // 7d
	RetentionConsoleLogs  time.Duration `env:"RETENTION_CONSOLE_LOGS" envDefault:"720h"`   // 30d
	RetentionPluginStatus time.Duration `env:"RETENTION_PLUGIN_STATUS" envDefault:"720h"`  // 30d
	RetentionCheckpoints  time.Duration `env:"RETENTION_CHECKPOINTS" envDefault:"168h"`    // 7d
	RetentionStaleCalls   time.Duration `env:"RETENTION_STALE_CALLS" envDefault:"1h"`

	// Transcription worker pool
	TranscribeWorkers     int     `env:"TRANSCRIBE_WORKERS" envDefault:"2"`
	TranscribeQueueSize   int     `env:"TRANSCRIBE_QUEUE_SIZE" envDefault:"500"`
	TranscribeMinDuration float64 `env:"TRANSCRIBE_MIN_DURATION" envDefault:"1.0"`
	TranscribeMaxDuration float64 `env:"TRANSCRIBE_MAX_DURATION" envDefault:"300"`

	// Transcription talkgroup filtering (plain "24513", numeric "1:24513", or name-based "butco:24513")
	TranscribeIncludeTGIDs string `env:"TRANSCRIBE_INCLUDE_TGIDS"` // allowlist: only transcribe these TGIDs
	TranscribeExcludeTGIDs string `env:"TRANSCRIBE_EXCLUDE_TGIDS"` // denylist: skip these TGIDs

	// S3 audio storage (optional — local disk used when S3_BUCKET is empty)
	S3 S3Config

	// Debug report forwarding
	DebugReportURL     string `env:"DEBUG_REPORT_URL" envDefault:"https://case.luxprimatech.com/debug/report"`
	DebugReportDisable bool   `env:"DEBUG_REPORT_DISABLE" envDefault:"false"`
}

// S3Config holds S3-compatible object storage settings for audio files.
// All fields are optional — S3 is disabled when Bucket is empty.
type S3Config struct {
	Bucket         string        `env:"S3_BUCKET"`
	Endpoint       string        `env:"S3_ENDPOINT"`
	Region         string        `env:"S3_REGION" envDefault:"us-east-1"`
	AccessKey      string        `env:"S3_ACCESS_KEY"`
	SecretKey      string        `env:"S3_SECRET_KEY"`
	Prefix         string        `env:"S3_PREFIX"`
	PresignExpiry  time.Duration `env:"S3_PRESIGN_EXPIRY" envDefault:"1h"`
	LocalCache     bool          `env:"S3_LOCAL_CACHE" envDefault:"true"`
	CacheRetention time.Duration `env:"S3_CACHE_RETENTION" envDefault:"720h"` // 30d
	CacheMaxGB     int           `env:"S3_CACHE_MAX_GB" envDefault:"0"`
	UploadMode     string        `env:"S3_UPLOAD_MODE" envDefault:"async"`
}

// Enabled reports whether S3 audio storage is configured.
func (c S3Config) Enabled() bool { return c.Bucket != "" }

// Validate checks that at least one ingest source (MQTT, watch directory, or TR auto-discovery) is configured.
func (c *Config) Validate() error {
	if c.MQTTBrokerURL == "" && c.WatchDir == "" && c.TRDir == "" {
		return fmt.Errorf("at least one of MQTT_BROKER_URL, WATCH_DIR, or TR_DIR must be set")
	}
	if c.S3.Enabled() && c.S3.UploadMode != "async" && c.S3.UploadMode != "sync" {
		return fmt.Errorf("S3_UPLOAD_MODE must be \"async\" or \"sync\", got %q", c.S3.UploadMode)
	}
	return nil
}

// Overrides holds CLI flag values that take priority over env vars.
type Overrides struct {
	EnvFile       string
	HTTPAddr      string
	LogLevel      string
	DatabaseURL   string
	MQTTBrokerURL string
	AudioDir      string
	WatchDir      string
	TRDir         string
	WhisperURL    string
	StreamListen  string
}

// Load reads configuration from .env file, environment variables, and CLI overrides.
// Priority: CLI flags > environment variables > .env file > struct defaults.
func Load(overrides Overrides) (*Config, error) {
	// Load .env file (silent if missing)
	envFile := overrides.EnvFile
	if envFile == "" {
		envFile = ".env"
	}
	if _, err := os.Stat(envFile); err == nil {
		_ = godotenv.Load(envFile)
	}

	// Parse environment variables into config struct
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}

	// Apply CLI overrides (non-empty values win)
	if overrides.HTTPAddr != "" {
		cfg.HTTPAddr = overrides.HTTPAddr
	}
	if overrides.LogLevel != "" {
		cfg.LogLevel = overrides.LogLevel
	}
	if overrides.DatabaseURL != "" {
		cfg.DatabaseURL = overrides.DatabaseURL
	}
	if overrides.MQTTBrokerURL != "" {
		cfg.MQTTBrokerURL = overrides.MQTTBrokerURL
	}
	if overrides.AudioDir != "" {
		cfg.AudioDir = overrides.AudioDir
	}
	if overrides.WatchDir != "" {
		cfg.WatchDir = overrides.WatchDir
	}
	if overrides.TRDir != "" {
		cfg.TRDir = overrides.TRDir
	}
	if overrides.WhisperURL != "" {
		cfg.WhisperURL = overrides.WhisperURL
	}
	if overrides.StreamListen != "" {
		cfg.StreamListen = overrides.StreamListen
	}

	// Auto-generate JWT_SECRET if not configured. A random secret means all
	// sessions are invalidated on restart — set JWT_SECRET in .env for persistence.
	if cfg.JWTSecret == "" && cfg.AdminPassword != "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err == nil {
			cfg.JWTSecret = base64.URLEncoding.EncodeToString(b)
		}
	}

	// When auth is explicitly disabled, clear any tokens so middleware passes everything through.
	if !cfg.AuthEnabled {
		cfg.AuthToken = ""
		cfg.WriteToken = ""
	} else if cfg.AuthToken == "" {
		// Auto-generate AUTH_TOKEN if not configured. This ensures the API is always
		// protected from automated scanners. Web pages get the token injected via auth.js.
		// The token changes on each restart; set AUTH_TOKEN in .env for a persistent one.
		b := make([]byte, 32)
		if _, err := rand.Read(b); err == nil {
			cfg.AuthToken = base64.URLEncoding.EncodeToString(b)
			cfg.AuthTokenGenerated = true
		}
	}

	return cfg, nil
}
