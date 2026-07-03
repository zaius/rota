package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/jackc/pgx/v5"
)

// SettingsRepository handles settings database operations
type SettingsRepository struct {
	db *database.DB
}

// NewSettingsRepository creates a new SettingsRepository
func NewSettingsRepository(db *database.DB) *SettingsRepository {
	return &SettingsRepository{db: db}
}

// Get retrieves a setting by key
func (r *SettingsRepository) Get(ctx context.Context, key string) (map[string]any, error) {
	query := `SELECT value FROM settings WHERE key = $1`

	var valueJSON []byte
	err := r.db.Pool.QueryRow(ctx, query, key).Scan(&valueJSON)

	if err == pgx.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get setting: %w", err)
	}

	var value map[string]any
	if err := json.Unmarshal(valueJSON, &value); err != nil {
		return nil, fmt.Errorf("failed to unmarshal setting: %w", err)
	}

	return value, nil
}

// GetAll retrieves all settings
func (r *SettingsRepository) GetAll(ctx context.Context) (*models.Settings, error) {
	query := `SELECT key, value FROM settings`

	rows, err := r.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get all settings: %w", err)
	}
	defer rows.Close()

	settingsMap := make(map[string]map[string]any)
	for rows.Next() {
		var key string
		var valueJSON []byte

		if err := rows.Scan(&key, &valueJSON); err != nil {
			return nil, fmt.Errorf("failed to scan setting: %w", err)
		}

		var value map[string]any
		if err := json.Unmarshal(valueJSON, &value); err != nil {
			return nil, fmt.Errorf("failed to unmarshal setting: %w", err)
		}

		settingsMap[key] = value
	}

	// Convert map to Settings struct
	return r.mapToSettings(settingsMap)
}

// Set updates or creates a setting
func (r *SettingsRepository) Set(ctx context.Context, key string, value map[string]any) error {
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal value: %w", err)
	}

	query := `
		INSERT INTO settings (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`

	_, err = r.db.Pool.Exec(ctx, query, key, valueJSON)
	if err != nil {
		return fmt.Errorf("failed to set setting: %w", err)
	}

	return nil
}

// UpdateAll updates multiple settings atomically, so a mid-write failure can't
// leave the settings half-applied.
func (r *SettingsRepository) UpdateAll(ctx context.Context, settings *models.Settings) error {
	settingsMap := r.settingsToMap(settings)

	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		for key, value := range settingsMap {
			valueJSON, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("failed to marshal value for %q: %w", key, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO settings (key, value, updated_at)
				 VALUES ($1, $2, NOW())
				 ON CONFLICT (key) DO UPDATE
				 SET value = EXCLUDED.value, updated_at = NOW()`,
				key, valueJSON); err != nil {
				return fmt.Errorf("failed to set setting %q: %w", key, err)
			}
		}
		return nil
	})
}

// defaultSettings is the single source of truth for default settings values.
// Both SeedDefaults (fresh install) and Reset use it, so the two can't drift —
// previously the migration seed and Reset defined defaults separately and
// disagreed (the migration omitted rotation's protocol/response-time/success
// filters; Reset omitted proxy_cleanup entirely).
func defaultSettings() map[string]map[string]any {
	return map[string]map[string]any{
		"authentication": {
			"enabled":  false,
			"username": "",
			"password": "",
		},
		"rotation": {
			"method": "random",
			"time_based": map[string]any{
				"interval": 120,
			},
			"remove_unhealthy":     true,
			"fallback":             true,
			"fallback_max_retries": 10,
			"follow_redirect":      false,
			"timeout":              90,
			"retries":              3,
			"allowed_protocols":    []string{"http", "https", "socks5"}, // empty/all allowed by default
			"max_response_time":    0,                                   // 0 means no limit
			"min_success_rate":     0.0,                                 // 0 means no minimum
		},
		"rate_limit": {
			"enabled":      false,
			"interval":     1,
			"max_requests": 100,
		},
		"healthcheck": {
			"timeout": 60,
			"workers": 20,
			"url":     "https://api.ipify.org",
			"status":  200,
			"headers": []string{"User-Agent: Rota-HealthCheck/1.0"},
		},
		"log_retention": {
			"enabled":                true,
			"retention_days":         30,
			"compression_after_days": 7,
			"cleanup_interval_hours": 24,
		},
		"proxy_cleanup": {
			"enabled":                false,
			"max_failed_days":        7,
			"min_success_rate":       0,
			"cleanup_interval_hours": 24,
		},
	}
}

// SeedDefaults inserts any default settings keys that don't exist yet. It is
// called once on startup (after migrations) and never overwrites existing
// values, so it is safe to run every boot.
func (r *SettingsRepository) SeedDefaults(ctx context.Context) error {
	return r.writeSettings(ctx, defaultSettings(), false)
}

// Reset resets all settings to their defaults, overwriting existing values.
func (r *SettingsRepository) Reset(ctx context.Context) error {
	return r.writeSettings(ctx, defaultSettings(), true)
}

// writeSettings upserts each key atomically. When overwrite is false, existing
// keys are left untouched (seed); when true, they are replaced (reset).
func (r *SettingsRepository) writeSettings(ctx context.Context, values map[string]map[string]any, overwrite bool) error {
	onConflict := "DO NOTHING"
	if overwrite {
		onConflict = "DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()"
	}
	query := `INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW()) ON CONFLICT (key) ` + onConflict

	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		for key, value := range values {
			valueJSON, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("failed to marshal default for %q: %w", key, err)
			}
			if _, err := tx.Exec(ctx, query, key, valueJSON); err != nil {
				return fmt.Errorf("failed to write setting %q: %w", key, err)
			}
		}
		return nil
	})
}

// Helper functions to convert between Settings struct and map

func (r *SettingsRepository) mapToSettings(m map[string]map[string]any) (*models.Settings, error) {
	settings := &models.Settings{}

	// Convert map to JSON and then to struct
	settingsJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal settings map: %w", err)
	}

	if err := json.Unmarshal(settingsJSON, settings); err != nil {
		return nil, fmt.Errorf("failed to unmarshal settings: %w", err)
	}

	return settings, nil
}

func (r *SettingsRepository) settingsToMap(s *models.Settings) map[string]map[string]any {
	m := make(map[string]map[string]any)

	// Convert struct to JSON and then to map
	settingsJSON, _ := json.Marshal(s)
	var settingsMap map[string]any
	json.Unmarshal(settingsJSON, &settingsMap)

	for key, value := range settingsMap {
		if valueMap, ok := value.(map[string]any); ok {
			m[key] = valueMap
		}
	}

	return m
}
