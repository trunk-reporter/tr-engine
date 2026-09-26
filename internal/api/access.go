package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// accessStore is what AccessHandler needs from the database.
type accessStore interface {
	GetAnonymousAccess(ctx context.Context) (database.AnonymousAccess, error)
	SetAnonymousAccess(ctx context.Context, access string, r *auth.Restriction) (database.AnonymousAccess, error)
	ListAuditLog(ctx context.Context, f database.AuditLogFilter) ([]database.AuditEntry, int, error)
}

// AccessHandler serves whoami (§4.1), the anonymous access policy (§4.3)
// and the audit log (§4.5).
type AccessHandler struct {
	db      accessStore
	authn   *authenticator
	version string
}

func NewAccessHandler(db *database.DB, authn *authenticator, version string) *AccessHandler {
	return &AccessHandler{db: db, authn: authn, version: version}
}

func (h *AccessHandler) Routes(r chi.Router) {
	r.Get("/whoami", h.Whoami)
	r.Get("/anonymous-access", h.GetAnonymousAccess)
	r.Put("/anonymous-access", h.PutAnonymousAccess)
	r.Get("/admin/audit-log", h.ListAuditLog)
}

// whoamiKey is the caller's own key, as whoami shows it.
type whoamiKey struct {
	ID          int               `json:"id"`
	Name        string            `json:"name"`
	Prefix      string            `json:"prefix"`
	Scopes      auth.Scopes       `json:"scopes"`
	Restriction *auth.Restriction `json:"restriction"`
	ExpiresAt   *time.Time        `json:"expires_at"`
	Legacy      bool              `json:"legacy"`
}

// whoamiAnonymous is the anonymous policy as every caller may see it: never
// the lists themselves.
type whoamiAnonymous struct {
	Access     string `json:"access"`
	Restricted bool   `json:"restricted"`
}

type whoamiResponse struct {
	Credential string          `json:"credential"` // "key" or "anonymous"
	Key        *whoamiKey      `json:"key"`
	Scopes     auth.Scopes     `json:"scopes"` // effective, implications expanded
	Restricted bool            `json:"restricted"`
	Anonymous  whoamiAnonymous `json:"anonymous"`
	Version    string          `json:"version"`
}

// Whoami tells any client what its credential can do (§4.1). An invalid key
// never gets here: principal resolution answers 401 invalid_key.
func (h *AccessHandler) Whoami(w http.ResponseWriter, r *http.Request) {
	s, err := h.authn.anonymousSettings(r.Context())
	if err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("whoami: reading the anonymous access policy failed")
		WriteErrorWithCode(w, http.StatusServiceUnavailable, ErrServiceUnavail, "reading the anonymous access policy failed")
		return
	}
	p := PrincipalFrom(r)
	resp := whoamiBase(s, h.version)
	if p != nil && p.Kind == auth.KindKey {
		resp.Credential = "key"
		resp.Scopes = p.Scopes.Expand()
		resp.Restricted = p.Restricted()
		if k := keyRecordFrom(r); k != nil {
			resp.Key = &whoamiKey{
				ID:          k.ID,
				Name:        k.Name,
				Prefix:      k.Prefix,
				Scopes:      k.Scopes,
				Restriction: k.Restriction,
				ExpiresAt:   k.ExpiresAt,
				Legacy:      k.Legacy,
			}
		}
	} else {
		anon := anonymousPrincipal(s, "")
		resp.Scopes = anon.Scopes.Expand()
		resp.Restricted = anon.Restricted()
	}
	WriteJSON(w, http.StatusOK, resp)
}

func whoamiBase(s anonymousSettings, version string) whoamiResponse {
	return whoamiResponse{
		Credential: "anonymous",
		Scopes:     auth.Scopes{},
		Anonymous: whoamiAnonymous{
			Access:     s.anon.Access,
			Restricted: s.anon.Restriction != nil,
		},
		Version: shortVersion(version),
	}
}

