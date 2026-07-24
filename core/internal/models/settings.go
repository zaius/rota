package models

import "time"

// Settings represents system configuration.
// Proxy server authentication is per-user only (proxy_users); there is no
// global shared credential setting.
type Settings struct {
	Rotation     RotationSettings     `json:"rotation"`
	RateLimit    RateLimitSettings    `json:"rate_limit"`
	HealthCheck  HealthCheckSettings  `json:"healthcheck"`
	LogRetention LogRetentionSettings `json:"log_retention"`
	ProxyCleanup ProxyCleanupSettings `json:"proxy_cleanup"`
}

// RotationSettings represents proxy rotation configuration
type RotationSettings struct {
	Method             string            `json:"method"`
	TimeBased          TimeBasedSettings `json:"time_based,omitempty"`
	RemoveUnhealthy    bool              `json:"remove_unhealthy"`
	Fallback           bool              `json:"fallback"`
	FallbackMaxRetries int               `json:"fallback_max_retries"`
	FollowRedirect     bool              `json:"follow_redirect"`
	Timeout            int               `json:"timeout"`
	Retries            int               `json:"retries"`
	AllowedProtocols   []string          `json:"allowed_protocols"` // ["http", "https", "socks4", "socks4a", "socks5"], empty means all
	MaxResponseTime    int               `json:"max_response_time"` // in milliseconds, 0 means no limit
	MinSuccessRate     float64           `json:"min_success_rate"`  // 0-100, 0 means no minimum
}

// TimeBasedSettings represents time-based rotation settings
type TimeBasedSettings struct {
	Interval int `json:"interval"` // in seconds
}

// RateLimitSettings represents rate limiting configuration
type RateLimitSettings struct {
	Enabled     bool `json:"enabled"`
	Interval    int  `json:"interval"` // in seconds
	MaxRequests int  `json:"max_requests"`
}

// HealthCheckSettings represents health check configuration
type HealthCheckSettings struct {
	Timeout int      `json:"timeout"`
	Workers int      `json:"workers"`
	URL     string   `json:"url"`
	Status  int      `json:"status"`
	Headers []string `json:"headers"`
}

// LogRetentionSettings represents log retention and cleanup configuration
type LogRetentionSettings struct {
	Enabled              bool `json:"enabled"`                // Enable automatic log cleanup
	RetentionDays        int  `json:"retention_days"`         // Days to keep logs (7, 15, 30, 60, 90)
	CompressionAfterDays int  `json:"compression_after_days"` // Compress logs older than X days (1, 3, 7, 14)
	CleanupIntervalHours int  `json:"cleanup_interval_hours"` // How often to run cleanup (1, 6, 12, 24)
}

// ProxyCleanupSettings represents dead proxy auto-removal configuration
type ProxyCleanupSettings struct {
	Enabled              bool    `json:"enabled"`                // Enable automatic dead proxy cleanup
	MaxFailedDays        int     `json:"max_failed_days"`        // Remove proxies failed for more than N days
	MinSuccessRate       float64 `json:"min_success_rate"`       // Remove proxies with success rate below X% (0 = disabled)
	CleanupIntervalHours int     `json:"cleanup_interval_hours"` // How often to run cleanup
}

// SettingRecord represents a settings database record
type SettingRecord struct {
	Key       string         `json:"key"`
	Value     map[string]any `json:"value"`
	UpdatedAt time.Time      `json:"updated_at"`
}
