package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/alpkeskin/rota/core/internal/api/handlers"
	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/services"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// Deps holds the repositories and services the API layer needs. They are built
// once by the composition root (main) and injected, so the API server itself
// constructs no repositories or background services.
type Deps struct {
	ProxyRepo         *repository.ProxyRepository
	EventStore        events.Store
	SettingsRepo      *repository.SettingsRepository
	DashboardRepo     *repository.DashboardRepository
	SourceRepo        *repository.SourceRepository
	FormatHistoryRepo *repository.FormatHistoryRepository
	PoolRepo          *repository.PoolRepository
	UserRepo          *repository.UserRepository
	AdminRepo         *repository.AdminRepository

	SourceSvc *services.SourceService
	PoolSvc   *services.PoolService
}

// Server represents the API server
type Server struct {
	router            *chi.Mux
	server            *http.Server
	logger            *logger.Logger
	db                *database.DB
	port              int
	jwtSecret         string
	corsOrigins       []string
	webDir            string
	trustProxyHeaders bool
	authRL            *authRateLimiter
	controlRL         *authRateLimiter
	userRepo          *repository.UserRepository

	// Proxy server reference for reloading
	proxyServer handlers.ProxyServer

	// Handlers
	authHandler          *handlers.AuthHandler
	healthHandler        *handlers.HealthHandler
	dashboardHandler     *handlers.DashboardHandler
	proxyHandler         *handlers.ProxyHandler
	logsHandler          *handlers.LogsHandler
	settingsHandler      *handlers.SettingsHandler
	websocketHandler     *handlers.WebSocketHandler
	metricsHandler       *handlers.MetricsHandler
	sourceHandler        *handlers.SourceHandler
	formatHistoryHandler *handlers.FormatHistoryHandler
	poolHandler          *handlers.PoolHandler
	userHandler          *handlers.UserHandler
	proxyControlHandler  *handlers.ProxyControlHandler
}

