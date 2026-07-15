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

type dialResult struct {
	req       OpenRequest
	client    *ssh.Client
	forwards  []io.Closer
	err       error
	userAgent string
	banner    string
}

func (p *Pool) handleDialResult(ctx context.Context, sessions map[string]*Session, res dialResult) {
	if res.err != nil {
		res.req.resp <- OpenResponse{Err: res.err}
		return
	}

	truncatedBanner := truncateBanner(res.banner)
	platform := detectPlatform(res.userAgent)

	machine := res.req.Config.Machine
	id := generateSessionID(machine, sessions)
	label := generateSessionLabel(machine)
	sess := newSession(ctx, id, machine, label, res.client, res.forwards, res.userAgent, truncatedBanner, platform, nil)
	sessions[id] = sess
	p.logger.Debug("ssh session opened", "id", id, "machine", machine)
	res.req.resp <- OpenResponse{
		ID:        id,
		Label:     label,
		UserAgent: res.userAgent,
		Banner:    truncatedBanner,
		Platform:  platform,
	}

	// Watch the connection and remove the session if it drops or fails.
	go p.watchSession(ctx, id, res.client, sess)
}

// handleAdopt registers an already-connected client as a session, giving it
// the same bookkeeping as one created via handleDialResult.
func (p *Pool) handleAdopt(ctx context.Context, sessions map[string]*Session, r AdoptRequest) {
	if r.Client == nil {
		r.resp <- OpenResponse{Err: fmt.Errorf("adopt: client is required")}
		return
	}

	machine := r.Machine
	if machine == "" {
		machine = "adopted"
	}
	userAgent := string(r.Client.ServerVersion())
	platform := detectPlatform(userAgent)

	id := generateSessionID(machine, sessions)
	label := generateSessionLabel(machine)
	sess := newSession(ctx, id, machine, label, r.Client, nil, userAgent, "", platform, r.OnClose)
	sessions[id] = sess
	p.logger.Debug("ssh session adopted", "id", id, "machine", machine)
	r.resp <- OpenResponse{
		ID:        id,
		Label:     label,
		UserAgent: userAgent,
		Platform:  platform,
	}

	// Watch the connection and remove the session if it drops or fails.
	go p.watchSession(ctx, id, r.Client, sess)
}

func (p *Pool) watchSession(poolCtx context.Context, sessionID string, c *ssh.Client, s *Session) {
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Wait()
	}()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-errCh:
			_ = p.Close(poolCtx, sessionID)
			return
		case <-ticker.C:
			if !strings.Contains(s.userAgent, "OpenSSH") {
				s.lastPing.Store(time.Now().UnixNano())
				continue
			}
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = p.Close(poolCtx, sessionID)
				return
			}
			s.lastPing.Store(time.Now().UnixNano())
		}
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

// lookup returns the session with id, recording activity on it. Keep this as
// the single place that reads from the sessions map by ID so every tool call
// touches lastUsed for the idle sweep.
func lookup(sessions map[string]*Session, id string) (*Session, error) {
	s, ok := sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	s.lastUsed.Store(time.Now().UnixNano())
	return s, nil
}

// closeSession tears down s unconditionally: cancels in-flight jobs, removes
// its spool files and its spool directory, closes forwards and the SSH client,
// and removes s from sessions. s.onClose (if set) always runs in its own
// goroutine, never on the caller's goroutine (the actor loop).
func (p *Pool) closeSession(sessions map[string]*Session, s *Session) {
	s.cancel()
	for _, job := range s.uploads {
		job.cancel()
	}
	for _, job := range s.downloads {
		job.cancel()
	}
	for _, path := range s.spools {
		_ = p.spoolFS.Remove(path)
	}
	closeAll(s.forwards)
	// The session's whole spool directory, so an unregistered spool file
	// cannot outlive the session either.
	_ = p.spoolFS.RemoveAll(s.ID)

	_ = s.client.Close()
	delete(sessions, s.ID)

	if s.onClose != nil {
		go s.onClose()
	}
}

func (p *Pool) handleGet(sessions map[string]*Session, r GetRequest) {
	s, err := lookup(sessions, r.ID)
	if err != nil {
		r.resp <- GetResponse{Err: err}
		return
	}
	r.resp <- GetResponse{Client: s.client}
}

func (p *Pool) handleClose(sessions map[string]*Session, r CloseRequest) {
	if s, ok := sessions[r.ID]; ok {
		p.logger.Debug("ssh session closed", "id", r.ID, "machine", s.Machine)
		p.closeSession(sessions, s)
	}
	r.resp <- nil
}

