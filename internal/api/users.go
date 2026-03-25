package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// UsersHandler manages user CRUD operations (admin only).
type UsersHandler struct {
	db  *database.DB
	log zerolog.Logger
}

func NewUsersHandler(db *database.DB, log zerolog.Logger) *UsersHandler {
	return &UsersHandler{db: db, log: log}
}

// Routes registers user management routes. All require admin role.
func (h *UsersHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Patch("/{id}", h.Update)
	r.Delete("/all", h.DeleteAll) // bulk delete (downgrade path) — must precede /{id}
	r.Delete("/{id}", h.Delete)
}

// List returns all users.
func (h *UsersHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("users: list failed")
		WriteError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	if users == nil {
		users = []database.User{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"users": users,
		"total": len(users),
	})
}

// Create adds a new user.
func (h *UsersHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Username = database.NormalizeUsername(req.Username)
	if req.Username == "" || req.Password == "" {
		WriteError(w, http.StatusBadRequest, "username and password required")
		return
	}
	if len(req.Password) < 8 {
		WriteError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}
	if req.Role != "viewer" && req.Role != "editor" && req.Role != "admin" {
		WriteError(w, http.StatusBadRequest, "role must be viewer, editor, or admin")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error().Err(err).Msg("users: bcrypt failed")
		WriteError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	user, err := h.db.CreateUser(r.Context(), req.Username, string(hash), req.Role)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			WriteErrorWithCode(w, http.StatusConflict, ErrDuplicate, "username already exists")
			return
		}
		h.log.Error().Err(err).Msg("users: create failed")
		WriteError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	WriteJSON(w, http.StatusCreated, user)
}

// Update modifies an existing user (role, password, enabled).
func (h *UsersHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Prevent self-disable
	callerID := ContextUserID(r)
	var req struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Enabled  *bool   `json:"enabled"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if callerID == id {
		if req.Enabled != nil && !*req.Enabled {
			WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "cannot disable your own account")
			return
		}
		if req.Role != nil && *req.Role != "admin" {
			WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "cannot demote your own account")
			return
		}
	}

	if req.Role != nil {
		if *req.Role != "viewer" && *req.Role != "editor" && *req.Role != "admin" {
			WriteError(w, http.StatusBadRequest, "role must be viewer, editor, or admin")
			return
		}
		// Last-admin protection: if demoting an admin, check they're not the last one
		if *req.Role != "admin" {
			target, err := h.db.GetUserByID(r.Context(), id)
			if err != nil || target == nil {
				WriteError(w, http.StatusNotFound, "user not found")
				return
			}
			if target.Role == "admin" {
				adminCount, err := h.db.CountAdmins(r.Context())
				if err != nil {
					h.log.Error().Err(err).Msg("users: count admins failed")
					WriteError(w, http.StatusInternalServerError, "internal error")
					return
				}
				if adminCount <= 1 {
					WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "cannot demote the last admin user")
					return
				}
			}
		}
	}

	upd := database.UserUpdate{
		Role:    req.Role,
		Enabled: req.Enabled,
	}

	if req.Password != nil && *req.Password != "" {
		if len(*req.Password) < 8 {
			WriteError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			h.log.Error().Err(err).Msg("users: bcrypt failed")
			WriteError(w, http.StatusInternalServerError, "failed to hash password")
			return
		}
		hashStr := string(hash)
		upd.PasswordHash = &hashStr
	}

	user, err := h.db.UpdateUser(r.Context(), id, upd)
	if err != nil {
		h.log.Error().Err(err).Msg("users: update failed")
		WriteError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	if user == nil {
		WriteError(w, http.StatusNotFound, "user not found")
		return
	}

	// Audit log for role/enabled changes
	if req.Role != nil {
		h.log.Info().
			Int("target_user_id", id).
			Str("target_username", user.Username).
			Str("new_role", *req.Role).
			Int("changed_by", callerID).
			Msg("user role changed")
	}
	if req.Enabled != nil {
		h.log.Info().
			Int("target_user_id", id).
			Str("target_username", user.Username).
			Bool("enabled", *req.Enabled).
			Int("changed_by", callerID).
			Msg("user enabled status changed")
	}

	WriteJSON(w, http.StatusOK, user)
}

// Delete removes a user.
func (h *UsersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Prevent self-delete
	if ContextUserID(r) == id {
		WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "cannot delete your own account")
		return
	}

	// Last-admin protection: check if target is an admin and if they're the last one
	target, err := h.db.GetUserByID(r.Context(), id)
	if err != nil || target == nil {
		WriteError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.Role == "admin" {
		adminCount, err := h.db.CountAdmins(r.Context())
		if err != nil {
			h.log.Error().Err(err).Msg("users: count admins failed")
			WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if adminCount <= 1 {
			WriteErrorWithCode(w, http.StatusForbidden, ErrForbidden, "cannot delete the last admin user")
			return
		}
	}

	if err := h.db.DeleteUser(r.Context(), id); err != nil {
		h.log.Error().Err(err).Msg("users: delete failed")
		WriteError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}

	h.log.Info().
		Int("deleted_user_id", id).
		Str("deleted_username", target.Username).
		Int("deleted_by", ContextUserID(r)).
		Msg("user deleted")

	WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// DeleteAll removes all users and their API keys (cascade). Used for the
// downgrade-to-token-auth path. Bypasses self-delete and last-admin protections.
func (h *UsersHandler) DeleteAll(w http.ResponseWriter, r *http.Request) {
	initiatedBy := ContextUserID(r)
	if err := h.db.DeleteAllUsers(r.Context()); err != nil {
		h.log.Error().Err(err).Msg("users: delete all failed")
		WriteError(w, http.StatusInternalServerError, "failed to delete all users")
		return
	}
	h.log.Warn().Int("initiated_by", initiatedBy).Msg("all users deleted — downgrading to token auth")
	WriteJSON(w, http.StatusOK, map[string]string{"status": "all users deleted"})
}
