package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/jackc/pgx/v5"
)

// BackoffScopeCooldown advances the failure streak independently of session
// bindings. The fourth invalidation excludes the proxy until reactivated.
func (r *ProxyRepository) BackoffScopeCooldown(ctx context.Context, id int, scope string, base time.Duration, reason string) (*models.Proxy, models.ProxyScopeCooldown, error) {
	p, state, err := r.backoffCooldown(ctx, id, scope, base, reason, false)
	return p, models.ProxyScopeCooldown{
		ProxyID: id, Scope: scope, CooldownUntil: state.until, Reason: reason,
		FailureCount: state.failures, Invalid: state.invalid,
	}, err
}

func (r *ProxyRepository) BackoffDomainCooldown(ctx context.Context, id int, domain string, base time.Duration, reason string) (*models.Proxy, models.ProxyDomainCooldown, error) {
	p, state, err := r.backoffCooldown(ctx, id, domain, base, reason, true)
	return p, models.ProxyDomainCooldown{
		ProxyID: id, Domain: domain, CooldownUntil: state.until, Reason: &reason,
		FailureCount: state.failures, Invalid: state.invalid,
	}, err
}

type cooldownBackoff struct {
	until    time.Time
	failures int
	invalid  bool
}

// Only these fixed identifiers are interpolated into SQL; the scope/domain is
// always a bound parameter. The upsert serializes concurrent invalidations so
// a failure count cannot be lost. Duration multiplication happens in Postgres
// to avoid overflowing time.Duration when the caller supplies a large base.
func cooldownTable(domain bool) (string, string) {
	if domain {
		return "proxy_domain_cooldowns", "domain"
	}
	return "proxy_scope_cooldowns", "scope"
}

func (r *ProxyRepository) backoffCooldown(ctx context.Context, id int, scope string, base time.Duration, reason string, domain bool) (*models.Proxy, cooldownBackoff, error) {
	var p models.Proxy
	var state cooldownBackoff
	if base <= 0 {
		return nil, state, fmt.Errorf("backoff duration must be positive")
	}
	table, column := cooldownTable(domain)
	query := fmt.Sprintf(`
		WITH target AS (
			SELECT id, address, protocol, status FROM proxies WHERE id = $1
		), upsert AS (
			INSERT INTO %[1]s AS c (proxy_id, %[2]s, cooldown_until, reason, failure_count, invalid)
			SELECT id, $2, NOW() + $3 * INTERVAL '1 second', $4, 1, FALSE FROM target
			ON CONFLICT (proxy_id, %[2]s) DO UPDATE SET
				failure_count = LEAST((%[3]s) + 1, 4),
				invalid = c.invalid OR (%[3]s) >= 3,
				cooldown_until = CASE WHEN c.invalid OR (%[3]s) >= 3 THEN NOW()
					ELSE NOW() + ($3 * INTERVAL '1 second') * power(2, (%[3]s)) END,
				reason = EXCLUDED.reason, recovery_after = NULL
			RETURNING cooldown_until, failure_count, invalid
		)
		SELECT target.id, address, protocol, status, cooldown_until, failure_count, invalid
		FROM target CROSS JOIN upsert
	`, table, column, "CASE WHEN c.recovery_after <= NOW() AND NOT c.invalid THEN 0 ELSE c.failure_count END")
	err := r.db.Pool.QueryRow(ctx, query, id, scope, base.Seconds(), reason).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Status, &state.until, &state.failures, &state.invalid,
	)
	if err == pgx.ErrNoRows {
		return nil, state, nil
	}
	if err != nil {
		return nil, state, fmt.Errorf("failed to back off proxy: %w", err)
	}
	return &p, state, nil
}

// StartCooldownRecovery records the first use after a cooldown. Requests that
// started before a newer invalidation cannot start its recovery period.
func (r *ProxyRepository) StartCooldownRecovery(ctx context.Context, id int, scope string, domain bool, requestStart, recoveryAfter time.Time) error {
	table, column := cooldownTable(domain)
	_, err := r.db.Pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET recovery_after = $4
		WHERE proxy_id = $1 AND %s = $2 AND cooldown_until <= $3
		  AND failure_count > 0 AND NOT invalid AND recovery_after IS NULL
	`, table, column), id, scope, requestStart, recoveryAfter)
	if err != nil {
		return fmt.Errorf("failed to start proxy backoff recovery: %w", err)
	}
	return nil
}
