package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
)

type StorageHandler struct {
	db  *database.DB
	log zerolog.Logger
}

func NewStorageHandler(db *database.DB, log zerolog.Logger) *StorageHandler {
	return &StorageHandler{db: db, log: log}
}

func (h *StorageHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.db.GetStorageStats(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get storage stats: "+err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, stats)
}

type purgeRequest struct {
	OlderThan string `json:"older_than"`
}

type purgeTableSpec struct {
	table      string
	timeColumn string
	usePartitionDrop bool
}

var purgeAllowlist = map[string]purgeTableSpec{
	"mqtt_raw_messages":      {"mqtt_raw_messages", "", true},
	"console_messages":       {"console_messages", "log_time", false},
	"trunking_messages":      {"trunking_messages", "time", false},
	"plugin_statuses":        {"plugin_statuses", "time", false},
	"call_active_checkpoints": {"call_active_checkpoints", "snapshot_time", false},
}

func (h *StorageHandler) PurgeTable(w http.ResponseWriter, r *http.Request) {
	table := chi.URLParam(r, "table")
	if table == "" {
		WriteError(w, http.StatusBadRequest, "table parameter required")
		return
	}

	spec, ok := purgeAllowlist[table]
	if !ok {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "unknown table")
		return
	}

	var req purgeRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}
	if req.OlderThan == "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "older_than is required (e.g. '48h' or '7d')")
		return
	}
	olderThan, err := parsePurgeAge(req.OlderThan)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "older_than: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	response := map[string]any{
		"table": spec.table,
		"warning": "This action is irreversible.",
	}

	response["older_than"] = req.OlderThan

	if spec.usePartitionDrop {
		dropped, err := h.db.DropOldWeeklyPartitions(ctx, spec.table, olderThan)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "purge failed: "+err.Error())
			return
		}
		timedOut := ctx.Err() != nil
		response["partitions_dropped"] = len(dropped)
		response["timed_out"] = timedOut
		response["duration_ms"] = time.Since(start).Milliseconds()
		if len(dropped) > 0 {
			response["partitions_dropped_names"] = dropped
		}

		h.log.Info().
			Str("table", spec.table).
			Str("older_than", req.OlderThan).
			Int("partitions_dropped", len(dropped)).
			Bool("timed_out", timedOut).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Msg("manual purge completed")
	} else {
		n, err := h.db.PurgeOlderThan(ctx, spec.table, spec.timeColumn, olderThan)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "purge failed: "+err.Error())
			return
		}
		timedOut := ctx.Err() != nil
		response["rows_deleted"] = n
		response["timed_out"] = timedOut
		response["duration_ms"] = time.Since(start).Milliseconds()

		h.log.Info().
			Str("table", spec.table).
			Str("older_than", req.OlderThan).
			Int64("rows_deleted", n).
			Bool("timed_out", timedOut).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Msg("manual purge completed")
	}

	status := http.StatusOK
	if ctx.Err() != nil {
		status = http.StatusGatewayTimeout
	}
	WriteJSON(w, status, response)
}

// minPurgeAge is the youngest data a manual purge may delete. A zero or
// negative age would delete every row, or drop the current raw-message
// partition.
const minPurgeAge = time.Hour

// parsePurgeAge parses a purge age: a Go duration ("48h") or a whole number
// of days ("7d"). It must be at least minPurgeAge.
func parsePurgeAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n < 0 || n > int64(math.MaxInt64/int64(24*time.Hour)) {
			return 0, fmt.Errorf("invalid age %q: use a duration like '48h' or days like '7d'", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid age %q: use a duration like '48h' or days like '7d'", s)
		}
	}
	if d < minPurgeAge {
		return 0, fmt.Errorf("must be at least %s", minPurgeAge)
	}
	return d, nil
}

func (h *StorageHandler) Routes(r chi.Router) {
	r.Get("/admin/storage/stats", h.GetStats)
	r.Post("/admin/storage/purge/{table}", h.PurgeTable)
}
