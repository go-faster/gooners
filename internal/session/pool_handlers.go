package session

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const minTransferSampleInterval = 500 * time.Millisecond

// poolState is the event loop's private state. Every handler runs on the loop
// goroutine, so none of it needs locking.
type poolState struct {
	sessions map[string]*Session
	// uploads and downloads are keyed by job ID, not by session, so a job stays
	// queryable after its session is closed or its connection drops. Terminal jobs
	// are evicted once older than the pool's job retention.
	uploads   map[string]*TransferJob
	downloads map[string]*TransferJob
}

// jobsOf returns the jobs of a session that are still running.
func jobsOf(jobs map[string]*TransferJob, sessionID string) []*TransferJob {
	var out []*TransferJob
	for _, job := range jobs {
		if job.SessionID == sessionID && !job.terminal() {
			out = append(out, job)
		}
	}
	return out
}

// evict drops terminal jobs that finished more than retention ago.
func evict(jobs map[string]*TransferJob, retention time.Duration, now time.Time) {
	for id, job := range jobs {
		if at := job.finishedAt(); !at.IsZero() && now.Sub(at) > retention {
			delete(jobs, id)
		}
	}
}

type dialResult struct {
	req       OpenRequest
	client    *ssh.Client
	forwards  []io.Closer
	err       error
	userAgent string
	banner    string
}

func (p *Pool) handleDialResult(ctx context.Context, st *poolState, res dialResult) {
	if res.err != nil {
		res.req.resp <- OpenResponse{Err: res.err}
		return
	}

	truncatedBanner := truncateBanner(res.banner)
	platform := detectPlatform(res.userAgent)

	id := generateSessionID(res.req.Config.Machine, st.sessions)
	sCtx, sCancel := context.WithCancelCause(ctx)
	sess := &Session{
		ID:        id,
		Machine:   res.req.Config.Machine,
		CreatedAt: time.Now(),
		client:    res.client,
		spools:    make(map[string]string),
		ctx:       sCtx,
		cancel:    sCancel,
		forwards:  res.forwards,
		userAgent: res.userAgent,
		banner:    truncatedBanner,
		platform:  platform,
	}
	st.sessions[id] = sess
	p.logger.Debug("ssh session opened", "id", id, "machine", res.req.Config.Machine)
	res.req.resp <- OpenResponse{
		ID:        id,
		UserAgent: res.userAgent,
		Banner:    truncatedBanner,
		Platform:  platform,
	}

	// Watch the connection and remove the session if it drops or fails.
	go p.watchSession(ctx, id, res.client, sess)
}

func (p *Pool) watchSession(poolCtx context.Context, sessionID string, c *ssh.Client, s *Session) {
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Wait()
	}()

	ticker := time.NewTicker(p.keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case err := <-errCh:
			if s.ctx.Err() != nil {
				return // closed on purpose; handleClose already reported it
			}
			p.dropSession(poolCtx, sessionID, fmt.Errorf("connection lost: %w", err))
			return
		case <-ticker.C:
			if !strings.Contains(s.userAgent, "OpenSSH") {
				s.lastPing.Store(time.Now().UnixNano())
				continue
			}
			if err := p.keepalive(c); err != nil {
				if s.ctx.Err() != nil {
					return
				}
				p.logger.Debug("ssh session keepalive failed", "id", sessionID, "machine", s.Machine, "err", err)
				p.dropSession(poolCtx, sessionID, err)
				return
			}
			s.lastPing.Store(time.Now().UnixNano())
		}
	}
}

// keepalive probes the connection, giving up after the pool's keepalive timeout.
//
// The wait must be bounded: a connection that stalls without being reset (a slow or
// half-open link) never answers and never errors, so an unbounded SendRequest parks
// the watcher forever and the disconnect is never noticed. That is what leaves a
// long upload hanging with no completion and no error.
func (p *Pool) keepalive(c *ssh.Client) error {
	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		errCh <- err
	}()

	t := time.NewTimer(p.keepaliveTimeout)
	defer t.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("keepalive failed: %w", err)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("keepalive timed out after %s", p.keepaliveTimeout)
	}
}

