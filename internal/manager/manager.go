package manager

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bucknercd/jobworker/internal/joblib"
	jobpb "github.com/bucknercd/jobworker/proto/gen/jobpb"
)

type Manager struct {
	mu   sync.RWMutex
	jobs map[string]*joblib.Job

	logger *log.Logger
}

func NewManager(logger *log.Logger) *Manager {
	return &Manager{
		jobs:   make(map[string]*joblib.Job),
		logger: logger,
	}
}

// StartJob: creates job, starts it, stores in map, and returns job id.
// NOTE: This currently uses UUID as job id. You can swap to your base36 sortable id later.
func (m *Manager) StartJob(ctx context.Context, req *jobpb.StartJobRequest) (*jobpb.StartJobResponse, error) {
	if req.GetExecutable() == "" {
		return nil, status.Error(codes.InvalidArgument, "executable required")
	}

	id := uuid.New().String()

	limits := translateLimits(req.GetLimits()) // TODO: upgrade later

	job, err := joblib.NewJob(id, req.GetExecutable(), req.GetArgs(), limits, m.logger)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create job: %v", err)
	}

	if err := job.Start(); err != nil {
		return nil, status.Errorf(codes.Internal, "start job: %v", err)
	}

	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()

	// Reap in background; keep job in map for now (you can add TTL cleanup later).
	go func() {
		<-job.Done()
		m.logger.Printf("job %s done status=%s exit=%d", id, job.Status(), job.ExitCode())
	}()

	return &jobpb.StartJobResponse{JobId: id}, nil
}

func (m *Manager) StopJob(ctx context.Context, req *jobpb.StopJobRequest) (*jobpb.StopJobResponse, error) {
	job := m.getJob(req.GetJobId())
	if job == nil {
		return nil, status.Error(codes.NotFound, "job not found")
	}

	if err := job.Stop(); err != nil {
		return nil, status.Errorf(codes.Internal, "stop job: %v", err)
	}

	return &jobpb.StopJobResponse{
		Metadata: &jobpb.JobMetadata{
			User:     "", // filled by server auth layer later
			Status:   mapStatus(job.Status()),
			ExitCode: job.ExitCode(),
		},
	}, nil
}

func (m *Manager) GetStatus(ctx context.Context, req *jobpb.GetStatusRequest) (*jobpb.GetStatusResponse, error) {
	job := m.getJob(req.GetJobId())
	if job == nil {
		return nil, status.Error(codes.NotFound, "job not found")
	}

	return &jobpb.GetStatusResponse{
		JobId: req.GetJobId(),
		Metadata: &jobpb.JobMetadata{
			User:     "", // filled by server auth layer later
			Status:   mapStatus(job.Status()),
			ExitCode: job.ExitCode(),
		},
	}, nil
}

func (m *Manager) getJob(id string) *joblib.Job {
	if id == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}

func translateLimits(l *jobpb.ResourceLimits) []string {
	if l == nil {
		return nil
	}

	// Minimal translation for now:
	// You probably want to parse cpu/mem strings into cpu.max/memory.max eventually.
	// For now, return empty and rely on defaults (or hardcode defaults inside joblib/cgroups).
	return nil
}

// mapStatus maps internal joblib.Status -> proto JobStatus
func mapStatus(s joblib.Status) jobpb.JobStatus {
	switch s {
	case joblib.StatusRunning:
		return jobpb.JobStatus_JOB_STATUS_RUNNING
	case joblib.StatusExited:
		return jobpb.JobStatus_JOB_STATUS_EXITED
	case joblib.StatusStopped:
		return jobpb.JobStatus_JOB_STATUS_STOPPED
	case joblib.StatusFailed:
		return jobpb.JobStatus_JOB_STATUS_FAILED
	default:
		return jobpb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

func (m *Manager) DebugDump() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fmt.Sprintf("jobs=%d", len(m.jobs))
}
