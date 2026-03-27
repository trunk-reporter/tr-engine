package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	trengine "github.com/snarg/tr-engine"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/ingest"
	"github.com/snarg/tr-engine/internal/mqttclient"
	"github.com/snarg/tr-engine/internal/storage"
	"github.com/snarg/tr-engine/internal/transcribe"
	"github.com/snarg/tr-engine/internal/trconfig"
	"golang.org/x/crypto/bcrypt"
)

// version, commit, and buildTime are injected at build time via ldflags.
// See Makefile or build script for usage.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	// CLI flags
	var overrides config.Overrides
	var showVersion bool
	flag.StringVar(&overrides.EnvFile, "env-file", "", "Path to .env file (default: .env)")
	flag.StringVar(&overrides.HTTPAddr, "listen", "", "HTTP listen address (overrides HTTP_ADDR)")
	flag.StringVar(&overrides.LogLevel, "log-level", "", "Log level: debug, info, warn, error (overrides LOG_LEVEL)")
	flag.StringVar(&overrides.DatabaseURL, "database-url", "", "PostgreSQL connection URL (overrides DATABASE_URL)")
	flag.StringVar(&overrides.MQTTBrokerURL, "mqtt-url", "", "MQTT broker URL (overrides MQTT_BROKER_URL)")
	flag.StringVar(&overrides.AudioDir, "audio-dir", "", "Audio file directory (overrides AUDIO_DIR)")
	flag.StringVar(&overrides.WatchDir, "watch-dir", "", "Watch TR audio directory for new files (overrides WATCH_DIR)")
	flag.StringVar(&overrides.TRDir, "tr-dir", "", "Path to trunk-recorder directory for auto-discovery (overrides TR_DIR)")
	flag.StringVar(&overrides.WhisperURL, "whisper-url", "", "Whisper API URL for transcription (overrides WHISPER_URL)")
	flag.StringVar(&overrides.StreamListen, "stream-listen", "", "UDP listen address for simplestream audio (overrides STREAM_LISTEN)")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("%s (commit=%s, built=%s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	// Check for subcommands (export, import)
	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "export":
			runExport(args[1:], overrides)
		case "import":
			runImport(args[1:], overrides)
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", args[0])
			os.Exit(1)
		}
		return
	}

	startTime := time.Now()

	// Config (loads .env automatically, then env vars, then CLI overrides)
	cfg, err := config.Load(overrides)
	if err != nil {
		early := zerolog.New(os.Stderr).With().Timestamp().Logger()
		early.Fatal().Err(err).Msg("failed to load config")
	}
	// TR auto-discovery: read trunk-recorder's config.json + docker-compose.yaml
	var discovered *trconfig.DiscoveryResult
	if cfg.TRDir != "" {
		earlyLog := zerolog.New(os.Stdout).With().Timestamp().Logger()
		discovered, err = trconfig.Discover(cfg.TRDir, earlyLog)
		if err != nil {
			earlyLog.Fatal().Err(err).Str("tr_dir", cfg.TRDir).Msg("failed to read trunk-recorder config")
		}
		// Auto-set WatchDir and TRAudioDir if not explicitly configured
		if cfg.WatchDir == "" {
			cfg.WatchDir = discovered.CaptureDir
		}
		if cfg.TRAudioDir == "" {
			cfg.TRAudioDir = discovered.CaptureDir
		}
	}

	if err := cfg.Validate(); err != nil {
		early := zerolog.New(os.Stderr).With().Timestamp().Logger()
		early.Fatal().Err(err).Msg("invalid config")
	}

	// Logger
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	log := zerolog.New(os.Stdout).With().Timestamp().Logger().Level(level)
	log.Info().
		Str("version", version).
		Str("commit", commit).
		Str("built", buildTime).
		Str("log_level", level.String()).
		Msg("tr-engine starting")

	// Context for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Database
	dbLog := log.With().Str("component", "database").Logger()
	db, err := database.Connect(ctx, cfg.DatabaseURL, dbLog)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer db.Close()

	// Auto-apply schema on fresh database (no-op if tables already exist)
	if err := db.InitSchema(ctx, trengine.SchemaSQL); err != nil {
		log.Fatal().Err(err).Msg("schema initialization failed")
	}

	// Run idempotent schema migrations — fatal on failure since queries depend on these columns
	if err := db.Migrate(ctx); err != nil {
		log.Fatal().Err(err).Msg("schema migration failed (run ALTER TABLE manually or grant ALTER privileges)")
	}

	// Seed admin user if ADMIN_PASSWORD is set and no users exist
	if cfg.AdminPassword != "" {
		count, err := db.CountUsers(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("failed to check user count for seeding")
		} else if count == 0 {
			hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
			if err != nil {
				log.Fatal().Err(err).Msg("failed to hash admin password")
			}
			if _, err := db.CreateUser(ctx, cfg.AdminUsername, string(hash), "admin"); err != nil {
				if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
					log.Info().Str("username", cfg.AdminUsername).Msg("admin user already exists (seeded by another instance)")
				} else {
					log.Fatal().Err(err).Msg("failed to seed admin user")
				}
			} else {
				log.Info().Str("username", cfg.AdminUsername).Msg("admin user seeded")
			}
		}
	} else {
		log.Info().Msg("ADMIN_PASSWORD not set — user auth not configured (legacy token mode)")
	}

	// Audio storage (local disk default, optional S3)
	store, bgServices, err := storage.New(cfg.S3, cfg.AudioDir, log)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize audio storage")
	}
	for _, svc := range bgServices {
		svc.Start()
		defer svc.Stop()
	}
	log.Info().Str("type", store.Type()).Msg("audio storage initialized")

	// Async uploader (only for tiered stores in async mode)
	var s3Uploader *storage.AsyncUploader
	if tiered, ok := store.(*storage.TieredStore); ok && cfg.S3.UploadMode == "async" {
		s3Uploader = storage.NewAsyncUploader(tiered.S3Store(), 500, log)
		s3Uploader.Start(2)
		// Stopped by pipeline.Stop()
	}

	// MQTT (optional — not needed when using watch mode)
	var mqtt *mqttclient.Client
	if cfg.MQTTBrokerURL != "" {
		mqttLog := log.With().Str("component", "mqtt").Logger()
		mqtt, err = mqttclient.Connect(mqttclient.Options{
			BrokerURL: cfg.MQTTBrokerURL,
			ClientID:  cfg.MQTTClientID,
			Topics:    cfg.MQTTTopics,
			Username:  cfg.MQTTUsername,
			Password:  cfg.MQTTPassword,
			Log:       mqttLog,
		})
		if err != nil {
			log.Fatal().Err(err).Msg("failed to connect to mqtt broker")
		}
		defer mqtt.Close()
		log.Info().Str("broker", cfg.MQTTBrokerURL).Str("client_id", cfg.MQTTClientID).Msg("mqtt connected")
	} else {
		log.Info().Msg("mqtt not configured (watch-only mode)")
	}

	// Transcription (optional — build provider based on STT_PROVIDER)
	var transcribeOpts *transcribe.WorkerPoolOptions
	var sttProvider transcribe.Provider
	switch cfg.STTProvider {
	case "whisper":
		if cfg.WhisperURL != "" {
			sttProvider = transcribe.NewWhisperClient(cfg.WhisperURL, cfg.WhisperModel, cfg.WhisperAPIKey, cfg.WhisperTimeout)
		}
	case "elevenlabs":
		if cfg.ElevenLabsAPIKey == "" {
			log.Fatal().Msg("STT_PROVIDER=elevenlabs requires ELEVENLABS_API_KEY")
		}
		sttProvider = transcribe.NewElevenLabsClient(cfg.ElevenLabsAPIKey, cfg.ElevenLabsModel, cfg.ElevenLabsKeyterms, cfg.WhisperTimeout)
	case "deepinfra":
		if cfg.DeepInfraAPIKey == "" {
			log.Fatal().Msg("STT_PROVIDER=deepinfra requires DEEPINFRA_STT_API_KEY")
		}
		sttProvider = transcribe.NewDeepInfraClient(cfg.DeepInfraAPIKey, cfg.DeepInfraModel, cfg.WhisperTimeout)
	case "imbe":
		if cfg.IMBEAsrURL == "" {
			log.Fatal().Msg("STT_PROVIDER=imbe requires IMBE_ASR_URL")
		}
		sttProvider = transcribe.NewIMBEClient(cfg.IMBEAsrURL, cfg.IMBEAsrModel, cfg.WhisperTimeout)
	case "none", "":
		// Transcription explicitly disabled
	default:
		log.Fatal().Str("provider", cfg.STTProvider).Msg("unknown STT_PROVIDER (valid: whisper, elevenlabs, deepinfra, imbe, none)")
	}

	if sttProvider != nil {
		transcribeOpts = &transcribe.WorkerPoolOptions{
			DB:              db,
			AudioDir:        cfg.AudioDir,
			TRAudioDir:      cfg.TRAudioDir,
			Store:           store,
			Provider:        sttProvider,
			ProviderTimeout: cfg.WhisperTimeout,
			Temperature:     cfg.WhisperTemperature,
			Language:        cfg.WhisperLanguage,
			Prompt:          cfg.WhisperPrompt,
			Hotwords:        cfg.WhisperHotwords,
			BeamSize:        cfg.WhisperBeamSize,
			PreprocessAudio: cfg.PreprocessAudio,
			Workers:         cfg.TranscribeWorkers,
			QueueSize:       cfg.TranscribeQueueSize,
			MinDuration:     cfg.TranscribeMinDuration,
			MaxDuration:     cfg.TranscribeMaxDuration,
			Log:             log.With().Str("component", "transcribe").Logger(),

			RepetitionPenalty:             cfg.WhisperRepetitionPenalty,
			NoRepeatNgramSize:             cfg.WhisperNoRepeatNgram,
			ConditionOnPreviousText:       cfg.WhisperConditionOnPrev,
			NoSpeechThreshold:             cfg.WhisperNoSpeechThreshold,
			HallucinationSilenceThreshold: cfg.WhisperHallucinationThreshold,
			MaxNewTokens:                  cfg.WhisperMaxTokens,
			VadFilter:                     cfg.WhisperVadFilter,
		}
		log.Info().
			Str("provider", sttProvider.Name()).
			Str("model", sttProvider.Model()).
			Int("workers", cfg.TranscribeWorkers).
			Msg("transcription enabled")
	}

	// Ingest Pipeline
	pipeline := ingest.NewPipeline(ingest.PipelineOptions{
		DB:               db,
		AudioDir:         cfg.AudioDir,
		TRAudioDir:       cfg.TRAudioDir,
		RawStore:         cfg.RawStore,
		RawIncludeTopics:  cfg.RawIncludeTopics,
		RawExcludeTopics:  cfg.RawExcludeTopics,
		MergeP25Systems:   cfg.MergeP25Systems,
		MQTTInstanceMap:   cfg.MQTTInstanceMap,
		TranscribeOpts:    transcribeOpts,
		TranscribeInclude: cfg.TranscribeIncludeTGIDs,
		TranscribeExclude: cfg.TranscribeExcludeTGIDs,
		RetentionRawMessages:  cfg.RetentionRawMessages,
		RetentionConsoleLogs:  cfg.RetentionConsoleLogs,
		RetentionPluginStatus: cfg.RetentionPluginStatus,
		RetentionCheckpoints:  cfg.RetentionCheckpoints,
		RetentionStaleCalls:   cfg.RetentionStaleCalls,
		StreamListen:      cfg.StreamListen,
		StreamInstanceID:  cfg.StreamInstanceID,
		StreamIdleTimeout: cfg.StreamIdleTimeout,
		StreamOpusBitrate: cfg.StreamOpusBitrate,
		Store:            store,
		S3Uploader:       s3Uploader,
		Log:              log,
	})
	if err := pipeline.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("failed to start ingest pipeline")
	}
	defer pipeline.Stop()

	// Wire MQTT → Pipeline
	if mqtt != nil {
		mqtt.SetMessageHandler(pipeline.HandleMessage)
	}

	// Import talkgroup directory from TR's CSV files (if TR_DIR discovery found any)
	// Also build CSV path maps for talkgroup and unit writeback on edit
	tgCSVPaths := make(map[int]string)
	unitCSVPaths := make(map[int]string)
	if discovered != nil {
		for _, sys := range discovered.Systems {
			// Resolve identity once per system (needed for both TG and unit imports)
			var systemID int
			var identityResolved bool
			if len(sys.Talkgroups) > 0 || len(sys.Units) > 0 {
				identity, idErr := pipeline.ResolveIdentity(ctx, cfg.WatchInstanceID, sys.ShortName)
				if idErr != nil {
					log.Warn().Err(idErr).Str("system", sys.ShortName).Msg("failed to resolve system for CSV import")
					continue
				}
				systemID = identity.SystemID
				identityResolved = true
			}
			if !identityResolved {
				continue
			}

			// Import talkgroups
			if len(sys.Talkgroups) > 0 {
				if cfg.CSVWriteback && sys.CSVPath != "" {
					tgCSVPaths[systemID] = sys.CSVPath
				}
				imported := 0
				for _, tg := range sys.Talkgroups {
					if uErr := db.UpsertTalkgroupDirectory(ctx, systemID, tg.Tgid,
						tg.AlphaTag, tg.Mode, tg.Description, tg.Tag, tg.Category, tg.Priority,
					); uErr != nil {
						log.Warn().Err(uErr).Int("tgid", tg.Tgid).Msg("failed to import talkgroup")
						continue
					}
					imported++
				}
				log.Info().
					Str("system", sys.ShortName).
					Int("imported", imported).
					Int("total", len(sys.Talkgroups)).
					Msg("talkgroup directory imported")

				// Enrich existing heard talkgroups with directory data
				enriched, enrichErr := db.EnrichTalkgroupsFromDirectory(ctx, systemID, 0)
				if enrichErr != nil {
					log.Warn().Err(enrichErr).Int("system_id", systemID).Msg("failed to enrich talkgroups from directory")
				} else if enriched > 0 {
					log.Info().Int64("enriched", enriched).Str("system", sys.ShortName).Msg("heard talkgroups enriched from directory")
				}
			}

			// Import unit tags
			if len(sys.Units) > 0 {
				if cfg.CSVWriteback && sys.UnitCSVPath != "" {
					unitCSVPaths[systemID] = sys.UnitCSVPath
				}
				imported := 0
				for _, u := range sys.Units {
					if uErr := db.ImportUnitTag(ctx, systemID, u.UnitID, u.AlphaTag); uErr != nil {
						log.Warn().Err(uErr).Int("unit_id", u.UnitID).Msg("failed to import unit tag")
						continue
					}
					imported++
				}
				log.Info().
					Str("system", sys.ShortName).
					Int("imported", imported).
					Int("total", len(sys.Units)).
					Msg("unit tags imported")
			}
		}
	}
	if cfg.CSVWriteback && (len(tgCSVPaths) > 0 || len(unitCSVPaths) > 0) {
		log.Info().Int("talkgroup_csvs", len(tgCSVPaths)).Int("unit_csvs", len(unitCSVPaths)).
			Msg("CSV writeback enabled — edits will be written back to TR's CSV files")
	}

	// File watcher (optional — alternative to MQTT ingest)
	if cfg.WatchDir != "" {
		if err := pipeline.StartWatcher(cfg.WatchDir, cfg.WatchInstanceID, cfg.WatchBackfillDays); err != nil {
			log.Fatal().Err(err).Msg("failed to start file watcher")
		}
		log.Info().Str("watch_dir", cfg.WatchDir).Str("instance_id", cfg.WatchInstanceID).Msg("file watcher started")
	}

	// Start live audio streaming if configured
	if cfg.StreamListen != "" {
		src := audio.NewSimplestreamSource(cfg.StreamListen, cfg.StreamSampleRate)
		src.SetLogger(log)
		if input := pipeline.AudioRouterInput(); input != nil {
			go func() {
				if err := src.Start(ctx, input); err != nil {
					log.Error().Err(err).Msg("simplestream source failed")
				}
			}()
			log.Info().Str("addr", cfg.StreamListen).Msg("live audio streaming enabled (simplestream UDP)")
		}
	}

	// Auth status
	if !cfg.AuthEnabled {
		log.Warn().Msg("AUTH_ENABLED=false — API authentication is disabled, all endpoints are open")
	} else if cfg.AuthTokenGenerated {
		log.Info().Str("token", cfg.AuthToken).Msg("AUTH_TOKEN auto-generated (set AUTH_TOKEN in .env for a persistent token)")
	} else {
		log.Info().Msg("AUTH_TOKEN loaded from configuration")
	}
	if cfg.AuthEnabled && cfg.WriteToken != "" {
		log.Info().Msg("write protection enabled (WRITE_TOKEN set)")
	} else if cfg.AuthEnabled {
		log.Warn().Msg("WRITE_TOKEN not set — write endpoints accept the read token")
	}
	if cfg.JWTSecret != "" {
		log.Info().Msg("JWT user authentication enabled")
	}

	// Detect ingest modes and Docker for update checker
	var ingestModes []string
	if cfg.MQTTBrokerURL != "" {
		ingestModes = append(ingestModes, "mqtt")
	}
	if cfg.WatchDir != "" {
		ingestModes = append(ingestModes, "watch")
	}
	ingestModes = append(ingestModes, "upload") // always available
	_, dockerErr := os.Stat("/.dockerenv")
	isDocker := dockerErr == nil

	// HTTP Server
	httpLog := log.With().Str("component", "http").Logger()
	srv := api.NewServer(api.ServerOptions{
		Config:         cfg,
		DB:             db,
		MQTT:           mqtt,
		Live:           pipeline,
		Uploader:       pipeline, // Pipeline implements CallUploader via ProcessUpload
		AudioStreamer:  pipeline, // Pipeline implements AudioStreamer via AudioBus
		Store:          store,
		WebFiles:       trengine.WebFiles,
		OpenAPISpec:    trengine.OpenAPISpec,
		Version:        fmt.Sprintf("%s (commit=%s, built=%s)", version, commit, buildTime),
		StartTime:      startTime,
		Log:            httpLog,
		OnSystemMerge:  pipeline.RewriteSystemID,
		TGCSVPaths:     tgCSVPaths,
		UnitCSVPaths:   unitCSVPaths,
		UpdateCheckURL: func() string { if cfg.UpdateCheck { return cfg.UpdateCheckURL }; return "" }(),
		IngestModes:    strings.Join(ingestModes, ","),
		IsDocker:       isDocker,
	})
	srv.StartUpdateChecker(ctx)

	// Start HTTP server in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	log.Info().
		Str("listen", cfg.HTTPAddr).
		Str("version", version).
		Dur("startup_ms", time.Since(startTime)).
		Msg("tr-engine ready")

	// Wait for shutdown signal or server error
	select {
	case <-ctx.Done():
		log.Info().Msg("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			log.Error().Err(err).Msg("http server error")
		}
	}

	// Graceful shutdown with 10s timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("http server shutdown error")
	}

	log.Info().Msg("tr-engine stopped")
}
