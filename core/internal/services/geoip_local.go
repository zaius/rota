package services

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/oschwald/geoip2-golang/v2"
)

// maxMMDBBytes bounds a downloaded database; GeoLite2-City is well under
// 100 MiB uncompressed.
const maxMMDBBytes = 512 << 20

const maxMindDownloadBase = "https://download.maxmind.com"

// geoDatabase is one loaded MaxMind database file.
type geoDatabase struct {
	reader  *geoip2.Reader
	modTime time.Time
}

// LocalGeoDB resolves IPs from local MaxMind databases: a City database for
// location and, optionally, an ASN database whose AS organization stands in
// for the ISP. Refresh picks up database files replaced on disk (for example
// by geoipupdate) and, with a license key, downloads missing or outdated ones
// from MaxMind.
type LocalGeoDB struct {
	cityPath     string
	asnPath      string
	licenseKey   string
	accountID    string
	maxAge       time.Duration
	client       *http.Client
	downloadBase string
	logger       *logger.Logger

	// Lookups hold mu for reading and reader swaps hold it for writing, so a
	// swap never closes a reader under a lookup.
	mu   sync.RWMutex
	city *geoDatabase
	asn  *geoDatabase
}

// NewLocalGeoDB returns nil when cfg names no City database. It loads
// the database files already on disk; Refresh downloads missing ones.
func NewLocalGeoDB(cfg config.GeoIPConfig, log *logger.Logger) *LocalGeoDB {
	if cfg.CityDB == "" {
		return nil
	}
	d := &LocalGeoDB{
		cityPath:     cfg.CityDB,
		asnPath:      cfg.ASNDB,
		licenseKey:   cfg.LicenseKey,
		accountID:    cfg.AccountID,
		maxAge:       time.Duration(cfg.UpdateHours) * time.Hour,
		client:       &http.Client{Timeout: 5 * time.Minute},
		downloadBase: maxMindDownloadBase,
		logger:       log,
	}
	d.reloadChanged()
	return d
}

// Ready reports whether the City database has loaded.
func (d *LocalGeoDB) Ready() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.city != nil
}

// Lookup resolves one IP. ok is false for an invalid IP or one the City
// database has no country for, such as a private address.
func (d *LocalGeoDB) Lookup(ip string) (geo models.GeoInfo, ok bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return geo, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.city == nil {
		return geo, false
	}

	rec, err := d.city.reader.City(addr)
	if err != nil {
		return geo, false
	}
	country := rec.Country
	if country.ISOCode == "" {
		country = rec.RegisteredCountry
	}
	if country.ISOCode == "" {
		return geo, false
	}
	geo.CountryCode = country.ISOCode
	geo.CountryName = country.Names.English
	geo.CityName = rec.City.Names.English
	if len(rec.Subdivisions) > 0 {
		geo.RegionName = rec.Subdivisions[0].Names.English
	}
	if rec.Location.HasCoordinates() {
		geo.Latitude = *rec.Location.Latitude
		geo.Longitude = *rec.Location.Longitude
	}
	if d.asn != nil {
		if a, err := d.asn.reader.ASN(addr); err == nil {
			geo.ISP = a.AutonomousSystemOrganization
		}
	}
	return geo, true
}

// Refresh downloads missing or outdated databases when there is a license key,
// then loads any database file that changed on disk.
func (d *LocalGeoDB) Refresh(ctx context.Context) {
	if d == nil {
		return
	}
	if d.licenseKey != "" {
		d.downloadIfStale(ctx, "GeoLite2-City", d.cityPath)
		if d.asnPath != "" {
			d.downloadIfStale(ctx, "GeoLite2-ASN", d.asnPath)
		}
	}
	d.reloadChanged()
}

func (d *LocalGeoDB) downloadIfStale(ctx context.Context, edition, path string) {
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < d.maxAge {
		return
	}
	if err := d.download(ctx, edition, path); err != nil {
		d.logger.Error("failed to download geoip database", "edition", edition, "error", err)
		return
	}
	d.logger.Info("downloaded geoip database", "edition", edition, "path", path)
}

// download fetches an edition's tar.gz from MaxMind and installs its .mmdb
// at dest, replacing the old file only once the new one opens cleanly.
func (d *LocalGeoDB) download(ctx context.Context, edition, dest string) error {
	req, err := d.downloadRequest(ctx, edition)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		// url.Error repeats the request URL, which carries the license key.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	if err := extractMMDB(resp.Body, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write database: %w", err)
	}
	reader, err := geoip2.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("downloaded database is invalid: %w", err)
	}
	reader.Close()
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("install database: %w", err)
	}
	return nil
}

// downloadRequest builds the request for an edition's tar.gz. An account ID
// selects MaxMind's authenticated endpoint; a license key alone uses the one
// that takes the key as a parameter.
func (d *LocalGeoDB) downloadRequest(ctx context.Context, edition string) (*http.Request, error) {
	if d.accountID != "" {
		u := fmt.Sprintf("%s/geoip/databases/%s/download?suffix=tar.gz", d.downloadBase, url.PathEscape(edition))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(d.accountID, d.licenseKey)
		return req, nil
	}
	q := url.Values{"edition_id": {edition}, "license_key": {d.licenseKey}, "suffix": {"tar.gz"}}
	return http.NewRequestWithContext(ctx, http.MethodGet, d.downloadBase+"/app/geoip_download?"+q.Encode(), nil)
}

// extractMMDB copies the first .mmdb file in a tar.gz stream to w.
func extractMMDB(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("archive has no .mmdb file")
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(hdr.Name, ".mmdb") {
			continue
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxMMDBBytes+1))
		if err != nil {
			return fmt.Errorf("extract database: %w", err)
		}
		if n > maxMMDBBytes {
			return fmt.Errorf("database exceeds %d bytes", maxMMDBBytes)
		}
		return nil
	}
}

func (d *LocalGeoDB) reloadChanged() {
	d.reload(&d.city, d.cityPath)
	d.reload(&d.asn, d.asnPath)
}

// reload opens the file at path into slot when it differs from the loaded
// one, closing the reader it replaces.
func (d *LocalGeoDB) reload(slot **geoDatabase, path string) {
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || d.licenseKey == "" {
			d.logger.Warn("geoip database unavailable", "path", path, "error", err)
		}
		return
	}
	d.mu.RLock()
	current := *slot
	d.mu.RUnlock()
	if current != nil && info.ModTime().Equal(current.modTime) {
		return
	}

	reader, err := geoip2.Open(path)
	if err != nil {
		d.logger.Error("failed to open geoip database", "path", path, "error", err)
		return
	}
	d.mu.Lock()
	old := *slot
	*slot = &geoDatabase{reader: reader, modTime: info.ModTime()}
	d.mu.Unlock()
	if old != nil {
		old.reader.Close()
	}
	meta := reader.Metadata()
	d.logger.Info("loaded geoip database", "path", path, "type", meta.DatabaseType,
		"built", time.Unix(int64(meta.BuildEpoch), 0).UTC().Format(time.DateOnly))
}
