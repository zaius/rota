package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
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

// GeoIPService performs IP geolocation lookups, from local MaxMind databases
// when configured and otherwise via ip-api.com (free, no key needed, batched in
// groups of 100). It caches results for 24 h.
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

	// local, when non-nil, answers lookups from MaxMind databases. Until its
	// City database loads, lookups fall back to ip-api.com.
	local *LocalGeoDB
}

// NewGeoIPService creates a new GeoIPService. local may be nil.
func NewGeoIPService(log *logger.Logger, local *LocalGeoDB) *GeoIPService {
	return &GeoIPService{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		cache:       make(map[string]cacheEntry),
		logger:      log,
		cacheTTL:    24 * time.Hour,
		minInterval: 1500 * time.Millisecond, // ~40 batch req/min, under the free-tier cap
		local:       local,
	}
}

// Name identifies the service for the lifecycle manager.
func (g *GeoIPService) Name() string { return "geoip" }

// Run keeps the local databases current and evicts expired cache entries every
// hour until ctx ends. Without the sweep the cache only ever grows: reads skip
// entries past their TTL, but nothing removes them.
func (g *GeoIPService) Run(ctx context.Context) {
	if g.local != nil {
		safeworker.Call(g.logger, "geoip_database_refresh", func() { g.local.Refresh(ctx) })
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safeworker.Call(g.logger, "geoip_cache_sweep", func() { g.sweep(time.Now()) })
			if g.local != nil {
				safeworker.Call(g.logger, "geoip_database_refresh", func() { g.local.Refresh(ctx) })
			}
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
	geo, ok := g.resolve(ctx, []string{ip})[ip]
	if !ok {
		return nil, fmt.Errorf("no result for %s", ip)
	}
	return &geo, nil
}

// LookupBatch resolves GeoInfo for any number of addresses and returns
// map[address] -> GeoInfo. It looks up each IP once, however many addresses
// share it.
func (g *GeoIPService) LookupBatch(ctx context.Context, addresses []string) map[string]models.GeoInfo {
	addrsByIP := make(map[string][]string)
	ips := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		ip := extractIP(addr)
		if ip == "" {
			continue
		}
		if _, seen := addrsByIP[ip]; !seen {
			ips = append(ips, ip)
		}
		addrsByIP[ip] = append(addrsByIP[ip], addr)
	}

	result := make(map[string]models.GeoInfo, len(addresses))
	for ip, geo := range g.resolve(ctx, ips) {
		for _, addr := range addrsByIP[ip] {
			result[addr] = geo
		}
	}
	return result
}

// resolve returns geo data for distinct IPs or hostnames: cached entries while
// fresh, IPs from the local databases when loaded, and the rest from
// ip-api.com.
func (g *GeoIPService) resolve(ctx context.Context, ips []string) map[string]models.GeoInfo {
	result := make(map[string]models.GeoInfo, len(ips))
	var needed []string
	g.mu.RLock()
	for _, ip := range ips {
		if entry, ok := g.cache[ip]; ok && time.Since(entry.cachedAt) < g.cacheTTL {
			result[ip] = entry.geo
		} else {
			needed = append(needed, ip)
		}
	}
	g.mu.RUnlock()
	if len(needed) == 0 {
		return result
	}

	if g.local.Ready() {
		found := make(map[string]models.GeoInfo, len(needed))
		var hostnames []string
		for _, ip := range needed {
			// The databases hold addresses only; ip-api.com resolves
			// proxies given by hostname itself.
			if _, err := netip.ParseAddr(ip); err != nil {
				hostnames = append(hostnames, ip)
				continue
			}
			if geo, ok := g.local.Lookup(ip); ok {
				found[ip] = geo
			}
		}
		now := time.Now()
		g.mu.Lock()
		for ip, geo := range found {
			result[ip] = geo
			g.cache[ip] = cacheEntry{geo: geo, cachedAt: now}
		}
		g.mu.Unlock()
		if len(hostnames) == 0 {
			return result
		}
		needed = hostnames
	}

	// The batch endpoint caps each request at 100 queries, so chunk to fit.
	// lookupBatchRaw already writes successful lookups into the cache.
	for i := 0; i < len(needed); i += ipAPIBatchSize {
		batch := needed[i:min(i+ipAPIBatchSize, len(needed))]
		raw, err := g.lookupBatchRaw(ctx, batch)
		if err != nil {
			g.logger.Warn("geoip batch lookup failed", "error", err, "ips", len(batch))
			continue
		}
		for ip, geo := range raw {
			result[ip] = geo
		}
	}
	return result
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

// EnrichProxies resolves geo data for proxy addresses, returning
// map[address] -> GeoInfo.
func (g *GeoIPService) EnrichProxies(ctx context.Context, addresses []string) map[string]models.GeoInfo {
	if len(addresses) == 0 {
		return nil
	}
	return g.LookupBatch(ctx, addresses)
}
