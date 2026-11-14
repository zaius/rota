package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zaius/rota/core/internal/events"
	"github.com/zaius/rota/core/internal/metrics"
	"github.com/zaius/rota/core/internal/models"
	"github.com/zaius/rota/core/internal/repository"
	"github.com/zaius/rota/core/pkg/logger"
	"github.com/zaius/rota/core/pkg/safeworker"
)

// proxyRouter authenticates incoming proxy requests and dispatches them by
// method.
type proxyRouter struct {
	upstream *UpstreamProxyHandler
	password string
}

// ServeHTTP checks the shared password, parses the request options from the
// username, and forwards the request with both attached to its context.
func (p *proxyRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username, password, ok := parseProxyAuth(r)
	if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(p.password)) != 1 {
		reason := "bad_credentials"
		if !ok {
			reason = "missing_credentials"
		}
		metrics.RecordAuthRejection(r.Context(), reason)
		writeAuthRequired(w, "proxy_auth_required", "proxy authentication required")
		return
	}
	opts, err := ParseOptions(username)
	if err != nil {
		metrics.RecordAuthRejection(r.Context(), "invalid_options")
		writeAuthRequired(w, "invalid_proxy_options", err.Error())
		return
	}
	r.Header.Del("Proxy-Authorization")

	// The normalized target host lets selection honor domain-scoped
	// invalidations and default the reservation scope.
	host := normalizeHost(requestTargetHost(r.Method, r.URL.Host, r.Host))
	r = r.WithContext(withRoute(r.Context(), route{Options: opts, host: host}))

	if r.Method == http.MethodConnect {
		p.upstream.HandleConnectRequest(w, r)
	} else {
		p.upstream.HandleHTTPRequest(w, r)
	}
}

// parseProxyAuth extracts username and password from the Proxy-Authorization
// header.
func parseProxyAuth(req *http.Request) (string, string, bool) {
	auth, ok := strings.CutPrefix(req.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(decoded), ":")
}

