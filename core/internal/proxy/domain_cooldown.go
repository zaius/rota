package proxy

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// targetHostKey is the context key that carries the normalized target host of
// the current proxy request (e.g. "foo.com"), set by the proxy router.
type targetHostKey struct{}

// TargetHostContextKey is exported for use in the handler / pool selector.
var TargetHostContextKey = targetHostKey{}

// DomainCooldownManager holds per-domain proxy invalidations for the
// "invalidate for one target domain only" feature: a proxy on a domain
// cooldown is skipped for requests to that domain (and its subdomains) but
// stays in rotation for every other target.
//
// Like SessionManager it is a process-wide singleton. Entries are persisted in
// proxy_domain_cooldowns by the API layer so they survive restarts; this
// manager is the live view consulted on the request hot path.
type DomainCooldownManager struct {
	mu      sync.RWMutex
	entries map[int]map[string]domainCooldownEntry // proxyID → domain → entry
	stop    chan struct{}
}

type domainCooldownEntry struct {
	until  time.Time
	reason string
}

// NewDomainCooldownManager creates a DomainCooldownManager and starts its
// expiry reaper.
func NewDomainCooldownManager() *DomainCooldownManager {
	m := &DomainCooldownManager{
		entries: make(map[int]map[string]domainCooldownEntry),
		stop:    make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

// Set puts (or refreshes) a domain cooldown for a proxy. domain must already
// be normalized (see NormalizeCooldownDomain).
func (m *DomainCooldownManager) Set(proxyID int, domain string, until time.Time, reason string) {
	if domain == "" || !until.After(time.Now()) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	byDomain, ok := m.entries[proxyID]
	if !ok {
		byDomain = make(map[string]domainCooldownEntry)
		m.entries[proxyID] = byDomain
	}
	byDomain[domain] = domainCooldownEntry{until: until, reason: reason}
}

// ReplaceAll atomically swaps the entire cooldown set for a fresh snapshot,
// typically one just reloaded from the DB. Expired or empty-domain entries are
// dropped. This keeps the in-memory view eventually consistent with the table
// (and so with cooldowns set by other instances sharing the same DB), mirroring
// how the proxy selectors fully refresh their lists from the DB on a tick.
func (m *DomainCooldownManager) ReplaceAll(cooldowns []models.ProxyDomainCooldown) {
	now := time.Now()
	next := make(map[int]map[string]domainCooldownEntry)
	for _, c := range cooldowns {
		if c.Domain == "" || !c.CooldownUntil.After(now) {
			continue
		}
		reason := ""
		if c.Reason != nil {
			reason = *c.Reason
		}
		byDomain, ok := next[c.ProxyID]
		if !ok {
			byDomain = make(map[string]domainCooldownEntry)
			next[c.ProxyID] = byDomain
		}
		byDomain[c.Domain] = domainCooldownEntry{until: c.CooldownUntil, reason: reason}
	}
	m.mu.Lock()
	m.entries = next
	m.mu.Unlock()
}

// Clear removes a single (proxy, domain) cooldown. Returns true if one existed.
func (m *DomainCooldownManager) Clear(proxyID int, domain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	byDomain, ok := m.entries[proxyID]
	if !ok {
		return false
	}
	if _, ok := byDomain[domain]; !ok {
		return false
	}
	delete(byDomain, domain)
	if len(byDomain) == 0 {
		delete(m.entries, proxyID)
	}
	return true
}

// ClearProxy removes every domain cooldown for a proxy. Returns the count removed.
func (m *DomainCooldownManager) ClearProxy(proxyID int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.entries[proxyID])
	delete(m.entries, proxyID)
	return n
}

// IsCooled reports whether the proxy is on an active cooldown that covers
// host. host must already be normalized (lowercase, no port) — the proxy
// router does this once per request before it lands in the context.
func (m *DomainCooldownManager) IsCooled(proxyID int, host string) bool {
	if host == "" {
		return false
	}
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	byDomain, ok := m.entries[proxyID]
	if !ok {
		return false
	}
	for domain, e := range byDomain {
		if e.until.After(now) && hostMatchesDomain(host, domain) {
			return true
		}
	}
	return false
}

// List returns a snapshot of all active domain cooldowns.
func (m *DomainCooldownManager) List() []models.ProxyDomainCooldown {
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]models.ProxyDomainCooldown, 0)
	for proxyID, byDomain := range m.entries {
		for domain, e := range byDomain {
			if !e.until.After(now) {
				continue
			}
			c := models.ProxyDomainCooldown{
				ProxyID:       proxyID,
				Domain:        domain,
				CooldownUntil: e.until,
			}
			if e.reason != "" {
				reason := e.reason
				c.Reason = &reason
			}
			out = append(out, c)
		}
	}
	return out
}

// reapLoop periodically drops expired cooldowns so the maps don't grow.
// Expired entries are already ignored by IsCooled, so this is housekeeping only.
func (m *DomainCooldownManager) reapLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			m.mu.Lock()
			for proxyID, byDomain := range m.entries {
				for domain, e := range byDomain {
					if !e.until.After(now) {
						delete(byDomain, domain)
					}
				}
				if len(byDomain) == 0 {
					delete(m.entries, proxyID)
				}
			}
			m.mu.Unlock()
		case <-m.stop:
			return
		}
	}
}

// Stop terminates the reaper goroutine.
func (m *DomainCooldownManager) Stop() {
	close(m.stop)
}

// hostMatchesDomain reports whether a (normalized) request host falls under a
// (normalized) cooldown domain: "foo.com" covers "foo.com" and "*.foo.com".
func hostMatchesDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// normalizeHost lowercases a request host and strips any port, brackets and
// trailing dot, e.g. "Foo.com:443" → "foo.com".
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimSuffix(host, ".")
}

// NormalizeCooldownDomain canonicalizes a user-supplied invalidation domain.
// It tolerates schemes, paths, ports and wildcard prefixes: "https://www.Foo.com/x",
// "*.foo.com" and "foo.com:443" all normalize to a plain registrable host
// ("www.foo.com" / "foo.com"). Returns "" if nothing usable remains.
func NormalizeCooldownDomain(raw string) string {
	d := strings.TrimSpace(raw)
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	d = normalizeHost(d)
	d = strings.TrimPrefix(d, "*.")
	return strings.Trim(d, ".")
}

// requestTargetHost extracts the target host of a proxy request: the CONNECT
// authority for tunnels, the absolute-URI host for plain HTTP forwards.
func requestTargetHost(method, urlHost, reqHost string) string {
	if method == "CONNECT" || urlHost == "" {
		return reqHost
	}
	return urlHost
}
