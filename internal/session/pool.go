package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	lastPing  atomic.Int64      // unix nanoseconds of last successful keepalive; 0 = no ping yet
	spools    map[string]string // spoolID -> localFilePath
	forwards  []io.Closer

	ctx    context.Context
	cancel context.CancelCauseFunc

	userAgent string
	banner    string
	platform  string
}

type OpenResult struct {
	ID        string
	UserAgent string
	Banner    string
	Platform  string
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Machine   string    `json:"machine"`
	CreatedAt time.Time `json:"created_at"`
	LastPing  time.Time `json:"last_ping,omitzero"`
	// Status is "connected" if a keepalive succeeded within the last 30s, "new" if no ping yet, "stale" otherwise.
	Status    string `json:"status"`
	UserAgent string `json:"user_agent,omitempty"`
	Banner    string `json:"banner,omitempty"`
	Platform  string `json:"platform,omitempty" jsonschema:"Detected OS platform (may be imprecise)"`
}

type Provider interface {
	Get(ctx context.Context, id string) (*ssh.Client, error)
	SFTP(ctx context.Context, id string) (*sftp.Client, error)
	Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error)
	UploadStatus(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error)
	UploadWait(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error)
	UploadCancel(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error)
	Download(ctx context.Context, sessionID, remotePath, localPath string) (string, error)
	DownloadStatus(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error)
	DownloadWait(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error)
	DownloadCancel(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error)
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
	reqCh        chan Request
	onDisconnect func(machine string, err error)

	commandTimeout    time.Duration
	maxOutputBytes    int64
	homeDir           string
	keepaliveInterval time.Duration
	keepaliveTimeout  time.Duration
	jobRetention      time.Duration

	logger *slog.Logger
}

// PoolOptions contains configuration for a new Pool.
type PoolOptions struct {
	CommandTimeout time.Duration
	MaxOutputBytes int64
	Logger         *slog.Logger
	// HomeDir overrides the home directory used to resolve ~/.ssh/config,
	// ~/.ssh/known_hosts, and identity keys for all sessions in this pool.
	// Defaults to the process home directory if empty.
	HomeDir string
	// OnDisconnect is invoked when a session is closed.
	OnDisconnect func(machine string, err error)
	// KeepaliveInterval is how often an open session is probed. Defaults to 15s.
	KeepaliveInterval time.Duration
	// KeepaliveTimeout bounds a single probe. A stalled connection never answers and
	// never fails, so an unbounded probe would hide the disconnect forever.
	// Defaults to 15s.
	KeepaliveTimeout time.Duration
	// JobRetention is how long a finished transfer job stays queryable after its
	// session goes away. Defaults to 1h.
	JobRetention time.Duration
}

func (opts *PoolOptions) setDefaults() {
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 10 * time.Second
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = 8192 // default 8KB
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.KeepaliveInterval <= 0 {
		opts.KeepaliveInterval = 15 * time.Second
	}
	if opts.KeepaliveTimeout <= 0 {
		opts.KeepaliveTimeout = 15 * time.Second
	}
	if opts.JobRetention <= 0 {
		opts.JobRetention = time.Hour
	}
}

