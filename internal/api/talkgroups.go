package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/trconfig"
)

type TalkgroupsHandler struct {
	db       *database.DB
	csvPaths map[int]string // system_id → CSV file path for writeback
}

func NewTalkgroupsHandler(db *database.DB, csvPaths map[int]string) *TalkgroupsHandler {
	return &TalkgroupsHandler{db: db, csvPaths: csvPaths}
}

var talkgroupSortFields = map[string]string{
	"alpha_tag":  "t.alpha_tag",
	"tgid":       "t.tgid",
	"group":      `t."group"`,
	"last_seen":  "t.last_seen",
	"call_count": "t.call_count_30d",
	"calls_1h":   "t.calls_1h",
	"calls_24h":  "t.calls_24h",
	"unit_count": "t.unit_count_30d",
}

// resolveTalkgroup parses the {id} path parameter, a composite
// "system_id:tgid" or a plain tgid, into a talkgroup the caller may see. A
// plain tgid resolves among the caller's allowed talkgroups only, so a 409
// lists only systems whose talkgroup the caller may see, and a talkgroup
// outside the caller's restriction is 404 like a missing one (§6.3). On
// failure it writes the response and returns ok=false.
func (h *TalkgroupsHandler) resolveTalkgroup(w http.ResponseWriter, r *http.Request) (CompositeID, bool) {
	cid, err := ParseCompositeID(r, "id")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return cid, false
	}
	p := PrincipalFrom(r)
	if cid.IsPlain {
		matches, err := h.db.FindTalkgroupSystems(r.Context(), p, cid.EntityID)
		if err != nil || len(matches) == 0 {
			WriteError(w, http.StatusNotFound, "talkgroup not found")
			return cid, false
		}
		if len(matches) > 1 {
			WriteAmbiguous(w, cid.EntityID, matches)
			return cid, false
		}
		cid.SystemID = matches[0].SystemID
	}
	if !p.AllowsTG(cid.SystemID, cid.EntityID) {
		WriteError(w, http.StatusNotFound, "talkgroup not found")
		return cid, false
	}
	return cid, true
}

// ListTalkgroups returns talkgroups with embedded stats.
func (h *TalkgroupsHandler) ListTalkgroups(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	sort := ParseSort(r, "alpha_tag", talkgroupSortFields)

	filter := database.TalkgroupFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
		Sort:   sort.SQLOrderBy(talkgroupSortFields),
	}

	filter.SystemIDs = QueryIntList(r, "system_id")
	filter.Sysids = QueryStringList(r, "sysid")
	if v, ok := QueryString(r, "group"); ok {
		filter.Group = &v
	}
	if v, ok := QueryString(r, "search"); ok {
		filter.Search = &v
	}
	if _, ok := QueryInt(r, "stats_days"); ok {
		WriteError(w, http.StatusBadRequest, "stats_days is no longer supported on the list endpoint; use GET /talkgroups/{id} for real-time stats")
		return
	}

	talkgroups, total, err := h.db.ListTalkgroups(r.Context(), PrincipalFrom(r), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list talkgroups")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"talkgroups": talkgroups,
		"total":      total,
		"limit":      p.Limit,
		"offset":     p.Offset,
	})
}

// GetTalkgroup returns a single talkgroup by composite or plain ID.
func (h *TalkgroupsHandler) GetTalkgroup(w http.ResponseWriter, r *http.Request) {
	cid, ok := h.resolveTalkgroup(w, r)
	if !ok {
		return
	}

	tg, err := h.db.GetTalkgroupByComposite(r.Context(), PrincipalFrom(r), cid.SystemID, cid.EntityID)
	if err != nil {
		writeLookupError(w, err, "talkgroup not found", "failed to get talkgroup")
		return
	}
	WriteJSON(w, http.StatusOK, tg)
}

