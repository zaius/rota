package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alpkeskin/rota/core/internal/api/handlers"
	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/services"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// ProxyServer interface for reloading proxy pool and live session/proxy control
type ProxyServer interface {
	ReloadSettings(ctx context.Context) error
	EvictProxy(proxyID int)
	ListSessions() []proxy.SessionInfo
	ReleaseSession(poolID int, token string) bool
	ReleaseSessionToken(token string) int
	SetDomainCooldown(proxyID int, domain string, until time.Time, reason string)
	ClearDomainCooldown(proxyID int, domain string) bool
	ClearProxyDomainCooldowns(proxyID int) int
	ListDomainCooldowns() []models.ProxyDomainCooldown
}

// Server represents the API server
type Server struct {
	router    *chi.Mux
	server    *http.Server
	logger    *logger.Logger
	db        *database.DB
	port      int
	jwtSecret string
	authRL    *authRateLimiter
	proxyRepo *repository.ProxyRepository

	// Proxy server reference for reloading
	proxyServer ProxyServer

	// Handlers
	authHandler          *handlers.AuthHandler
	healthHandler        *handlers.HealthHandler
	dashboardHandler     *handlers.DashboardHandler
	proxyHandler         *handlers.ProxyHandler
	logsHandler          *handlers.LogsHandler
	settingsHandler      *handlers.SettingsHandler
	websocketHandler     *handlers.WebSocketHandler
	metricsHandler       *handlers.MetricsHandler
	documentationHandler *handlers.DocumentationHandler
	sourceHandler        *handlers.SourceHandler
	poolHandler          *handlers.PoolHandler
	userHandler          *handlers.UserHandler
}