func NewPool(opts PoolOptions) *Pool {
	opts.setDefaults()
	return &Pool{
		reqCh:        make(chan Request),
		onDisconnect: opts.OnDisconnect,

		commandTimeout:    opts.CommandTimeout,
		maxOutputBytes:    opts.MaxOutputBytes,
		homeDir:           opts.HomeDir,
		keepaliveInterval: opts.KeepaliveInterval,
		keepaliveTimeout:  opts.KeepaliveTimeout,
		jobRetention:      opts.JobRetention,

		logger: opts.Logger,
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
	lg := p.logger.With("session_id", id)

	serverVersion := string(client.ServerVersion())
	if !strings.Contains(serverVersion, "OpenSSH") {
		lg.DebugContext(ctx, "ssh ping: non-OpenSSH server, skipping keepalive", "server_version", serverVersion)
		p.touchLastPing(ctx, id)
		return time.Since(start), nil
	}

	errCh := make(chan error, 1)
	go func() {
		lg.DebugContext(ctx, "ssh ping: sending keepalive request")
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case err := <-errCh:
		lg.DebugContext(ctx, "ssh ping: keepalive response received", "err", err)
		if err != nil {
			p.dropSession(ctx, id, fmt.Errorf("keepalive failed: %w", err))
			return 0, err
		}
		took := time.Since(start)
		lg.DebugContext(ctx, "ssh ping: keepalive succeeded", "took", took)
		p.touchLastPing(ctx, id)
		return took, nil
	}
}

// dropSession tears down a session whose connection is gone and tells the caller why.
// Any transfer still running on it ends as failed with cause, so a poller learns what
// happened instead of hanging or getting "session not found".
func (p *Pool) dropSession(ctx context.Context, id string, cause error) {
	machine := p.disconnectMachine(ctx, id)
	_ = p.closeWithCause(ctx, id, cause)
	p.notifyDisconnect(machine, cause)
}

func (p *Pool) touchLastPing(ctx context.Context, id string) {
	respCh := make(chan error, 1)
	if err, ok := send(ctx, p.reqCh, TouchRequest{SessionID: id, At: time.Now(), resp: respCh}, respCh); ok {
		_ = err
	}
}

func (p *Pool) disconnectMachine(ctx context.Context, id string) string {
	if p.onDisconnect == nil {
		return ""
	}
	machine, _ := p.Machine(ctx, id)
	if machine == "" {
		machine = id
	}
	return machine
}

func (p *Pool) notifyDisconnect(machine string, err error) {
	if p.onDisconnect == nil {
		return
	}
	p.onDisconnect(machine, err)
}

func (p *Pool) RunLoop(ctx context.Context) {
	st := &poolState{
		sessions:  make(map[string]*Session),
		uploads:   make(map[string]*TransferJob),
		downloads: make(map[string]*TransferJob),
	}
	dialCh := make(chan dialResult)

	for {
		select {
		case <-ctx.Done():
			for _, s := range st.sessions {
				s.cancel(ErrSessionClosed)
				for _, path := range s.spools {
					_ = os.Remove(path)
				}
				closeAll(s.forwards)
				sessionDir := filepath.Join(os.TempDir(), "ssh-mcp", "sessions", s.ID)
				_ = os.RemoveAll(sessionDir)

				_ = s.client.Close()
				p.logger.Debug("ssh session closed (shutdown)", "id", s.ID, "machine", s.Machine)
			}
			return
		case res := <-dialCh:
			p.handleDialResult(ctx, st, res)
		case req := <-p.reqCh:
			switch r := req.(type) {
			case OpenRequest:
				p.handleOpen(ctx, dialCh, r)
			case GetRequest:
				p.handleGet(st, r)
			case CloseRequest:
				p.handleClose(st, r)
			case ListRequest:
				p.handleList(st, r)
			case UploadRequest:
				p.handleUpload(st, r)
			case UploadStatusRequest:
				p.handleUploadStatus(st, r)
			case UploadWaitRequest:
				p.handleUploadWait(st, r)
			case UploadCancelRequest:
				p.handleUploadCancel(st, r)
			case DownloadRequest:
				p.handleDownload(st, r)
			case DownloadStatusRequest:
				p.handleDownloadStatus(st, r)
			case DownloadWaitRequest:
				p.handleDownloadWait(st, r)
			case DownloadCancelRequest:
				p.handleDownloadCancel(st, r)
			case RegisterSpoolRequest:
				p.handleRegisterSpool(st, r)
			case GetSpoolRequest:
				p.handleGetSpool(st, r)
			case DeleteSpoolRequest:
				p.handleDeleteSpool(st, r)
			case MachineRequest:
				p.handleMachine(st, r)
			case TouchRequest:
				p.handleTouch(st, r)
			case ExecRequest:
				p.handleExec(st, r)
			}
		}
	}
}

func (p *Pool) Run(ctx context.Context, sessionID, cmd string) (sshutil.Result, error) {
	return p.RunWithOptions(ctx, sessionID, cmd, sshutil.RunOptions{
		Timeout: p.CommandTimeout(),
		Logger:  p.logger,
	})
}

func (p *Pool) RunWithOptions(ctx context.Context, sessionID, cmd string, opts sshutil.RunOptions) (sshutil.Result, error) {
	client, err := p.Get(ctx, sessionID)
	if err != nil {
		return sshutil.Result{}, err
	}
	if opts.Logger == nil {
		opts.Logger = p.logger
	}
	return sshutil.Run(ctx, client, cmd, opts)
}

func (p *Pool) GetSpool(ctx context.Context, sessionID, spoolID string) (string, error) {
	respCh := make(chan GetSpoolResponse, 1)
	resp, ok := send(ctx, p.reqCh, GetSpoolRequest{SessionID: sessionID, SpoolID: spoolID, resp: respCh}, respCh)
	if !ok {
		return "", ctx.Err()
	}
	return resp.Path, resp.Err
}

func (p *Pool) DeleteSpool(ctx context.Context, sessionID, spoolID string) error {
	respCh := make(chan error, 1)
	err, ok := send(ctx, p.reqCh, DeleteSpoolRequest{SessionID: sessionID, SpoolID: spoolID, resp: respCh}, respCh)
	if !ok {
		return ctx.Err()
	}
	return err
}

func (p *Pool) RegisterSpool(ctx context.Context, sessionID, spoolID, path string) error {
	p.logger.DebugContext(ctx, "registering spool file", "session_id", sessionID, "spool_id", spoolID, "path", path)

	respCh := make(chan error, 1)
	err, ok := send(ctx, p.reqCh, RegisterSpoolRequest{SessionID: sessionID, SpoolID: spoolID, Path: path, resp: respCh}, respCh)
	if !ok {
		return ctx.Err()
	}
	return err
}

func (p *Pool) executeCommand(ctx context.Context, client *ssh.Client, r ExecRequest) {
	start := time.Now()

	cmdText := r.Command
	if r.DescriptionComment && r.Description != "" {
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

	p.logger.DebugContext(ctx, "ssh run start", "command", full)

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
		p.logger.DebugContext(ctx, "ssh run canceled by handler during NewSession", "duration", time.Since(start))
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
		sess.Stdin = strings.NewReader(r.SudoPassword + "\n" + r.Stdin)
	} else if r.Stdin != "" {
		sess.Stdin = strings.NewReader(r.Stdin)
	}

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(full)
	}()

	select {
	case <-r.cancel:
		out, errOut := stdout.String(), stderr.String()
		p.logger.DebugContext(ctx, "ssh run canceled by handler",
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
		go p.cleanupExec(ctx, sess, r, cmdText, done, stdout, stderr)
		return
	case <-ctx.Done():
		out, errOut := stdout.String(), stderr.String()
		p.logger.DebugContext(ctx, "ssh run canceled by context",
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
		go p.cleanupExec(context.Background(), sess, r, cmdText, done, stdout, stderr)
		return
	case err := <-done:
		_ = sess.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		if stdout.Spilled() {
			_ = p.RegisterSpool(ctx, r.SessionID, stdout.SpoolID(), stdout.FilePath())
		}
		if stderr.Spilled() {
			_ = p.RegisterSpool(ctx, r.SessionID, stderr.SpoolID(), stderr.FilePath())
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
			if exitErr, ok := errors.AsType[*ssh.ExitError](err); ok {
				res.ExitCode = exitErr.ExitStatus()
				p.logger.DebugContext(ctx, "ssh run exited",
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
				p.logger.DebugContext(ctx, "ssh run error", "err", err, "duration", dur)
			}
		} else {
			p.logger.DebugContext(ctx, "ssh run success",
				"duration", dur,
				"stdout_len", len(res.Stdout),
				"stderr_len", len(res.Stderr),
			)
		}
		r.resp <- res
	}
}

func (p *Pool) cleanupExec(spoolCtx context.Context, sess *ssh.Session, r ExecRequest, cmdText string, done <-chan error, stdout, stderr *SpoolingBuffer) {
	lg := p.logger.With("session_id", r.SessionID, "command", cmdText)

	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()

	if err := sess.Signal(ssh.SIGKILL); err != nil {
		lg.DebugContext(spoolCtx, "ssh run: failed to send SIGKILL", "err", err)
	} else {
		lg.DebugContext(spoolCtx, "ssh run: SIGKILL sent")
	}
	_ = sess.Close()

	select {
	case <-killCtx.Done():
		lg.DebugContext(spoolCtx, "ssh run: cancel cleanup timed out")
	case <-done:
	}

	_ = stdout.Close()
	_ = stderr.Close()
	if stdout.Spilled() {
		_ = p.RegisterSpool(spoolCtx, r.SessionID, stdout.SpoolID(), stdout.FilePath())
	}
	if stderr.Spilled() {
		_ = p.RegisterSpool(spoolCtx, r.SessionID, stderr.SpoolID(), stderr.FilePath())
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

func (p *Pool) Open(ctx context.Context, machine string) (OpenResult, error) {
	return p.OpenCfg(ctx, Config{Machine: machine})
}

func (p *Pool) OpenCfg(ctx context.Context, cfg Config) (OpenResult, error) {
	if cfg.HomeDir == "" {
		cfg.HomeDir = p.homeDir
	}
	respCh := make(chan OpenResponse, 1)
	resp, ok := send(ctx, p.reqCh, OpenRequest{Config: cfg, resp: respCh}, respCh)
	if !ok {
		return OpenResult{}, ctx.Err()
	}
	if resp.Err != nil {
		return OpenResult{}, resp.Err
	}
	return OpenResult{
		ID:        resp.ID,
		UserAgent: resp.UserAgent,
		Banner:    resp.Banner,
		Platform:  resp.Platform,
	}, nil
}

func (p *Pool) Close(ctx context.Context, id string) error {
	return p.closeWithCause(ctx, id, ErrSessionClosed)
}

// closeWithCause closes a session and records cause as the reason its transfers ended.
func (p *Pool) closeWithCause(ctx context.Context, id string, cause error) error {
	respCh := make(chan error, 1)
	err, ok := send(ctx, p.reqCh, CloseRequest{ID: id, Cause: cause, resp: respCh}, respCh)
	if !ok {
		return ctx.Err()
	}
	return err
}

func (p *Pool) Get(ctx context.Context, id string) (*ssh.Client, error) {
	respCh := make(chan GetResponse, 1)
	resp, ok := send(ctx, p.reqCh, GetRequest{ID: id, resp: respCh}, respCh)
	if !ok {
		return nil, ctx.Err()
	}
	return resp.Client, resp.Err
}

func (p *Pool) Machine(ctx context.Context, id string) (string, error) {
	respCh := make(chan MachineResponse, 1)
	resp, ok := send(ctx, p.reqCh, MachineRequest{ID: id, resp: respCh}, respCh)
	if !ok {
		return "", ctx.Err()
	}
	return resp.Machine, resp.Err
}

func (p *Pool) List(ctx context.Context) ([]SessionInfo, error) {
	respCh := make(chan []SessionInfo, 1)
	resp, ok := send(ctx, p.reqCh, ListRequest{resp: respCh}, respCh)
	if !ok {
		return nil, ctx.Err()
	}
	return resp, nil
}

func (p *Pool) Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error) {
	respCh := make(chan UploadResponse, 1)
	resp, ok := send(ctx, p.reqCh, UploadRequest{SessionID: sessionID, LocalPath: localPath, RemotePath: remotePath, resp: respCh}, respCh)
	if !ok {
		return "", ctx.Err()
	}
	return resp.UploadID, resp.Err
}

func (p *Pool) UploadStatus(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error) {
	respCh := make(chan UploadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, UploadStatusRequest{SessionID: sessionID, UploadID: uploadID, resp: respCh}, respCh)
	if !ok {
		return UploadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

func (p *Pool) UploadWait(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error) {
	respCh := make(chan UploadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, UploadWaitRequest{Ctx: ctx, SessionID: sessionID, UploadID: uploadID, resp: respCh}, respCh)
	if !ok {
		return UploadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

func (p *Pool) UploadCancel(ctx context.Context, sessionID, uploadID string) (UploadStatusResponse, error) {
	respCh := make(chan UploadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, UploadCancelRequest{Ctx: ctx, SessionID: sessionID, UploadID: uploadID, resp: respCh}, respCh)
	if !ok {
		return UploadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

func (p *Pool) Download(ctx context.Context, sessionID, remotePath, localPath string) (string, error) {
	respCh := make(chan DownloadResponse, 1)
	resp, ok := send(ctx, p.reqCh, DownloadRequest{SessionID: sessionID, RemotePath: remotePath, LocalPath: localPath, resp: respCh}, respCh)
	if !ok {
		return "", ctx.Err()
	}
	return resp.DownloadID, resp.Err
}

func (p *Pool) DownloadStatus(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error) {
	respCh := make(chan DownloadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, DownloadStatusRequest{SessionID: sessionID, DownloadID: downloadID, resp: respCh}, respCh)
	if !ok {
		return DownloadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

func (p *Pool) DownloadWait(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error) {
	respCh := make(chan DownloadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, DownloadWaitRequest{Ctx: ctx, SessionID: sessionID, DownloadID: downloadID, resp: respCh}, respCh)
	if !ok {
		return DownloadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

func (p *Pool) DownloadCancel(ctx context.Context, sessionID, downloadID string) (DownloadStatusResponse, error) {
	respCh := make(chan DownloadStatusResponse, 1)
	resp, ok := send(ctx, p.reqCh, DownloadCancelRequest{Ctx: ctx, SessionID: sessionID, DownloadID: downloadID, resp: respCh}, respCh)
	if !ok {
		return DownloadStatusResponse{}, ctx.Err()
	}
	return resp, resp.Err
}

// send dispatches req to the pool's request channel and waits for a response.
// Returns (zero, false) if ctx is canceled during send or receive.
func send[Resp any](ctx context.Context, ch chan<- Request, req Request, respCh <-chan Resp) (Resp, bool) {
	select {
	case ch <- req:
	case <-ctx.Done():
		var zero Resp
		return zero, false
	}
	select {
	case resp := <-respCh:
		return resp, true
	case <-ctx.Done():
		var zero Resp
		return zero, false
	}
}
