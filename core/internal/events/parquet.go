package events

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
)

// ParquetStore implements Store on local files, so request and tunnel history
// needs no database server.
//
// Each event table is a directory with one file per UTC day. The open day is a
// tab-separated log; every event is a single append, and queries scan the log
// directly. When a day ends, one conversion replaces its log with a Parquet
// file. Queries read logs and Parquet files through the same aggregations.
//
// A completed append survives a crash or kill of this process. Surviving power
// loss or a kernel crash takes EVENT_STORE_SYNC, which makes every append wait
// for the disk.
//
// One process owns a directory: replicas sharing history need a database
// backend.
type ParquetStore struct {
	logger       *logger.Logger
	observe      FlushObserver
	syncWrites   bool
	partitionDay func() int64

	requests *eventTable
	tunnels  *eventTable

	// filesMu makes a conversion's rename and log removal, and retention's
	// removals, atomic to openFiles.
	filesMu sync.RWMutex

	convertMu     sync.Mutex
	convertSignal chan struct{}
	converterDone chan struct{}
	cancel        context.CancelFunc
	closeOnce     sync.Once

	// rollupClosed holds the rollup of the Parquet files named by rollupKey.
	// Those files never change, so each minute's refresh rescans only logs.
	rollupMu     sync.Mutex
	rollupKey    string
	rollupClosed *proxyAgg
}

var _ Store = (*ParquetStore)(nil)

var lineBuffers = sync.Pool{New: func() any {
	b := make([]byte, 0, 1024)
	return &b
}}

// NewParquetStore opens the event directory and converts any logs left over
// from earlier days. observe, which may be nil, hears every write.
func NewParquetStore(cfg *config.ParquetConfig, log *logger.Logger, observe FlushObserver) (*ParquetStore, error) {
	return newParquetStore(cfg, log, observe, func() int64 { return dayOf(time.Now().UnixMilli()) })
}

func newParquetStore(cfg *config.ParquetConfig, log *logger.Logger, observe FlushObserver, partitionDay func() int64) (*ParquetStore, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ParquetStore{
		logger:        log,
		observe:       observe,
		syncWrites:    cfg.SyncWrites,
		partitionDay:  partitionDay,
		convertSignal: make(chan struct{}, 1),
		converterDone: make(chan struct{}),
		cancel:        cancel,
	}
	s.requests = &eventTable{
		store:        s,
		dir:          filepath.Join(cfg.Dir, "requests"),
		parseColumns: parseRequestColumns,
		convertLog: func(ctx context.Context, lines io.Reader, out io.Writer) (int64, error) {
			return convertRows(ctx, lines, out, parseRequestRow)
		},
	}
	s.tunnels = &eventTable{
		store:        s,
		dir:          filepath.Join(cfg.Dir, "tunnels"),
		parseColumns: parseTunnelColumns,
		convertLog: func(ctx context.Context, lines io.Reader, out io.Writer) (int64, error) {
			return convertRows(ctx, lines, out, parseTunnelRow)
		},
	}
	for _, t := range s.tables() {
		if err := os.MkdirAll(t.dir, 0o750); err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create event directory: %w", err)
		}
		if err := t.removeTemps(); err != nil {
			cancel()
			return nil, fmt.Errorf("failed to clear interrupted conversions: %w", err)
		}
	}

	go s.runConverter(ctx)
	s.requestConversion()
	log.Info("parquet event store ready", "dir", cfg.Dir, "sync_writes", cfg.SyncWrites)
	return s, nil
}

func (s *ParquetStore) tables() []*eventTable {
	return []*eventTable{s.requests, s.tunnels}
}

// Close stops the converter and closes the open logs. A conversion in progress
// stops; the next start repeats it from the intact log.
func (s *ParquetStore) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.converterDone
		for _, t := range s.tables() {
			if err := t.close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

func (s *ParquetStore) requestConversion() {
	select {
	case s.convertSignal <- struct{}{}:
	default:
	}
}

func (s *ParquetStore) runConverter(ctx context.Context) {
	defer close(s.converterDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.convertSignal:
			safeworker.Call(s.logger, "event_log_convert", func() { s.convertPending(ctx) })
		}
	}
}

// convertPending converts every log older than the day being written. A log
// that fails stays queryable, and the next rotation or start retries it.
func (s *ParquetStore) convertPending(ctx context.Context) {
	s.convertMu.Lock()
	defer s.convertMu.Unlock()

	for _, t := range s.tables() {
		writing := t.writingDay()
		listed, err := t.list()
		if err != nil {
			s.logger.Error("failed to list event logs", "error", err)
			continue
		}
		for _, day := range slices.Sorted(maps.Keys(listed)) {
			if day >= writing || !listed[day].log || ctx.Err() != nil {
				continue
			}
			started := time.Now()
			if err := t.convert(ctx, day, listed[day]); err != nil {
				if ctx.Err() == nil {
					s.logger.Error("failed to convert event log", "dir", t.dir, "day", dayName(day), "error", err)
				}
				continue
			}
			s.logger.Info("converted event log to parquet", "dir", t.dir, "day", dayName(day), "elapsed", time.Since(started).String())
		}
	}
}

