package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// authRateLimiter protects the login endpoint against brute-force attacks.
//
// Two independent mechanisms run simultaneously:
//
//  1. Per-IP limiter — tracks failed attempts per IP address within a sliding
//     window (AuthIPWindowMinutes). Once an IP exceeds AuthIPMaxAttempts failures
//     it is blocked for AuthIPBlockMinutes regardless of success/failure.
//
//  2. Global limiter — counts ALL login attempts (not just failures) across all
//     IPs within the last 60 seconds. If the count exceeds AuthGlobalMaxPerMinute
//     the endpoint is locked for AuthGlobalLockoutMin minutes for everyone.
//
// Both counters live in memory and are safe for concurrent access.
// They are intentionally NOT persisted — a restart clears them, which is fine
// since the goal is to blunt online attacks, not forensic accounting.
type authRateLimiter struct {
	mu  sync.Mutex
	log *logger.Logger

	// trustProxyHeaders honours X-Forwarded-For / X-Real-IP only when the API is
	// behind a trusted reverse proxy. Otherwise they are ignored, since a client
	// that can set them freely could present a new IP per attempt.
	trustProxyHeaders bool

	// per-IP state
	ipAttempts map[string][]time.Time // timestamps of failed attempts per IP
	ipBlocked  map[string]time.Time   // unblock time per IP

	// global state
	globalAttempts  []time.Time // timestamps of ALL attempts (last 60 s)
	globalLockUntil time.Time   // when the global lockout expires

	// config (immutable after construction)
	ipMaxAttempts   int
	ipWindow        time.Duration
	ipBlockDuration time.Duration
	globalMax       int
	globalLockout   time.Duration
}

func newAuthRateLimiter(
	ipMaxAttempts, ipWindowMin, ipBlockMin int,
	globalMax, globalLockoutMin int,
	trustProxyHeaders bool,
	log *logger.Logger,
) *authRateLimiter {
	rl := &authRateLimiter{
		log:               log,
		trustProxyHeaders: trustProxyHeaders,
		ipAttempts:        make(map[string][]time.Time),
		ipBlocked:         make(map[string]time.Time),
		ipMaxAttempts:     ipMaxAttempts,
		ipWindow:          time.Duration(ipWindowMin) * time.Minute,
		ipBlockDuration:   time.Duration(ipBlockMin) * time.Minute,
		globalMax:         globalMax,
		globalLockout:     time.Duration(globalLockoutMin) * time.Minute,
	}
	// Background cleanup every 5 minutes
	go rl.cleanup()
	return rl
}

// Middleware returns an http.Handler middleware that enforces rate limits.
// It wraps the next handler and:
//   - returns 429 immediately if the global lockout or a per-IP block is active
//   - records every attempt for the global counter
//   - records failed attempts (non-200 response) for the per-IP counter
func (rl *authRateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rl.clientIP(r)
			now := time.Now()

			rl.mu.Lock()

			// ── 1. Global lockout check ──────────────────────────────────────
			if now.Before(rl.globalLockUntil) {
				remaining := rl.globalLockUntil.Sub(now).Truncate(time.Second)
				rl.mu.Unlock()
				rl.log.Warn("auth global lockout active",
					"ip", ip,
					"remaining", remaining.String(),
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", remaining.String())
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"Login temporarily disabled due to too many requests. Try again later."}`))
				return
			}

			// ── 2. Per-IP block check ────────────────────────────────────────
			if unblockAt, blocked := rl.ipBlocked[ip]; blocked && now.Before(unblockAt) {
				remaining := unblockAt.Sub(now).Truncate(time.Second)
				rl.mu.Unlock()
				rl.log.Warn("auth per-IP block active",
					"ip", ip,
					"remaining", remaining.String(),
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", remaining.String())
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"Too many failed login attempts from your IP. Try again later."}`))
				return
			}

			// ── 3. Record attempt for global counter ─────────────────────────
			cutoff1m := now.Add(-time.Minute)
			rl.globalAttempts = filterAfter(rl.globalAttempts, cutoff1m)
			rl.globalAttempts = append(rl.globalAttempts, now)

			if len(rl.globalAttempts) > rl.globalMax {
				rl.globalLockUntil = now.Add(rl.globalLockout)
				rl.log.Warn("auth global rate limit exceeded — engaging lockout",
					"attempts_per_min", len(rl.globalAttempts),
					"limit", rl.globalMax,
					"lockout", rl.globalLockout.String(),
				)
				rl.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", rl.globalLockout.String())
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"Login temporarily disabled due to too many requests. Try again later."}`))
				return
			}

			rl.mu.Unlock()

			// ── 4. Execute the actual login handler ──────────────────────────
			ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)

			// ── 5. On failure, record per-IP attempt ─────────────────────────
			if ww.status == http.StatusUnauthorized || ww.status == http.StatusForbidden {
				rl.mu.Lock()
				cutoffWindow := now.Add(-rl.ipWindow)
				prev := filterAfter(rl.ipAttempts[ip], cutoffWindow)
				prev = append(prev, now)
				rl.ipAttempts[ip] = prev

				if len(prev) >= rl.ipMaxAttempts {
					rl.ipBlocked[ip] = now.Add(rl.ipBlockDuration)
					rl.log.Warn("auth per-IP rate limit exceeded — IP blocked",
						"ip", ip,
						"attempts", len(prev),
						"limit", rl.ipMaxAttempts,
						"block_until", rl.ipBlocked[ip].Format(time.RFC3339),
					)
				}
				rl.mu.Unlock()
			}
		})
	}
}

// cleanup removes stale entries every 5 minutes to prevent unbounded growth.
func (rl *authRateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		// Remove expired IP blocks
		for ip, unblockAt := range rl.ipBlocked {
			if now.After(unblockAt) {
				delete(rl.ipBlocked, ip)
				delete(rl.ipAttempts, ip)
			}
		}
		// Trim old per-IP attempt slices
		for ip, times := range rl.ipAttempts {
			filtered := filterAfter(times, now.Add(-rl.ipWindow))
			if len(filtered) == 0 {
				delete(rl.ipAttempts, ip)
			} else {
				rl.ipAttempts[ip] = filtered
			}
		}
		// Trim global attempts older than 1 minute
		rl.globalAttempts = filterAfter(rl.globalAttempts, now.Add(-time.Minute))
		rl.mu.Unlock()
	}
}

// filterAfter returns only timestamps that are after cutoff.
func filterAfter(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for _, t := range ts {
		if t.After(cutoff) {
			ts[i] = t
			i++
		}
	}
	return ts[:i]
}

// clientIP extracts the client IP used to key the per-IP login block.
// X-Forwarded-For / X-Real-IP are only consulted when the API is configured to
// trust an upstream reverse proxy; otherwise the request's socket peer is the
// only value a client cannot forge.
func (rl *authRateLimiter) clientIP(r *http.Request) string {
	if rl.trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// X-Forwarded-For may be "client, proxy1, proxy2" — take the first.
			if idx := strings.IndexByte(xff, ','); idx >= 0 {
				xff = xff[:idx]
			}
			if ip := net.ParseIP(strings.TrimSpace(xff)); ip != nil {
				return ip.String()
			}
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}
