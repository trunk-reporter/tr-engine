package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
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
	"github.com/snarg/tr-engine/internal/unittags"
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

	// Check for subcommands (export, import, keys, access)
	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "export":
			runExport(args[1:], overrides)
		case "import":
			runImport(args[1:], overrides)
		case "keys":
			os.Exit(runKeys(args[1:], overrides))
		case "access":
			os.Exit(runAccess(args[1:], overrides))
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

	// Auto-apply schema on fresh database (no-op if tables already exist).
	// freshDatabase: this process created the schema, so there is no old
	// auth configuration to carry over.
	freshDatabase, err := db.ApplySchemaIfEmpty(ctx, trengine.SchemaSQL)
	if err != nil {
		log.Fatal().Err(err).Msg("schema initialization failed")
	}

	// Run idempotent schema migrations — fatal on failure since queries depend on these columns
	if err := db.Migrate(ctx); err != nil {
		log.Fatal().Err(err).Msg("schema migration failed (run ALTER TABLE manually or grant ALTER privileges)")
	}

	// One-time upgrade fixup (first start only): older versions left tag edits of
	// CSV-sourced talkgroups/units marked 'csv', which CSV imports now overwrite.
	// Mark them 'manual' before the TR_DIR import below and before ingest starts.
	// Fatal on failure: continuing would silently replace those edits.
	keepPreUpgradeTagEdits(ctx, db, discovered, cfg.WatchInstanceID, log)

	// Access control: the one-time legacy import, warnings for the removed
	// auth variables, the ticket secret and the bootstrap admin key.
	_, dockerErr := os.Stat("/.dockerenv")
	isDocker := dockerErr == nil
	if err := setupAuth(ctx, db, cfg, freshDatabase, isDocker, os.Stderr, log); err != nil {
		log.Fatal().Err(err).Msg("auth setup failed")
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
			wc := transcribe.NewWhisperClient(cfg.WhisperURL, cfg.WhisperModel, cfg.WhisperAPIKey, cfg.WhisperTimeout)
			wc.SetLogger(log.With().Str("component", "transcribe").Logger())
			sttProvider = wc
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
		RetentionRawMessages:      cfg.RetentionRawMessages,
		RetentionConsoleLogs:      cfg.RetentionConsoleLogs,
		RetentionPluginStatus:     cfg.RetentionPluginStatus,
		RetentionTrunkingMessages: cfg.RetentionTrunkingMessages,
		RetentionCheckpoints:      cfg.RetentionCheckpoints,
		RetentionStaleCalls:       cfg.RetentionStaleCalls,
		RetentionAuditLog:         cfg.RetentionAuditLog,
		StreamListen:      cfg.StreamListen,
		StreamInstanceID:  cfg.StreamInstanceID,
		StreamSourceMap:   cfg.StreamSourceMap,
		WatchInstanceID:   cfg.WatchInstanceID,
		UploadInstanceID:  cfg.UploadInstanceID,
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
	var trCSVs []*trCSVFile
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
				_ = importTRTalkgroups(ctx, db, systemID, sys.ShortName, sys.Talkgroups, log)
			}

			// Import unit tags
			if len(sys.Units) > 0 {
				if cfg.CSVWriteback && sys.UnitCSVPath != "" {
					unitCSVPaths[systemID] = sys.UnitCSVPath
				}
				_ = importTRUnitTags(ctx, db, systemID, sys.ShortName, sys.Units, log)
			}

			// Re-import the CSVs when they change: tr-engine reads them only here,
			// and MQTT no longer updates CSV-sourced tags.
			if sys.CSVPath != "" {
				trCSVs = append(trCSVs, &trCSVFile{watch: trconfig.NewFileWatch(sys.CSVPath, sys.CSVStamp),
					systemID: systemID, shortName: sys.ShortName})
			}
			if sys.UnitCSVPath != "" {
				trCSVs = append(trCSVs, &trCSVFile{watch: trconfig.NewFileWatch(sys.UnitCSVPath, sys.UnitCSVStamp),
					systemID: systemID, shortName: sys.ShortName, units: true})
			}
		}
	}
	if len(trCSVs) > 0 {
		go watchTRCSVs(ctx, db, trCSVs, trCSVPollInterval, log)
		log.Info().Int("files", len(trCSVs)).Dur("interval", trCSVPollInterval).
			Msg("watching trunk-recorder CSV files for changes")
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

	// Unit tag suggestion scanner (opt-in). Walks transcriptions from a
	// persisted cursor — enabling it backfills history — and queues candidate
	// unit alpha tags for review. Never modifies units itself.
	if cfg.UnitTagSuggestions {
		scanner := unittags.NewScanner(db, unittags.Options{
			Interval: cfg.UnitTagSuggestionsInterval,
			Log:      log.With().Str("component", "unittags").Logger(),
		})
		scanner.Start(ctx)
		defer scanner.Stop()
		log.Info().
			Int("min_calls", cfg.UnitTagSuggestionsMinCalls).
			Float64("min_share", cfg.UnitTagSuggestionsMinShare).
			Msg("unit tag suggestions enabled")
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

	// Detect ingest modes and Docker for update checker
	var ingestModes []string
	if cfg.MQTTBrokerURL != "" {
		ingestModes = append(ingestModes, "mqtt")
	}
	if cfg.WatchDir != "" {
		ingestModes = append(ingestModes, "watch")
	}
	ingestModes = append(ingestModes, "upload") // always available

	// HTTP Server
	httpLog := log.With().Str("component", "http").Logger()
	trustedProxies, err := api.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid TRUSTED_PROXIES")
	}

	srv := api.NewServer(api.ServerOptions{
		Config:         cfg,
		TrustedProxies: trustedProxies,
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

// trCSVPollInterval is how often TR_DIR CSV files are checked for changes. A
// change is imported on the second check after it (see trconfig.FileWatch).
const trCSVPollInterval = 30 * time.Second

// trCSVFile is a TR_DIR talkgroup CSV (units=false) or unit tags CSV
// (units=true) imported into systemID.
type trCSVFile struct {
	watch     *trconfig.FileWatch
	systemID  int
	shortName string
	units     bool
}

// watchTRCSVs re-imports TR_DIR CSV files whose contents changed since tr-engine
// started (e.g. talkgroupsFile edited and only trunk-recorder restarted), the
// same way the startup import does, until ctx is done.
func watchTRCSVs(ctx context.Context, db *database.DB, files []*trCSVFile, interval time.Duration, log zerolog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, f := range files {
			stamp, ok := f.watch.Poll()
			if !ok {
				continue
			}
			log.Info().Str("system", f.shortName).Str("path", f.watch.Path).Msg("trunk-recorder CSV changed, re-importing")
			if f.units {
				result, err := trconfig.LoadUnitCSV(f.watch.Path)
				if err != nil {
					// Retrying the same file won't help; wait for the next change.
					log.Warn().Err(err).Str("path", f.watch.Path).Msg("failed to load unit tags CSV")
					f.watch.Imported(stamp)
					continue
				}
				if importTRUnitTags(ctx, db, f.systemID, f.shortName, result.Entries, log) == nil {
					f.watch.Imported(stamp)
				}
			} else {
				result, err := trconfig.LoadTalkgroupCSV(f.watch.Path)
				if err != nil {
					log.Warn().Err(err).Str("path", f.watch.Path).Msg("failed to load talkgroup CSV")
					f.watch.Imported(stamp)
					continue
				}
				if importTRTalkgroups(ctx, db, f.systemID, f.shortName, result.Entries, log) == nil {
					f.watch.Imported(stamp)
				}
			}
		}
	}
}

// importTRTalkgroups imports a TR_DIR talkgroup CSV into the talkgroup directory
// and applies it to heard talkgroups. It logs failures and returns an error if
// any row or the enrichment failed (so a re-import is retried).
func importTRTalkgroups(ctx context.Context, db *database.DB, systemID int, shortName string,
	entries []trconfig.TalkgroupEntry, log zerolog.Logger) error {
	imported := 0
	var firstErr error
	for _, tg := range entries {
		if uErr := db.UpsertTalkgroupDirectory(ctx, systemID, tg.Tgid,
			tg.AlphaTag, tg.Mode, tg.Description, tg.Tag, tg.Category, tg.Priority,
		); uErr != nil {
			log.Warn().Err(uErr).Int("tgid", tg.Tgid).Msg("failed to import talkgroup")
			if firstErr == nil {
				firstErr = uErr
			}
			continue
		}
		imported++
	}
	log.Info().
		Str("system", shortName).
		Int("imported", imported).
		Int("total", len(entries)).
		Msg("talkgroup directory imported")

	// Enrich existing heard talkgroups with directory data
	enriched, enrichErr := db.EnrichTalkgroupsFromDirectory(ctx, systemID, 0)
	if enrichErr != nil {
		log.Warn().Err(enrichErr).Int("system_id", systemID).Msg("failed to enrich talkgroups from directory")
		return enrichErr
	} else if enriched > 0 {
		log.Info().Int64("enriched", enriched).Str("system", shortName).Msg("heard talkgroups enriched from directory")
	}
	return firstErr
}

// importTRUnitTags imports a TR_DIR unit tags CSV. It logs the result and
// returns the error, if any.
func importTRUnitTags(ctx context.Context, db *database.DB, systemID int, shortName string,
	entries []trconfig.UnitEntry, log zerolog.Logger) error {
	changed, err := db.ImportUnitTags(ctx, systemID, unitTagsFromTR(entries))
	if err != nil {
		log.Warn().Err(err).Str("system", shortName).Int("total", len(entries)).
			Msg("failed to import unit tags")
		return err
	}
	log.Info().
		Str("system", shortName).
		Int("total", len(entries)).
		Int64("changed", changed).
		Msg("unit tags imported")
	return nil
}

// unitTagsFromTR converts unit tags CSV entries discovered via TR_DIR.
func unitTagsFromTR(entries []trconfig.UnitEntry) []database.UnitTag {
	tags := make([]database.UnitTag, len(entries))
	for i, e := range entries {
		tags[i] = database.UnitTag{UnitID: e.UnitID, AlphaTag: e.AlphaTag}
	}
	return tags
}

// keepPreUpgradeTagEdits runs database.KeepPreUpgradeTagEdits with the talkgroup
// and unit tags CSVs discovered via TR_DIR, keyed by the system each one is
// imported into (systems not created yet have nothing to keep), and logs the
// talkgroups and units it marked manual. Exits on failure.
//
// A configured CSV that failed to load confirms nothing, so all csv rows it
// covers are kept as manual. That is the safe choice (the old behavior) but
// permanent, as the fixup runs once, so it is logged per file: the user can fix
// the file and hand the rows back (docs/getting-started.md).
func keepPreUpgradeTagEdits(ctx context.Context, db *database.DB, discovered *trconfig.DiscoveryResult,
	instanceID string, log zerolog.Logger) {
	type unloadedCSV struct {
		system   string
		systemID int
		units    bool
		err      error
	}
	var unloaded []unloadedCSV
	files := make(map[int]database.CSVTags)
	if discovered != nil {
		for _, sys := range discovered.Systems {
			if len(sys.Talkgroups) == 0 && len(sys.Units) == 0 && sys.CSVErr == nil && sys.UnitCSVErr == nil {
				continue
			}
			// Same lookup the TR_DIR import's identity resolution does first.
			systemID, err := db.FindSystemViaSiteIdentity(ctx, instanceID, sys.ShortName)
			if err != nil {
				log.Fatal().Err(err).Str("system", sys.ShortName).
					Msg("failed to look up system for pre-upgrade tag edit check")
			}
			if systemID == 0 {
				continue
			}
			if sys.CSVErr != nil {
				unloaded = append(unloaded, unloadedCSV{sys.ShortName, systemID, false, sys.CSVErr})
			}
			if sys.UnitCSVErr != nil {
				unloaded = append(unloaded, unloadedCSV{sys.ShortName, systemID, true, sys.UnitCSVErr})
			}
			f := files[systemID]
			for _, tg := range sys.Talkgroups {
				f.Talkgroups = append(f.Talkgroups, database.TalkgroupTag{Tgid: tg.Tgid, AlphaTag: tg.AlphaTag})
			}
			f.Units = append(f.Units, unitTagsFromTR(sys.Units)...)
			files[systemID] = f
		}
	}

	pinned, ran, err := db.KeepPreUpgradeTagEdits(ctx, files)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to keep tag edits made before CSV tag priority (retried on next start)")
	}
	if ran && (len(pinned.Talkgroups) > 0 || len(pinned.Units) > 0) {
		// The full lists are in data_fixups.detail; the log shows the first few.
		const maxLogged = 50
		log.Warn().
			Int("talkgroups", len(pinned.Talkgroups)).Strs("talkgroup_ids", firstN(pinned.Talkgroups, maxLogged)).
			Int("units", len(pinned.Units)).Strs("unit_ids", firstN(pinned.Units, maxLogged)).
			Str("fixup", database.PreUpgradeTagEditsFixup).
			Msg("kept CSV-sourced tags that older versions may have let you edit (not confirmed by a TR_DIR CSV) as manual edits; " +
				"all IDs are listed in data_fixups.detail. PATCH alpha_tag_source=csv on a talkgroup or unit to use its CSV tag " +
				"again (docs/getting-started.md has SQL to do that for all of them)")
	} else if ran {
		log.Info().Msg("pre-upgrade tag edit check done: no unconfirmed CSV-sourced tags found")
	}
	if !ran {
		return
	}
	for _, u := range unloaded {
		field, file, tags, ids := "talkgroups", "talkgroup CSV", "talkgroup", pinned.Talkgroups
		if u.units {
			field, file, tags, ids = "units", "unit tags CSV", "unit", pinned.Units
		}
		prefix := strconv.Itoa(u.systemID) + ":"
		n := 0
		for _, id := range ids {
			if strings.HasPrefix(id, prefix) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		log.Warn().Err(u.err).Str("system", u.system).Int("system_id", u.systemID).Int(field, n).
			Msg("trunk-recorder " + file + " failed to load, so none of this system's CSV-sourced " + tags +
				" tags could be confirmed and all were kept as manual edits (this check runs only once). " +
				"Fix the CSV, hand them back with the SQL in docs/getting-started.md, then restart")
	}
}

// firstN returns at most the first n elements of ids.
func firstN(ids []string, n int) []string {
	if len(ids) > n {
		return ids[:n]
	}
	return ids
}
