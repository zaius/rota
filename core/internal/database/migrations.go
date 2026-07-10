package database

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migration represents a database migration
type Migration struct {
	Version     int
	Description string
	Up          string
	Down        string
}

// migrations holds all database migrations
var migrations = []Migration{
	{
		Version:     1,
		Description: "Create initial schema",
		Up: `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version INT PRIMARY KEY,
				description TEXT NOT NULL,
				applied_at TIMESTAMP NOT NULL DEFAULT NOW()
			);
		`,
		Down: `
			DROP TABLE IF EXISTS schema_migrations;
		`,
	},
	{
		Version:     2,
		Description: "Enable TimescaleDB extension when available",
		// TimescaleDB is an optional accelerator: rota runs on plain Postgres,
		// so only create the extension where the server actually ships it.
		// On managed Postgres (e.g. Azure Flexible Server) CREATE EXTENSION is
		// admin-only, and the permission check fires before IF NOT EXISTS gets
		// a chance to skip — so also only issue it when the extension is
		// actually missing (an admin pre-creates it there).
		Up: `
			DO $do$
			BEGIN
				IF NOT EXISTS (SELECT FROM pg_extension WHERE extname = 'timescaledb')
				   AND EXISTS (SELECT FROM pg_available_extensions WHERE name = 'timescaledb') THEN
					CREATE EXTENSION timescaledb;
				END IF;
			END
			$do$;
		`,
		Down: `
			DROP EXTENSION IF EXISTS timescaledb;
		`,
	},
	{
		Version:     3,
		Description: "Create proxies table",
		Up: `
			CREATE TABLE IF NOT EXISTS proxies (
				id SERIAL PRIMARY KEY,
				address VARCHAR(255) NOT NULL,
				protocol VARCHAR(20) NOT NULL DEFAULT 'http',
				username VARCHAR(255),
				password TEXT,
				status VARCHAR(20) NOT NULL DEFAULT 'idle',
				requests BIGINT NOT NULL DEFAULT 0,
				successful_requests BIGINT NOT NULL DEFAULT 0,
				failed_requests BIGINT NOT NULL DEFAULT 0,
				avg_response_time INTEGER DEFAULT 0,
				last_check TIMESTAMP,
				last_error TEXT,
				created_at TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at TIMESTAMP NOT NULL DEFAULT NOW()
			);

			CREATE INDEX idx_proxies_address ON proxies(address);
			CREATE INDEX idx_proxies_status ON proxies(status);
			CREATE INDEX idx_proxies_protocol ON proxies(protocol);
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxies_protocol;
			DROP INDEX IF EXISTS idx_proxies_status;
			DROP INDEX IF EXISTS idx_proxies_address;
			DROP TABLE IF EXISTS proxies;
		`,
	},
	{
		Version:     4,
		Description: "Create settings table",
		Up: `
			CREATE TABLE IF NOT EXISTS settings (
				key VARCHAR(255) PRIMARY KEY,
				value JSONB NOT NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT NOW()
			);
			-- Default values are seeded by the app on startup (SettingsRepository.
			-- SeedDefaults) from a single Go-defined source, not here.
		`,
		Down: `
			DROP TABLE IF EXISTS settings;
		`,
	},
	{
		Version:     5,
		Description: "Create logs table (hypertable when TimescaleDB is available)",
		Up: `
			CREATE TABLE IF NOT EXISTS logs (
				id BIGSERIAL,
				timestamp TIMESTAMP NOT NULL DEFAULT NOW(),
				level VARCHAR(20) NOT NULL,
				message TEXT NOT NULL,
				details TEXT,
				metadata JSONB
			);

			-- Convert to hypertable where TimescaleDB exists; a plain table
			-- (with the timestamp indexes below) works fine on stock Postgres.
			DO $ts$
			BEGIN
				IF EXISTS (SELECT FROM pg_extension WHERE extname = 'timescaledb') THEN
					PERFORM create_hypertable('logs', 'timestamp', if_not_exists => TRUE);
				END IF;
			END
			$ts$;

			-- Create indexes
			CREATE INDEX idx_logs_level ON logs(level, timestamp DESC);
			CREATE INDEX idx_logs_timestamp ON logs(timestamp DESC);

			-- Retention/compression policies are TSL-licensed and unavailable on
			-- Apache-only builds (e.g. Azure Flexible Server) — skip them there.
			DO $ts$
			BEGIN
				IF current_setting('timescaledb.license', true) = 'timescale' THEN
					-- Add retention policy (keep logs for 30 days)
					PERFORM add_retention_policy('logs', INTERVAL '30 days', if_not_exists => TRUE);

					-- Add compression policy (compress data older than 7 days)
					ALTER TABLE logs SET (
						timescaledb.compress,
						timescaledb.compress_segmentby = 'level'
					);
					PERFORM add_compression_policy('logs', INTERVAL '7 days', if_not_exists => TRUE);
				END IF;
			END
			$ts$;
		`,
		Down: `
			DROP TABLE IF EXISTS logs;
		`,
	},
	{
		Version:     6,
		Description: "Create proxy_requests table (hypertable when TimescaleDB is available)",
		Up: `
			CREATE TABLE IF NOT EXISTS proxy_requests (
				id BIGSERIAL,
				timestamp TIMESTAMP NOT NULL DEFAULT NOW(),
				proxy_id INTEGER REFERENCES proxies(id) ON DELETE CASCADE,
				proxy_address VARCHAR(255) NOT NULL,
				method VARCHAR(10) NOT NULL,
				url TEXT,
				status_code INTEGER,
				response_time INTEGER,
				success BOOLEAN NOT NULL,
				error TEXT
			);

			-- Convert to hypertable where TimescaleDB exists; a plain table
			-- (with the timestamp indexes below) works fine on stock Postgres.
			DO $ts$
			BEGIN
				IF EXISTS (SELECT FROM pg_extension WHERE extname = 'timescaledb') THEN
					PERFORM create_hypertable('proxy_requests', 'timestamp', if_not_exists => TRUE);
				END IF;
			END
			$ts$;

			-- Create indexes
			CREATE INDEX idx_proxy_requests_proxy_id ON proxy_requests(proxy_id, timestamp DESC);
			CREATE INDEX idx_proxy_requests_success ON proxy_requests(success, timestamp DESC);
			CREATE INDEX idx_proxy_requests_timestamp ON proxy_requests(timestamp DESC);

			-- Retention/compression policies are TSL-licensed and unavailable on
			-- Apache-only builds (e.g. Azure Flexible Server) — skip them there.
			DO $ts$
			BEGIN
				IF current_setting('timescaledb.license', true) = 'timescale' THEN
					-- Add retention policy (keep request logs for 90 days)
					PERFORM add_retention_policy('proxy_requests', INTERVAL '90 days', if_not_exists => TRUE);

					-- Add compression policy (compress data older than 14 days)
					ALTER TABLE proxy_requests SET (
						timescaledb.compress,
						timescaledb.compress_segmentby = 'proxy_id'
					);
					PERFORM add_compression_policy('proxy_requests', INTERVAL '14 days', if_not_exists => TRUE);
				END IF;
			END
			$ts$;
		`,
		Down: `
			DROP TABLE IF EXISTS proxy_requests;
		`,
	},
	{
		Version:     7,
		Description: "Add log retention settings (now seeded by the app)",
		Up: `
			-- log_retention defaults are seeded by SettingsRepository.SeedDefaults.
			SELECT 1;
		`,
		Down: `
			SELECT 1;
		`,
	},
	{
		Version:     8,
		Description: "Add metadata source index for proxy logs filtering",
		Up: `
			CREATE INDEX IF NOT EXISTS idx_logs_metadata_source ON logs((metadata->>'source'));
		`,
		Down: `
			DROP INDEX IF EXISTS idx_logs_metadata_source;
		`,
	},
	{
		Version:     9,
		Description: "Add unique constraint to proxy address",
		Up: `
			-- First, remove any duplicate proxies (keep the oldest one)
			DELETE FROM proxies
			WHERE id NOT IN (
				SELECT MIN(id)
				FROM proxies
				GROUP BY address, protocol
			);

			-- Now add the unique constraint
			ALTER TABLE proxies ADD CONSTRAINT unique_proxy_address_protocol UNIQUE (address, protocol);
		`,
		Down: `
			ALTER TABLE proxies DROP CONSTRAINT IF EXISTS unique_proxy_address_protocol;
		`,
	},
	{
		Version:     11,
		Description: "Add GeoIP fields to proxies table",
		Up: `
			ALTER TABLE proxies
				ADD COLUMN IF NOT EXISTS country_code  VARCHAR(3),
				ADD COLUMN IF NOT EXISTS country_name  VARCHAR(100),
				ADD COLUMN IF NOT EXISTS region_name   VARCHAR(100),
				ADD COLUMN IF NOT EXISTS city_name     VARCHAR(100),
				ADD COLUMN IF NOT EXISTS latitude      DOUBLE PRECISION,
				ADD COLUMN IF NOT EXISTS longitude     DOUBLE PRECISION,
				ADD COLUMN IF NOT EXISTS isp           VARCHAR(255),
				ADD COLUMN IF NOT EXISTS geo_updated_at TIMESTAMP;

			CREATE INDEX IF NOT EXISTS idx_proxies_country_code ON proxies(country_code);
			CREATE INDEX IF NOT EXISTS idx_proxies_region_name  ON proxies(region_name);
		`,
		Down: `
			ALTER TABLE proxies
				DROP COLUMN IF EXISTS country_code,
				DROP COLUMN IF EXISTS country_name,
				DROP COLUMN IF EXISTS region_name,
				DROP COLUMN IF EXISTS city_name,
				DROP COLUMN IF EXISTS latitude,
				DROP COLUMN IF EXISTS longitude,
				DROP COLUMN IF EXISTS isp,
				DROP COLUMN IF EXISTS geo_updated_at;
			DROP INDEX IF EXISTS idx_proxies_country_code;
			DROP INDEX IF EXISTS idx_proxies_region_name;
		`,
	},
	{
		Version:     12,
		Description: "Create proxy_sources table",
		Up: `
			CREATE TABLE IF NOT EXISTS proxy_sources (
				id          SERIAL PRIMARY KEY,
				name        VARCHAR(255) NOT NULL,
				url         TEXT NOT NULL,
				protocol    VARCHAR(20) NOT NULL DEFAULT 'http',
				enabled     BOOLEAN NOT NULL DEFAULT true,
				interval_minutes INTEGER NOT NULL DEFAULT 60,
				last_fetched_at  TIMESTAMP,
				last_count       INTEGER NOT NULL DEFAULT 0,
				last_error       TEXT,
				created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at  TIMESTAMP NOT NULL DEFAULT NOW()
			);
			CREATE INDEX IF NOT EXISTS idx_proxy_sources_enabled ON proxy_sources(enabled);
		`,
		Down: `
			DROP TABLE IF EXISTS proxy_sources;
		`,
	},
	{
		Version:     13,
		Description: "Create proxy pools and pool_proxies tables",
		Up: `
			CREATE TABLE IF NOT EXISTS proxy_pools (
				id               SERIAL PRIMARY KEY,
				name             VARCHAR(255) NOT NULL,
				description      TEXT,
				country_code     VARCHAR(3),
				region_name      VARCHAR(100),
				city_name        VARCHAR(100),
				rotation_method  VARCHAR(30) NOT NULL DEFAULT 'roundrobin',
				stick_count      INTEGER NOT NULL DEFAULT 10,
				health_check_url TEXT NOT NULL DEFAULT 'https://api.ipify.org',
				health_check_cron VARCHAR(100) NOT NULL DEFAULT '*/30 * * * *',
				health_check_enabled BOOLEAN NOT NULL DEFAULT true,
				auto_sync        BOOLEAN NOT NULL DEFAULT true,
				enabled          BOOLEAN NOT NULL DEFAULT true,
				created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
			);

			CREATE TABLE IF NOT EXISTS pool_proxies (
				pool_id   INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
				proxy_id  INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
				added_at  TIMESTAMP NOT NULL DEFAULT NOW(),
				PRIMARY KEY (pool_id, proxy_id)
			);

			CREATE INDEX IF NOT EXISTS idx_pool_proxies_pool_id  ON pool_proxies(pool_id);
			CREATE INDEX IF NOT EXISTS idx_pool_proxies_proxy_id ON pool_proxies(proxy_id);
		`,
		Down: `
			DROP TABLE IF EXISTS pool_proxies;
			DROP TABLE IF EXISTS proxy_pools;
		`,
	},
	{
		Version:     16,
		Description: "Create admin_credentials table for dashboard authentication",
		Up: `
			CREATE TABLE IF NOT EXISTS admin_credentials (
				id            SERIAL PRIMARY KEY,
				username      VARCHAR(255) NOT NULL UNIQUE,
				password_hash TEXT NOT NULL,
				created_at    TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at    TIMESTAMP NOT NULL DEFAULT NOW()
			);
		`,
		Down: `DROP TABLE IF EXISTS admin_credentials;`,
	},
	{
		Version:     15,
		Description: "Add pool_geo_filters table for multi-location pool membership",
		Up: `
			CREATE TABLE IF NOT EXISTS pool_geo_filters (
				id           SERIAL PRIMARY KEY,
				pool_id      INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
				country_code VARCHAR(3),
				city_name    VARCHAR(100),
				UNIQUE (pool_id, country_code, city_name)
			);
			CREATE INDEX IF NOT EXISTS idx_pool_geo_filters_pool_id ON pool_geo_filters(pool_id);

			-- Migrate existing single country/city filters into the new table
			INSERT INTO pool_geo_filters (pool_id, country_code, city_name)
			SELECT id, country_code, city_name
			FROM proxy_pools
			WHERE country_code IS NOT NULL
			ON CONFLICT DO NOTHING;
		`,
		Down: `DROP TABLE IF EXISTS pool_geo_filters;`,
	},
	{
		Version:     14,
		Description: "Create proxy_users table for per-user pool authentication",
		Up: `
			CREATE TABLE IF NOT EXISTS proxy_users (
				id               SERIAL PRIMARY KEY,
				username         VARCHAR(255) NOT NULL UNIQUE,
				password_hash    TEXT NOT NULL,
				enabled          BOOLEAN NOT NULL DEFAULT true,
				main_pool_id     INTEGER REFERENCES proxy_pools(id) ON DELETE SET NULL,
				fallback_pool_ids INTEGER[] NOT NULL DEFAULT '{}',
				max_retries      INTEGER NOT NULL DEFAULT 5,
				created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
			);
			CREATE INDEX IF NOT EXISTS idx_proxy_users_username ON proxy_users(username);
			CREATE INDEX IF NOT EXISTS idx_proxy_users_enabled  ON proxy_users(enabled);
		`,
		Down: `
			DROP TABLE IF EXISTS proxy_users;
		`,
	},
	{
		Version:     17,
		Description: "Add tags to proxies table",
		Up: `
			ALTER TABLE proxies ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
			CREATE INDEX IF NOT EXISTS idx_proxies_tags ON proxies USING gin(tags);
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxies_tags;
			ALTER TABLE proxies DROP COLUMN IF EXISTS tags;
		`,
	},
	{
		Version:     18,
		Description: "Add sync_mode to proxy_pools, ISP filter table, pool_alerts table",
		Up: `
			-- sync_mode: 'auto' | 'manual'
			ALTER TABLE proxy_pools ADD COLUMN IF NOT EXISTS sync_mode VARCHAR(10) NOT NULL DEFAULT 'auto';

			-- ISP filters for pools
			CREATE TABLE IF NOT EXISTS pool_isp_filters (
				id       SERIAL PRIMARY KEY,
				pool_id  INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
				isp      TEXT NOT NULL,
				UNIQUE (pool_id, isp)
			);
			CREATE INDEX IF NOT EXISTS idx_pool_isp_filters_pool_id ON pool_isp_filters(pool_id);

			-- Tag filters for pools
			CREATE TABLE IF NOT EXISTS pool_tag_filters (
				id      SERIAL PRIMARY KEY,
				pool_id INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
				tag     TEXT NOT NULL,
				UNIQUE (pool_id, tag)
			);
			CREATE INDEX IF NOT EXISTS idx_pool_tag_filters_pool_id ON pool_tag_filters(pool_id);

			-- Alert rules per pool: fire webhook when active proxy count drops below threshold
			CREATE TABLE IF NOT EXISTS pool_alert_rules (
				id                  SERIAL PRIMARY KEY,
				pool_id             INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
				enabled             BOOLEAN NOT NULL DEFAULT true,
				min_active_proxies  INTEGER NOT NULL DEFAULT 5,
				webhook_url         TEXT NOT NULL,
				webhook_method      VARCHAR(10) NOT NULL DEFAULT 'POST',
				last_fired_at       TIMESTAMP,
				cooldown_minutes    INTEGER NOT NULL DEFAULT 30,
				created_at          TIMESTAMP NOT NULL DEFAULT NOW(),
				updated_at          TIMESTAMP NOT NULL DEFAULT NOW()
			);
			CREATE INDEX IF NOT EXISTS idx_pool_alert_rules_pool_id ON pool_alert_rules(pool_id);
		`,
		Down: `
			DROP TABLE IF EXISTS pool_alert_rules;
			DROP TABLE IF EXISTS pool_tag_filters;
			DROP TABLE IF EXISTS pool_isp_filters;
			ALTER TABLE proxy_pools DROP COLUMN IF EXISTS sync_mode;
		`,
	},
	{
		Version:     20,
		Description: "Add source_id to proxies for cascade delete on source removal",
		Up: `
			ALTER TABLE proxies ADD COLUMN IF NOT EXISTS source_id INTEGER REFERENCES proxy_sources(id) ON DELETE CASCADE;
			CREATE INDEX IF NOT EXISTS idx_proxies_source_id ON proxies(source_id);
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxies_source_id;
			ALTER TABLE proxies DROP COLUMN IF EXISTS source_id;
		`,
	},
	{
		Version:     19,
		Description: "Add proxy cleanup settings and rate limit to proxy_users",
		Up: `
			-- proxy_cleanup defaults are seeded by SettingsRepository.SeedDefaults.
			-- Per-user rate limiting
			ALTER TABLE proxy_users ADD COLUMN IF NOT EXISTS requests_per_minute INTEGER NOT NULL DEFAULT 0;
		`,
		Down: `
			DELETE FROM settings WHERE key = 'proxy_cleanup';
			ALTER TABLE proxy_users DROP COLUMN IF EXISTS requests_per_minute;
		`,
	},
	{
		Version:     10,
		Description: "Update default timeout and retry settings for better proxy compatibility",
		Up: `
			-- Update rotation settings: increase timeout from 30s to 90s, retries from 2 to 3
			UPDATE settings
			SET value = jsonb_set(
				jsonb_set(value, '{timeout}', '90'),
				'{retries}', '3'
			)
			WHERE key = 'rotation';

			-- Update healthcheck settings: increase timeout from 30s to 60s
			UPDATE settings
			SET value = jsonb_set(value, '{timeout}', '60')
			WHERE key = 'healthcheck';
		`,
		Down: `
			-- Revert rotation settings to original values
			UPDATE settings
			SET value = jsonb_set(
				jsonb_set(value, '{timeout}', '30'),
				'{retries}', '2'
			)
			WHERE key = 'rotation';

			-- Revert healthcheck settings to original values
			UPDATE settings
			SET value = jsonb_set(value, '{timeout}', '30')
			WHERE key = 'healthcheck';
		`,
	},
	{
		Version:     22,
		Description: "Session rotation (pool session TTL) + proxy cooldown (manual invalidation)",
		Up: `
			-- session rotation: how long an idle session keeps its proxy binding
			ALTER TABLE proxy_pools
			  ADD COLUMN IF NOT EXISTS session_ttl_minutes INTEGER NOT NULL DEFAULT 10;

			-- manual invalidation: proxy is excluded from rotation until cooldown_until
			ALTER TABLE proxies
			  ADD COLUMN IF NOT EXISTS cooldown_until  TIMESTAMP,
			  ADD COLUMN IF NOT EXISTS cooldown_reason TEXT;

			CREATE INDEX IF NOT EXISTS idx_proxies_cooldown_until
			  ON proxies(cooldown_until)
			  WHERE cooldown_until IS NOT NULL;
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxies_cooldown_until;
			ALTER TABLE proxies     DROP COLUMN IF EXISTS cooldown_reason;
			ALTER TABLE proxies     DROP COLUMN IF EXISTS cooldown_until;
			ALTER TABLE proxy_pools DROP COLUMN IF EXISTS session_ttl_minutes;
		`,
	},
	{
		Version:     21,
		Description: "Source: last_total column + per-source soft cleanup settings + proxies.last_seen_at",
		Up: `
			-- proxy_sources: total lines in last fetch + opt-in cleanup config
			ALTER TABLE proxy_sources
			  ADD COLUMN IF NOT EXISTS last_total       INTEGER NOT NULL DEFAULT 0,
			  ADD COLUMN IF NOT EXISTS cleanup_enabled  BOOLEAN NOT NULL DEFAULT false,
			  ADD COLUMN IF NOT EXISTS cleanup_days     INTEGER NOT NULL DEFAULT 7;

			-- proxies: last time a proxy was seen in its source's fetch response
			ALTER TABLE proxies
			  ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMP;

			CREATE INDEX IF NOT EXISTS idx_proxies_source_last_seen
			  ON proxies(source_id, last_seen_at)
			  WHERE source_id IS NOT NULL;
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxies_source_last_seen;
			ALTER TABLE proxies       DROP COLUMN IF EXISTS last_seen_at;
			ALTER TABLE proxy_sources DROP COLUMN IF EXISTS cleanup_days;
			ALTER TABLE proxy_sources DROP COLUMN IF EXISTS cleanup_enabled;
			ALTER TABLE proxy_sources DROP COLUMN IF EXISTS last_total;
		`,
	},
	{
		Version:     23,
		Description: "Per-domain proxy invalidation (proxy_domain_cooldowns)",
		Up: `
			-- domain-scoped invalidation: proxy is excluded from rotation for
			-- requests to domain (and its subdomains) until cooldown_until,
			-- while remaining available for all other targets
			CREATE TABLE IF NOT EXISTS proxy_domain_cooldowns (
				proxy_id        INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
				domain          VARCHAR(255) NOT NULL,
				cooldown_until  TIMESTAMP NOT NULL,
				reason          TEXT,
				created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
				PRIMARY KEY (proxy_id, domain)
			);

			CREATE INDEX IF NOT EXISTS idx_proxy_domain_cooldowns_until
			  ON proxy_domain_cooldowns(cooldown_until);
		`,
		Down: `
			DROP INDEX IF EXISTS idx_proxy_domain_cooldowns_until;
			DROP TABLE IF EXISTS proxy_domain_cooldowns;
		`,
	},
	{
		Version:     24,
		Description: "proxy_sources.format — line format of the fetched list",
		Up: `
			ALTER TABLE proxy_sources
			  ADD COLUMN IF NOT EXISTS format VARCHAR(30) NOT NULL DEFAULT 'auto';
		`,
		Down: `
			ALTER TABLE proxy_sources DROP COLUMN IF EXISTS format;
		`,
	},
	{
		Version:     25,
		Description: "free-form line formats: widen proxy_sources.format, drop 'auto', add format_history",
		Up: `
			-- format is now a lineformat template, not an enum
			ALTER TABLE proxy_sources ALTER COLUMN format TYPE TEXT;

			-- 'auto' detection is gone; the URL template covers the same lines
			UPDATE proxy_sources
			SET format = '[protocol://][user[:pass]@]host:port'
			WHERE format IS NULL OR format = '' OR format = 'auto';

			ALTER TABLE proxy_sources
			ALTER COLUMN format SET DEFAULT '[protocol://][user[:pass]@]host:port';

			-- custom formats the user has used before, re-offered in the dashboard
			CREATE TABLE IF NOT EXISTS format_history (
				id           SERIAL PRIMARY KEY,
				format       TEXT NOT NULL UNIQUE,
				use_count    INTEGER NOT NULL DEFAULT 1,
				last_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
				created_at   TIMESTAMP NOT NULL DEFAULT NOW()
			);
		`,
		Down: `
			DROP TABLE IF EXISTS format_history;
			ALTER TABLE proxy_sources ALTER COLUMN format DROP DEFAULT;
			ALTER TABLE proxy_sources ALTER COLUMN format TYPE VARCHAR(30) USING LEFT(format, 30);
			ALTER TABLE proxy_sources ALTER COLUMN format SET DEFAULT 'auto';
		`,
	},
	{
		Version:     26,
		Description: "Add pool/user/domain dimensions to proxy_requests",
		// Dimension columns only — no foreign keys: request history is an
		// event record that must survive pool/user deletion and stay portable
		// across event-store backends. domain is normalized the same way as
		// proxy_domain_cooldowns entries (NormalizeCooldownDomain), so the two
		// can be joined for per-domain analytics. Historical rows stay NULL
		// and age out with retention. Indexes come with the queries that need
		// them.
		Up: `
			ALTER TABLE proxy_requests
				ADD COLUMN IF NOT EXISTS pool_id  INTEGER,
				ADD COLUMN IF NOT EXISTS username VARCHAR(255),
				ADD COLUMN IF NOT EXISTS domain   VARCHAR(255);
		`,
		Down: `
			ALTER TABLE proxy_requests
				DROP COLUMN IF EXISTS domain,
				DROP COLUMN IF EXISTS username,
				DROP COLUMN IF EXISTS pool_id;
		`,
	},
}