// New creates a new API server instance from injected dependencies. It builds
// only API-layer objects (handlers, middleware, HTTP server); repositories and
// background services are owned by the composition root and passed in via deps.
func New(cfg *config.Config, log *logger.Logger, db *database.DB, deps Deps) *Server {
	// Seed admin credentials from env on first start (no-op if already seeded)
	if err := deps.AdminRepo.Seed(context.Background(), cfg.AdminUser, cfg.AdminPass); err != nil {
		log.Warn("failed to seed admin credentials", "error", err)
	}

	// Prefer a configured JWT secret so tokens survive restarts and work across
	// replicas. Falling back to a per-boot random secret keeps single-node dev
	// zero-config, at the cost of logging everyone out on every restart.
	jwtSecret := cfg.JWTSecret
	if jwtSecret == "" {
		jwtSecret = generateJWTSecret()
		log.Warn("JWT_SECRET not set: generated an ephemeral secret; all sessions will be invalidated on restart and multi-replica deployments will not share sessions")
	}

	// Usage tracker + health checker back only the on-demand proxy-test endpoint.
	tracker := proxy.NewUsageTracker(deps.EventStore, deps.ProxyRepo)
	healthChecker := proxy.NewHealthChecker(deps.ProxyRepo, deps.SettingsRepo, tracker, log)

	// Initialize handlers
	authHandler := handlers.NewAuthHandler(deps.SettingsRepo, deps.AdminRepo, log, jwtSecret, cfg.AdminUser, cfg.AdminPass)
	healthHandler := handlers.NewHealthHandler(db, deps.ProxyRepo, log)
	dashboardHandler := handlers.NewDashboardHandler(deps.DashboardRepo, deps.ProxyRepo, log)
	proxyHandler := handlers.NewProxyHandler(deps.ProxyRepo, healthChecker, log)
	logsHandler := handlers.NewLogsHandler(deps.EventStore, log)
	settingsHandler := handlers.NewSettingsHandler(deps.SettingsRepo, log, nil) // onUpdate set below
	websocketHandler := handlers.NewWebSocketHandler(deps.DashboardRepo, deps.ProxyRepo, deps.EventStore, log, cfg.CORSAllowedOrigins)
	metricsHandler := handlers.NewMetricsHandler(log)
	sourceHandler := handlers.NewSourceHandler(deps.SourceRepo, deps.FormatHistoryRepo, deps.SourceSvc, log)
	formatHistoryHandler := handlers.NewFormatHistoryHandler(deps.FormatHistoryRepo, log)
	poolHandler := handlers.NewPoolHandler(deps.PoolRepo, deps.PoolSvc, log)
	userHandler := handlers.NewUserHandler(deps.UserRepo, deps.PoolRepo, log)
	proxyControlHandler := handlers.NewProxyControlHandler(deps.ProxyRepo, deps.PoolRepo, log)

	// Auth rate limiter (per-IP block + global lockout)
	authRL := newAuthRateLimiter(
		cfg.AuthIPMaxAttempts,
		cfg.AuthIPWindowMinutes,
		cfg.AuthIPBlockMinutes,
		cfg.AuthGlobalMaxPerMinute,
		cfg.AuthGlobalLockoutMin,
		cfg.TrustProxyHeaders,
		log,
	)

	// A separate limiter instance protects the client-control endpoints (which
	// accept proxy-user Basic credentials) so brute-forcing proxy passwords is
	// blocked with the same thresholds — without heavy but legitimate control
	// traffic ever tripping the login endpoint's global lockout.
	controlRL := newAuthRateLimiter(
		cfg.AuthIPMaxAttempts,
		cfg.AuthIPWindowMinutes,
		cfg.AuthIPBlockMinutes,
		cfg.AuthGlobalMaxPerMinute,
		cfg.AuthGlobalLockoutMin,
		cfg.TrustProxyHeaders,
		log,
	)

	s := &Server{
		router:               chi.NewRouter(),
		logger:               log,
		db:                   db,
		port:                 cfg.APIPort,
		jwtSecret:            jwtSecret,
		corsOrigins:          cfg.CORSAllowedOrigins,
		webDir:               cfg.WebDir,
		trustProxyHeaders:    cfg.TrustProxyHeaders,
		authRL:               authRL,
		controlRL:            controlRL,
		userRepo:             deps.UserRepo,
		authHandler:          authHandler,
		healthHandler:        healthHandler,
		dashboardHandler:     dashboardHandler,
		proxyHandler:         proxyHandler,
		logsHandler:          logsHandler,
		settingsHandler:      settingsHandler,
		websocketHandler:     websocketHandler,
		metricsHandler:       metricsHandler,
		sourceHandler:        sourceHandler,
		formatHistoryHandler: formatHistoryHandler,
		poolHandler:          poolHandler,
		userHandler:          userHandler,
		proxyControlHandler:  proxyControlHandler,
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

	// Wire user invalidation: when a proxy user is updated or deleted, drop the
	// proxy server's cached auth entry for that user so disables, password
	// changes and pool reassignments take effect immediately rather than after
	// the auth cache TTL.
	userHandler.SetOnUserChanged(func(username string) {
		if s.proxyServer != nil {
			s.proxyServer.InvalidateUser(username)
		}
	})

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

	// CORS middleware. Credentialed CORS with a wildcard origin is invalid per
	// the Fetch spec and is rejected by browsers, so only enable credentials
	// when explicit origins are configured. The dashboard authenticates with a
	// Bearer token (not cookies), so the wildcard dev default needs no
	// credentials anyway.
	allowWildcard := false
	for _, o := range s.corsOrigins {
		if o == "*" {
			allowWildcard = true
			break
		}
	}
	s.router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.corsOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: !allowWildcard,
		MaxAge:           300,
	}))

	s.router.Use(middleware.RequestID)
	// RealIP rewrites RemoteAddr from X-Forwarded-For / X-Real-IP. Only do that
	// when an upstream reverse proxy is trusted to set them; otherwise a directly
	// exposed API would let a client choose its own apparent address, which the
	// login rate limiter keys on.
	if s.trustProxyHeaders {
		s.router.Use(middleware.RealIP)
	}
	s.router.Use(LoggerMiddleware(s.logger))
	s.router.Use(middleware.Recoverer)
	// No global timeout — health-check routes need minutes; individual routes handle their own timeouts
}