func (p *Pool) handleOpen(ctx context.Context, dialCh chan<- dialResult, r OpenRequest) {
	if r.Config.Logger == nil {
		r.Config.Logger = p.logger
	}
	go func() {
		client, userAgent, banner, err := r.Config.dial()
		if err != nil {
			p.logger.Debug("ssh dial failed", "machine", r.Config.Machine, "err", err)
		} else {
			var alias string
			_, alias, _ = parseTarget(r.Config.Machine)
			if alias == "" {
				alias = r.Config.Machine
			}
			home := r.Config.effectiveHome()
			var forwards []io.Closer
			forwards, err = startLocalForwards(client, newSettings(home), alias, home, p.logger)
			if err != nil {
				_ = client.Close()
				p.logger.Debug("ssh local forward setup failed", "machine", r.Config.Machine, "err", err)
			} else {
				select {
				case dialCh <- dialResult{
					req:       r,
					client:    client,
					forwards:  forwards,
					err:       err,
					userAgent: userAgent,
					banner:    banner,
				}:
				case <-ctx.Done():
					closeAll(forwards)
					_ = client.Close()
				}
				return
			}
		}
		select {
		case dialCh <- dialResult{
			req:       r,
			client:    client,
			err:       err,
			userAgent: userAgent,
			banner:    banner,
		}:
		case <-ctx.Done():
			if client != nil {
				_ = client.Close()
			}
		}
	}()
}

func (p *Pool) handleGet(st *poolState, r GetRequest) {
	s, ok := st.sessions[r.ID]
	if !ok {
		r.resp <- GetResponse{Err: fmt.Errorf("session not found: %s", r.ID)}
	} else {
		r.resp <- GetResponse{Client: s.client}
	}
}

func (p *Pool) handleClose(st *poolState, r CloseRequest) {
	s, ok := st.sessions[r.ID]
	if ok {
		cause := r.Cause
		if cause == nil {
			cause = ErrSessionClosed
		}
		s.cancel(cause)

		// Record the cause now, so every transfer reports why it ended even if it is
		// wedged on a dead connection. Closing the SFTP client can block on that same
		// connection, so it goes off the event loop; the ssh.Client.Close below drops the
		// socket and unblocks it.
		for _, job := range append(jobsOf(st.uploads, r.ID), jobsOf(st.downloads, r.ID)...) {
			if closer := job.abortCause(cause); closer != nil {
				go func() { _ = closer.Close() }()
			}
		}

		for _, path := range s.spools {
			_ = p.spoolFS.Remove(path)
		}
		closeAll(s.forwards)
		// The session's whole spool directory, so an unregistered spool file
		// cannot outlive the session either.
		_ = p.spoolFS.RemoveAll(r.ID)

		_ = s.client.Close()
		delete(st.sessions, r.ID)
		p.logger.Debug("ssh session closed", "id", r.ID, "machine", s.Machine, "cause", cause)
	}
	r.resp <- nil
}

func (p *Pool) handleList(st *poolState, r ListRequest) {
	out := make([]SessionInfo, 0, len(st.sessions))
	for _, s := range st.sessions {
		info := SessionInfo{
			ID:        s.ID,
			Machine:   s.Machine,
			CreatedAt: s.CreatedAt,
			UserAgent: s.userAgent,
			Banner:    s.banner,
			Platform:  s.platform,
		}
		if ns := s.lastPing.Load(); ns != 0 {
			info.LastPing = time.Unix(0, ns)
			if time.Since(info.LastPing) < 30*time.Second {
				info.Status = "connected"
			} else {
				info.Status = "stale"
			}
		} else {
			info.Status = "new"
		}
		out = append(out, info)
	}
	r.resp <- out
}

