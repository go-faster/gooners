package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// TransferStatus is the lifecycle state of a [TransferJob].
type TransferStatus string

const (
	TransferRunning   TransferStatus = "running"
	TransferCompleted TransferStatus = "completed"
	TransferFailed    TransferStatus = "failed"
	TransferCanceled  TransferStatus = "canceled"
)

var (
	// ErrTransferCanceled is the cancellation cause when a transfer is canceled explicitly.
	ErrTransferCanceled = errors.New("transfer canceled")
	// ErrSessionClosed is the cancellation cause when the owning SSH session goes away.
	ErrSessionClosed = errors.New("ssh session closed")
)

// TransferJob tracks a single upload or download.
//
// A job outlives its session: the pool keeps it addressable after the session is
// closed or the connection drops, so a caller can still observe the terminal state
// instead of getting "session not found".
type TransferJob struct {
	ID         string
	SessionID  string
	LocalPath  string
	RemotePath string
	StartedAt  time.Time

	// mu guards every field below.
	mu           sync.Mutex
	TotalBytes   int64
	Bytes        int64
	FinishedAt   time.Time
	LastStatusAt time.Time
	LastStatus   int64
	Status       TransferStatus
	Err          error
	Done         bool

	cancel  context.CancelCauseFunc
	closer  io.Closer
	aborted bool
	done    chan struct{}
}

// newTransferJob creates a job and the context its transfer goroutine must run under.
func newTransferJob(parent context.Context, id, sessionID, localPath, remotePath string) (*TransferJob, context.Context) {
	ctx, cancel := context.WithCancelCause(parent)
	return &TransferJob{
		ID:         id,
		SessionID:  sessionID,
		LocalPath:  localPath,
		RemotePath: remotePath,
		StartedAt:  time.Now(),
		Status:     TransferRunning,
		cancel:     cancel,
		done:       make(chan struct{}),
	}, ctx
}

func (j *TransferJob) setTotal(n int64) {
	j.mu.Lock()
	j.TotalBytes = n
	j.mu.Unlock()
}

func (j *TransferJob) add(n int64) {
	j.mu.Lock()
	j.Bytes += n
	j.mu.Unlock()
}

// setCloser hands the job the resource whose Close aborts the in-flight transfer,
// i.e. the SFTP client. It reports false if the job was already aborted, in which
// case the caller owns c and must close it.
func (j *TransferJob) setCloser(c io.Closer) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.aborted {
		return false
	}
	j.closer = c
	return true
}

// abortCause records cause as the reason the transfer is ending and returns the SFTP
// client that has to be closed to unblock it. The first cause wins: a later
// session-level abort does not relabel a job the caller explicitly canceled.
//
// Canceling the context alone is not enough. On a stalled connection the transfer sits
// in the SFTP write path, which never consults the context; only failing the in-flight
// requests gets it moving. Closing can itself block on that same stalled connection, so
// abortCause hands the closer back instead of closing it — the caller keeps that off the
// pool event loop. Recording the cause never blocks.
func (j *TransferJob) abortCause(cause error) io.Closer {
	j.mu.Lock()
	if j.aborted {
		j.mu.Unlock()
		return nil
	}
	j.aborted = true
	closer := j.closer
	j.closer = nil
	j.mu.Unlock()

	j.cancel(cause)
	return closer
}

// finish records the terminal state and unblocks waiters. It runs exactly once, from
// the transfer goroutine.
func (j *TransferJob) finish(ctx context.Context, err error) {
	if err != nil {
		// A transfer torn down mid-flight fails with an opaque "connection lost" from
		// deep in the SFTP stack. The cancellation cause says why it really stopped.
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
	}

	j.mu.Lock()
	j.FinishedAt = time.Now()
	j.Done = true
	j.closer = nil
	switch {
	case err == nil:
		j.Status = TransferCompleted
	case errors.Is(err, ErrTransferCanceled):
		j.Status = TransferCanceled
		j.Err = err
	default:
		j.Status = TransferFailed
		j.Err = err
	}
	j.mu.Unlock()

	j.cancel(nil)
	close(j.done)
}

func (j *TransferJob) terminal() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// finishedAt reports when the job reached a terminal state, or the zero time if it is
// still running.
func (j *TransferJob) finishedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.Done {
		return time.Time{}
	}
	return j.FinishedAt
}

// progressReader counts bytes pulled from the source and aborts the read when the job's
// context is canceled. It only covers the read side; see [TransferJob.abort] for the
// write side.
type progressReader struct {
	r   io.Reader
	ctx context.Context
	job *TransferJob
}

func (pr *progressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, context.Cause(pr.ctx)
	}
	n, err := pr.r.Read(p)
	pr.job.add(int64(n))
	return n, err
}
