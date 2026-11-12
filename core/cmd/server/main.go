// Package main provides the entry point for the Rota Proxy Server
//
//	@title			Rota Proxy API
//	@version		1.0.0
//	@description	A high-performance proxy rotation server with health monitoring and intelligent routing
//	@description	Provides comprehensive API for managing proxy servers, monitoring their health,
//	@description	and configuring rotation strategies.
//
//	@contact.name	API Support
//	@contact.url	https://github.com/alpkeskin/rota
//
//	@license.name	LICENSE
//	@license.url	https://github.com/alpkeskin/rota/blob/main/LICENSE
//
//	@host		localhost:8001
//	@BasePath	/api/v1
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Type "Bearer" followed by a space and JWT token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/alpkeskin/rota/core/internal/api"
	"github.com/alpkeskin/rota/core/internal/authlimit"
	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/metrics"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/services"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

func main() {
	command := run
	if len(os.Args) > 1 && os.Args[1] == "import-postgres-history" {
		command = func() error { return importPostgresHistory(os.Args[2:]) }
	}
	if err := command(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// importPostgresHistory copies request and tunnel history from the Postgres
// event tables into the parquet store, then exits. Rota may keep running.
func importPostgresHistory(args []string) error {
	flags := flag.NewFlagSet("import-postgres-history", flag.ContinueOnError)
	beforeFlag := flags.String("before", "", "copy events from before this RFC 3339 time (default: the earliest event already in the parquet store)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var before time.Time
	if *beforeFlag != "" {
		var err error
		if before, err = time.Parse(time.RFC3339Nano, *beforeFlag); err != nil {
			return fmt.Errorf("invalid -before: %w", err)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}
	log := logger.New(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.New(ctx, &cfg.Database, nil, log)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	return events.ImportPostgresHistory(ctx, db.Pool, cfg.Parquet.Dir, before, func(r events.ImportReport) {
		log.Info("imported event history", "table", r.Table, "day", r.Day, "action", r.Action, "rows", r.Rows)
	})
}

// metricsHTTPHandler returns the /metrics handler, or nil when metrics are
// disabled (a nil api.Deps.Metrics leaves the route unregistered).
func metricsHTTPHandler(p *metrics.Provider) http.Handler {
	if p == nil {
		return nil
	}
	return p.Handler()
}

// flushHistory writes out buffered request and tunnel events. It gets its own
// deadline: the servers may already have spent the shutdown context waiting on
// requests that outlived it. Calls after the first return at once.
func flushHistory(w *events.BatchWriter, log *logger.Logger) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		log.Warn("event history was not fully written", "error", err)
	}
}

// openedEventStore is one event backend and how to shut it down.
type openedEventStore struct {
	store events.Store
	batch *events.BatchWriter // nil when the backend takes events directly
	close func()
}

// openEventStore opens a backend by name. Database backends get a batch
// writer, so a database takes one multi-row insert per flush instead of a
// round trip per event. The parquet store appends each event directly.
func openEventStore(ctx context.Context, name string, cfg *config.Config, db *database.DB, log *logger.Logger, observe events.FlushObserver) (*openedEventStore, error) {
	var store events.Store
	closeStore := func() {}
	switch name {
	case "parquet":
		files, err := events.NewParquetStore(&cfg.Parquet, log, observe)
		if err != nil {
			return nil, fmt.Errorf("failed to open parquet event store: %w", err)
		}
		return &openedEventStore{store: files, close: func() { files.Close() }}, nil
	case "postgres":
		store = events.NewPostgresStore(db, log)
	case "clickhouse":
		ch, err := events.NewClickHouseStore(ctx, &cfg.ClickHouse, log)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to clickhouse event store: %w", err)
		}
		store, closeStore = ch, func() { ch.Close() }
	default:
		return nil, fmt.Errorf("unsupported event store: %s", name)
	}
	batch := events.NewBatchWriter(store, log, observe)
	return &openedEventStore{store: batch, batch: batch, close: closeStore}, nil
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
	// (request and tunnel history). Config selects the backend, and optionally
	// a mirror that also receives every write. Only the primary reports
	// metrics, so the written-events counter counts each event once.
	primaryEvents, err := openEventStore(ctx, cfg.EventStore, cfg, db, log, metrics.RecordEventsWritten)
	if err != nil {
		return err
	}
	defer primaryEvents.close()
	eventStore := primaryEvents.store
	batchWriters := []*events.BatchWriter{primaryEvents.batch}

	if cfg.EventStoreMirror != "" {
		mirrorEvents, err := openEventStore(ctx, cfg.EventStoreMirror, cfg, db, log, nil)
		if err != nil {
			return err
		}
		defer mirrorEvents.close()
		eventStore = events.NewMirrorStore(eventStore, mirrorEvents.store, log)
		batchWriters = append(batchWriters, mirrorEvents.batch)
		log.Info("mirroring event writes", "primary", cfg.EventStore, "mirror", cfg.EventStoreMirror)
	}
	flushAllHistory := func() {
		for _, w := range batchWriters {
			flushHistory(w, log)
		}
	}
	// Registered after the store and database closes, so it runs before them
	// on every return path, including a server failing to start.
	defer flushAllHistory()

	// Create repositories once — this is the single place they are constructed.
	proxyRepo := repository.NewProxyRepository(db)
	settingsRepo := repository.NewSettingsRepository(db)
	poolRepo := repository.NewPoolRepository(db)
	userRepo := repository.NewUserRepository(db)
	dashboardRepo := repository.NewDashboardRepository(db, eventStore)
	sourceRepo := repository.NewSourceRepository(db)
	formatHistoryRepo := repository.NewFormatHistoryRepository(db)
	adminRepo := repository.NewAdminRepository(db)

	// Seed any missing default settings from the single Go-defined source of
	// truth (migrations no longer seed settings). No-op for keys already set.
	if err := settingsRepo.SeedDefaults(ctx); err != nil {
		return fmt.Errorf("failed to seed default settings: %w", err)
	}

	// Build background services once and hand their lifecycle to a single
	// manager, so they start and stop with the process instead of leaking on a
	// never-cancelled context.Background().
	geoSvc := services.NewGeoIPService(log, services.NewLocalGeoDB(cfg.GeoIP, log))
	sourceSvc := services.NewSourceService(sourceRepo, proxyRepo, poolRepo, geoSvc, log)
	poolSvc := services.NewPoolService(poolRepo, proxyRepo, settingsRepo, log)
	alertWatcher := services.NewAlertWatcher(poolRepo, log)
	cleanupSvc := services.NewProxyCleanupService(proxyRepo, settingsRepo, eventStore, log)
	historyCleanupSvc := services.NewHistoryCleanupService(eventStore, log)
	statsRefresher := services.NewStatsRefresher(eventStore, proxyRepo, time.Minute, log)

	backgroundSvcs := []services.Service{geoSvc, sourceSvc, poolSvc, alertWatcher, cleanupSvc, historyCleanupSvc, statsRefresher}
	if metricsProvider != nil {
		backgroundSvcs = append(backgroundSvcs, metrics.NewFleetPoller(db, poolRepo, 30*time.Second, log))
	}
	svcManager := services.NewManager(log, backgroundSvcs...)

	// Optional HTTPS interception. Without a configured CA the inspector is
	// nil and requests for inspection fail. The per-user inspect_tls flag
	// requires a server-side CA; other CONNECT tunnels stay opaque.
	var inspector *proxy.TLSInspector
	if cfg.TLSInspect.Enabled() {
		ca, err := proxy.LoadCertAuthority(cfg.TLSInspect.CACertFile, cfg.TLSInspect.CAKeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS interception CA: %w", err)
		}
		inspector = proxy.NewTLSInspector(ca, cfg.TLSInspect.BypassDomains, log)
		log.Info("HTTPS interception available for opted-in proxy users",
			"ca_subject", ca.Subject(),
			"ca_expires", ca.NotAfter().Format(time.RFC3339),
			"bypass_domains", len(cfg.TLSInspect.BypassDomains),
		)
	} else {
		log.Info("HTTPS interception disabled: TLS_INSPECT_CA_CERT and TLS_INSPECT_CA_KEY are unset")
	}

	// Create servers
	authLimiter := authlimit.New(cfg.AuthIPMaxAttempts, time.Duration(cfg.AuthIPWindowMinutes)*time.Minute, time.Duration(cfg.AuthIPBlockMinutes)*time.Minute)
	proxyServer, err := proxy.New(cfg.ProxyPort, log, db, eventStore, proxyRepo, poolRepo, userRepo, settingsRepo, inspector, authLimiter)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}
	apiServer := api.New(cfg, log, db, api.Deps{
		ProxyRepo:         proxyRepo,
		EventStore:        eventStore,
		SettingsRepo:      settingsRepo,
		DashboardRepo:     dashboardRepo,
		SourceRepo:        sourceRepo,
		FormatHistoryRepo: formatHistoryRepo,
		PoolRepo:          poolRepo,
		UserRepo:          userRepo,
		AdminRepo:         adminRepo,
		SourceSvc:         sourceSvc,
		PoolSvc:           poolSvc,
		Metrics:           metricsHTTPHandler(metricsProvider),
		AuthLimiter:       authLimiter,
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

	// Flush buffered history once the servers have stopped producing it, while
	// the metrics pipeline can still report the writes.
	flushAllHistory()

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
