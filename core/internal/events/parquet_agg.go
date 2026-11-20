package events

import (
	"maps"
	"slices"
	"strings"
)

type windowTotals struct {
	requests, successes, responseMs int64
}

func (w *windowTotals) merge(other windowTotals) {
	w.requests += other.requests
	w.successes += other.successes
	w.responseMs += other.responseMs
}

func (w windowTotals) successRate() float64 {
	if w.requests == 0 {
		return 0
	}
	return float64(w.successes) * 100 / float64(w.requests)
}

func (w windowTotals) averageMs() int {
	if w.requests == 0 {
		return 0
	}
	return int(w.responseMs / w.requests)
}

const requestStatsColumns = colTimestamp | colResponseTime | colSuccess

type requestStatsAgg struct {
	todayFrom, yesterdayFrom int64
	today, yesterday         windowTotals
}

func (a *requestStatsAgg) add(b *eventBatch) {
	for i := range b.n {
		var window *windowTotals
		switch ts := b.timestamp[i]; {
		case ts >= a.todayFrom:
			window = &a.today
		case ts >= a.yesterdayFrom:
			window = &a.yesterday
		default:
			continue
		}
		window.requests++
		window.responseMs += int64(b.responseTime[i])
		if b.success[i] {
			window.successes++
		}
	}
}

func (a *requestStatsAgg) merge(other *requestStatsAgg) {
	a.today.merge(other.today)
	a.yesterday.merge(other.yesterday)
}

const tunnelStatsColumns = colTimestamp | colDurationMs | colBytesUp | colBytesDown | colRequests

// tunnelStatsAgg counts tunnels by close time.
type tunnelStatsAgg struct {
	from    int64
	summary TunnelSummary
}

func (a *tunnelStatsAgg) add(b *eventBatch) {
	for i := range b.n {
		if b.timestamp[i]+int64(b.durationMs[i]) < a.from {
			continue
		}
		a.summary.Tunnels++
		a.summary.BytesUp += b.bytesUp[i]
		a.summary.BytesDown += b.bytesDown[i]
		a.summary.Requests += int64(b.requests[i])
		a.summary.TotalDurationMs += int64(b.durationMs[i])
	}
}

func (a *tunnelStatsAgg) merge(other *tunnelStatsAgg) {
	a.summary.Tunnels += other.summary.Tunnels
	a.summary.BytesUp += other.summary.BytesUp
	a.summary.BytesDown += other.summary.BytesDown
	a.summary.Requests += other.summary.Requests
	a.summary.TotalDurationMs += other.summary.TotalDurationMs
}

const proxyColumns = colTimestamp | colProxyID | colResponseTime | colSuccess | colTargetFailure

type proxyCounts struct {
	requests, successes, successMs int64
}

// proxyAgg sums proxy reliability, excluding target failures.
type proxyAgg struct {
	from    int64
	proxies map[int32]*proxyCounts
}

func newProxyAgg(from int64) *proxyAgg {
	return &proxyAgg{from: from, proxies: map[int32]*proxyCounts{}}
}

func (a *proxyAgg) add(b *eventBatch) {
	for i := range b.n {
		if b.proxyID[i] <= 0 || b.targetFailure[i] || b.timestamp[i] < a.from {
			continue
		}
		counts := a.proxies[b.proxyID[i]]
		if counts == nil {
			counts = &proxyCounts{}
			a.proxies[b.proxyID[i]] = counts
		}
		counts.requests++
		if b.success[i] {
			counts.successes++
			counts.successMs += int64(b.responseTime[i])
		}
	}
}

// merge copies other's counts, so the cached rollup of finished files stays
// unchanged when a live result merges it.
func (a *proxyAgg) merge(other *proxyAgg) {
	for id, from := range other.proxies {
		counts := a.proxies[id]
		if counts == nil {
			counts = &proxyCounts{}
			a.proxies[id] = counts
		}
		counts.requests += from.requests
		counts.successes += from.successes
		counts.successMs += from.successMs
	}
}

func (a *proxyAgg) rollup() []ProxyRequestStats {
	stats := make([]ProxyRequestStats, 0, len(a.proxies))
	for _, id := range slices.Sorted(maps.Keys(a.proxies)) {
		counts := a.proxies[id]
		st := ProxyRequestStats{ProxyID: int(id), Requests: counts.requests, Successes: counts.successes}
		if counts.successes > 0 {
			st.AvgResponseTime = int(counts.successMs / counts.successes)
		}
		stats = append(stats, st)
	}
	return stats
}

const bucketColumns = colTimestamp | colResponseTime | colSuccess

type bucketCounts struct {
	requests, successes, successMs int64
}

// bucketAgg groups requests into fixed-width buckets aligned to the epoch.
type bucketAgg struct {
	from, width int64
	buckets     map[int64]*bucketCounts
}

func newBucketAgg(from, width int64) *bucketAgg {
	return &bucketAgg{from: from, width: width, buckets: map[int64]*bucketCounts{}}
}