func (p *Pool) handleList(sessions map[string]*Session, r ListRequest) {
	out := make([]SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		info := SessionInfo{
			ID:        s.ID,
			Label:     s.Label,
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

func (p *Pool) handleUpload(sessions map[string]*Session, r UploadRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- UploadResponse{Err: err}
		return
	}
	// The upload itself goes through p.localFS and would refuse a disallowed
	// path anyway; rejecting it here only turns an async job failure into an
	// immediate, legible error.
	if _, err := p.localFS.Resolve(r.LocalPath); err != nil {
		r.resp <- UploadResponse{Err: err}
		return
	}
	uploadID := fmt.Sprintf("upload-%d", time.Now().UnixNano())
	uCtx, uCancel := context.WithCancel(s.ctx)
	job := &UploadJob{
		ID:         uploadID,
		LocalPath:  r.LocalPath,
		RemotePath: r.RemotePath,
		StartedAt:  time.Now(),
		cancel:     uCancel,
		done:       make(chan struct{}),
	}
	s.uploads[uploadID] = job
	go runUpload(uCtx, s.client, p.localFS, job)
	r.resp <- UploadResponse{UploadID: uploadID}
}

func (p *Pool) handleUploadStatus(sessions map[string]*Session, r UploadStatusRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- UploadStatusResponse{Err: err}
		return
	}
	job, ok := s.uploads[r.UploadID]
	if !ok {
		r.resp <- UploadStatusResponse{Err: fmt.Errorf("upload not found: %s", r.UploadID)}
		return
	}
	r.resp <- uploadStatus(job)
}

func uploadStatus(job *UploadJob) UploadStatusResponse {
	now := time.Now()
	job.mu.Lock()
	defer job.mu.Unlock()
	percent := float64(0)
	if job.TotalBytes > 0 {
		percent = (float64(job.BytesUploaded) / float64(job.TotalBytes)) * 100
	} else if job.Done {
		percent = 100
	}
	instantSpeedBPS, averageSpeedBPS, etaSeconds := transferStats(
		now,
		job.StartedAt,
		job.LastStatusAt,
		job.LastStatus,
		job.BytesUploaded,
		job.TotalBytes,
		job.Done,
	)
	if job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval {
		job.LastStatusAt = now
		job.LastStatus = job.BytesUploaded
	}
	resp := UploadStatusResponse{
		UploadID:        job.ID,
		BytesUploaded:   job.BytesUploaded,
		TotalBytes:      job.TotalBytes,
		Percent:         percent,
		InstantSpeedBPS: instantSpeedBPS,
		AverageSpeedBPS: averageSpeedBPS,
		ETASeconds:      etaSeconds,
		Done:            job.Done,
		Err:             job.Err,
	}
	return resp
}

func (p *Pool) handleUploadWait(sessions map[string]*Session, r UploadWaitRequest) {
	job, resp, ok := findUploadJob(sessions, r.SessionID, r.UploadID)
	if !ok {
		r.resp <- resp
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

func (p *Pool) handleUploadCancel(sessions map[string]*Session, r UploadCancelRequest) {
	job, resp, ok := findUploadJob(sessions, r.SessionID, r.UploadID)
	if !ok {
		r.resp <- resp
		return
	}
	job.cancel()
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

func findUploadJob(sessions map[string]*Session, sessionID, uploadID string) (*UploadJob, UploadStatusResponse, bool) {
	s, err := lookup(sessions, sessionID)
	if err != nil {
		return nil, UploadStatusResponse{Err: err}, false
	}
	job, ok := s.uploads[uploadID]
	if !ok {
		return nil, UploadStatusResponse{Err: fmt.Errorf("upload not found: %s", uploadID)}, false
	}
	return job, UploadStatusResponse{}, true
}

func transferStats(
	now time.Time,
	startedAt time.Time,
	lastStatusAt time.Time,
	lastStatus int64,
	current int64,
	total int64,
	done bool,
) (instantSpeedBPS, averageSpeedBPS, etaSeconds float64) {
	if elapsed := now.Sub(startedAt).Seconds(); !done && !startedAt.IsZero() && elapsed > 0 {
		averageSpeedBPS = float64(current) / elapsed
	}

	if elapsed := now.Sub(lastStatusAt).Seconds(); !done && !lastStatusAt.IsZero() && elapsed > 0 {
		instantSpeedBPS = float64(current-lastStatus) / elapsed
	}

	remaining := total - current
	if !done && remaining > 0 && averageSpeedBPS > 0 {
		etaSeconds = float64(remaining) / averageSpeedBPS
	}

	return instantSpeedBPS, averageSpeedBPS, etaSeconds
}

func (p *Pool) handleDownload(sessions map[string]*Session, r DownloadRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- DownloadResponse{Err: err}
		return
	}
	// See handleUpload: p.localFS gates the write regardless; this just fails
	// fast on a destination it would refuse.
	if _, err := p.localFS.Resolve(r.LocalPath); err != nil {
		r.resp <- DownloadResponse{Err: err}
		return
	}
	downloadID := fmt.Sprintf("download-%d", time.Now().UnixNano())
	dCtx, dCancel := context.WithCancel(s.ctx)
	job := &DownloadJob{
		ID:         downloadID,
		LocalPath:  r.LocalPath,
		RemotePath: r.RemotePath,
		StartedAt:  time.Now(),
		cancel:     dCancel,
		done:       make(chan struct{}),
	}
	s.downloads[downloadID] = job
	go runDownload(dCtx, s.client, p.localFS, job)
	r.resp <- DownloadResponse{DownloadID: downloadID}
}

func (p *Pool) handleDownloadStatus(sessions map[string]*Session, r DownloadStatusRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- DownloadStatusResponse{Err: err}
		return
	}
	job, ok := s.downloads[r.DownloadID]
	if !ok {
		r.resp <- DownloadStatusResponse{Err: fmt.Errorf("download not found: %s", r.DownloadID)}
		return
	}
	r.resp <- downloadStatus(job)
}

func downloadStatus(job *DownloadJob) DownloadStatusResponse {
	now := time.Now()
	job.mu.Lock()
	defer job.mu.Unlock()
	percent := float64(0)
	if job.TotalBytes > 0 {
		percent = (float64(job.BytesDownloaded) / float64(job.TotalBytes)) * 100
	} else if job.Done {
		percent = 100
	}
	instantSpeedBPS, averageSpeedBPS, etaSeconds := transferStats(
		now,
		job.StartedAt,
		job.LastStatusAt,
		job.LastStatus,
		job.BytesDownloaded,
		job.TotalBytes,
		job.Done,
	)
	if job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval {
		job.LastStatusAt = now
		job.LastStatus = job.BytesDownloaded
	}
	resp := DownloadStatusResponse{
		DownloadID:      job.ID,
		BytesDownloaded: job.BytesDownloaded,
		TotalBytes:      job.TotalBytes,
		Percent:         percent,
		InstantSpeedBPS: instantSpeedBPS,
		AverageSpeedBPS: averageSpeedBPS,
		ETASeconds:      etaSeconds,
		Done:            job.Done,
		Err:             job.Err,
	}
	return resp
}

func (p *Pool) handleDownloadWait(sessions map[string]*Session, r DownloadWaitRequest) {
	job, resp, ok := findDownloadJob(sessions, r.SessionID, r.DownloadID)
	if !ok {
		r.resp <- resp
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

func (p *Pool) handleDownloadCancel(sessions map[string]*Session, r DownloadCancelRequest) {
	job, resp, ok := findDownloadJob(sessions, r.SessionID, r.DownloadID)
	if !ok {
		r.resp <- resp
		return
	}
	job.cancel()
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

func findDownloadJob(sessions map[string]*Session, sessionID, downloadID string) (*DownloadJob, DownloadStatusResponse, bool) {
	s, err := lookup(sessions, sessionID)
	if err != nil {
		return nil, DownloadStatusResponse{Err: err}, false
	}
	job, ok := s.downloads[downloadID]
	if !ok {
		return nil, DownloadStatusResponse{Err: fmt.Errorf("download not found: %s", downloadID)}, false
	}
	return job, DownloadStatusResponse{}, true
}

func (p *Pool) handleRegisterSpool(sessions map[string]*Session, r RegisterSpoolRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- err
		return
	}
	s.spools[r.SpoolID] = r.Path
	r.resp <- nil
}

func (p *Pool) handleGetSpool(sessions map[string]*Session, r GetSpoolRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- GetSpoolResponse{Err: err}
		return
	}
	path, ok := s.spools[r.SpoolID]
	if !ok {
		r.resp <- GetSpoolResponse{Err: fmt.Errorf("spool ID not found: %s", r.SpoolID)}
		return
	}
	r.resp <- GetSpoolResponse{Path: path}
}

func (p *Pool) handleDeleteSpool(sessions map[string]*Session, r DeleteSpoolRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- err
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

func (p *Pool) handleMachine(sessions map[string]*Session, r MachineRequest) {
	s, err := lookup(sessions, r.ID)
	if err != nil {
		r.resp <- MachineResponse{Err: err}
		return
	}
	r.resp <- MachineResponse{Machine: s.Machine}
}

func (p *Pool) handleTouch(sessions map[string]*Session, r TouchRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- err
		return
	}
	s.lastPing.Store(r.At.UnixNano())
	r.resp <- nil
}

func (p *Pool) handleExec(sessions map[string]*Session, r ExecRequest) {
	s, err := lookup(sessions, r.SessionID)
	if err != nil {
		r.resp <- ExecResponse{Err: err}
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
	case strings.Contains(serverVersion, "gooners_sandbox_agent"):
		return "linux" // in-container sandbox agent always runs Linux
	case strings.Contains(serverVersion, "OpenSSH"):
		return "linux" // best guess; covers the vast majority
	default:
		return "unknown"
	}
}
