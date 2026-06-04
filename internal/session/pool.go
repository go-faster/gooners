package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/sshutil"
)

type Session struct {
	ID        string
	Machine   string
	CreatedAt time.Time
	client    *ssh.Client
	lastPing  atomic.Int64 // unix nanoseconds of last successful keepalive; 0 = no ping yet
	// TODO: completed jobs are never evicted from these maps, which is a known leak
	uploads   map[string]*UploadJob
	downloads map[string]*DownloadJob
	spools    map[string]string // spoolID -> localFilePath
}

type UploadJob struct {
	ID            string
	LocalPath     string
	RemotePath    string
	TotalBytes    int64
	BytesUploaded int64
	Done          bool
	Err           error
	mu            sync.Mutex
	cancel        context.CancelFunc
}

type DownloadJob struct {
	ID              string
	LocalPath       string
	RemotePath      string
	TotalBytes      int64
	BytesDownloaded int64
	Done            bool
	Err             error
	mu              sync.Mutex
	cancel          context.CancelFunc
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Machine   string    `json:"machine"`
	CreatedAt time.Time `json:"created_at"`
	LastPing  time.Time `json:"last_ping,omitempty"`
	// Status is "connected" if a keepalive succeeded within the last 30s, "new" if no ping yet, "stale" otherwise.
	Status string `json:"status"`
}

type Provider interface {
	Get(ctx context.Context, id string) (*ssh.Client, error)
	SFTP(ctx context.Context, id string) (*sftp.Client, error)
	Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error)
	UploadStatus(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error)
	Download(ctx context.Context, sessionID, remotePath, localPath string) (string, error)
	DownloadStatus(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error)
	Run(ctx context.Context, sessionID, cmd string) (sshutil.Result, error)
	RunWithOptions(ctx context.Context, sessionID, cmd string, opts sshutil.RunOptions) (sshutil.Result, error)
	CommandTimeout() time.Duration
	Ping(ctx context.Context, id string) (time.Duration, error)
}

// Pool manages SSH sessions.
// Note: You must call RunLoop(ctx) on the Pool before using it, otherwise
// methods like Open, Close, and Exec will deadlock waiting for the event loop.
// The event loop and all managed sessions are terminated when ctx is canceled.
type Pool struct {
	reqCh          chan Request
	commandTimeout time.Duration
	maxOutputBytes int64
}

// PoolOptions contains configuration for a new Pool.
type PoolOptions struct {
	CommandTimeout time.Duration
	MaxOutputBytes int64
}

func (opts *PoolOptions) setDefaults() {
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 10 * time.Second
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = 8192 // default 8KB
	}
}

func NewPool(opts PoolOptions) *Pool {
	opts.setDefaults()
	return &Pool{
		reqCh:          make(chan Request),
		commandTimeout: opts.CommandTimeout,
		maxOutputBytes: opts.MaxOutputBytes,
	}
}

func (p *Pool) CommandTimeout() time.Duration {
	return p.commandTimeout
}

func (p *Pool) Ping(ctx context.Context, id string) (time.Duration, error) {
	client, err := p.Get(ctx, id)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case err := <-errCh:
		if err != nil {
			_ = p.Close(ctx, id)
			return 0, err
		}
		return time.Since(start), nil
	}
}

func (p *Pool) Run(ctx context.Context, sessionID, cmd string) (sshutil.Result, error) {
	return p.RunWithOptions(ctx, sessionID, cmd, sshutil.RunOptions{Timeout: p.CommandTimeout()})
}

func (p *Pool) RunWithOptions(ctx context.Context, sessionID, cmd string, opts sshutil.RunOptions) (sshutil.Result, error) {
	client, err := p.Get(ctx, sessionID)
	if err != nil {
		return sshutil.Result{}, err
	}
	return sshutil.Run(ctx, client, cmd, opts)
}