func (p *Pool) handleUpload(st *poolState, r UploadRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- UploadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
		return
	}
	// The upload itself goes through p.localFS and would refuse a disallowed
	// path anyway; rejecting it here only turns an async job failure into an
	// immediate, legible error.
	if _, err := p.localFS.Resolve(r.LocalPath); err != nil {
		r.resp <- UploadResponse{Err: err}
		return
	}
	evict(st.uploads, p.jobRetention, time.Now())

	uploadID := fmt.Sprintf("upload-%d", time.Now().UnixNano())
	job, uCtx := newTransferJob(s.ctx, uploadID, r.SessionID, r.LocalPath, r.RemotePath)
	st.uploads[uploadID] = job
	go runUpload(uCtx, s.client, p.localFS, job)
	r.resp <- UploadResponse{UploadID: uploadID}
}

func (p *Pool) handleUploadStatus(st *poolState, r UploadStatusRequest) {
	job, err := findUploadJob(st, r.SessionID, r.UploadID)
	if err != nil {
		r.resp <- UploadStatusResponse{Err: err}
		return
	}
	r.resp <- uploadStatus(job)
}

func uploadStatus(job *TransferJob) UploadStatusResponse {
	s := newTransferSnapshot(job)
	return UploadStatusResponse{
		UploadID:        job.ID,
		BytesUploaded:   s.bytes,
		TotalBytes:      s.total,
		Percent:         s.percent,
		InstantSpeedBPS: s.instantSpeedBPS,
		AverageSpeedBPS: s.averageSpeedBPS,
		DurationSeconds: s.durationSeconds,
		ETASeconds:      s.etaSeconds,
		Done:            s.done,
		Status:          s.status,
		Err:             s.err,
	}
}

func (p *Pool) handleUploadWait(st *poolState, r UploadWaitRequest) {
	job, err := findUploadJob(st, r.SessionID, r.UploadID)
	if err != nil {
		r.resp <- UploadStatusResponse{Err: err}
		return
	}
	go func() {
		select {
		case <-job.done:
			r.resp <- uploadStatus(job)
		case <-r.Ctx.Done():
			resp := uploadStatus(job)
			resp.Err = r.Ctx.Err()
			r.resp <- resp
		}
	}()
}

func (p *Pool) handleUploadCancel(st *poolState, r UploadCancelRequest) {
	job, err := findUploadJob(st, r.SessionID, r.UploadID)
	if err != nil {
		r.resp <- UploadStatusResponse{Err: err}
		return
	}
	cancelJob(job)
	go func() {
		select {
		case <-job.done:
			r.resp <- uploadStatus(job)
		case <-r.Ctx.Done():
			resp := uploadStatus(job)
			resp.Err = r.Ctx.Err()
			r.resp <- resp
		}
	}()
}

// cancelJob cancels a job from the event loop: the cause is recorded inline, the SFTP client
// is closed on a separate goroutine because that can block on a dead connection.
func cancelJob(job *TransferJob) {
	if closer := job.abortCause(ErrTransferCanceled); closer != nil {
		go func() { _ = closer.Close() }()
	}
}

// findUploadJob looks the job up by ID. The job is returned even when its session is
// long gone: a caller polling a transfer that died with the connection needs the
// terminal status, not "session not found".
func findUploadJob(st *poolState, sessionID, uploadID string) (*TransferJob, error) {
	job, ok := st.uploads[uploadID]
	if ok {
		return job, nil
	}
	if _, ok := st.sessions[sessionID]; !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return nil, fmt.Errorf("upload not found: %s", uploadID)
}

// transferSnapshot is a consistent read of a job's progress.
type transferSnapshot struct {
	bytes           int64
	total           int64
	percent         float64
	instantSpeedBPS float64
	averageSpeedBPS float64
	durationSeconds float64
	etaSeconds      float64
	done            bool
	status          TransferStatus
	err             error
}

