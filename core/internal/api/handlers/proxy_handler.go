package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/zaius/rota/core/internal/models"
	"github.com/zaius/rota/core/internal/proxy"
	"github.com/zaius/rota/core/internal/repository"
	"github.com/zaius/rota/core/internal/services"
	"github.com/zaius/rota/core/pkg/logger"
)

// maxBulkTest is a safety ceiling on how many proxies a single bulk-test job
// will load and check, guarding against pathological runs. Anything beyond this
// is reported back as skipped. It is high enough not to constrain normal use.
const maxBulkTest = 100000

// bulkTestTimeoutSecs is the per-proxy timeout used for interactive bulk tests.
// It is deliberately shorter than the background health-check timeout (60s) so
// a user watching the progress bar isn't stuck waiting on dead proxies that
// each burn the full timeout.
const bulkTestTimeoutSecs = 15

// ProxyHandler handles proxy management endpoints
type ProxyHandler struct {
	proxyRepo     *repository.ProxyRepository
	healthChecker *proxy.HealthChecker
	logger        *logger.Logger
}

// NewProxyHandler creates a new ProxyHandler
func NewProxyHandler(proxyRepo *repository.ProxyRepository, healthChecker *proxy.HealthChecker, log *logger.Logger) *ProxyHandler {
	return &ProxyHandler{
		proxyRepo:     proxyRepo,
		healthChecker: healthChecker,
		logger:        log,
	}
}

func queryFilter(r *http.Request) models.ProxyFilter {
	q := r.URL.Query()
	return models.ProxyFilter{
		Search:   q.Get("search"),
		Status:   q.Get("status"),
		Protocol: q.Get("protocol"),
		Source:   q.Get("source"),
	}
}

// List handles proxy listing with pagination and filters
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

	sortField := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")

	proxies, total, err := h.proxyRepo.List(r.Context(), page, limit, queryFilter(r), sortField, sortOrder)
	if err != nil {
		h.logger.Error("failed to list proxies", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to list proxies")
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

	writeJSON(w, http.StatusOK, response)
}

// Create handles proxy creation
func (h *ProxyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate request
	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "Address is required")
		return
	}

	if req.Protocol == "" {
		req.Protocol = "http"
	}

	proxy, err := h.proxyRepo.Create(r.Context(), req)
	if err != nil {
		h.logger.Error("failed to create proxy", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to create proxy")
		return
	}

	writeJSON(w, http.StatusCreated, proxy)
}

// Update handles proxy update
func (h *ProxyHandler) Update(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	var req models.UpdateProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Capture the pre-update endpoint: an address or protocol change moves the
	// proxy to a different cache key, so the old entry has to be dropped by its
	// old identity or it lingers with stale settings.
	before, err := h.proxyRepo.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to load proxy before update", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to update proxy")
		return
	}

	updated, err := h.proxyRepo.Update(r.Context(), id, req)
	if err != nil {
		h.logger.Error("failed to update proxy", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to update proxy")
		return
	}

	if updated == nil {
		writeError(w, http.StatusNotFound, "Proxy not found")
		return
	}

	if before != nil {
		proxy.InvalidateTransport(before)
	}
	proxy.InvalidateTransport(updated)

	writeJSON(w, http.StatusOK, updated)
}

// Delete handles proxy deletion
func (h *ProxyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	// Look the proxy up first so its cached transport can be dropped by endpoint
	// once the row is gone.
	deleted, err := h.proxyRepo.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to load proxy before delete", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete proxy")
		return
	}

	if err := h.proxyRepo.Delete(r.Context(), id); err != nil {
		h.logger.Error("failed to delete proxy", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete proxy")
		return
	}

	if deleted != nil {
		proxy.InvalidateTransport(deleted)
	}

	w.WriteHeader(http.StatusNoContent)
}

// BulkTag adds and removes tags on many proxies in one statement.
func (h *ProxyHandler) BulkTag(w http.ResponseWriter, r *http.Request) {
	var req models.BulkTagProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if !req.All && len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "At least one proxy ID is required")
		return
	}
	add, remove := normalizeTags(req.Add), normalizeTags(req.Remove)
	if len(add) == 0 && len(remove) == 0 {
		writeError(w, http.StatusBadRequest, "At least one tag to add or remove is required")
		return
	}

	var filter *models.ProxyFilter
	if req.All {
		filter = &models.ProxyFilter{}
		if req.Filter != nil {
			filter = req.Filter
		}
	}
	updated, err := h.proxyRepo.BulkUpdateTags(r.Context(), req.IDs, filter, add, remove)
	if err != nil {
		h.logger.Error("failed to bulk update proxy tags", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to update proxy tags")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"updated": updated,
		"message": fmt.Sprintf("Updated tags on %d proxies", updated),
	})
}

// normalizeTags trims tags and drops empty ones.
func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// BulkDelete handles bulk proxy deletion
func (h *ProxyHandler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	var req models.BulkDeleteProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !req.All && len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "At least one proxy ID is required")
		return
	}

	// Use a detached context so client disconnect during a long delete doesn't
	// cancel the DB operation mid-way.
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
		writeError(w, http.StatusInternalServerError, "Failed to delete proxies")
		return
	}

	// The deleted set isn't returned, so drop every cached transport rather than
	// track endpoints. Mutations are rare and the cache refills on demand.
	if deleted > 0 {
		proxy.ClearTransportCache()
	}

	response := map[string]interface{}{
		"deleted": deleted,
		"message": fmt.Sprintf("Successfully deleted %d proxies", deleted),
	}

	writeJSON(w, http.StatusOK, response)
}

