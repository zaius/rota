package services

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// cityRecord builds a GeoLite2-City style record.
func cityRecord(countryCode, country, region, city string) mmdbtype.Map {
	return mmdbtype.Map{
		"country":      mmdbtype.Map{"iso_code": mmdbtype.String(countryCode), "names": mmdbtype.Map{"en": mmdbtype.String(country)}},
		"city":         mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(city)}},
		"subdivisions": mmdbtype.Slice{mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(region)}}},
		"location":     mmdbtype.Map{"latitude": mmdbtype.Float64(51.5), "longitude": mmdbtype.Float64(-0.1)},
	}
}

// mmdbBytes serializes records keyed by CIDR into a database of dbType.
func mmdbBytes(t *testing.T, dbType string, records map[string]mmdbtype.Map) []byte {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: dbType, RecordSize: 24})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, rec := range records {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Insert(network, rec); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if _, err := tree.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path string, data []byte, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestLocalGeoDB_LooksUpCityAndISP(t *testing.T) {
	dir := t.TempDir()
	cityPath, asnPath := filepath.Join(dir, "city.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeFile(t, cityPath, mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("GB", "United Kingdom", "England", "London"),
	}), time.Now())
	writeFile(t, asnPath, mmdbBytes(t, "GeoLite2-ASN", map[string]mmdbtype.Map{
		"81.2.69.0/24": {"autonomous_system_organization": mmdbtype.String("Andrews & Arnold Ltd")},
	}), time.Now())

	d := NewLocalGeoDB(config.GeoIPConfig{CityDB: cityPath, ASNDB: asnPath, UpdateHours: 1}, logger.New("error"))
	if !d.Ready() {
		t.Fatal("databases on disk were not loaded")
	}

	geo, ok := d.Lookup("81.2.69.160")
	if !ok {
		t.Fatal("known IP not found")
	}
	if geo.CountryCode != "GB" || geo.CountryName != "United Kingdom" || geo.RegionName != "England" ||
		geo.CityName != "London" || geo.ISP != "Andrews & Arnold Ltd" || geo.Latitude != 51.5 {
		t.Errorf("geo = %+v", geo)
	}
	for _, ip := range []string{"8.8.8.8", "10.0.0.1", "not-an-ip"} {
		if _, ok := d.Lookup(ip); ok {
			t.Errorf("Lookup(%q) found a record, want none", ip)
		}
	}
}

func TestLocalGeoDB_ReloadsReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("GB", "United Kingdom", "England", "London"),
	}), time.Now().Add(-time.Hour))
	d := NewLocalGeoDB(config.GeoIPConfig{CityDB: path, UpdateHours: 1}, logger.New("error"))

	writeFile(t, path, mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("FR", "France", "Ile-de-France", "Paris"),
	}), time.Now())
	d.Refresh(context.Background())

	if geo, _ := d.Lookup("81.2.69.160"); geo.CountryCode != "FR" {
		t.Errorf("after replacing the file, country = %q, want FR", geo.CountryCode)
	}
}

// tarGz wraps one file in a tar.gz the way MaxMind ships its editions.
func tarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"GeoLite2-City_20260922/COPYRIGHT.txt", []byte("(c) MaxMind")}, {name, data}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLocalGeoDB_DownloadsMissingDatabase(t *testing.T) {
	archive := tarGz(t, "GeoLite2-City_20260922/GeoLite2-City.mmdb", mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("GB", "United Kingdom", "England", "London"),
	}))

	for _, tc := range []struct {
		name      string
		accountID string
		check     func(*testing.T, *http.Request)
	}{
		{"license key parameter", "", func(t *testing.T, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/app/geoip_download" || q.Get("edition_id") != "GeoLite2-City" || q.Get("license_key") != "k3y" {
				t.Errorf("request = %s", r.URL)
			}
		}},
		{"account basic auth", "12345", func(t *testing.T, r *http.Request) {
			user, pass, _ := r.BasicAuth()
			if r.URL.Path != "/geoip/databases/GeoLite2-City/download" || user != "12345" || pass != "k3y" {
				t.Errorf("request = %s (auth %s:%s)", r.URL, user, pass)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var downloads atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				tc.check(t, r)
				w.Write(archive)
			}))
			defer srv.Close()

			path := filepath.Join(t.TempDir(), "geoip", "GeoLite2-City.mmdb")
			d := NewLocalGeoDB(config.GeoIPConfig{CityDB: path, LicenseKey: "k3y", AccountID: tc.accountID, UpdateHours: 24}, logger.New("error"))
			d.downloadBase = srv.URL
			if d.Ready() {
				t.Fatal("ready before the database exists")
			}

			d.Refresh(context.Background())
			if geo, ok := d.Lookup("81.2.69.160"); !ok || geo.CityName != "London" {
				t.Fatalf("after download: %+v, %v", geo, ok)
			}

			// Refresh leaves a fresh database alone.
			d.Refresh(context.Background())
			if n := downloads.Load(); n != 1 {
				t.Errorf("downloads = %d, want 1", n)
			}
		})
	}
}

func TestLocalGeoDB_KeepsDatabaseWhenDownloadIsInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarGz(t, "GeoLite2-City.mmdb", []byte("not a database")))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("GB", "United Kingdom", "England", "London"),
	}), time.Now().Add(-48*time.Hour))
	d := NewLocalGeoDB(config.GeoIPConfig{CityDB: path, LicenseKey: "k3y", UpdateHours: 24}, logger.New("error"))
	d.downloadBase = srv.URL

	d.Refresh(context.Background())
	if geo, _ := d.Lookup("81.2.69.160"); geo.CountryCode != "GB" {
		t.Errorf("an invalid download replaced the working database: %+v", geo)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestGeoIPService_PrefersLocalDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, mmdbBytes(t, "GeoLite2-City", map[string]mmdbtype.Map{
		"81.2.69.0/24": cityRecord("GB", "United Kingdom", "England", "London"),
	}), time.Now())
	var requests, queries atomic.Int64
	api := stubGeoAPI(t, &requests, &queries)
	defer api.Close()

	g := geoTestService(api.URL)
	g.local = NewLocalGeoDB(config.GeoIPConfig{CityDB: path, UpdateHours: 1}, logger.New("error"))

	got := g.EnrichProxies(context.Background(), []string{"81.2.69.160:8080", "81.2.69.160:3128", "10.0.0.1:80"})
	if len(got) != 2 || got["81.2.69.160:8080"].CountryCode != "GB" || got["81.2.69.160:3128"].CountryCode != "GB" {
		t.Errorf("EnrichProxies = %+v, want both GB addresses and no private one", got)
	}
	if n := queries.Load(); n != 0 {
		t.Errorf("ip-api.com queried for %d IPs despite a loaded local database", n)
	}

	// The databases cannot place a hostname, so it goes to ip-api.com.
	got = g.EnrichProxies(context.Background(), []string{"gate.example.com:7000", "81.2.69.160:8080"})
	if got["gate.example.com:7000"].CountryCode != "US" || got["81.2.69.160:8080"].CountryCode != "GB" || queries.Load() != 1 {
		t.Errorf("EnrichProxies = %+v after %d ip-api queries, want the hostname from ip-api only", got, queries.Load())
	}
}
