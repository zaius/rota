package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// UserHandler handles proxy user management endpoints
type UserHandler struct {
	userRepo *repository.UserRepository
	poolRepo *repository.PoolRepository
	logger   *logger.Logger

	// onUserChanged, if set, is invoked with the affected username after a user
	// is updated or deleted so the proxy server can drop its cached auth entry.
	onUserChanged func(username string)
}

// NewUserHandler creates a new UserHandler
func NewUserHandler(
	userRepo *repository.UserRepository,
	poolRepo *repository.PoolRepository,
	log *logger.Logger,
) *UserHandler {
	return &UserHandler{userRepo: userRepo, poolRepo: poolRepo, logger: log}
}

// SetOnUserChanged registers a callback invoked after a user is updated or
// deleted, so cached proxy auth can be invalidated promptly.
func (h *UserHandler) SetOnUserChanged(fn func(username string)) {
	h.onUserChanged = fn
}

// notifyUserChanged invokes the change callback if one is registered.
func (h *UserHandler) notifyUserChanged(username string) {
	if h.onUserChanged != nil && username != "" {
		h.onUserChanged(username)
	}
}

// List returns all proxy users
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := h.userRepo.List(r.Context())
	if err != nil {
		h.logger.Error("list users failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

// Get returns a single user (no password)
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u, err := h.userRepo.GetByID(r.Context(), id)
	if err != nil || u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// Create adds a new proxy user
func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateProxyUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if req.MaxRetries <= 0 {
		req.MaxRetries = 5
	}

	u, err := h.userRepo.Create(r.Context(), req)
	if err != nil {
		h.logger.Error("create user failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create user: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// Update modifies an existing user
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req models.UpdateProxyUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := h.userRepo.Update(r.Context(), id, req)
	if err != nil || u == nil {
		writeError(w, http.StatusNotFound, "user not found or update failed")
		return
	}
	h.notifyUserChanged(u.Username)
	writeJSON(w, http.StatusOK, u)
}

// Delete removes a user
func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	// Resolve the username before deleting so the cached proxy auth entry can be
	// invalidated (the cache is keyed by username, not id).
	var username string
	if existing, err := h.userRepo.GetByID(r.Context(), id); err == nil && existing != nil {
		username = existing.Username
	}
	if err := h.userRepo.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	h.notifyUserChanged(username)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
