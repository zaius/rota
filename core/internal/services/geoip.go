package services

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
	"github.com/oschwald/geoip2-golang/v2"
)

// geoDBPath is relative to the working directory: /app/data/geoip in the
// container, on the persistent data volume.
const geoDBPath = "data/geoip/dbip-country-lite.mmdb"

var errNotPublished = errors.New("not published yet")

// GeoIPService resolves proxy countries from DB-IP's free IP to Country Lite
// database (CC BY 4.0), which it downloads each month.
//
// The database file's modification time records the month of the release it
// holds, so the file is due for replacement once a new month starts.
type GeoIPService struct {
	client *http.Client
	logger *logger.Logger

	// Lookups hold mu for reading and a reader swap holds it for writing, so a
	// swap never closes a reader under a lookup.
	mu     sync.RWMutex
	reader *geoip2.Reader
}

// NewGeoIPService loads the database already on disk, if any; Run downloads
// one when it is missing or outdated.
func NewGeoIPService(log *logger.Logger) *GeoIPService {
	g := &GeoIPService{client: &http.Client{Timeout: 5 * time.Minute}, logger: log}
	if _, err := os.Stat(geoDBPath); err == nil {
		g.open()
	}
	return g
}

// Name identifies the service for the lifecycle manager.
func (g *GeoIPService) Name() string { return "geoip" }

// Run keeps the database current, checking hourly until ctx ends.
func (g *GeoIPService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		safeworker.Call(g.logger, "geoip_refresh", func() { g.refresh(ctx) })
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh downloads this month's release once the file on disk is from an
// earlier month. DB-IP publishes early in the month; until it does, the
// previous release stays in use, and a server with no file at all takes it.
func (g *GeoIPService) refresh(ctx context.Context) {
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	info, statErr := os.Stat(geoDBPath)
	if statErr == nil && !info.ModTime().Before(month) {
		return
	}
	err := g.download(ctx, month)
	if errors.Is(err, errNotPublished) && statErr != nil {
		err = g.download(ctx, month.AddDate(0, -1, 0))
	}
	if errors.Is(err, errNotPublished) && statErr == nil {
		return
	}
	if err != nil {
		g.logger.Error("failed to download geoip database", "error", err)
		return
	}
	g.open()
}

// download installs a month's release at geoDBPath, replacing the old file
// only once the new one opens cleanly.
func (g *GeoIPService) download(ctx context.Context, month time.Time) error {
	url := "https://download.db-ip.com/free/dbip-country-lite-" + month.Format("2006-01") + ".mmdb.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", url, errNotPublished)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(geoDBPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(geoDBPath), "dbip-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := io.Copy(tmp, gz); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	reader, err := geoip2.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("downloaded database is invalid: %w", err)
	}
	reader.Close()
	if err := os.Chtimes(tmp.Name(), month, month); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), geoDBPath)
}

// open loads the file at geoDBPath, closing the reader it replaces.
func (g *GeoIPService) open() {
	reader, err := geoip2.Open(geoDBPath)
	if err != nil {
		g.logger.Error("failed to open geoip database", "path", geoDBPath, "error", err)
		return
	}
	g.mu.Lock()
	old := g.reader
	g.reader = reader
	g.mu.Unlock()
	if old != nil {
		old.Close()
	}
	g.logger.Info("loaded geoip database", "path", geoDBPath,
		"built", time.Unix(int64(reader.Metadata().BuildEpoch), 0).UTC().Format(time.DateOnly))
}

// EnrichProxies returns the countries of the proxies at addresses ("host:port",
// by IP or hostname), keyed by address. It leaves out addresses it cannot
// place.
func (g *GeoIPService) EnrichProxies(ctx context.Context, addresses []string) map[string]models.GeoInfo {
	result := make(map[string]models.GeoInfo, len(addresses))
	byHost := make(map[string]*models.GeoInfo)
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		geo, seen := byHost[host]
		if !seen {
			geo = g.lookup(ctx, host)
			byHost[host] = geo
		}
		if geo != nil {
			result[address] = *geo
		}
	}
	return result
}

// lookup resolves a host to an address, then to its country.
func (g *GeoIPService) lookup(ctx context.Context, host string) *models.GeoInfo {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			return nil
		}
		addr = addrs[0]
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.reader == nil {
		return nil
	}
	rec, err := g.reader.Country(addr.Unmap())
	if err != nil || rec.Country.ISOCode == "" {
		return nil
	}
	return &models.GeoInfo{CountryCode: rec.Country.ISOCode, CountryName: rec.Country.Names.English}
}
