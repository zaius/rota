package models

import "time"

// ProxyUser is a user that authenticates to the proxy server port (8000).
// Each user has a main pool and optional ordered fallback pools.
type ProxyUser struct {
	ID                int       `json:"id"`
	Username          string    `json:"username"`
	PasswordHash      string    `json:"-"` // bcrypt, never in JSON
	Enabled           bool      `json:"enabled"`
	MainPoolID        *int      `json:"main_pool_id,omitempty"`
	FallbackPoolIDs   []int     `json:"fallback_pool_ids"`
	MaxRetries        int       `json:"max_retries"`
	RequestsPerMinute int       `json:"requests_per_minute"` // 0 = no limit
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// Enriched fields (JOIN, not stored)
	MainPoolName string `json:"main_pool_name,omitempty"`
}

// ProxyUserWithPools is ProxyUser + full pool objects for the API
type ProxyUserWithPools struct {
	ProxyUser
	MainPool      *ProxyPool  `json:"main_pool,omitempty"`
	FallbackPools []ProxyPool `json:"fallback_pools"`
}

// CreateProxyUserRequest is the payload for POST /api/v1/proxy-users
type CreateProxyUserRequest struct {
	Username          string `json:"username"             validate:"required"`
	Password          string `json:"password"             validate:"required,min=6"`
	Enabled           bool   `json:"enabled"`
	MainPoolID        *int   `json:"main_pool_id,omitempty"`
	FallbackPoolIDs   []int  `json:"fallback_pool_ids"`
	MaxRetries        int    `json:"max_retries"           validate:"omitempty,min=1,max=50"`
	RequestsPerMinute int    `json:"requests_per_minute"` // 0 = no limit
}

// UpdateProxyUserRequest is the payload for PUT /api/v1/proxy-users/{id}.
// Partial updates: every field left out of the document keeps its current
// value — only fields explicitly present are applied.
type UpdateProxyUserRequest struct {
	Password          string          `json:"password,omitempty"`            // "" keeps
	Enabled           *bool           `json:"enabled,omitempty"`             // omitted keeps
	MainPoolID        Optional[int]   `json:"main_pool_id"`                  // omitted keeps, null clears
	FallbackPoolIDs   Optional[[]int] `json:"fallback_pool_ids"`             // omitted keeps, null/[] clears, list replaces
	MaxRetries        int             `json:"max_retries,omitempty"`         // 0 keeps
	RequestsPerMinute *int            `json:"requests_per_minute,omitempty"` // omitted keeps
}

// proxyUserContextKey is used to pass the resolved ProxyUser through request context
type proxyUserContextKey struct{}

// ProxyUserContextKey is the exported key for request context
var ProxyUserContextKey = proxyUserContextKey{}

// PoolIDs returns every pool the user is assigned to: the main pool followed
// by the fallbacks. This is the scope a proxy-user-authenticated API call may
// act on.
func (u *ProxyUser) PoolIDs() []int {
	ids := make([]int, 0, len(u.FallbackPoolIDs)+1)
	if u.MainPoolID != nil {
		ids = append(ids, *u.MainPoolID)
	}
	return append(ids, u.FallbackPoolIDs...)
}