// migrationLockKey is an arbitrary constant identifying Rota's migration
// advisory lock, so two instances starting together serialize instead of both
// trying to apply the same migration.
const migrationLockKey int64 = 4927562011

// Migrate runs all pending migrations. It is safe to call from multiple
// instances concurrently: a session advisory lock serializes them, and pending
// migrations are determined per-version (not by MAX(version)), so a migration
// backfilled with a lower version number than one already applied is still run.
func (db *DB) Migrate(ctx context.Context) error {
	db.logger.Info("starting database migrations")

	// Hold the advisory lock on a dedicated connection for the whole run.
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection for migration lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Use a fresh context so unlock still runs if ctx was cancelled.
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			db.logger.Warn("failed to release migration advisory lock", "error", err)
		}
	}()

	// Sort migrations by version
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("failed to load applied migrations: %w", err)
	}

	// Apply any migration whose version has not been recorded yet.
	appliedCount := 0
	for _, migration := range migrations {
		if applied[migration.Version] {
			continue
		}

		db.logger.Info("applying migration",
			"version", migration.Version,
			"description", migration.Description,
		)

		if err := db.applyMigration(ctx, migration); err != nil {
			return fmt.Errorf("failed to apply migration %d: %w", migration.Version, err)
		}

		appliedCount++
	}

	if appliedCount == 0 {
		db.logger.Info("no migrations to apply")
	} else {
		db.logger.Info("migrations completed", "applied", appliedCount)
	}

	return nil
}

