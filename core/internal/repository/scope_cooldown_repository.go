package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/jackc/pgx/v5"
)

func (r *ProxyRepository) SetScopeCooldown(ctx context.Context, id int, scope string, until time.Time, reason string) (*models.Proxy, error) {
	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, `
		WITH target AS (
			SELECT id, address, protocol, status FROM proxies WHERE id = $1
		), upsert AS (
			INSERT INTO proxy_scope_cooldowns (proxy_id, scope, cooldown_until, reason)
			SELECT id, $2, $3, $4 FROM target
			ON CONFLICT (proxy_id, scope)
			DO UPDATE SET cooldown_until = EXCLUDED.cooldown_until, reason = EXCLUDED.reason
		)
		SELECT id, address, protocol, status FROM target
	`, id, scope, until, reason).Scan(&p.ID, &p.Address, &p.Protocol, &p.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to set proxy scope cooldown: %w", err)
	}
	return &p, nil
}

// ClearScopeCooldowns clears one exact scope, or all scopes when scope is empty.
func (r *ProxyRepository) ClearScopeCooldowns(ctx context.Context, id int, scope string) (int, error) {
	result, err := r.db.Pool.Exec(ctx,
		`DELETE FROM proxy_scope_cooldowns WHERE proxy_id = $1 AND ($2 = '' OR scope = $2)`, id, scope)
	if err != nil {
		return 0, fmt.Errorf("failed to clear proxy scope cooldowns: %w", err)
	}
	return int(result.RowsAffected()), nil
}

func (r *ProxyRepository) ListActiveScopeCooldowns(ctx context.Context) ([]models.ProxyScopeCooldown, error) {
	_, _ = r.db.Pool.Exec(ctx, `DELETE FROM proxy_scope_cooldowns WHERE cooldown_until < NOW()`)
	rows, err := r.db.Pool.Query(ctx, `
		SELECT proxy_id, scope, cooldown_until, reason FROM proxy_scope_cooldowns WHERE cooldown_until > NOW()
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list proxy scope cooldowns: %w", err)
	}
	defer rows.Close()
	out := []models.ProxyScopeCooldown{}
	for rows.Next() {
		var c models.ProxyScopeCooldown
		if err := rows.Scan(&c.ProxyID, &c.Scope, &c.CooldownUntil, &c.Reason); err != nil {
			return nil, fmt.Errorf("failed to scan proxy scope cooldown: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
