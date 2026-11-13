package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/parquet-go/parquet-go"
)

// fakeDay supplies the store's partition day. It starts at the real day and
// moves only when a test advances it, simulating midnight without changing the
// clock that trailing windows use.
type fakeDay struct{ day atomic.Int64 }

func newFakeDay() *fakeDay {
	f := &fakeDay{}
	f.day.Store(dayOf(time.Now().UnixMilli()))
	return f
}

func (f *fakeDay) get() int64 { return f.day.Load() }

const testInstance = "rota"

func newTestParquetStore(t *testing.T, dir string, day *fakeDay) *ParquetStore {
	t.Helper()
	return newTestParquetStoreAs(t, dir, day, testInstance)
}

func newTestParquetStoreAs(t *testing.T, dir string, day *fakeDay, instance string) *ParquetStore {
	t.Helper()
	store, err := newParquetStore(&config.ParquetConfig{Dir: dir, Instance: instance}, logger.New("error"), nil, day.get)
	if err != nil {
		t.Fatalf("open parquet store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// midnight moves the partition day forward and finishes the resulting
// conversions before returning.
func midnight(t *testing.T, store *ParquetStore, day *fakeDay) {
	t.Helper()
	next := day.day.Add(1)
	for _, table := range store.tables() {
		if err := table.rotate(next); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	store.convertPending(context.Background())
}

// sealingStore finishes the current day before every read, so testStores
// exercises the Parquet read path instead of the log scan.
type sealingStore struct {
	*ParquetStore
	day *fakeDay
}

func (s sealingStore) seal() {
	next := s.day.day.Add(1)
	for _, table := range s.tables() {
		if err := table.rotate(next); err != nil {
			panic(err)
		}
	}
	s.convertPending(context.Background())
}

func (s sealingStore) RequestStats(ctx context.Context) (*RequestStats, error) {
	s.seal()
	return s.ParquetStore.RequestStats(ctx)
}

func (s sealingStore) TunnelStats(ctx context.Context, window time.Duration) (*TunnelSummary, error) {
	s.seal()
	return s.ParquetStore.TunnelStats(ctx, window)
}

func (s sealingStore) ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error) {
	s.seal()
	return s.ParquetStore.ProxyRollup(ctx)
}

func (s sealingStore) LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error) {
	s.seal()
	return s.ParquetStore.LowSuccessProxies(ctx, window, minRate, minRequests)
}

func (s sealingStore) TrafficSeries(ctx context.Context, rng string) ([]models.TrafficPoint, error) {
	s.seal()
	return s.ParquetStore.TrafficSeries(ctx, rng)
}

func (s sealingStore) DomainStats(ctx context.Context, rng, domain string, limit int) ([]models.DomainStats, error) {
	s.seal()
	return s.ParquetStore.DomainStats(ctx, rng, domain, limit)
}

func (s sealingStore) ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	s.seal()
	return s.ParquetStore.ResponseTimeChart(ctx, interval)
}

func (s sealingStore) SuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error) {
	s.seal()
	return s.ParquetStore.SuccessRateChart(ctx, interval)
}

