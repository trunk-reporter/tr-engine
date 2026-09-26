package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/mqttclient"
)

// HealthResponse is the /health body. Everyone gets status, version and
// checks; the rest only goes to a key with unrestricted listen or better
// (§6.3).
type HealthResponse struct {
	Status         string                `json:"status"`
	Version        string                `json:"version"`
	UptimeSeconds  *int64                `json:"uptime_seconds,omitempty"`
	Checks         map[string]string     `json:"checks"`
	Database       *DatabasePoolStats    `json:"database_pool,omitempty"`
	TrunkRecorders []TRInstanceStatusData `json:"trunk_recorders,omitempty"`
	AudioStream    *AudioStreamStatusData `json:"audio_stream,omitempty"`
	UpdateAvailable *bool                `json:"update_available,omitempty"`
	LatestVersion   string               `json:"latest_version,omitempty"`
	ReleaseURL      string               `json:"release_url,omitempty"`
}

type DatabasePoolStats struct {
	MaxConns          int32 `json:"max_conns"`
	TotalConns        int32 `json:"total_conns"`
	AcquiredConns     int32 `json:"acquired_conns"`
	IdleConns         int32 `json:"idle_conns"`
	ConstructingConns int32 `json:"constructing_conns"`
	AcquireCount      int64 `json:"acquire_count"`
	EmptyAcquireCount int64 `json:"empty_acquire_count"`
}

type updateStatus struct {
	Available     bool
	LatestVersion string
	ReleaseURL    string
}

type HealthHandler struct {
	db            *database.DB
	mqtt          *mqttclient.Client
	live          LiveDataSource
	audioStreamer AudioStreamer // nil if live audio streaming not configured
	version       string
	startTime     time.Time

	// Update checker state
	updateCheckURL string
	ingestModes    string
	isDocker       bool
	log            zerolog.Logger
	mu             sync.RWMutex
	update         *updateStatus
}

func NewHealthHandler(db *database.DB, mqtt *mqttclient.Client, live LiveDataSource, audioStreamer AudioStreamer, version string, startTime time.Time) *HealthHandler {
	return &HealthHandler{
		db:           db,
		mqtt:         mqtt,
		live:         live,
		audioStreamer: audioStreamer,
		version:      version,
		startTime:    startTime,
	}
}

// ConfigureUpdateChecker sets up the update checker parameters. Call before StartUpdateChecker.
func (h *HealthHandler) ConfigureUpdateChecker(url, ingestModes string, isDocker bool, log zerolog.Logger) {
	h.updateCheckURL = url
	h.ingestModes = ingestModes
	h.isDocker = isDocker
	h.log = log
}

// StartUpdateChecker begins periodic update checks in the background.
// Does nothing if no update check URL is configured.
func (h *HealthHandler) StartUpdateChecker(ctx context.Context) {
	if h.updateCheckURL == "" {
		return
	}

	// Extract just the version prefix (e.g. "v0.8.7.6" from "v0.8.7.6 (commit=..., built=...)")
	ver := h.version
	if idx := strings.Index(ver, " "); idx > 0 {
		ver = ver[:idx]
	}

	checkURL := fmt.Sprintf("%s?product=tr-engine&v=%s&os=%s&arch=%s&go=%s&ingest=%s&docker=%t",
		h.updateCheckURL, ver, runtime.GOOS, runtime.GOARCH, runtime.Version(),
		h.ingestModes, h.isDocker)

	h.log.Debug().Str("url", checkURL).Msg("update checker configured")

	// Check immediately, then every hour
	go func() {
		h.checkForUpdate(checkURL, true)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.checkForUpdate(checkURL, false)
			}
		}
	}()
}

