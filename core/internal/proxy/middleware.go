package proxy

import (
	"net"
	"net/http"
	"sync"

	"github.com/alpkeskin/rota/core/internal/models"
	"golang.org/x/time/rate"
)

// RateLimitMiddleware handles per-IP rate limiting
type RateLimitMiddleware struct {
	enabled     bool
	interval    int // seconds
	maxRequests int
	limiters    map[string]*rate.Limiter
	mu          sync.RWMutex
}

// NewRateLimitMiddleware creates a new rate limiting middleware
func NewRateLimitMiddleware(settings models.RateLimitSettings) *RateLimitMiddleware {
	return &RateLimitMiddleware{
		enabled:     settings.Enabled,
		interval:    settings.Interval,
		maxRequests: settings.MaxRequests,
		limiters:    make(map[string]*rate.Limiter),
	}
}

// UpdateSettings updates the rate limit settings
func (m *RateLimitMiddleware) UpdateSettings(settings models.RateLimitSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.enabled = settings.Enabled
	m.interval = settings.Interval
	m.maxRequests = settings.MaxRequests

	// Clear existing limiters to apply new settings
	m.limiters = make(map[string]*rate.Limiter)
}

// HandleRequest validates rate limits for HTTP requests
func (m *RateLimitMiddleware) HandleRequest(req *http.Request) (*http.Request, *http.Response) {
	if !m.enabled {
		return req, nil
	}

	// Get client IP
	clientIP := m.getClientIP(req)

	// Check rate limit
	if !m.allow(clientIP) {
		return req, m.tooManyRequests()
	}

	return req, nil
}

// HandleConnect validates rate limits for HTTPS CONNECT requests
func (m *RateLimitMiddleware) HandleConnect(req *http.Request) (*http.Request, *http.Response) {
	return m.HandleRequest(req)
}

// allow checks if the request is allowed based on rate limiting
func (m *RateLimitMiddleware) allow(clientIP string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A non-positive interval would make rps +Inf and a non-positive maxRequests
	// would set the burst to 0, denying everything. Treat either as "limiter
	// misconfigured, effectively off" rather than silently breaking the proxy.
	if m.interval <= 0 || m.maxRequests <= 0 {
		return true
	}

	// Get or create limiter for this IP
	limiter, exists := m.limiters[clientIP]
	if !exists {
		// Create new limiter: maxRequests per interval seconds
		// Convert to requests per second
		rps := float64(m.maxRequests) / float64(m.interval)
		limiter = rate.NewLimiter(rate.Limit(rps), m.maxRequests)
		m.limiters[clientIP] = limiter
	}

	return limiter.Allow()
}

// getClientIP extracts the client IP used as the per-IP rate-limit key.
//
// X-Forwarded-For and X-Real-IP are supplied by the caller. On a forward proxy
// the caller is the client being limited, so honouring those headers would let
// it rotate the header value and sidestep the limit entirely. The socket peer
// is the only address it cannot choose.
func (m *RateLimitMiddleware) getClientIP(req *http.Request) string {
	// SplitHostPort rather than a trailing-colon search, so a bare IPv6 address
	// isn't truncated at its last hextet.
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// tooManyRequests returns a 429 Too Many Requests response
func (m *RateLimitMiddleware) tooManyRequests() *http.Response {
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
}

// CleanupLimiters removes limiters for IPs that haven't been seen recently
// Should be called periodically to prevent memory leaks
func (m *RateLimitMiddleware) CleanupLimiters() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Simple cleanup: clear all limiters
	// In production, you might want to track last access time and only remove stale entries
	if len(m.limiters) > 10000 {
		m.limiters = make(map[string]*rate.Limiter)
	}
}
