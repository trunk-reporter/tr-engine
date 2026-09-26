package ingest

import (
	"context"
	"testing"
	"time"
)

// GET /admin/maintenance reports where each retention value comes from:
// "db" after PUT /admin/maintenance/config (SetRetention) and back to
// "default" (or "env") after DELETE (DeleteRetention), without a restart
// (k-01). Skipped unless TEST_DATABASE_URL is set.
func TestRetentionSourceFollowsOverrides(t *testing.T) {
	for _, env := range retentionKeyToEnv {
		t.Setenv(env, "")
	}
	t.Setenv("RETENTION_CONSOLE_LOGS", "48h") // locked by the environment
	p, db, _ := uploadTestPipeline(t)
	ctx := context.Background()

	cfg := p.MaintenanceStatus().Config
	if cfg.RetentionAuditLogSource != "default" || cfg.RetentionConsoleLogsSource != "env" || !cfg.RetentionConsoleLogsLocked {
		t.Fatalf("initial config = %+v", cfg)
	}

	if err := p.SetRetention(ctx, "retention_audit_log", 720*time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg = p.MaintenanceStatus().Config
	if cfg.RetentionAuditLog != "720h0m0s" || cfg.RetentionAuditLogSource != "db" || cfg.RetentionAuditLogLocked {
		t.Errorf("after set: %s %s locked=%v", cfg.RetentionAuditLog, cfg.RetentionAuditLogSource, cfg.RetentionAuditLogLocked)
	}

	if err := p.DeleteRetention(ctx, "retention_audit_log"); err != nil {
		t.Fatal(err)
	}
	cfg = p.MaintenanceStatus().Config
	if cfg.RetentionAuditLog != "8760h0m0s" || cfg.RetentionAuditLogSource != "default" {
		t.Errorf("after delete: %s %s", cfg.RetentionAuditLog, cfg.RetentionAuditLogSource)
	}

	// An environment variable still wins.
	if err := p.SetRetention(ctx, "retention_console_logs", time.Hour); err == nil {
		t.Error("SetRetention changed a setting locked by RETENTION_CONSOLE_LOGS")
	}

	// A stored override is reported as "db" from startup on.
	if err := db.SetConfigOverride(ctx, "retention_stale_calls", "2h"); err != nil {
		t.Fatal(err)
	}
	p.loadRetentionOverrides(ctx)
	cfg = p.MaintenanceStatus().Config
	if cfg.RetentionStaleCalls != "2h0m0s" || cfg.RetentionStaleCallsSource != "db" {
		t.Errorf("loaded override: %s %s", cfg.RetentionStaleCalls, cfg.RetentionStaleCallsSource)
	}
}
