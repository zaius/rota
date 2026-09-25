package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all application configuration
type Config struct {
	ProxyPort int
	APIPort   int
	LogLevel  string
	Database  DatabaseConfig
	AdminUser string
	AdminPass string

	// EventStore selects the backend for request and tunnel history:
	// "postgres" (default — events live in the primary database) or
	// "clickhouse" (EVENT_STORE).
	EventStore string

	// ClickHouse is the event-store connection when EVENT_STORE=clickhouse.
	// The primary (Postgres) database is still required for control-plane
	// data either way.
	ClickHouse ClickHouseConfig

	// JWTSecret, when set, signs dashboard session tokens in place of the key
	// the server generates once and stores in the database. Set it only to
	// manage or rotate the key yourself; changing it logs every session out.
	JWTSecret string

	// CORSAllowedOrigins lists browser origins allowed to call the API.
	// Defaults to ["*"] for zero-config local development. Set explicit
	// origins (CORS_ALLOWED_ORIGINS, comma-separated) in production to lock
	// the API down; doing so also enables credentialed CORS requests.
	CORSAllowedOrigins []string

	// WebDir, if set (WEB_DIR), is a directory of built dashboard assets that the
	// API server serves at "/" with SPA fallback — so the Go binary serves both
	// the UI and the API on one port, with no separate Node/Next runtime. Empty
	// in dev, where the dashboard runs under the Vite dev server.
	WebDir string

	// TrustProxyHeaders controls whether X-Forwarded-For / X-Real-IP are used to
	// derive the client IP. Enable it only when the API sits behind a trusted
	// reverse proxy that overwrites those headers. When the API is exposed
	// directly, a client can set them freely, so trusting them would let an
	// attacker present a fresh IP on every login attempt and walk straight past
	// the per-IP block. (TRUST_PROXY_HEADERS, default false)
	TrustProxyHeaders bool

	// TLSInspect configures optional HTTPS interception (TLS_INSPECT_*).
	TLSInspect TLSInspectConfig

	// GeoIP selects local MaxMind databases for proxy geolocation in place of
	// the ip-api.com web service (GEOIP_*, MAXMIND_*).
	GeoIP GeoIPConfig

	// MetricsEnabled controls the OpenTelemetry metrics pipeline: the
	// Prometheus /metrics endpoint on the API port and, when the standard
	// OTEL_EXPORTER_OTLP_* env vars are set, OTLP push. (METRICS_ENABLED,
	// default true)
	MetricsEnabled bool

	// MetricsBearerToken, when set, requires scrapers to send
	// "Authorization: Bearer <token>" on GET /metrics. Empty leaves the
	// endpoint unauthenticated, the usual choice for internal-network
	// deployments. (METRICS_BEARER_TOKEN)
	MetricsBearerToken string

	// Failed-auth protection shares per-IP blocks across proxy and API auth.
	AuthIPMaxAttempts   int // failures before IP block (AUTH_IP_MAX_ATTEMPTS, default 10)
	AuthIPWindowMinutes int // sliding failure window (AUTH_IP_WINDOW_MINUTES, default 10)
	AuthIPBlockMinutes  int // IP block duration (AUTH_IP_BLOCK_MINUTES, default 30)
}

// DatabaseConfig holds database configuration
type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
}

// TLSInspectConfig holds the server-side half of HTTPS interception.
//
// Interception requires this AND the per-user inspect_tls flag: a CA here only
// makes interception possible, never automatic. With no CA configured, requests
// for inspection fail; other CONNECT tunnels stay opaque.
type TLSInspectConfig struct {
	// CACertFile / CAKeyFile are PEM paths for the CA that signs the
	// certificates presented to intercepted clients. Both or neither.
	CACertFile string
	CAKeyFile  string

	// BypassDomains are hosts (and their subdomains) never intercepted, for
	// targets that pin certificates or must see an untouched handshake.
	BypassDomains []string
}

// Enabled reports whether a CA keypair is configured.
func (t *TLSInspectConfig) Enabled() bool {
	return t.CACertFile != "" && t.CAKeyFile != ""
}

// GeoIPConfig points geolocation at local MaxMind databases. With no City
// database configured, lookups go to the ip-api.com web service instead.
type GeoIPConfig struct {
	// CityDB is a GeoLite2/GeoIP2 City .mmdb for country, region, city and
	// coordinates (GEOIP_CITY_DB).
	CityDB string
	// ASNDB is an optional GeoLite2 ASN .mmdb; its AS organization fills the
	// ISP that pool ISP filters match (GEOIP_ASN_DB).
	ASNDB string
	// LicenseKey enables downloading the databases from MaxMind when they are
	// missing or older than UpdateHours (MAXMIND_LICENSE_KEY). AccountID
	// selects MaxMind's authenticated download endpoint (MAXMIND_ACCOUNT_ID).
	LicenseKey string
	AccountID  string
	// UpdateHours is the download age limit (GEOIP_UPDATE_HOURS, default 168).
	UpdateHours int
}

// defaultGeoIPDir holds downloaded databases when the environment names no
// database paths.
const defaultGeoIPDir = "data/geoip"

// ClickHouseConfig holds the ClickHouse connection settings (native protocol).
type ClickHouseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
}

