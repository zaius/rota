package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// maxBulkTest caps how many proxies a single bulk-test request will check, to
// keep the (synchronous) request bounded. Larger selections are truncated and
// the skipped count is reported back to the caller.
const maxBulkTest = 1000

// HealthChecker interface for testing proxies
type HealthChecker interface {
	CheckProxy(ctx context.Context, proxy *models.Proxy) (*models.ProxyTestResult, error)
	CheckProxies(ctx context.Context, proxies []*models.Proxy) ([]models.ProxyTestResult, error)
}

// ProxyHandler handles proxy management endpoints
type ProxyHandler struct {
	proxyRepo     *repository.ProxyRepository
	healthChecker HealthChecker
	logger        *logger.Logger
}

// NewProxyHandler creates a new ProxyHandler
func NewProxyHandler(proxyRepo *repository.ProxyRepository, healthChecker HealthChecker, log *logger.Logger) *ProxyHandler {
	return &ProxyHandler{
		proxyRepo:     proxyRepo,
		healthChecker: healthChecker,
		logger:        log,
	}
}

// List handles proxy listing with pagination and filters
//	@Summary		List proxies
//	@Description	Get paginated list of proxies with optional filters
//	@Tags			proxies
//	@Produce		json
//	@Param			page		query		int							false	"Page number"			default(1)
//	@Param			limit		query		int							false	"Items per page"		default(10)
//	@Param			search		query		string						false	"Search term"
//	@Param			status		query		string						false	"Filter by status"
//	@Param			protocol	query		string						false	"Filter by protocol"
//	@Param			sort		query		string						false	"Sort field"
//	@Param			order		query		string						false	"Sort order (asc/desc)"
//	@Success		200			{object}	models.ProxyListResponse	"List of proxies"
//	@Failure		500			{object}	models.ErrorResponse
//	@Router			/proxies [get]
func (h *ProxyHandler) List(w http.ResponseWriter, r *http.Request) {
	// Parse query parameters
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 10
	}

	search := r.URL.Query().Get("search")
	status := r.URL.Query().Get("status")
	protocol := r.URL.Query().Get("protocol")
	sortField := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")

	// Get proxies
	proxies, total, err := h.proxyRepo.List(r.Context(), page, limit, search, status, protocol, sortField, sortOrder)
	if err != nil {
		h.logger.Error("failed to list proxies", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to list proxies")
		return
	}

	// Calculate pagination
	totalPages := int(math.Ceil(float64(total) / float64(limit)))

	response := models.ProxyListResponse{
		Proxies: proxies,
		Pagination: models.PaginationMeta{
			Page:       page,
			Limit:      limit,
			Total:      total,
			TotalPages: totalPages,
		},
	}

	h.jsonResponse(w, http.StatusOK, response)
}

