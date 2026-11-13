package models

import "time"

// Settings represents system configuration.
type Settings struct {
	Rotation     RotationSettings     `json:"rotation"`
	HealthCheck  HealthCheckSettings  `json:"healthcheck"`
	ProxyCleanup ProxyCleanupSettings `json:"proxy_cleanup"`
}

// RotationSettings holds the per-request behaviour of the proxy port.
type RotationSettings struct {
	FollowRedirect bool `json:"follow_redirect"`
	Timeout        int  `json:"timeout"` // upstream request timeout in seconds
	// SessionTTLMinutes is how long an idle session keeps its proxy, and the
	// grace period after a cooldown before a session's use resets its backoff.
	SessionTTLMinutes int `json:"session_ttl_minutes"`
}

// HealthCheckSettings represents health check configuration
type HealthCheckSettings struct {
	// IntervalMinutes is how often every proxy is checked.
	IntervalMinutes int      `json:"interval_minutes"`
	Timeout         int      `json:"timeout"`
	Workers         int      `json:"workers"`
	URL             string   `json:"url"`
	Status          int      `json:"status"`
	Headers         []string `json:"headers"`
	// StrictTLS verifies the check target's certificate, so a proxy that
	// intercepts TLS with an expired or forged certificate fails its check.
	StrictTLS bool `json:"strict_tls"`
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
