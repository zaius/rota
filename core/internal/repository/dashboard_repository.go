package repository

import (
	"context"
	"fmt"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
)

// DashboardRepository assembles dashboard statistics. Proxy fleet aggregates
// come from the primary database; request history comes from the event store.
// The two must never meet in SQL — they are merged here, in Go, so the event
// store can live in a different database entirely.
type DashboardRepository struct {
	db     *database.DB
	events events.Store
}

// NewDashboardRepository creates a new DashboardRepository
func NewDashboardRepository(db *database.DB, eventStore events.Store) *DashboardRepository {
	return &DashboardRepository{db: db, events: eventStore}
}

// GetStats retrieves overall dashboard statistics
func (r *DashboardRepository) GetStats(ctx context.Context) (*models.DashboardStats, error) {
	query := `
		SELECT
			COUNT(*) FILTER (WHERE status = 'active') as active_proxies,
			COUNT(*) as total_proxies,
			COALESCE(SUM(requests), 0) as total_requests,
			COALESCE(AVG(CASE WHEN requests > 0 THEN (successful_requests::float / requests * 100) END), 0) as avg_success_rate,
			COALESCE(AVG(avg_response_time), 0)::int as avg_response_time
		FROM proxies
	`

	var stats models.DashboardStats
	err := r.db.Pool.QueryRow(ctx, query).Scan(
		&stats.ActiveProxies,
		&stats.TotalProxies,
		&stats.TotalRequests,
		&stats.AvgSuccessRate,
		&stats.AvgResponseTime,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard stats: %w", err)
	}

	reqStats, err := r.events.RequestStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get request stats: %w", err)
	}

	if reqStats.RequestsYesterday > 0 {
		stats.RequestGrowth = float64(reqStats.RequestsToday-reqStats.RequestsYesterday) / float64(reqStats.RequestsYesterday) * 100
	}
	stats.SuccessRateGrowth = reqStats.SuccessRateToday - reqStats.SuccessRateYesterday
	stats.ResponseTimeDelta = reqStats.ResponseTimeToday - reqStats.ResponseTimeYesterday

	return &stats, nil
}

// GetTrafficChart retrieves the traffic series (volume + latency percentiles)
// for the given range.
func (r *DashboardRepository) GetTrafficChart(ctx context.Context, rng string) ([]models.TrafficPoint, error) {
	return r.events.TrafficSeries(ctx, rng)
}

// GetResponseTimeChart retrieves response time chart data
func (r *DashboardRepository) GetResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	return r.events.ResponseTimeChart(ctx, interval)
}

// GetSuccessRateChart retrieves success rate chart data
func (r *DashboardRepository) GetSuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error) {
	return r.events.SuccessRateChart(ctx, interval)
}