func (h *HealthHandler) checkForUpdate(url string, firstCheck bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		h.log.Debug().Err(err).Msg("update check failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.log.Debug().Int("status", resp.StatusCode).Msg("update check returned non-200")
		return
	}

	var result struct {
		Latest string `json:"latest"`
		URL    string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		h.log.Debug().Err(err).Msg("update check response parse failed")
		return
	}

	// Compare versions locally — the server response is for stats, not authority
	currentVer := h.version
	if idx := strings.Index(currentVer, " "); idx > 0 {
		currentVer = currentVer[:idx]
	}
	available := compareVersions(result.Latest, currentVer) > 0

	h.mu.Lock()
	h.update = &updateStatus{
		Available:     available,
		LatestVersion: result.Latest,
		ReleaseURL:    result.URL,
	}
	h.mu.Unlock()

	if firstCheck && available {
		h.log.Warn().
			Str("current", currentVer).
			Str("latest", result.Latest).
			Str("release_url", result.URL).
			Msg("a newer version of tr-engine is available")
	}
}

// compareVersions compares two version strings like "v0.8.8.1" or "0.8.8".
// Returns >0 if a > b, <0 if a < b, 0 if equal.
// Handles variable-length segments (0.8.8 < 0.8.8.1).
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aNum, bNum int
		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}
		if aNum != bNum {
			return aNum - bNum
		}
	}
	return 0
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]string)
	status := "healthy"
	httpStatus := http.StatusOK

	// Database check
	if h.db == nil {
		checks["database"] = "not_configured"
	} else if err := h.db.HealthCheck(r.Context()); err != nil {
		checks["database"] = "error"
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	} else {
		checks["database"] = "ok"
	}

	// MQTT check
	if h.mqtt != nil {
		if h.mqtt.IsConnected() {
			checks["mqtt"] = "ok"
		} else {
			checks["mqtt"] = "disconnected"
			if status == "healthy" {
				status = "degraded"
			}
		}
	} else {
		checks["mqtt"] = "not_configured"
	}

	// File watcher check
	if h.live != nil {
		if ws := h.live.WatcherStatus(); ws != nil {
			checks["file_watcher"] = ws.Status
		}
	}

	// Transcription check
	if h.live != nil {
		if ts := h.live.TranscriptionStatus(); ts != nil {
			checks["transcription"] = ts.Status
		} else {
			checks["transcription"] = "not_configured"
		}
	}

	// Only a key with unrestricted listen (or better) sees more than the
	// checks: TR instances, pool stats, the stream address and updates.
	if p := PrincipalFrom(r); p == nil || p.Kind != auth.KindKey || !p.Has(auth.ScopeListen) || p.Restricted() {
		writeHealth(w, httpStatus, HealthResponse{Status: status, Version: h.version, Checks: checks})
		return
	}

	// TR instance status
	var trInstances []TRInstanceStatusData
	if h.live != nil {
		trInstances = h.live.TRInstanceStatus()
	}

	// Database pool stats
	var poolStats *DatabasePoolStats
	if h.db != nil {
		stat := h.db.Pool.Stat()
		poolStats = &DatabasePoolStats{
			MaxConns:          stat.MaxConns(),
			TotalConns:        stat.TotalConns(),
			AcquiredConns:     stat.AcquiredConns(),
			IdleConns:         stat.IdleConns(),
			ConstructingConns: stat.ConstructingConns(),
			AcquireCount:      stat.AcquireCount(),
			EmptyAcquireCount: stat.EmptyAcquireCount(),
		}
	}

	// Audio stream status
	var audioStreamStatus *AudioStreamStatusData
	if h.audioStreamer != nil && h.audioStreamer.AudioStreamEnabled() {
		audioStreamStatus = h.audioStreamer.AudioStreamStatus()
	}

	uptime := int64(time.Since(h.startTime).Seconds())
	resp := HealthResponse{
		Status:         status,
		Version:        h.version,
		UptimeSeconds:  &uptime,
		Checks:         checks,
		Database:       poolStats,
		TrunkRecorders: trInstances,
		AudioStream:    audioStreamStatus,
	}

	// Add update status if available
	h.mu.RLock()
	if h.update != nil {
		resp.UpdateAvailable = &h.update.Available
		resp.LatestVersion = h.update.LatestVersion
		resp.ReleaseURL = h.update.ReleaseURL
	}
	h.mu.RUnlock()

	writeHealth(w, httpStatus, resp)
}

func writeHealth(w http.ResponseWriter, status int, resp HealthResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}
