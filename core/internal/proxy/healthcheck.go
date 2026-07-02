package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/gammazero/workerpool"
)

// HealthChecker manages proxy health checking
type HealthChecker struct {
	proxyRepo    *repository.ProxyRepository
	settingsRepo *repository.SettingsRepository
	tracker      *UsageTracker
	logger       *logger.Logger
	settings     *models.HealthCheckSettings
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(
	proxyRepo *repository.ProxyRepository,
	settingsRepo *repository.SettingsRepository,
	tracker *UsageTracker,
	log *logger.Logger,
) *HealthChecker {
	return &HealthChecker{
		proxyRepo:    proxyRepo,
		settingsRepo: settingsRepo,
		tracker:      tracker,
		logger:       log,
	}
}

// CheckProxy tests a single proxy using the configured health-check timeout.
func (h *HealthChecker) CheckProxy(ctx context.Context, proxy *models.Proxy) (*models.ProxyTestResult, error) {
	return h.checkProxy(ctx, proxy, 0)
}

// checkProxy tests a single proxy. If timeoutSecs > 0 it overrides the
// configured health-check timeout for this check only (used by interactive
// bulk tests that want a snappier per-proxy timeout than the background cron).
func (h *HealthChecker) checkProxy(ctx context.Context, proxy *models.Proxy, timeoutSecs int) (*models.ProxyTestResult, error) {
	startTime := time.Now()

	// Load settings if not cached
	if h.settings == nil {
		settings, err := h.settingsRepo.GetAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load settings: %w", err)
		}
		h.settings = &settings.HealthCheck
	}

	timeout := h.settings.Timeout
	if timeoutSecs > 0 {
		timeout = timeoutSecs
	}

	result := &models.ProxyTestResult{
		ID:       proxy.ID,
		Address:  proxy.Address,
		TestedAt: startTime,
	}

	// Create HTTP client with proxy
	transport, err := h.createTransport(proxy)
	if err != nil {
		result.Status = "failed"
		errMsg := fmt.Sprintf("failed to create transport: %v", err)
		result.Error = &errMsg
		return result, nil
	}

	// Override TLS config for health checks to be maximally permissive
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.TLSClientConfig.MinVersion = 0     // Allow all TLS versions including SSLv3
	transport.TLSClientConfig.MaxVersion = 0     // No maximum version restriction
	transport.TLSClientConfig.CipherSuites = nil // Accept all cipher suites
	// This callback allows us to accept even unparseable certificates
	transport.TLSClientConfig.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// Always return nil to accept any certificate, even malformed ones
		return nil
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(timeout) * time.Second,
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "GET", h.settings.URL, nil)
	if err != nil {
		result.Status = "failed"
		errMsg := fmt.Sprintf("failed to create request: %v", err)
		result.Error = &errMsg
		return result, nil
	}

	// Add custom headers
	for _, header := range h.settings.Headers {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			req.Header.Set(key, value)
		}
	}

	// Send request
	resp, err := client.Do(req)
	duration := int(time.Since(startTime).Milliseconds())

	if err != nil {
		result.Status = "failed"
		errMsg := err.Error()

		// Make TLS errors more user-friendly
		if strings.Contains(errMsg, "x509:") || strings.Contains(errMsg, "tls:") {
			errMsg = fmt.Sprintf("TLS/SSL error: %s (Note: Certificate verification is disabled, but proxy may have issues)", err.Error())
		} else if strings.Contains(errMsg, "timeout") {
			errMsg = fmt.Sprintf("Connection timeout after %ds", timeout)
		} else if strings.Contains(errMsg, "connection refused") {
			errMsg = "Connection refused - proxy may be offline"
		}

		result.Error = &errMsg

		// Record health check failure
		go func() {
			recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h.tracker.RecordHealthCheck(recordCtx, proxy.ID, false, duration, errMsg)
		}()

		return result, nil
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != h.settings.Status {
		result.Status = "failed"
		errMsg := fmt.Sprintf("unexpected status code: got %d, expected %d", resp.StatusCode, h.settings.Status)
		result.Error = &errMsg

		// Record health check failure
		go func() {
			recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h.tracker.RecordHealthCheck(recordCtx, proxy.ID, false, duration, errMsg)
		}()

		return result, nil
	}

	// Success!
	result.Status = "active"
	result.ResponseTime = &duration

	// Record health check success
	go func() {
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.tracker.RecordHealthCheck(recordCtx, proxy.ID, true, duration, "")
	}()

	return result, nil
}

