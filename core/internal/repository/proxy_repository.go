package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ProxyRepository handles proxy database operations
type ProxyRepository struct {
	db *database.DB
}

// NewProxyRepository creates a new ProxyRepository
func NewProxyRepository(db *database.DB) *ProxyRepository {
	return &ProxyRepository{db: db}
}

// GetDB returns the database instance
func (r *ProxyRepository) GetDB() *database.DB {
	return r.db
}

// buildProxyWhere builds a WHERE clause (and its arguments) for the list-style
// filters shared by List and the bulk-by-filter operations. argPos is the next
// positional placeholder to use, so callers can append further args afterwards.
func buildProxyWhere(search, status, protocol string, argPos int) (string, []interface{}) {
	whereClauses := []string{}
	args := []interface{}{}

	if search != "" {
		// Use both ILIKE for simple search and to_tsvector for full-text search
		whereClauses = append(whereClauses, fmt.Sprintf("(address ILIKE $%d OR to_tsvector('simple', address) @@ plainto_tsquery('simple', $%d))", argPos, argPos))
		args = append(args, "%"+search+"%")
		argPos++
	}

	if status != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argPos))
		args = append(args, status)
		argPos++
	}

	if protocol != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("protocol = $%d", argPos))
		args = append(args, protocol)
		argPos++
	}

	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}
	return whereClause, args
}

// List retrieves proxies with pagination and filters
func (r *ProxyRepository) List(ctx context.Context, page, limit int, search, status, protocol, sortField, sortOrder string) ([]models.ProxyWithStats, int, error) {
	whereClause, args := buildProxyWhere(search, status, protocol, 1)
	argPos := len(args) + 1

	// Validate and set sort field
	validSortFields := map[string]bool{
		"address":           true,
		"status":            true,
		"requests":          true,
		"avg_response_time": true,
		"created_at":        true,
	}

	if !validSortFields[sortField] {
		sortField = "created_at"
	}

	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "desc"
	}

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM proxies %s", whereClause)
	var total int
	if err := r.db.Pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count proxies: %w", err)
	}

	// Get proxies
	offset := (page - 1) * limit
	query := fmt.Sprintf(`
		SELECT
			id, address, protocol, username, status,
			requests, successful_requests, failed_requests,
			avg_response_time, last_check,
			cooldown_until, cooldown_reason,
			country_code, country_name, region_name, city_name, isp,
			COALESCE(tags, '{}') AS tags,
			created_at, updated_at
		FROM proxies
		%s
		ORDER BY %s %s
		LIMIT $%d OFFSET $%d
	`, whereClause, sortField, sortOrder, argPos, argPos+1)

	args = append(args, limit, offset)

	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list proxies: %w", err)
	}
	defer rows.Close()

	proxies := []models.ProxyWithStats{}
	for rows.Next() {
		var p models.Proxy
		err := rows.Scan(
			&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Status,
			&p.Requests, &p.SuccessfulRequests, &p.FailedRequests,
			&p.AvgResponseTime, &p.LastCheck,
			&p.CooldownUntil, &p.CooldownReason,
			&p.CountryCode, &p.CountryName, &p.RegionName, &p.CityName, &p.ISP,
			&p.Tags,
			&p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to scan proxy: %w", err)
		}

		// Calculate success rate
		successRate := 0.0
		if p.Requests > 0 {
			successRate = (float64(p.SuccessfulRequests) / float64(p.Requests)) * 100
		}

		tags := p.Tags
		if tags == nil {
			tags = []string{}
		}

		proxies = append(proxies, models.ProxyWithStats{
			ID:              p.ID,
			Address:         p.Address,
			Protocol:        p.Protocol,
			Username:        p.Username,
			Status:          p.Status,
			Requests:        p.Requests,
			SuccessRate:     successRate,
			AvgResponseTime: p.AvgResponseTime,
			LastCheck:       p.LastCheck,
			CooldownUntil:   p.CooldownUntil,
			CooldownReason:  p.CooldownReason,
			CountryCode:     p.CountryCode,
			CountryName:     p.CountryName,
			RegionName:      p.RegionName,
			CityName:        p.CityName,
			ISP:             p.ISP,
			Tags:            tags,
			CreatedAt:       p.CreatedAt,
			UpdatedAt:       p.UpdatedAt,
		})
	}

	return proxies, total, nil
}

