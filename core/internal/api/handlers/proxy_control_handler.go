package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// ProxyControlHandler serves the live proxy-control endpoints (reload,
// invalidate/reactivate, session and domain-cooldown management, export).
// These act on the running proxy server plus the proxy repo.
type ProxyControlHandler struct {
	proxyRepo   *repository.ProxyRepository
	logger      *logger.Logger
	proxyServer *proxy.Server
	// Keep database commits and their live updates in the same order.
	cooldownMu sync.Mutex
}

// NewProxyControlHandler creates a ProxyControlHandler. The proxy server
// reference is attached later via SetProxyServer, since it is constructed after
// the API server.
func NewProxyControlHandler(proxyRepo *repository.ProxyRepository, log *logger.Logger) *ProxyControlHandler {
	return &ProxyControlHandler{proxyRepo: proxyRepo, logger: log}
}

// ExportWorkingProxies lists proxies as plain text, one per line, credentials
// included, for clients that connect to upstream proxies directly.
//
// Query: country, tag (repeatable; all must match), limit, status (active — the
// default, active and outside a cooldown — or all), format (url —
// protocol://user:pass@host:port, the default — or colon —
// host:port:user:pass).
func (h *ProxyControlHandler) ExportWorkingProxies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	status := q.Get("status")
	if status != "" && status != "active" && status != "all" {
		writeError(w, http.StatusBadRequest, "status must be active or all")
		return
	}
	format := q.Get("format")
	if format != "" && format != "url" && format != "colon" {
		writeError(w, http.StatusBadRequest, "format must be url or colon")
		return
	}

	proxies, err := h.proxyRepo.WorkingProxies(r.Context(), strings.ToUpper(q.Get("country")), q["tag"], limit, status == "all")
	if err != nil {
		h.logger.Error("failed to list working proxies", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list proxies")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	for _, p := range proxies {
		fmt.Fprintln(w, exportLine(p, format))
	}
}

