package services

import (
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/google/uuid"
)

// JobStatus represents the state of an async, pollable job.
type JobStatus string

const (
	JobPending JobStatus = "pending"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
)

// Job holds the state of one bulk proxy test, which the UI polls.
type Job struct {
	ID         string     `json:"id"`
	Status     JobStatus  `json:"status"`
	Progress   int        `json:"progress"` // items processed so far
	Total      int        `json:"total"`    // total items to process
	Active     int        `json:"active"`
	Failed     int        `json:"failed"`
	Skipped    int        `json:"skipped,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Full results (populated when done)
	Results []models.ProxyTestResult `json:"results,omitempty"`
}

// JobStore keeps an in-memory map of recent jobs (TTL 30 min).
type JobStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

var globalJobStore = &JobStore{
	jobs: make(map[string]*Job),
}

// GetJobStore returns the singleton job store.
func GetJobStore() *JobStore {
	return globalJobStore
}

// CreateBulkTest registers a new bulk proxy-test job for `total` proxies.
func (s *JobStore) CreateBulkTest(total int) *Job {
	job := &Job{
		ID:        uuid.New().String(),
		Total:     total,
		Status:    JobPending,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	// Cleanup old jobs in background
	go s.cleanup()
	return job
}

// Update mutates a job (caller must hold no lock).
func (s *JobStore) Update(id string, fn func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		fn(j)
		j.UpdatedAt = time.Now()
	}
}

// cleanup removes finished jobs older than 30 minutes. A job that is still
// pending or running is kept however old it is: a test over many proxies can
// outlive the cutoff, and evicting it mid-flight would make its status
// endpoint report "not found" while the work is still going on.
func (s *JobStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, j := range s.jobs {
		if j.Status == JobPending || j.Status == JobRunning {
			continue
		}
		if j.StartedAt.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
}

// Latest returns the most recently started job.
func (s *JobStore) Latest() (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *Job
	for _, j := range s.jobs {
		if latest == nil || j.StartedAt.After(latest.StartedAt) {
			latest = j
		}
	}
	return latest, latest != nil
}
