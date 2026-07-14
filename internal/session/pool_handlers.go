package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	id := generateSessionID(res.req.Config.Machine, sessions)
	sCtx, sCancel := context.WithCancel(ctx)
	sess := &Session{
		ID:        id,
		Machine:   res.req.Config.Machine,
		CreatedAt: time.Now(),
		client:    res.client,
		uploads:   make(map[string]*UploadJob),
		downloads: make(map[string]*DownloadJob),
		spools:    make(map[string]string),
		ctx:       sCtx,
		cancel:    sCancel,
		forwards:  res.forwards,
		userAgent: res.userAgent,
		banner:    truncatedBanner,
		platform:  platform,
	}
	sessions[id] = sess
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

func (p *Pool) handleGet(sessions map[string]*Session, r GetRequest) {
	s, ok := sessions[r.ID]
	if !ok {
		r.resp <- GetResponse{Err: fmt.Errorf("session not found: %s", r.ID)}
	} else {
		r.resp <- GetResponse{Client: s.client}
	}
}

func (p *Pool) handleClose(sessions map[string]*Session, r CloseRequest) {
	s, ok := sessions[r.ID]
	if ok {
		s.cancel()
		for _, job := range s.uploads {
			job.cancel()
		}
		for _, job := range s.downloads {
			job.cancel()
		}
		for _, path := range s.spools {
			_ = os.Remove(path)
		}
		closeAll(s.forwards)
		sessionDir := filepath.Join(os.TempDir(), "ssh-mcp", "sessions", r.ID)
		_ = os.RemoveAll(sessionDir)

		_ = s.client.Close()
		delete(sessions, r.ID)
		p.logger.Debug("ssh session closed", "id", r.ID, "machine", s.Machine)
	}
	r.resp <- nil
}

func (p *Pool) handleList(sessions map[string]*Session, r ListRequest) {
	out := make([]SessionInfo, 0, len(sessions))
	for _, s := range sessions {
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

func (p *Pool) handleUpload(sessions map[string]*Session, r UploadRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- UploadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
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
	go runUpload(uCtx, s.client, job)
	r.resp <- UploadResponse{UploadID: uploadID}
}

func (p *Pool) handleUploadStatus(sessions map[string]*Session, r UploadStatusRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- UploadStatusResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
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
	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		job.StartedAt,
		job.FinishedAt,
		job.LastStatusAt,
		job.LastStatus,
		job.BytesUploaded,
		job.TotalBytes,
		job.Done,
	)
	if !job.Done && (job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval) {
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
		DurationSeconds: durationSeconds,
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
	s, ok := sessions[sessionID]
	if !ok {
		return nil, UploadStatusResponse{Err: fmt.Errorf("session not found: %s", sessionID)}, false
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

func (p *Pool) handleDownload(sessions map[string]*Session, r DownloadRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- DownloadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
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
	go runDownload(dCtx, s.client, job)
	r.resp <- DownloadResponse{DownloadID: downloadID}
}

func (p *Pool) handleDownloadStatus(sessions map[string]*Session, r DownloadStatusRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- DownloadStatusResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
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
	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		job.StartedAt,
		job.FinishedAt,
		job.LastStatusAt,
		job.LastStatus,
		job.BytesDownloaded,
		job.TotalBytes,
		job.Done,
	)
	if !job.Done && (job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval) {
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
		DurationSeconds: durationSeconds,
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
	s, ok := sessions[sessionID]
	if !ok {
		return nil, DownloadStatusResponse{Err: fmt.Errorf("session not found: %s", sessionID)}, false
	}
	job, ok := s.downloads[downloadID]
	if !ok {
		return nil, DownloadStatusResponse{Err: fmt.Errorf("download not found: %s", downloadID)}, false
	}
	return job, DownloadStatusResponse{}, true
}

func (p *Pool) handleRegisterSpool(sessions map[string]*Session, r RegisterSpoolRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	s.spools[r.SpoolID] = r.Path
	r.resp <- nil
}

func (p *Pool) handleGetSpool(sessions map[string]*Session, r GetSpoolRequest) {
	s, ok := sessions[r.SessionID]
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

func (p *Pool) handleDeleteSpool(sessions map[string]*Session, r DeleteSpoolRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	path, ok := s.spools[r.SpoolID]
	if !ok {
		r.resp <- fmt.Errorf("spool ID not found: %s", r.SpoolID)
		return
	}
	_ = os.Remove(path)
	delete(s.spools, r.SpoolID)
	r.resp <- nil
}

func (p *Pool) handleMachine(sessions map[string]*Session, r MachineRequest) {
	s, ok := sessions[r.ID]
	if !ok {
		r.resp <- MachineResponse{Err: fmt.Errorf("session not found: %s", r.ID)}
	} else {
		r.resp <- MachineResponse{Machine: s.Machine}
	}
}

func (p *Pool) handleTouch(sessions map[string]*Session, r TouchRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
		return
	}
	s.lastPing.Store(r.At.UnixNano())
	r.resp <- nil
}

func (p *Pool) handleExec(sessions map[string]*Session, r ExecRequest) {
	s, ok := sessions[r.SessionID]
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
