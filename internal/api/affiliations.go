package api

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

type AffiliationsHandler struct {
	live LiveDataSource
}

func NewAffiliationsHandler(live LiveDataSource) *AffiliationsHandler {
	return &AffiliationsHandler{live: live}
}

// ListAffiliations returns current talkgroup affiliations from the in-memory map.
func (h *AffiliationsHandler) ListAffiliations(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"affiliations": []any{},
			"total":        0,
			"limit":        50,
			"offset":       0,
			"summary":      map[string]any{"talkgroup_counts": map[string]any{}},
		})
		return
	}

	// Optional filters (comma-separated multi-value)
	systemIDs := QueryIntList(r, "system_id")
	sysids := QueryStringList(r, "sysid")
	tgids := QueryIntList(r, "tgid")
	unitIDs := QueryIntList(r, "unit_id")
	staleThreshold, hasStaleThreshold := QueryInt(r, "stale_threshold") // seconds
	activeWithin, hasActiveWithin := QueryInt(r, "active_within")       // seconds
	status, hasStatus := QueryString(r, "status")

	all := h.live.UnitAffiliations()

	now := time.Now()
	filtered := make([]UnitAffiliationData, 0, len(all))
	for _, a := range all {
		if len(systemIDs) > 0 && !intSliceContains(systemIDs, a.SystemID) {
			continue
		}
		if len(sysids) > 0 && !stringSliceContains(sysids, a.Sysid) {
			continue
		}
		if len(tgids) > 0 && !intSliceContains(tgids, a.Tgid) {
			continue
		}
		if len(unitIDs) > 0 && !intSliceContains(unitIDs, a.UnitID) {
			continue
		}
		if hasStatus && a.Status != status {
			continue
		}
		if hasStaleThreshold {
			age := now.Sub(a.LastEventTime)
			if age > time.Duration(staleThreshold)*time.Second {
				continue
			}
		}
		if hasActiveWithin {
			age := now.Sub(a.LastEventTime)
			if age > time.Duration(activeWithin)*time.Second {
				continue
			}
		}
		filtered = append(filtered, a)
	}

	// Compute summary over full filtered set (before pagination), keyed
	// "system_id:tgid" so equal tgids of different systems stay apart.
	tgCounts := make(map[string]int)
	for _, a := range filtered {
		if a.Status == "affiliated" {
			tgCounts[strconv.Itoa(a.SystemID)+":"+strconv.Itoa(a.Tgid)]++
		}
	}

	// Sort by tgid then unit_id
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Tgid != filtered[j].Tgid {
			return filtered[i].Tgid < filtered[j].Tgid
		}
		return filtered[i].UnitID < filtered[j].UnitID
	})

	total := len(filtered)

	// Apply pagination
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	start := p.Offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + p.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	WriteJSON(w, http.StatusOK, map[string]any{
		"affiliations": page,
		"total":        total,
		"limit":        p.Limit,
		"offset":       p.Offset,
		"summary": map[string]any{
			"talkgroup_counts": tgCounts,
		},
	})
}

func (h *AffiliationsHandler) Routes(r chi.Router) {
	r.Get("/unit-affiliations", h.ListAffiliations)
}