// exportLine renders one proxy with its credentials in the requested format.
func exportLine(p models.Proxy, format string) string {
	user, pass := "", ""
	if p.Username != nil {
		user = *p.Username
	}
	if p.Password != nil {
		pass = *p.Password
	}
	if format == "colon" {
		if user == "" {
			return p.Address
		}
		return p.Address + ":" + user + ":" + pass
	}
	u := url.URL{Scheme: p.Protocol, Host: p.Address}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

// SetProxyServer attaches the running proxy server.
func (h *ProxyControlHandler) SetProxyServer(ps *proxy.Server) {
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

// invalidateBody is the shared request body for proxy/session invalidation.
type invalidateBody struct {
	Minutes int    `json:"minutes"`
	Reason  string `json:"reason"`
	Domain  string `json:"domain"`
	Global  bool   `json:"global"`

	// Set from the selected binding, never directly from JSON. The request's
	// scope field filters bindings; it does not name an arbitrary cooldown.
	cooldownScope  string
	sessionBackoff bool
}

func (b *invalidateBody) validate() string {
	if b.Minutes < 1 || int64(b.Minutes) > int64((1<<63-1)/time.Minute) {
		return "minutes must be a positive integer within the supported duration range"
	}
	if b.Global && b.Domain != "" {
		return "global and domain cannot be combined"
	}
	if b.Domain != "" {
		b.Domain = proxy.NormalizeCooldownDomain(b.Domain)
		if b.Domain == "" {
			return "invalid domain"
		}
	}
	return ""
}

// applyInvalidation puts one proxy on a global, domain or reservation cooldown,
// makes it effective on the running proxy server immediately, and returns the
// per-proxy response payload. On failure it returns the HTTP status and
// message to report instead.
func (h *ProxyControlHandler) applyInvalidation(ctx context.Context, id int, body invalidateBody) (map[string]interface{}, int, string) {
	h.cooldownMu.Lock()
	defer h.cooldownMu.Unlock()
	if msg := body.validate(); msg != "" {
		return nil, http.StatusBadRequest, msg
	}
	d := time.Duration(body.Minutes) * time.Minute
	if body.cooldownScope != "" {
		p, cooldown, err := h.proxyRepo.BackoffScopeCooldown(ctx, id, body.cooldownScope, d, body.Reason)
		if err != nil {
			h.logger.Error("failed to invalidate proxy for scope", "id", id, "scope", body.cooldownScope, "error", err)
			return nil, http.StatusInternalServerError, "failed to invalidate proxy"
		}
		if p == nil {
			return nil, http.StatusNotFound, "proxy not found"
		}
		if h.proxyServer != nil {
			h.proxyServer.SetScopeCooldown(cooldown)
		}
		h.logger.Info("proxy invalidated for scope", "id", id, "scope", body.cooldownScope, "minutes", body.Minutes, "reason", body.Reason)
		return map[string]interface{}{
			"status": "invalidated", "id": p.ID, "address": p.Address,
			"scope": body.cooldownScope, "cooldown_until": cooldownExpiry(cooldown.CooldownUntil, cooldown.Invalid),
			"failure_count": cooldown.FailureCount, "invalid": cooldown.Invalid,
		}, http.StatusOK, ""
	}

	// Domain-scoped invalidation: cooldown applies only to this target domain.
	if body.Domain != "" {
		domain := body.Domain
		until := time.Now().Add(d)

		var proxyObj *models.Proxy
		var cooldown models.ProxyDomainCooldown
		var err error
		if body.sessionBackoff {
			proxyObj, cooldown, err = h.proxyRepo.BackoffDomainCooldown(ctx, id, domain, d, body.Reason)
		} else {
			proxyObj, cooldown, err = h.proxyRepo.SetDomainCooldown(ctx, id, domain, until, body.Reason)
		}
		if err != nil {
			h.logger.Error("failed to invalidate proxy for domain", "id", id, "domain", domain, "error", err)
			return nil, http.StatusInternalServerError, "failed to invalidate proxy"
		}
		if proxyObj == nil {
			return nil, http.StatusNotFound, "proxy not found"
		}

		// Make it effective immediately. Sessions bound to this proxy are kept;
		// they rebind lazily on their next request to the cooled domain.
		if h.proxyServer != nil {
			h.proxyServer.SetDomainCooldown(cooldown)
		}

		h.logger.Info("proxy invalidated for domain",
			"id", id, "domain", domain, "minutes", body.Minutes, "reason", body.Reason)
		return map[string]interface{}{
			"status":         "invalidated",
			"id":             proxyObj.ID,
			"address":        proxyObj.Address,
			"domain":         domain,
			"cooldown_until": cooldownExpiry(cooldown.CooldownUntil, cooldown.Invalid),
			"failure_count":  cooldown.FailureCount, "invalid": cooldown.Invalid,
		}, http.StatusOK, ""
	}

	proxyObj, err := h.proxyRepo.SetCooldown(ctx, id, d, body.Reason)
	if err != nil {
		h.logger.Error("failed to invalidate proxy", "id", id, "error", err)
		return nil, http.StatusInternalServerError, "failed to invalidate proxy"
	}
	if proxyObj == nil {
		return nil, http.StatusNotFound, "proxy not found"
	}

	// Evict from live rotation + drop bound sessions immediately.
	if h.proxyServer != nil {
		h.proxyServer.EvictProxy(id)
	}

	h.logger.Info("proxy invalidated", "id", id, "minutes", body.Minutes, "reason", body.Reason)
	return map[string]interface{}{
		"status":         "invalidated",
		"id":             proxyObj.ID,
		"address":        proxyObj.Address,
		"cooldown_until": proxyObj.CooldownUntil,
	}, http.StatusOK, ""
}

// A permanent scoped exclusion has no expiry, even though its stored deadline
// remains a timestamp for compatibility with existing cooldown rows.
func cooldownExpiry(until time.Time, invalid bool) *time.Time {
	if invalid {
		return nil
	}
	return &until
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
//	@Param			minutes	body	int		false	"Positive cooldown minutes (default 30)"
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

	body := invalidateBody{Minutes: 30}
	// Body is optional.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if msg := body.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	payload, status, msg := h.applyInvalidation(r.Context(), id, body)
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

// InvalidateSession invalidates the proxy currently bound to a sticky-session
// token — for when the client knows its session is burned (e.g. it hit a 429)
// but not which proxy ID served it. By default each bound proxy is cooled only
// in that binding's reservation scope. domain overrides with a domain cooldown;
// global explicitly cools the proxy across all targets and drops all bindings.
//
//	@Summary		Invalidate the proxy bound to a session
//	@Description	Cool each bound proxy in its reservation scope by default. domain and global override cooldown scope. scope restricts which bindings match. minutes sets the base duration (default 30); consecutive invalidations use 1x, 2x, 4x, then exclude until reactivated. Resumed traffic without invalidation for the session TTL resets the streak.
//	@Tags			sessions
//	@Param			token	body	string	true	"Session token"
//	@Param			scope	body	string	false	"Restrict to a reservation scope"
//	@Param			minutes	body	int		false	"Positive cooldown minutes (default 30)"
//	@Param			reason	body	string	false	"Why the proxy was invalidated"
//	@Param			domain	body	string	false	"Scope the cooldown to this domain"
//	@Param			global	body	bool	false	"Invalidate across all targets instead of the reservation scope"
//	@Success		200	{object}	map[string]interface{}
//	@Failure		409	{object}	models.ErrorResponse	"No live session for token"
//	@Router			/sessions/invalidate [post]
func (h *ProxyControlHandler) InvalidateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
		invalidateBody
	}
	body.Minutes = 30
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if msg := body.invalidateBody.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if h.proxyServer == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy server not available")
		return
	}

	scope := proxy.NormalizeSessionScope(body.Scope)
	if body.Scope != "" && scope == "" {
		writeError(w, http.StatusBadRequest, "invalid reservation scope")
		return
	}
	sessions := h.proxyServer.FindSessions(proxy.SessionFilter{Token: body.Token, Scope: scope})
	if len(sessions) == 0 {
		writeError(w, http.StatusConflict, "no live session for token")
		return
	}

	if !body.Global && body.Domain == "" {
		for _, s := range sessions {
			if s.Scope == "" {
				writeError(w, http.StatusBadRequest, "session has no reservation scope; supply domain or global")
				return
			}
		}
	}
	// A proxy can be bound in several scopes. Cool each (proxy, scope) pair;
	// an explicit domain/global override only needs one write per proxy.
	type cooldownKey struct {
		proxyID int
		scope   string
	}
	seen := make(map[cooldownKey]bool)
	invalidated := make([]map[string]interface{}, 0, len(sessions))
	for _, s := range sessions {
		invalidation := body.invalidateBody
		invalidation.sessionBackoff = true
		if !body.Global && body.Domain == "" {
			invalidation.cooldownScope = s.Scope
		}
		key := cooldownKey{s.ProxyID, invalidation.cooldownScope}
		if seen[key] {
			continue
		}
		seen[key] = true
		payload, status, msg := h.applyInvalidation(r.Context(), s.ProxyID, invalidation)
		if status != http.StatusOK {
			writeError(w, status, msg)
			return
		}
		invalidated = append(invalidated, payload)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "invalidated",
		"token":    body.Token,
		"sessions": len(sessions),
		"proxies":  invalidated,
	})
}