// GetByID retrieves a proxy by ID
func (r *ProxyRepository) GetByID(ctx context.Context, id int) (*models.Proxy, error) {
	query := `
		SELECT
			id, address, protocol, username, password, status,
			requests, successful_requests, failed_requests,
			avg_response_time, last_check, last_error,
			country_code, country_name, region_name, city_name, isp,
			COALESCE(tags, '{}') AS tags,
			created_at, updated_at
		FROM proxies
		WHERE id = $1
	`

	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, id).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Password, &p.Status,
		&p.Requests, &p.SuccessfulRequests, &p.FailedRequests,
		&p.AvgResponseTime, &p.LastCheck, &p.LastError,
		&p.CountryCode, &p.CountryName, &p.RegionName, &p.CityName, &p.ISP,
		&p.Tags,
		&p.CreatedAt, &p.UpdatedAt,
	)

	if err == pgx.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get proxy: %w", err)
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	return &p, nil
}

// Create creates a new proxy
func (r *ProxyRepository) Create(ctx context.Context, req models.CreateProxyRequest) (*models.Proxy, error) {
	tags := req.Tags
	if tags == nil {
		tags = []string{}
	}
	query := `
		INSERT INTO proxies (address, protocol, username, password, tags, source_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, address, protocol, username, status, tags, created_at, updated_at
	`

	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, req.Address, req.Protocol, req.Username, req.Password, tags, req.SourceID).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Status, &p.Tags, &p.CreatedAt, &p.UpdatedAt,
	)

	if err != nil {
		// Check if it's a unique constraint violation
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("proxy with address %s and protocol %s already exists", req.Address, req.Protocol)
		}
		return nil, fmt.Errorf("failed to create proxy: %w", err)
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	return &p, nil
}

// Upsert creates or updates a proxy, returning the result status. It is a single
// statement keyed on the (address, protocol) unique constraint — the previous
// implementation issued a SELECT then a separate INSERT/UPDATE (two round-trips
// per proxy, ~20k queries for a 10k-line import). The `xmax = 0` check
// distinguishes a fresh insert from a conflict update.
func (r *ProxyRepository) Upsert(ctx context.Context, req models.CreateProxyRequest) (id int, status string, err error) {
	tags := req.Tags
	if tags == nil {
		tags = []string{}
	}

	var inserted bool
	err = r.db.Pool.QueryRow(ctx,
		`INSERT INTO proxies (address, protocol, username, password, tags, source_id)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (address, protocol) DO UPDATE SET
			username   = COALESCE(EXCLUDED.username, proxies.username),
			password   = COALESCE(EXCLUDED.password, proxies.password),
			tags       = CASE WHEN array_length(EXCLUDED.tags, 1) > 0 THEN EXCLUDED.tags ELSE proxies.tags END,
			source_id  = COALESCE(EXCLUDED.source_id, proxies.source_id),
			updated_at = NOW()
		 RETURNING id, (xmax = 0) AS inserted`,
		req.Address, req.Protocol, req.Username, req.Password, tags, req.SourceID,
	).Scan(&id, &inserted)
	if err != nil {
		return 0, "failed", err
	}
	if inserted {
		return id, "created", nil
	}
	return id, "updated", nil
}