// UpdateTalkgroup patches talkgroup metadata.
func (h *TalkgroupsHandler) UpdateTalkgroup(w http.ResponseWriter, r *http.Request) {
	cid, ok := h.resolveTalkgroup(w, r)
	if !ok {
		return
	}

	var patch struct {
		AlphaTag       *string `json:"alpha_tag"`
		AlphaTagSource *string `json:"alpha_tag_source"`
		Description    *string `json:"description"`
		Group          *string `json:"group"`
		Tag            *string `json:"tag"`
		Priority       *int    `json:"priority"`
	}
	if err := DecodeJSON(r, &patch); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}
	if !checkTagSource(w, patch.AlphaTagSource, talkgroupTagSources) {
		return
	}

	if err := h.db.UpdateTalkgroupFields(r.Context(), cid.SystemID, cid.EntityID,
		patch.AlphaTag, patch.AlphaTagSource, patch.Description, patch.Group, patch.Tag, patch.Priority); err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to update talkgroup")
		return
	}

	tg, err := h.db.GetTalkgroupByComposite(r.Context(), PrincipalFrom(r), cid.SystemID, cid.EntityID)
	if err != nil {
		WriteError(w, http.StatusNotFound, "talkgroup not found")
		return
	}

	// Best-effort writeback to TR's talkgroup CSV (CSV_WRITEBACK). The
	// talkgroup_directory row mirrors the imported CSV, so it only takes the edit
	// when the CSV file on disk did too. Syncing it otherwise would turn the edit
	// into a "CSV" value: releasing the talkgroup (alpha_tag_source csv/mqtt)
	// would then re-apply the old edit instead of the real CSV or MQTT tag.
	if patch.AlphaTag != nil {
		if csvPath, ok := h.csvPaths[cid.SystemID]; ok {
			log := hlog.FromRequest(r)
			if csvErr := trconfig.UpdateTalkgroupCSV(csvPath, cid.EntityID, *patch.AlphaTag); csvErr != nil {
				log.Warn().Err(csvErr).Str("csv_path", csvPath).Int("tgid", cid.EntityID).
					Msg("failed to write back talkgroup CSV")
			} else {
				log.Info().Str("csv_path", csvPath).Int("tgid", cid.EntityID).Str("alpha_tag", *patch.AlphaTag).
					Msg("talkgroup CSV updated")
				if dirErr := h.db.UpsertTalkgroupDirectory(r.Context(), cid.SystemID, cid.EntityID,
					*patch.AlphaTag, "", "", "", "", 0,
				); dirErr != nil {
					log.Warn().Err(dirErr).Int("system_id", cid.SystemID).Int("tgid", cid.EntityID).
						Msg("failed to sync talkgroup_directory")
				}
			}
		}
	}

	WriteJSON(w, http.StatusOK, tg)
}

// ListTalkgroupCalls returns calls for a specific talkgroup.
func (h *TalkgroupsHandler) ListTalkgroupCalls(w http.ResponseWriter, r *http.Request) {
	cid, ok := h.resolveTalkgroup(w, r)
	if !ok {
		return
	}

	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	filter := database.CallFilter{
		Limit:     p.Limit,
		Offset:    p.Offset,
		SystemIDs: []int{cid.SystemID},
		Tgids:     []int{cid.EntityID},
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

	calls, total, err := h.db.ListCalls(r.Context(), PrincipalFrom(r), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list calls")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"calls":  calls,
		"total":  total,
		"limit":  p.Limit,
		"offset": p.Offset,
	})
}

// ListTalkgroupUnits returns units affiliated with a talkgroup.
func (h *TalkgroupsHandler) ListTalkgroupUnits(w http.ResponseWriter, r *http.Request) {
	cid, ok := h.resolveTalkgroup(w, r)
	if !ok {
		return
	}

	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	window := 60
	if v, ok := QueryInt(r, "window"); ok && v >= 1 && v <= 1440 {
		window = v
	}

	units, total, err := h.db.ListTalkgroupUnits(r.Context(), cid.SystemID, cid.EntityID, window, p.Limit, p.Offset)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list units")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"units":  units,
		"total":  total,
		"limit":  p.Limit,
		"offset": p.Offset,
	})
}

