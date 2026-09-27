package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/database"
)

type SystemsHandler struct {
	db *database.DB
}

func NewSystemsHandler(db *database.DB) *SystemsHandler {
	return &SystemsHandler{db: db}
}

// ListSystems returns all active systems the caller may see, with embedded
// sites.
func (h *SystemsHandler) ListSystems(w http.ResponseWriter, r *http.Request) {
	systems, err := h.db.ListSystemsWithSites(r.Context(), PrincipalFrom(r))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list systems")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"systems": systems,
		"total":   len(systems),
	})
}

// GetSystem returns a single system by ID with embedded sites. A system the
// caller may not see is 404, like one that doesn't exist.
func (h *SystemsHandler) GetSystem(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid system ID")
		return
	}
	system, err := h.db.GetSystemByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "system not found", "failed to get system")
		return
	}
	WriteJSON(w, http.StatusOK, system)
}

// UpdateSystem patches system metadata.
func (h *SystemsHandler) UpdateSystem(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid system ID")
		return
	}

	var patch struct {
		Name  *string `json:"name"`
		Sysid *string `json:"sysid"`
		Wacn  *string `json:"wacn"`
	}
	if err := DecodeJSON(r, &patch); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}

	if err := h.db.UpdateSystemFields(r.Context(), id, patch.Name, patch.Sysid, patch.Wacn); err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to update system")
		return
	}

	system, err := h.db.GetSystemByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		WriteError(w, http.StatusNotFound, "system not found")
		return
	}
	WriteJSON(w, http.StatusOK, system)
}

// GetSite returns a single site by ID. A site whose system the caller may not
// see is 404, like one that doesn't exist.
func (h *SystemsHandler) GetSite(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid site ID")
		return
	}
	site, err := h.db.GetSiteByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "site not found", "failed to get site")
		return
	}
	WriteJSON(w, http.StatusOK, site)
}

// UpdateSite patches site metadata.
func (h *SystemsHandler) UpdateSite(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid site ID")
		return
	}

	var patch struct {
		ShortName  *string `json:"short_name"`
		InstanceID *string `json:"instance_id"`
		Nac        *string `json:"nac"`
		Rfss       *int    `json:"rfss"`
		P25SiteID  *int    `json:"p25_site_id"`
	}
	if err := DecodeJSON(r, &patch); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}

	if err := h.db.UpdateSiteFields(r.Context(), id, patch.ShortName, patch.InstanceID, patch.Nac, patch.Rfss, patch.P25SiteID); err != nil {
		if err.Error() == "site not found" {
			WriteError(w, http.StatusNotFound, "site not found")
			return
		}
		WriteError(w, http.StatusInternalServerError, "failed to update site")
		return
	}

	site, err := h.db.GetSiteByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		WriteError(w, http.StatusNotFound, "site not found")
		return
	}
	WriteJSON(w, http.StatusOK, site)
}

// ListP25Systems returns P25 systems grouped by network.
func (h *SystemsHandler) ListP25Systems(w http.ResponseWriter, r *http.Request) {
	systems, err := h.db.ListP25Systems(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list P25 systems")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"systems": systems,
		"total":   len(systems),
	})
}

// Routes registers system/site routes on the given router.
func (h *SystemsHandler) Routes(r chi.Router) {
	r.Get("/systems", h.ListSystems)
	r.Get("/systems/{id}", h.GetSystem)
	r.Patch("/systems/{id}", h.UpdateSystem)
	r.Get("/sites/{id}", h.GetSite)
	r.Patch("/sites/{id}", h.UpdateSite)
	r.Get("/p25-systems", h.ListP25Systems)
}
