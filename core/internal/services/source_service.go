package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// parsedProxy holds the extracted fields from a single proxy list line.
type parsedProxy struct {
	address  string  // host:port
	protocol string  // http|https|socks4|socks4a|socks5 — empty means "use source default"
	username *string // nil if not present
	password *string // nil if not present
}

// parseProxyLine parses one line according to the source's format setting.
//
// Format "auto" accepts (auth is always optional):
//
//	host:port
//	user:pass@host:port
//	protocol://host:port
//	protocol://user:pass@host:port
//
// The explicit formats handle lists whose field order auto-detection cannot
// distinguish, e.g. host:port:user:pass (Webshare downloads). Lines that are
// a bare host:port still parse under any explicit format, so mixed lists
// don't break.
func parseProxyLine(line string, format string) (parsedProxy, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return parsedProxy{}, false
	}

	var proto string
	var user, pass *string

	// ── 1. Strip protocol scheme if present ──────────────────────────────
	if idx := strings.Index(line, "://"); idx != -1 {
		scheme := strings.ToLower(line[:idx])
		switch scheme {
		case "http", "https", "socks4", "socks4a", "socks5":
			proto = scheme
		default:
			// Unknown scheme — treat whole thing as address and hope for the best
		}
		line = line[idx+3:]
	}

	switch format {
	case models.SourceFormatHostPortUserPass:
		return parseColonFields(line, proto, false)
	case models.SourceFormatUserPassHostPort:
		return parseColonFields(line, proto, true)
	case models.SourceFormatHostPortAtAuth:
		return parseAddressAtAuth(line, proto)
	}
	// "auto" (and anything unrecognized) falls through to detection below.

	// ── 2. Try url.Parse for user:pass@host:port ─────────────────────────
	// Wrap with a fake scheme so url.Parse handles the userinfo correctly.
	parsed, err := url.Parse("x://" + line)
	if err == nil && parsed.Host != "" {
		if ui := parsed.User; ui != nil {
			u := ui.Username()
			if u != "" {
				user = &u
			}
			if p, ok := ui.Password(); ok && p != "" {
				pass = &p
			}
		}
		host := parsed.Host
		// url.Parse puts host:port in Host
		if !strings.Contains(host, ":") {
			return parsedProxy{}, false // no port — unusable
		}
		return parsedProxy{
			address:  host,
			protocol: proto,
			username: user,
			password: pass,
		}, true
	}

	// ── 3. Fallback: bare host:port (no userinfo) ─────────────────────────
	if strings.Contains(line, ":") {
		return parsedProxy{address: line, protocol: proto}, true
	}

	return parsedProxy{}, false
}

// parseColonFields handles 4-field colon-separated lines. authFirst picks
// user:pass:host:port over host:port:user:pass. A 2-field line is treated as
// a bare host:port.
func parseColonFields(line, proto string, authFirst bool) (parsedProxy, bool) {
	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return parsedProxy{}, false
		}
		return parsedProxy{address: line, protocol: proto}, true
	case 4:
		var host, port, user, pass string
		if authFirst {
			user, pass, host, port = parts[0], parts[1], parts[2], parts[3]
		} else {
			host, port, user, pass = parts[0], parts[1], parts[2], parts[3]
		}
		if host == "" || port == "" {
			return parsedProxy{}, false
		}
		p := parsedProxy{address: host + ":" + port, protocol: proto}
		if user != "" {
			p.username = &user
		}
		if pass != "" {
			p.password = &pass
		}
		return p, true
	}
	return parsedProxy{}, false
}

// parseAddressAtAuth handles host:port@user:pass lines (the reverse of the
// URL userinfo convention). The @user:pass part is optional.
func parseAddressAtAuth(line, proto string) (parsedProxy, bool) {
	addr, auth, hasAuth := strings.Cut(line, "@")
	if !strings.Contains(addr, ":") {
		return parsedProxy{}, false
	}
	p := parsedProxy{address: addr, protocol: proto}
	if hasAuth {
		user, pass, _ := strings.Cut(auth, ":")
		if user != "" {
			p.username = &user
		}
		if pass != "" {
			p.password = &pass
		}
	}
	return p, true
}

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
		if p.protocol != "" {
			proto = p.protocol
		}
		requests = append(requests, models.CreateProxyRequest{
			Address:  p.address,
			Protocol: proto,
			Username: p.username,
			Password: p.password,
			SourceID: &src.ID,
		})
		addresses = append(addresses, p.address)
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
		if _, err := s.proxyRepo.GetDB().Pool.Exec(ctx, `
			UPDATE proxies SET
				country_code   = $1,
				country_name   = $2,
				region_name    = $3,
				city_name      = $4,
				latitude       = $5,
				longitude      = $6,
				isp            = $7,
				geo_updated_at = NOW()
			WHERE address = $8
		`, geo.CountryCode, geo.CountryName, geo.RegionName, geo.CityName,
			geo.Latitude, geo.Longitude, geo.ISP, addr,
		); err != nil {
			s.logger.Warn("failed to update geo for proxy", "address", addr, "error", err)
		}
	}
}

// EnrichAll re-runs geo enrichment for all proxies that have no geo data yet.
func (s *SourceService) EnrichAll(ctx context.Context) (int, error) {
	rows, err := s.proxyRepo.GetDB().Pool.Query(ctx,
		`SELECT address FROM proxies WHERE country_code IS NULL LIMIT 500`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			continue
		}
		addresses = append(addresses, addr)
	}
	rows.Close()

	if len(addresses) == 0 {
		return 0, nil
	}

	geos := s.geoSvc.EnrichProxies(ctx, addresses)
	for addr, geo := range geos {
		s.proxyRepo.GetDB().Pool.Exec(ctx, `
			UPDATE proxies SET
				country_code   = $1,
				country_name   = $2,
				region_name    = $3,
				city_name      = $4,
				latitude       = $5,
				longitude      = $6,
				isp            = $7,
				geo_updated_at = NOW()
			WHERE address = $8
		`, geo.CountryCode, geo.CountryName, geo.RegionName, geo.CityName,
			geo.Latitude, geo.Longitude, geo.ISP, addr)
	}

	// Re-sync pools now that geo data has changed
	go s.syncAllPools(context.Background())

	return len(geos), nil
}

// parseProxyList parses a proxy list file, one entry per line, using the
// source's format setting. Invalid lines are silently skipped.
func parseProxyList(r io.Reader, format string) ([]parsedProxy, error) {
	var proxies []parsedProxy
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if p, ok := parseProxyLine(scanner.Text(), format); ok {
			proxies = append(proxies, p)
		}
	}
	return proxies, scanner.Err()
}
