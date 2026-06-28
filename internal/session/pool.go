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

	ctx    context.Context
	cancel context.CancelFunc

	userAgent string
	banner    string
	platform  string
}

type UploadJob struct {
	ID            string
	LocalPath     string
	RemotePath    string
	TotalBytes    int64
	BytesUploaded int64
	StartedAt     time.Time
	LastStatusAt  time.Time
	LastStatus    int64
	Done          bool
	Err           error
	mu            sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
}

type DownloadJob struct {
	ID              string
	LocalPath       string
	RemotePath      string
	TotalBytes      int64
	BytesDownloaded int64
	StartedAt       time.Time
	LastStatusAt    time.Time
	LastStatus      int64
	Done            bool
	Err             error
	mu              sync.Mutex
	cancel          context.CancelFunc
	done            chan struct{}
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
	reqCh          chan Request
	commandTimeout time.Duration
	maxOutputBytes int64
	logger         *slog.Logger
	homeDir        string
	onDisconnect   func(machine string, err error)
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
}

func NewPool(opts PoolOptions) *Pool {
	opts.setDefaults()
	return &Pool{
		reqCh:          make(chan Request),
		commandTimeout: opts.CommandTimeout,
		maxOutputBytes: opts.MaxOutputBytes,
		logger:         opts.Logger,
		homeDir:        opts.HomeDir,
		onDisconnect:   opts.OnDisconnect,
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

	if !strings.Contains(string(client.ServerVersion()), "OpenSSH") {
		return time.Since(start), nil
	}

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
			machine := p.disconnectMachine(ctx, id)
			_ = p.Close(ctx, id)
			p.notifyDisconnect(machine, fmt.Errorf("keepalive failed: %w", err))
			return 0, err
		}
		return time.Since(start), nil
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

func (p *Pool) RunLoop(ctx context.Context) {
	sessions := make(map[string]*Session)
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
				p.logger.Debug("ssh session closed (shutdown)", "id", s.ID, "machine", s.Machine)
			}
			return
		case res := <-dialCh:
			p.handleDialResult(ctx, sessions, res)
		case req := <-p.reqCh:
			switch r := req.(type) {
			case OpenRequest:
				p.handleOpen(ctx, dialCh, r)
			case GetRequest:
				p.handleGet(sessions, r)
			case CloseRequest:
				p.handleClose(sessions, r)
			case ListRequest:
				p.handleList(sessions, r)
			case UploadRequest:
				p.handleUpload(sessions, r)
			case UploadStatusRequest:
				p.handleUploadStatus(sessions, r)
			case UploadWaitRequest:
				p.handleUploadWait(sessions, r)
			case UploadCancelRequest:
				p.handleUploadCancel(sessions, r)
			case DownloadRequest:
				p.handleDownload(sessions, r)
			case DownloadStatusRequest:
				p.handleDownloadStatus(sessions, r)
			case DownloadWaitRequest:
				p.handleDownloadWait(sessions, r)
			case DownloadCancelRequest:
				p.handleDownloadCancel(sessions, r)
			case RegisterSpoolRequest:
				p.handleRegisterSpool(sessions, r)
			case GetSpoolRequest:
				p.handleGetSpool(sessions, r)
			case DeleteSpoolRequest:
				p.handleDeleteSpool(sessions, r)
			case MachineRequest:
				p.handleMachine(sessions, r)
			case ExecRequest:
				p.handleExec(sessions, r)
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
		if b.file == nil {
			b.err = fmt.Errorf("spooling buffer closed")
			return 0, b.err
		}
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
		sess.Stdin = strings.NewReader(r.SudoPassword + "\n")
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
		go p.cleanupExec(ctx, client, sess, r, cmdText, done, stdout, stderr)
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
		go p.cleanupExec(context.Background(), client, sess, r, cmdText, done, stdout, stderr)
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
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
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

func (p *Pool) cleanupExec(spoolCtx context.Context, client *ssh.Client, sess *ssh.Session, r ExecRequest, cmdText string, done <-chan error, stdout, stderr *SpoolingBuffer) {
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()

	type abortRes struct{}
	abortDone := make(chan abortRes, 1)
	go func() {
		abortSess, err := client.NewSession()
		if err == nil {
			abortCmd := "timeout 3s pkill -f " + shellquote.Join(regexp.QuoteMeta(cmdText)) + " 2>/dev/null || true"
			_ = abortSess.Run(abortCmd)
			_ = abortSess.Close()
		}
		abortDone <- abortRes{}
	}()

	select {
	case <-killCtx.Done():
		_ = client.Close()
	case <-abortDone:
	}

	_ = sess.Signal(ssh.SIGKILL)
	_ = sess.Close()

	select {
	case <-killCtx.Done():
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
	respCh := make(chan error, 1)
	err, ok := send(ctx, p.reqCh, CloseRequest{ID: id, resp: respCh}, respCh)
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
		close(job.done)
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

		sftpClient, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true))
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

		pr := &poolProgressReader{r: src, ctx: ctx, job: job}
		if _, err := io.Copy(dst, pr); err != nil {
			_ = dst.Close()
			_ = sftpClient.Remove(job.RemotePath)
			return err
		}
		if err := dst.Close(); err != nil {
			_ = sftpClient.Remove(job.RemotePath)
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
		close(job.done)
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
