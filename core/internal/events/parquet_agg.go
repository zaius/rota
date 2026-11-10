package events

import (
	"maps"
	"slices"
	"strings"

	"github.com/alpkeskin/rota/core/internal/models"
)

// latencyLowLimit bounds the flat array of latency counts. Almost every
// proxied request lands under it; the slow tail goes to a map.
const latencyLowLimit = 1 << 12

// latencyCounts is an exact multiset of millisecond latencies. Latencies are
// small integers, so counting each value yields exact percentiles in bounded
// memory, however many requests a range covers.
type latencyCounts struct {
	total uint64
	low   []uint32
	high  map[int32]uint32
}

func (c *latencyCounts) add(ms int32) {
	ms = max(ms, 0)
	c.total++
	if ms >= latencyLowLimit {
		if c.high == nil {
			c.high = map[int32]uint32{}
		}
		c.high[ms]++
		return
	}
	if int(ms) >= len(c.low) {
		c.growLow(max(int(ms)+1, 2*len(c.low)))
	}
	c.low[ms]++
}

func (c *latencyCounts) growLow(size int) {
	grown := make([]uint32, min(size, latencyLowLimit))
	copy(grown, c.low)
	c.low = grown
}

func (c *latencyCounts) merge(other *latencyCounts) {
	c.total += other.total
	if len(other.low) > len(c.low) {
		c.growLow(len(other.low))
	}
	for ms, n := range other.low {
		c.low[ms] += n
	}
	if len(other.high) > 0 && c.high == nil {
		c.high = make(map[int32]uint32, len(other.high))
	}
	for ms, n := range other.high {
		c.high[ms] += n
	}
}

// percentile interpolates between ranks like Postgres' percentile_cont and
// returns 0 for an empty set.
func (c *latencyCounts) percentile(q float64) float64 {
	if c.total == 0 {
		return 0
	}
	position := q * float64(c.total-1)
	rank := uint64(position)
	fraction := position - float64(rank)
	lower := c.valueAt(rank)
	if fraction == 0 {
		return float64(lower)
	}
	return float64(lower) + fraction*float64(c.valueAt(rank+1)-lower)
}

func (c *latencyCounts) valueAt(rank uint64) int32 {
	var seen uint64
	for ms, n := range c.low {
		seen += uint64(n)
		if seen > rank {
			return int32(ms)
		}
	}
	for _, ms := range slices.Sorted(maps.Keys(c.high)) {
		seen += uint64(c.high[ms])
		if seen > rank {
			return ms
		}
	}
	return 0
}

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
	latency                        latencyCounts
}

// bucketAgg groups requests into fixed-width buckets aligned to the epoch.
type bucketAgg struct {
	from, width int64
	latencies   bool
	buckets     map[int64]*bucketCounts
}

func newBucketAgg(from, width int64, latencies bool) *bucketAgg {
	return &bucketAgg{from: from, width: width, latencies: latencies, buckets: map[int64]*bucketCounts{}}
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
			if a.latencies {
				current.latency.add(b.responseTime[i])
			}
		}
	}
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
		counts.latency.merge(&from.latency)
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
	latency                                     latencyCounts
}

// domainAgg groups one event table by hostname. filter, when set, keeps that
// hostname and its subdomains.
type domainAgg struct {
	from    int64
	filter  string
	tunnels bool
	domains map[string]*domainCounts
}

func newDomainAgg(from int64, filter string, tunnels bool) *domainAgg {
	return &domainAgg{from: from, filter: filter, tunnels: tunnels, domains: map[string]*domainCounts{}}
}

func (a *domainAgg) keeps(domain string) bool {
	if domain == "" {
		return false
	}
	return a.filter == "" || domain == a.filter || strings.HasSuffix(domain, "."+a.filter)
}

func (a *domainAgg) add(b *eventBatch) {
	// excluded marks a domain the filter rejected, so each ID resolves once.
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
			if name := b.domains[id]; a.keeps(name) {
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
			counts.latency.add(b.responseTime[i])
		}
	}
}

func (a *domainAgg) merge(other *domainAgg) {
	for name, from := range other.domains {
		counts := a.domains[name]
		if counts == nil {
			a.domains[name] = from
			continue
		}
		counts.requests += from.requests
		counts.successes += from.successes
		counts.rateLimited += from.rateLimited
		counts.successMs += from.successMs
		counts.tunnels += from.tunnels
		counts.tunnelErrors += from.tunnelErrors
		counts.bytesUp += from.bytesUp
		counts.bytesDown += from.bytesDown
		counts.latency.merge(&from.latency)
	}
}

// stats orders domains by total traffic, then name, and applies limit.
func (a *domainAgg) stats(limit int) []models.DomainStats {
	stats := make([]models.DomainStats, 0, len(a.domains))
	for name, counts := range a.domains {
		st := models.DomainStats{
			Domain:       name,
			Requests:     counts.requests,
			Successes:    counts.successes,
			Failures:     counts.requests - counts.successes,
			RateLimited:  counts.rateLimited,
			Tunnels:      counts.tunnels,
			TunnelErrors: counts.tunnelErrors,
			BytesUp:      counts.bytesUp,
			BytesDown:    counts.bytesDown,
		}
		if counts.successes > 0 {
			st.AvgResponseTime = int(counts.successMs / counts.successes)
			st.P50Ms = int(counts.latency.percentile(0.5))
			st.P95Ms = int(counts.latency.percentile(0.95))
		}
		stats = append(stats, st)
	}
	slices.SortFunc(stats, func(x, y models.DomainStats) int {
		if diff := (y.Requests + y.Tunnels) - (x.Requests + x.Tunnels); diff != 0 {
			if diff > 0 {
				return 1
			}
			return -1
		}
		return strings.Compare(x.Domain, y.Domain)
	})
	if limit >= 0 && len(stats) > limit {
		stats = stats[:limit]
	}
	return stats
}