func (p *Pool) RunLoop(ctx context.Context) {
	sessions := make(map[string]*Session)

	type dialResult struct {
		req    OpenRequest
		client *ssh.Client
		err    error
	}
	dialCh := make(chan dialResult)

	for {
		select {
		case <-ctx.Done():
			for _, s := range sessions {
				for _, path := range s.spools {
					_ = os.Remove(path)
				}
				sessionDir := filepath.Join(os.TempDir(), "ssh-mcp", "sessions", s.ID)
				_ = os.RemoveAll(sessionDir)

				_ = s.client.Close()
				slog.Debug("ssh session closed (shutdown)", "id", s.ID, "machine", s.Machine)
			}
			return
		case res := <-dialCh:
			if res.err != nil {
				res.req.resp <- OpenResponse{Err: res.err}
				continue
			}

			id := generateSessionID(res.req.Config.Machine, sessions)
			sess := &Session{
				ID:        id,
				Machine:   res.req.Config.Machine,
				CreatedAt: time.Now(),
				client:    res.client,
				uploads:   make(map[string]*UploadJob),
				downloads: make(map[string]*DownloadJob),
				spools:    make(map[string]string),
			}
			sessions[id] = sess
			slog.Debug("ssh session opened", "id", id, "machine", res.req.Config.Machine)
			res.req.resp <- OpenResponse{ID: id}

			// Watch the connection and remove the session if it drops or fails.
			go func(sessionID string, c *ssh.Client, s *Session) {
				errCh := make(chan error, 1)
				go func() {
					errCh <- c.Wait()
				}()

				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()

				for {
					select {
					case <-errCh:
						_ = p.Close(context.Background(), sessionID)
						return
					case <-ticker.C:
						if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
							_ = c.Close()
							return
						}
						s.lastPing.Store(time.Now().UnixNano())
					}
				}
			}(id, res.client, sess)

		case req := <-p.reqCh:
			switch r := req.(type) {
			case OpenRequest:
				go func(r OpenRequest) {
					client, err := r.Config.dial()
					if err != nil {
						slog.Debug("ssh dial failed", "machine", r.Config.Machine, "err", err)
					}
					select {
					case dialCh <- dialResult{req: r, client: client, err: err}:
					case <-ctx.Done():
						if client != nil {
							_ = client.Close()
						}
					}
				}(r)
			case GetRequest:
				s, ok := sessions[r.ID]
				if !ok {
					r.resp <- GetResponse{Err: fmt.Errorf("session not found: %s", r.ID)}
				} else {
					r.resp <- GetResponse{Client: s.client}
				}
			case CloseRequest:
				s, ok := sessions[r.ID]
				if ok {
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
					slog.Debug("ssh session closed", "id", r.ID, "machine", s.Machine)
				}
				r.resp <- nil
			case ListRequest:
				out := make([]SessionInfo, 0, len(sessions))
				for _, s := range sessions {
					info := SessionInfo{ID: s.ID, Machine: s.Machine, CreatedAt: s.CreatedAt}
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

			case UploadRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- UploadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}
				uploadID := fmt.Sprintf("upload-%d", time.Now().UnixNano())
				uCtx, uCancel := context.WithCancel(ctx)
				job := &UploadJob{
					ID:         uploadID,
					LocalPath:  r.LocalPath,
					RemotePath: r.RemotePath,
					cancel:     uCancel,
				}
				s.uploads[uploadID] = job
				go runUpload(uCtx, s.client, job)
				r.resp <- UploadResponse{UploadID: uploadID}

			case UploadStatusRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- UploadStatusResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}
				job, ok := s.uploads[r.UploadID]
				if !ok {
					r.resp <- UploadStatusResponse{Err: fmt.Errorf("upload not found: %s", r.UploadID)}
					continue
				}
				job.mu.Lock()
				percent := float64(0)
				if job.TotalBytes > 0 {
					percent = (float64(job.BytesUploaded) / float64(job.TotalBytes)) * 100
				} else if job.Done {
					percent = 100
				}
				r.resp <- UploadStatusResponse{
					UploadID:      job.ID,
					BytesUploaded: job.BytesUploaded,
					TotalBytes:    job.TotalBytes,
					Percent:       percent,
					Done:          job.Done,
					Err:           job.Err,
				}
				job.mu.Unlock()

			case DownloadRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- DownloadResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}
				downloadID := fmt.Sprintf("download-%d", time.Now().UnixNano())
				dCtx, dCancel := context.WithCancel(ctx)
				job := &DownloadJob{
					ID:         downloadID,
					LocalPath:  r.LocalPath,
					RemotePath: r.RemotePath,
					cancel:     dCancel,
				}
				s.downloads[downloadID] = job
				go runDownload(dCtx, s.client, job)
				r.resp <- DownloadResponse{DownloadID: downloadID}

			case DownloadStatusRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- DownloadStatusResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}
				job, ok := s.downloads[r.DownloadID]
				if !ok {
					r.resp <- DownloadStatusResponse{Err: fmt.Errorf("download not found: %s", r.DownloadID)}
					continue
				}
				job.mu.Lock()
				percent := float64(0)
				if job.TotalBytes > 0 {
					percent = (float64(job.BytesDownloaded) / float64(job.TotalBytes)) * 100
				} else if job.Done {
					percent = 100
				}
				r.resp <- DownloadStatusResponse{
					DownloadID:      job.ID,
					BytesDownloaded: job.BytesDownloaded,
					TotalBytes:      job.TotalBytes,
					Percent:         percent,
					Done:            job.Done,
					Err:             job.Err,
				}
				job.mu.Unlock()

			case RegisterSpoolRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
					continue
				}
				s.spools[r.SpoolID] = r.Path
				r.resp <- nil

			case GetSpoolRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- GetSpoolResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}
				path, ok := s.spools[r.SpoolID]
				if !ok {
					r.resp <- GetSpoolResponse{Err: fmt.Errorf("spool ID not found: %s", r.SpoolID)}
					continue
				}
				r.resp <- GetSpoolResponse{Path: path}

			case DeleteSpoolRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- fmt.Errorf("session not found: %s", r.SessionID)
					continue
				}
				path, ok := s.spools[r.SpoolID]
				if !ok {
					r.resp <- fmt.Errorf("spool ID not found: %s", r.SpoolID)
					continue
				}
				_ = os.Remove(path)
				delete(s.spools, r.SpoolID)
				r.resp <- nil

			case ExecRequest:
				s, ok := sessions[r.SessionID]
				if !ok {
					r.resp <- ExecResponse{Err: fmt.Errorf("session not found: %s", r.SessionID)}
					continue
				}

				// Run in background so we don't block the event loop
				go p.executeCommand(ctx, s.client, r)
			}
		}
	}
}

