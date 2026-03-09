package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/database"
)

type StatsHandler struct {
	db *database.DB
}

func NewStatsHandler(db *database.DB) *StatsHandler {
	return &StatsHandler{db: db}
}

// GetStats returns overall system statistics.
func (h *StatsHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.db.GetStats(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get stats")
		return
	}
	WriteJSON(w, http.StatusOK, stats)
}

// GetDecodeRates returns decode rate measurements over time.
func (h *StatsHandler) GetDecodeRates(w http.ResponseWriter, r *http.Request) {
	filter := database.DecodeRateFilter{}
	filter.SystemIDs = QueryIntList(r, "system_id")
	if t, ok := QueryTime(r, "start_time"); ok {
		filter.StartTime = &t
	}
	if t, ok := QueryTime(r, "end_time"); ok {
		filter.EndTime = &t
	}
	if msg := ValidateTimeRange(filter.StartTime, filter.EndTime); msg != "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidTimeRange, msg)
		return
	}
	if v, ok := QueryInt(r, "limit"); ok {
		if v < 1 || v > 10000 {
			WriteError(w, http.StatusBadRequest, "limit must be between 1 and 10000")
			return
		}
		filter.Limit = v
	}

	rates, err := h.db.GetDecodeRates(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get decode rates")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"rates": rates,
		"total": len(rates),
	})
}

// ListTrunkingMessages returns paginated trunking messages.
func (h *StatsHandler) ListTrunkingMessages(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	filter := database.TrunkingMessageFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
	}
	filter.SystemIDs = QueryIntList(r, "system_id")
	if v, ok := QueryString(r, "opcode"); ok {
		filter.Opcode = &v
	}
	if v, ok := QueryString(r, "opcode_type"); ok {
		filter.OpcodeType = &v
	}
	if t, ok := QueryTime(r, "start_time"); ok {
		filter.StartTime = &t
	}
	if t, ok := QueryTime(r, "end_time"); ok {
		filter.EndTime = &t
	}
	if msg := ValidateTimeRange(filter.StartTime, filter.EndTime); msg != "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidTimeRange, msg)
		return
	}

	messages, total, err := h.db.ListTrunkingMessages(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list trunking messages")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"total":    total,
	})
}

// ListConsoleMessages returns paginated console log messages.
func (h *StatsHandler) ListConsoleMessages(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	filter := database.ConsoleMessageFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
	}
	if v, ok := QueryString(r, "instance_id"); ok {
		filter.InstanceID = &v
	}
	if v, ok := QueryString(r, "severity"); ok {
		filter.Severity = &v
	}
	if t, ok := QueryTime(r, "start_time"); ok {
		filter.StartTime = &t
	}
	if t, ok := QueryTime(r, "end_time"); ok {
		filter.EndTime = &t
	}
	if msg := ValidateTimeRange(filter.StartTime, filter.EndTime); msg != "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidTimeRange, msg)
		return
	}

	messages, total, err := h.db.ListConsoleMessages(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list console messages")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"total":    total,
	})
}

// GetTalkgroupActivity returns call counts grouped by talkgroup for a time range.
func (h *StatsHandler) GetTalkgroupActivity(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	filter := database.TalkgroupActivityFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
	}
	filter.SystemIDs = QueryIntListAliased(r, "system_id", "systems")
	filter.SiteIDs = QueryIntListAliased(r, "site_id", "sites")
	filter.Tgids = QueryIntListAliased(r, "tgid", "tgids")

	if t, ok := QueryTime(r, "after"); ok {
		filter.After = &t
	}
	if t, ok := QueryTime(r, "before"); ok {
		filter.Before = &t
	}
	if v, ok := QueryString(r, "sort"); ok {
		filter.SortField = v
	}
	if v, ok := QueryString(r, "call_state"); ok {
		filter.CallState = &v
	}

	activity, total, err := h.db.GetTalkgroupActivity(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get talkgroup activity")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"activity": activity,
		"total":    total,
	})
}

