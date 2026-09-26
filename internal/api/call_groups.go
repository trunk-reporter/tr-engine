package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/database"
)

type CallGroupsHandler struct {
	db         *database.DB
	trAudioDir string
}

func NewCallGroupsHandler(db *database.DB, trAudioDir string) *CallGroupsHandler {
	return &CallGroupsHandler{db: db, trAudioDir: trAudioDir}
}

// ListCallGroups returns deduplicated call groups.
func (h *CallGroupsHandler) ListCallGroups(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}

	filter := database.CallGroupFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
	}

	filter.Sysids = QueryStringList(r, "sysid")
	filter.Tgids = QueryIntList(r, "tgid")
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

	groups, total, err := h.db.ListCallGroups(r.Context(), PrincipalFrom(r), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list call groups")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"call_groups": groups,
		"total":       total,
		"limit":       p.Limit,
		"offset":      p.Offset,
	})
}

// GetCallGroup returns a call group with all its individual recordings. A
// group outside the caller's restriction is 404, like a missing one.
func (h *CallGroupsHandler) GetCallGroup(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid call group ID")
		return
	}

	group, calls, err := h.db.GetCallGroupByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "call group not found", "failed to get call group")
		return
	}
	if h.trAudioDir != "" {
		for i := range calls {
			if calls[i].AudioURL == nil && calls[i].CallFilename != "" {
				url := fmt.Sprintf("/api/v1/calls/%d/audio", calls[i].CallID)
				calls[i].AudioURL = &url
			}
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"call_group": group,
		"calls":      calls,
	})
}

// Routes registers call group routes on the given router.
func (h *CallGroupsHandler) Routes(r chi.Router) {
	r.Get("/call-groups", h.ListCallGroups)
	r.Get("/call-groups/{id}", h.GetCallGroup)
}