func newTransferSnapshot(job *TransferJob) transferSnapshot {
	now := time.Now()
	job.mu.Lock()
	defer job.mu.Unlock()

	percent := float64(0)
	switch {
	case job.TotalBytes > 0:
		percent = (float64(job.Bytes) / float64(job.TotalBytes)) * 100
	case job.Status == TransferCompleted:
		percent = 100
	}

	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		job.StartedAt,
		job.FinishedAt,
		job.LastStatusAt,
		job.LastStatus,
		job.Bytes,
		job.TotalBytes,
		job.Done,
	)
	if !job.Done && (job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval) {
		job.LastStatusAt = now
		job.LastStatus = job.Bytes
	}

	return transferSnapshot{
		bytes:           job.Bytes,
		total:           job.TotalBytes,
		percent:         percent,
		instantSpeedBPS: instantSpeedBPS,
		averageSpeedBPS: averageSpeedBPS,
		durationSeconds: durationSeconds,
		etaSeconds:      etaSeconds,
		done:            job.Done,
		status:          job.Status,
		err:             job.Err,
	}
}

func transferStats(
	now time.Time,
	startedAt time.Time,
	finishedAt time.Time,
	lastStatusAt time.Time,
	lastStatus int64,
	current int64,
	total int64,
	done bool,
) (instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds float64) {
	endAt := now
	if done && !finishedAt.IsZero() {
		endAt = finishedAt
	}

	if elapsed := endAt.Sub(startedAt).Seconds(); !startedAt.IsZero() && elapsed > 0 {
		durationSeconds = elapsed
		averageSpeedBPS = float64(current) / elapsed
	}

	if elapsed := endAt.Sub(lastStatusAt).Seconds(); !lastStatusAt.IsZero() && elapsed > 0 {
		instantSpeedBPS = float64(current-lastStatus) / elapsed
	}
	if done && instantSpeedBPS == 0 {
		instantSpeedBPS = averageSpeedBPS
	}

	remaining := total - current
	if !done && remaining > 0 && averageSpeedBPS > 0 {
		etaSeconds = float64(remaining) / averageSpeedBPS
	}

	return instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds
}

func (p *Pool) handleDownload(st *poolState, r DownloadRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- DownloadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
		return
	}
	// See handleUpload: p.localFS gates the write regardless; this just fails
	// fast on a destination it would refuse.
	if _, err := p.localFS.Resolve(r.LocalPath); err != nil {
		r.resp <- DownloadResponse{Err: err}
		return
	}
	evict(st.downloads, p.jobRetention, time.Now())

	downloadID := fmt.Sprintf("download-%d", time.Now().UnixNano())
	job, dCtx := newTransferJob(s.ctx, downloadID, r.SessionID, r.LocalPath, r.RemotePath)
	st.downloads[downloadID] = job
	go runDownload(dCtx, s.client, p.localFS, job)
	r.resp <- DownloadResponse{DownloadID: downloadID}
}

func (p *Pool) handleDownloadStatus(st *poolState, r DownloadStatusRequest) {
	job, err := findDownloadJob(st, r.SessionID, r.DownloadID)
	if err != nil {
		r.resp <- DownloadStatusResponse{Err: err}
		return
	}
	r.resp <- downloadStatus(job)
}

func downloadStatus(job *TransferJob) DownloadStatusResponse {
	s := newTransferSnapshot(job)
	return DownloadStatusResponse{
		DownloadID:      job.ID,
		BytesDownloaded: s.bytes,
		TotalBytes:      s.total,
		Percent:         s.percent,
		InstantSpeedBPS: s.instantSpeedBPS,
		AverageSpeedBPS: s.averageSpeedBPS,
		DurationSeconds: s.durationSeconds,
		ETASeconds:      s.etaSeconds,
		Done:            s.done,
		Status:          s.status,
		Err:             s.err,
	}
}

