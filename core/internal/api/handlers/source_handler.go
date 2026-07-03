package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/services"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// SourceHandler handles proxy source CRUD + manual fetch
type SourceHandler struct {
	sourceRepo *repository.SourceRepository
	sourceSvc  *services.SourceService
	logger     *logger.Logger
}

// NewSourceHandler creates a new SourceHandler
func NewSourceHandler(
	sourceRepo *repository.SourceRepository,
	sourceSvc *services.SourceService,
	log *logger.Logger,
) *SourceHandler {
	return &SourceHandler{
		sourceRepo: sourceRepo,
		sourceSvc:  sourceSvc,
		logger:     log,
	}
}

// List returns all proxy sources
func (h *SourceHandler) List(w http.ResponseWriter, r *http.Request) {
	sources, err := h.sourceRepo.List(r.Context())
	if err != nil {
		h.logger.Error("failed to list sources", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sources": sources})
}

// Create adds a new proxy source
func (h *SourceHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateProxySourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" || req.URL == "" || req.Protocol == "" {
		writeError(w, http.StatusBadRequest, "name, url and protocol are required")
		return
	}
	if req.Format == "" {
		req.Format = models.SourceFormatAuto
	}
	if !models.ValidSourceFormats[req.Format] {
		writeError(w, http.StatusBadRequest, "invalid format")
		return
	}
	if req.IntervalMinutes <= 0 {
		req.IntervalMinutes = 60
	}
	// cleanup_days bounds
	if req.CleanupDays < 0 {
		req.CleanupDays = 0
	}
	if req.CleanupDays > 365 {
		req.CleanupDays = 365
	}
	if req.CleanupEnabled && req.CleanupDays == 0 {
		req.CleanupDays = 7 // sensible default
	}

	src, err := h.sourceRepo.Create(r.Context(), req)
	if err != nil {
		h.logger.Error("failed to create source", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create source")
		return
	}
	writeJSON(w, http.StatusCreated, src)
}

// Update modifies an existing source
func (h *SourceHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req models.UpdateProxySourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Format != "" && !models.ValidSourceFormats[req.Format] {
		writeError(w, http.StatusBadRequest, "invalid format")
		return
	}
	// cleanup_days bounds
	if req.CleanupDays < 0 {
		req.CleanupDays = 0
	}
	if req.CleanupDays > 365 {
		req.CleanupDays = 365
	}
	src, err := h.sourceRepo.Update(r.Context(), id, req)
	if err != nil || src == nil {
		writeError(w, http.StatusNotFound, "source not found or update failed")
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// Delete removes a source
func (h *SourceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.sourceRepo.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete source")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// FetchNow triggers an immediate fetch for a given source
func (h *SourceHandler) FetchNow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	src, count, err := h.sourceSvc.FetchNow(r.Context(), id)
	if err != nil {
		h.logger.Error("fetch now failed", "source_id", id, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"source":   src,
		"imported": count,
	})
}

// EnrichGeo triggers geo enrichment for all ungeotagged proxies
func (h *SourceHandler) EnrichGeo(w http.ResponseWriter, r *http.Request) {
	count, err := h.sourceSvc.EnrichAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enriched": count})
}
