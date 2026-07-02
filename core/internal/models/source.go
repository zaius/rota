package models

import "time"

// Line formats a proxy source file can use. Auto covers host:port,
// user:pass@host:port and scheme://… lines; the explicit formats exist for
// lists that put credentials in positions auto-detection can't distinguish
// (e.g. Webshare downloads use host:port:user:pass).
const (
	SourceFormatAuto             = "auto"
	SourceFormatHostPortUserPass = "host:port:user:pass"
	SourceFormatUserPassHostPort = "user:pass:host:port"
	SourceFormatHostPortAtAuth   = "host:port@user:pass"
)

// ValidSourceFormats is the set of accepted values for ProxySource.Format.
var ValidSourceFormats = map[string]bool{
	SourceFormatAuto:             true,
	SourceFormatHostPortUserPass: true,
	SourceFormatUserPassHostPort: true,
	SourceFormatHostPortAtAuth:   true,
}

// ProxySource represents a remote URL that provides a list of proxies
type ProxySource struct {
	ID              int        `json:"id"`
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Protocol        string     `json:"protocol"`
	Format          string     `json:"format"`
	Enabled         bool       `json:"enabled"`
	IntervalMinutes int        `json:"interval_minutes"`
	LastFetchedAt   *time.Time `json:"last_fetched_at,omitempty"`
	LastCount       int        `json:"last_count"`        // newly imported on last fetch
	LastTotal       int        `json:"last_total"`        // total lines returned on last fetch
	LastError       *string    `json:"last_error,omitempty"`
	CleanupEnabled  bool       `json:"cleanup_enabled"`   // per-source opt-in soft cleanup
	CleanupDays     int        `json:"cleanup_days"`      // delete proxies stale for this many days
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// CreateProxySourceRequest is the payload for creating a source
type CreateProxySourceRequest struct {
	Name            string `json:"name"     validate:"required"`
	URL             string `json:"url"      validate:"required,url"`
	Protocol        string `json:"protocol" validate:"required,oneof=http https socks4 socks4a socks5"`
	Format          string `json:"format"   validate:"omitempty,oneof=auto host:port:user:pass user:pass:host:port host:port@user:pass"`
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes" validate:"min=1"`
	CleanupEnabled  bool   `json:"cleanup_enabled"`
	CleanupDays     int    `json:"cleanup_days" validate:"omitempty,min=1,max=365"`
}

// UpdateProxySourceRequest is the payload for updating a source
type UpdateProxySourceRequest struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	Protocol        string `json:"protocol" validate:"omitempty,oneof=http https socks4 socks4a socks5"`
	Format          string `json:"format"   validate:"omitempty,oneof=auto host:port:user:pass user:pass:host:port host:port@user:pass"`
	Enabled         *bool  `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes" validate:"omitempty,min=1"`
	CleanupEnabled  *bool  `json:"cleanup_enabled"`
	CleanupDays     int    `json:"cleanup_days" validate:"omitempty,min=1,max=365"`
}
