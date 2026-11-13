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
		Description: "Reserved (TimescaleDB support removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
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
		// Keep retired versions reserved for databases that already applied them.
		Version:     5,
		Description: "Reserved (dashboard log feature removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     6,
		Description: "Reserved (Postgres event store removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     7,
		Description: "Reserved (dashboard log feature removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     8,
		Description: "Reserved (dashboard log feature removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
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
		Description: "Reserved (proxy pools removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     16,
		Description: "Reserved (dashboard logins removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     15,
		Description: "Reserved (proxy pools removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     14,
		Description: "Reserved (proxy users removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
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
		Description: "Reserved (proxy pools removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
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
		Description: "Reserved (proxy users removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
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
		Description: "Proxy cooldown (manual invalidation)",
		Up: `
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
			ALTER TABLE proxies DROP COLUMN IF EXISTS cooldown_reason;
			ALTER TABLE proxies DROP COLUMN IF EXISTS cooldown_until;
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
		Description: "Reserved (Postgres event store removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     27,
		Description: "Drop the legacy shared proxy credential (settings key 'authentication')",
		// The stored shared credential is removed rather than left as an unread
		// secret. Down restores the key disabled — the credential itself is not
		// recoverable.
		Up: `
			DELETE FROM settings WHERE key = 'authentication';
		`,
		Down: `
			INSERT INTO settings (key, value, updated_at)
			VALUES ('authentication', '{"enabled": false, "username": "", "password": ""}', NOW())
			ON CONFLICT (key) DO NOTHING;
		`,
	},
	{
		Version:     28,
		Description: "Reserved (proxy users removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     29,
		Description: "Reserved (proxy users removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     30,
		Description: "Persist reservation scope cooldowns",
		Up: `
			CREATE TABLE IF NOT EXISTS proxy_scope_cooldowns (
				proxy_id INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
				scope TEXT NOT NULL,
				cooldown_until TIMESTAMPTZ NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (proxy_id, scope)
			);
			CREATE INDEX IF NOT EXISTS idx_proxy_scope_cooldowns_until ON proxy_scope_cooldowns(cooldown_until);
		`,
		Down: `
			DROP TABLE IF EXISTS proxy_scope_cooldowns;
		`,
	},
	{
		Version:     31,
		Description: "Persist session invalidation backoff and scoped exclusions",
		Up: `
			-- Preserve the existing database interpretation of domain deadlines
			-- while making new deadlines unambiguous across server time zones.
			ALTER TABLE proxy_domain_cooldowns ALTER COLUMN cooldown_until TYPE TIMESTAMPTZ
			  USING cooldown_until AT TIME ZONE current_setting('TimeZone');
			ALTER TABLE proxy_scope_cooldowns
			  ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 4),
			  ADD COLUMN invalid BOOLEAN NOT NULL DEFAULT FALSE,
			  ADD COLUMN recovery_after TIMESTAMPTZ;
			ALTER TABLE proxy_domain_cooldowns
			  ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 4),
			  ADD COLUMN invalid BOOLEAN NOT NULL DEFAULT FALSE,
			  ADD COLUMN recovery_after TIMESTAMPTZ;
		`,
		Down: `
			ALTER TABLE proxy_scope_cooldowns DROP COLUMN failure_count, DROP COLUMN invalid, DROP COLUMN recovery_after;
			ALTER TABLE proxy_domain_cooldowns DROP COLUMN failure_count, DROP COLUMN invalid, DROP COLUMN recovery_after;
			ALTER TABLE proxy_domain_cooldowns ALTER COLUMN cooldown_until TYPE TIMESTAMP
			  USING cooldown_until AT TIME ZONE current_setting('TimeZone');
		`,
	},
	{
		Version:     32,
		Description: "Reserved (Postgres event store removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     33,
		Description: "Remove request-volume rate limit settings",
		Up:          `DELETE FROM settings WHERE key = 'rate_limit';`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     34,
		Description: "Reserved (dashboard logins removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     35,
		Description: "Reserved (proxy users removed)",
		Up:          `SELECT 1;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     36,
		Description: "Drop the Postgres event tables",
		// Request and tunnel history lives in Parquet files. On TimescaleDB the
		// tables are hypertables, which take one DROP each and lose their
		// chunks and policy jobs with it; the extension stays installed. Down
		// cannot bring the history back.
		Up: `
			DROP TABLE IF EXISTS proxy_requests;
			DROP TABLE IF EXISTS proxy_tunnels;
		`,
		Down: `SELECT 1;`,
	},
	{
		Version:     37,
		Description: "Drop pool alert rules",
		Up:          `DROP TABLE IF EXISTS pool_alert_rules;`,
		Down:        `SELECT 1;`,
	},
	{
		Version:     38,
		Description: "Drop proxy users, pools and dashboard logins",
		// One shared password replaces accounts, and the proxy username's
		// options replace pools. Down cannot bring the rows back.
		Up: `
			DROP TABLE IF EXISTS proxy_users;
			DROP TABLE IF EXISTS pool_proxies;
			DROP TABLE IF EXISTS pool_geo_filters;
			DROP TABLE IF EXISTS pool_isp_filters;
			DROP TABLE IF EXISTS pool_tag_filters;
			DROP TABLE IF EXISTS proxy_pools;
			DROP TABLE IF EXISTS admin_credentials;
			DROP TABLE IF EXISTS system_secrets;
		`,
		Down: `SELECT 1;`,
	},
}

// migrationLockKey serializes each migration transaction across instances.
const migrationLockKey int64 = 4927562011

// Migrate runs all pending migrations. It is safe to call from multiple
// instances concurrently, and a migration backfilled below the current maximum
// version is still applied.
func (db *DB) Migrate(ctx context.Context) error {
	db.logger.Info("starting database migrations")

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	appliedCount := 0
	for _, migration := range migrations {
		applied, err := db.applyPendingMigration(ctx, migration)
		if err != nil {
			return fmt.Errorf("failed to apply migration %d: %w", migration.Version, err)
		}
		if applied {
			appliedCount++
		}
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

func (db *DB) applyPendingMigration(ctx context.Context, migration Migration) (bool, error) {
	applied := false
	err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
			return fmt.Errorf("failed to acquire migration lock: %w", err)
		}

		var tableExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'schema_migrations'
			)`).Scan(&tableExists); err != nil {
			return fmt.Errorf("failed to inspect migration table: %w", err)
		}
		if tableExists {
			var alreadyApplied bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT FROM schema_migrations WHERE version = $1)`,
				migration.Version,
			).Scan(&alreadyApplied); err != nil {
				return fmt.Errorf("failed to inspect migration version: %w", err)
			}
			if alreadyApplied {
				return nil
			}
		}

		db.logger.Info("applying migration",
			"version", migration.Version,
			"description", migration.Description,
		)
		if _, err := tx.Exec(ctx, migration.Up); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}

		query := `
			INSERT INTO schema_migrations (version, description, applied_at)
			VALUES ($1, $2, $3)
		`
		if _, err := tx.Exec(ctx, query, migration.Version, migration.Description, time.Now()); err != nil {
			return fmt.Errorf("failed to record migration: %w", err)
		}

		applied = true
		return nil
	})
	return applied, err
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
