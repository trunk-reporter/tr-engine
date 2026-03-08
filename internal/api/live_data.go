package api

import (
	"context"
	"time"

	"github.com/snarg/tr-engine/internal/audio"
)

// LiveDataSource provides real-time data from the ingest pipeline to the API layer.
// The pipeline implements this interface — no circular imports since api owns the interface.
type LiveDataSource interface {
	// ActiveCalls returns currently in-progress calls from the MQTT tracker.
	ActiveCalls() []ActiveCallData

	// LatestRecorders returns the most recent recorder state snapshot.
	LatestRecorders() []RecorderStateData

	// TRInstanceStatus returns the cached status of all known TR instances.
	TRInstanceStatus() []TRInstanceStatusData

	// UnitAffiliations returns current talkgroup affiliation state for all tracked units.
	UnitAffiliations() []UnitAffiliationData

	// Subscribe returns a channel that receives SSE events matching the filter,
	// and a cancel function to unsubscribe.
	Subscribe(filter EventFilter) (<-chan SSEEvent, func())

	// ReplaySince returns buffered events since the given event ID (for Last-Event-ID recovery).
	ReplaySince(lastEventID string, filter EventFilter) []SSEEvent

	// WatcherStatus returns the file watcher status, or nil if not active.
	WatcherStatus() *WatcherStatusData

	// TranscriptionStatus returns the transcription service status, or nil if not configured.
	TranscriptionStatus() *TranscriptionStatusData

	// EnqueueTranscription adds a call to the transcription queue.
	// Returns false if the queue is full or transcription is disabled.
	EnqueueTranscription(callID int64) bool

	// TranscriptionQueueStats returns queue statistics, or nil if not configured.
	TranscriptionQueueStats() *TranscriptionQueueStatsData

	// IngestMetrics returns pipeline metrics for the Prometheus collector.
	// Returns nil if the pipeline is not running.
	IngestMetrics() *IngestMetricsData

	// MaintenanceStatus returns the current maintenance config and last run results.
	MaintenanceStatus() *MaintenanceStatusData

	// RunMaintenance triggers an immediate maintenance run.
	// Returns the results, or an error if maintenance is already running.
	RunMaintenance(ctx context.Context) (*MaintenanceRunData, error)

	// SubmitBackfill queues a transcription backfill job.
	SubmitBackfill(ctx context.Context, filters BackfillFiltersData) (jobID, position, total int, err error)

	// BackfillStatus returns the active and queued backfill jobs.
	BackfillStatus() *BackfillStatusData

	// CancelBackfill cancels a backfill job by ID. If id <= 0, cancels all.
	CancelBackfill(id int) bool
}

// CallUploader processes an uploaded call (audio + metadata).
// The ingest Pipeline implements this interface via an adapter.
type CallUploader interface {
	// ProcessUpload handles an HTTP-uploaded call. fields contains the parsed
	// form field values, audioData is the raw audio bytes, audioFilename is the
	// original filename from the upload. format is "rdio-scanner" or "openmhz".
	// Returns the result or an error (containing "duplicate call" for 409s).
	ProcessUpload(ctx context.Context, instanceID string, format string, fields map[string]string, audioData []byte, audioFilename string) (*UploadCallResult, error)
}

// UploadCallResult is returned after a successful call upload.
type UploadCallResult struct {
	CallID        int64     `json:"call_id"`
	SystemID      int       `json:"system_id"`
	Tgid          int       `json:"tgid"`
	StartTime     time.Time `json:"start_time"`
	AudioFilePath string    `json:"audio_file_path,omitempty"`
}

// WatcherStatusData represents the status of the file watcher ingest mode.
type WatcherStatusData struct {
	Status         string `json:"status"`           // "watching", "backfilling", "stopped"
	WatchDir       string `json:"watch_dir"`
	FilesProcessed int64  `json:"files_processed"`
	FilesSkipped   int64  `json:"files_skipped"`
}

