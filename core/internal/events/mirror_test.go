package events

import (
	"context"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

func newTestMirror(t *testing.T) (*MirrorStore, *ParquetStore, *ParquetStore, *fakeDay) {
	t.Helper()
	day := newFakeDay()
	primary := newTestParquetStore(t, t.TempDir(), day)
	mirror := newTestParquetStore(t, t.TempDir(), day)
	return NewMirrorStore(primary, mirror, logger.New("error")), primary, mirror, day
}

func tunnelsToday(t *testing.T, store Store) int64 {
	t.Helper()
	stats, err := store.TunnelStats(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return stats.Tunnels
}

func TestMirrorWritesToBothStores(t *testing.T) {
	store, primary, mirror, _ := newTestMirror(t)
	ctx := context.Background()
	tunnel := TunnelEvent{ProxyID: 1, ProxyAddress: "p", Host: "a.example:443", OpenedAt: time.Now()}

	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRequests(ctx, []RequestEvent{request(200), request(300)}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTunnel(ctx, tunnel); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTunnels(ctx, []TunnelEvent{tunnel, tunnel}); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]*ParquetStore{"primary": primary, "mirror": mirror} {
		if got := countToday(t, target); got != 3 {
			t.Errorf("%s requests: want 3, got %d", name, got)
		}
		if got := tunnelsToday(t, target); got != 3 {
			t.Errorf("%s tunnels: want 3, got %d", name, got)
		}
	}
}

func TestMirrorReadsOnlyFromPrimary(t *testing.T) {
	store, _, mirror, _ := newTestMirror(t)
	ctx := context.Background()
	if err := mirror.InsertRequest(ctx, request(100)); err != nil {
		t.Fatal(err)
	}
	stats, err := store.RequestStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RequestsToday != 0 {
		t.Fatalf("read reached the mirror: %+v", stats)
	}
}

func TestMirrorFailureLeavesPrimaryWriteIntact(t *testing.T) {
	store, primary, mirror, _ := newTestMirror(t)
	ctx := context.Background()
	mirror.Close()

	if err := store.InsertRequest(ctx, request(100)); err != nil {
		t.Fatalf("a failing mirror must not fail the write: %v", err)
	}
	if got := countToday(t, primary); got != 1 {
		t.Fatalf("primary lost the event: got %d", got)
	}
}

func TestMirrorPrimaryFailureIsReported(t *testing.T) {
	store, primary, mirror, _ := newTestMirror(t)
	ctx := context.Background()
	primary.Close()

	if err := store.InsertRequest(ctx, request(100)); err == nil {
		t.Fatal("a failing primary must fail the write")
	}
	if got := countToday(t, mirror); got != 1 {
		t.Fatalf("mirror lost the event: got %d", got)
	}
}

func TestMirrorAppliesRetentionToBothStores(t *testing.T) {
	store, primary, mirror, day := newTestMirror(t)
	ctx := context.Background()
	for _, target := range []*ParquetStore{primary, mirror} {
		if err := target.InsertRequest(ctx, request(100)); err != nil {
			t.Fatal(err)
		}
		midnight(t, target, day)
		day.day.Add(-1)
	}
	for range 5 {
		day.day.Add(1)
	}
	if err := store.ApplyRetention(ctx, RetentionConfig{RequestRetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]*ParquetStore{"primary": primary, "mirror": mirror} {
		rollup, err := target.ProxyRollup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rollup) != 0 {
			t.Errorf("%s kept expired files: %+v", name, rollup)
		}
	}
}
