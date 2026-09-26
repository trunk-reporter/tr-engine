package api

import (
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// importSystemResolver is the subset of database.DB used to pick the target
// system for a CSV import.
type importSystemResolver interface {
	GetSystemByID(ctx context.Context, p *auth.Principal, systemID int) (*database.SystemAPI, error)
	FindSystemsByName(ctx context.Context, name string) ([]database.AmbiguousMatch, error)
}

// resolveImportSystem resolves the target system of a CSV import from either
// ?system_id= (must exist) or ?system_name=. A name selects the one non-deleted
// system with that name or with a site of that short name. It never creates a
// system: systems are created when trunk-recorder first reports them (MQTT,
// upload, file watch or TR_DIR), and a system created here would never receive
// that traffic. So an unknown name is a 404 and a name matching several
// systems is a 409 listing them. On failure it writes the error response and
// returns ok=false.
func resolveImportSystem(w http.ResponseWriter, r *http.Request, db importSystemResolver) (int, bool) {
	if id, ok := QueryInt(r, "system_id"); ok && id > 0 {
		if _, err := db.GetSystemByID(r.Context(), PrincipalFrom(r), id); err != nil {
			WriteError(w, http.StatusNotFound, fmt.Sprintf("system_id %d not found", id))
			return 0, false
		}
		return id, true
	}
	if name, ok := QueryString(r, "system_name"); ok && strings.TrimSpace(name) != "" {
		name = strings.TrimSpace(name)
		matches, err := db.FindSystemsByName(r.Context(), name)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, fmt.Sprintf("failed to resolve system %q: %v", name, err))
			return 0, false
		}
		switch {
		case len(matches) == 0:
			WriteError(w, http.StatusNotFound, fmt.Sprintf(
				"no system named %q; systems are created when trunk-recorder first reports them, "+
					"so import after that or use system_id", name))
			return 0, false
		case len(matches) > 1:
			WriteJSON(w, http.StatusConflict, AmbiguousErrorResponse{
				Code:    ErrAmbiguousID,
				Error:   fmt.Sprintf("system_name %q matches %d systems; use system_id", name, len(matches)),
				Matches: matches,
			})
			return 0, false
		}
		return matches[0].SystemID, true
	}
	WriteError(w, http.StatusBadRequest, "system_id or system_name query parameter is required")
	return 0, false
}

// openImportFile parses a multipart CSV upload (10 MB max) and returns its
// "file" field. On failure it writes the error response and returns ok=false.
func openImportFile(w http.ResponseWriter, r *http.Request) (multipart.File, bool) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid multipart form (10 MB max)")
		return nil, false
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "missing 'file' field in multipart form")
		return nil, false
	}
	return file, true
}

// Allowed alpha_tag_source values (mirrors the chk_*_alpha_tag_source constraints).
var (
	talkgroupTagSources = []string{"manual", "csv", "mqtt", "directory"}
	unitTagSources      = []string{"manual", "csv", "mqtt"}
)

// checkTagSource validates an optional alpha_tag_source PATCH value. Empty means
// "leave unchanged". On an invalid value it writes a 400 and returns false.
func checkTagSource(w http.ResponseWriter, source *string, allowed []string) bool {
	if source == nil || *source == "" {
		return true
	}
	for _, s := range allowed {
		if *source == s {
			return true
		}
	}
	WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody,
		"alpha_tag_source must be one of: "+strings.Join(allowed, ", "))
	return false
}