// DeleteAll removes all proxies from the database. Returns count deleted.
func (r *ProxyRepository) DeleteAll(ctx context.Context) (int, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM proxies`)
	if err != nil {
		return 0, fmt.Errorf("failed to delete all proxies: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeleteDeadProxies removes proxies that have been in failed status for more than maxDays days
// and optionally those with success rate below minSuccessRate (0 = disabled)
func (r *ProxyRepository) DeleteDeadProxies(ctx context.Context, maxFailedDays int, minSuccessRate float64) (int, error) {
	var total int64
	// Delete by failed duration
	if maxFailedDays > 0 {
		tag, err := r.db.Pool.Exec(ctx, `
			DELETE FROM proxies
			WHERE status = 'failed'
			  AND last_check < NOW() - ($1 || ' days')::INTERVAL`,
			maxFailedDays)
		if err != nil {
			return 0, fmt.Errorf("failed to delete dead proxies by age: %w", err)
		}
		total += tag.RowsAffected()
	}
	// Delete by success rate (only proxies with enough requests to be meaningful: >= 10)
	if minSuccessRate > 0 {
		tag, err := r.db.Pool.Exec(ctx, `
			DELETE FROM proxies
			WHERE requests >= 10
			  AND (successful_requests::float / requests::float * 100) < $1`,
			minSuccessRate)
		if err != nil {
			return 0, fmt.Errorf("failed to delete dead proxies by success rate: %w", err)
		}
		total += tag.RowsAffected()
	}
	return int(total), nil
}

// Update updates a proxy
func (r *ProxyRepository) Update(ctx context.Context, id int, req models.UpdateProxyRequest) (*models.Proxy, error) {
	tags := req.Tags
	if tags == nil {
		tags = []string{}
	}
	query := `
		UPDATE proxies
		SET address    = COALESCE(NULLIF($1, ''), address),
		    protocol   = COALESCE(NULLIF($2, ''), protocol),
		    username   = $3,
		    password   = $4,
		    tags       = $5,
		    updated_at = NOW()
		WHERE id = $6
		RETURNING id, address, protocol, status, COALESCE(tags,'{}'), updated_at
	`

	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, req.Address, req.Protocol, req.Username, req.Password, tags, id).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Status, &p.Tags, &p.UpdatedAt,
	)

	if err == pgx.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to update proxy: %w", err)
	}

	return &p, nil
}

// Delete deletes a proxy by ID
func (r *ProxyRepository) Delete(ctx context.Context, id int) error {
	query := `DELETE FROM proxies WHERE id = $1`
	_, err := r.db.Pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to delete proxy: %w", err)
	}
	return nil
}

// SetCooldown marks a proxy as temporarily invalidated (e.g. rate-limited) by
// setting cooldown_until to now + the given duration. While in cooldown the
// proxy is excluded from rotation. A non-positive duration applies a long
// default (effectively "until manually reactivated").
func (r *ProxyRepository) SetCooldown(ctx context.Context, id int, d time.Duration, reason string) (*models.Proxy, error) {
	if d <= 0 {
		d = 24 * time.Hour
	}
	until := time.Now().Add(d)
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	query := `
		UPDATE proxies
		SET cooldown_until = $2, cooldown_reason = $3, updated_at = NOW()
		WHERE id = $1
		RETURNING id, address, protocol, status, cooldown_until, cooldown_reason
	`
	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, id, until, reasonPtr).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Status, &p.CooldownUntil, &p.CooldownReason,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to set proxy cooldown: %w", err)
	}
	return &p, nil
}

// ClearCooldown removes a proxy's cooldown, returning it to rotation immediately.
func (r *ProxyRepository) ClearCooldown(ctx context.Context, id int) (*models.Proxy, error) {
	query := `
		UPDATE proxies
		SET cooldown_until = NULL, cooldown_reason = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING id, address, protocol, status, cooldown_until, cooldown_reason
	`
	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, id).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Status, &p.CooldownUntil, &p.CooldownReason,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to clear proxy cooldown: %w", err)
	}
	return &p, nil
}

// SetDomainCooldown records a domain-scoped invalidation for a proxy: it is
// excluded from rotation for requests to domain (and its subdomains) until
// the given time, but stays available for other targets. Returns nil if the
// proxy does not exist. domain must already be normalized.
func (r *ProxyRepository) SetDomainCooldown(ctx context.Context, id int, domain string, until time.Time, reason string) (*models.Proxy, error) {
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	query := `
		WITH target AS (
			SELECT id, address, protocol, status FROM proxies WHERE id = $1
		), upsert AS (
			INSERT INTO proxy_domain_cooldowns (proxy_id, domain, cooldown_until, reason)
			SELECT id, $2, $3, $4 FROM target
			ON CONFLICT (proxy_id, domain)
			DO UPDATE SET cooldown_until = EXCLUDED.cooldown_until, reason = EXCLUDED.reason
		)
		SELECT id, address, protocol, status FROM target
	`
	var p models.Proxy
	err := r.db.Pool.QueryRow(ctx, query, id, domain, until, reasonPtr).Scan(
		&p.ID, &p.Address, &p.Protocol, &p.Status,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to set proxy domain cooldown: %w", err)
	}
	return &p, nil
}

// ClearDomainCooldown removes a single (proxy, domain) cooldown.
// Returns true if one existed.
func (r *ProxyRepository) ClearDomainCooldown(ctx context.Context, id int, domain string) (bool, error) {
	result, err := r.db.Pool.Exec(ctx,
		`DELETE FROM proxy_domain_cooldowns WHERE proxy_id = $1 AND domain = $2`, id, domain)
	if err != nil {
		return false, fmt.Errorf("failed to clear proxy domain cooldown: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

// ClearAllDomainCooldowns removes every domain cooldown for a proxy.
// Returns the number of cooldowns removed.
func (r *ProxyRepository) ClearAllDomainCooldowns(ctx context.Context, id int) (int, error) {
	result, err := r.db.Pool.Exec(ctx,
		`DELETE FROM proxy_domain_cooldowns WHERE proxy_id = $1`, id)
	if err != nil {
		return 0, fmt.Errorf("failed to clear proxy domain cooldowns: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// ListActiveDomainCooldowns returns all unexpired domain cooldowns, pruning
// expired rows along the way. Used to warm the in-memory manager at startup.
func (r *ProxyRepository) ListActiveDomainCooldowns(ctx context.Context) ([]models.ProxyDomainCooldown, error) {
	// Opportunistic cleanup; expired rows are inert either way.
	_, _ = r.db.Pool.Exec(ctx, `DELETE FROM proxy_domain_cooldowns WHERE cooldown_until < NOW()`)

	rows, err := r.db.Pool.Query(ctx, `
		SELECT proxy_id, domain, cooldown_until, reason
		FROM proxy_domain_cooldowns
		WHERE cooldown_until > NOW()
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list proxy domain cooldowns: %w", err)
	}
	defer rows.Close()

	var out []models.ProxyDomainCooldown
	for rows.Next() {
		var c models.ProxyDomainCooldown
		if err := rows.Scan(&c.ProxyID, &c.Domain, &c.CooldownUntil, &c.Reason); err != nil {
			return nil, fmt.Errorf("failed to scan proxy domain cooldown: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate proxy domain cooldowns: %w", err)
	}
	return out, nil
}

