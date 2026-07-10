package handlers

import (
	"net/http"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// DashboardHandler handles dashboard endpoints
type DashboardHandler struct {
	dashboardRepo *repository.DashboardRepository
	proxyRepo     *repository.ProxyRepository
	logger        *logger.Logger
}

// NewDashboardHandler creates a new DashboardHandler
func NewDashboardHandler(dashboardRepo *repository.DashboardRepository, proxyRepo *repository.ProxyRepository, log *logger.Logger) *DashboardHandler {
	return &DashboardHandler{
		dashboardRepo: dashboardRepo,
		proxyRepo:     proxyRepo,
		logger:        log,
	}
}

// GetStats handles dashboard statistics requests
//
//	@Summary		Dashboard statistics
//	@Description	Get dashboard statistics including proxy and request metrics
//	@Tags			dashboard
//	@Produce		json
//	@Success		200	{object}	models.DashboardStats	"Dashboard statistics"
//	@Failure		500	{object}	models.ErrorResponse
//	@Router			/dashboard/stats [get]
func (h *DashboardHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.dashboardRepo.GetStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get dashboard stats", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get dashboard stats")
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// trafficRanges is the set of ranges the traffic chart accepts.
var trafficRanges = map[string]bool{"1h": true, "6h": true, "24h": true, "7d": true, "30d": true}

// GetTrafficChart handles traffic series requests: request volume and latency
// percentiles in shared time buckets.
//
//	@Summary		Traffic chart
//	@Description	Get request volume and latency percentiles bucketed over a trailing range
//	@Tags			dashboard
//	@Produce		json
//	@Param			range	query		string					false	"Trailing range (1h, 6h, 24h, 7d, 30d)"	default(24h)
//	@Success		200		{object}	models.TrafficChartData	"Traffic series"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/dashboard/charts/traffic [get]
func (h *DashboardHandler) GetTrafficChart(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	if !trafficRanges[rng] {
		writeError(w, http.StatusBadRequest, "Invalid range (use 1h, 6h, 24h, 7d or 30d)")
		return
	}

	data, err := h.dashboardRepo.GetTrafficChart(r.Context(), rng)
	if err != nil {
		h.logger.Error("failed to get traffic chart", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get traffic chart")
		return
	}

	bucket, _ := events.SeriesWindow(rng)
	writeJSON(w, http.StatusOK, models.TrafficChartData{
		Range:         rng,
		BucketSeconds: int(bucket.Seconds()),
		Data:          data,
	})
}

// GetResponseTimeChart handles response time chart requests
//
//	@Summary		Response time chart
//	@Description	Get response time chart data for visualization
//	@Tags			dashboard
//	@Produce		json
//	@Param			interval	query		string							false	"Time interval (e.g., 4h, 24h)"	default(4h)
//	@Success		200			{object}	models.ResponseTimeChartData	"Chart data"
//	@Failure		500			{object}	models.ErrorResponse
//	@Router			/dashboard/charts/response-time [get]
func (h *DashboardHandler) GetResponseTimeChart(w http.ResponseWriter, r *http.Request) {
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "4h"
	}

	data, err := h.dashboardRepo.GetResponseTimeChart(r.Context(), interval)
	if err != nil {
		h.logger.Error("failed to get response time chart", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get response time chart")
		return
	}

	response := models.ResponseTimeChartData{
		Data: data,
	}

	writeJSON(w, http.StatusOK, response)
}

// GetSuccessRateChart handles success rate chart requests
//
//	@Summary		Success rate chart
//	@Description	Get success rate chart data for visualization
//	@Tags			dashboard
//	@Produce		json
//	@Param			interval	query		string							false	"Time interval (e.g., 4h, 24h)"	default(4h)
//	@Success		200			{object}	models.SuccessRateChartData		"Chart data"
//	@Failure		500			{object}	models.ErrorResponse
//	@Router			/dashboard/charts/success-rate [get]
func (h *DashboardHandler) GetSuccessRateChart(w http.ResponseWriter, r *http.Request) {
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "4h"
	}

	data, err := h.dashboardRepo.GetSuccessRateChart(r.Context(), interval)
	if err != nil {
		h.logger.Error("failed to get success rate chart", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get success rate chart")
		return
	}

	response := models.SuccessRateChartData{
		Data: data,
	}

	writeJSON(w, http.StatusOK, response)
}
