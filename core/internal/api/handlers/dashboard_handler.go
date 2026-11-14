package handlers

import (
	"net/http"
	"strconv"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// DashboardHandler handles dashboard endpoints
type DashboardHandler struct {
	dashboardRepo *repository.DashboardRepository
	proxyRepo     *repository.ProxyRepository
	logger        *logger.Logger

	// proxyServer supplies the live open-tunnel count. Stored tunnel history
	// only covers tunnels that have closed, so without this the dashboard
	// cannot show traffic that is in flight right now. Set after
	// construction; nil until SetProxyServer runs.
	proxyServer *proxy.Server
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
func (h *DashboardHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.dashboardRepo.GetStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get dashboard stats", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get dashboard stats")
		return
	}

	if h.proxyServer != nil {
		stats.Tunnels.Open = h.proxyServer.OpenTunnels()
	}

	writeJSON(w, http.StatusOK, stats)
}

// SetProxyServer attaches the running proxy server, which supplies live
// counters the event store cannot (open tunnels).
func (h *DashboardHandler) SetProxyServer(ps *proxy.Server) { h.proxyServer = ps }

// trafficRanges is the set of ranges the traffic chart accepts.
var trafficRanges = map[string]bool{"1h": true, "6h": true, "24h": true, "7d": true, "30d": true}

// GetDomainStats returns traffic grouped by normalized target hostname.
func (h *DashboardHandler) GetDomainStats(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	if !trafficRanges[rng] {
		writeError(w, http.StatusBadRequest, "Invalid range (use 1h, 6h, 24h, 7d or 30d)")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
	}
	rawDomain := r.URL.Query().Get("domain")
	domain := proxy.NormalizeCooldownDomain(rawDomain)
	if rawDomain != "" && domain == "" {
		writeError(w, http.StatusBadRequest, "invalid domain")
		return
	}
	data, err := h.dashboardRepo.GetDomainStats(r.Context(), rng, domain, limit)
	if err != nil {
		h.logger.Error("failed to get domain stats", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get domain stats")
		return
	}
	writeJSON(w, http.StatusOK, models.DomainStatsData{Range: rng, Data: data})
}

// GetTrafficChart handles traffic series requests: request volume and latency
// percentiles in shared time buckets.
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
