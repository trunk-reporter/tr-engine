package api

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// SetupHandler handles first-run admin creation.
type SetupHandler struct {
	db  *database.DB
	log zerolog.Logger
}

func NewSetupHandler(db *database.DB, log zerolog.Logger) *SetupHandler {
	return &SetupHandler{db: db, log: log}
}

// Status returns whether first-run setup is needed (zero users exist).
// GET /auth/setup
func (h *SetupHandler) Status(w http.ResponseWriter, r *http.Request) {
	count, err := h.db.CountUsers(r.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("setup: count users failed")
		WriteError(w, http.StatusInternalServerError, "database error")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"needs_setup": count == 0,
		"user_count":  count,
	})
}

// Setup creates the first admin user. Only works when zero users exist.
// Uses an atomic INSERT ... WHERE NOT EXISTS to prevent race conditions
// where two concurrent requests could both create admin users.
// POST /auth/setup {"username": "admin@example.com", "password": "..."}
func (h *SetupHandler) Setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	req.Username = database.NormalizeUsername(req.Username)
	if req.Username == "" {
		WriteError(w, http.StatusBadRequest, "username is required")
		return
	}
	if len(req.Password) < 8 {
		WriteError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error().Err(err).Msg("setup: bcrypt failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Atomic: inserts only if no users exist, returns nil if race lost
	user, err := h.db.CreateFirstUser(r.Context(), req.Username, string(hash), "admin")
	if err != nil {
		h.log.Error().Err(err).Msg("setup: create user failed")
		WriteError(w, http.StatusInternalServerError, "failed to create admin user")
		return
	}
	if user == nil {
		WriteError(w, http.StatusConflict, "setup already completed — users exist")
		return
	}

	h.log.Info().
		Int("user_id", user.ID).
		Str("username", user.Username).
		Msg("first admin user created via setup")

	WriteJSON(w, http.StatusCreated, map[string]any{
		"message": "admin account created",
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
	})
}
