package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config holds all application configuration
type Config struct {
	ProxyPort int    `env:"PROXY_PORT" envDefault:"8000"`
	APIPort   int    `env:"API_PORT" envDefault:"8001"`
	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	Database  DatabaseConfig

	// Password guards the proxy port, the API and the dashboard.
	Password string `env:"ROTA_PASSWORD,notEmpty"`

	// Parquet configures the file-based store for request and tunnel history.
	Parquet ParquetConfig

	// CORSAllowedOrigins lists browser origins allowed to call the API. The
	// wildcard default suits local development; explicit origins lock the API
	// down and also enable credentialed CORS requests.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envDefault:"*"`

	// WebDir is a directory of built dashboard assets that the API server
	// serves at "/" with SPA fallback, so the Go binary serves both the UI and
	// the API on one port. It defaults to a "web" directory beside the
	// executable, and is empty in dev, where the dashboard runs under the Vite
	// dev server.
	WebDir string `env:"WEB_DIR"`

	// TLSInspect configures optional HTTPS interception.
	TLSInspect TLSInspectConfig

	// GeoIP selects local MaxMind databases for proxy geolocation in place of
	// the ip-api.com web service.
	GeoIP GeoIPConfig

	// MetricsEnabled controls the OpenTelemetry metrics pipeline: the
	// Prometheus /metrics endpoint on the API port and, when the standard
	// OTEL_EXPORTER_OTLP_* env vars are set, OTLP push.
	MetricsEnabled bool `env:"METRICS_ENABLED" envDefault:"true"`

	// MetricsBearerToken, when set, requires scrapers to send
	// "Authorization: Bearer <token>" on GET /metrics. Empty leaves the
	// endpoint unauthenticated, the usual choice for internal-network
	// deployments.
	MetricsBearerToken string `env:"METRICS_BEARER_TOKEN"`
}

// DatabaseConfig holds database configuration
type DatabaseConfig struct {
	Host     string `env:"DB_HOST" envDefault:"localhost"`
	Port     int    `env:"DB_PORT" envDefault:"5432"`
	User     string `env:"DB_USER" envDefault:"rota"`
	Password string `env:"DB_PASSWORD" envDefault:"rota_password"`
	Name     string `env:"DB_NAME" envDefault:"rota"`
	SSLMode  string `env:"DB_SSLMODE" envDefault:"disable"`
}

// TLSInspectConfig holds the server-side half of HTTPS interception.
//
// Interception requires this AND a profile option in the proxy username: a CA
// here only makes interception possible, never automatic. With no CA
// configured, requests for inspection fail; other CONNECT tunnels stay opaque.
type TLSInspectConfig struct {
	// CACertFile / CAKeyFile are PEM paths for the CA that signs the
	// certificates presented to intercepted clients. Both or neither.
	CACertFile string `env:"TLS_INSPECT_CA_CERT"`
	CAKeyFile  string `env:"TLS_INSPECT_CA_KEY"`

	// BypassDomains are hosts (and their subdomains) never intercepted, for
	// targets that pin certificates or must see an untouched handshake.
	BypassDomains []string `env:"TLS_INSPECT_BYPASS_DOMAINS"`
}

// Enabled reports whether a CA keypair is configured.
func (t *TLSInspectConfig) Enabled() bool {
	return t.CACertFile != "" && t.CAKeyFile != ""
}

// GeoIPConfig points geolocation at local MaxMind databases. With no City
// database configured, lookups go to the ip-api.com web service instead.
type GeoIPConfig struct {
	// CityDB is a GeoLite2/GeoIP2 City .mmdb for country, region, city and
	// coordinates.
	CityDB string `env:"GEOIP_CITY_DB"`
	// ASNDB is an optional GeoLite2 ASN .mmdb; its AS organization fills the
	// proxy's ISP.
	ASNDB string `env:"GEOIP_ASN_DB"`
	// LicenseKey enables downloading the databases from MaxMind when they are
	// missing or older than UpdateHours. AccountID selects MaxMind's
	// authenticated download endpoint.
	LicenseKey string `env:"MAXMIND_LICENSE_KEY"`
	AccountID  string `env:"MAXMIND_ACCOUNT_ID"`
	// UpdateHours is the download age limit.
	UpdateHours int `env:"GEOIP_UPDATE_HOURS" envDefault:"168"`
}

// defaultGeoIPDir holds downloaded databases when the environment names no
// database paths.
const defaultGeoIPDir = "data/geoip"

