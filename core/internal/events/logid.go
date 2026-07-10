package events

import (
	"sync/atomic"
	"time"
)

// lastLogID holds the most recently issued log ID.
var lastLogID atomic.Int64

// nextLogID returns an application-generated, monotonically increasing log ID.
//
// IDs are UnixNano timestamps bumped past the previous ID on collision, so
// they are unique and strictly increasing within a process, keep increasing
// across restarts, and sort after any IDs an old BIGSERIAL default produced —
// the LogsSince streaming cursor works unchanged. Generating IDs in the
// application (instead of a database sequence) keeps the Store interface
// implementable by backends without sequences, e.g. ClickHouse.
//
// Two rota instances sharing a database could in principle issue the same ID;
// logs have no uniqueness constraint, so the only effect is that a streaming
// cursor may skip one of the two colliding rows.
func nextLogID() int64 {
	for {
		id := time.Now().UnixNano()
		last := lastLogID.Load()
		if id <= last {
			id = last + 1
		}
		if lastLogID.CompareAndSwap(last, id) {
			return id
		}
	}
}
