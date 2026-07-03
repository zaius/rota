package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// ProxyServer is the subset of the running proxy server the API needs for
// settings reloads and live session/cooldown control.
type ProxyServer interface {
	ReloadSettings(ctx context.Context) error
	EvictProxy(proxyID int)
	InvalidateUser(username string)
	ListSessions() []proxy.SessionInfo
	ReleaseSession(poolID int, token string) bool
	ReleaseSessionToken(token string) int
	SetDomainCooldown(proxyID int, domain string, until time.Time, reason string)
	ClearDomainCooldown(proxyID int, domain string) bool
	ClearProxyDomainCooldowns(proxyID int) int
	ListDomainCooldowns() []models.ProxyDomainCooldown
}

// ProxyControlHandler serves the live proxy-control endpoints (reload,
// invalidate/reactivate, session and domain-cooldown management). These act on
// the running proxy server via the ProxyServer interface plus the proxy repo,
// so they previously lived on the API Server itself; they are extracted here so
// api.Server holds no HTTP handlers of its own.
type ProxyControlHandler struct {
	proxyRepo   *repository.ProxyRepository
	logger      *logger.Logger
	proxyServer ProxyServer
}

// NewProxyControlHandler creates a ProxyControlHandler. The proxy server
// reference is attached later via SetProxyServer, since it is constructed after
// the API server.
func NewProxyControlHandler(proxyRepo *repository.ProxyRepository, log *logger.Logger) *ProxyControlHandler {
	return &ProxyControlHandler{proxyRepo: proxyRepo, logger: log}
}

// SetProxyServer attaches the running proxy server.
func (h *ProxyControlHandler) SetProxyServer(ps ProxyServer) {
	h.proxyServer = ps
}

