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

	// JWTSecret signs dashboard session tokens. Leave empty to generate a
	// random secret on each boot (fine for single-node dev, but logs everyone
	// out on restart and cannot work behind more than one replica). Set a
	// stable value (JWT_SECRET) in production / multi-replica deployments.
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

	// Auth brute-force protection
	// Per-IP: after AuthIPMaxAttempts failures within AuthIPWindowMinutes,
	// that IP is blocked for AuthIPBlockMinutes.
	// Global: if total login attempts across all IPs exceed AuthGlobalMaxPerMinute
	// in a 1-minute window, the login endpoint is locked for AuthGlobalLockoutMin.
	AuthIPMaxAttempts      int // failed attempts before IP block       (AUTH_IP_MAX_ATTEMPTS, default 10)
	AuthIPWindowMinutes    int // sliding window to count attempts      (AUTH_IP_WINDOW_MINUTES, default 10)
	AuthIPBlockMinutes     int // how long to block an IP               (AUTH_IP_BLOCK_MINUTES, default 30)
	AuthGlobalMaxPerMinute int // max total attempts/min before lockout (AUTH_GLOBAL_MAX_PER_MINUTE, default 1000)
	AuthGlobalLockoutMin   int // global lockout duration in minutes    (AUTH_GLOBAL_LOCKOUT_MINUTES, default 1)
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

// DSN returns the database connection string
func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
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
		AdminUser: getEnv("ROTA_ADMIN_USER", "admin"),
		AdminPass: getEnv("ROTA_ADMIN_PASSWORD", "admin"),
		JWTSecret: getEnv("JWT_SECRET", ""),

		CORSAllowedOrigins: getEnvAsSlice("CORS_ALLOWED_ORIGINS", []string{"*"}),
		WebDir:             getEnv("WEB_DIR", ""),

		AuthIPMaxAttempts:      getEnvAsInt("AUTH_IP_MAX_ATTEMPTS", 10),
		AuthIPWindowMinutes:    getEnvAsInt("AUTH_IP_WINDOW_MINUTES", 10),
		AuthIPBlockMinutes:     getEnvAsInt("AUTH_IP_BLOCK_MINUTES", 30),
		AuthGlobalMaxPerMinute: getEnvAsInt("AUTH_GLOBAL_MAX_PER_MINUTE", 1000),
		AuthGlobalLockoutMin:   getEnvAsInt("AUTH_GLOBAL_LOCKOUT_MINUTES", 1),
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

	return nil
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt retrieves an environment variable as an integer or returns a default value
func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
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