// New creates a new API server instance
func New(cfg *config.Config, log *logger.Logger, db *database.DB) *Server {
	// Initialize repositories
	proxyRepo := repository.NewProxyRepository(db)
	logRepo := repository.NewLogRepository(db)
	settingsRepo := repository.NewSettingsRepository(db)
	dashboardRepo := repository.NewDashboardRepository(db)
	sourceRepo := repository.NewSourceRepository(db)
	poolRepo := repository.NewPoolRepository(db)
	userRepo := repository.NewUserRepository(db)
	adminRepo := repository.NewAdminRepository(db)

	// Seed admin credentials from env on first start (no-op if already seeded)
	if err := adminRepo.Seed(context.Background(), cfg.AdminUser, cfg.AdminPass); err != nil {
		log.Warn("failed to seed admin credentials", "error", err)
	}

	// Generate random JWT secret on startup
	// This ensures all previous tokens become invalid on restart
	jwtSecret := generateJWTSecret()
	log.Info("generated new JWT secret for this session", "length", len(jwtSecret))

	// Create usage tracker for health checks
	tracker := proxy.NewUsageTracker(proxyRepo)

	// Create health checker for testing proxies
	healthChecker := proxy.NewHealthChecker(proxyRepo, settingsRepo, tracker, log)

	// GeoIP + source + pool services
	geoSvc := services.NewGeoIPService(log)
	sourceSvc := services.NewSourceService(sourceRepo, proxyRepo, poolRepo, geoSvc, log)
	// NOTE: Intentionally NOT wiring healthChecker into sourceSvc or starting a
	// global periodic health check. The global HealthChecker uses a lenient
	// 60s timeout and was flapping pool-marked 'failed' proxies back to 'active',
	// putting dead proxies back into rotation. Pool-level health checks (cron-
	// scheduled per pool in PoolService) are the single source of truth.
	poolSvc := services.NewPoolService(poolRepo, proxyRepo, log)

	// Initialize handlers
	authHandler := handlers.NewAuthHandler(settingsRepo, adminRepo, log, jwtSecret, cfg.AdminUser, cfg.AdminPass)
	healthHandler := handlers.NewHealthHandler(db, proxyRepo, log)
	dashboardHandler := handlers.NewDashboardHandler(dashboardRepo, proxyRepo, log)
	proxyHandler := handlers.NewProxyHandler(proxyRepo, healthChecker, log)
	logsHandler := handlers.NewLogsHandler(logRepo, log)
	settingsHandler := handlers.NewSettingsHandler(settingsRepo, log, nil) // onUpdate set below
	websocketHandler := handlers.NewWebSocketHandler(dashboardRepo, proxyRepo, logRepo, log)
	metricsHandler := handlers.NewMetricsHandler(log)
	documentationHandler := handlers.NewDocumentationHandler()
	sourceHandler := handlers.NewSourceHandler(sourceRepo, sourceSvc, log)
	poolHandler := handlers.NewPoolHandler(poolRepo, poolSvc, log)
	userHandler := handlers.NewUserHandler(userRepo, poolRepo, log)

	// Auth rate limiter (per-IP block + global lockout)
	authRL := newAuthRateLimiter(
		cfg.AuthIPMaxAttempts,
		cfg.AuthIPWindowMinutes,
		cfg.AuthIPBlockMinutes,
		cfg.AuthGlobalMaxPerMinute,
		cfg.AuthGlobalLockoutMin,
		log,
	)

	s := &Server{
		router:               chi.NewRouter(),
		logger:               log,
		db:                   db,
		port:                 cfg.APIPort,
		jwtSecret:            jwtSecret,
		authRL:               authRL,
		proxyRepo:            proxyRepo,
		authHandler:          authHandler,
		healthHandler:        healthHandler,
		dashboardHandler:     dashboardHandler,
		proxyHandler:         proxyHandler,
		logsHandler:          logsHandler,
		settingsHandler:      settingsHandler,
		websocketHandler:     websocketHandler,
		metricsHandler:       metricsHandler,
		documentationHandler: documentationHandler,
		sourceHandler:        sourceHandler,
		poolHandler:          poolHandler,
		userHandler:          userHandler,
	}

	// Wire settings reload: when settings are updated via API, reload proxy server
	settingsHandler.SetOnUpdate(func(ctx context.Context) {
		if s.proxyServer != nil {
			if err := s.proxyServer.ReloadSettings(ctx); err != nil {
				log.Error("failed to reload proxy settings after update", "error", err)
			} else {
				log.Info("proxy settings reloaded after update")
			}
		}
	})

	// Alert watcher + proxy cleanup services
	alertWatcher := services.NewAlertWatcher(poolRepo, log)
	cleanupSvc := services.NewProxyCleanupService(proxyRepo, settingsRepo, log)

	// Start background services
	sourceSvc.Start(context.Background())
	poolSvc.Start(context.Background())
	alertWatcher.Start(context.Background())

	// NOTE: global StartPeriodicHealthCheck is intentionally NOT started.
	// Its 60s timeout was too lenient and kept flapping pool-marked 'failed'
	// proxies back to 'active', returning dead proxies to rotation.
	// Pool-level health checks (PoolService cron) are authoritative.

	cleanupSvc.Start(context.Background())

	s.setupMiddleware()
	s.setupRoutes()

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // health checks on large pools can take several minutes
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// setupMiddleware configures middleware for the API server
func (s *Server) setupMiddleware() {
	// Handle OPTIONS requests first (for CORS preflight)
	s.router.Use(OptionsMiddleware())

	// CORS middleware
	s.router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(LoggerMiddleware(s.logger))
	s.router.Use(middleware.Recoverer)
	// No global timeout — health-check routes need minutes; individual routes handle their own timeouts
}

// setupRoutes configures all API routes
func (s *Server) setupRoutes() {
	// ── Fully public routes ────────────────────────────────────────────────
	s.router.Get("/health", s.healthHandler.Health)

	// API Documentation (public — read-only reference)
	s.router.Get("/docs", s.documentationHandler.ServeDocumentation)
	s.router.Get("/api/v1/swagger.json", s.serveSwaggerJSON)

	// Auth: only login is public; everything else requires a valid JWT
	// Auth rate limiter wraps the login handler — per-IP block + global lockout
	s.router.With(s.authRL.Middleware()).Post("/api/v1/auth/login", s.authHandler.Login)

	// ── Protected routes (JWT required) ────────────────────────────────────
	s.router.Route("/api/v1", func(r chi.Router) {
		r.Use(JWTMiddleware(s.jwtSecret))

		// Auth (require token — change-password, whoami)
		r.Post("/auth/change-password", s.authHandler.ChangePassword)
		r.Get("/auth/me", s.authHandler.GetAdminInfo)

		// Health & Status
		r.Get("/status", s.healthHandler.Status)
		r.Get("/database/health", s.healthHandler.DatabaseHealth)
		r.Get("/database/stats", s.healthHandler.DatabaseStats)

		// System Metrics
		r.Get("/metrics/system", s.metricsHandler.GetSystemMetrics)

		// Dashboard endpoints
		r.Get("/dashboard/stats", s.dashboardHandler.GetStats)
		r.Get("/dashboard/charts/response-time", s.dashboardHandler.GetResponseTimeChart)
		r.Get("/dashboard/charts/success-rate", s.dashboardHandler.GetSuccessRateChart)

		// Proxy management
		r.Get("/proxies", s.proxyHandler.List)
		r.Post("/proxies", s.proxyHandler.Create)
		r.Post("/proxies/bulk", s.proxyHandler.BulkCreate)
		r.Post("/proxies/bulk-delete", s.proxyHandler.BulkDelete)
		r.Post("/proxies/bulk-test", s.proxyHandler.BulkTest)
		r.Get("/proxies/bulk-test/{job_id}", s.proxyHandler.BulkTestStatus)
		r.Delete("/proxies", s.proxyHandler.DeleteAll)
		r.Get("/proxies/export", s.proxyHandler.Export)
		r.Put("/proxies/{id}", s.proxyHandler.Update)
		r.Delete("/proxies/{id}", s.proxyHandler.Delete)
		r.Post("/proxies/{id}/test", s.proxyHandler.Test)
		r.Post("/proxies/{id}/invalidate", s.InvalidateProxy)
		r.Post("/proxies/{id}/reactivate", s.ReactivateProxy)
		r.Get("/proxies/domain-cooldowns", s.ListDomainCooldowns)
		r.Post("/proxies/reload", s.ReloadProxyPool)

		// Sticky sessions (session rotation method)
		r.Get("/sessions", s.ListSessions)
		r.Post("/sessions/release", s.ReleaseSession)

		// System logs
		r.Get("/logs", s.logsHandler.List)
		r.Get("/logs/export", s.logsHandler.Export)

		// Settings
		r.Get("/settings", s.settingsHandler.Get)
		r.Put("/settings", s.settingsHandler.Update)
		r.Post("/settings/reset", s.settingsHandler.Reset)

		// Proxy Sources
		r.Get("/sources", s.sourceHandler.List)
		r.Post("/sources", s.sourceHandler.Create)
		r.Put("/sources/{id}", s.sourceHandler.Update)
		r.Delete("/sources/{id}", s.sourceHandler.Delete)
		r.Post("/sources/{id}/fetch", s.sourceHandler.FetchNow)
		r.Post("/sources/enrich-geo", s.sourceHandler.EnrichGeo)

		// Proxy Users (per-user pool authentication)
		r.Get("/proxy-users", s.userHandler.List)
		r.Post("/proxy-users", s.userHandler.Create)
		r.Get("/proxy-users/{id}", s.userHandler.Get)
		r.Put("/proxy-users/{id}", s.userHandler.Update)
		r.Delete("/proxy-users/{id}", s.userHandler.Delete)

		// Proxy Pools
		r.Get("/pools", s.poolHandler.List)
		r.Post("/pools", s.poolHandler.Create)
		r.Get("/pools/geo-summary", s.poolHandler.GeoSummary)
		r.Get("/pools/geo-countries", s.poolHandler.GeoByCountry)
		r.Get("/pools/geo-cities/{country_code}", s.poolHandler.GeoCitiesByCountry)
		r.Get("/pools/isp-list", s.poolHandler.GetISPList)
		r.Get("/pools/tag-list", s.poolHandler.GetTagList)
		r.Get("/pools/{id}", s.poolHandler.Get)
		r.Put("/pools/{id}", s.poolHandler.Update)
		r.Delete("/pools/{id}", s.poolHandler.Delete)
		r.Get("/pools/{id}/proxies", s.poolHandler.GetProxies)
		r.Post("/pools/{id}/proxies", s.poolHandler.AddProxies)
		r.Delete("/pools/{id}/proxies", s.poolHandler.RemoveProxies)
		r.Post("/pools/{id}/sync", s.poolHandler.Sync)
		r.Get("/pools/{id}/export", s.poolHandler.Export)
		r.Post("/pools/{id}/health-check", s.poolHandler.HealthCheck)
		r.Get("/pools/{id}/health-check/jobs", s.poolHandler.HealthCheckJobs)
		r.Get("/pools/{id}/health-check/{job_id}", s.poolHandler.HealthCheckStatus)
		// Alert rules
		r.Get("/pools/{id}/alert-rules", s.poolHandler.ListAlertRules)
		r.Post("/pools/{id}/alert-rules", s.poolHandler.CreateAlertRule)
		r.Put("/pools/{id}/alert-rules/{rule_id}", s.poolHandler.UpdateAlertRule)
		r.Delete("/pools/{id}/alert-rules/{rule_id}", s.poolHandler.DeleteAlertRule)
	})

	// WebSocket routes — protected via token query param
	s.router.With(JWTMiddleware(s.jwtSecret)).Get("/ws/dashboard", s.websocketHandler.DashboardWebSocket)
	s.router.With(JWTMiddleware(s.jwtSecret)).Get("/ws/logs", s.websocketHandler.LogsWebSocket)
}

// Start starts the API server
func (s *Server) Start() error {
	s.logger.Info("starting API server", "port", s.port)

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("API server failed: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the API server
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down API server")
	return s.server.Shutdown(ctx)
}

// SetProxyServer sets the proxy server reference after initialization
func (s *Server) SetProxyServer(ps ProxyServer) {
	s.proxyServer = ps
}

// ReloadProxyPool reloads the proxy pool from database
//	@Summary		Reload proxy pool
//	@Description	Reload proxy pool from database
//	@Tags			proxies
//	@Produce		json
//	@Success		200	{object}	map[string]interface{}	"Reload confirmation"
//	@Failure		500	{object}	models.ErrorResponse
//	@Failure		503	{object}	models.ErrorResponse
//	@Router			/proxies/reload [post]
func (s *Server) ReloadProxyPool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.proxyServer == nil {
		s.logger.Error("proxy server not initialized")
		http.Error(w, "proxy server not available", http.StatusServiceUnavailable)
		return
	}

	s.logger.Info("reloading proxy pool via API request")

	if err := s.proxyServer.ReloadSettings(ctx); err != nil {
		s.logger.Error("failed to reload proxy pool", "error", err)
		http.Error(w, fmt.Sprintf("failed to reload proxy pool: %v", err), http.StatusInternalServerError)
		return
	}

	s.logger.Info("proxy pool reloaded successfully")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"success","message":"Proxy pool reloaded successfully"}`))
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
func (s *Server) InvalidateProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
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
			http.Error(w, `{"error":"invalid domain"}`, http.StatusBadRequest)
			return
		}
		if d <= 0 {
			d = 24 * time.Hour // same "until reactivated" default as SetCooldown
		}
		until := time.Now().Add(d)

		proxyObj, err := s.proxyRepo.SetDomainCooldown(r.Context(), id, domain, until, body.Reason)
		if err != nil {
			s.logger.Error("failed to invalidate proxy for domain", "id", id, "domain", domain, "error", err)
			http.Error(w, `{"error":"failed to invalidate proxy"}`, http.StatusInternalServerError)
			return
		}
		if proxyObj == nil {
			http.Error(w, `{"error":"proxy not found"}`, http.StatusNotFound)
			return
		}

		// Make it effective immediately. Sessions bound to this proxy are kept;
		// they rebind lazily on their next request to the cooled domain.
		if s.proxyServer != nil {
			s.proxyServer.SetDomainCooldown(id, domain, until, body.Reason)
		}

		s.logger.Info("proxy invalidated for domain",
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

	proxyObj, err := s.proxyRepo.SetCooldown(r.Context(), id, d, body.Reason)
	if err != nil {
		s.logger.Error("failed to invalidate proxy", "id", id, "error", err)
		http.Error(w, `{"error":"failed to invalidate proxy"}`, http.StatusInternalServerError)
		return
	}
	if proxyObj == nil {
		http.Error(w, `{"error":"proxy not found"}`, http.StatusNotFound)
		return
	}

	// Evict from live rotation + drop bound sessions immediately.
	if s.proxyServer != nil {
		s.proxyServer.EvictProxy(id)
	}

	s.logger.Info("proxy invalidated", "id", id, "minutes", body.Minutes, "reason", body.Reason)
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
func (s *Server) ReactivateProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
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
			http.Error(w, `{"error":"invalid domain"}`, http.StatusBadRequest)
			return
		}
		cleared, err := s.proxyRepo.ClearDomainCooldown(r.Context(), id, domain)
		if err != nil {
			s.logger.Error("failed to reactivate proxy for domain", "id", id, "domain", domain, "error", err)
			http.Error(w, `{"error":"failed to reactivate proxy"}`, http.StatusInternalServerError)
			return
		}
		if s.proxyServer != nil {
			s.proxyServer.ClearDomainCooldown(id, domain)
		}
		s.logger.Info("proxy reactivated for domain", "id", id, "domain", domain)
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

	proxyObj, err := s.proxyRepo.ClearCooldown(r.Context(), id)
	if err != nil {
		s.logger.Error("failed to reactivate proxy", "id", id, "error", err)
		http.Error(w, `{"error":"failed to reactivate proxy"}`, http.StatusInternalServerError)
		return
	}
	if proxyObj == nil {
		http.Error(w, `{"error":"proxy not found"}`, http.StatusNotFound)
		return
	}

	// Full reactivation also drops any domain-scoped cooldowns.
	if _, err := s.proxyRepo.ClearAllDomainCooldowns(r.Context(), id); err != nil {
		s.logger.Warn("failed to clear proxy domain cooldowns", "id", id, "error", err)
	}
	if s.proxyServer != nil {
		s.proxyServer.ClearProxyDomainCooldowns(id)
	}

	s.logger.Info("proxy reactivated", "id", id)
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
func (s *Server) ListDomainCooldowns(w http.ResponseWriter, r *http.Request) {
	var cooldowns []models.ProxyDomainCooldown
	if s.proxyServer != nil {
		cooldowns = s.proxyServer.ListDomainCooldowns()
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
func (s *Server) ListSessions(w http.ResponseWriter, r *http.Request) {
	var sessions []proxy.SessionInfo
	if s.proxyServer != nil {
		sessions = s.proxyServer.ListSessions()
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
func (s *Server) ReleaseSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		PoolID *int   `json:"pool_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		http.Error(w, `{"error":"token is required"}`, http.StatusBadRequest)
		return
	}
	if s.proxyServer == nil {
		http.Error(w, `{"error":"proxy server not available"}`, http.StatusServiceUnavailable)
		return
	}

	released := 0
	if body.PoolID != nil {
		if s.proxyServer.ReleaseSession(*body.PoolID, body.Token) {
			released = 1
		}
	} else {
		released = s.proxyServer.ReleaseSessionToken(body.Token)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "released", "count": released})
}

// serveSwaggerJSON serves the swagger.json file
func (s *Server) serveSwaggerJSON(w http.ResponseWriter, r *http.Request) {
	// Serve from the docs directory in the project root
	swaggerPath := "docs/swagger.json"
	http.ServeFile(w, r, swaggerPath)
}

// generateJWTSecret generates a cryptographically secure random JWT secret
func generateJWTSecret() string {
	// Generate 32 random bytes (256 bits)
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to timestamp-based random if crypto/rand fails
		return fmt.Sprintf("fallback-secret-%d", time.Now().UnixNano())
	}

	// Convert to hex string (64 characters)
	return hex.EncodeToString(bytes)
}