// eventInstancePattern keeps an instance name safe in a file name on every
// platform. It has no dots, which separate the name from the day and the
// extension.
var eventInstancePattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidEventInstance reports whether name can name event store files.
func ValidEventInstance(name string) bool {
	return eventInstancePattern.MatchString(name)
}

// checkEventStore fails on settings that ask for history anywhere but the
// Parquet files, so a stale setting stops startup instead of being ignored.
func checkEventStore() error {
	if store := os.Getenv("EVENT_STORE"); store != "" && store != "parquet" {
		return fmt.Errorf("EVENT_STORE=%s is not supported: request and tunnel history lives in Parquet files; unset EVENT_STORE", store)
	}
	if os.Getenv("EVENT_STORE_MIRROR") != "" {
		return fmt.Errorf("EVENT_STORE_MIRROR is not supported: request and tunnel history lives only in Parquet files; unset EVENT_STORE_MIRROR")
	}
	return nil
}

// ParquetConfig holds the settings of the file-based event store.
type ParquetConfig struct {
	// Dir holds the event logs and Parquet files. It must outlive the
	// process, because the current day exists only here. The default is
	// /app/data/events in the container, on the same volume as the GeoIP
	// databases.
	Dir string `env:"EVENT_STORE_DIR" envDefault:"data/events"`

	// Instance names the files this process writes in Dir. The store converts
	// only logs with its own name, so the name must stay the same across
	// restarts; the default is fixed rather than the hostname, which changes
	// when a container is recreated.
	Instance string `env:"EVENT_STORE_INSTANCE" envDefault:"rota"`

	// SyncWrites makes every append wait for the disk, so events survive
	// power loss at the cost of write latency.
	SyncWrites bool `env:"EVENT_STORE_SYNC"`
}

// DSN returns the connection URL, which escapes credentials that a key=value
// string would need quoted.
func (d *DatabaseConfig) DSN() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(d.User, d.Password),
		Host:     net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:     d.Name,
		RawQuery: url.Values{"sslmode": {d.SSLMode}}.Encode(),
	}
	return u.String()
}

// Load reads configuration from environment variables. For runs outside
// Docker, it first reads a .env file in the working directory; variables
// already set in the environment take precedence over it.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read .env: %w", err)
	}

	if err := checkEventStore(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.WebDir == "" {
		cfg.WebDir = defaultWebDir()
	}

	// A license key alone is enough: download both databases to the default
	// location.
	if cfg.GeoIP.LicenseKey != "" {
		if cfg.GeoIP.CityDB == "" {
			cfg.GeoIP.CityDB = defaultGeoIPDir + "/GeoLite2-City.mmdb"
		}
		if cfg.GeoIP.ASNDB == "" {
			cfg.GeoIP.ASNDB = defaultGeoIPDir + "/GeoLite2-ASN.mmdb"
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return fmt.Errorf("invalid proxy port: %d", c.ProxyPort)
	}
	if c.APIPort < 1 || c.APIPort > 65535 {
		return fmt.Errorf("invalid API port: %d", c.APIPort)
	}
	if c.ProxyPort == c.APIPort {
		return fmt.Errorf("proxy port and API port cannot be the same: %d", c.ProxyPort)
	}

	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	// Half a keypair means someone intended to enable interception and it
	// would silently stay off — worth failing over rather than discovering it
	// through absent data.
	if (c.TLSInspect.CACertFile == "") != (c.TLSInspect.CAKeyFile == "") {
		return fmt.Errorf("TLS_INSPECT_CA_CERT and TLS_INSPECT_CA_KEY must be set together")
	}

	// An ASN database only adds ISP names to City lookups.
	if c.GeoIP.ASNDB != "" && c.GeoIP.CityDB == "" {
		return fmt.Errorf("GEOIP_ASN_DB requires GEOIP_CITY_DB")
	}
	if c.GeoIP.UpdateHours < 1 {
		return fmt.Errorf("GEOIP_UPDATE_HOURS must be positive")
	}

	if !ValidEventInstance(c.Parquet.Instance) {
		return fmt.Errorf("invalid EVENT_STORE_INSTANCE %q (must be 1-64 lowercase letters, digits, hyphens or underscores)", c.Parquet.Instance)
	}

	return nil
}

// defaultWebDir returns the "web" directory beside the executable when there
// is one — where release archives put the dashboard — so a standalone binary
// serves it without configuration.
func defaultWebDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Join(filepath.Dir(exe), "web")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}
