package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/lineformat"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// ProxyTester is the subset of HealthChecker used by SourceService.
type ProxyTester interface {
	CheckAllProxies(ctx context.Context) ([]models.ProxyTestResult, error)
}

// SourceService fetches proxy lists from remote URLs and imports them into the DB.
type SourceService struct {
	sourceRepo *repository.SourceRepository
	proxyRepo  *repository.ProxyRepository
	poolRepo   *repository.PoolRepository
	geoSvc     *GeoIPService
	tester     ProxyTester // optional: auto health-check after import
	logger     *logger.Logger
	client     *http.Client

	mu     sync.Mutex
	stopCh chan struct{}
}

// NewSourceService creates a new SourceService.
func NewSourceService(
	sourceRepo *repository.SourceRepository,
	proxyRepo *repository.ProxyRepository,
	poolRepo *repository.PoolRepository,
	geoSvc *GeoIPService,
	log *logger.Logger,
) *SourceService {
	return &SourceService{
		sourceRepo: sourceRepo,
		proxyRepo:  proxyRepo,
		poolRepo:   poolRepo,
		geoSvc:     geoSvc,
		logger:     log,
		client:     &http.Client{Timeout: 30 * time.Second},
		stopCh:     make(chan struct{}),
	}
}

// SetHealthChecker sets the proxy tester for auto health checks after import.
func (s *SourceService) SetHealthChecker(t ProxyTester) {
	s.tester = t
}

// Name identifies the service for the lifecycle manager.
func (s *SourceService) Name() string { return "source-fetcher" }

// Run checks for due sources every minute until ctx is cancelled.
func (s *SourceService) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.fetchDueSources(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// FetchNow fetches a single source immediately (called from API handler).
func (s *SourceService) FetchNow(ctx context.Context, sourceID int) (*models.ProxySource, int, error) {
	src, err := s.sourceRepo.GetByID(ctx, sourceID)
	if err != nil || src == nil {
		return nil, 0, fmt.Errorf("source not found: %w", err)
	}
	imported, total, fetchErr := s.fetchAndImport(ctx, src)
	_ = s.sourceRepo.UpdateFetchResult(ctx, src.ID, imported, total, fetchErr)
	if fetchErr != nil {
		return src, 0, fetchErr
	}
	updated, _ := s.sourceRepo.GetByID(ctx, src.ID)
	return updated, imported, nil
}

// fetchDueSources finds all sources that are overdue and fetches them.
func (s *SourceService) fetchDueSources(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sources, err := s.sourceRepo.GetDueForFetch(ctx)
	if err != nil {
		s.logger.Error("failed to get due sources", "error", err)
		return
	}
	for _, src := range sources {
		srcCopy := src
		imported, total, fetchErr := s.fetchAndImport(ctx, &srcCopy)
		if updateErr := s.sourceRepo.UpdateFetchResult(ctx, src.ID, imported, total, fetchErr); updateErr != nil {
			s.logger.Error("failed to update source fetch result", "source_id", src.ID, "error", updateErr)
		}
		if fetchErr != nil {
			s.logger.Error("failed to fetch source", "source_id", src.ID, "url", src.URL, "error", fetchErr)
		} else {
			s.logger.Info("fetched source",
				"source_id", src.ID, "name", src.Name,
				"imported", imported, "total", total)
		}
	}

	// After all sources are fetched, re-sync all auto_sync pools
	go s.syncAllPools(ctx)
}

// syncAllPools re-syncs all auto_sync pools — called after a fetch batch completes
func (s *SourceService) syncAllPools(ctx context.Context) {
	synced, err := s.poolRepo.SyncAllAutoSyncPools(ctx)
	if err != nil {
		s.logger.Error("auto pool sync after fetch failed", "error", err)
	} else if synced > 0 {
		s.logger.Info("auto-synced pools after fetch", "pools", synced)
	}
}

// fetchAndImport downloads the list, parses it, and upserts proxies.
// Returns (imported, total, err):
//   - imported = number of NEW proxies created this fetch
//   - total    = total number of parseable proxy lines returned by the source
func (s *SourceService) fetchAndImport(ctx context.Context, src *models.ProxySource) (int, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", "Rota-SourceFetcher/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("unexpected HTTP %d from %s", resp.StatusCode, src.URL)
	}

	parsed, err := parseProxyList(resp.Body, src.Format)
	if err != nil {
		return 0, 0, fmt.Errorf("parse failed: %w", err)
	}
	total := len(parsed)
	if total == 0 {
		return 0, 0, nil
	}

	// Build upsert requests — protocol from line takes priority over source default
	requests := make([]models.CreateProxyRequest, 0, total)
	addresses := make([]string, 0, total)
	for _, p := range parsed {
		proto := src.Protocol
		if p.Protocol != "" {
			proto = p.Protocol
		}
		requests = append(requests, models.CreateProxyRequest{
			Address:  p.Address,
			Protocol: proto,
			Username: p.Username,
			Password: p.Password,
			SourceID: &src.ID,
		})
		addresses = append(addresses, p.Address)
	}

	created, _ := s.bulkUpsert(ctx, requests)

	// Stamp every address present in this fetch as "last seen now" so the
	// soft-cleanup cron knows they're still live.
	if err := s.sourceRepo.MarkSeen(ctx, src.ID, addresses); err != nil {
		s.logger.Warn("failed to mark proxies as seen", "source_id", src.ID, "error", err)
	}

	// Per-source soft cleanup: delete proxies that have been absent from this
	// source's fetch output for longer than cleanup_days.
	if src.CleanupEnabled && src.CleanupDays > 0 {
		deleted, err := s.sourceRepo.DeleteStaleForSource(ctx, src.ID, src.CleanupDays)
		if err != nil {
			s.logger.Warn("source cleanup failed", "source_id", src.ID, "error", err)
		} else if deleted > 0 {
			s.logger.Info("source cleanup removed stale proxies",
				"source_id", src.ID, "deleted", deleted, "threshold_days", src.CleanupDays)
		}
	}

	// Enrich geo data in the background
	go s.enrichGeo(context.Background(), addresses)

	return created, total, nil
}