// setupRoutes configures all API routes
func (s *Server) setupRoutes() {
	// ── Fully public routes ────────────────────────────────────────────────
	s.router.Get("/health", s.healthHandler.Health)
	// HEAD too — probes like wget --spider and some load balancers send HEAD,
	// and chi would otherwise answer 405. net/http drops the body for HEAD.
	s.router.Head("/health", s.healthHandler.Health)

	// Auth: only login is public; everything else requires a valid JWT
	// Auth rate limiter wraps the login handler — per-IP block + global lockout
	s.router.With(s.authRL.Middleware()).Post("/api/v1/auth/login", s.authHandler.Login)

	// ── Protected routes ───────────────────────────────────────────────────
	s.router.Route("/api/v1", func(r chi.Router) {
		// Client-control endpoints: accessible with an admin JWT or with
		// proxy-user Basic credentials, so the client actually using the proxy
		// can invalidate/release without holding an admin token. Handlers
		// scope proxy-user calls to the user's own pools; the brute-force
		// limiter guards the Basic path.
		r.Group(func(cr chi.Router) {
			cr.Use(s.controlRL.Middleware())
			cr.Use(JWTOrProxyUserMiddleware(s.jwtSecret, s.userRepo, s.logger))

			cr.Post("/proxies/{id}/invalidate", s.proxyControlHandler.InvalidateProxy)
			cr.Post("/sessions/invalidate", s.proxyControlHandler.InvalidateSession)
			cr.Post("/sessions/release", s.proxyControlHandler.ReleaseSession)
		})

		// Everything else requires an admin JWT.
		r.Group(func(r chi.Router) {
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
			r.Get("/dashboard/charts/traffic", s.dashboardHandler.GetTrafficChart)
			r.Get("/dashboard/charts/response-time", s.dashboardHandler.GetResponseTimeChart)
			r.Get("/dashboard/charts/success-rate", s.dashboardHandler.GetSuccessRateChart)

			// Proxy management
			r.Get("/proxies", s.proxyHandler.List)
			r.Post("/proxies", s.proxyHandler.Create)
			r.Post("/proxies/bulk", s.proxyHandler.BulkCreate)
			r.Post("/proxies/bulk-delete", s.proxyHandler.BulkDelete)
			r.Post("/proxies/bulk-test", s.proxyHandler.BulkTest)
			r.Get("/proxies/bulk-test", s.proxyHandler.BulkTestLatest)
			r.Get("/proxies/bulk-test/{job_id}", s.proxyHandler.BulkTestStatus)
			r.Delete("/proxies", s.proxyHandler.DeleteAll)
			r.Get("/proxies/export", s.proxyHandler.Export)
			r.Put("/proxies/{id}", s.proxyHandler.Update)
			r.Delete("/proxies/{id}", s.proxyHandler.Delete)
			r.Post("/proxies/{id}/test", s.proxyHandler.Test)
			r.Post("/proxies/{id}/reactivate", s.proxyControlHandler.ReactivateProxy)
			r.Get("/proxies/domain-cooldowns", s.proxyControlHandler.ListDomainCooldowns)
			r.Post("/proxies/reload", s.proxyControlHandler.ReloadProxyPool)

			// Sticky sessions (session rotation method)
			r.Get("/sessions", s.proxyControlHandler.ListSessions)

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

			// Line-format history (custom formats used for sources/imports)
			r.Get("/format-history", s.formatHistoryHandler.List)
			r.Post("/format-history", s.formatHistoryHandler.Record)
			r.Delete("/format-history/{id}", s.formatHistoryHandler.Delete)

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
	})

	// WebSocket routes — protected via token query param
	s.router.With(JWTMiddleware(s.jwtSecret)).Get("/ws/dashboard", s.websocketHandler.DashboardWebSocket)
	s.router.With(JWTMiddleware(s.jwtSecret)).Get("/ws/logs", s.websocketHandler.LogsWebSocket)

	// Serve the built dashboard (SPA) from the same origin as the API when a web
	// directory is configured — one binary, one port, no separate Node runtime.
	if s.webDir != "" {
		s.router.NotFound(spaHandler(s.webDir))
		s.logger.Info("serving dashboard SPA", "dir", s.webDir)
	}
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

// SetProxyServer sets the proxy server reference after initialization. It is
// also handed to the proxy-control handler, which drives reload/session/cooldown
// endpoints against the running proxy server.
func (s *Server) SetProxyServer(ps handlers.ProxyServer) {
	s.proxyServer = ps
	s.proxyControlHandler.SetProxyServer(ps)
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
