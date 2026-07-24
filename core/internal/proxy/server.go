package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// proxyRouter is the core HTTP handler that dispatches incoming proxy requests.
// It replaces the goproxy library with a minimal, zero-dependency implementation
// that supports both HTTP forwarding and HTTPS CONNECT tunneling.
type proxyRouter struct {
	upstream    *UpstreamProxyHandler
	userAuthMw  *UserAuthMiddleware
	rateLimitMw *RateLimitMiddleware
	logger      *logger.Logger
}

// ServeHTTP dispatches incoming requests through the middleware chain
// and routes them to the appropriate handler based on method.
func (p *proxyRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. User auth middleware (sets PoolChain in context or falls back to legacy)
	r, reject := p.userAuthMw.HandleRequest(r)
	if reject != nil {
		writeHTTPResponse(w, reject)
		return
	}

	// 2. Rate limit middleware
	r, reject = p.rateLimitMw.HandleRequest(r)
	if reject != nil {
		writeHTTPResponse(w, reject)
		return
	}

	// 3. Attach the normalized target host so selection can honor
	//    domain-scoped invalidations.
	if host := normalizeHost(requestTargetHost(r.Method, r.URL.Host, r.Host)); host != "" {
		r = r.WithContext(context.WithValue(r.Context(), TargetHostContextKey, host))
	}

	// 4. Dispatch based on method
	if r.Method == http.MethodConnect {
		p.upstream.HandleConnectRequest(w, r)
	} else {
		p.upstream.HandleHTTPRequest(w, r)
	}
}

// writeHTTPResponse translates a middleware-returned *http.Response into
// http.ResponseWriter calls. This bridges the middleware return convention
// (returning *http.Response for reject) with the stdlib interface.
func writeHTTPResponse(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		return
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		io.Copy(w, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}
}

// Server represents the proxy server
type Server struct {
	router         *proxyRouter
	server         *http.Server
	logger         *logger.Logger
	port           int
	tracker        *UsageTracker
	handler        *UpstreamProxyHandler
	authMiddleware *AuthMiddleware
	userAuthMw     *UserAuthMiddleware
	rateLimitMw    *RateLimitMiddleware
	proxyRepo      *repository.ProxyRepository
	settingsRepo   *repository.SettingsRepository
	sessionMgr     *SessionManager
	domainCD       *DomainCooldownManager
	refreshTicker  *time.Ticker
	cleanupTicker  *time.Ticker
	stopChan       chan struct{}
}

// New creates a new proxy server instance
func New(
	port int,
	log *logger.Logger,
	db *database.DB,
	eventStore events.Store,
	proxyRepo *repository.ProxyRepository,
	poolRepo *repository.PoolRepository,
	userRepo *repository.UserRepository,
	settingsRepo *repository.SettingsRepository,
) (*Server, error) {
	// Load settings
	ctx := context.Background()
	settings, err := settingsRepo.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}

	// Create usage tracker
	tracker := NewUsageTracker(eventStore, proxyRepo)

	// Domain-scoped invalidations (process-wide, warmed from DB so they
	// survive restarts)
	domainCD := NewDomainCooldownManager()
	if cooldowns, err := proxyRepo.ListActiveDomainCooldowns(ctx); err != nil {
		log.Warn("failed to load domain cooldowns", "error", err)
	} else {
		for _, c := range cooldowns {
			reason := ""
			if c.Reason != nil {
				reason = *c.Reason
			}
			domainCD.Set(c.ProxyID, c.Domain, c.CooldownUntil, reason)
		}
	}

	// Create upstream proxy handler (forwards through the request's PoolChain)
	handler := NewUpstreamProxyHandler(&settings.Rotation, log)

	// Create middlewares
	authMiddleware := NewAuthMiddleware(settings.Authentication)
	rateLimitMw := NewRateLimitMiddleware(settings.RateLimit)

	// Session manager for "session" rotation (process-wide, survives chain rebuilds)
	sessionMgr := NewSessionManager()

	// Create user-aware auth middleware (pool-based routing)
	userAuthMw := NewUserAuthMiddleware(userRepo, poolRepo, db, authMiddleware, &settings.Rotation, sessionMgr, domainCD, tracker, log)

	// Create the proxy router
	router := &proxyRouter{
		upstream:    handler,
		userAuthMw:  userAuthMw,
		rateLimitMw: rateLimitMw,
		logger:      log,
	}

	// WriteTimeout must be 0 for CONNECT tunnels (they are long-lived).
	// HTTP path enforces timeouts via context.
	httpServer := &http.Server{
		Addr:        fmt.Sprintf(":%d", port),
		Handler:     router,
		ReadTimeout: time.Duration(settings.Rotation.Timeout) * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	s := &Server{
		router:         router,
		server:         httpServer,
		logger:         log,
		port:           port,
		tracker:        tracker,
		handler:        handler,
		authMiddleware: authMiddleware,
		userAuthMw:     userAuthMw,
		rateLimitMw:    rateLimitMw,
		proxyRepo:      proxyRepo,
		settingsRepo:   settingsRepo,
		sessionMgr:     sessionMgr,
		domainCD:       domainCD,
		stopChan:       make(chan struct{}),
	}

	// Start background tasks
	s.startBackgroundTasks()

	return s, nil
}

