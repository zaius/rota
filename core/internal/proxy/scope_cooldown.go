package proxy

import (
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// SetScopeCooldown keeps other scopes' bindings intact. A cooled binding
// reselects on its next request, including when it uses a fallback pool. Its
// tunnels through the cooled proxy end, so a client holding one open sends
// that request.
func (m *SessionManager) SetScopeCooldown(c models.ProxyScopeCooldown) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cooldowns[reservationKey{c.ProxyID, c.Scope}] = c
	if !c.Invalid && !c.CooldownUntil.After(time.Now()) {
		return
	}
	for key, e := range m.sessions {
		if key.scope == c.Scope {
			e.endTunnelsLocked(func(h *sessionHold) bool { return h.proxyID == c.ProxyID })
		}
	}
}

func (m *SessionManager) ReplaceScopeCooldowns(cooldowns []models.ProxyScopeCooldown) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cooldowns = make(map[reservationKey]models.ProxyScopeCooldown, len(cooldowns))
	for _, c := range cooldowns {
		m.cooldowns[reservationKey{c.ProxyID, c.Scope}] = c
	}
}

// ClearScopeCooldowns clears one exact scope, or all scopes when scope is empty.
func (m *SessionManager) ClearScopeCooldowns(proxyID int, scope string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.cooldowns {
		if key.proxyID == proxyID && (scope == "" || key.scope == scope) {
			delete(m.cooldowns, key)
			n++
		}
	}
	return n
}

func (m *SessionManager) ListScopeCooldowns() []models.ProxyScopeCooldown {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []models.ProxyScopeCooldown{}
	now := time.Now()
	for _, c := range m.cooldowns {
		if c.Invalid || c.CooldownUntil.After(now) {
			out = append(out, c)
		}
	}
	return out
}