// appliedVersions returns the set of migration versions already recorded in
// schema_migrations. If the table does not exist yet the set is empty.
func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	applied := make(map[int]bool)

	var exists bool
	if err := db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'schema_migrations'
		)`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return applied, nil
	}

	rows, err := db.Pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// getCurrentVersion returns the current migration version
func (db *DB) getCurrentVersion(ctx context.Context) (int, error) {
	// Check if migrations table exists
	var exists bool
	query := `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_schema = 'public'
			AND table_name = 'schema_migrations'
		);
	`
	if err := db.Pool.QueryRow(ctx, query).Scan(&exists); err != nil {
		return 0, err
	}

	if !exists {
		return 0, nil
	}

	// Get latest version
	var version int
	query = `SELECT COALESCE(MAX(version), 0) FROM schema_migrations;`
	if err := db.Pool.QueryRow(ctx, query).Scan(&version); err != nil {
		return 0, err
	}

	return version, nil
}

// applyMigration applies a single migration
func (db *DB) applyMigration(ctx context.Context, migration Migration) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		// Execute migration
		if _, err := tx.Exec(ctx, migration.Up); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}

		// Record migration
		query := `
			INSERT INTO schema_migrations (version, description, applied_at)
			VALUES ($1, $2, $3)
		`
		if _, err := tx.Exec(ctx, query, migration.Version, migration.Description, time.Now()); err != nil {
			return fmt.Errorf("failed to record migration: %w", err)
		}

		return nil
	})
}

// Rollback rolls back the last migration
func (db *DB) Rollback(ctx context.Context) error {
	db.logger.Info("rolling back last migration")

	// Get current version
	currentVersion, err := db.getCurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current version: %w", err)
	}

	if currentVersion == 0 {
		db.logger.Info("no migrations to rollback")
		return nil
	}

	// Find migration to rollback
	var migrationToRollback *Migration
	for i := range migrations {
		if migrations[i].Version == currentVersion {
			migrationToRollback = &migrations[i]
			break
		}
	}

	if migrationToRollback == nil {
		return fmt.Errorf("migration version %d not found", currentVersion)
	}

	db.logger.Info("rolling back migration",
		"version", migrationToRollback.Version,
		"description", migrationToRollback.Description,
	)

	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		// Execute rollback
		if _, err := tx.Exec(ctx, migrationToRollback.Down); err != nil {
			return fmt.Errorf("failed to execute rollback: %w", err)
		}

		// Remove migration record
		query := `DELETE FROM schema_migrations WHERE version = $1`
		if _, err := tx.Exec(ctx, query, migrationToRollback.Version); err != nil {
			return fmt.Errorf("failed to remove migration record: %w", err)
		}

		return nil
	})
}

// GetMigrationStatus returns the status of all migrations
func (db *DB) GetMigrationStatus(ctx context.Context) ([]map[string]interface{}, error) {
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load applied migrations: %w", err)
	}

	var status []map[string]interface{}
	for _, migration := range migrations {
		status = append(status, map[string]interface{}{
			"version":     migration.Version,
			"description": migration.Description,
			"applied":     applied[migration.Version],
		})
	}

	return status, nil
}
