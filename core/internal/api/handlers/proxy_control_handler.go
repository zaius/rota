package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
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
	SessionsForToken(token string) []proxy.SessionInfo
	ReleaseSessions(filter proxy.SessionFilter) int
	SetDomainCooldown(c models.ProxyDomainCooldown)
	ClearDomainCooldown(proxyID int, domain string) bool
	ClearProxyDomainCooldowns(proxyID int) int
	ListDomainCooldowns() []models.ProxyDomainCooldown
	SetScopeCooldown(c models.ProxyScopeCooldown)
	ClearScopeCooldowns(proxyID int, scope string) int
	ListScopeCooldowns() []models.ProxyScopeCooldown
	OpenTunnels() int64
}

// ProxyUserFrom returns the proxy user the request was authenticated as (set
// by the API's JWT-or-proxy-user middleware under models.ProxyUserContextKey),
// or nil for admin (JWT) requests. When present, control handlers scope their
// effects to that user's own pools.
func ProxyUserFrom(ctx context.Context) *models.ProxyUser {
	u, _ := ctx.Value(models.ProxyUserContextKey).(*models.ProxyUser)
	return u
}

// ProxyControlHandler serves the live proxy-control endpoints (reload,
// invalidate/reactivate, session and domain-cooldown management). These act on
// the running proxy server via the ProxyServer interface plus the proxy repo,
// so they previously lived on the API Server itself; they are extracted here so
// api.Server holds no HTTP handlers of its own.
type ProxyControlHandler struct {
	proxyRepo   proxyControlRepository
	poolRepo    *repository.PoolRepository
	logger      *logger.Logger
	proxyServer ProxyServer
	// Keep database commits and their live updates in the same order.
	cooldownMu sync.Mutex
}

type proxyControlRepository interface {
	SetCooldown(context.Context, int, time.Duration, string) (*models.Proxy, error)
	SetDomainCooldown(context.Context, int, string, time.Time, string) (*models.Proxy, models.ProxyDomainCooldown, error)
	BackoffScopeCooldown(context.Context, int, string, time.Duration, string) (*models.Proxy, models.ProxyScopeCooldown, error)
	BackoffDomainCooldown(context.Context, int, string, time.Duration, string) (*models.Proxy, models.ProxyDomainCooldown, error)
	ClearCooldown(context.Context, int) (*models.Proxy, error)
	ClearDomainCooldown(context.Context, int, string) (bool, error)
	ClearAllDomainCooldowns(context.Context, int) (int, error)
	ClearScopeCooldowns(context.Context, int, string) (int, error)
}

