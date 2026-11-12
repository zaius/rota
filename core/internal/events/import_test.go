package events

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

// newImportBackend returns the Postgres test backend, skipping unless the
// conformance suite targets Postgres.
func newImportBackend(t *testing.T) *pgTestBackend {
	t.Helper()
	b, ok := newTestBackend(t).(*pgTestBackend)
	if !ok {
		t.Skip("the import reads Postgres")
	}
	return b
}

func importInto(t *testing.T, b *pgTestBackend, dir string, before time.Time) []ImportReport {
	t.Helper()
	var reports []ImportReport
	err := ImportPostgresHistory(context.Background(), b.db.Pool, dir, before, func(r ImportReport) {
		reports = append(reports, r)
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return reports
}

func TestIntegration_ImportPostgresHistoryIntoPastDays(t *testing.T) {
	b := newImportBackend(t)
	ctx := context.Background()
	proxyID := b.SeedProxy(t, "127.0.0.1:9301")
	midnightToday := time.Now().UTC().Truncate(24 * time.Hour)
	day := func(offset int) time.Time { return midnightToday.Add(time.Duration(offset) * 24 * time.Hour) }
	cutoff := day(-1).Add(12 * time.Hour)

	pgRequest := func(at time.Time, url string) {
		t.Helper()
		err := b.store.InsertRequest(ctx, RequestEvent{
			ProxyID: proxyID, ProxyAddress: "127.0.0.1:9301", PoolID: 4, Username: "alice", Method: "GET",
			URL: url, Domain: "example.com", StatusCode: 200, ResponseTime: 120, Success: true, Timestamp: at,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pgTunnel := func(opened time.Time, lifetime time.Duration, host string) {
		t.Helper()
		err := b.store.InsertTunnel(ctx, TunnelEvent{
			ProxyID: proxyID, ProxyAddress: "127.0.0.1:9301", Host: host, Domain: "example.com",
			BytesUp: 10, BytesDown: 20, DurationMs: int(lifetime.Milliseconds()), OpenedAt: opened,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pgRequest(day(-3).Add(time.Hour), "http://example.com/old-1")
	pgRequest(day(-3).Add(2*time.Hour), "http://example.com/old-2")
	pgRequest(day(-1).Add(2*time.Hour), "http://example.com/before-cutoff")
	// Recorded after the cutoff, so the files already hold it.
	pgRequest(day(-1).Add(13*time.Hour), "http://example.com/mirrored")
	pgTunnel(day(-2).Add(-10*time.Second), 20*time.Second, "crosses-midnight:443")
	pgTunnel(day(-3).Add(time.Hour), 49*time.Hour, "long-lived:443")

	// The files begin at the cutoff: yesterday is a finished Parquet file
	// and today is an open log.
	partition := newFakeDay()
	partition.day.Add(-1)
	dir := t.TempDir()
	store := newTestParquetStore(t, dir, partition)
	mirrored := RequestEvent{
		ProxyID: proxyID, ProxyAddress: "127.0.0.1:9301", PoolID: 4, Username: "alice", Method: "GET",
		URL: "http://example.com/mirrored", Domain: "example.com", StatusCode: 200, ResponseTime: 120,
		Success: true, Timestamp: day(-1).Add(13 * time.Hour),
	}
	if err := store.InsertRequest(ctx, mirrored); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTunnel(ctx, TunnelEvent{ProxyID: proxyID, ProxyAddress: "p", Host: "mirrored:443", OpenedAt: day(-1).Add(13 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	midnight(t, store, partition)
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	before, err := store.ProxyRollup(ctx)
	if err != nil {
		t.Fatal(err)
	}

	reports := importInto(t, b, dir, cutoff)

	urls := []string{}
	for _, row := range readAllRequests(t, store) {
		urls = append(urls, row.URL)
	}
	slices.Sort(urls)
	want := []string{"http://example.com/", "http://example.com/before-cutoff", "http://example.com/mirrored",
		"http://example.com/old-1", "http://example.com/old-2"}
	if !slices.Equal(urls, want) {
		t.Fatalf("requests after import:\n got %v\nwant %v", urls, want)
	}

	hostsByDay := map[string][]string{}
	tunnelFiles, err := store.tunnels.openFiles(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tunnelFiles {
		if !f.parquet {
			continue
		}
		reader := parquet.NewGenericReader[tunnelRow](io.NewSectionReader(f.file, 0, f.size))
		rows := make([]tunnelRow, reader.NumRows())
		reader.Read(rows)
		reader.Close()
		for _, row := range rows {
			hostsByDay[filepath.Base(f.path)] = append(hostsByDay[filepath.Base(f.path)], row.Host)
		}
	}
	closeEventFiles(tunnelFiles)
	if got := hostsByDay[dayName(dayOf(day(-2).UnixMilli()))+parquetSuffix]; !slices.Equal(got, []string{"crosses-midnight:443"}) {
		t.Errorf("a tunnel belongs to the day it closed: %v", hostsByDay)
	}
	if got := hostsByDay[dayName(dayOf(day(-1).UnixMilli()))+parquetSuffix]; !slices.Equal(got, []string{"long-lived:443", "mirrored:443"}) {
		t.Errorf("yesterday must merge the imported tunnel before its own: %v", hostsByDay)
	}

	after, err := store.ProxyRollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requestsOf := func(stats []ProxyRequestStats) int64 {
		for _, st := range stats {
			if st.ProxyID == proxyID {
				return st.Requests
			}
		}
		return 0
	}
	if got, want := requestsOf(after), requestsOf(before)+3; got != want {
		t.Fatalf("the rollup must pick up imported days, even rewritten ones: got %d requests, want %d", got, want)
	}

	actions := map[string]int{}
	for _, r := range reports {
		actions[r.Table+"/"+r.Action]++
	}
	if actions["requests/new"] != 1 || actions["requests/merged"] != 1 || actions["tunnels/new"] != 1 || actions["tunnels/merged"] != 1 {
		t.Errorf("reports: %+v", reports)
	}

	// A second run finds every day done and changes nothing.
	for _, r := range importInto(t, b, dir, time.Time{}) {
		if r.Action != "skipped" {
			t.Errorf("rerun repeated a day: %+v", r)
		}
	}
	if got := len(readAllRequests(t, store)); got != 5 {
		t.Fatalf("rerun changed the history: %d requests", got)
	}
	if err := ImportPostgresHistory(ctx, b.db.Pool, dir, cutoff.Add(time.Hour), nil); err == nil || !strings.Contains(err.Error(), "earlier import") {
		t.Fatalf("a different cutoff must be refused: %v", err)
	}
}

func TestIntegration_ImportPostgresHistoryIntoToday(t *testing.T) {
	b := newImportBackend(t)
	now := time.Now().UTC()
	if now.Sub(now.Truncate(24*time.Hour)) < 10*time.Minute {
		t.Skip("too close to midnight for events earlier today")
	}
	ctx := context.Background()
	proxyID := b.SeedProxy(t, "127.0.0.1:9302")
	if err := b.store.InsertRequest(ctx, RequestEvent{
		ProxyID: proxyID, ProxyAddress: "127.0.0.1:9302", Method: "GET", URL: "http://example.com/earlier-today",
		Success: true, ResponseTime: 50, Timestamp: now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store := newTestParquetStore(t, dir, newFakeDay())
	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}

	// No cutoff: the import stops where the files begin.
	reports := importInto(t, b, dir, time.Time{})
	if len(reports) != 1 || reports[0].Action != "appended" || reports[0].Rows != 1 {
		t.Fatalf("reports: %+v", reports)
	}
	if got := countToday(t, store); got != 2 {
		t.Fatalf("today's log must hold the imported request too: %d", got)
	}
	names, err := os.ReadDir(store.requests.dir)
	if err != nil || len(names) != 1 {
		t.Fatalf("the import must add to today's log, not a new file: %v %v", names, err)
	}
}