// shortVersion is the version without the build details that follow it
// ("v1.2.3 (commit=..., built=...)" → "v1.2.3").
func shortVersion(v string) string {
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return ""
}

// GetAnonymousAccess returns the anonymous access policy, lists included
// (admin).
func (h *AccessHandler) GetAnonymousAccess(w http.ResponseWriter, r *http.Request) {
	a, err := h.db.GetAnonymousAccess(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to read the anonymous access policy")
		return
	}
	WriteJSON(w, http.StatusOK, a)
}

// PutAnonymousAccess replaces the anonymous access policy (§3.6, §4.3). Both
// fields are required; "restriction": null clears the restriction. Open
// streams pick the change up through the auth generation bump.
func (h *AccessHandler) PutAnonymousAccess(w http.ResponseWriter, r *http.Request) {
	fields, err := decodeObject(r, "access", "restriction")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, err.Error())
		return
	}
	rawAccess, ok := fields["access"]
	if !ok {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, `access is required ("off" or "listen")`)
		return
	}
	rawRestriction, ok := fields["restriction"]
	if !ok {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "restriction is required (null for no restriction)")
		return
	}
	var access string
	if err := json.Unmarshal(rawAccess, &access); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, `access: must be "off" or "listen"`)
		return
	}
	restriction, err := decodeRestriction(rawRestriction)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "restriction: "+err.Error())
		return
	}

	a, err := h.db.SetAnonymousAccess(r.Context(), access, restriction)
	var fe *database.FieldError
	if errors.As(err, &fe) {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, fe.Error())
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to store the anonymous access policy")
		return
	}
	// The database layer bumped the auth generation; drop the cached policy
	// here too so this process serves the new one at once.
	h.authn.invalidateSettings()
	hlog.FromRequest(r).Info().Str("access", a.Access).Bool("restricted", a.Restriction != nil).
		Str("by", PrincipalFrom(r).Attribution()).Msg("anonymous access policy changed")
	WriteJSON(w, http.StatusOK, a)
}

// ListAuditLog returns audit entries, newest first (§4.5).
func (h *AccessHandler) ListAuditLog(w http.ResponseWriter, r *http.Request) {
	page, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	f := database.AuditLogFilter{Limit: page.Limit, Offset: page.Offset}
	q := r.URL.Query()
	if v := q.Get("key_id"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "key_id: must be a positive integer")
			return
		}
		f.KeyID = &id
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, p.name+": must be an RFC3339 time")
				return
			}
			*p.dst = &t
		}
	}
	entries, total, err := h.db.ListAuditLog(r.Context(), f)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to read the audit log")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
}

// decodeObject decodes a JSON object body into its raw fields, so callers
// can tell an absent field from an explicit null. Fields not in allowed are
// an error.
func decodeObject(r *http.Request, allowed ...string) (map[string]json.RawMessage, error) {
	if r.Body == nil {
		return nil, errors.New("missing request body")
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&fields); err != nil {
		return nil, errors.New("invalid JSON body: want an object")
	}
	if dec.More() {
		return nil, errors.New("invalid JSON body: trailing data")
	}
	if fields == nil {
		return nil, errors.New("invalid JSON body: want an object")
	}
	for name := range fields {
		known := false
		for _, a := range allowed {
			known = known || name == a
		}
		if !known {
			return nil, fmt.Errorf("unknown field %q", name)
		}
	}
	return fields, nil
}

// isNull reports whether a raw JSON value is null.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeRestriction decodes a restriction field: null means none (nil);
// an object is a restriction, even an empty one (which allows nothing).
func decodeRestriction(raw json.RawMessage) (*auth.Restriction, error) {
	if isNull(raw) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("must be an object or null")
	}
	r := new(auth.Restriction)
	if err := json.Unmarshal(trimmed, r); err != nil {
		return nil, err
	}
	return r, nil
}
