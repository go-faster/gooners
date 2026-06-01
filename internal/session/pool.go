package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/kballard/go-shellquote"
	"golang.org/x/crypto/ssh"
)

type Session struct {
	ID        string
	Machine   string
	CreatedAt time.Time
	client    *ssh.Client
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Machine   string    `json:"machine"`
	CreatedAt time.Time `json:"created_at"`
}

type Provider interface {
	Get(ctx context.Context, id string) (*ssh.Client, error)
}

// Pool manages SSH sessions.
// Note: You must call Run(ctx) on the Pool before using it, otherwise
// methods like Open, Close, and Exec will deadlock waiting for the event loop.
// The event loop and all managed sessions are terminated when ctx is canceled.
type Pool struct {
	reqCh chan Request
}

func NewPool() *Pool {
	return &Pool{
		reqCh: make(chan Request),
	}
}

func (p *Pool) Run(ctx context.Context) {
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
			}
			sessions[id] = sess
			slog.Debug("ssh session opened", "id", id, "machine", res.req.Config.Machine)
			res.req.resp <- OpenResponse{ID: id}

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
					_ = s.client.Close()
					delete(sessions, r.ID)
					slog.Debug("ssh session closed", "id", r.ID, "machine", s.Machine)
				}
				r.resp <- nil
			case ListRequest:
				out := make([]SessionInfo, 0, len(sessions))
				for _, s := range sessions {
					out = append(out, SessionInfo{ID: s.ID, Machine: s.Machine, CreatedAt: s.CreatedAt})
				}
				r.resp <- out
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

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (p *Pool) executeCommand(ctx context.Context, client *ssh.Client, r ExecRequest) {
	start := time.Now()
	full := r.Command
	if r.Cwd != "" {
		full = "cd " + shellquote.Join(r.Cwd) + " && " + r.Command
	}
	if r.Sudo {
		full = "sudo -n -- sh -c " + shellquote.Join(full)
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
			r.resp <- ExecResponse{Err: res.err}
			return
		}
		sess = res.sess
	}

	var stdout, stderr safeBuffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

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
			Stdout: out,
			Stderr: errOut,
			Err:    fmt.Errorf("handler timeout"),
		}
		go func() {
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
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
			Stdout: out,
			Stderr: errOut,
			Err:    ctx.Err(),
		}
		go func() {
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		}()
		return
	case err := <-done:
		_ = sess.Close()
		res := ExecResponse{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
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
				res.Err = err
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
		close(cancelCh)
		return <-respCh
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