// GetCallVolume returns hourly or daily call counts over a time range.
func (h *StatsHandler) GetCallVolume(w http.ResponseWriter, r *http.Request) {
	filter := database.CallVolumeFilter{}
	if v, ok := QueryString(r, "interval"); ok {
		if v != "hour" && v != "day" {
			WriteError(w, http.StatusBadRequest, "interval must be 'hour' or 'day'")
			return
		}
		filter.Interval = v
	}
	if v, ok := QueryInt(r, "days"); ok {
		if v < 1 || v > 90 {
			WriteError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return
		}
		filter.Days = v
	}
	filter.SystemIDs = QueryIntList(r, "system_id")

	buckets, err := h.db.GetCallVolume(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get call volume")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
}

// GetDailyOverview returns daily call aggregates with active talkgroup counts.
func (h *StatsHandler) GetDailyOverview(w http.ResponseWriter, r *http.Request) {
	filter := database.DailyOverviewFilter{}
	if v, ok := QueryInt(r, "days"); ok {
		if v < 1 || v > 90 {
			WriteError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return
		}
		filter.Days = v
	}
	filter.SystemIDs = QueryIntList(r, "system_id")

	days, err := h.db.GetDailyOverview(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get daily overview")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"days": days})
}

// GetCategoryBreakdown returns calls grouped by talkgroup tag.
func (h *StatsHandler) GetCategoryBreakdown(w http.ResponseWriter, r *http.Request) {
	filter := database.CategoryBreakdownFilter{}
	if v, ok := QueryInt(r, "hours"); ok {
		if v < 1 || v > 720 {
			WriteError(w, http.StatusBadRequest, "hours must be between 1 and 720")
			return
		}
		filter.Hours = v
	}
	if v, ok := QueryInt(r, "limit"); ok {
		if v < 1 || v > 100 {
			WriteError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		filter.Limit = v
	}
	filter.SystemIDs = QueryIntList(r, "system_id")

	categories, err := h.db.GetCategoryBreakdown(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get category breakdown")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"categories": categories})
}

// GetCallHeatmap returns day-of-week × hour-of-day call counts.
func (h *StatsHandler) GetCallHeatmap(w http.ResponseWriter, r *http.Request) {
	filter := database.CallHeatmapFilter{}
	if v, ok := QueryInt(r, "days"); ok {
		if v < 1 || v > 90 {
			WriteError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return
		}
		filter.Days = v
	}
	if v, ok := QueryString(r, "tz"); ok {
		filter.Timezone = v
	}
	filter.SystemIDs = QueryIntList(r, "system_id")

	cells, err := h.db.GetCallHeatmap(r.Context(), filter)
	if err != nil {
		// Invalid timezone returns a PG error — surface as 400
		if isInvalidTimezone(err) {
			WriteError(w, http.StatusBadRequest, "invalid timezone")
			return
		}
		WriteError(w, http.StatusInternalServerError, "failed to get call heatmap")
		return
	}
	tz := filter.Timezone
	if tz == "" {
		tz = "UTC"
	}
	WriteJSON(w, http.StatusOK, map[string]any{"cells": cells, "timezone": tz})
}

// isInvalidTimezone checks if a PG error is due to an invalid timezone name.
func isInvalidTimezone(err error) bool {
	return strings.Contains(err.Error(), "time zone")
}

// GetRecorderUtilization returns recorder state percentages bucketed by hour or day.
func (h *StatsHandler) GetRecorderUtilization(w http.ResponseWriter, r *http.Request) {
	filter := database.RecorderUtilizationFilter{}
	if v, ok := QueryString(r, "bucket"); ok {
		if v != "hour" && v != "day" {
			WriteError(w, http.StatusBadRequest, "bucket must be 'hour' or 'day'")
			return
		}
		filter.Bucket = v
	}
	if v, ok := QueryInt(r, "days"); ok {
		if v < 1 || v > 90 {
			WriteError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return
		}
		filter.Days = v
	}
	if v, ok := QueryInt(r, "src_num"); ok {
		filter.SrcNum = &v
	}
	if v, ok := QueryString(r, "instance_id"); ok {
		filter.InstanceID = &v
	}

	buckets, err := h.db.GetRecorderUtilization(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get recorder utilization")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"buckets": buckets,
		"total":   len(buckets),
	})
}

// GetDecodeRateBuckets returns decode rates aggregated into time buckets.
func (h *StatsHandler) GetDecodeRateBuckets(w http.ResponseWriter, r *http.Request) {
	filter := database.DecodeRateBucketFilter{}
	if v, ok := QueryString(r, "bucket"); ok {
		if v != "hour" && v != "day" {
			WriteError(w, http.StatusBadRequest, "bucket must be 'hour' or 'day'")
			return
		}
		filter.Bucket = v
	}
	if v, ok := QueryInt(r, "days"); ok {
		if v < 1 || v > 90 {
			WriteError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return
		}
		filter.Days = v
	}
	filter.SystemIDs = QueryIntList(r, "system_id")

	buckets, err := h.db.GetDecodeRateBuckets(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get decode rate buckets")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"buckets": buckets,
		"total":   len(buckets),
	})
}

// Routes registers stats routes on the given router.
func (h *StatsHandler) Routes(r chi.Router) {
	r.Get("/stats", h.GetStats)
	r.Get("/stats/rates", h.GetDecodeRates)
	r.Get("/stats/talkgroup-activity", h.GetTalkgroupActivity)
	r.Get("/stats/call-volume", h.GetCallVolume)
	r.Get("/stats/daily-overview", h.GetDailyOverview)
	r.Get("/stats/category-breakdown", h.GetCategoryBreakdown)
	r.Get("/stats/call-heatmap", h.GetCallHeatmap)
	r.Get("/analytics/recorder-utilization", h.GetRecorderUtilization)
	r.Get("/analytics/decode-rates", h.GetDecodeRateBuckets)
	r.Get("/trunking-messages", h.ListTrunkingMessages)
	r.Get("/console-messages", h.ListConsoleMessages)
}