func (a *bucketAgg) add(b *eventBatch) {
	var current *bucketCounts
	currentStart := int64(-1)
	for i := range b.n {
		ts := b.timestamp[i]
		if ts < a.from {
			continue
		}
		// Events arrive nearly in time order, so most rows stay in the
		// bucket of the row before and skip the map lookup.
		if start := ts - ts%a.width; start != currentStart {
			currentStart = start
			current = a.buckets[start]
			if current == nil {
				current = &bucketCounts{}
				a.buckets[start] = current
			}
		}
		current.requests++
		if b.success[i] {
			current.successes++
			current.successMs += int64(b.responseTime[i])
		}
	}
}

// load takes a finished day's daily buckets from its summary instead of its
// rows.
func (a *bucketAgg) load(f *eventFile) (bool, error) {
	if a.width != dayMillis || f.summary == "" {
		return false, nil
	}
	rows, err := readSummary(f.summary, true)
	for i := range rows {
		if rows[i].Domain == "" && rows[i].Bucket >= a.from {
			a.merge(&bucketAgg{buckets: map[int64]*bucketCounts{
				rows[i].Bucket: {requests: rows[i].Requests, successes: rows[i].Successes, successMs: rows[i].SuccessMs},
			}})
		}
	}
	return true, err
}

// rows returns the buckets as summary rows, in time order.
func (a *bucketAgg) rows() []summaryRow {
	rows := make([]summaryRow, 0, len(a.buckets))
	for _, start := range a.sortedStarts() {
		counts := a.buckets[start]
		rows = append(rows, summaryRow{Bucket: start, Requests: counts.requests, Successes: counts.successes, SuccessMs: counts.successMs})
	}
	return rows
}

func (a *bucketAgg) merge(other *bucketAgg) {
	for start, from := range other.buckets {
		counts := a.buckets[start]
		if counts == nil {
			a.buckets[start] = from
			continue
		}
		counts.requests += from.requests
		counts.successes += from.successes
		counts.successMs += from.successMs
	}
}

// sortedStarts returns the bucket start times in ascending order.
func (a *bucketAgg) sortedStarts() []int64 {
	return slices.Sorted(maps.Keys(a.buckets))
}

const (
	domainRequestColumns = colTimestamp | colDomain | colResponseTime | colStatusCode | colSuccess
	domainTunnelColumns  = colTimestamp | colDomain | colDurationMs | colBytesUp | colBytesDown | colFailed
)

type domainCounts struct {
	requests, successes, rateLimited, successMs int64
	tunnels, tunnelErrors, bytesUp, bytesDown   int64
}

// domainAgg groups one file of an event table by hostname, leaving out events
// with none. A from of zero counts every event in the file.
type domainAgg struct {
	from    int64
	tunnels bool
	domains map[string]*domainCounts
	// summary names a finished day's summary when it stands in for the day's
	// rows. rows reads it, so only one day's summary is in memory at a time.
	summary string
}

func newDomainAgg(from int64, tunnels bool) *domainAgg {
	return &domainAgg{from: from, tunnels: tunnels, domains: map[string]*domainCounts{}}
}

func (a *domainAgg) load(f *eventFile) (bool, error) {
	if a.from == 0 {
		a.summary = f.summary
	}
	return a.summary != "", nil
}

func (a *domainAgg) add(b *eventBatch) {
	// excluded marks the events with no domain, so each ID resolves once.
	excluded := &domainCounts{}
	resolved := make([]*domainCounts, len(b.domains))
	for i := range b.n {
		if a.tunnels {
			if b.timestamp[i]+int64(b.durationMs[i]) < a.from {
				continue
			}
		} else if b.timestamp[i] < a.from {
			continue
		}

		id := b.domain[i]
		counts := resolved[id]
		if counts == nil {
			counts = excluded
			if name := b.domains[id]; name != "" {
				if counts = a.domains[name]; counts == nil {
					counts = &domainCounts{}
					a.domains[name] = counts
				}
			}
			resolved[id] = counts
		}
		if counts == excluded {
			continue
		}

		if a.tunnels {
			counts.tunnels++
			counts.bytesUp += b.bytesUp[i]
			counts.bytesDown += b.bytesDown[i]
			if b.failed[i] {
				counts.tunnelErrors++
			}
			continue
		}
		counts.requests++
		if b.statusCode[i] == 429 {
			counts.rateLimited++
		}
		if b.success[i] {
			counts.successes++
			counts.successMs += int64(b.responseTime[i])
		}
	}
}

// rows returns the file's domains as summary rows sorted by domain.
func (a *domainAgg) rows() ([]summaryRow, error) {
	if a.summary != "" {
		return readSummary(a.summary, false)
	}
	rows := make([]summaryRow, 0, len(a.domains))
	for name, counts := range a.domains {
		rows = append(rows, summaryRow{
			Domain: name, Requests: counts.requests, Successes: counts.successes, RateLimited: counts.rateLimited,
			SuccessMs: counts.successMs, Tunnels: counts.tunnels, TunnelErrors: counts.tunnelErrors,
			BytesUp: counts.bytesUp, BytesDown: counts.bytesDown,
		})
	}
	slices.SortFunc(rows, func(x, y summaryRow) int { return strings.Compare(x.Domain, y.Domain) })
	return rows, nil
}
