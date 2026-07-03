package services

import (
	"context"
	"fmt"
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

// JobKind distinguishes the operations that share the job store.
type JobKind string

const (
	JobKindPoolHealthCheck JobKind = "pool_health_check"
	JobKindBulkTest        JobKind = "bulk_test"
)

// Backwards-compatible aliases for the original health-check-only names so the
// existing pool code keeps compiling unchanged.
type (
	HCJob       = Job
	HCJobStore  = JobStore
	HCJobStatus = JobStatus
)

const (
	HCJobPending = JobPending
	HCJobRunning = JobRunning
	HCJobDone    = JobDone
	HCJobFailed  = JobFailed
)

// Job holds state for one async, pollable operation (a pool health check, a
// bulk proxy test, …). Kind-specific fields are omitempty so unrelated jobs
// stay lean in the JSON the UI polls.
type Job struct {
	ID         string     `json:"id"`
	Kind       JobKind    `json:"kind"`
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

	// Pool-health-check-specific metadata.
	PoolID   int    `json:"pool_id,omitempty"`
	PoolName string `json:"pool_name,omitempty"`
	CheckURL string `json:"check_url,omitempty"`
	Workers  int    `json:"workers,omitempty"`
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

// add registers a job and schedules a background cleanup.
func (s *JobStore) add(job *Job) *Job {
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	// Cleanup old jobs in background
	go s.cleanup()
	return job
}

// Create registers a new pool health-check job and returns it.
func (s *JobStore) Create(poolID int, poolName, checkURL string, workers int) *Job {
	return s.add(&Job{
		ID:        uuid.New().String(),
		Kind:      JobKindPoolHealthCheck,
		PoolID:    poolID,
		PoolName:  poolName,
		CheckURL:  checkURL,
		Workers:   workers,
		Status:    JobPending,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
}

// CreateBulkTest registers a new bulk proxy-test job for `total` proxies.
func (s *JobStore) CreateBulkTest(total int) *Job {
	return s.add(&Job{
		ID:        uuid.New().String(),
		Kind:      JobKindBulkTest,
		Total:     total,
		Status:    JobPending,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
}

// Get returns a job by ID.
func (s *JobStore) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
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

// cleanup removes jobs older than 30 minutes.
func (s *JobStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, j := range s.jobs {
		if j.StartedAt.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
}

// LatestByKind returns the most recently started job of the given kind.
func (s *JobStore) LatestByKind(kind JobKind) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *Job
	for _, j := range s.jobs {
		if j.Kind != kind {
			continue
		}
		if latest == nil || j.StartedAt.After(latest.StartedAt) {
			latest = j
		}
	}
	return latest, latest != nil
}

// ListByPool returns all pool health-check jobs for a given pool (newest first).
func (s *JobStore) ListByPool(poolID int) []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Job
	for _, j := range s.jobs {
		if j.Kind == JobKindPoolHealthCheck && j.PoolID == poolID {
			out = append(out, j)
		}
	}
	// sort newest first
	for i := 0; i < len(out)-1; i++ {
		for k := i + 1; k < len(out); k++ {
			if out[k].StartedAt.After(out[i].StartedAt) {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	return out
}

// RunPoolHealthCheckAsync starts the health check in a goroutine and returns job_id immediately.
// It calls poolSvc.HealthCheckPoolWithProgress which updates the job store as proxies are checked.
func RunPoolHealthCheckAsync(
	ctx context.Context,
	poolSvc *PoolService,
	poolID int,
	poolName, checkURL string,
	workers int,
) (*Job, error) {
	store := GetJobStore()
	if poolName == "" {
		poolName = fmt.Sprintf("Pool #%d", poolID)
	}
	if workers <= 0 {
		workers = 20
	}

	// Get proxy count upfront so frontend can show progress %
	proxies, _ := poolSvc.poolRepo.GetProxies(ctx, poolID)

	job := store.Create(poolID, poolName, checkURL, workers)
	store.Update(job.ID, func(j *Job) {
		j.Total = len(proxies)
	})

	go func() {
		store.Update(job.ID, func(j *Job) {
			j.Status = JobRunning
		})

		result, err := poolSvc.HealthCheckPoolWithProgress(
			context.Background(), // use background so UI disconnect doesn't kill it
			poolID, checkURL, workers,
			func(checked, active, failed int) {
				store.Update(job.ID, func(j *Job) {
					j.Progress = checked
					j.Active = active
					j.Failed = failed
				})
			},
		)

		now := time.Now()
		if err != nil {
			store.Update(job.ID, func(j *Job) {
				j.Status = JobFailed
				j.Error = err.Error()
				j.FinishedAt = &now
			})
			return
		}

		store.Update(job.ID, func(j *Job) {
			j.Status = JobDone
			j.Total = result.Checked
			j.Active = result.Active
			j.Failed = result.Failed
			j.Progress = result.Checked
			j.Results = result.Results
			j.FinishedAt = &now
		})
	}()

	return job, nil
}
