package events

import (
	"context"
	"testing"
	"time"
)

func TestIntegration_DomainStats(t *testing.T) {
	b := newTestBackend(t)
	s := b.Store()
	ctx := context.Background()
	rows, err := s.DomainStats(ctx, "24h", "", 100)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty domain stats: %v %v", rows, err)
	}
	id := b.SeedProxy(t, "127.0.0.1:9989")
	now := time.Now()
	for _, event := range []RequestEvent{
		{Domain: "foo.com", Success: true, ResponseTime: 100, StatusCode: 200},
		{Domain: "foo.com", Success: true, ResponseTime: 300, StatusCode: 200},
		{Domain: "foo.com", Success: false, ResponseTime: 9000, StatusCode: 429},
		{Domain: "api.foo.com", Success: false, ResponseTime: 9000, StatusCode: 503},
		{Domain: "notfoo.com", Success: true, ResponseTime: 50},
		{Domain: "old.com", Success: true, Timestamp: now.Add(-48 * time.Hour)},
		{Domain: "", Success: true},
	} {
		event.ProxyID, event.ProxyAddress, event.Method, event.URL = id, "127.0.0.1:9989", "GET", "http://"+event.Domain
		if event.Timestamp.IsZero() {
			event.Timestamp = now
		}
		if err := s.InsertRequest(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []TunnelEvent{
		{Domain: "foo.com", BytesUp: 10, BytesDown: 20, Requests: 2, OpenedAt: now.Add(-time.Minute), DurationMs: 30_000},
		// Opened outside the window, closed inside. Must still be counted.
		{Domain: "tunnel.com", BytesUp: 30, BytesDown: 40, Error: "reset", OpenedAt: now.Add(-25 * time.Hour), DurationMs: int((2 * time.Hour).Milliseconds())},
		{Domain: "old.com", OpenedAt: now.Add(-48 * time.Hour)},
		{Domain: "", OpenedAt: now},
	} {
		event.ProxyID, event.ProxyAddress, event.Host = id, "127.0.0.1:9989", event.Domain+":443"
		if err := s.InsertTunnel(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = s.DomainStats(ctx, "24h", "", 100)
	if err != nil || len(rows) != 4 {
		t.Fatalf("domain stats: %v %v", rows, err)
	}
	foo := rows[0]
	if foo.Domain != "foo.com" || foo.Requests != 3 || foo.Successes != 2 || foo.Failures != 1 || foo.RateLimited != 1 ||
		foo.AvgResponseTime != 200 || foo.P50Ms != 200 || foo.P95Ms != 290 || foo.Tunnels != 1 || foo.BytesUp != 10 || foo.BytesDown != 20 {
		t.Fatalf("incorrect aggregate or double-counted tunnel requests: %+v", foo)
	}
	if row := rows[1]; row.Domain != "api.foo.com" || row.Failures != 1 || row.AvgResponseTime != 0 || row.P95Ms != 0 {
		t.Fatalf("failure-only domain or tie ordering: %+v", row)
	}
	if row := rows[3]; row.Domain != "tunnel.com" || row.Tunnels != 1 || row.TunnelErrors != 1 || row.Requests != 0 || row.BytesUp != 30 || row.BytesDown != 40 {
		t.Fatalf("tunnel-only domain: %+v", row)
	}
	rows, err = s.DomainStats(ctx, "24h", "foo.com", 100)
	if err != nil || len(rows) != 2 || rows[0].Domain != "foo.com" || rows[1].Domain != "api.foo.com" {
		t.Fatalf("domain boundary filtering: %v %v", rows, err)
	}
	rows, err = s.DomainStats(ctx, "24h", "", 1)
	if err != nil || len(rows) != 1 || rows[0].Domain != "foo.com" {
		t.Fatalf("limit: %v %v", rows, err)
	}
	rows, err = s.DomainStats(ctx, "7d", "old.com", 100)
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].Tunnels != 1 {
		t.Fatalf("range: %v %v", rows, err)
	}
	rows, err = s.DomainStats(ctx, "24h", "missing.com", 100)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("unmatched filter: %v %v", rows, err)
	}
}
