package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// ipAPIResponse is the response from ip-api.com batch endpoint
type ipAPIResponse struct {
	Status      string  `json:"status"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"regionName"`
	City        string  `json:"city"`
	ISP         string  `json:"isp"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Query       string  `json:"query"`
}

type cacheEntry struct {
	geo      models.GeoInfo
	cachedAt time.Time
}

// ipAPIBatchSize is the maximum number of queries ip-api.com accepts in one
// batch request.
const ipAPIBatchSize = 100

// GeoIPService performs IP geolocation lookups via ip-api.com (free, no key needed)
// It caches results for 24 h and batches requests in groups of 100.
type GeoIPService struct {
	client   *http.Client
	cache    map[string]cacheEntry
	mu       sync.RWMutex
	logger   *logger.Logger
	cacheTTL time.Duration

	// reqMu serialises outbound batch requests and spaces them out to stay
	// under ip-api.com's free-tier rate limit.
	reqMu       sync.Mutex
	lastReq     time.Time
	minInterval time.Duration

	// endpoint overrides the batch URL in tests.
	endpoint string
}

// NewGeoIPService creates a new GeoIPService
func NewGeoIPService(log *logger.Logger) *GeoIPService {
	return &GeoIPService{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		cache:       make(map[string]cacheEntry),
		logger:      log,
		cacheTTL:    24 * time.Hour,
		minInterval: 1500 * time.Millisecond, // ~40 batch req/min, under the free-tier cap
	}
}

// Name identifies the service for the lifecycle manager.
func (g *GeoIPService) Name() string { return "geoip-cache-sweep" }

// Run evicts expired cache entries every hour until ctx is cancelled. Without
// it the cache only ever grows: entries past their TTL are skipped on read but
// never removed.
func (g *GeoIPService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(time.Now())
		}
	}
}

// sweep drops every cache entry that has outlived the TTL as of now.
func (g *GeoIPService) sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for ip, entry := range g.cache {
		if now.Sub(entry.cachedAt) >= g.cacheTTL {
			delete(g.cache, ip)
		}
	}
}

// throttle blocks until at least minInterval has elapsed since the previous
// outbound request. Respects context cancellation.
func (g *GeoIPService) throttle(ctx context.Context) error {
	g.reqMu.Lock()
	defer g.reqMu.Unlock()
	if !g.lastReq.IsZero() {
		if wait := g.minInterval - time.Since(g.lastReq); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	g.lastReq = time.Now()
	return nil
}

// parseRetryAfter reads a Retry-After header in delta-seconds form, falling
// back when it is absent or unparseable.
func parseRetryAfter(h string, fallback time.Duration) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// extractIP parses "host:port" and returns just the host IP.
func extractIP(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// maybe no port
		return strings.TrimSpace(address)
	}
	return strings.TrimSpace(host)
}

// LookupOne returns GeoInfo for a single proxy address ("host:port" or bare IP).
func (g *GeoIPService) LookupOne(ctx context.Context, address string) (*models.GeoInfo, error) {
	ip := extractIP(address)
	if ip == "" {
		return nil, fmt.Errorf("empty address")
	}

	// Check cache first
	g.mu.RLock()
	if entry, ok := g.cache[ip]; ok && time.Since(entry.cachedAt) < g.cacheTTL {
		g.mu.RUnlock()
		geo := entry.geo
		return &geo, nil
	}
	g.mu.RUnlock()

	results, err := g.lookupBatch(ctx, []string{ip})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no result for %s", ip)
	}
	return &results[0], nil
}

// LookupBatch resolves GeoInfo for up to 100 addresses at once.
// Returns map[address] -> GeoInfo.
func (g *GeoIPService) LookupBatch(ctx context.Context, addresses []string) map[string]models.GeoInfo {
	result := make(map[string]models.GeoInfo)

	// Deduplicate by IP and separate cached from needed. Two proxies can share
	// an address, so `needed` is keyed off the map to avoid asking for the same
	// IP twice in one batch.
	ipToAddr := make(map[string]string) // ip -> original address
	pending := make(map[string]struct{})

	g.mu.RLock()
	for _, addr := range addresses {
		ip := extractIP(addr)
		if ip == "" {
			continue
		}
		ipToAddr[ip] = addr
		if entry, ok := g.cache[ip]; ok && time.Since(entry.cachedAt) < g.cacheTTL {
			result[addr] = entry.geo
		} else {
			pending[ip] = struct{}{}
		}
	}
	g.mu.RUnlock()

	if len(pending) == 0 {
		return result
	}

	needed := make([]string, 0, len(pending))
	for ip := range pending {
		needed = append(needed, ip)
	}

	// The batch endpoint caps each request at 100 queries, so chunk to fit.
	// lookupBatchRaw already writes successful lookups into the cache.
	for i := 0; i < len(needed); i += ipAPIBatchSize {
		end := min(i+ipAPIBatchSize, len(needed))
		batch := needed[i:end]

		raw, err := g.lookupBatchRaw(ctx, batch)
		if err != nil {
			g.logger.Warn("geoip batch lookup failed", "error", err, "ips", len(batch))
			continue
		}
		for ip, geo := range raw {
			if addr, ok := ipToAddr[ip]; ok {
				result[addr] = geo
			}
		}
	}

	return result
}

// lookupBatch fetches geo data for a slice of IPs (max 100)
func (g *GeoIPService) lookupBatch(ctx context.Context, ips []string) ([]models.GeoInfo, error) {
	raw, err := g.lookupBatchRaw(ctx, ips)
	if err != nil {
		return nil, err
	}
	var out []models.GeoInfo
	for _, v := range raw {
		out = append(out, v)
	}
	return out, nil
}

// doBatchRequest POSTs a marshalled batch body, applying the outbound throttle
// and backing off when the API answers 429.
func (g *GeoIPService) doBatchRequest(ctx context.Context, body []byte) ([]ipAPIResponse, error) {
	const maxAttempts = 3
	var lastErr error
	for range maxAttempts {
		if err := g.throttle(ctx); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.batchURL(), strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("geoip request failed: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := parseRetryAfter(resp.Header.Get("Retry-After"), 2*g.minInterval)
			resp.Body.Close()
			g.logger.Warn("geoip rate limited (429), backing off", "wait", wait.String())
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("geoip api returned 429")
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("geoip api returned %d", resp.StatusCode)
		}

		var responses []ipAPIResponse
		if err := json.NewDecoder(resp.Body).Decode(&responses); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to decode geoip response: %w", err)
		}
		resp.Body.Close()
		return responses, nil
	}
	return nil, lastErr
}

// batchURL is the ip-api.com batch endpoint, overridable in tests.
func (g *GeoIPService) batchURL() string {
	if g.endpoint != "" {
		return g.endpoint
	}
	return "http://ip-api.com/batch"
}

// lookupBatchRaw fetches geo data and returns map[ip] -> GeoInfo
func (g *GeoIPService) lookupBatchRaw(ctx context.Context, ips []string) (map[string]models.GeoInfo, error) {
	if len(ips) == 0 {
		return nil, nil
	}

	// Build JSON body: [{"query":"1.2.3.4","fields":"..."}, ...]
	type reqItem struct {
		Query  string `json:"query"`
		Fields string `json:"fields"`
	}
	items := make([]reqItem, len(ips))
	fields := "status,country,countryCode,regionName,city,isp,lat,lon,query"
	for i, ip := range ips {
		items[i] = reqItem{Query: ip, Fields: fields}
	}

	body, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal geoip request: %w", err)
	}

	responses, err := g.doBatchRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	result := make(map[string]models.GeoInfo, len(responses))
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range responses {
		if r.Status != "success" {
			continue
		}
		geo := models.GeoInfo{
			CountryCode: r.CountryCode,
			CountryName: r.Country,
			RegionName:  r.Region,
			CityName:    r.City,
			ISP:         r.ISP,
			Latitude:    r.Lat,
			Longitude:   r.Lon,
		}
		result[r.Query] = geo
		g.cache[r.Query] = cacheEntry{geo: geo, cachedAt: time.Now()}
	}
	return result, nil
}

// EnrichProxies calls ip-api.com for all addresses and returns map[address]->GeoInfo
func (g *GeoIPService) EnrichProxies(ctx context.Context, addresses []string) map[string]models.GeoInfo {
	if len(addresses) == 0 {
		return nil
	}

	// Deduplicate IPs
	ipToAddr := make(map[string]string)
	for _, addr := range addresses {
		ip := extractIP(addr)
		if ip != "" {
			ipToAddr[ip] = addr
		}
	}

	ips := make([]string, 0, len(ipToAddr))
	for ip := range ipToAddr {
		ips = append(ips, ip)
	}

	result := make(map[string]models.GeoInfo)

	// Check cache
	var needed []string
	g.mu.RLock()
	for _, ip := range ips {
		if entry, ok := g.cache[ip]; ok && time.Since(entry.cachedAt) < g.cacheTTL {
			if addr, ok2 := ipToAddr[ip]; ok2 {
				result[addr] = entry.geo
			}
		} else {
			needed = append(needed, ip)
		}
	}
	g.mu.RUnlock()

	if len(needed) == 0 {
		return result
	}

	for i := 0; i < len(needed); i += ipAPIBatchSize {
		end := min(i+ipAPIBatchSize, len(needed))
		batch := needed[i:end]

		raw, err := g.lookupBatchRaw(ctx, batch)
		if err != nil {
			g.logger.Warn("geoip enrichment batch failed", "error", err, "ips", len(batch))
			continue
		}
		for ip, geo := range raw {
			if addr, ok := ipToAddr[ip]; ok {
				result[addr] = geo
			}
		}
	}

	return result
}
