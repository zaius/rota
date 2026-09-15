package proxy

import (
	"context"
	"time"
)

type cooldownRecoveryRepository interface {
	StartCooldownRecovery(context.Context, int, string, bool, time.Time, time.Time) error
}

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

// startCooldownRecovery observes use, independently of response codes. The
// client has one session idle-expiry period to invalidate this attempt before
// the next invalidation starts a new streak. No post-cooldown use means no reset.
func (c *PoolChain) startCooldownRecovery(ctx context.Context, proxyID, selIdx int) {
	if c.recoveryRepo == nil {
		return
	}
	token, _ := ctx.Value(SessionTokenContextKey).(string)
	sel := c.selectors[selIdx]
	if token == "" || (sel.method != "session" && c.selectors[0].method != "session") {
		return
	}
	host, _ := ctx.Value(TargetHostContextKey).(string)
	scope, _ := ctx.Value(SessionScopeContextKey).(string)
	if scope == "" {
		scope = normalizeHost(host)
	}
	ttl := sel.sessionTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	start := time.Now()
	after := start.Add(ttl)
	scopeStarted := sel.sessionMgr != nil && sel.sessionMgr.beginScopeRecovery(proxyID, scope, start, after)
	var domains []string
	if sel.domainCD != nil {
		domains = sel.domainCD.beginDomainRecovery(proxyID, host, start, after)
	}
	if !scopeStarted && len(domains) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		record := func(target string, domain bool) {
			if err := c.recoveryRepo.StartCooldownRecovery(ctx, proxyID, target, domain, start, after); err != nil {
				c.logger.Error("failed to start proxy backoff recovery", "proxy_id", proxyID, "target", target, "error", err)
			}
		}
		if scopeStarted {
			record(scope, false)
		}
		for _, domain := range domains {
			record(domain, true)
		}
	}()
}