func (s *ParquetStore) write(ctx context.Context, t *eventTable, kind string, count int, lines []byte) error {
	err := t.append(lines)
	if s.observe != nil {
		s.observe(ctx, kind, count, err == nil)
	}
	if err != nil {
		return fmt.Errorf("failed to append %s event: %w", kind, err)
	}
	return nil
}

// InsertRequest appends one proxied request outcome.
func (s *ParquetStore) InsertRequest(ctx context.Context, event RequestEvent) error {
	return s.InsertRequests(ctx, []RequestEvent{event})
}

// InsertRequests appends a batch of request outcomes in one write.
func (s *ParquetStore) InsertRequests(ctx context.Context, events []RequestEvent) error {
	if len(events) == 0 {
		return nil
	}
	buf := lineBuffers.Get().(*[]byte)
	defer lineBuffers.Put(buf)
	*buf = (*buf)[:0]
	for i := range events {
		*buf = appendRequestLine(*buf, &events[i])
	}
	return s.write(ctx, s.requests, "request", len(events), *buf)
}

// InsertTunnel appends one completed CONNECT tunnel.
func (s *ParquetStore) InsertTunnel(ctx context.Context, event TunnelEvent) error {
	return s.InsertTunnels(ctx, []TunnelEvent{event})
}

// InsertTunnels appends a batch of completed tunnels in one write.
func (s *ParquetStore) InsertTunnels(ctx context.Context, events []TunnelEvent) error {
	if len(events) == 0 {
		return nil
	}
	buf := lineBuffers.Get().(*[]byte)
	defer lineBuffers.Put(buf)
	*buf = (*buf)[:0]
	for i := range events {
		*buf = appendTunnelLine(*buf, &events[i])
	}
	return s.write(ctx, s.tunnels, "tunnel", len(events), *buf)
}

// RequestStats returns today/yesterday request aggregates for the dashboard.
func (s *ParquetStore) RequestStats(ctx context.Context) (*RequestStats, error) {
	now := time.Now()
	todayFrom := now.Add(-24 * time.Hour).UnixMilli()
	yesterdayFrom := now.Add(-48 * time.Hour).UnixMilli()
	agg, err := collect(ctx, s.requests, dayOf(yesterdayFrom), requestStatsColumns, func() *requestStatsAgg {
		return &requestStatsAgg{todayFrom: todayFrom, yesterdayFrom: yesterdayFrom}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get request stats: %w", err)
	}
	return &RequestStats{
		RequestsToday:         agg.today.requests,
		SuccessRateToday:      agg.today.successRate(),
		ResponseTimeToday:     agg.today.averageMs(),
		RequestsYesterday:     agg.yesterday.requests,
		SuccessRateYesterday:  agg.yesterday.successRate(),
		ResponseTimeYesterday: agg.yesterday.averageMs(),
	}, nil
}

// TunnelStats aggregates tunnels that closed within the trailing window.
func (s *ParquetStore) TunnelStats(ctx context.Context, window time.Duration) (*TunnelSummary, error) {
	from := time.Now().Add(-window).UnixMilli()
	agg, err := collect(ctx, s.tunnels, dayOf(from), tunnelStatsColumns, func() *tunnelStatsAgg {
		return &tunnelStatsAgg{from: from}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get tunnel stats: %w", err)
	}
	return &agg.summary, nil
}

// ProxyRollup returns per-proxy request aggregates over the whole event
// window.
func (s *ParquetStore) ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error) {
	s.rollupMu.Lock()
	defer s.rollupMu.Unlock()

	files, err := s.requests.openFiles(0)
	if err != nil {
		return nil, fmt.Errorf("failed to roll up proxy requests: %w", err)
	}
	defer closeEventFiles(files)

	var closed, open []*eventFile
	var names []string
	for _, f := range files {
		if f.parquet {
			closed = append(closed, f)
			names = append(names, filepath.Base(f.path))
		} else {
			open = append(open, f)
		}
	}
	fresh := func() *proxyAgg { return newProxyAgg(math.MinInt64) }

	if key := strings.Join(names, ","); s.rollupClosed == nil || key != s.rollupKey {
		parts, err := scanFiles(ctx, closed, proxyColumns, fresh)
		if err != nil {
			return nil, fmt.Errorf("failed to roll up proxy requests: %w", err)
		}
		total := fresh()
		for _, part := range parts {
			total.merge(part)
		}
		s.rollupClosed, s.rollupKey = total, key
	}

	parts, err := scanFiles(ctx, open, proxyColumns, fresh)
	if err != nil {
		return nil, fmt.Errorf("failed to roll up proxy requests: %w", err)
	}
	total := fresh()
	total.merge(s.rollupClosed)
	for _, part := range parts {
		total.merge(part)
	}
	return total.rollup(), nil
}

// LowSuccessProxies returns the IDs of proxies below minRate percent success
// over the trailing window, with at least minRequests requests.
func (s *ParquetStore) LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error) {
	from := time.Now().Add(-window).UnixMilli()
	agg, err := collect(ctx, s.requests, dayOf(from), proxyColumns, func() *proxyAgg { return newProxyAgg(from) })
	if err != nil {
		return nil, fmt.Errorf("failed to query low-success proxies: %w", err)
	}
	ids := []int{}
	for _, st := range agg.rollup() {
		if st.Requests >= int64(minRequests) && float64(st.Successes)*100/float64(st.Requests) < minRate {
			ids = append(ids, st.ProxyID)
		}
	}
	return ids, nil
}

