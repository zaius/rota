// Package main provides the entry point for the Rota Proxy Server
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zaius/rota/core/internal/api"
	"github.com/zaius/rota/core/internal/config"
	"github.com/zaius/rota/core/internal/database"
	"github.com/zaius/rota/core/internal/events"
	"github.com/zaius/rota/core/internal/metrics"
	"github.com/zaius/rota/core/internal/proxy"
	"github.com/zaius/rota/core/internal/repository"
	"github.com/zaius/rota/core/internal/services"
	"github.com/zaius/rota/core/pkg/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// metricsHTTPHandler returns the /metrics handler, or nil when metrics are
// disabled (a nil api.Deps.Metrics leaves the route unregistered).
func metricsHTTPHandler(p *metrics.Provider) http.Handler {
	if p == nil {
		return nil
	}
	return p.Handler()
}

func run() error {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Initialize logger
	log := logger.New(cfg.LogLevel)
	log.Info("starting application",
		"proxy_port", cfg.ProxyPort,
		"api_port", cfg.APIPort,
	)

	ctx := context.Background()

	// Install the metrics pipeline before anything records: a Prometheus
	// /metrics endpoint on the API server, plus OTLP push when the standard
	// OTEL_EXPORTER_OTLP_* env vars are set. When disabled, no SDK is
	// installed and every instrument in internal/metrics stays a no-op.
	var metricsProvider *metrics.Provider
	if cfg.MetricsEnabled {
		var err error
		metricsProvider, err = metrics.Setup(ctx, cfg.MetricsBearerToken, log)
		if err != nil {
			return fmt.Errorf("failed to set up metrics: %w", err)
		}
	}

	// Initialize database
	db, err := database.New(ctx, &cfg.Database, database.DefaultConfig(), log)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	// Run database migrations
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	// Create the event store — the single boundary for time-series event data
	// (request and tunnel history).
	eventStore, err := events.NewParquetStore(&cfg.Parquet, log, metrics.RecordEventsWritten)
	if err != nil {
		return fmt.Errorf("failed to open event store: %w", err)
	}
	defer eventStore.Close()

	// Create repositories once — this is the single place they are constructed.
	proxyRepo := repository.NewProxyRepository(db)
	settingsRepo := repository.NewSettingsRepository(db)
	dashboardRepo := repository.NewDashboardRepository(db, eventStore)
	sourceRepo := repository.NewSourceRepository(db)
	formatHistoryRepo := repository.NewFormatHistoryRepository(db)

	// Seed any missing default settings from the single Go-defined source of
	// truth (migrations no longer seed settings). No-op for keys already set.
	if err := settingsRepo.SeedDefaults(ctx); err != nil {
		return fmt.Errorf("failed to seed default settings: %w", err)
	}

	// Build background services once and hand their lifecycle to a single
	// manager, so they start and stop with the process instead of leaking on a
	// never-cancelled context.Background().
	healthChecker := proxy.NewHealthChecker(proxyRepo, settingsRepo, proxy.NewUsageTracker(eventStore, proxyRepo), log)
	geoSvc := services.NewGeoIPService(log)
	sourceSvc := services.NewSourceService(sourceRepo, proxyRepo, geoSvc, log)
	healthCheckSvc := services.NewHealthCheckScheduler(healthChecker, settingsRepo, log)
	cleanupSvc := services.NewProxyCleanupService(proxyRepo, settingsRepo, eventStore, log)
	historyCleanupSvc := services.NewHistoryCleanupService(eventStore, log)
	statsRefresher := services.NewStatsRefresher(eventStore, proxyRepo, time.Minute, log)

	backgroundSvcs := []services.Service{geoSvc, sourceSvc, healthCheckSvc, cleanupSvc, historyCleanupSvc, statsRefresher}
	if metricsProvider != nil {
		backgroundSvcs = append(backgroundSvcs, metrics.NewFleetPoller(db, 30*time.Second, log))
	}
	svcManager := services.NewManager(log, backgroundSvcs...)

	// Optional HTTPS interception. Without a configured CA the inspector is
	// nil and requests for inspection fail; other CONNECT tunnels stay opaque.
	var inspector *proxy.TLSInspector
	if cfg.TLSInspect.Enabled() {
		ca, err := proxy.LoadCertAuthority(cfg.TLSInspect.CACertFile, cfg.TLSInspect.CAKeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS interception CA: %w", err)
		}
		inspector = proxy.NewTLSInspector(ca, cfg.TLSInspect.BypassDomains, log)
		log.Info("HTTPS interception available for requests that name a profile",
			"ca_subject", ca.Subject(),
			"ca_expires", ca.NotAfter().Format(time.RFC3339),
			"bypass_domains", len(cfg.TLSInspect.BypassDomains),
		)
	} else {
		log.Info("HTTPS interception disabled: TLS_INSPECT_CA_CERT and TLS_INSPECT_CA_KEY are unset")
	}

	// Create servers
	proxyServer, err := proxy.New(cfg.ProxyPort, cfg.Password, log, eventStore, proxyRepo, settingsRepo, inspector)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}
	apiServer := api.New(cfg, log, api.Deps{
		ProxyRepo:         proxyRepo,
		SettingsRepo:      settingsRepo,
		DashboardRepo:     dashboardRepo,
		SourceRepo:        sourceRepo,
		FormatHistoryRepo: formatHistoryRepo,
		SourceSvc:         sourceSvc,
		HealthChecker:     healthChecker,
		Metrics:           metricsHTTPHandler(metricsProvider),
	})

	// Set proxy server reference in API server for reload functionality
	apiServer.SetProxyServer(proxyServer)

	// Start background services (cancelled during graceful shutdown below).
	svcManager.Start(ctx)

	// Start servers in goroutines
	errChan := make(chan error, 2)

	// Start proxy server
	go func() {
		if err := proxyServer.Start(); err != nil {
			errChan <- fmt.Errorf("proxy server error: %w", err)
		}
	}()

	// Start API server
	go func() {
		if err := apiServer.Start(); err != nil {
			errChan <- fmt.Errorf("API server error: %w", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errChan:
		log.Error("server error", "error", err)
		return err
	case sig := <-quit:
		log.Info("received shutdown signal", "signal", sig.String())
	}

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Info("shutting down servers...")

	// Stop background services first so they don't act mid-shutdown, then close
	// the HTTP servers.
	if err := svcManager.Stop(ctx); err != nil {
		log.Warn("background services did not stop cleanly", "error", err)
	}

	// Shutdown both servers
	var shutdownWg sync.WaitGroup
	shutdownErrors := make(chan error, 2)

	shutdownWg.Go(func() {
		if err := proxyServer.Shutdown(ctx); err != nil {
			shutdownErrors <- fmt.Errorf("proxy server shutdown error: %w", err)
		}
	})

	shutdownWg.Go(func() {
		if err := apiServer.Shutdown(ctx); err != nil {
			shutdownErrors <- fmt.Errorf("API server shutdown error: %w", err)
		}
	})

	// Wait for shutdown to complete
	shutdownWg.Wait()
	close(shutdownErrors)

	// Stop the metrics pipeline last, so the final OTLP push (if configured)
	// still sees everything the servers recorded on the way down.
	if metricsProvider != nil {
		if err := metricsProvider.Shutdown(ctx); err != nil {
			log.Warn("metrics provider did not shut down cleanly", "error", err)
		}
	}

	// Collect any shutdown errors
	var shutdownErr error
	for err := range shutdownErrors {
		if shutdownErr == nil {
			shutdownErr = err
		} else {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}

	if shutdownErr != nil {
		log.Error("shutdown completed with errors", "error", shutdownErr)
		return shutdownErr
	}

	log.Info("shutdown completed successfully")
	return nil
}