// ReloadProxyPool reloads proxy settings from the database.
//
//	@Summary		Reload proxy pool
//	@Description	Reload proxy pool from database
//	@Tags			proxies
//	@Produce		json
//	@Success		200	{object}	map[string]interface{}	"Reload confirmation"
//	@Failure		500	{object}	models.ErrorResponse
//	@Failure		503	{object}	models.ErrorResponse
//	@Router			/proxies/reload [post]
func (h *ProxyControlHandler) ReloadProxyPool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.proxyServer == nil {
		h.logger.Error("proxy server not initialized")
		writeError(w, http.StatusServiceUnavailable, "proxy server not available")
		return
	}

	h.logger.Info("reloading proxy pool via API request")

	if err := h.proxyServer.ReloadSettings(ctx); err != nil {
		h.logger.Error("failed to reload proxy pool", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to reload proxy pool: %v", err))
		return
	}

	h.logger.Info("proxy pool reloaded successfully")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"success","message":"Proxy pool reloaded successfully"}`)) //nolint:errcheck
}

// InvalidateProxy marks a proxy as temporarily out of rotation (e.g. when the
// client detects it has been rate-limited). It sets a DB cooldown and evicts the
// proxy from live rotation immediately, rebinding any sessions that were using it.
// When a domain is supplied the cooldown is scoped to that domain (and its
// subdomains) only: the proxy keeps serving every other target.
//
//	@Summary		Invalidate a proxy
//	@Description	Pull a proxy out of rotation for a cooldown period (rate-limited, etc.). Pass "domain" to only invalidate it for that domain and its subdomains, keeping it available for other targets.
//	@Tags			proxies
//	@Param			id		path	int		true	"Proxy ID"
//	@Param			minutes	body	int		false	"Cooldown minutes (default 30; 0 = until reactivated)"
//	@Param			reason	body	string	false	"Why the proxy was invalidated"
//	@Param			domain	body	string	false	"Scope the cooldown to this domain (e.g. foo.com, also covers *.foo.com)"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/proxies/{id}/invalidate [post]
func (h *ProxyControlHandler) InvalidateProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
		Domain  string `json:"domain"`
	}
	// Body is optional.
	_ = json.NewDecoder(r.Body).Decode(&body)

	// minutes <= 0 → long default ("until reactivated"); >0 → that many minutes.
	d := time.Duration(body.Minutes) * time.Minute

	// Domain-scoped invalidation: cooldown applies only to this target domain.
	if body.Domain != "" {
		domain := proxy.NormalizeCooldownDomain(body.Domain)
		if domain == "" {
			writeError(w, http.StatusBadRequest, "invalid domain")
			return
		}
		if d <= 0 {
			d = 24 * time.Hour // same "until reactivated" default as SetCooldown
		}
		until := time.Now().Add(d)

		proxyObj, err := h.proxyRepo.SetDomainCooldown(r.Context(), id, domain, until, body.Reason)
		if err != nil {
			h.logger.Error("failed to invalidate proxy for domain", "id", id, "domain", domain, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to invalidate proxy")
			return
		}
		if proxyObj == nil {
			writeError(w, http.StatusNotFound, "proxy not found")
			return
		}

		// Make it effective immediately. Sessions bound to this proxy are kept;
		// they rebind lazily on their next request to the cooled domain.
		if h.proxyServer != nil {
			h.proxyServer.SetDomainCooldown(id, domain, until, body.Reason)
		}

		h.logger.Info("proxy invalidated for domain",
			"id", id, "domain", domain, "minutes", body.Minutes, "reason", body.Reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":         "invalidated",
			"id":             proxyObj.ID,
			"address":        proxyObj.Address,
			"domain":         domain,
			"cooldown_until": until,
		})
		return
	}

	proxyObj, err := h.proxyRepo.SetCooldown(r.Context(), id, d, body.Reason)
	if err != nil {
		h.logger.Error("failed to invalidate proxy", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to invalidate proxy")
		return
	}
	if proxyObj == nil {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}

	// Evict from live rotation + drop bound sessions immediately.
	if h.proxyServer != nil {
		h.proxyServer.EvictProxy(id)
	}

	h.logger.Info("proxy invalidated", "id", id, "minutes", body.Minutes, "reason", body.Reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := map[string]interface{}{
		"status":         "invalidated",
		"id":             proxyObj.ID,
		"address":        proxyObj.Address,
		"cooldown_until": proxyObj.CooldownUntil,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ReactivateProxy clears a proxy's cooldown, returning it to rotation. With a
// "domain" in the body only that domain-scoped cooldown is cleared; without
// one the global cooldown and all domain cooldowns are cleared.
//
//	@Summary		Reactivate a proxy
//	@Description	Clear a proxy's cooldown and return it to rotation. Pass "domain" to clear only that domain-scoped cooldown; omit it to clear the global cooldown and all domain cooldowns.
//	@Tags			proxies
//	@Param			id		path	int		true	"Proxy ID"
//	@Param			domain	body	string	false	"Clear only the cooldown for this domain"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/proxies/{id}/reactivate [post]
func (h *ProxyControlHandler) ReactivateProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Domain string `json:"domain"`
	}
	// Body is optional.
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Domain-scoped reactivation: clear just that domain's cooldown.
	if body.Domain != "" {
		domain := proxy.NormalizeCooldownDomain(body.Domain)
		if domain == "" {
			writeError(w, http.StatusBadRequest, "invalid domain")
			return
		}
		cleared, err := h.proxyRepo.ClearDomainCooldown(r.Context(), id, domain)
		if err != nil {
			h.logger.Error("failed to reactivate proxy for domain", "id", id, "domain", domain, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to reactivate proxy")
			return
		}
		if h.proxyServer != nil {
			h.proxyServer.ClearDomainCooldown(id, domain)
		}
		h.logger.Info("proxy reactivated for domain", "id", id, "domain", domain)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "reactivated",
			"id":      id,
			"domain":  domain,
			"cleared": cleared,
		})
		return
	}

	proxyObj, err := h.proxyRepo.ClearCooldown(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to reactivate proxy", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to reactivate proxy")
		return
	}
	if proxyObj == nil {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}

	// Full reactivation also drops any domain-scoped cooldowns.
	if _, err := h.proxyRepo.ClearAllDomainCooldowns(r.Context(), id); err != nil {
		h.logger.Warn("failed to clear proxy domain cooldowns", "id", id, "error", err)
	}
	if h.proxyServer != nil {
		h.proxyServer.ClearProxyDomainCooldowns(id)
	}

	h.logger.Info("proxy reactivated", "id", id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "reactivated", "id": proxyObj.ID})
}

// ListDomainCooldowns returns all active domain-scoped proxy cooldowns.
//
//	@Summary		List domain-scoped proxy cooldowns
//	@Tags			proxies
//	@Produce		json
//	@Success		200	{object}	map[string]interface{}
//	@Router			/proxies/domain-cooldowns [get]
func (h *ProxyControlHandler) ListDomainCooldowns(w http.ResponseWriter, r *http.Request) {
	var cooldowns []models.ProxyDomainCooldown
	if h.proxyServer != nil {
		cooldowns = h.proxyServer.ListDomainCooldowns()
	}
	if cooldowns == nil {
		cooldowns = []models.ProxyDomainCooldown{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"domain_cooldowns": cooldowns})
}

// ListSessions returns all live sticky-session bindings.
//
//	@Summary		List active sessions
//	@Tags			sessions
//	@Produce		json
//	@Success		200	{object}	map[string]interface{}
//	@Router			/sessions [get]
func (h *ProxyControlHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	var sessions []proxy.SessionInfo
	if h.proxyServer != nil {
		sessions = h.proxyServer.ListSessions()
	}
	if sessions == nil {
		sessions = []proxy.SessionInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
}

// ReleaseSession drops a sticky session. Provide a token (released across all
// pools) and optionally a pool_id to scope the release to a single pool.
//
//	@Summary		Release a sticky session
//	@Tags			sessions
//	@Param			token	body	string	true	"Session token"
//	@Param			pool_id	body	int		false	"Restrict to a single pool"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/sessions/release [post]
func (h *ProxyControlHandler) ReleaseSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		PoolID *int   `json:"pool_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if h.proxyServer == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy server not available")
		return
	}

	released := 0
	if body.PoolID != nil {
		if h.proxyServer.ReleaseSession(*body.PoolID, body.Token) {
			released = 1
		}
	} else {
		released = h.proxyServer.ReleaseSessionToken(body.Token)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "released", "count": released})
}
