package events

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/zaius/rota/core/internal/config"
	"github.com/zaius/rota/core/internal/models"
	"github.com/zaius/rota/core/pkg/logger"
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
	return newTestParquetStoreAs(t, dir, day, testInstance, sweepRows)
}

func newTestParquetStoreAs(t *testing.T, dir string, day *fakeDay, instance string, sweepRows int64) *ParquetStore {
	t.Helper()
	store, err := newParquetStore(&config.ParquetConfig{Dir: dir, Instance: instance}, logger.New("error"), nil, day.get, sweepRows)
	if err != nil {
		t.Fatalf("open parquet store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// midnight moves the partition day forward and finishes the day it leaves
// before returning.
func midnight(t *testing.T, store *ParquetStore, day *fakeDay) {
	t.Helper()
	next := day.day.Add(1)
	for _, table := range store.tables() {
		if err := table.rotate(next); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	store.sweepPending(context.Background())
}

// preparedStore runs prepare before every read, so testStores can move the
// events into the kind of file it wants to read them from.
type preparedStore struct {
	*ParquetStore
	prepare func()
}

func (s preparedStore) RequestStats(ctx context.Context) (*RequestStats, error) {
	s.prepare()
	return s.ParquetStore.RequestStats(ctx)
}

func (s preparedStore) ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error) {
	s.prepare()
	return s.ParquetStore.ProxyRollup(ctx)
}

func (s preparedStore) LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error) {
	s.prepare()
	return s.ParquetStore.LowSuccessProxies(ctx, window, minRate, minRequests)
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
			err := forEachLine(context.Background(), io.NewSectionReader(f.file, f.start, f.size-f.start), func(line []byte) error {
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

func TestParquetMidnightFinishesDayAndKeepsAnswers(t *testing.T) {
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

	// The all-history views read the finished day from its summary.
	views := func() (day, all []models.DomainStats, traffic []models.TrafficPoint) {
		t.Helper()
		var errs [3]error
		day, errs[0] = store.DomainStats(ctx, "24h", "", 10)
		all, errs[1] = store.DomainStats(ctx, "all", "", 10)
		traffic, errs[2] = store.TrafficSeries(ctx, "all")
		if err := errors.Join(errs[:]...); err != nil {
			t.Fatal(err)
		}
		return day, all, traffic
	}
	before, beforeAll, beforeTraffic := views()
	closedDay := day.get()
	midnight(t, store, day)

	got := dirNames(t, store.requests.dir)
	finished := store.requests.ownKey(closedDay)
	want := []string{filepath.Base(store.requests.path(finished, parquetSuffix)), filepath.Base(store.requests.path(finished, summarySuffix)),
		filepath.Base(store.requests.path(store.requests.ownKey(day.get()), logSuffix))}
	if !slices.Equal(got, want) {
		t.Fatalf("files after midnight: got %v, want %v", got, want)
	}
	after, afterAll, afterTraffic := views()
	if !slices.Equal(before, after) || !slices.Equal(beforeAll, afterAll) || !slices.Equal(beforeTraffic, afterTraffic) {
		t.Fatalf("finishing the day changed answers:\nbefore %+v %+v %+v\nafter  %+v %+v %+v", before, beforeAll, beforeTraffic, after, afterAll, afterTraffic)
	}
	if len(after) != 1 || after[0].Requests != 3 || after[0].AvgResponseTime != 200 || after[0].TunnelErrors != 1 {
		t.Fatalf("domain stats: %+v", after)
	}

	rows := readAllRequests(t, store)
	if len(rows) != 3 || rows[0].URL != "http://example.com/" || rows[0].Username != "alice" {
		t.Fatalf("parquet lost fields: %+v", rows)
	}
}

func TestParquetResumesAfterInterruptedSweep(t *testing.T) {
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
	reopened.sweepPending(ctx)
	names := dirNames(t, requests)
	if slices.Contains(names, filepath.Base(log)) || slices.Contains(names, filepath.Base(temp)) {
		t.Fatalf("startup left interrupted files behind: %v", names)
	}
}

func ptr[T any](v T) *T { return &v }

// Sweeps move the open day's log into its Parquet file while appends continue.
// Every event must count once, before and after a restart and in the proxy
// rollup, and the finished day must be one Parquet file holding every field.
func TestParquetSweepsOpenDayWithoutLosingOrRepeatingEvents(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	ctx := context.Background()
	insert := func(store *ParquetStore, n int) {
		t.Helper()
		for range n {
			if err := store.InsertRequest(ctx, request(100)); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func(store *ParquetStore, want int64) {
		t.Helper()
		rollup, err := store.ProxyRollup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := countToday(t, store); got != want || len(rollup) != 1 || rollup[0].Requests != want {
			t.Fatalf("want %d events, got %d and rollup %+v", want, got, rollup)
		}
	}

	store := newTestParquetStoreAs(t, dir, day, testInstance, 2)
	insert(store, 3)
	store.sweepPending(ctx)
	insert(store, 1)
	check(store, 4)
	store.Close()

	reopened := newTestParquetStoreAs(t, dir, day, testInstance, 2)
	check(reopened, 4)
	insert(reopened, 1)
	reopened.sweepPending(ctx)
	check(reopened, 5)

	closedDay := day.get()
	midnight(t, reopened, day)
	got := dirNames(t, reopened.requests.dir)
	finished := reopened.requests.ownKey(closedDay)
	want := []string{filepath.Base(reopened.requests.path(finished, parquetSuffix)), filepath.Base(reopened.requests.path(finished, summarySuffix)),
		filepath.Base(reopened.requests.path(reopened.requests.ownKey(day.get()), logSuffix))}
	if !slices.Equal(got, want) {
		t.Fatalf("files after midnight: got %v, want %v", got, want)
	}
	if rows := readAllRequests(t, reopened); len(rows) != 5 || rows[4].URL != "http://example.com/" {
		t.Fatalf("finished day: %+v", rows)
	}
}

func TestParquetReadsEveryInstanceAndSweepsOnlyItsOwn(t *testing.T) {
	dir := t.TempDir()
	day := newFakeDay()
	a := newTestParquetStoreAs(t, dir, day, "a", sweepRows)
	b := newTestParquetStoreAs(t, dir, day, "b", sweepRows)
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
	check(name(today, "a", parquetSuffix), name(today, "a", summarySuffix), name(today+1, "a", logSuffix), name(today, "b", logSuffix))

	for _, table := range b.tables() {
		if err := table.rotate(day.get()); err != nil {
			t.Fatal(err)
		}
	}
	b.sweepPending(ctx)
	check(name(today, "a", parquetSuffix), name(today, "a", summarySuffix), name(today+1, "a", logSuffix),
		name(today, "b", parquetSuffix), name(today, "b", summarySuffix), name(today+1, "b", logSuffix))
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

// The race detector run of this test also covers appends racing scans,
// rotation and sweeps.
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
			store.sweepPending(ctx)
		}
	})
	wg.Wait()
	store.sweepPending(ctx)
	if got := countToday(t, store); got != writers*perWriter {
		t.Fatalf("want %d events, got %d", writers*perWriter, got)
	}
}
