package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/trconfig"
)

// unitTagImporter is the subset of database.DB used by ImportUnitTags.
type unitTagImporter interface {
	importSystemResolver
	ImportUnitTags(ctx context.Context, systemID int, tags []database.UnitTag) (int64, error)
}

type UnitsHandler struct {
	db       *database.DB
	tags     unitTagImporter
	csvPaths map[int]string // system_id → unit CSV file path for writeback
}

func NewUnitsHandler(db *database.DB, csvPaths map[int]string) *UnitsHandler {
	return &UnitsHandler{db: db, tags: db, csvPaths: csvPaths}
}

var unitSortFields = map[string]string{
	"alpha_tag":       "u.alpha_tag",
	"unit_id":         "u.unit_id",
	"last_seen":       "u.last_seen",
	"last_event_time": "u.last_event_time",
}

// ListUnits returns radio units with optional filters.
func (h *UnitsHandler) ListUnits(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	sort := ParseSort(r, "unit_id", unitSortFields)

	filter := database.UnitFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
		Sort:   sort.SQLOrderBy(unitSortFields),
	}

	if v, ok := QueryString(r, "sysid"); ok {
		filter.Sysid = &v
	}
	if v, ok := QueryString(r, "search"); ok {
		filter.Search = &v
	}
	if v, ok := QueryInt(r, "active_within"); ok {
		filter.ActiveWithin = &v
	}
	filter.Talkgroups = QueryIntList(r, "talkgroup")

	units, total, err := h.db.ListUnits(r.Context(), filter)
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

// GetUnit returns a single unit by composite or plain ID.
func (h *UnitsHandler) GetUnit(w http.ResponseWriter, r *http.Request) {
	cid, err := ParseCompositeID(r, "id")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	if cid.IsPlain {
		matches, err := h.db.FindUnitSystems(r.Context(), cid.EntityID)
		if err != nil || len(matches) == 0 {
			WriteError(w, http.StatusNotFound, "unit not found")
			return
		}
		if len(matches) > 1 {
			WriteAmbiguous(w, cid.EntityID, matches)
			return
		}
		cid.SystemID = matches[0].SystemID
	}

	unit, err := h.db.GetUnitByComposite(r.Context(), cid.SystemID, cid.EntityID)
	if err != nil {
		WriteError(w, http.StatusNotFound, "unit not found")
		return
	}
	WriteJSON(w, http.StatusOK, unit)
}

// UpdateUnit patches unit metadata.
func (h *UnitsHandler) UpdateUnit(w http.ResponseWriter, r *http.Request) {
	cid, err := ParseCompositeID(r, "id")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	if cid.IsPlain {
		matches, err := h.db.FindUnitSystems(r.Context(), cid.EntityID)
		if err != nil || len(matches) == 0 {
			WriteError(w, http.StatusNotFound, "unit not found")
			return
		}
		if len(matches) > 1 {
			WriteAmbiguous(w, cid.EntityID, matches)
			return
		}
		cid.SystemID = matches[0].SystemID
	}

	var patch struct {
		AlphaTag       *string `json:"alpha_tag"`
		AlphaTagSource *string `json:"alpha_tag_source"`
	}
	if err := DecodeJSON(r, &patch); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}
	if !checkTagSource(w, patch.AlphaTagSource, unitTagSources) {
		return
	}

	if err := h.db.UpdateUnitFields(r.Context(), cid.SystemID, cid.EntityID,
		patch.AlphaTag, patch.AlphaTagSource); err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to update unit")
		return
	}

	unit, err := h.db.GetUnitByComposite(r.Context(), cid.SystemID, cid.EntityID)
	if err != nil {
		WriteError(w, http.StatusNotFound, "unit not found")
		return
	}

	// Best-effort writeback to TR's unit tags CSV
	if patch.AlphaTag != nil {
		writeBackUnitCSV(r, h.csvPaths, cid.SystemID, cid.EntityID, *patch.AlphaTag)
	}

	WriteJSON(w, http.StatusOK, unit)
}

// writeBackUnitCSV writes a unit alpha_tag edit back to trunk-recorder's unit
// tags CSV when CSV_WRITEBACK is configured for the system. Best-effort:
// failures are logged, never returned. Shared by PATCH /units/{id} and unit
// tag suggestion approval.
func writeBackUnitCSV(r *http.Request, csvPaths map[int]string, systemID, unitID int, alphaTag string) {
	csvPath, ok := csvPaths[systemID]
	if !ok {
		return
	}
	if err := trconfig.UpdateUnitCSV(csvPath, unitID, alphaTag); err != nil {
		log := hlog.FromRequest(r)
		log.Warn().Err(err).Str("csv_path", csvPath).Int("unit_id", unitID).
			Msg("failed to write back unit CSV")
	}
}