// CheckAllProxies tests all proxies concurrently
func (h *HealthChecker) CheckAllProxies(ctx context.Context) ([]models.ProxyTestResult, error) {
	// Load settings
	settings, err := h.settingsRepo.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}
	h.settings = &settings.HealthCheck

	// Get all proxies (including failed ones for re-testing)
	query := `
		SELECT
			id, address, protocol, username, password, status,
			requests, successful_requests, failed_requests,
			avg_response_time, last_check, last_error, created_at, updated_at
		FROM proxies
		ORDER BY address
	`

	rows, err := h.proxyRepo.GetDB().Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get proxies: %w", err)
	}
	defer rows.Close()

	proxies := make([]*models.Proxy, 0)
	for rows.Next() {
		var p models.Proxy
		err := rows.Scan(
			&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Password, &p.Status,
			&p.Requests, &p.SuccessfulRequests, &p.FailedRequests,
			&p.AvgResponseTime, &p.LastCheck, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan proxy: %w", err)
		}
		proxies = append(proxies, &p)
	}

	return h.CheckProxies(ctx, proxies, 0, nil)
}

// CheckProxies tests the provided proxies concurrently using the configured
// worker pool and returns one result per proxy (in the same order). It loads
// health-check settings if they have not been cached yet, so it is safe to call
// without a prior CheckAllProxies. If timeoutSecs > 0 it overrides the
// per-proxy health-check timeout for this run only. If progressFn is non-nil it
// is invoked after each proxy finishes with the running (checked, active,
// failed) counts; the values passed are computed under a lock so they are safe.
func (h *HealthChecker) CheckProxies(ctx context.Context, proxies []*models.Proxy, timeoutSecs int, progressFn func(checked, active, failed int)) ([]models.ProxyTestResult, error) {
	if len(proxies) == 0 {
		return []models.ProxyTestResult{}, nil
	}

	// Ensure settings are loaded (CheckProxy also lazy-loads, but we read
	// Workers here to size the pool).
	if h.settings == nil {
		settings, err := h.settingsRepo.GetAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load settings: %w", err)
		}
		h.settings = &settings.HealthCheck
	}

	workers := h.settings.Workers
	if workers < 1 {
		workers = 1
	}

	h.logger.Info("starting health check", "proxy_count", len(proxies), "workers", workers)

	// Create worker pool
	wp := workerpool.New(workers)
	results := make([]models.ProxyTestResult, len(proxies))

	// Running counters for progress reporting (guarded; callbacks fire from
	// worker goroutines).
	var (
		mu                      sync.Mutex
		checked, active, failed int
	)

	// Submit jobs
	for i, proxy := range proxies {
		idx := i
		p := proxy
		wp.Submit(func() {
			result, err := h.checkProxy(ctx, p, timeoutSecs)
			if err != nil {
				h.logger.Error("health check error",
					"proxy_id", p.ID,
					"proxy_address", p.Address,
					"error", err,
				)
				results[idx] = models.ProxyTestResult{
					ID:       p.ID,
					Address:  p.Address,
					Status:   "failed",
					TestedAt: time.Now(),
				}
				errMsg := err.Error()
				results[idx].Error = &errMsg
			} else {
				results[idx] = *result
			}

			if progressFn != nil {
				mu.Lock()
				checked++
				if results[idx].Status == "active" {
					active++
				} else {
					failed++
				}
				c, a, f := checked, active, failed
				mu.Unlock()
				progressFn(c, a, f)
			}
		})
	}

	// Wait for all jobs to complete
	wp.StopWait()

	h.logger.Info("health check completed", "proxy_count", len(proxies))

	return results, nil
}

// createTransport creates an HTTP transport for the proxy
func (h *HealthChecker) createTransport(p *models.Proxy) (*http.Transport, error) {
	// Use shared transport creation utility
	return CreateProxyTransport(p)
}