type SpoolingBuffer struct {
	mu        sync.Mutex
	sessionID string
	spoolID   string
	threshold int64
	buf       bytes.Buffer
	file      *os.File
	filePath  string
	size      int64
	spilled   bool
	err       error
}

func NewSpoolingBuffer(sessionID string, threshold int64) *SpoolingBuffer {
	return &SpoolingBuffer{
		sessionID: sessionID,
		spoolID:   generateSpoolID(),
		threshold: threshold,
	}
}

func generateSpoolID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (b *SpoolingBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.err != nil {
		return 0, b.err
	}

	n = len(p)
	b.size += int64(n)

	if b.spilled {
		var nw int
		nw, err = b.file.Write(p)
		if err != nil {
			b.err = err
			return nw, err
		}
		return n, nil
	}

	if b.size > b.threshold {
		dir := filepath.Join(os.TempDir(), "ssh-mcp", "sessions", b.sessionID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.err = fmt.Errorf("creating session spool directory: %w", err)
			return 0, b.err
		}

		path := filepath.Join(dir, b.spoolID+".out")
		//nolint:gosec // path is dynamically generated securely
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			b.err = fmt.Errorf("creating spool file: %w", err)
			return 0, b.err
		}
		b.file = f
		b.filePath = path
		b.spilled = true

		if b.buf.Len() > 0 {
			if _, err := b.file.Write(b.buf.Bytes()); err != nil {
				_ = b.file.Close()
				b.file = nil
				b.err = fmt.Errorf("writing buffer to spool file: %w", err)
				return 0, b.err
			}
		}

		var nw int
		nw, err = b.file.Write(p)
		if err != nil {
			_ = b.file.Close()
			b.file = nil
			b.err = fmt.Errorf("writing to spool file: %w", err)
			return nw, err
		}
		return n, nil
	}

	_, err = b.buf.Write(p)
	if err != nil {
		b.err = err
		return 0, err
	}
	return n, nil
}