// BulkDelete deletes multiple proxies
func (r *ProxyRepository) BulkDelete(ctx context.Context, ids []int) (int, error) {
	query := `DELETE FROM proxies WHERE id = ANY($1)`
	result, err := r.db.Pool.Exec(ctx, query, ids)
	if err != nil {
		return 0, fmt.Errorf("failed to bulk delete proxies: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// BulkDeleteByFilter deletes every proxy matching the given list-style filter
// and returns the number of rows removed. An empty filter deletes all proxies.
func (r *ProxyRepository) BulkDeleteByFilter(ctx context.Context, filter models.ProxyFilter) (int, error) {
	whereClause, args := buildProxyWhere(filter.Search, filter.Status, filter.Protocol, 1)
	query := fmt.Sprintf("DELETE FROM proxies %s", whereClause)
	result, err := r.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to bulk delete proxies by filter: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// scanProxies scans rows selecting the full proxy columns (including password)
// needed to build a transport for testing.
func scanProxies(rows pgx.Rows) ([]*models.Proxy, error) {
	defer rows.Close()
	proxies := []*models.Proxy{}
	for rows.Next() {
		var p models.Proxy
		err := rows.Scan(
			&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Password, &p.Status,
			&p.Requests, &p.SuccessfulRequests, &p.FailedRequests,
			&p.AvgResponseTime, &p.LastCheck, &p.LastError,
			&p.CountryCode, &p.CountryName, &p.RegionName, &p.CityName, &p.ISP,
			&p.Tags,
			&p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan proxy: %w", err)
		}
		if p.Tags == nil {
			p.Tags = []string{}
		}
		proxies = append(proxies, &p)
	}
	return proxies, nil
}

const proxyFullColumns = `
	id, address, protocol, username, password, status,
	requests, successful_requests, failed_requests,
	avg_response_time, last_check, last_error,
	country_code, country_name, region_name, city_name, isp,
	COALESCE(tags, '{}') AS tags,
	created_at, updated_at`

// GetByIDs returns the full proxy records (including credentials) for the given
// IDs, ordered by address. The limit caps how many are returned.
func (r *ProxyRepository) GetByIDs(ctx context.Context, ids []int, limit int) ([]*models.Proxy, error) {
	query := fmt.Sprintf("SELECT %s FROM proxies WHERE id = ANY($1) ORDER BY address LIMIT $2", proxyFullColumns)
	rows, err := r.db.Pool.Query(ctx, query, ids, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get proxies by ids: %w", err)
	}
	return scanProxies(rows)
}

// GetByFilter returns the full proxy records (including credentials) matching
// the given list-style filter, ordered by address. The limit caps how many are
// returned. An empty filter matches every proxy.
func (r *ProxyRepository) GetByFilter(ctx context.Context, filter models.ProxyFilter, limit int) ([]*models.Proxy, error) {
	whereClause, args := buildProxyWhere(filter.Search, filter.Status, filter.Protocol, 1)
	args = append(args, limit)
	query := fmt.Sprintf("SELECT %s FROM proxies %s ORDER BY address LIMIT $%d", proxyFullColumns, whereClause, len(args))
	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get proxies by filter: %w", err)
	}
	return scanProxies(rows)
}

// CountByFilter returns how many proxies match the given list-style filter.
func (r *ProxyRepository) CountByFilter(ctx context.Context, filter models.ProxyFilter) (int, error) {
	whereClause, args := buildProxyWhere(filter.Search, filter.Status, filter.Protocol, 1)
	query := fmt.Sprintf("SELECT COUNT(*) FROM proxies %s", whereClause)
	var total int
	if err := r.db.Pool.QueryRow(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("failed to count proxies by filter: %w", err)
	}
	return total, nil
}

// GetStats retrieves overall proxy statistics
func (r *ProxyRepository) GetStats(ctx context.Context) (map[string]interface{}, error) {
	query := `
		SELECT
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE status = 'active') as active,
			COUNT(*) FILTER (WHERE status = 'failed') as failed,
			COUNT(*) FILTER (WHERE status = 'idle') as idle,
			COALESCE(SUM(requests), 0) as total_requests,
			COALESCE(AVG(avg_response_time), 0) as avg_response_time
		FROM proxies
	`

	var stats struct {
		Total           int
		Active          int
		Failed          int
		Idle            int
		TotalRequests   int64
		AvgResponseTime float64
	}

	err := r.db.Pool.QueryRow(ctx, query).Scan(
		&stats.Total, &stats.Active, &stats.Failed, &stats.Idle,
		&stats.TotalRequests, &stats.AvgResponseTime,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to get stats: %w", err)
	}

	return map[string]interface{}{
		"total":             stats.Total,
		"active":            stats.Active,
		"failed":            stats.Failed,
		"idle":              stats.Idle,
		"total_requests":    stats.TotalRequests,
		"avg_response_time": int(stats.AvgResponseTime),
	}, nil
}

// GetAllActive retrieves all active proxies
func (r *ProxyRepository) GetAllActive(ctx context.Context) ([]models.ProxyStatusSimple, error) {
	query := `
		SELECT
			id, address, status, requests,
			successful_requests, failed_requests
		FROM proxies
		WHERE status = 'active'
		ORDER BY address
	`

	rows, err := r.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get active proxies: %w", err)
	}
	defer rows.Close()

	proxies := []models.ProxyStatusSimple{}
	for rows.Next() {
		var p struct {
			ID                 int
			Address            string
			Status             string
			Requests           int64
			SuccessfulRequests int64
			FailedRequests     int64
		}

		err := rows.Scan(&p.ID, &p.Address, &p.Status, &p.Requests, &p.SuccessfulRequests, &p.FailedRequests)
		if err != nil {
			return nil, fmt.Errorf("failed to scan proxy: %w", err)
		}

		successRate := 0.0
		if p.Requests > 0 {
			successRate = (float64(p.SuccessfulRequests) / float64(p.Requests)) * 100
		}

		proxies = append(proxies, models.ProxyStatusSimple{
			ID:          fmt.Sprintf("%d", p.ID),
			Address:     p.Address,
			Status:      p.Status,
			Requests:    p.Requests,
			SuccessRate: successRate,
		})
	}

	return proxies, nil
}