// TrafficSeries returns request volume and latency percentiles bucketed over
// the trailing range, zero-filled to a dense series.
func (s *ParquetStore) TrafficSeries(ctx context.Context, rng string) ([]models.TrafficPoint, error) {
	bucket, lookback := SeriesWindow(rng)
	now := time.Now()
	from := now.Add(-lookback).UnixMilli()
	agg, err := collect(ctx, s.requests, dayOf(from), bucketColumns, func() *bucketAgg {
		return newBucketAgg(from, bucket.Milliseconds(), true)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get traffic series: %w", err)
	}

	points := make([]models.TrafficPoint, 0, len(agg.buckets))
	for _, start := range agg.sortedStarts() {
		counts := agg.buckets[start]
		points = append(points, models.TrafficPoint{
			Time:      time.UnixMilli(start).UTC(),
			Requests:  counts.requests,
			Successes: counts.successes,
			P50Ms:     int(counts.latency.percentile(0.5)),
			P95Ms:     int(counts.latency.percentile(0.95)),
		})
	}
	return fillTrafficGaps(points, bucket, lookback, now), nil
}

// DomainStats groups traffic by target hostname over the trailing range.
func (s *ParquetStore) DomainStats(ctx context.Context, rng, domain string, limit int) ([]models.DomainStats, error) {
	_, window := SeriesWindow(rng)
	from := time.Now().Add(-window).UnixMilli()
	total, err := collect(ctx, s.requests, dayOf(from), domainRequestColumns, func() *domainAgg {
		return newDomainAgg(from, domain, false)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get domain stats: %w", err)
	}
	tunnels, err := collect(ctx, s.tunnels, dayOf(from), domainTunnelColumns, func() *domainAgg {
		return newDomainAgg(from, domain, true)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get domain stats: %w", err)
	}
	total.merge(tunnels)
	return total.stats(limit), nil
}

// chartBuckets maps an API interval to a bucket width and lookback period.
func chartBuckets(interval string) (width, lookback time.Duration) {
	switch interval {
	case "1h":
		return time.Hour, 24 * time.Hour
	case "1d":
		return 24 * time.Hour, 7 * 24 * time.Hour
	default: // "4h"
		return 4 * time.Hour, 24 * time.Hour
	}
}

func (s *ParquetStore) chartAgg(ctx context.Context, interval string) (*bucketAgg, error) {
	width, lookback := chartBuckets(interval)
	from := time.Now().Add(-lookback).UnixMilli()
	return collect(ctx, s.requests, dayOf(from), bucketColumns, func() *bucketAgg {
		return newBucketAgg(from, width.Milliseconds(), false)
	})
}

// ResponseTimeChart returns average response time of successful requests
// bucketed over time.
func (s *ParquetStore) ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	agg, err := s.chartAgg(ctx, interval)
	if err != nil {
		return nil, fmt.Errorf("failed to get response time chart: %w", err)
	}
	data := []models.ChartDataPoint{}
	for _, start := range agg.sortedStarts() {
		counts := agg.buckets[start]
		if counts.successes == 0 {
			continue
		}
		data = append(data, models.ChartDataPoint{
			Time:  chartBucketLabel(time.UnixMilli(start).UTC(), interval),
			Value: int(counts.successMs / counts.successes),
		})
	}
	return data, nil
}

// SuccessRateChart returns success/failure percentages bucketed over time.
func (s *ParquetStore) SuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error) {
	agg, err := s.chartAgg(ctx, interval)
	if err != nil {
		return nil, fmt.Errorf("failed to get success rate chart: %w", err)
	}
	data := []models.SuccessRateDataPoint{}
	for _, start := range agg.sortedStarts() {
		counts := agg.buckets[start]
		data = append(data, models.SuccessRateDataPoint{
			Time:    chartBucketLabel(time.UnixMilli(start).UTC(), interval),
			Success: int(counts.successes * 100 / counts.requests),
			Failure: int((counts.requests - counts.successes) * 100 / counts.requests),
		})
	}
	return data, nil
}

// ApplyRetention removes the files of expired days. A file goes only once its
// whole day has aged past retention, so no event is deleted early.
func (s *ParquetStore) ApplyRetention(_ context.Context, cfg RetentionConfig) error {
	if cfg.RequestRetentionDays <= 0 {
		return nil
	}
	before := s.partitionDay() - int64(cfg.RequestRetentionDays)
	for _, t := range s.tables() {
		if err := t.expire(before); err != nil {
			return fmt.Errorf("failed to expire event files: %w", err)
		}
	}
	return nil
}