func writeAuthRequired(w http.ResponseWriter, reason, message string) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="Rota Proxy"`)
	writeRotaError(w, http.StatusProxyAuthRequired, reason, message)
}

// Server represents the proxy server
type Server struct {
	server        *http.Server
	logger        *logger.Logger
	handler       *UpstreamProxyHandler
	rotator       *Rotator
	proxyRepo     *repository.ProxyRepository
	settingsRepo  *repository.SettingsRepository
	sessionMgr    *SessionManager
	domainCD      *DomainCooldownManager
	refreshTicker *time.Ticker
	stopChan      chan struct{}
	// Serialize live scope updates with DB snapshots, so a refresh started
	// before an invalidation/reactivation cannot overwrite its live effect.
	scopeCooldownMu  sync.Mutex
	domainCooldownMu sync.Mutex
}

// New creates a new proxy server instance
func New(
	port int,
	password string,
	log *logger.Logger,
	eventStore events.Store,
	proxyRepo *repository.ProxyRepository,
	settingsRepo *repository.SettingsRepository,
	inspector *TLSInspector,
) (*Server, error) {
	// Load settings
	ctx := context.Background()
	settings, err := settingsRepo.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}

	sessionMgr := NewSessionManager()
	domainCD := NewDomainCooldownManager()
	tracker := NewUsageTracker(eventStore, proxyRepo)
	rotator := NewRotator(proxyRepo, sessionMgr, domainCD, tracker, &settings.Rotation, log)
	// A nil inspector rejects inspection requests and leaves other tunnels opaque.
	handler := NewUpstreamProxyHandler(rotator, inspector, log)

	// WriteTimeout must be 0 for CONNECT tunnels (they are long-lived).
	// HTTP path enforces timeouts via context.
	httpServer := &http.Server{
		Addr:        fmt.Sprintf(":%d", port),
		Handler:     &proxyRouter{upstream: handler, password: password},
		ReadTimeout: time.Duration(settings.Rotation.Timeout) * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	// Live gauges: open tunnels, sticky sessions, domain cooldowns. Recording
	// is a no-op until (unless) main installs the metrics SDK.
	metrics.RegisterProxyObservables(
		handler.OpenTunnels,
		func() int64 { return int64(len(sessionMgr.List())) },
		func() int64 { return int64(len(domainCD.List())) },
	)

	s := &Server{
		server:       httpServer,
		logger:       log,
		handler:      handler,
		rotator:      rotator,
		proxyRepo:    proxyRepo,
		settingsRepo: settingsRepo,
		sessionMgr:   sessionMgr,
		domainCD:     domainCD,
		stopChan:     make(chan struct{}),
	}
	// Load proxies and cooldowns before serving, so restarts keep rotating and
	// honoring invalidations from the first request.
	s.refresh()
	s.startBackgroundTasks()
	return s, nil
}

// startBackgroundTasks starts periodic background tasks
func (s *Server) startBackgroundTasks() {
	s.refreshTicker = time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-s.refreshTicker.C:
				safeworker.Call(s.logger, "proxy_refresh", s.refresh)
			case <-s.stopChan:
				return
			}
		}
	}()
}

// refresh re-syncs the proxy list and the domain and scope cooldowns from the
// database, so the in-memory view tracks new proxies, expirations, and changes
// made by other instances.
func (s *Server) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.rotator.Refresh(ctx); err != nil {
		s.logger.Error("failed to refresh proxies", "error", err)
	}
	func() {
		s.domainCooldownMu.Lock()
		defer s.domainCooldownMu.Unlock()
		if cooldowns, err := s.proxyRepo.ListActiveDomainCooldowns(ctx); err != nil {
			s.logger.Error("failed to refresh domain cooldowns", "error", err)
		} else {
			s.domainCD.ReplaceAll(cooldowns)
		}
	}()
	func() {
		s.scopeCooldownMu.Lock()
		defer s.scopeCooldownMu.Unlock()
		if cooldowns, err := s.proxyRepo.ListActiveScopeCooldowns(ctx); err != nil {
			s.logger.Error("failed to refresh scope cooldowns", "error", err)
		} else {
			s.sessionMgr.ReplaceScopeCooldowns(cooldowns)
		}
	}()
}

// Start starts the proxy server
func (s *Server) Start() error {
	s.logger.Info("starting proxy server", "addr", s.server.Addr)

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy server failed: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the proxy server
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down proxy server")

	close(s.stopChan)
	s.refreshTicker.Stop()
	s.sessionMgr.Stop()
	s.domainCD.Stop()

	return s.server.Shutdown(ctx)
}

// OpenTunnels returns the number of CONNECT tunnels currently established.
// Tunnel history in the event store only covers tunnels that have closed, so
// this is the only view of traffic that is in flight right now.
func (s *Server) OpenTunnels() int64 { return s.handler.OpenTunnels() }

// ListSessions returns a snapshot of all live sticky-session bindings.
func (s *Server) ListSessions() []SessionInfo { return s.sessionMgr.List() }

// FindSessions returns the live sticky-session bindings matching filter.
func (s *Server) FindSessions(filter SessionFilter) []SessionInfo {
	return s.sessionMgr.FindSessions(filter)
}

// ReleaseSessions drops the bindings matching filter.
func (s *Server) ReleaseSessions(filter SessionFilter) int {
	return s.sessionMgr.ReleaseSessions(filter)
}

// EvictProxy immediately removes a proxy from rotation and drops any sessions
// bound to it. The DB cooldown (set by the API handler) keeps it out of
// rotation on the next refresh; this makes the removal take effect right away
// without waiting for the 30s refresh cycle.
func (s *Server) EvictProxy(proxyID int) {
	s.sessionMgr.Evict(proxyID)
	s.rotator.Remove(proxyID)
}

// SetDomainCooldown puts a proxy on a domain-scoped cooldown: it is skipped
// for requests to domain (and its subdomains) until the given time, but stays
// in rotation for every other target. Takes effect immediately, including on
// session tunnels already open to domain through the proxy.
func (s *Server) SetDomainCooldown(c models.ProxyDomainCooldown) {
	s.domainCooldownMu.Lock()
	defer s.domainCooldownMu.Unlock()
	s.domainCD.SetCooldown(c)
	s.sessionMgr.EndDomainTunnels(c.ProxyID, c.Domain)
}

// ClearDomainCooldown removes a single (proxy, domain) cooldown.
// Returns true if one existed.
func (s *Server) ClearDomainCooldown(proxyID int, domain string) bool {
	s.domainCooldownMu.Lock()
	defer s.domainCooldownMu.Unlock()
	return s.domainCD.Clear(proxyID, domain)
}

// ClearProxyDomainCooldowns removes every domain cooldown for a proxy.
// Returns the count removed.
func (s *Server) ClearProxyDomainCooldowns(proxyID int) int {
	s.domainCooldownMu.Lock()
	defer s.domainCooldownMu.Unlock()
	return s.domainCD.ClearProxy(proxyID)
}

func (s *Server) SetScopeCooldown(c models.ProxyScopeCooldown) {
	s.scopeCooldownMu.Lock()
	defer s.scopeCooldownMu.Unlock()
	s.sessionMgr.SetScopeCooldown(c)
}

func (s *Server) ClearScopeCooldowns(proxyID int, scope string) int {
	s.scopeCooldownMu.Lock()
	defer s.scopeCooldownMu.Unlock()
	return s.sessionMgr.ClearScopeCooldowns(proxyID, scope)
}

// ReloadSettings reloads settings from database and updates components
func (s *Server) ReloadSettings(ctx context.Context) error {
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	s.rotator.SetSettings(&settings.Rotation)
	s.logger.Info("settings reloaded successfully")
	return nil
}