// NewProxyControlHandler creates a ProxyControlHandler. The proxy server
// reference is attached later via SetProxyServer, since it is constructed after
// the API server.
func NewProxyControlHandler(proxyRepo *repository.ProxyRepository, poolRepo *repository.PoolRepository, log *logger.Logger) *ProxyControlHandler {
	return &ProxyControlHandler{proxyRepo: proxyRepo, poolRepo: poolRepo, logger: log}
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

// proxyInCallerScope reports whether a proxy-user-authenticated caller may act
// on the given proxy: it must be a member of one of the caller's pools. Admin
// (JWT) callers are always in scope.
func (h *ProxyControlHandler) proxyInCallerScope(r *http.Request, proxyID int) (bool, error) {
	pu := ProxyUserFrom(r.Context())
	if pu == nil {
		return true, nil
	}
	return h.poolRepo.ProxyInPools(r.Context(), proxyID, pu.PoolIDs())
}

// InvalidateProxy marks a proxy as temporarily out of rotation (e.g. when the
// client detects it has been rate-limited). It sets a DB cooldown and evicts the
// proxy from live rotation immediately, rebinding any sessions that were using it.
// When a domain is supplied the cooldown is scoped to that domain (and its
// subdomains) only: the proxy keeps serving every other target.
//
// Callers authenticated as a proxy user (Basic auth) may only invalidate
// proxies that belong to one of their own pools.
//
//	@Summary		Invalidate a proxy
//	@Description	Pull a proxy out of rotation for a cooldown period (rate-limited, etc.). Pass "domain" to only invalidate it for that domain and its subdomains, keeping it available for other targets. Authenticates with an admin JWT or proxy-user Basic credentials (scoped to the user's pools).
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

	allowed, err := h.proxyInCallerScope(r, id)
	if err != nil {
		h.logger.Error("failed to check proxy pool membership", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to invalidate proxy")
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "proxy is not in your pools")
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
//	@Description	Cool each bound proxy in its reservation scope by default. domain and global override cooldown scope. Filter bindings by pool_id, scope, or username (admin). Proxy users only match their own bindings in assigned pools. minutes sets the base duration (default 30); consecutive invalidations use 1x, 2x, 4x, then exclude until reactivated. Resumed traffic without invalidation for the session idle-expiry duration resets the streak.
//	@Tags			sessions
//	@Param			token	body	string	true	"Session token"
//	@Param			pool_id	body	int		false	"Restrict to a single pool"
//	@Param			scope	body	string	false	"Restrict to a reservation scope"
//	@Param			username	body	string	false	"Restrict to a session owner (admin)"
//	@Param			minutes	body	int		false	"Positive cooldown minutes (default 30)"
//	@Param			reason	body	string	false	"Why the proxy was invalidated"
//	@Param			domain	body	string	false	"Scope the cooldown to this domain"
//	@Param			global	body	bool	false	"Invalidate across all targets instead of the reservation scope"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/sessions/invalidate [post]
func (h *ProxyControlHandler) InvalidateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		PoolID   *int   `json:"pool_id"`
		Scope    string `json:"scope"`
		Username string `json:"username"`
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

	sessions := h.proxyServer.SessionsForToken(body.Token)
	reservationScope := proxy.NormalizeSessionScope(body.Scope)
	if body.Scope != "" && reservationScope == "" {
		writeError(w, http.StatusBadRequest, "invalid reservation scope")
		return
	}
	filtered := sessions[:0]
	for _, s := range sessions {
		if (reservationScope == "" || s.Scope == reservationScope) && (body.Username == "" || s.Username == body.Username) {
			filtered = append(filtered, s)
		}
	}
	sessions = filtered
	if body.PoolID != nil {
		filtered := sessions[:0]
		for _, s := range sessions {
			if s.PoolID == *body.PoolID {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}
	// Proxy-user callers only see their own sessions in their assigned pools. Out-of-scope
	// bindings are reported as not found, not as forbidden, so the endpoint
	// does not leak other pools' session tokens.
	if pu := ProxyUserFrom(r.Context()); pu != nil {
		scope := make(map[int]bool)
		for _, id := range pu.PoolIDs() {
			scope[id] = true
		}
		filtered := sessions[:0]
		for _, s := range sessions {
			if scope[s.PoolID] && s.Username == pu.Username {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}
	if len(sessions) == 0 {
		writeError(w, http.StatusNotFound, "no live session for token")
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
		payload["pool_id"] = s.PoolID
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

// ReleaseSession drops bindings matching a token and optional pool/scope/owner.
// Proxy-user callers can only release their own sessions in their assigned pools.
//
//	@Summary		Release a sticky session
//	@Tags			sessions
//	@Param			token	body	string	true	"Session token"
//	@Param			pool_id	body	int		false	"Restrict to a single pool"
//	@Param			scope	body	string	false	"Restrict to a reservation scope"
//	@Param			username	body	string	false	"Restrict to a session owner (admin)"
//	@Success		200	{object}	map[string]interface{}
//	@Router			/sessions/release [post]
func (h *ProxyControlHandler) ReleaseSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		PoolID   *int   `json:"pool_id"`
		Scope    string `json:"scope"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if h.proxyServer == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy server not available")
		return
	}

	pu := ProxyUserFrom(r.Context())
	if pu != nil && body.PoolID != nil {
		inScope := false
		for _, id := range pu.PoolIDs() {
			if id == *body.PoolID {
				inScope = true
				break
			}
		}
		if !inScope {
			writeError(w, http.StatusForbidden, "pool is not in your pools")
			return
		}
	}

	filter := proxy.SessionFilter{Token: body.Token, Username: body.Username, Scope: proxy.NormalizeSessionScope(body.Scope)}
	if pu != nil {
		filter.Username = pu.Username
		filter.PoolIDs = append([]int{}, pu.PoolIDs()...)
	}
	if body.PoolID != nil {
		filter.PoolIDs = []int{*body.PoolID}
	}
	released := h.proxyServer.ReleaseSessions(filter)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "released", "count": released})
}
