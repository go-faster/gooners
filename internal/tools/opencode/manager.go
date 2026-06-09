package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type JobStatus string

const (
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobError   JobStatus = "error"
)

type Job struct {
	SessionID    string          `json:"session_id"`
	Status       JobStatus       `json:"status"`
	PromptResult json.RawMessage `json:"prompt_result,omitempty"`
	Err          error           `json:"-"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	mu           sync.Mutex
}

// jobRecord is the on-disk representation of a Job. Err is stored as a string
// because the error interface is not JSON-serializable.
type jobRecord struct {
	SessionID    string          `json:"session_id"`
	Status       JobStatus       `json:"status"`
	PromptResult json.RawMessage `json:"prompt_result,omitempty"`
	ErrMessage   string          `json:"err_message,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type Manager struct {
	ctx      context.Context
	client   *Client
	jobs     map[string]*Job
	mu       sync.Mutex
	logger   *slog.Logger
	stateDir string
}

func NewManager(ctx context.Context, client *Client, logger *slog.Logger) *Manager {
	return NewManagerWithStateDir(ctx, client, logger, "")
}

// NewManagerWithStateDir creates a Manager that persists job state to stateDir.
// An empty stateDir disables persistence (suitable for tests).
func NewManagerWithStateDir(ctx context.Context, client *Client, logger *slog.Logger, stateDir string) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		ctx:      ctx,
		client:   client,
		jobs:     make(map[string]*Job),
		logger:   logger,
		stateDir: stateDir,
	}
	if stateDir != "" {
		m.loadJobs()
	}
	return m
}

// loadJobs reads persisted job records from stateDir on startup.
// Jobs found in "running" state are marked as error: the server crashed before
// they could finish, so we don't know the outcome.
func (m *Manager) loadJobs() {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Warn("failed to read state dir", "dir", m.stateDir, "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(m.stateDir, e.Name())
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from operator-controlled state dir + filename from ReadDir
		if err != nil {
			m.logger.Warn("failed to read job file", "path", path, "err", err)
			continue
		}
		var rec jobRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			m.logger.Warn("failed to parse job file", "path", path, "err", err)
			continue
		}
		job := &Job{
			SessionID:    rec.SessionID,
			Status:       rec.Status,
			PromptResult: rec.PromptResult,
			CreatedAt:    rec.CreatedAt,
			UpdatedAt:    rec.UpdatedAt,
		}
		if rec.ErrMessage != "" {
			job.Err = errors.New(rec.ErrMessage)
		}
		// A job persisted as "running" means the server crashed before it finished.
		if job.Status == JobRunning {
			job.Status = JobError
			job.Err = errors.New("server restarted before job completed")
			job.UpdatedAt = time.Now()
		}
		m.jobs[rec.SessionID] = job
		m.logger.Debug("loaded persisted job", "session_id", rec.SessionID, "status", job.Status)
	}
}

func (m *Manager) saveJob(job *Job) {
	if m.stateDir == "" {
		return
	}
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		m.logger.Warn("failed to create state dir", "dir", m.stateDir, "err", err)
		return
	}
	rec := jobRecord{
		SessionID:    job.SessionID,
		Status:       job.Status,
		PromptResult: job.PromptResult,
		CreatedAt:    job.CreatedAt,
		UpdatedAt:    job.UpdatedAt,
	}
	if job.Err != nil {
		rec.ErrMessage = job.Err.Error()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		m.logger.Warn("failed to marshal job", "session_id", job.SessionID, "err", err)
		return
	}
	path := filepath.Join(m.stateDir, job.SessionID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		m.logger.Warn("failed to write job file", "path", path, "err", err)
	}
}

func (m *Manager) Submit(ctx context.Context, loc Location, sessionID string, createReq CreateSessionRequest, req PromptRequest) (string, error) {
	if req.Prompt.Text == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if sessionID == "" {
		session, err := m.client.CreateSession(ctx, loc, createReq)
		if err != nil {
			return "", err
		}
		sessionID = session.ID
	}

	now := time.Now()
	job := &Job{
		SessionID: sessionID,
		Status:    JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.mu.Lock()
	if existing, ok := m.jobs[sessionID]; ok {
		existing.mu.Lock()
		running := existing.Status == JobRunning
		existing.mu.Unlock()
		if running {
			m.mu.Unlock()
			return "", fmt.Errorf("session %q already has a running handoff job", sessionID)
		}
	}
	m.jobs[sessionID] = job
	m.mu.Unlock()

	m.saveJob(job)
	go m.run(job, loc, req)
	return sessionID, nil
}

func (m *Manager) Job(sessionID string) (*Job, bool) {
	m.mu.Lock()
	job, ok := m.jobs[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	return snapshotJob(job), true
}

func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()

	out := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, *snapshotJob(job))
	}
	return out
}

func (m *Manager) run(job *Job, loc Location, req PromptRequest) {
	res, err := m.client.Prompt(m.ctx, loc, job.SessionID, req)
	job.mu.Lock()
	defer job.mu.Unlock()
	job.PromptResult = append(json.RawMessage(nil), res...)
	job.UpdatedAt = time.Now()
	if err != nil {
		job.Status = JobError
		job.Err = err
		m.logger.Warn("opencode handoff job failed", "session_id", job.SessionID, "err", err)
	} else {
		job.Status = JobDone
		m.logger.Debug("opencode handoff job completed", "session_id", job.SessionID)
	}
	m.saveJob(job)
}

func snapshotJob(job *Job) *Job {
	job.mu.Lock()
	defer job.mu.Unlock()
	return &Job{
		SessionID:    job.SessionID,
		Status:       job.Status,
		PromptResult: append(json.RawMessage(nil), job.PromptResult...),
		Err:          job.Err,
		CreatedAt:    job.CreatedAt,
		UpdatedAt:    job.UpdatedAt,
	}
}
