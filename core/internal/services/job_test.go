package services

import (
	"testing"
	"time"
)

func newTestJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*Job)}
}

func (s *JobStore) put(id string, status JobStatus, startedAt time.Time) {
	s.jobs[id] = &Job{ID: id, Status: status, StartedAt: startedAt}
}

// A long-running bulk test must survive the TTL sweep, otherwise the dashboard
// loses track of it while it is still working.
func TestJobStore_CleanupKeepsInFlightJobs(t *testing.T) {
	s := newTestJobStore()
	stale := time.Now().Add(-time.Hour)

	s.put("pending", JobPending, stale)
	s.put("running", JobRunning, stale)
	s.put("done", JobDone, stale)
	s.put("failed", JobFailed, stale)

	s.cleanup()

	for _, id := range []string{"pending", "running"} {
		if _, ok := s.jobs[id]; !ok {
			t.Errorf("expected the %s job to survive cleanup", id)
		}
	}
	for _, id := range []string{"done", "failed"} {
		if _, ok := s.jobs[id]; ok {
			t.Errorf("expected the stale %s job to be evicted", id)
		}
	}
}
