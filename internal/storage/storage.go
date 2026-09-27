package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
)

// AudioStore abstracts audio file storage backends.
type AudioStore interface {
	// Save stores audio data, replacing any file stored under key. Key
	// formats: {sys_name}/{YYYY-MM-DD}/{filename} (MQTT audio) and
	// upload/{system_id}/{YYYY-MM-DD}/{call_id}.{ext} (HTTP uploads).
	Save(ctx context.Context, key string, data []byte, contentType string) error

	// LocalPath returns the local filesystem path if the file exists on disk.
	// Returns "" if not available locally.
	LocalPath(key string) string

	// URL returns a presigned URL for the audio file.
	// Returns "" for local-only backends.
	URL(ctx context.Context, key string) (string, error)

	// Open returns a reader for the audio file.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists checks if an audio file exists in any backend.
	Exists(ctx context.Context, key string) bool

	// Type returns "local", "s3", or "tiered".
	Type() string
}

// New creates an AudioStore based on config. Returns the store and optional
// background services (pruner, reconciler) that the caller must Start/Stop.
// Returns an error if S3 is configured but unreachable.
func New(cfg config.S3Config, audioDir string, log zerolog.Logger) (AudioStore, []BackgroundService, error) {
	if !cfg.Enabled() {
		return NewLocalStore(audioDir), nil, nil
	}

	s3store, err := NewS3Store(cfg, log)
	if err != nil {
		return nil, nil, fmt.Errorf("S3 init failed: %w", err)
	}

	// Startup validation: verify credentials and bucket access
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s3store.HeadBucket(ctx); err != nil {
		return nil, nil, fmt.Errorf("S3 startup check failed (bucket=%q endpoint=%q): %w",
			cfg.Bucket, cfg.Endpoint, err)
	}
	log.Info().Str("bucket", cfg.Bucket).Str("endpoint", cfg.Endpoint).Msg("S3 connection verified")

	if !cfg.LocalCache {
		return s3store, nil, nil
	}

	// Tiered mode: local primary + S3 backup
	local := NewLocalStore(audioDir)
	tiered := NewTieredStore(s3store, local, log)

	var services []BackgroundService

	// Cache pruner
	if cfg.CacheRetention > 0 || cfg.CacheMaxGB > 0 {
		pruner := NewCachePruner(audioDir, cfg.CacheRetention, cfg.CacheMaxGB, s3store, log)
		services = append(services, pruner)
	}

	// Upload reconciler
	reconciler := NewUploadReconciler(audioDir, s3store, log)
	services = append(services, reconciler)

	return tiered, services, nil
}

// BackgroundService is a stoppable background goroutine.
type BackgroundService interface {
	Start()
	Stop()
}
