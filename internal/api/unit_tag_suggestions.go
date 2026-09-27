package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/database"
)

// unitTagSuggestionStore is the subset of database.DB used by UnitTagSuggestionsHandler.
type unitTagSuggestionStore interface {
	ListUnitTagSuggestions(ctx context.Context, filter database.UnitTagSuggestionFilter) ([]database.UnitTagSuggestionAPI, int, error)
	GetUnitTagSuggestion(ctx context.Context, id int64) (*database.UnitTagSuggestionAPI, error)
	ApproveUnitTagSuggestion(ctx context.Context, id int64, alphaTag, decidedBy string) (*database.UnitTagApproval, error)
	DismissUnitTagSuggestion(ctx context.Context, id int64, decidedBy string) error
	GetUnitTagScanStatus(ctx context.Context) (database.UnitTagScanStatus, error)
	GetUnitByComposite(ctx context.Context, systemID, unitID int) (*database.UnitAPI, error)
}

// UnitTagSuggestionsHandler serves the unit alpha tag review queue. Candidates
// are produced by the background scanner (internal/unittags); the unit record
// is only changed when a reviewer approves one.
type UnitTagSuggestionsHandler struct {
	db             unitTagSuggestionStore
	csvPaths       map[int]string // system_id → unit CSV file path for writeback
	scannerEnabled bool
	minCalls       int
	minShare       float64
}

func NewUnitTagSuggestionsHandler(db *database.DB, csvPaths map[int]string, scannerEnabled bool, minCalls int, minShare float64) *UnitTagSuggestionsHandler {
	return &UnitTagSuggestionsHandler{
		db:             db,
		csvPaths:       csvPaths,
		scannerEnabled: scannerEnabled,
		minCalls:       minCalls,
		minShare:       minShare,
	}
}

// unitTagScannerInfo describes the scanner configuration and backfill progress.
type unitTagScannerInfo struct {
	Enabled  bool    `json:"enabled"`
	MinCalls int     `json:"min_calls"`
	MinShare float64 `json:"min_share"`
	database.UnitTagScanStatus
}

// ListUnitTagSuggestions returns suggestions for review. Pending suggestions
// are only listed once they pass the review gate (UNIT_TAG_SUGGESTIONS_MIN_CALLS
// and UNIT_TAG_SUGGESTIONS_MIN_SHARE, and not already the unit's tag).
func (h *UnitTagSuggestionsHandler) ListUnitTagSuggestions(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	status := database.SuggestionPending
	if v, ok := QueryString(r, "status"); ok {
		switch v {
		case database.SuggestionPending, database.SuggestionApproved, database.SuggestionDismissed, "all":
			status = v
		default:
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter,
				"invalid status: must be pending, approved, dismissed, or all")
			return
		}
	}

	filter := database.UnitTagSuggestionFilter{
		Status:    status,
		SystemIDs: QueryIntListAliased(r, "system_id", "systems"),
		UnitIDs:   QueryIntListAliased(r, "unit_id", "units"),
		MinCalls:  h.minCalls,
		MinShare:  h.minShare,
		Limit:     p.Limit,
		Offset:    p.Offset,
	}

	suggestions, total, err := h.db.ListUnitTagSuggestions(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list unit tag suggestions")
		return
	}
	scan, err := h.db.GetUnitTagScanStatus(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to read unit tag scanner status")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"suggestions": suggestions,
		"total":       total,
		"limit":       p.Limit,
		"offset":      p.Offset,
		"scanner": unitTagScannerInfo{
			Enabled:           h.scannerEnabled,
			MinCalls:          h.minCalls,
			MinShare:          h.minShare,
			UnitTagScanStatus: scan,
		},
	})
}

// GetUnitTagSuggestion returns one suggestion by ID, regardless of status.
func (h *UnitTagSuggestionsHandler) GetUnitTagSuggestion(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSuggestionID(w, r)
	if !ok {
		return
	}
	s, err := h.db.GetUnitTagSuggestion(r.Context(), id)
	if err != nil {
		writeSuggestionError(w, err, "failed to get unit tag suggestion")
		return
	}
	WriteJSON(w, http.StatusOK, s)
}