// DSN returns the database connection string
func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// Load reads configuration from environment variables. For runs outside
// Docker, it first reads a .env file in the working directory; variables
// already set in the environment take precedence over it.
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		ProxyPort: getEnvAsInt("PROXY_PORT", 8000),
		APIPort:   getEnvAsInt("API_PORT", 8001),
		LogLevel:  getEnv("LOG_LEVEL", "info"),
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnvAsInt("DB_PORT", 5432),
			User:     getEnv("DB_USER", "rota"),
			Password: getEnv("DB_PASSWORD", "rota_password"),
			Name:     getEnv("DB_NAME", "rota"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		AdminUser:  getEnv("ROTA_ADMIN_USER", "admin"),
		AdminPass:  getEnv("ROTA_ADMIN_PASSWORD", "admin"),
		JWTSecret:  getEnv("JWT_SECRET", ""),
		EventStore: getEnv("EVENT_STORE", "postgres"),
		ClickHouse: ClickHouseConfig{
			Host:     getEnv("CLICKHOUSE_HOST", "localhost"),
			Port:     getEnvAsInt("CLICKHOUSE_PORT", 9000),
			User:     getEnv("CLICKHOUSE_USER", "rota"),
			Password: getEnv("CLICKHOUSE_PASSWORD", ""),
			Name:     getEnv("CLICKHOUSE_DB", "rota"),
		},

		CORSAllowedOrigins: getEnvAsSlice("CORS_ALLOWED_ORIGINS", []string{"*"}),
		WebDir:             getEnv("WEB_DIR", ""),
		TrustProxyHeaders:  getEnvAsBool("TRUST_PROXY_HEADERS", false),

		TLSInspect: TLSInspectConfig{
			CACertFile:    getEnv("TLS_INSPECT_CA_CERT", ""),
			CAKeyFile:     getEnv("TLS_INSPECT_CA_KEY", ""),
			BypassDomains: getEnvAsSlice("TLS_INSPECT_BYPASS_DOMAINS", nil),
		},

		GeoIP: GeoIPConfig{
			CityDB:      getEnv("GEOIP_CITY_DB", ""),
			ASNDB:       getEnv("GEOIP_ASN_DB", ""),
			LicenseKey:  getEnv("MAXMIND_LICENSE_KEY", ""),
			AccountID:   getEnv("MAXMIND_ACCOUNT_ID", ""),
			UpdateHours: getEnvAsInt("GEOIP_UPDATE_HOURS", 168),
		},

		MetricsEnabled:     getEnvAsBool("METRICS_ENABLED", true),
		MetricsBearerToken: getEnv("METRICS_BEARER_TOKEN", ""),

		AuthIPMaxAttempts:   getEnvAsInt("AUTH_IP_MAX_ATTEMPTS", 10),
		AuthIPWindowMinutes: getEnvAsInt("AUTH_IP_WINDOW_MINUTES", 10),
		AuthIPBlockMinutes:  getEnvAsInt("AUTH_IP_BLOCK_MINUTES", 30),
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

	return cfg, nil
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

	if c.EventStore != "postgres" && c.EventStore != "clickhouse" {
		return fmt.Errorf("invalid event store: %s (must be postgres or clickhouse)", c.EventStore)
	}
	if c.AuthIPMaxAttempts < 1 || c.AuthIPWindowMinutes < 1 || c.AuthIPBlockMinutes < 1 {
		return fmt.Errorf("AUTH_IP_MAX_ATTEMPTS, AUTH_IP_WINDOW_MINUTES and AUTH_IP_BLOCK_MINUTES must be positive")
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

	return nil
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt retrieves an environment variable as an integer or returns a
// default value. A typo used to fall through to the default silently, which is
// indistinguishable from not setting the variable at all. Every caller wants a
// non-negative count or duration, so negatives are rejected too.
func getEnvAsInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	intValue, err := strconv.Atoi(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: WARNING invalid integer for %s=%q (%v); using default %d\n", key, value, err, defaultValue)
		return defaultValue
	}
	if intValue < 0 {
		fmt.Fprintf(os.Stderr, "config: WARNING negative value for %s=%d not allowed; using default %d\n", key, intValue, defaultValue)
		return defaultValue
	}
	return intValue
}

// getEnvAsBool accepts the usual truthy/falsy spellings (1/t/true/yes/on and
// their negatives, case-insensitive) and warns on anything else.
func getEnvAsBool(key string, defaultValue bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	switch strings.ToLower(value) {
	case "1", "t", "true", "yes", "on":
		return true
	case "0", "f", "false", "no", "off":
		return false
	default:
		fmt.Fprintf(os.Stderr, "config: WARNING invalid boolean for %s=%q; using default %t\n", key, value, defaultValue)
		return defaultValue
	}
}

// getEnvAsSlice retrieves a comma-separated environment variable as a string
// slice (trimming whitespace and dropping empty entries) or returns a default.
func getEnvAsSlice(key string, defaultValue []string) []string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return defaultValue
	}
	return result
}

// loadDotEnv sets KEY=VALUE pairs from path for keys the environment leaves
// unset or empty. It reads the subset of the format Docker Compose does: it
// skips blank lines and # comments, allows an "export " prefix, keeps
// everything between a quoted value's quotes, and ends an unquoted value at a
// " #" comment. It ignores a missing file.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		value = dotEnvValue(strings.TrimSpace(value))
		// Empty counts as unset, matching getEnv.
		if os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}

// dotEnvValue unquotes a .env value or strips its trailing comment.
func dotEnvValue(v string) string {
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}
