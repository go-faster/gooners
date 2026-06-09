package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

type Manager struct {
	ctx    context.Context
	client *Client
	jobs   map[string]*Job
	mu     sync.Mutex
	logger *slog.Logger
}

func NewManager(ctx context.Context, client *Client, logger *slog.Logger) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		ctx:    ctx,
		client: client,
		jobs:   make(map[string]*Job),
		logger: logger,
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
		return
	}
	job.Status = JobDone
	m.logger.Debug("opencode handoff job completed", "session_id", job.SessionID)
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