// ApproveUnitTagSuggestion applies a pending suggestion to its unit. The
// optional body {"alpha_tag": "..."} overrides the proposed tag (edit, then
// approve). The unit is written through the same path as PATCH /units/{id}
// with alpha_tag_source=manual, including CSV writeback.
func (h *UnitTagSuggestionsHandler) ApproveUnitTagSuggestion(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSuggestionID(w, r)
	if !ok {
		return
	}

	var body struct {
		AlphaTag *string `json:"alpha_tag"`
	}
	if err := DecodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}
	override := ""
	if body.AlphaTag != nil {
		override = strings.TrimSpace(*body.AlphaTag)
		if override == "" {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "alpha_tag must not be empty")
			return
		}
	}

	approval, err := h.db.ApproveUnitTagSuggestion(r.Context(), id, override, PrincipalFrom(r).Attribution())
	if err != nil {
		writeSuggestionError(w, err, "failed to approve unit tag suggestion")
		return
	}

	// Best-effort writeback to TR's unit tags CSV (same as PATCH /units/{id})
	writeBackUnitCSV(r, h.csvPaths, approval.SystemID, approval.UnitID, approval.AppliedTag)

	s, err := h.db.GetUnitTagSuggestion(r.Context(), id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "suggestion approved but failed to reload it")
		return
	}
	unit, err := h.db.GetUnitByComposite(r.Context(), approval.SystemID, approval.UnitID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "suggestion approved but failed to reload the unit")
		return
	}
	hlog.FromRequest(r).Info().
		Int64("suggestion_id", id).
		Int("system_id", approval.SystemID).
		Int("unit_id", approval.UnitID).
		Str("alpha_tag", approval.AppliedTag).
		Msg("unit tag suggestion approved")

	WriteJSON(w, http.StatusOK, map[string]any{
		"suggestion": s,
		"unit":       unit,
	})
}

// DismissUnitTagSuggestion marks a pending suggestion dismissed without
// touching the unit. Later sightings keep counting on the dismissed row but
// it never returns to the pending queue.
func (h *UnitTagSuggestionsHandler) DismissUnitTagSuggestion(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSuggestionID(w, r)
	if !ok {
		return
	}
	if err := h.db.DismissUnitTagSuggestion(r.Context(), id, PrincipalFrom(r).Attribution()); err != nil {
		writeSuggestionError(w, err, "failed to dismiss unit tag suggestion")
		return
	}
	s, err := h.db.GetUnitTagSuggestion(r.Context(), id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "suggestion dismissed but failed to reload it")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"suggestion": s})
}

func parseSuggestionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "invalid suggestion id")
		return 0, false
	}
	return id, true
}

// writeSuggestionError maps database errors to HTTP responses:
// 404 for a missing suggestion or unit, 409 when the suggestion is not pending.
func writeSuggestionError(w http.ResponseWriter, err error, msg string) {
	var statusErr *database.SuggestionStatusError
	switch {
	case errors.Is(err, database.ErrSuggestionNotFound):
		WriteError(w, http.StatusNotFound, "unit tag suggestion not found")
	case errors.Is(err, database.ErrSuggestionUnitNotFound):
		WriteError(w, http.StatusNotFound, "unit not found")
	case errors.As(err, &statusErr):
		WriteErrorWithCodeDetail(w, http.StatusConflict, ErrConflict, statusErr.Error(), "status: "+statusErr.Status)
	default:
		WriteError(w, http.StatusInternalServerError, msg)
	}
}

// Routes registers unit tag suggestion routes on the given router.
func (h *UnitTagSuggestionsHandler) Routes(r chi.Router) {
	r.Get("/unit-tag-suggestions", h.ListUnitTagSuggestions)
	r.Get("/unit-tag-suggestions/{id}", h.GetUnitTagSuggestion)
	r.Post("/unit-tag-suggestions/{id}/approve", h.ApproveUnitTagSuggestion)
	r.Post("/unit-tag-suggestions/{id}/dismiss", h.DismissUnitTagSuggestion)
}
