package proxy

import (
	"net/http"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

// ── RateLimitMiddleware ─────────────────────────────────────────────────────

func TestRateLimitMiddleware_Disabled(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: false})
	req, _ := http.NewRequest("GET", "http://example.com", nil)

	_, resp := m.HandleRequest(req)
	if resp != nil {
		t.Fatalf("expected nil reject, got status %d", resp.StatusCode)
	}
}

func TestRateLimitMiddleware_AllowsUnderLimit(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{
		Enabled:     true,
		Interval:    1,   // 1 second
		MaxRequests: 100, // 100 per second
	})

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	// First 10 requests should pass easily
	for i := 0; i < 10; i++ {
		_, resp := m.HandleRequest(req)
		if resp != nil {
			t.Fatalf("request %d should be allowed, got status %d", i, resp.StatusCode)
		}
	}
}

func TestRateLimitMiddleware_BlocksOverLimit(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{
		Enabled:     true,
		Interval:    1, // 1 second
		MaxRequests: 5, // only 5 burst
	})

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "10.0.0.1:12345"

	// Exhaust the burst
	for i := 0; i < 5; i++ {
		m.HandleRequest(req)
	}

	// Next request should be blocked
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %v", resp)
	}
}

func TestRateLimitMiddleware_PerIP(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{
		Enabled:     true,
		Interval:    1,
		MaxRequests: 2, // tiny burst
	})

	// Exhaust limit for IP1
	req1, _ := http.NewRequest("GET", "http://example.com", nil)
	req1.RemoteAddr = "1.1.1.1:1111"
	m.HandleRequest(req1)
	m.HandleRequest(req1)
	_, resp1 := m.HandleRequest(req1)
	if resp1 == nil || resp1.StatusCode != http.StatusTooManyRequests {
		t.Fatal("IP1 should be rate-limited")
	}

	// IP2 should still be allowed
	req2, _ := http.NewRequest("GET", "http://example.com", nil)
	req2.RemoteAddr = "2.2.2.2:2222"
	_, resp2 := m.HandleRequest(req2)
	if resp2 != nil {
		t.Fatal("IP2 should not be rate-limited")
	}
}

func TestRateLimitMiddleware_Cleanup(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{
		Enabled:     true,
		Interval:    1,
		MaxRequests: 100,
	})

	// Add many IPs
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		m.HandleRequest(req)
	}

	// Cleanup should not panic or error
	m.CleanupLimiters()
}

// ── RateLimitMiddleware client IP keying ────────────────────────────────────

func TestRateLimit_GetClientIPIgnoresForwardedHeaders(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: true, Interval: 60, MaxRequests: 10})
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	if got := m.getClientIP(req); got != "203.0.113.7" {
		t.Fatalf("expected the socket peer to key the limiter, got %q", got)
	}
}

func TestRateLimit_GetClientIPHandlesIPv6(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: true, Interval: 60, MaxRequests: 10})
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "[2001:db8::1]:54321"

	if got := m.getClientIP(req); got != "2001:db8::1" {
		t.Fatalf("expected the full IPv6 address, got %q", got)
	}
}

func TestRateLimit_GetClientIPWithoutPort(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: true, Interval: 60, MaxRequests: 10})
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "203.0.113.7"

	if got := m.getClientIP(req); got != "203.0.113.7" {
		t.Fatalf("expected the raw address when there is no port, got %q", got)
	}
}

// A spoofed X-Forwarded-For must not buy a fresh bucket: every request from the
// same peer shares one limiter regardless of the header.
func TestRateLimit_SpoofedForwardedForCannotBypassLimit(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: true, Interval: 60, MaxRequests: 2})

	var lastResp *http.Response
	for i := range 5 {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		req.RemoteAddr = "203.0.113.7:54321"
		req.Header.Set("X-Forwarded-For", "10.0.0."+string(rune('1'+i)))
		_, lastResp = m.HandleRequest(req)
	}

	if lastResp == nil || lastResp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("expected the limiter to reject once the burst is exhausted despite rotating X-Forwarded-For")
	}
}

func TestRateLimit_MisconfiguredLimiterAllows(t *testing.T) {
	m := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: true, Interval: 0, MaxRequests: 0})
	if !m.allow("203.0.113.7") {
		t.Fatal("expected a misconfigured limiter to allow the request rather than deny everything")
	}
}