// Create handles proxy creation
//	@Summary		Create proxy
//	@Description	Create a new proxy server
//	@Tags			proxies
//	@Accept			json
//	@Produce		json
//	@Param			request	body		models.CreateProxyRequest	true	"Proxy details"
//	@Success		201		{object}	models.Proxy				"Created proxy"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/proxies [post]
func (h *ProxyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validate request
	if req.Address == "" {
		h.errorResponse(w, http.StatusBadRequest, "Address is required")
		return
	}

	if req.Protocol == "" {
		req.Protocol = "http"
	}

	proxy, err := h.proxyRepo.Create(r.Context(), req)
	if err != nil {
		h.logger.Error("failed to create proxy", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to create proxy")
		return
	}

	h.jsonResponse(w, http.StatusCreated, proxy)
}

// BulkCreate handles bulk proxy creation
//	@Summary		Bulk create proxies
//	@Description	Create multiple proxy servers at once
//	@Tags			proxies
//	@Accept			json
//	@Produce		json
//	@Param			request	body		models.BulkCreateProxyRequest	true	"List of proxies to create"
//	@Success		201		{object}	map[string]interface{}			"Creation results"
//	@Failure		400		{object}	models.ErrorResponse
//	@Router			/proxies/bulk [post]
func (h *ProxyHandler) BulkCreate(w http.ResponseWriter, r *http.Request) {
	var req models.BulkCreateProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if len(req.Proxies) == 0 {
		h.errorResponse(w, http.StatusBadRequest, "At least one proxy is required")
		return
	}

	created := 0
	failed := 0
	results := []map[string]interface{}{}

	for _, proxyReq := range req.Proxies {
		proxy, err := h.proxyRepo.Create(r.Context(), proxyReq)
		if err != nil {
			failed++
			results = append(results, map[string]interface{}{
				"address": proxyReq.Address,
				"status":  "failed",
				"error":   err.Error(),
			})
		} else {
			created++
			results = append(results, map[string]interface{}{
				"address": proxyReq.Address,
				"status":  "success",
				"id":      fmt.Sprintf("%d", proxy.ID),
			})
		}
	}

	response := map[string]interface{}{
		"created": created,
		"failed":  failed,
		"results": results,
	}

	h.jsonResponse(w, http.StatusCreated, response)
}

// Update handles proxy update
//	@Summary		Update proxy
//	@Description	Update an existing proxy server
//	@Tags			proxies
//	@Accept			json
//	@Produce		json
//	@Param			id		path		int							true	"Proxy ID"
//	@Param			request	body		models.UpdateProxyRequest	true	"Updated proxy details"
//	@Success		200		{object}	models.Proxy				"Updated proxy"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		404		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/proxies/{id} [put]
func (h *ProxyHandler) Update(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	var req models.UpdateProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	proxy, err := h.proxyRepo.Update(r.Context(), id, req)
	if err != nil {
		h.logger.Error("failed to update proxy", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to update proxy")
		return
	}

	if proxy == nil {
		h.errorResponse(w, http.StatusNotFound, "Proxy not found")
		return
	}

	h.jsonResponse(w, http.StatusOK, proxy)
}

// Delete handles proxy deletion
//	@Summary		Delete proxy
//	@Description	Delete a proxy server by ID
//	@Tags			proxies
//	@Param			id	path	int	true	"Proxy ID"
//	@Success		204	"Successfully deleted"
//	@Failure		400	{object}	models.ErrorResponse
//	@Failure		500	{object}	models.ErrorResponse
//	@Router			/proxies/{id} [delete]
func (h *ProxyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	if err := h.proxyRepo.Delete(r.Context(), id); err != nil {
		h.logger.Error("failed to delete proxy", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to delete proxy")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// BulkDelete handles bulk proxy deletion
//	@Summary		Bulk delete proxies
//	@Description	Delete multiple proxy servers at once
//	@Tags			proxies
//	@Accept			json
//	@Produce		json
//	@Param			request	body		models.BulkDeleteProxyRequest	true	"List of proxy IDs to delete"
//	@Success		200		{object}	map[string]interface{}			"Deletion results"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/proxies/bulk-delete [post]
func (h *ProxyHandler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	var req models.BulkDeleteProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if !req.All && len(req.IDs) == 0 {
		h.errorResponse(w, http.StatusBadRequest, "At least one proxy ID is required")
		return
	}

	// Use a detached context so client disconnect during a long delete doesn't
	// cancel the DB operation mid-way. Large bulk deletes can take tens of
	// seconds against TimescaleDB with CASCADE on proxy_requests.
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		deleted int
		err     error
	)
	if req.All {
		// Delete every proxy matching the supplied filter (empty filter = all).
		filter := models.ProxyFilter{}
		if req.Filter != nil {
			filter = *req.Filter
		}
		deleted, err = h.proxyRepo.BulkDeleteByFilter(dbCtx, filter)
	} else {
		deleted, err = h.proxyRepo.BulkDelete(dbCtx, req.IDs)
	}
	if err != nil {
		h.logger.Error("failed to bulk delete proxies", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to delete proxies")
		return
	}

	response := map[string]interface{}{
		"deleted": deleted,
		"message": fmt.Sprintf("Successfully deleted %d proxies", deleted),
	}

	h.jsonResponse(w, http.StatusOK, response)
}

// Test handles proxy testing
//	@Summary		Test proxy
//	@Description	Test a proxy server's connectivity and performance
//	@Tags			proxies
//	@Produce		json
//	@Param			id	path		int							true	"Proxy ID"
//	@Success		200	{object}	models.ProxyTestResult		"Test results"
//	@Failure		400	{object}	models.ErrorResponse
//	@Failure		404	{object}	models.ErrorResponse
//	@Failure		500	{object}	models.ErrorResponse
//	@Router			/proxies/{id}/test [post]
func (h *ProxyHandler) Test(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	// Get proxy
	proxy, err := h.proxyRepo.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to get proxy", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to get proxy")
		return
	}

	if proxy == nil {
		h.errorResponse(w, http.StatusNotFound, "Proxy not found")
		return
	}

	// Perform actual proxy test
	h.logger.Info("testing proxy", "proxy_id", id, "address", proxy.Address)
	result, err := h.healthChecker.CheckProxy(r.Context(), proxy)
	if err != nil {
		h.logger.Error("failed to test proxy", "error", err, "proxy_id", id)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to test proxy")
		return
	}

	h.logger.Info("proxy test completed",
		"proxy_id", id,
		"status", result.Status,
		"response_time", result.ResponseTime,
	)

	h.jsonResponse(w, http.StatusOK, result)
}

// BulkTest handles testing multiple proxies at once
//	@Summary		Bulk test proxies
//	@Description	Test multiple proxies at once, either an explicit list of IDs or every proxy matching a filter (all=true). Capped at 1000 proxies per request.
//	@Tags			proxies
//	@Accept			json
//	@Produce		json
//	@Param			request	body		models.BulkTestProxyRequest	true	"Proxies to test"
//	@Success		200		{object}	models.BulkTestResult		"Test summary"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/proxies/bulk-test [post]
func (h *ProxyHandler) BulkTest(w http.ResponseWriter, r *http.Request) {
	var req models.BulkTestProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errorResponse(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if !req.All && len(req.IDs) == 0 {
		h.errorResponse(w, http.StatusBadRequest, "At least one proxy ID is required")
		return
	}

	// Detached context so a client disconnect mid-run doesn't cancel testing.
	dbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Resolve the target proxies. We over-fetch by one beyond the cap so we can
	// tell the caller how many were skipped due to truncation.
	var (
		matched int
		proxies []*models.Proxy
		err     error
	)
	if req.All {
		filter := models.ProxyFilter{}
		if req.Filter != nil {
			filter = *req.Filter
		}
		if matched, err = h.proxyRepo.CountByFilter(dbCtx, filter); err == nil {
			proxies, err = h.proxyRepo.GetByFilter(dbCtx, filter, maxBulkTest)
		}
	} else {
		matched = len(req.IDs)
		proxies, err = h.proxyRepo.GetByIDs(dbCtx, req.IDs, maxBulkTest)
	}
	if err != nil {
		h.logger.Error("failed to load proxies for bulk test", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to load proxies")
		return
	}

	h.logger.Info("bulk testing proxies", "count", len(proxies), "matched", matched)
	results, err := h.healthChecker.CheckProxies(dbCtx, proxies)
	if err != nil {
		h.logger.Error("failed to bulk test proxies", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to test proxies")
		return
	}

	summary := models.BulkTestResult{Tested: len(results)}
	for _, result := range results {
		if result.Status == "active" {
			summary.Active++
		} else {
			summary.Failed++
		}
	}
	if matched > summary.Tested {
		summary.Skipped = matched - summary.Tested
	}

	h.logger.Info("bulk test completed",
		"tested", summary.Tested, "active", summary.Active,
		"failed", summary.Failed, "skipped", summary.Skipped,
	)

	h.jsonResponse(w, http.StatusOK, summary)
}

// Export handles proxy export
//	@Summary		Export proxies
//	@Description	Export proxy list in various formats (txt, json, csv)
//	@Tags			proxies
//	@Produce		plain
//	@Produce		json
//	@Produce		text/csv
//	@Param			format		query	string	false	"Export format (txt/json/csv)"	default(txt)
//	@Param			status		query	string	false	"Filter by status"
//	@Param			search		query	string	false	"Filter by search term"
//	@Param			protocol	query	string	false	"Filter by protocol"
//	@Success		200		{file}	file	"Exported file"
//	@Failure		400		{object}	models.ErrorResponse
//	@Failure		500		{object}	models.ErrorResponse
//	@Router			/proxies/export [get]
func (h *ProxyHandler) Export(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "txt"
	}

	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")
	protocol := r.URL.Query().Get("protocol")

	// Get all proxies matching the supplied filters
	proxies, _, err := h.proxyRepo.List(r.Context(), 1, 10000, search, status, protocol, "created_at", "asc")
	if err != nil {
		h.logger.Error("failed to get proxies for export", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to export proxies")
		return
	}

	switch format {
	case "txt":
		h.exportTxt(w, proxies)
	case "json":
		h.exportJSON(w, proxies)
	case "csv":
		h.exportCSV(w, proxies)
	default:
		h.errorResponse(w, http.StatusBadRequest, "Invalid format")
	}
}

func (h *ProxyHandler) exportTxt(w http.ResponseWriter, proxies []models.ProxyWithStats) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", "attachment; filename=\"proxies.txt\"")

	for _, p := range proxies {
		line := p.Address
		if p.Username != nil && *p.Username != "" {
			// Format: address:username:password
			line = fmt.Sprintf("%s:%s", line, *p.Username)
		}
		fmt.Fprintln(w, line)
	}
}

func (h *ProxyHandler) exportJSON(w http.ResponseWriter, proxies []models.ProxyWithStats) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"proxies.json\"")
	json.NewEncoder(w).Encode(proxies)
}

func (h *ProxyHandler) exportCSV(w http.ResponseWriter, proxies []models.ProxyWithStats) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=\"proxies.csv\"")

	fmt.Fprintln(w, "Address,Protocol,Status,Requests,SuccessRate,AvgResponseTime")
	for _, p := range proxies {
		fmt.Fprintf(w, "%s,%s,%s,%d,%.2f,%d\n",
			p.Address, p.Protocol, p.Status, p.Requests, p.SuccessRate, p.AvgResponseTime)
	}
}

// DeleteAll removes every proxy from the database.
func (h *ProxyHandler) DeleteAll(w http.ResponseWriter, r *http.Request) {
	// Use a detached context so client disconnect doesn't cancel the delete.
	// Deleting all proxies can take minutes when there are many rows in
	// proxy_requests (CASCADE delete on foreign key).
	dbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	deleted, err := h.proxyRepo.DeleteAll(dbCtx)
	if err != nil {
		h.logger.Error("failed to delete all proxies", "error", err)
		h.errorResponse(w, http.StatusInternalServerError, "Failed to delete all proxies")
		return
	}
	h.jsonResponse(w, http.StatusOK, map[string]interface{}{"deleted": deleted})
}

// jsonResponse sends a JSON response
func (h *ProxyHandler) jsonResponse(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

// errorResponse sends an error JSON response
func (h *ProxyHandler) errorResponse(w http.ResponseWriter, statusCode int, message string) {
	response := models.ErrorResponse{
		Error: message,
	}
	h.jsonResponse(w, statusCode, response)
}
