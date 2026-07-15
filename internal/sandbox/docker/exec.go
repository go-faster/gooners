package docker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/sandbox/streamconn"
)

// Dial starts the sandbox agent inside id, over `docker exec`, and returns
// its stdio as a net.Conn. Dial knows nothing about SSH: the caller
// (internal/sandbox.Manager) drives the handshake over the returned conn.
func (r *Runner) Dial(ctx context.Context, id string) (net.Conn, error) {
	execRes, err := r.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          []string{agent.DefaultPath},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		// TTY must be false: a pty does LF->CRLF translation and interprets
		// ^C/^D, which would silently corrupt binary SSH traffic.
		TTY: false,
	})
	if err != nil {
		return nil, errors.Wrap(err, "create sandbox agent exec")
	}

	// ExecAttach implies start; ExecStart must never be called afterwards.
	att, err := r.cli.ExecAttach(ctx, execRes.ID, client.ExecAttachOptions{TTY: false})
	if err != nil {
		return nil, errors.Wrap(err, "attach to sandbox agent exec")
	}

	// stdin (client -> container) is raw and unframed: write straight to
	// att.Conn. stdout+stderr (container -> client) are stdcopy-framed and
	// must be demultiplexed - att.Reader, never att.Conn directly, since
	// att.Reader is the *bufio.Reader that may already hold buffered bytes.
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = pw.Close() }()
		stderr := &stderrLogWriter{logger: r.logger, containerID: id}
		if _, err := stdcopy.StdCopy(pw, stderr, att.Reader); err != nil {
			r.logger.Debug("sandbox exec stream demux ended", "id", id, "err", err)
		}
	}()

	conn := streamconn.New(pr, att.Conn, streamconn.Options{
		Close: func() error {
			// Close the pipe read end BEFORE the hijacked conn. This order
			// guarantees the StdCopy goroutine above always exits (first
			// unblocking any pending pipe Write, then unblocking the
			// goroutine's blocking Read via the hijacked conn closing),
			// instead of leaking one goroutine per sandbox.
			_ = pr.Close()
			att.Close()
			<-done
			return nil
		},
	})
	return conn, nil
}

// stderrLogWriter drains an exec's stderr stream into slog. If nobody reads
// stderr, the whole multiplexed stdcopy stream stalls - even a single log
// line the agent writes there would otherwise wedge every SSH tool.
type stderrLogWriter struct {
	logger      *slog.Logger
	containerID string
}

func (w *stderrLogWriter) Write(p []byte) (int, error) {
	w.logger.Warn("sandbox agent stderr", "container", w.containerID, "output", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
