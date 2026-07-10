package services

import (
	"testing"
	"time"
)

func newTestJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*Job)}
}

func (s *JobStore) put(id string, status JobStatus, startedAt time.Time) {
	s.jobs[id] = &Job{ID: id, Kind: JobKindPoolHealthCheck, Status: status, StartedAt: startedAt}
}

// A long-running health check must survive the TTL sweep, otherwise its status
// endpoint starts reporting "not found" while it is still working.
func TestJobStore_CleanupKeepsInFlightJobs(t *testing.T) {
	s := newTestJobStore()
	stale := time.Now().Add(-time.Hour)

	s.put("pending", JobPending, stale)
	s.put("running", JobRunning, stale)
	s.put("done", JobDone, stale)
	s.put("failed", JobFailed, stale)

	s.cleanup()

	for _, id := range []string{"pending", "running"} {
		if _, ok := s.Get(id); !ok {
			t.Errorf("expected the %s job to survive cleanup", id)
		}
	}
	for _, id := range []string{"done", "failed"} {
		if _, ok := s.Get(id); ok {
			t.Errorf("expected the stale %s job to be evicted", id)
		}
	}
}

func TestJobStore_CleanupKeepsRecentFinishedJobs(t *testing.T) {
	s := newTestJobStore()
	s.put("recent", JobDone, time.Now())

	s.cleanup()

	if _, ok := s.Get("recent"); !ok {
		t.Fatal("expected a recently finished job to be retained")
	}
}

func TestJobStore_ListByPoolIsNewestFirst(t *testing.T) {
	s := newTestJobStore()
	now := time.Now()
	s.jobs["old"] = &Job{ID: "old", Kind: JobKindPoolHealthCheck, PoolID: 1, StartedAt: now.Add(-2 * time.Hour)}
	s.jobs["new"] = &Job{ID: "new", Kind: JobKindPoolHealthCheck, PoolID: 1, StartedAt: now}
	s.jobs["mid"] = &Job{ID: "mid", Kind: JobKindPoolHealthCheck, PoolID: 1, StartedAt: now.Add(-time.Hour)}
	// A job in another pool must not appear.
	s.jobs["other"] = &Job{ID: "other", Kind: JobKindPoolHealthCheck, PoolID: 2, StartedAt: now}

	got := s.ListByPool(1)

	want := []string{"new", "mid", "old"}
	if len(got) != len(want) {
		t.Fatalf("expected %d jobs for pool 1, got %d", len(want), len(got))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d: expected %q, got %q", i, id, got[i].ID)
		}
	}
}