// Test handles proxy testing
func (h *ProxyHandler) Test(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	// Get proxy
	proxy, err := h.proxyRepo.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to get proxy", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get proxy")
		return
	}

	if proxy == nil {
		writeError(w, http.StatusNotFound, "Proxy not found")
		return
	}

	// Perform actual proxy test
	h.logger.Info("testing proxy", "proxy_id", id, "address", proxy.Address)
	result, err := h.healthChecker.CheckProxy(r.Context(), proxy)
	if err != nil {
		h.logger.Error("failed to test proxy", "error", err, "proxy_id", id)
		writeError(w, http.StatusInternalServerError, "Failed to test proxy")
		return
	}

	h.logger.Info("proxy test completed",
		"proxy_id", id,
		"status", result.Status,
		"response_time", result.ResponseTime,
	)

	writeJSON(w, http.StatusOK, result)
}

// BulkTest starts an async job that tests multiple proxies and returns the job
// ID immediately. The dashboard polls GET /proxies/bulk-test for progress. The
// target is either an explicit list of IDs or every proxy matching a filter
// (all=true).
func (h *ProxyHandler) BulkTest(w http.ResponseWriter, r *http.Request) {
	var req models.BulkTestProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := validateStruct(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !req.All && len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "At least one proxy ID is required")
		return
	}

	filter := models.ProxyFilter{}
	if req.Filter != nil {
		filter = *req.Filter
	}

	// Count the target set upfront so the job can report a progress percentage.
	var (
		matched int
		err     error
	)
	if req.All {
		matched, err = h.proxyRepo.CountByFilter(r.Context(), filter)
		if err != nil {
			h.logger.Error("failed to count proxies for bulk test", "error", err)
			writeError(w, http.StatusInternalServerError, "Failed to count proxies")
			return
		}
	} else {
		matched = len(req.IDs)
	}

	tested := matched
	if tested > maxBulkTest {
		tested = maxBulkTest
	}

	store := services.GetJobStore()
	job := store.CreateBulkTest(tested)

	// Capture request data for the detached goroutine.
	all := req.All
	ids := req.IDs

	go func() {
		// Detached context so a client disconnect mid-run doesn't cancel testing.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		store.Update(job.ID, func(j *services.Job) { j.Status = services.JobRunning })

		var proxies []*models.Proxy
		var loadErr error
		if all {
			proxies, loadErr = h.proxyRepo.GetByFilter(ctx, filter, maxBulkTest)
		} else {
			proxies, loadErr = h.proxyRepo.GetByIDs(ctx, ids, maxBulkTest)
		}

		now := time.Now()
		if loadErr != nil {
			h.logger.Error("failed to load proxies for bulk test", "error", loadErr)
			store.Update(job.ID, func(j *services.Job) {
				j.Status = services.JobFailed
				j.Error = "failed to load proxies"
				j.FinishedAt = &now
			})
			return
		}

		h.logger.Info("bulk testing proxies", "count", len(proxies), "matched", matched)
		results, testErr := h.healthChecker.CheckProxies(ctx, proxies, bulkTestTimeoutSecs, func(checked, active, failed int) {
			store.Update(job.ID, func(j *services.Job) {
				j.Progress = checked
				j.Active = active
				j.Failed = failed
			})
		})

		finished := time.Now()
		if testErr != nil {
			h.logger.Error("failed to bulk test proxies", "error", testErr)
			store.Update(job.ID, func(j *services.Job) {
				j.Status = services.JobFailed
				j.Error = "failed to test proxies"
				j.FinishedAt = &finished
			})
			return
		}

		active, failed := 0, 0
		for _, result := range results {
			if result.Status == "active" {
				active++
			} else {
				failed++
			}
		}

		store.Update(job.ID, func(j *services.Job) {
			j.Status = services.JobDone
			j.Progress = len(results)
			j.Active = active
			j.Failed = failed
			if matched > len(results) {
				j.Skipped = matched - len(results)
			}
			j.Results = results
			j.FinishedAt = &finished
		})

		h.logger.Info("bulk test completed",
			"tested", len(results), "active", active, "failed", failed,
		)
	}()

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id": job.ID,
		"total":  tested,
		"status": job.Status,
	})
}

// BulkTestLatest returns the most recent bulk-test job (or {"job": null} if
// none is known). The UI calls this on load so an in-flight test started before
// a page reload can be re-attached to and its progress resumed.
func (h *ProxyHandler) BulkTestLatest(w http.ResponseWriter, r *http.Request) {
	job, ok := services.GetJobStore().Latest()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"job": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"job": job})
}

// Tags lists the distinct proxy tags.
func (h *ProxyHandler) Tags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.proxyRepo.ListTags(r.Context())
	if err != nil {
		h.logger.Error("failed to list tags", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list tags")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"tags": tags})
}

// Export handles proxy export
func (h *ProxyHandler) Export(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "txt"
	}

	proxies, _, err := h.proxyRepo.List(r.Context(), 1, 10000, queryFilter(r), "created_at", "asc")
	if err != nil {
		h.logger.Error("failed to get proxies for export", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to export proxies")
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
		writeError(w, http.StatusBadRequest, "Invalid format")
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
	dbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	deleted, err := h.proxyRepo.DeleteAll(dbCtx)
	if err != nil {
		h.logger.Error("failed to delete all proxies", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete all proxies")
		return
	}
	proxy.ClearTransportCache()
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": deleted})
}
