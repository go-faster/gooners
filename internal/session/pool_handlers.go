package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type dialResult struct {
	req       OpenRequest
	client    *ssh.Client
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
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = c.Close()
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
		cancel:     uCancel,
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
	job.mu.Lock()
	percent := float64(0)
	if job.TotalBytes > 0 {
		percent = (float64(job.BytesUploaded) / float64(job.TotalBytes)) * 100
	} else if job.Done {
		percent = 100
	}
	resp := UploadStatusResponse{
		UploadID:      job.ID,
		BytesUploaded: job.BytesUploaded,
		TotalBytes:    job.TotalBytes,
		Percent:       percent,
		Done:          job.Done,
		Err:           job.Err,
	}
	job.mu.Unlock()
	r.resp <- resp
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
		cancel:     dCancel,
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
	job.mu.Lock()
	percent := float64(0)
	if job.TotalBytes > 0 {
		percent = (float64(job.BytesDownloaded) / float64(job.TotalBytes)) * 100
	} else if job.Done {
		percent = 100
	}
	resp := DownloadStatusResponse{
		DownloadID:      job.ID,
		BytesDownloaded: job.BytesDownloaded,
		TotalBytes:      job.TotalBytes,
		Percent:         percent,
		Done:            job.Done,
		Err:             job.Err,
	}
	job.mu.Unlock()
	r.resp <- resp
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

func (p *Pool) handleExec(sessions map[string]*Session, r ExecRequest) {
	s, ok := sessions[r.SessionID]
	if !ok {
		r.resp <- ExecResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
		return
	}

	// Run in background so we don't block the event loop
	go p.executeCommand(s.ctx, s.client, r)
}

func truncateBanner(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	cutLen := min(len(line), 100)
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