// ActiveCallData represents an in-progress call from the pipeline.
type ActiveCallData struct {
	CallID        int64     `json:"call_id"`
	SystemID      int       `json:"system_id"`
	SystemName    string    `json:"system_name"`
	Sysid         string    `json:"sysid"`
	SiteID        *int      `json:"site_id,omitempty"`
	SiteShortName string    `json:"site_short_name,omitempty"`
	Tgid          int       `json:"tgid"`
	TgAlphaTag    string    `json:"tg_alpha_tag,omitempty"`
	TgDescription string    `json:"tg_description,omitempty"`
	TgTag         string    `json:"tg_tag,omitempty"`
	TgGroup       string    `json:"tg_group,omitempty"`
	StartTime     time.Time `json:"start_time"`
	Duration      float32   `json:"duration,omitempty"`
	Freq          int64     `json:"freq,omitempty"`
	Emergency     bool      `json:"emergency"`
	Encrypted     bool      `json:"encrypted"`
	Analog        bool      `json:"analog"`
	Conventional  bool      `json:"conventional"`
	Phase2TDMA    bool      `json:"phase2_tdma"`
	AudioType     string    `json:"audio_type,omitempty"`
}

// RecorderStateData represents a recorder's current state.
type RecorderStateData struct {
	ID           string  `json:"id"`
	InstanceID   string  `json:"instance_id"`
	SrcNum       int16   `json:"src_num"`
	RecNum       int16   `json:"rec_num"`
	Type         string  `json:"type"`
	RecState     string  `json:"rec_state"`
	Freq         int64   `json:"freq"`
	Duration     float32 `json:"duration"`
	Count        int     `json:"count"`
	Squelched    bool    `json:"squelched"`
	SystemID     *int    `json:"system_id,omitempty"`
	Tgid         *int    `json:"tgid,omitempty"`
	TgAlphaTag   *string `json:"tg_alpha_tag,omitempty"`
	UnitID       *int    `json:"unit_id,omitempty"`
	UnitAlphaTag *string `json:"unit_alpha_tag,omitempty"`
}

// TRInstanceStatusData represents the cached status of a trunk-recorder instance.
type TRInstanceStatusData struct {
	InstanceID string    `json:"instance_id"`
	Status     string    `json:"status"`
	LastSeen   time.Time `json:"last_seen"`
}

// UnitAffiliationData represents a unit's current talkgroup affiliation.
type UnitAffiliationData struct {
	SystemID        int       `json:"system_id"`
	SystemName      string    `json:"system_name,omitempty"`
	Sysid           string    `json:"sysid,omitempty"`
	UnitID          int       `json:"unit_id"`
	UnitAlphaTag    string    `json:"unit_alpha_tag,omitempty"`
	Tgid            int       `json:"tgid"`
	TgAlphaTag      string    `json:"tg_alpha_tag,omitempty"`
	TgDescription   string    `json:"tg_description,omitempty"`
	TgTag           string    `json:"tg_tag,omitempty"`
	TgGroup         string    `json:"tg_group,omitempty"`
	PreviousTgid    *int      `json:"previous_tgid,omitempty"`
	AffiliatedSince time.Time `json:"affiliated_since"`
	LastEventTime   time.Time `json:"last_event_time"`
	Status          string    `json:"status"` // "affiliated" or "off"
}

// TranscriptionStatusData represents the status of the transcription service.
type TranscriptionStatusData struct {
	Status  string `json:"status"`            // "ok", "unavailable", "not_configured"
	Model   string `json:"model,omitempty"`
	Workers int    `json:"workers,omitempty"`
}

// TranscriptionQueueStatsData reports transcription queue statistics.
type TranscriptionQueueStatsData struct {
	Pending     int                           `json:"pending"`
	Completed   int64                         `json:"completed"`
	Failed      int64                         `json:"failed"`
	Performance *TranscriptionPerformanceData `json:"performance,omitempty"`
}

// TranscriptionPerformanceData reports aggregate STT performance.
type TranscriptionPerformanceData struct {
	SampleSize       int                                    `json:"sample_size"`
	AvgRealTimeRatio *float64                               `json:"avg_real_time_ratio"`
	AvgProviderMs    *float64                               `json:"avg_provider_ms"`
	ByProvider       map[string]TranscriptionProviderMetrics `json:"by_provider,omitempty"`
}

// TranscriptionProviderMetrics reports per-provider metrics.
type TranscriptionProviderMetrics struct {
	Count            int      `json:"count"`
	AvgRealTimeRatio *float64 `json:"avg_real_time_ratio"`
	AvgProviderMs    *float64 `json:"avg_provider_ms"`
}

