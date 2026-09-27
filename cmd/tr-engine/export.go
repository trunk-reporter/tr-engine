package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	trengine "github.com/snarg/tr-engine"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/export"
)

func parseTime(s string) (time.Time, error) {
	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Try date-only
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %q", s)
}

func runExport(args []string, overrides config.Overrides) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	output := fs.String("output", "", "Output file path (required)")
	systems := fs.String("systems", "", "Comma-separated system IDs to export (default: all)")
	mode := fs.String("mode", "metadata", "Export mode: metadata, full")
	includeAudio := fs.Bool("include-audio", false, "Include audio files in archive (only with --mode full)")
	startStr := fs.String("start", "", "Start time for calls (ISO 8601, e.g. 2026-02-01 or 2026-02-01T00:00:00Z)")
	endStr := fs.String("end", "", "End time for calls (ISO 8601, e.g. 2026-03-01)")
	fs.StringVar(&overrides.EnvFile, "env-file", overrides.EnvFile, "Path to .env file")
	fs.StringVar(&overrides.DatabaseURL, "database-url", overrides.DatabaseURL, "PostgreSQL connection URL")
	fs.StringVar(&overrides.AudioDir, "audio-dir", overrides.AudioDir, "Audio file directory")
	migrate := fs.Bool("migrate", false, "Also upgrade a database from a tr-engine version before API keys (irreversible; normally the first start of the server does that)")
	fs.Parse(args)

	if *output == "" {
		fmt.Fprintln(os.Stderr, "error: --output is required")
		fs.Usage()
		os.Exit(1)
	}

	if *mode != "metadata" && *mode != "full" {
		fmt.Fprintf(os.Stderr, "error: invalid mode %q (valid: metadata, full)\n", *mode)
		os.Exit(1)
	}

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	cfg, err := config.Load(overrides)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}

	timeout := 10 * time.Minute
	if *mode == "full" {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	db, err := database.Connect(ctx, cfg.DatabaseURL, log)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer db.Close()

	// Try to apply migrations (non-fatal for export — read-only operation).
	// Without --migrate, the irreversible upgrade of a database from before
	// API keys is left to the server: export doesn't need it, and an older
	// engine still running on the database wouldn't survive it.
	if err := db.InitSchema(ctx, trengine.SchemaSQL); err != nil {
		log.Warn().Err(err).Msg("schema initialization failed (continuing anyway)")
	}
	if err := migrateForCLI(ctx, db, *migrate, skipAuthConversion, os.Stderr); err != nil {
		log.Warn().Err(err).Msg("schema migration failed (some columns may be missing)")
	}

	// Parse system IDs
	var systemIDs []int
	if *systems != "" {
		for _, s := range strings.Split(*systems, ",") {
			id, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				log.Fatal().Str("value", s).Msg("invalid system ID")
			}
			systemIDs = append(systemIDs, id)
		}
	}

	var start, end *time.Time
	if *startStr != "" {
		t, err := parseTime(*startStr)
		if err != nil {
			log.Fatal().Str("value", *startStr).Msg("invalid --start time (use YYYY-MM-DD or RFC3339)")
		}
		start = &t
	}
	if *endStr != "" {
		t, err := parseTime(*endStr)
		if err != nil {
			log.Fatal().Str("value", *endStr).Msg("invalid --end time (use YYYY-MM-DD or RFC3339)")
		}
		end = &t
	}

	f, err := os.Create(*output)
	if err != nil {
		log.Fatal().Err(err).Str("path", *output).Msg("failed to create output file")
	}
	defer f.Close()

	opts := export.ExportOptions{
		SystemIDs:    systemIDs,
		Version:      fmt.Sprintf("%s (commit=%s)", version, commit),
		Mode:         *mode,
		IncludeAudio: *includeAudio,
		Start:        start,
		End:          end,
		AudioDir:     cfg.AudioDir,
	}

	log.Info().Str("output", *output).Str("mode", *mode).Ints("systems", systemIDs).Msg("starting export")

	if err := export.Export(ctx, db, f, opts); err != nil {
		os.Remove(*output) // clean up partial file
		log.Fatal().Err(err).Msg("export failed")
	}

	info, _ := f.Stat()
	log.Info().
		Str("output", *output).
		Int64("size_bytes", info.Size()).
		Msg("export complete")
}
