package events

import (
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// SeriesWindow maps a chart range to its bucket width and lookback period.
// Bucket widths aim for roughly 50 points per series. Unknown ranges fall
// back to 24h. All widths divide a day evenly, so epoch-aligned bucketing
// (ClickHouse toStartOfInterval) and date_bin with a midnight origin
// (Postgres) land on identical boundaries.
func SeriesWindow(rng string) (bucket, lookback time.Duration) {
	switch rng {
	case "1h":
		return time.Minute, time.Hour
	case "6h":
		return 5 * time.Minute, 6 * time.Hour
	case "7d":
		return 4 * time.Hour, 7 * 24 * time.Hour
	case "30d":
		return 24 * time.Hour, 30 * 24 * time.Hour
	default: // "24h"
		return 30 * time.Minute, 24 * time.Hour
	}
}

// fillTrafficGaps expands a sparse, time-ascending traffic series into a
// dense one: every bucket from (now - lookback) through now is present, with
// zero-valued points where the backend returned no row. Charts then render
// quiet periods as zero traffic instead of interpolating across them.
func fillTrafficGaps(points []models.TrafficPoint, bucket, lookback time.Duration, now time.Time) []models.TrafficPoint {
	byBucket := make(map[int64]models.TrafficPoint, len(points))
	for _, p := range points {
		byBucket[p.Time.Unix()] = p
	}

	start := now.Add(-lookback).UTC().Truncate(bucket)
	end := now.UTC().Truncate(bucket)

	filled := make([]models.TrafficPoint, 0, int(end.Sub(start)/bucket)+1)
	for t := start; !t.After(end); t = t.Add(bucket) {
		if p, ok := byBucket[t.Unix()]; ok {
			filled = append(filled, p)
			continue
		}
		filled = append(filled, models.TrafficPoint{Time: t})
	}
	return filled
}