// IngestMetricsData provides pipeline state for Prometheus metrics.
type IngestMetricsData struct {
	MsgCount       int64
	ActiveCalls    int
	HandlerCounts  map[string]int64
	SSESubscribers int
}

// MaintenanceStatusData reports the current maintenance configuration and last run results.
type MaintenanceStatusData struct {
	Config  MaintenanceConfigData `json:"config"`
	LastRun *MaintenanceRunData   `json:"last_run"`
}

// MaintenanceConfigData reports the active retention settings.
type MaintenanceConfigData struct {
	RetentionRawMessages  string `json:"retention_raw_messages"`
	RetentionConsoleLogs  string `json:"retention_console_logs"`
	RetentionPluginStatus string `json:"retention_plugin_status"`
	RetentionCheckpoints  string `json:"retention_checkpoints"`
	RetentionStaleCalls   string `json:"retention_stale_calls"`
	Schedule              string `json:"schedule"`
}

// MaintenanceRunData reports the results of a single maintenance run.
type MaintenanceRunData struct {
	StartedAt         time.Time                   `json:"started_at"`
	DurationMs        int64                       `json:"duration_ms"`
	Decimation        map[string]DecimationResult `json:"decimation"`
	Purged            map[string]int64            `json:"purged"`
	PartitionsCreated int                         `json:"partitions_created"`
	PartitionsDropped []string                    `json:"partitions_dropped"`
}

// DecimationResult reports rows deleted in each decimation phase.
type DecimationResult struct {
	Phase1Deleted int64 `json:"phase1_deleted"`
	Phase2Deleted int64 `json:"phase2_deleted"`
}

// BackfillFiltersData contains the user-provided filters for a backfill job.
type BackfillFiltersData struct {
	SystemID  *int       `json:"system_id,omitempty"`
	Tgids     []int      `json:"tgids,omitempty"`
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// BackfillStatusData reports the state of the backfill manager.
type BackfillStatusData struct {
	Active *BackfillJobData  `json:"active"`
	Queued []BackfillJobData `json:"queued"`
}

// BackfillJobData reports the state of a single backfill job.
type BackfillJobData struct {
	JobID     int                 `json:"job_id"`
	Filters   BackfillFiltersData `json:"filters"`
	Total     int                 `json:"total"`
	Completed int64               `json:"completed,omitempty"`
	Failed    int64               `json:"failed,omitempty"`
	StartedAt *time.Time          `json:"started_at,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}

// AudioStreamer provides live audio streaming capabilities.
type AudioStreamer interface {
	SubscribeAudio(filter audio.AudioFilter) (<-chan audio.AudioFrame, func())
	UpdateAudioFilter(ch <-chan audio.AudioFrame, filter audio.AudioFilter)
	AudioStreamEnabled() bool
	AudioStreamStatus() *AudioStreamStatusData
}

// AudioStreamStatusData reports the status of the live audio streaming subsystem.
type AudioStreamStatusData struct {
	Enabled          bool   `json:"enabled"`
	Listen           string `json:"listen,omitempty"`
	ActiveEncoders   int    `json:"active_encoders"`
	ConnectedClients int    `json:"connected_clients"`
	LastChunkReceived string `json:"last_chunk_received,omitempty"`
}

// EventFilter specifies which events an SSE subscriber wants to receive.
type EventFilter struct {
	Systems       []int
	Sites         []int
	Tgids         []int
	Units         []int
	Types         []string
	EmergencyOnly bool
}

// SSEEvent represents a server-sent event ready for transmission.
type SSEEvent struct {
	ID        string `json:"event_id"`
	Type      string `json:"event_type"`
	SubType   string `json:"sub_type,omitempty"`
	Timestamp string `json:"timestamp"`
	SystemID  int    `json:"system_id,omitempty"`
	SiteID    int    `json:"site_id,omitempty"`
	Tgid      int    `json:"tgid,omitempty"`
	UnitID    int    `json:"unit_id,omitempty"`
	Emergency bool   `json:"-"` // used for server-side filtering only
	Data      []byte `json:"-"` // pre-serialized JSON payload
}
