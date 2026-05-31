// Package sshutil provides helpers for running commands over SSH connections.
package sshutil

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/kballard/go-shellquote"
	"golang.org/x/crypto/ssh"
)

type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
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

func Run(ctx context.Context, client *ssh.Client, command string) (Result, error) {
	start := time.Now()
	slog.DebugContext(ctx, "ssh run start", "command", command)

	sess, err := client.NewSession()
	if err != nil {
		slog.DebugContext(ctx, "ssh run session error", "err", err, "duration", time.Since(start))
		return Result{}, err
	}

	var stdout, stderr safeBuffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(command)
	}()

	select {
	case <-ctx.Done():
		// Best-effort termination: signal the remote process and close the
		// session. Run in background so we return promptly even if the
		// underlying channel is stuck (network partition, uninterruptible
		// remote process, etc.). The Run goroutine will exit once the SSH
		// library unblocks (or when the whole client conn is later closed).
		go func() {
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		}()
		out, errOut := stdout.String(), stderr.String()
		slog.DebugContext(ctx, "ssh run canceled",
			"err", ctx.Err(),
			"duration", time.Since(start),
			"stdout_len", len(out),
			"stderr_len", len(errOut),
		)
		return Result{
			Stdout: out,
			Stderr: errOut,
		}, ctx.Err()
	case err := <-done:
		// Happy path: Run has returned; close synchronously.
		_ = sess.Close()
		res := Result{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}
		dur := time.Since(start)
		if err != nil {
			if e, ok := err.(*ssh.ExitError); ok {
				res.ExitCode = e.ExitStatus()
				slog.DebugContext(ctx, "ssh run exited",
					"exit_code", res.ExitCode,
					"duration", dur,
					"stdout_len", len(res.Stdout),
					"stderr_len", len(res.Stderr),
				)
			} else {
				slog.DebugContext(ctx, "ssh run error", "err", err, "duration", dur)
				return res, err
			}
		} else {
			slog.DebugContext(ctx, "ssh run success",
				"duration", dur,
				"stdout_len", len(res.Stdout),
				"stderr_len", len(res.Stderr),
			)
		}
		return res, nil
	}
}

func (r Result) Text() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// Quote returns a shell-escaped version of s, safe to use as a single argument
// in a POSIX shell command (e.g. via ssh).
func Quote(s string) string {
	return shellquote.Join(s)
}
