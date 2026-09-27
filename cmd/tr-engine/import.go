package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	trengine "github.com/snarg/tr-engine"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/export"
)

func runImport(args []string, overrides config.Overrides) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	file := fs.String("file", "", "Archive file path (required)")
	mode := fs.String("mode", "metadata", "Import mode: full, metadata, calls")
	dryRun := fs.Bool("dry-run", false, "Show what would be imported without making changes")
	audioDir := fs.String("audio-dir", "", "Directory to extract audio files to (default: from config AUDIO_DIR)")
	fs.StringVar(&overrides.EnvFile, "env-file", overrides.EnvFile, "Path to .env file")
	fs.StringVar(&overrides.DatabaseURL, "database-url", overrides.DatabaseURL, "PostgreSQL connection URL")
	migrate := fs.Bool("migrate", false, "Also upgrade a database from a tr-engine version before API keys (irreversible; normally the first start of the server does that)")
	fs.Parse(args)

	if *file == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required")
		fs.Usage()
		os.Exit(1)
	}

	if *mode != "metadata" && *mode != "full" && *mode != "calls" {
		fmt.Fprintf(os.Stderr, "error: invalid mode %q (valid: full, metadata, calls)\n", *mode)
		os.Exit(1)
	}

	if *dryRun && *migrate {
		fmt.Fprintln(os.Stderr, "error: --migrate can't be combined with --dry-run: a dry run never upgrades a database from before API keys")
		os.Exit(1)
	}

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	cfg, err := config.Load(overrides)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	db, err := database.Connect(ctx, cfg.DatabaseURL, log)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer db.Close()

	// Try to apply migrations (non-fatal — import upserts handle missing columns gracefully).
	// Without --migrate (never with --dry-run), the irreversible upgrade of a
	// database from before API keys is left to the server: import doesn't
	// need it, and an older engine still running on the database wouldn't
	// survive it.
	if err := db.InitSchema(ctx, trengine.SchemaSQL); err != nil {
		log.Warn().Err(err).Msg("schema initialization failed (continuing anyway)")
	}
	if err := migrateForCLI(ctx, db, *migrate, skipAuthConversion, os.Stderr); err != nil {
		log.Warn().Err(err).Msg("schema migration failed (some columns may be missing)")
	}

	f, err := os.Open(*file)
	if err != nil {
		log.Fatal().Err(err).Str("path", *file).Msg("failed to open archive")
	}
	defer f.Close()

	// Determine audio directory
	importAudioDir := *audioDir
	if importAudioDir == "" {
		importAudioDir = cfg.AudioDir
	}

	opts := export.ImportOptions{
		Mode:     *mode,
		DryRun:   *dryRun,
		AudioDir: importAudioDir,
	}

	action := "importing"
	if *dryRun {
		action = "dry-run importing"
	}
	log.Info().Str("file", *file).Str("mode", *mode).Bool("dry_run", *dryRun).Msgf("%s metadata", action)

	result, err := export.ImportMetadata(ctx, db, f, opts, log)
	if err != nil {
		log.Fatal().Err(err).Msg("import failed")
	}

	summary, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(summary))

	if *dryRun {
		log.Info().Msg("dry run complete — no changes made")
	} else {
		log.Info().Msg("import complete")
	}
}