// ListUnitCalls returns calls that include transmissions from a specific unit.
func (h *UnitsHandler) ListUnitCalls(w http.ResponseWriter, r *http.Request) {
	cid, err := ParseCompositeID(r, "id")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	if cid.IsPlain {
		matches, err := h.db.FindUnitSystems(r.Context(), cid.EntityID)
		if err != nil || len(matches) == 0 {
			WriteError(w, http.StatusNotFound, "unit not found")
			return
		}
		if len(matches) > 1 {
			WriteAmbiguous(w, cid.EntityID, matches)
			return
		}
		cid.SystemID = matches[0].SystemID
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
		UnitIDs:   []int{cid.EntityID},
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

// ListUnitEvents returns events for a specific unit.
func (h *UnitsHandler) ListUnitEvents(w http.ResponseWriter, r *http.Request) {
	cid, err := ParseCompositeID(r, "id")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	if cid.IsPlain {
		matches, err := h.db.FindUnitSystems(r.Context(), cid.EntityID)
		if err != nil || len(matches) == 0 {
			WriteError(w, http.StatusNotFound, "unit not found")
			return
		}
		if len(matches) > 1 {
			WriteAmbiguous(w, cid.EntityID, matches)
			return
		}
		cid.SystemID = matches[0].SystemID
	}

	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	filter := database.UnitEventFilter{
		SystemID: cid.SystemID,
		UnitID:   cid.EntityID,
		Limit:    p.Limit,
		Offset:   p.Offset,
	}
	if v, ok := QueryString(r, "type"); ok {
		filter.EventType = &v
	}
	if v, ok := QueryInt(r, "talkgroup"); ok {
		filter.Tgid = &v
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

	events, total, err := h.db.ListUnitEvents(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"total":  total,
		"limit":  p.Limit,
		"offset": p.Offset,
	})
}

// ImportUnitTags accepts a trunk-recorder unit tags CSV upload (TR's unitTagsFile:
// unit_id,alpha_tag) and imports it into the units table with alpha_tag_source
// 'csv'. CSV tags replace MQTT-discovered and previously imported CSV tags;
// manual edits are kept. The whole file is applied in one statement, so it is
// all-or-nothing. Accepts either system_id or system_name (an existing system,
// resolved like the talkgroup directory import; see resolveImportSystem).
// POST /api/v1/unit-tags/import?system_id=1
// POST /api/v1/unit-tags/import?system_name=butco
// Content-Type: multipart/form-data (field name: "file")
func (h *UnitsHandler) ImportUnitTags(w http.ResponseWriter, r *http.Request) {
	systemID, ok := resolveImportSystem(w, r, h.tags)
	if !ok {
		return
	}

	file, ok := openImportFile(w, r)
	if !ok {
		return
	}
	defer file.Close()

	result, err := trconfig.ParseUnitCSV(file)
	if err != nil {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("failed to parse CSV: %v", err))
		return
	}
	if len(result.Entries) == 0 {
		WriteError(w, http.StatusBadRequest, "CSV contains no valid unit tag entries")
		return
	}

	changed, err := h.tags.ImportUnitTags(r.Context(), systemID, unitTagsFromCSV(result.Entries))
	if err != nil {
		hlog.FromRequest(r).Error().Err(err).Int("system_id", systemID).
			Int("rows", len(result.Entries)).Msg("failed to import unit tags")
		WriteError(w, http.StatusInternalServerError, "failed to import unit tags")
		return
	}
	hlog.FromRequest(r).Info().Int("system_id", systemID).Int("rows", len(result.Entries)).
		Int64("changed", changed).Msg("unit tags imported")

	resp := map[string]any{
		"imported":  len(result.Entries),
		"total":     len(result.Entries),
		"system_id": systemID,
		"skipped":   result.Skipped,
	}
	if result.Duplicates > 0 {
		resp["duplicates"] = result.Duplicates
	}
	WriteJSON(w, http.StatusOK, resp)
}

// unitTagsFromCSV converts parsed unit tags CSV rows for database.ImportUnitTags.
func unitTagsFromCSV(entries []trconfig.UnitEntry) []database.UnitTag {
	tags := make([]database.UnitTag, len(entries))
	for i, e := range entries {
		tags[i] = database.UnitTag{UnitID: e.UnitID, AlphaTag: e.AlphaTag}
	}
	return tags
}

// Routes registers unit routes on the given router.
func (h *UnitsHandler) Routes(r chi.Router) {
	r.Get("/units", h.ListUnits)
	r.Get("/units/{id}", h.GetUnit)
	r.Patch("/units/{id}", h.UpdateUnit)
	r.Get("/units/{id}/calls", h.ListUnitCalls)
	r.Get("/units/{id}/events", h.ListUnitEvents)
	r.Post("/unit-tags/import", h.ImportUnitTags)
}