// readAllRequests returns every stored request with all its fields, from logs
// and Parquet files alike.
func readAllRequests(t *testing.T, store *ParquetStore) []requestRow {
	t.Helper()
	files, err := store.requests.openFiles(0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeEventFiles(files)

	rows := []requestRow{}
	for _, f := range files {
		if !f.parquet {
			err := forEachLine(context.Background(), io.NewSectionReader(f.file, 0, f.size), func(line []byte) error {
				var row requestRow
				if parseRequestRow(line, &row) {
					rows = append(rows, row)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		reader := parquet.NewGenericReader[requestRow](io.NewSectionReader(f.file, 0, f.size))
		batch := make([]requestRow, reader.NumRows())
		if _, err := reader.Read(batch); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		reader.Close()
		rows = append(rows, batch...)
	}
	return rows
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func request(ms int) RequestEvent {
	return RequestEvent{
		ProxyID: 1, ProxyAddress: "10.0.0.1:8080", Username: "alice", Method: "GET",
		URL: "http://example.com/", Domain: "example.com", StatusCode: 200,
		ResponseTime: ms, Success: true, Timestamp: time.Now(),
	}
}

func countToday(t *testing.T, store *ParquetStore) int64 {
	t.Helper()
	stats, err := store.RequestStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.RequestsToday
}

func TestParquetLogRoundTripsHostileText(t *testing.T) {
	hostile := "tab\there\nnewline\r\\back\\tslash"
	invalid := "bad\xffbyte"
	event := RequestEvent{
		ProxyID: 9, ProxyAddress: hostile, PoolID: 3, Username: hostile, Method: "GET",
		URL: hostile, Domain: "example.com", StatusCode: 503, ResponseTime: 42,
		Error: invalid, TargetFailure: true, Timestamp: time.UnixMilli(1_700_000_000_123),
	}
	line := appendRequestLine(nil, &event)
	if bytes.Count(line, []byte{'\n'}) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("event spans more than one line: %q", line)
	}
	if got := bytes.Count(line, []byte{'\t'}); got != requestAllFields-1 {
		t.Fatalf("hostile text forged fields: %d tabs", got)
	}

	var row requestRow
	if !parseRequestRow(line[:len(line)-1], &row) {
		t.Fatal("line did not parse")
	}
	want := requestRow{
		Timestamp: 1_700_000_000_123, ProxyID: 9, ProxyAddress: hostile, PoolID: 3, Username: hostile,
		Method: "GET", URL: hostile, Domain: "example.com", StatusCode: 503, ResponseTime: 42,
		Error: "bad�byte", TargetFailure: true,
	}
	if row != want {
		t.Fatalf("round trip:\n got %+v\nwant %+v", row, want)
	}

	tunnel := TunnelEvent{
		ProxyID: 2, ProxyAddress: "p", PoolID: 1, Username: hostile, Host: hostile,
		Domain: "t.example", BytesUp: 1 << 40, BytesDown: 7, Requests: 3, DurationMs: 11,
		Error: "reset", OpenedAt: time.UnixMilli(1_700_000_000_000),
	}
	line = appendTunnelLine(nil, &tunnel)
	var tr tunnelRow
	if !parseTunnelRow(line[:len(line)-1], &tr) {
		t.Fatal("tunnel line did not parse")
	}
	if tr.Host != hostile || tr.Username != hostile || tr.BytesUp != 1<<40 || tr.Error != "reset" || tr.DurationMs != 11 {
		t.Fatalf("tunnel round trip: %+v", tr)
	}
}

func TestParquetSkipsDamagedLines(t *testing.T) {
	store := newTestParquetStore(t, t.TempDir(), newFakeDay())
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	path := store.requests.path(store.requests.ownKey(store.requests.writingDay()), logSuffix)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, garbage := range []string{"not an event\n", "\n", "abc\t1\t2\t3\t1\t0\tx.com\t0\ta\tb\tc\td\te\n", "\x00\x00\x00\n"} {
		if _, err := file.WriteString(garbage); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	if err := store.InsertRequest(ctx, request(200)); err != nil {
		t.Fatal(err)
	}
	if got := countToday(t, store); got != 2 {
		t.Fatalf("want the 2 valid events, got %d", got)
	}
}

func TestParquetRepairsTornTailOnReopen(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	store := newTestParquetStore(t, dir, day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	path := store.requests.path(store.requests.ownKey(day.get()), logSuffix)
	store.Close()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("1700000000000\t1\t50\t2"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	reopened := newTestParquetStore(t, dir, day)
	if got := countToday(t, reopened); got != 1 {
		t.Fatalf("readers must ignore the partial line: got %d events", got)
	}
	if err := reopened.InsertRequest(ctx, request(200)); err != nil {
		t.Fatal(err)
	}
	if got := countToday(t, reopened); got != 2 {
		t.Fatalf("the append after a torn write was lost: got %d events", got)
	}
}

func TestParquetMidnightConvertsAndKeepsAnswers(t *testing.T) {
	day := newFakeDay()
	store := newTestParquetStore(t, t.TempDir(), day)
	ctx := context.Background()
	for _, ms := range []int{100, 200, 300} {
		if err := store.InsertRequest(ctx, request(ms)); err != nil {
			t.Fatal(err)
		}
	}
	err := store.InsertTunnel(ctx, TunnelEvent{
		ProxyID: 1, ProxyAddress: "p", Host: "example.com:443", Domain: "example.com",
		BytesUp: 5, BytesDown: 6, DurationMs: 10, Error: "reset", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	before, err := store.DomainStats(ctx, "24h", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	closedDay := day.get()
	midnight(t, store, day)

	got := dirNames(t, store.requests.dir)
	want := []string{filepath.Base(store.requests.path(store.requests.ownKey(closedDay), parquetSuffix)),
		filepath.Base(store.requests.path(store.requests.ownKey(day.get()), logSuffix))}
	if !slices.Equal(got, want) {
		t.Fatalf("files after midnight: got %v, want %v", got, want)
	}
	after, err := store.DomainStats(ctx, "24h", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("conversion changed answers:\nbefore %+v\nafter  %+v", before, after)
	}
	if len(after) != 1 || after[0].Requests != 3 || after[0].P50Ms != 200 || after[0].TunnelErrors != 1 {
		t.Fatalf("domain stats: %+v", after)
	}

	rows := readAllRequests(t, store)
	if len(rows) != 3 || rows[0].URL != "http://example.com/" || rows[0].Username != "alice" {
		t.Fatalf("parquet lost fields: %+v", rows)
	}
}

func TestParquetEventsAcrossMidnightStayVisible(t *testing.T) {
	day := newFakeDay()
	store := newTestParquetStore(t, t.TempDir(), day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	day.day.Add(1)
	// The first append of the new day triggers rotation and conversion.
	if err := store.InsertRequest(ctx, request(200)); err != nil {
		t.Fatal(err)
	}
	store.convertPending(ctx)
	if got := countToday(t, store); got != 2 {
		t.Fatalf("want both days' events, got %d", got)
	}
}

func TestParquetLongTunnelCountsWhenItCloses(t *testing.T) {
	store := newTestParquetStore(t, t.TempDir(), newFakeDay())
	ctx := context.Background()
	lifetime := 10 * 24 * time.Hour
	err := store.InsertTunnel(ctx, TunnelEvent{
		ProxyID: 1, ProxyAddress: "p", Host: "long.example:443", Domain: "long.example",
		BytesDown: 99, DurationMs: int(lifetime.Milliseconds()), OpenedAt: time.Now().Add(-lifetime),
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := store.TunnelStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tunnels != 1 || stats.BytesDown != 99 {
		t.Fatalf("a tunnel closing now must count: %+v", stats)
	}
}

func TestParquetResumesAfterInterruptedConversion(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	store := newTestParquetStore(t, dir, day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	closedDay := day.get()
	midnight(t, store, day)
	store.Close()

	// A crash after the rename leaves the log beside its Parquet file, and a
	// crash during the write leaves a temp file.
	requests := filepath.Join(dir, "requests")
	log := store.requests.path(store.requests.ownKey(closedDay), logSuffix)
	if err := os.WriteFile(log, appendRequestLine(nil, ptr(request(100))), 0o640); err != nil {
		t.Fatal(err)
	}
	temp := store.requests.path(store.requests.ownKey(closedDay-1), parquetSuffix+tempSuffix)
	if err := os.WriteFile(temp, []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}

	reopened := newTestParquetStore(t, dir, day)
	if got := countToday(t, reopened); got != 1 {
		t.Fatalf("a day with both files must count once: got %d", got)
	}
	reopened.convertPending(ctx)
	names := dirNames(t, requests)
	if slices.Contains(names, filepath.Base(log)) || slices.Contains(names, filepath.Base(temp)) {
		t.Fatalf("startup left interrupted files behind: %v", names)
	}
}

func ptr[T any](v T) *T { return &v }

func TestParquetStartupConvertsOlderLogs(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	requests := filepath.Join(dir, "requests")
	if err := os.MkdirAll(requests, 0o750); err != nil {
		t.Fatal(err)
	}
	old := day.get() - 3
	if err := os.WriteFile(filepath.Join(requests, dayName(old)+logSuffix), appendRequestLine(nil, ptr(request(100))), 0o640); err != nil {
		t.Fatal(err)
	}

	store := newTestParquetStore(t, dir, day)
	store.convertPending(context.Background())
	if got := dirNames(t, requests); !slices.Equal(got, []string{dayName(old) + parquetSuffix}) {
		t.Fatalf("files after startup: %v", got)
	}
	rollup, err := store.ProxyRollup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rollup) != 1 || rollup[0].Requests != 1 {
		t.Fatalf("rollup after startup conversion: %+v", rollup)
	}
}

func TestParquetReadsEveryInstanceAndConvertsOnlyItsOwn(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	a := newTestParquetStoreAs(t, dir, day, "a")
	b := newTestParquetStoreAs(t, dir, day, "b")
	ctx := context.Background()
	for _, insert := range []*ParquetStore{a, b, b} {
		if err := insert.InsertRequest(ctx, request(100)); err != nil {
			t.Fatal(err)
		}
	}
	today := day.get()
	name := func(day int64, instance, suffix string) string {
		return filepath.Base(a.requests.path(fileKey{day, instance}, suffix))
	}
	check := func(want ...string) {
		t.Helper()
		slices.Sort(want)
		if got := dirNames(t, a.requests.dir); !slices.Equal(got, want) {
			t.Fatalf("files: got %v, want %v", got, want)
		}
		for _, store := range []*ParquetStore{a, b} {
			if got := countToday(t, store); got != 3 {
				t.Fatalf("%s reads %d of the 3 events", store.instance, got)
			}
		}
	}

	midnight(t, a, day)
	check(name(today, "a", parquetSuffix), name(today+1, "a", logSuffix), name(today, "b", logSuffix))

	for _, table := range b.tables() {
		if err := table.rotate(day.get()); err != nil {
			t.Fatal(err)
		}
	}
	b.convertPending(ctx)
	check(name(today, "a", parquetSuffix), name(today+1, "a", logSuffix),
		name(today, "b", parquetSuffix), name(today+1, "b", logSuffix))
}

// A log named without an instance, still open when this store starts, gets
// converted once its day ends, beside the store's own log for that day.
func TestParquetConvertsUnnamedLogs(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	today := day.get()
	requests := filepath.Join(dir, "requests")
	if err := os.MkdirAll(requests, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int64{today - 2, today} {
		if err := os.WriteFile(filepath.Join(requests, dayName(d)+logSuffix), appendRequestLine(nil, ptr(request(100))), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	store := newTestParquetStore(t, dir, day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(200)); err != nil {
		t.Fatal(err)
	}
	total := func() int64 {
		t.Helper()
		rollup, err := store.ProxyRollup(ctx)
		if err != nil || len(rollup) != 1 {
			t.Fatalf("rollup: %+v %v", rollup, err)
		}
		return rollup[0].Requests
	}
	if got := total(); got != 3 {
		t.Fatalf("want the events of both unnamed logs and the named one, got %d", got)
	}
	midnight(t, store, day)

	want := []string{
		dayName(today-2) + parquetSuffix,
		dayName(today) + parquetSuffix,
		filepath.Base(store.requests.path(store.requests.ownKey(today), parquetSuffix)),
		filepath.Base(store.requests.path(store.requests.ownKey(today+1), logSuffix)),
	}
	slices.Sort(want)
	if got := dirNames(t, requests); !slices.Equal(got, want) {
		t.Fatalf("files after midnight: got %v, want %v", got, want)
	}
	if got := total(); got != 3 {
		t.Fatalf("conversion changed the count: got %d", got)
	}
}

func TestParquetClockStepBackDoesNotReopenFinishedDay(t *testing.T) {
	day := newFakeDay()
	store := newTestParquetStore(t, t.TempDir(), day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	midnight(t, store, day)
	day.day.Add(-1)
	if err := store.InsertRequest(ctx, request(200)); err != nil {
		t.Fatal(err)
	}
	if got := len(dirNames(t, store.requests.dir)); got != 2 {
		t.Fatalf("clock step created extra files: %v", dirNames(t, store.requests.dir))
	}
	if got := countToday(t, store); got != 2 {
		t.Fatalf("want 2 events, got %d", got)
	}
}

func TestParquetRollupCacheFollowsFiles(t *testing.T) {
	day := newFakeDay()
	store := newTestParquetStore(t, t.TempDir(), day)
	ctx := context.Background()
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	midnight(t, store, day)
	if err := store.InsertRequest(ctx, request(300)); err != nil {
		t.Fatal(err)
	}

	// The second call reuses the cached finished day and must not double it.
	for range 2 {
		rollup, err := store.ProxyRollup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rollup) != 1 || rollup[0].Requests != 2 || rollup[0].AvgResponseTime != 200 {
			t.Fatalf("rollup: %+v", rollup)
		}
	}

	for range 3 {
		day.day.Add(1)
	}
	if err := store.ApplyRetention(ctx, RetentionConfig{RequestRetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	rollup, err := store.ProxyRollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rollup) != 0 {
		t.Fatalf("expired files still in the rollup: %+v", rollup)
	}
}

func TestParquetSyncWritesAppends(t *testing.T) {
	store, err := newParquetStore(&config.ParquetConfig{Dir: t.TempDir(), Instance: testInstance, SyncWrites: true}, logger.New("error"), nil, newFakeDay().get)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertRequest(context.Background(), request(100)); err != nil {
		t.Fatal(err)
	}
	if got := countToday(t, store); got != 1 {
		t.Fatalf("want 1 event, got %d", got)
	}
}

func TestParquetReportsWritesToObserver(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	observe := func(_ context.Context, kind string, count int, success bool) {
		mu.Lock()
		defer mu.Unlock()
		counts[fmt.Sprintf("%s/%v", kind, success)] += count
	}
	store, err := newParquetStore(&config.ParquetConfig{Dir: t.TempDir(), Instance: testInstance}, logger.New("error"), observe, newFakeDay().get)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ms := range []int{1, 2} {
		if err := store.InsertRequest(ctx, request(ms)); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	if err := store.InsertRequest(ctx, request(3)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("append after Close: %v", err)
	}
	if counts["request/true"] != 2 || counts["request/false"] != 1 {
		t.Fatalf("observed writes: %v", counts)
	}
}

// The race detector run of this test also covers appends racing scans,
// rotation and conversion.
func TestParquetConcurrentAppendsAndReads(t *testing.T) {
	day := newFakeDay()
	store := newTestParquetStore(t, t.TempDir(), day)
	ctx := context.Background()

	const writers, perWriter = 8, 500
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for i := range perWriter {
				event := request(i)
				event.URL = strings.Repeat("x", i%700)
				if err := store.InsertRequest(ctx, event); err != nil {
					t.Error(err)
					return
				}
				if i%100 == 0 {
					if _, err := store.TrafficSeries(ctx, "1h"); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Go(func() {
		for range 2 {
			time.Sleep(5 * time.Millisecond)
			next := day.day.Add(1)
			for _, table := range store.tables() {
				if err := table.rotate(next); err != nil {
					t.Error(err)
					return
				}
			}
			store.convertPending(ctx)
		}
	})
	wg.Wait()
	store.convertPending(ctx)
	if got := countToday(t, store); got != writers*perWriter {
		t.Fatalf("want %d events, got %d", writers*perWriter, got)
	}
}

func TestLatencyCountsMatchSortedPercentiles(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, size := range []int{1, 2, 4, 5, 1000, 20_000} {
		var counts, other latencyCounts
		values := make([]int32, size)
		for i := range values {
			values[i] = int32(r.Intn(30_000))
			if i%2 == 0 {
				counts.add(values[i])
			} else {
				other.add(values[i])
			}
		}
		counts.merge(&other)
		slices.Sort(values)
		for _, q := range []float64{0, 0.5, 0.95, 1} {
			position := q * float64(size-1)
			rank := int(position)
			want := float64(values[rank])
			if rank+1 < size {
				want += (position - float64(rank)) * float64(values[rank+1]-values[rank])
			}
			if got := counts.percentile(q); got != want {
				t.Errorf("size %d q %v: got %v, want %v", size, q, got, want)
			}
		}
	}
}