// ReactivateProxy clears a proxy's cooldown. A domain or scope in the body
// clears only that cooldown; otherwise all global, domain and scope cooldowns
// are cleared.
//
//	@Summary		Reactivate a proxy
//	@Description	Clear a proxy's cooldown. Pass domain or scope to clear one cooldown; omit both to clear all cooldowns.
//	@Tags			proxies
//	@Param			id		path	int		true	"Proxy ID"
//	@Param			domain	body	string	false	"Clear only the cooldown for this domain"
//	@Param			scope	body	string	false	"Clear only the cooldown for this reservation scope"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/proxies/{id}/reactivate [post]
func (h *ProxyControlHandler) ReactivateProxy(w http.ResponseWriter, r *http.Request) {
	h.cooldownMu.Lock()
	defer h.cooldownMu.Unlock()
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Domain string `json:"domain"`
		Scope  string `json:"scope"`
	}
	// Body is optional.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Scope != "" {
		scope := proxy.NormalizeSessionScope(body.Scope)
		if scope == "" || body.Domain != "" {
			writeError(w, http.StatusBadRequest, "supply a valid scope or domain, not both")
			return
		}
		cleared, err := h.proxyRepo.ClearScopeCooldowns(r.Context(), id, scope)
		if err != nil {
			h.logger.Error("failed to reactivate proxy for scope", "id", id, "scope", scope, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to reactivate proxy")
			return
		}
		if h.proxyServer != nil {
			h.proxyServer.ClearScopeCooldowns(id, scope)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "reactivated", "id": id, "scope": scope, "cleared": cleared,
		})
		return
	}

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
	if _, err := h.proxyRepo.ClearScopeCooldowns(r.Context(), id, ""); err != nil {
		h.logger.Error("failed to clear proxy scope cooldowns", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to clear proxy scope cooldowns")
		return
	}
	if h.proxyServer != nil {
		h.proxyServer.ClearScopeCooldowns(id, "")
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

// ListScopeCooldowns returns active reservation-scope cooldowns.
//
//	@Summary List reservation scope cooldowns
//	@Tags proxies
//	@Produce json
//	@Success 200 {object} map[string]interface{}
//	@Router /proxies/scope-cooldowns [get]
func (h *ProxyControlHandler) ListScopeCooldowns(w http.ResponseWriter, r *http.Request) {
	cooldowns := []models.ProxyScopeCooldown{}
	if h.proxyServer != nil {
		if live := h.proxyServer.ListScopeCooldowns(); live != nil {
			cooldowns = live
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"scope_cooldowns": cooldowns})
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

// ReleaseSession drops the bindings of a token, in one scope or all of them.
//
//	@Summary		Release a sticky session
//	@Tags			sessions
//	@Param			token	body	string	true	"Session token"
//	@Param			scope	body	string	false	"Restrict to a reservation scope"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/sessions/release [post]
func (h *ProxyControlHandler) ReleaseSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if h.proxyServer == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy server not available")
		return
	}
	scope := proxy.NormalizeSessionScope(body.Scope)
	if body.Scope != "" && scope == "" {
		writeError(w, http.StatusBadRequest, "invalid reservation scope")
		return
	}

	released := h.proxyServer.ReleaseSessions(proxy.SessionFilter{Token: body.Token, Scope: scope})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "released", "count": released})
}