// bulkUpsert upserts proxies. Returns (created, failed).
// Uses Upsert so that username/password from the list update existing entries.
func (s *SourceService) bulkUpsert(ctx context.Context, proxies []models.CreateProxyRequest) (int, int) {
	created := 0
	failed := 0
	for _, req := range proxies {
		_, status, err := s.proxyRepo.Upsert(ctx, req)
		if err != nil {
			failed++
		} else if status == "created" {
			created++
		}
		// "updated" counts neither as created nor failed — it's an update
	}
	return created, failed
}

// enrichGeo fetches geo data for the given addresses and updates the DB.
func (s *SourceService) enrichGeo(ctx context.Context, addresses []string) {
	if len(addresses) == 0 {
		return
	}
	geos := s.geoSvc.EnrichProxies(ctx, addresses)
	if len(geos) == 0 {
		return
	}

	for addr, geo := range geos {
		if err := s.proxyRepo.UpdateGeo(ctx, addr, geo); err != nil {
			s.logger.Warn("failed to update geo for proxy", "address", addr, "error", err)
		}
	}
}

// EnrichAll re-runs geo enrichment for all proxies that have no geo data yet.
func (s *SourceService) EnrichAll(ctx context.Context) (int, error) {
	addresses, err := s.proxyRepo.ListUngeotaggedAddresses(ctx, 500)
	if err != nil {
		return 0, err
	}
	if len(addresses) == 0 {
		return 0, nil
	}

	geos := s.geoSvc.EnrichProxies(ctx, addresses)
	for addr, geo := range geos {
		if err := s.proxyRepo.UpdateGeo(ctx, addr, geo); err != nil {
			s.logger.Warn("failed to update geo for proxy", "address", addr, "error", err)
		}
	}

	// Re-sync pools now that geo data has changed
	go s.syncAllPools(context.Background())

	return len(geos), nil
}

// parseProxyList parses a proxy list file, one entry per line, using the
// source's lineformat template. Lines that don't match the format are
// silently skipped.
func parseProxyList(r io.Reader, format string) ([]lineformat.Parsed, error) {
	lf, err := lineformat.Compile(format)
	if err != nil {
		return nil, fmt.Errorf("invalid line format %q: %w", format, err)
	}
	var proxies []lineformat.Parsed
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if p, ok := lf.Parse(scanner.Text()); ok {
			proxies = append(proxies, p)
		}
	}
	return proxies, scanner.Err()
}
