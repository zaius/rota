package repository

import (
	"context"
	"fmt"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
)

// FormatHistoryRepository stores custom line formats the user has used, so the
// dashboard can offer them again alongside the built-in presets.
type FormatHistoryRepository struct {
	db *database.DB
}

// NewFormatHistoryRepository creates a new FormatHistoryRepository.
func NewFormatHistoryRepository(db *database.DB) *FormatHistoryRepository {
	return &FormatHistoryRepository{db: db}
}

// List returns the most recently used formats, newest first.
func (r *FormatHistoryRepository) List(ctx context.Context) ([]models.FormatHistoryEntry, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, format, use_count, last_used_at
		FROM format_history
		ORDER BY last_used_at DESC
		LIMIT 20
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list format history: %w", err)
	}
	defer rows.Close()

	entries := []models.FormatHistoryEntry{}
	for rows.Next() {
		var e models.FormatHistoryEntry
		if err := rows.Scan(&e.ID, &e.Format, &e.UseCount, &e.LastUsedAt); err != nil {
			return nil, fmt.Errorf("failed to scan format history entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Record upserts a format, bumping its use count and recency.
func (r *FormatHistoryRepository) Record(ctx context.Context, format string) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO format_history (format)
		VALUES ($1)
		ON CONFLICT (format) DO UPDATE
		SET use_count = format_history.use_count + 1,
		    last_used_at = NOW()
	`, format)
	return err
}

// Delete removes a format from history.
func (r *FormatHistoryRepository) Delete(ctx context.Context, id int) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM format_history WHERE id = $1`, id)
	return err
}
