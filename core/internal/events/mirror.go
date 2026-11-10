package events

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// mirrorWarnInterval limits failure logging, because a broken mirror fails
// every event.
const mirrorWarnInterval = time.Minute

// MirrorStore answers reads from its primary store and sends every write to a
// second store too, so a deployment can move between backends with both
// receiving the same events. The primary alone decides whether a write
// succeeded: a failing mirror costs the mirror its copy and nothing else.
type MirrorStore struct {
	Store
	mirror      Store
	logger      *logger.Logger
	lastWarning atomic.Int64
}

var _ Store = (*MirrorStore)(nil)

// NewMirrorStore returns a store that reads from primary and writes to both.
func NewMirrorStore(primary, mirror Store, log *logger.Logger) *MirrorStore {
	return &MirrorStore{Store: primary, mirror: mirror, logger: log}
}

func (s *MirrorStore) warn(operation string, err error) {
	if err == nil {
		return
	}
	now := time.Now().UnixNano()
	last := s.lastWarning.Load()
	if now-last < int64(mirrorWarnInterval) || !s.lastWarning.CompareAndSwap(last, now) {
		return
	}
	s.logger.Warn("event mirror failed", "operation", operation, "error", err)
}

// InsertRequest records one proxied request outcome in both stores.
func (s *MirrorStore) InsertRequest(ctx context.Context, event RequestEvent) error {
	err := s.Store.InsertRequest(ctx, event)
	s.warn("insert request", s.mirror.InsertRequest(ctx, event))
	return err
}

// InsertRequests records a batch of request outcomes in both stores.
func (s *MirrorStore) InsertRequests(ctx context.Context, events []RequestEvent) error {
	err := s.Store.InsertRequests(ctx, events)
	s.warn("insert requests", s.mirror.InsertRequests(ctx, events))
	return err
}

// InsertTunnel records one completed CONNECT tunnel in both stores.
func (s *MirrorStore) InsertTunnel(ctx context.Context, event TunnelEvent) error {
	err := s.Store.InsertTunnel(ctx, event)
	s.warn("insert tunnel", s.mirror.InsertTunnel(ctx, event))
	return err
}

// InsertTunnels records a batch of completed tunnels in both stores.
func (s *MirrorStore) InsertTunnels(ctx context.Context, events []TunnelEvent) error {
	err := s.Store.InsertTunnels(ctx, events)
	s.warn("insert tunnels", s.mirror.InsertTunnels(ctx, events))
	return err
}

// ApplyRetention applies the retention configuration to both stores, so the
// mirror cannot grow without bound.
func (s *MirrorStore) ApplyRetention(ctx context.Context, cfg RetentionConfig) error {
	err := s.Store.ApplyRetention(ctx, cfg)
	s.warn("apply retention", s.mirror.ApplyRetention(ctx, cfg))
	return err
}
