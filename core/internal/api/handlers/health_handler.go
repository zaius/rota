package handlers

import (
	"net/http"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/version"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

var startTime = time.Now()

// HealthHandler handles health and status endpoints
type HealthHandler struct {
	db        *database.DB
	proxyRepo *repository.ProxyRepository
	logger    *logger.Logger
}

// NewHealthHandler creates a new HealthHandler
func NewHealthHandler(db *database.DB, proxyRepo *repository.ProxyRepository, log *logger.Logger) *HealthHandler {
	return &HealthHandler{
		db:        db,
		proxyRepo: proxyRepo,
		logger:    log,
	}
}

// Health handles basic health check
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":  "healthy",
		"version": version.Version,
		"uptime":  int(time.Since(startTime).Seconds()),
	}

	writeJSON(w, http.StatusOK, response)
}

// Status handles detailed status check
func (h *HealthHandler) Status(w http.ResponseWriter, r *http.Request) {
	// Get proxy stats
	proxyStats, err := h.proxyRepo.GetStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get proxy stats", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get proxy stats")
		return
	}

	// TODO: Get request stats from the event store
	// For now, using dummy data
	requestStats := map[string]interface{}{
		"total":       proxyStats["total_requests"],
		"last_minute": 0,
		"last_hour":   0,
	}

	// TODO: Get actual system stats
	systemStats := map[string]interface{}{
		"cpu_usage":    0.0,
		"memory_usage": 0,
		"memory_total": 0,
	}

	response := map[string]interface{}{
		"version": version.Version,
		"uptime":  int(time.Since(startTime).Seconds()),
		"proxies": map[string]interface{}{
			"total":  proxyStats["total"],
			"active": proxyStats["active"],
			"failed": proxyStats["failed"],
			"idle":   proxyStats["idle"],
		},
		"requests": requestStats,
		"system":   systemStats,
	}

	writeJSON(w, http.StatusOK, response)
}

// DatabaseHealth handles database health check
func (h *HealthHandler) DatabaseHealth(w http.ResponseWriter, r *http.Request) {
	health, err := h.db.Health(r.Context())
	if err != nil {
		h.logger.Error("database health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database health check failed")
		return
	}

	writeJSON(w, http.StatusOK, health)
}

// DatabaseStats handles database statistics
func (h *HealthHandler) DatabaseStats(w http.ResponseWriter, r *http.Request) {
	stats := h.db.Stats()

	response := map[string]interface{}{
		"total_conns":            stats.TotalConns(),
		"acquired_conns":         stats.AcquiredConns(),
		"idle_conns":             stats.IdleConns(),
		"max_conns":              stats.MaxConns(),
		"acquire_count":          stats.AcquireCount(),
		"acquire_duration":       stats.AcquireDuration().String(),
		"empty_acquire_count":    stats.EmptyAcquireCount(),
		"canceled_acquire_count": stats.CanceledAcquireCount(),
	}

	writeJSON(w, http.StatusOK, response)
}