// GetEncryptionStats returns encryption stats per talkgroup.
func (h *TalkgroupsHandler) GetEncryptionStats(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v, ok := QueryInt(r, "hours"); ok {
		if v < 1 || v > 8760 {
			WriteError(w, http.StatusBadRequest, "hours must be between 1 and 8760")
			return
		}
		hours = v
	}
	sysid, _ := QueryString(r, "sysid")

	stats, err := h.db.GetEncryptionStats(r.Context(), hours, sysid)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to get encryption stats")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"stats": stats,
		"total": len(stats),
		"hours": hours,
	})
}

// ListTalkgroupDirectory searches the talkgroup directory (reference table imported from TR's CSV).
func (h *TalkgroupsHandler) ListTalkgroupDirectory(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	filter := database.TalkgroupDirectoryFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
	}

	filter.SystemIDs = QueryIntList(r, "system_id")
	if v, ok := QueryString(r, "search"); ok {
		filter.Search = &v
	}
	if v, ok := QueryString(r, "category"); ok {
		filter.Category = &v
	}
	if v, ok := QueryString(r, "mode"); ok {
		filter.Mode = &v
	}

	entries, total, err := h.db.SearchTalkgroupDirectory(r.Context(), PrincipalFrom(r), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to search talkgroup directory")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"talkgroups": entries,
		"total":      total,
		"limit":      p.Limit,
		"offset":     p.Offset,
	})
}

// ImportTalkgroupDirectory accepts a CSV upload and imports it into the talkgroup directory.
// Accepts either system_id or system_name (an existing system; see resolveImportSystem).
// POST /api/v1/talkgroup-directory/import?system_id=1
// POST /api/v1/talkgroup-directory/import?system_name=butco
// Content-Type: multipart/form-data (field name: "file")
func (h *TalkgroupsHandler) ImportTalkgroupDirectory(w http.ResponseWriter, r *http.Request) {
	systemID, ok := resolveImportSystem(w, r, h.db)
	if !ok {
		return
	}

	file, ok := openImportFile(w, r)
	if !ok {
		return
	}
	defer file.Close()

	result, err := trconfig.ParseTalkgroupCSVDetailed(file)
	if err != nil {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("failed to parse CSV: %v", err))
		return
	}

	if len(result.Entries) == 0 {
		WriteError(w, http.StatusBadRequest, "CSV contains no valid talkgroup entries")
		return
	}

	imported := 0
	for _, tg := range result.Entries {
		if err := h.db.UpsertTalkgroupDirectory(r.Context(), systemID, tg.Tgid,
			tg.AlphaTag, tg.Mode, tg.Description, tg.Tag, tg.Category, tg.Priority,
		); err != nil {
			continue
		}
		imported++
	}

	// Apply the newly imported directory data to heard talkgroups (CSV tags
	// replace MQTT-discovered ones; manual edits are kept).
	enriched, enrichErr := h.db.EnrichTalkgroupsFromDirectory(r.Context(), systemID, 0)
	if enrichErr != nil {
		hlog.FromRequest(r).Warn().Err(enrichErr).Int("system_id", systemID).
			Msg("failed to enrich talkgroups from directory")
	}

	resp := map[string]any{
		"imported":  imported,
		"total":     len(result.Entries),
		"system_id": systemID,
	}
	if enriched > 0 {
		resp["enriched"] = enriched
	}
	if result.Skipped > 0 {
		resp["skipped"] = result.Skipped
	}
	if result.Duplicates > 0 {
		resp["duplicates"] = result.Duplicates
	}
	WriteJSON(w, http.StatusOK, resp)
}

// Routes registers talkgroup routes on the given router.
func (h *TalkgroupsHandler) Routes(r chi.Router) {
	r.Get("/talkgroups", h.ListTalkgroups)
	r.Get("/talkgroups/encryption-stats", h.GetEncryptionStats)
	r.Get("/talkgroups/{id}", h.GetTalkgroup)
	r.Patch("/talkgroups/{id}", h.UpdateTalkgroup)
	r.Get("/talkgroups/{id}/calls", h.ListTalkgroupCalls)
	r.Get("/talkgroups/{id}/units", h.ListTalkgroupUnits)
	r.Get("/talkgroup-directory", h.ListTalkgroupDirectory)
	r.Post("/talkgroup-directory/import", h.ImportTalkgroupDirectory)
}