func (p *Pool) handleDownloadWait(st *poolState, r DownloadWaitRequest) {
	job, err := findDownloadJob(st, r.SessionID, r.DownloadID)
	if err != nil {
		r.resp <- DownloadStatusResponse{Err: err}
		return
	}
	go func() {
		select {
		case <-job.done:
			r.resp <- downloadStatus(job)
		case <-r.Ctx.Done():
			resp := downloadStatus(job)
			resp.Err = r.Ctx.Err()
			r.resp <- resp
		}
	}()
}

func (p *Pool) handleDownloadCancel(st *poolState, r DownloadCancelRequest) {
	job, err := findDownloadJob(st, r.SessionID, r.DownloadID)
	if err != nil {
		r.resp <- DownloadStatusResponse{Err: err}
		return
	}
	cancelJob(job)
	go func() {
		select {
		case <-job.done:
			r.resp <- downloadStatus(job)
		case <-r.Ctx.Done():
			resp := downloadStatus(job)
			resp.Err = r.Ctx.Err()
			r.resp <- resp
		}
	}()
}

// findDownloadJob mirrors [findUploadJob] for downloads.
func findDownloadJob(st *poolState, sessionID, downloadID string) (*TransferJob, error) {
	job, ok := st.downloads[downloadID]
	if ok {
		return job, nil
	}
	if _, ok := st.sessions[sessionID]; !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return nil, fmt.Errorf("download not found: %s", downloadID)
}

func (p *Pool) handleRegisterSpool(st *poolState, r RegisterSpoolRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	s.spools[r.SpoolID] = r.Path
	r.resp <- nil
}

func (p *Pool) handleGetSpool(st *poolState, r GetSpoolRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- GetSpoolResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
		return
	}
	path, ok := s.spools[r.SpoolID]
	if !ok {
		r.resp <- GetSpoolResponse{Err: fmt.Errorf("spool ID not found: %s", r.SpoolID)}
		return
	}
	r.resp <- GetSpoolResponse{Path: path}
}

func (p *Pool) handleDeleteSpool(st *poolState, r DeleteSpoolRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	path, ok := s.spools[r.SpoolID]
	if !ok {
		r.resp <- fmt.Errorf("spool ID not found: %s", r.SpoolID)
		return
	}
	_ = p.spoolFS.Remove(path)
	delete(s.spools, r.SpoolID)
	r.resp <- nil
}

func (p *Pool) handleMachine(st *poolState, r MachineRequest) {
	s, ok := st.sessions[r.ID]
	if !ok {
		r.resp <- MachineResponse{Err: fmt.Errorf("session not found: %s", r.ID)}
	} else {
		r.resp <- MachineResponse{Machine: s.Machine}
	}
}

func (p *Pool) handleTouch(st *poolState, r TouchRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	s.lastPing.Store(r.At.UnixNano())
	r.resp <- nil
}

func (p *Pool) handleExec(st *poolState, r ExecRequest) {
	s, ok := st.sessions[r.SessionID]
	if !ok {
		r.resp <- ExecResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
		return
	}
	r.DescriptionComment = r.DescriptionComment && s.platform == "linux"

	// Run in background so we don't block the event loop
	go p.executeCommand(s.ctx, s.client, r)
}

func truncateBanner(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if len(line) <= 100 {
		return line
	}
	cutLen := 0
	for cutLen < len(line) {
		_, size := utf8.DecodeRuneInString(line[cutLen:])
		if cutLen+size > 100 {
			break
		}
		cutLen += size
	}
	return line[:cutLen]
}

func detectPlatform(serverVersion string) string {
	switch {
	case strings.Contains(serverVersion, "Cisco"):
		return "cisco"
	case strings.Contains(serverVersion, "ROSSSH"):
		return "mikrotik"
	case strings.Contains(serverVersion, "OpenSSH_for_Windows"):
		return "windows"
	case strings.Contains(serverVersion, "OpenSSH"):
		return "linux" // best guess; covers the vast majority
	default:
		return "unknown"
	}
}
