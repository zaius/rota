// Package authlimit blocks password authentication after repeated failures.
package authlimit

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type attempts struct {
	failures []time.Time
	blocked  time.Time
}

// Limiter shares per-IP failure counts across authentication entry points.
// Successful requests never consume a budget. An active block prevents further
// password checks from that IP until it expires. A nil limiter allows requests.
type Limiter struct {
	mu        sync.Mutex
	entries   map[string]attempts
	max       int
	window    time.Duration
	block     time.Duration
	now       func() time.Time
	nextSweep time.Time
}

func New(maxFailures int, window, block time.Duration) *Limiter {
	return &Limiter{entries: make(map[string]attempts), max: maxFailures, window: window, block: block, now: time.Now}
}

// clientIP uses the socket peer. The API may rewrite RemoteAddr through its
// trusted-proxy middleware; the forward proxy never trusts forwarded headers.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RetryAfter returns the remaining block in whole seconds, or zero.
func (l *Limiter) RetryAfter(r *http.Request) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := l.entries[clientIP(r)].blocked.Sub(l.now())
	return max(0, int(math.Ceil(remaining.Seconds())))
}

// Failed records only rejected credentials, never authorization or server errors.
func (l *Limiter) Failed(r *http.Request) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	if !now.Before(l.nextSweep) {
		for ip, a := range l.entries {
			if !a.blocked.IsZero() {
				if !now.Before(a.blocked) {
					delete(l.entries, ip)
				}
			} else if len(a.failures) == 0 || !a.failures[len(a.failures)-1].After(cutoff) {
				delete(l.entries, ip)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}
	ip := clientIP(r)
	a := l.entries[ip]
	if now.Before(a.blocked) {
		return // In-flight failures must not extend an existing block.
	}
	if !a.blocked.IsZero() {
		a = attempts{}
	}
	live := a.failures[:0]
	for _, t := range a.failures {
		if t.After(cutoff) {
			live = append(live, t)
		}
	}
	a.failures = append(live, now)
	if len(a.failures) >= l.max {
		a.blocked = now.Add(l.block)
		a.failures = nil
	}
	l.entries[ip] = a
}

// Allow rejects blocked API authentication attempts before credential checks.
func (l *Limiter) Allow(w http.ResponseWriter, r *http.Request) bool {
	if seconds := l.RetryAfter(r); seconds > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		http.Error(w, `{"error":"Too many failed authentication attempts from your IP. Try again later."}`, http.StatusTooManyRequests)
		return false
	}
	return true
}

// Login wraps the password-login handler; only its 401 responses count as failures.
func (l *Limiter) Login(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(w, r) {
			return
		}
		ww := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(ww, r)
		if ww.status == http.StatusUnauthorized {
			l.Failed(r)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