// startBackgroundTasks starts periodic background tasks
func (s *Server) startBackgroundTasks() {
	// Refresh proxy list every 30 seconds
	s.refreshTicker = time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-s.refreshTicker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				// Proxy lists are refreshed by UserAuthMiddleware (default + per-user
				// chains). Here we only re-sync domain cooldowns from the DB so the
				// in-memory view tracks expirations and cooldowns set by other
				// instances.
				if s.domainCD != nil {
					if cooldowns, err := s.proxyRepo.ListActiveDomainCooldowns(ctx); err != nil {
						s.logger.Error("failed to refresh domain cooldowns", "error", err)
					} else {
						s.domainCD.ReplaceAll(cooldowns)
					}
				}
				cancel()
			case <-s.stopChan:
				return
			}
		}
	}()

	// Cleanup rate limiters every 5 minutes
	s.cleanupTicker = time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-s.cleanupTicker.C:
				s.rateLimitMw.CleanupLimiters()
				s.logger.Debug("cleaned up rate limiters")
			case <-s.stopChan:
				return
			}
		}
	}()
}

// Start starts the proxy server
func (s *Server) Start() error {
	s.logger.Info("starting proxy server", "port", s.port)

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy server failed: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the proxy server
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down proxy server")

	close(s.stopChan)
	if s.refreshTicker != nil {
		s.refreshTicker.Stop()
	}
	if s.cleanupTicker != nil {
		s.cleanupTicker.Stop()
	}
	if s.sessionMgr != nil {
		s.sessionMgr.Stop()
	}
	if s.domainCD != nil {
		s.domainCD.Stop()
	}

	return s.server.Shutdown(ctx)
}

// ListSessions returns a snapshot of all live sticky-session bindings.
func (s *Server) ListSessions() []SessionInfo {
	if s.sessionMgr == nil {
		return nil
	}
	return s.sessionMgr.List()
}

// ReleaseSession drops a sticky session for a specific pool. Returns true if a
// binding existed.
func (s *Server) ReleaseSession(poolID int, token string) bool {
	if s.sessionMgr == nil {
		return false
	}
	return s.sessionMgr.Release(poolID, token)
}

// ReleaseSessionToken drops a sticky session across all pools. Returns the count
// of bindings removed.
func (s *Server) ReleaseSessionToken(token string) int {
	if s.sessionMgr == nil {
		return 0
	}
	return s.sessionMgr.ReleaseToken(token)
}

// ReleaseSessionTokenInPools drops a sticky session's bindings restricted to
// the given pools. Returns the count of bindings removed.
func (s *Server) ReleaseSessionTokenInPools(token string, poolIDs []int) int {
	if s.sessionMgr == nil {
		return 0
	}
	return s.sessionMgr.ReleaseTokenInPools(token, poolIDs)
}

// SessionsForToken returns the live sticky-session bindings for a token across
// all pools.
func (s *Server) SessionsForToken(token string) []SessionInfo {
	if s.sessionMgr == nil {
		return nil
	}
	return s.sessionMgr.FindByToken(token)
}

// EvictProxy immediately removes a proxy from every active user's in-memory
// selector and drops any sessions bound to it. The DB cooldown (set by the API
// handler) keeps it out of rotation on the next refresh; this makes the removal
// take effect right away without waiting for the 30s refresh cycle.
func (s *Server) EvictProxy(proxyID int) {
	if s.sessionMgr != nil {
		s.sessionMgr.Evict(proxyID)
	}
	if s.userAuthMw != nil {
		s.userAuthMw.EvictProxy(proxyID)
	}
}

// InvalidateUser drops a proxy user's cached auth entry so that changes to the
// user (disable, password change, pool reassignment, deletion) take effect on
// the next request instead of after the auth cache TTL.
func (s *Server) InvalidateUser(username string) {
	if s.userAuthMw != nil {
		s.userAuthMw.InvalidateUser(username)
	}
}

// SetDomainCooldown puts a proxy on a domain-scoped cooldown: it is skipped
// for requests to domain (and its subdomains) until the given time, but stays
// in rotation for every other target. Takes effect immediately.
func (s *Server) SetDomainCooldown(proxyID int, domain string, until time.Time, reason string) {
	if s.domainCD == nil {
		return
	}
	s.domainCD.Set(proxyID, domain, until, reason)
}

// ClearDomainCooldown removes a single (proxy, domain) cooldown.
// Returns true if one existed.
func (s *Server) ClearDomainCooldown(proxyID int, domain string) bool {
	if s.domainCD == nil {
		return false
	}
	return s.domainCD.Clear(proxyID, domain)
}

// ClearProxyDomainCooldowns removes every domain cooldown for a proxy.
// Returns the count removed.
func (s *Server) ClearProxyDomainCooldowns(proxyID int) int {
	if s.domainCD == nil {
		return 0
	}
	return s.domainCD.ClearProxy(proxyID)
}

// ListDomainCooldowns returns a snapshot of all active domain cooldowns.
func (s *Server) ListDomainCooldowns() []models.ProxyDomainCooldown {
	if s.domainCD == nil {
		return nil
	}
	return s.domainCD.List()
}

// ReloadSettings reloads settings from database and updates components
func (s *Server) ReloadSettings(ctx context.Context) error {
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	// Update middleware settings
	s.authMiddleware.UpdateSettings(settings.Authentication)
	s.rateLimitMw.UpdateSettings(settings.RateLimit)

	// Update handler settings (atomic publish; read concurrently on the hot path)
	s.handler.setSettings(&settings.Rotation)

	// Rebuild the default pool chain so global rotation-method/filter changes take
	// effect. Per-user chains pick up rotation settings on their own refresh.
	s.userAuthMw.RebuildDefaultChain(ctx, &settings.Rotation)

	s.logger.Info("settings reloaded successfully")
	return nil
}