func (b *SpoolingBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.file != nil {
		err := b.file.Close()
		b.file = nil
		return err
	}
	return nil
}

func (b *SpoolingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *SpoolingBuffer) Spilled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spilled
}

func (b *SpoolingBuffer) SpoolID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spoolID
}

func (b *SpoolingBuffer) FilePath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.filePath
}

func (b *SpoolingBuffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

func mapSpoolID(b *SpoolingBuffer) string {
	if b.Spilled() {
		return b.SpoolID()
	}
	return ""
}

func (p *Pool) GetSpool(ctx context.Context, sessionID, spoolID string) (string, error) {
	respCh := make(chan GetSpoolResponse, 1)
	select {
	case p.reqCh <- GetSpoolRequest{SessionID: sessionID, SpoolID: spoolID, resp: respCh}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp.Path, resp.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Pool) DeleteSpool(ctx context.Context, sessionID, spoolID string) error {
	respCh := make(chan error, 1)
	select {
	case p.reqCh <- DeleteSpoolRequest{SessionID: sessionID, SpoolID: spoolID, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) RegisterSpool(ctx context.Context, sessionID, spoolID, path string) error {
	respCh := make(chan error, 1)
	select {
	case p.reqCh <- RegisterSpoolRequest{SessionID: sessionID, SpoolID: spoolID, Path: path, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) executeCommand(ctx context.Context, client *ssh.Client, r ExecRequest) {
	start := time.Now()

	cmdText := r.Command
	if r.Description != "" {
		// Prevent command injection via newlines in the description
		desc := strings.ReplaceAll(r.Description, "\n", " ")
		desc = strings.ReplaceAll(desc, "\r", " ")
		cmdText += " # " + strings.ReplaceAll(desc, "#", "\\#")
	}

	full := cmdText
	if r.Cwd != "" {
		full = "cd " + shellquote.Join(r.Cwd) + " && " + cmdText
	}
	if r.Sudo {
		if r.SudoPassword != "" {
			// -S reads password from stdin; -p "" suppresses the prompt.
			// Password is delivered via sess.Stdin, keeping it out of the process list.
			full = "sudo -S -p \"\" -- sh -c " + shellquote.Join(full)
		} else {
			// -n: fail immediately if a password is required (passwordless sudo only).
			full = "sudo -n -- sh -c " + shellquote.Join(full)
		}
	}

	slog.DebugContext(ctx, "ssh run start", "command", full)

	type sessionResult struct {
		sess *ssh.Session
		err  error
	}
	sessCh := make(chan sessionResult, 1)
	go func() {
		sess, err := client.NewSession()
		sessCh <- sessionResult{sess, err}
	}()

	var sess *ssh.Session
	select {
	case <-r.cancel:
		slog.DebugContext(ctx, "ssh run canceled by handler during NewSession", "duration", time.Since(start))
		r.resp <- ExecResponse{Err: fmt.Errorf("handler timeout during NewSession")}
		go func() {
			if res := <-sessCh; res.err == nil {
				_ = res.sess.Close()
			}
		}()
		return
	case <-ctx.Done():
		r.resp <- ExecResponse{Err: ctx.Err()}
		go func() {
			if res := <-sessCh; res.err == nil {
				_ = res.sess.Close()
			}
		}()
		return
	case res := <-sessCh:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				res.err = fmt.Errorf("SSH session disconnected (EOF)")
			} else {
				res.err = fmt.Errorf("creating session: %w", res.err)
			}
			r.resp <- ExecResponse{Err: res.err}
			return
		}
		sess = res.sess
	}

	stdout := NewSpoolingBuffer(r.SessionID, p.maxOutputBytes)
	stderr := NewSpoolingBuffer(r.SessionID, p.maxOutputBytes)
	sess.Stdout = stdout
	sess.Stderr = stderr
	if r.Sudo && r.SudoPassword != "" {
		sess.Stdin = strings.NewReader(r.SudoPassword + "\n")
	}

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(full)
	}()

	select {
	case <-r.cancel:
		out, errOut := stdout.String(), stderr.String()
		slog.DebugContext(ctx, "ssh run canceled by handler",
			"duration", time.Since(start),
			"stdout_len", len(out),
			"stderr_len", len(errOut),
		)
		r.resp <- ExecResponse{
			Stdout:        out,
			Stderr:        errOut,
			StdoutSize:    stdout.Size(),
			StderrSize:    stderr.Size(),
			StdoutSpoolID: mapSpoolID(stdout),
			StderrSpoolID: mapSpoolID(stderr),
		}
		go func() {
			abortSess, err := client.NewSession()
			if err == nil {
				abortCmd := "timeout 3s pkill -f " + shellquote.Join(regexp.QuoteMeta(cmdText)) + " 2>/dev/null || true"
				_ = abortSess.Run(abortCmd)
				_ = abortSess.Close()
			}
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
			<-done
			_ = stdout.Close()
			_ = stderr.Close()
			if stdout.Spilled() {
				_ = p.RegisterSpool(context.Background(), r.SessionID, stdout.SpoolID(), stdout.FilePath())
			}
			if stderr.Spilled() {
				_ = p.RegisterSpool(context.Background(), r.SessionID, stderr.SpoolID(), stderr.FilePath())
			}
		}()
		return
	case <-ctx.Done():
		out, errOut := stdout.String(), stderr.String()
		slog.DebugContext(ctx, "ssh run canceled by context",
			"err", ctx.Err(),
			"duration", time.Since(start),
			"stdout_len", len(out),
			"stderr_len", len(errOut),
		)
		r.resp <- ExecResponse{
			Stdout:        out,
			Stderr:        errOut,
			StdoutSize:    stdout.Size(),
			StderrSize:    stderr.Size(),
			StdoutSpoolID: mapSpoolID(stdout),
			StderrSpoolID: mapSpoolID(stderr),
			Err:           ctx.Err(),
		}
		go func() {
			abortSess, err := client.NewSession()
			if err == nil {
				abortCmd := "timeout 3s pkill -f " + shellquote.Join(regexp.QuoteMeta(cmdText)) + " 2>/dev/null || true"
				_ = abortSess.Run(abortCmd)
				_ = abortSess.Close()
			}
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
			<-done
			_ = stdout.Close()
			_ = stderr.Close()
			if stdout.Spilled() {
				_ = p.RegisterSpool(context.Background(), r.SessionID, stdout.SpoolID(), stdout.FilePath())
			}
			if stderr.Spilled() {
				_ = p.RegisterSpool(context.Background(), r.SessionID, stderr.SpoolID(), stderr.FilePath())
			}
		}()
		return
	case err := <-done:
		_ = sess.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		if stdout.Spilled() {
			_ = p.RegisterSpool(context.Background(), r.SessionID, stdout.SpoolID(), stdout.FilePath())
		}
		if stderr.Spilled() {
			_ = p.RegisterSpool(context.Background(), r.SessionID, stderr.SpoolID(), stderr.FilePath())
		}

		res := ExecResponse{
			Stdout:        stdout.String(),
			Stderr:        stderr.String(),
			StdoutSize:    stdout.Size(),
			StderrSize:    stderr.Size(),
			StdoutSpoolID: mapSpoolID(stdout),
			StderrSpoolID: mapSpoolID(stderr),
		}
		dur := time.Since(start)
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				res.ExitCode = exitErr.ExitStatus()
				slog.DebugContext(ctx, "ssh run exited",
					"exit_code", res.ExitCode,
					"duration", dur,
					"stdout_len", len(res.Stdout),
					"stderr_len", len(res.Stderr),
				)
			} else {
				if errors.Is(err, io.EOF) {
					res.Err = fmt.Errorf("SSH session disconnected (EOF)")
				} else {
					res.Err = err
				}
				slog.DebugContext(ctx, "ssh run error", "err", err, "duration", dur)
			}
		} else {
			slog.DebugContext(ctx, "ssh run success",
				"duration", dur,
				"stdout_len", len(res.Stdout),
				"stderr_len", len(res.Stderr),
			)
		}
		r.resp <- res
	}
}

func (p *Pool) Exec(ctx context.Context, r ExecRequest) ExecResponse {
	respCh := make(chan ExecResponse, 1)
	r.resp = respCh

	cancelCh := make(chan struct{})
	r.cancel = cancelCh

	select {
	case p.reqCh <- r:
	case <-ctx.Done():
		return ExecResponse{Err: ctx.Err()}
	}

	select {
	case res := <-respCh:
		return res
	case <-ctx.Done():
		ctxErr := ctx.Err()
		close(cancelCh)
		res := <-respCh
		if res.Err == nil {
			res.Err = ctxErr
		}
		return res
	}
}

func (p *Pool) Open(ctx context.Context, machine string) (string, error) {
	return p.OpenCfg(ctx, Config{Machine: machine})
}

func (p *Pool) OpenCfg(ctx context.Context, cfg Config) (string, error) {
	respCh := make(chan OpenResponse, 1)
	select {
	case p.reqCh <- OpenRequest{Config: cfg, resp: respCh}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp.ID, resp.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Pool) Close(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case p.reqCh <- CloseRequest{ID: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) Get(ctx context.Context, id string) (*ssh.Client, error) {
	respCh := make(chan GetResponse, 1)
	select {
	case p.reqCh <- GetRequest{ID: id, resp: respCh}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp.Client, resp.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Pool) List(ctx context.Context) ([]SessionInfo, error) {
	respCh := make(chan []SessionInfo, 1)
	select {
	case p.reqCh <- ListRequest{resp: respCh}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Pool) Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error) {
	respCh := make(chan UploadResponse, 1)
	select {
	case p.reqCh <- UploadRequest{SessionID: sessionID, LocalPath: localPath, RemotePath: remotePath, resp: respCh}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp.UploadID, resp.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Pool) UploadStatus(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error) {
	respCh := make(chan UploadStatusResponse, 1)
	select {
	case p.reqCh <- UploadStatusRequest{SessionID: sessionID, UploadID: uploadID, resp: respCh}:
	case <-ctx.Done():
		return UploadStatusResponse{}, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, resp.Err
	case <-ctx.Done():
		return UploadStatusResponse{}, ctx.Err()
	}
}

func (p *Pool) Download(ctx context.Context, sessionID, remotePath, localPath string) (string, error) {
	respCh := make(chan DownloadResponse, 1)
	select {
	case p.reqCh <- DownloadRequest{SessionID: sessionID, RemotePath: remotePath, LocalPath: localPath, resp: respCh}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp.DownloadID, resp.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Pool) DownloadStatus(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error) {
	respCh := make(chan DownloadStatusResponse, 1)
	select {
	case p.reqCh <- DownloadStatusRequest{SessionID: sessionID, DownloadID: downloadID, resp: respCh}:
	case <-ctx.Done():
		return DownloadStatusResponse{}, ctx.Err()
	}
	select {
	case resp := <-respCh:
		return resp, resp.Err
	case <-ctx.Done():
		return DownloadStatusResponse{}, ctx.Err()
	}
}

func generateSessionID(machine string, sessions map[string]*Session) string {
	slug := machineSlug(machine)
	for range 100 {
		id := fmt.Sprintf("%s-%s-%s", slug, randomAdjective(), randomSurname())
		if _, ok := sessions[id]; !ok {
			return id
		}
	}
	return fmt.Sprintf("%s-%d", slug, time.Now().UnixNano())
}

var adjectives = []string{
	"cool", "silly", "brave", "happy", "clever", "eager", "funny", "gentle",
	"jolly", "kind", "lively", "nice", "proud", "quiet", "witty", "young",
	"zany", "fancy", "mighty", "swift", "calm", "bold", "wise", "merry",
	"plucky", "spry", "zesty", "quirky", "jovial", "vibrant",
}

var surnames = []string{
	"einstein", "newton", "darwin", "curie", "tesla", "hopper", "lovelace",
	"turing", "galileo", "kepler", "pasteur", "nobel", "bohr", "fermi",
	"feynman", "hawking", "torvalds", "knuth", "dijkstra", "musk", "neumann",
	"oppenheimer", "shannon", "babbage", "ellis", "carver", "cerf", "kahn", "ritchie",
	"pike", "postel", "keller",
}

func machineSlug(m string) string {
	// strip user@ prefix and :port suffix
	if at := strings.LastIndex(m, "@"); at != -1 {
		m = m[at+1:]
	}
	if idx := strings.LastIndex(m, ":"); idx != -1 {
		m = m[:idx]
	}
	m = strings.ToLower(strings.TrimSpace(m))

	var b strings.Builder
	for _, r := range m {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "host"
	}
	return s
}

func randomAdjective() string {
	return adjectives[randomIndex(len(adjectives))]
}

func randomSurname() string {
	return surnames[randomIndex(len(surnames))]
}

func randomIndex(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

type poolProgressReader struct {
	r   io.Reader
	ctx context.Context
	job *UploadJob
}

func (pr *poolProgressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pr.r.Read(p)
	pr.job.mu.Lock()
	pr.job.BytesUploaded += int64(n)
	pr.job.mu.Unlock()
	return n, err
}

func runUpload(ctx context.Context, client *ssh.Client, job *UploadJob) {
	defer func() {
		job.mu.Lock()
		job.Done = true
		job.mu.Unlock()
	}()

	if err := func() error {
		src, err := os.Open(job.LocalPath)
		if err != nil {
			return err
		}
		defer func() {
			_ = src.Close()
		}()

		stat, err := src.Stat()
		if err != nil {
			return err
		}

		job.mu.Lock()
		job.TotalBytes = stat.Size()
		job.mu.Unlock()

		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			return err
		}
		defer func() {
			_ = sftpClient.Close()
		}()

		dst, err := sftpClient.Create(job.RemotePath)
		if err != nil {
			return err
		}
		defer func() {
			_ = dst.Close()
		}()

		pr := &poolProgressReader{r: src, ctx: ctx, job: job}
		if _, err := io.Copy(dst, pr); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		job.mu.Lock()
		job.Err = err
		job.mu.Unlock()
		return
	}
}

type poolDownloadProgressReader struct {
	r   io.Reader
	ctx context.Context
	job *DownloadJob
}

func (pr *poolDownloadProgressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pr.r.Read(p)
	pr.job.mu.Lock()
	pr.job.BytesDownloaded += int64(n)
	pr.job.mu.Unlock()
	return n, err
}

func runDownload(ctx context.Context, client *ssh.Client, job *DownloadJob) {
	defer func() {
		job.mu.Lock()
		job.Done = true
		job.mu.Unlock()
	}()

	if err := func() error {
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			return err
		}
		defer func() {
			_ = sftpClient.Close()
		}()

		src, err := sftpClient.Open(job.RemotePath)
		if err != nil {
			return err
		}
		defer func() {
			_ = src.Close()
		}()

		stat, err := src.Stat()
		if err != nil {
			return err
		}

		job.mu.Lock()
		job.TotalBytes = stat.Size()
		job.mu.Unlock()

		tmpPath := job.LocalPath + ".tmp"
		//nolint:gosec // LocalPath is validated to be within the allowed directory by withinDir
		dst, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		defer func() {
			_ = dst.Close()
			_ = os.Remove(tmpPath) // cleans up partial file if not renamed
		}()

		pr := &poolDownloadProgressReader{r: src, ctx: ctx, job: job}
		if _, err := io.Copy(dst, pr); err != nil {
			return err
		}

		if err := dst.Close(); err != nil {
			return err
		}

		if err := os.Rename(tmpPath, job.LocalPath); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		job.mu.Lock()
		job.Err = err
		job.mu.Unlock()
		return
	}
}

func (p *Pool) SFTP(ctx context.Context, id string) (*sftp.Client, error) {
	client, err := p.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return sftp.NewClient(client)
}
