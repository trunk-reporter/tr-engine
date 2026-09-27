package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// keyStore is what KeysHandler needs from the database.
type keyStore interface {
	ListAPIKeys(ctx context.Context, includeRevoked bool) ([]database.APIKey, error)
	CreateAPIKey(ctx context.Context, in database.NewAPIKey) (*database.APIKeyWithPlaintext, error)
	GetAPIKeyByID(ctx context.Context, id int) (*database.APIKey, error)
	PatchAPIKey(ctx context.Context, id int, p database.APIKeyPatch, guard database.LastAdminGuard) (*database.APIKey, error)
	RevokeAPIKey(ctx context.Context, id int, guard database.LastAdminGuard) (*database.APIKey, error)
}

// KeysHandler manages API keys (§4.2, admin).
type KeysHandler struct {
	db    keyStore
	authn *authenticator
}

func NewKeysHandler(db *database.DB, authn *authenticator) *KeysHandler {
	return &KeysHandler{db: db, authn: authn}
}

func (h *KeysHandler) Routes(r chi.Router) {
	r.Get("/keys", h.List)
	r.Post("/keys", h.Create)
	r.Get("/keys/{id}", h.Get)
	r.Patch("/keys/{id}", h.Patch)
	r.Delete("/keys/{id}", h.Revoke)
}

// List returns keys ordered by ID; revoked keys only with include_revoked.
func (h *KeysHandler) List(w http.ResponseWriter, r *http.Request) {
	includeRevoked := false
	if v := r.URL.Query().Get("include_revoked"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "include_revoked: must be true or false")
			return
		}
		includeRevoked = b
	}
	keys, err := h.db.ListAPIKeys(r.Context(), includeRevoked)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list API keys")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"keys": keys, "total": len(keys)})
}

// Create makes a key and returns it with its plaintext, which is never
// shown again.
func (h *KeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string            `json:"name"`
		Scopes       []string          `json:"scopes"`
		Restriction  *auth.Restriction `json:"restriction"`
		ExpiresAt    *time.Time        `json:"expires_at"`
		RateLimitRPS *float32          `json:"rate_limit_rps"`
	}
	if r.Body == nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "missing request body")
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body: "+err.Error())
		return
	}
	scopes, err := auth.ParseScopes(req.Scopes)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "scopes: "+err.Error())
		return
	}
	key, err := h.db.CreateAPIKey(r.Context(), database.NewAPIKey{
		Name:         req.Name,
		Scopes:       scopes,
		Restriction:  req.Restriction,
		ExpiresAt:    req.ExpiresAt,
		RateLimitRPS: req.RateLimitRPS,
	})
	if writeKeyError(w, err) {
		return
	}
	hlog.FromRequest(r).Info().Int("key_id", key.ID).Str("name", key.Name).Str("prefix", key.Prefix).
		Strs("scopes", key.Scopes.Strings()).Str("by", PrincipalFrom(r).Attribution()).Msg("API key created")
	WriteJSON(w, http.StatusCreated, key)
}

// Get returns one key.
func (h *KeysHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := keyIDParam(w, r)
	if !ok {
		return
	}
	key, err := h.db.GetAPIKeyByID(r.Context(), id)
	if writeKeyError(w, err) {
		return
	}
	WriteJSON(w, http.StatusOK, key)
}

// Patch changes a key. An absent field is left unchanged; an explicit null
// clears restriction, expires_at or rate_limit_rps.
func (h *KeysHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, ok := keyIDParam(w, r)
	if !ok {
		return
	}
	fields, err := decodeObject(r, "name", "scopes", "restriction", "expires_at", "rate_limit_rps")
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, err.Error())
		return
	}
	patch, msg := parseKeyPatch(fields)
	if msg != "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, msg)
		return
	}
	key, err := h.db.PatchAPIKey(r.Context(), id, patch, database.GuardLastAdmin)
	if writeKeyError(w, err) {
		return
	}
	// The database layer bumped the auth generation, which clears the key
	// cache; drop the entry explicitly as well.
	h.authn.invalidateKey(id)
	hlog.FromRequest(r).Info().Int("key_id", id).Str("by", PrincipalFrom(r).Attribution()).Msg("API key changed")
	WriteJSON(w, http.StatusOK, key)
}

// parseKeyPatch turns PATCH /keys/{id} fields into a patch, or returns a
// message naming the bad field.
func parseKeyPatch(fields map[string]json.RawMessage) (database.APIKeyPatch, string) {
	var p database.APIKeyPatch
	if raw, ok := fields["name"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &p.Name) != nil {
			return p, "name: must be a string"
		}
		p.SetName = true
	}
	if raw, ok := fields["scopes"]; ok {
		var in []string
		if isNull(raw) || json.Unmarshal(raw, &in) != nil {
			return p, "scopes: must be an array of scope names"
		}
		scopes, err := auth.ParseScopes(in)
		if err != nil {
			return p, "scopes: " + err.Error()
		}
		p.SetScopes, p.Scopes = true, scopes
	}
	if raw, ok := fields["restriction"]; ok {
		r, err := decodeRestriction(raw)
		if err != nil {
			return p, "restriction: " + err.Error()
		}
		p.SetRestriction, p.Restriction = true, r
	}
	if raw, ok := fields["expires_at"]; ok {
		p.SetExpiresAt = true
		if !isNull(raw) {
			var t time.Time
			if err := json.Unmarshal(raw, &t); err != nil {
				return p, "expires_at: must be an RFC3339 time or null"
			}
			p.ExpiresAt = &t
		}
	}
	if raw, ok := fields["rate_limit_rps"]; ok {
		p.SetRateLimitRPS = true
		if !isNull(raw) {
			var v float32
			if err := json.Unmarshal(raw, &v); err != nil {
				return p, "rate_limit_rps: must be a number or null"
			}
			p.RateLimitRPS = &v
		}
	}
	return p, ""
}

// Revoke revokes a key. Revoking a revoked key succeeds.
func (h *KeysHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	id, ok := keyIDParam(w, r)
	if !ok {
		return
	}
	_, err := h.db.RevokeAPIKey(r.Context(), id, database.GuardLastAdmin)
	if writeKeyError(w, err) {
		return
	}
	h.authn.invalidateKey(id)
	hlog.FromRequest(r).Info().Int("key_id", id).Str("by", PrincipalFrom(r).Attribution()).Msg("API key revoked")
	w.WriteHeader(http.StatusNoContent)
}

func keyIDParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := PathInt(r, "id")
	if err != nil || id <= 0 {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, "id: must be a positive integer")
		return 0, false
	}
	return id, true
}

// writeKeyError answers a key store error and reports whether there was one.
func writeKeyError(w http.ResponseWriter, err error) bool {
	var fe *database.FieldError
	switch {
	case err == nil:
		return false
	case errors.As(err, &fe):
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, fe.Error())
	case errors.Is(err, database.ErrAPIKeyNotFound):
		WriteErrorWithCode(w, http.StatusNotFound, ErrNotFound, "API key not found")
	case errors.Is(err, database.ErrAPIKeyRevoked):
		WriteErrorWithCode(w, http.StatusConflict, ErrConflict, "API key is revoked and can't be changed")
	case errors.Is(err, database.ErrLastAdminKey):
		WriteErrorWithCode(w, http.StatusConflict, ErrConflict, database.ErrLastAdminKey.Error())
	default:
		WriteError(w, http.StatusInternalServerError, "API key operation failed")
	}
	return true
}
