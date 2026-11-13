package proxy

import (
	"context"
	"time"
)

// beginScopeRecovery marks the first post-cooldown request locally. Only that
// request needs a database write; a refresh restores the persisted deadline.
func (m *SessionManager) beginScopeRecovery(proxyID int, scope string, start, after time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := reservationKey{proxyID, scope}
	c, ok := m.cooldowns[key]
	if !ok || c.Invalid || c.FailureCount == 0 || c.CooldownUntil.After(start) || c.RecoveryAfter != nil {
		return false
	}
	c.RecoveryAfter = &after
	m.cooldowns[key] = c
	return true
}

func (m *DomainCooldownManager) beginDomainRecovery(proxyID int, host string, start, after time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var domains []string
	for domain, c := range m.entries[proxyID] {
		if !hostMatchesDomain(host, domain) || c.Invalid || c.FailureCount == 0 || c.CooldownUntil.After(start) || c.RecoveryAfter != nil {
			continue
		}
		c.RecoveryAfter = &after
		m.entries[proxyID][domain] = c
		domains = append(domains, domain)
	}
	return domains
}

// startCooldownRecovery observes a session's use of a proxy, independently of
// response codes. The client has one session TTL to invalidate this attempt
// before the next invalidation starts a new streak. No post-cooldown use means
// no reset.
func (r *Rotator) startCooldownRecovery(ctx context.Context, proxyID int) {
	rt := routeFrom(ctx)
	key := rt.sessionKey()
	if r.proxyRepo == nil || key.token == "" {
		return
	}
	start := time.Now()
	after := start.Add(r.sessionTTL())
	scopeStarted := r.sessions.beginScopeRecovery(proxyID, key.scope, start, after)
	domains := r.domainCD.beginDomainRecovery(proxyID, rt.host, start, after)
	if !scopeStarted && len(domains) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		record := func(target string, domain bool) {
			if err := r.proxyRepo.StartCooldownRecovery(ctx, proxyID, target, domain, start, after); err != nil {
				r.logger.Error("failed to start proxy backoff recovery", "proxy_id", proxyID, "target", target, "error", err)
			}
		}
		if scopeStarted {
			record(key.scope, false)
		}
		for _, domain := range domains {
			record(domain, true)
		}
	}()
}
