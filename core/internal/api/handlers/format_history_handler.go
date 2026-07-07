package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/alpkeskin/rota/core/internal/lineformat"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// FormatHistoryHandler serves the custom line formats the user has used
// before, so the dashboard's format picker can offer them again.
type FormatHistoryHandler struct {
	repo   *repository.FormatHistoryRepository
	logger *logger.Logger
}

// NewFormatHistoryHandler creates a new FormatHistoryHandler.
func NewFormatHistoryHandler(repo *repository.FormatHistoryRepository, log *logger.Logger) *FormatHistoryHandler {
	return &FormatHistoryHandler{repo: repo, logger: log}
}

// List returns recently used custom formats, newest first.
func (h *FormatHistoryHandler) List(w http.ResponseWriter, r *http.Request) {
	entries, err := h.repo.List(r.Context())
	if err != nil {
		h.logger.Error("failed to list format history", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list format history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"formats": entries})
}

// Record saves a format into history. Built-in presets are accepted but not
// stored — they are always offered anyway.
func (h *FormatHistoryHandler) Record(w http.ResponseWriter, r *http.Request) {
	var req models.RecordFormatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := lineformat.Validate(req.Format); err != nil {
		writeError(w, http.StatusBadRequest, "invalid format: "+err.Error())
		return
	}
	if !lineformat.IsPreset(req.Format) {
		if err := h.repo.Record(r.Context(), req.Format); err != nil {
			h.logger.Error("failed to record format history", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to record format")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Delete removes a format from history.
func (h *FormatHistoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.repo.Delete(r.Context(), id); err != nil {
		h.logger.Error("failed to delete format history entry", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete format")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
