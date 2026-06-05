// Package sshutil provides helpers for running commands over SSH connections.
package sshutil

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kballard/go-shellquote"
	"golang.org/x/crypto/ssh"
)

// DefaultTimeout is the default command execution timeout.
// Defaults to 10 seconds.
const DefaultTimeout = 10 * time.Second

type RunOptions struct {
	Timeout time.Duration
	Logger  *slog.Logger
}

func (opts *RunOptions) setDefaults() {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
}

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

func Run(ctx context.Context, client *ssh.Client, command string, opts RunOptions) (Result, error) {
	opts.setDefaults()

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	start := time.Now()
	opts.Logger.DebugContext(runCtx, "ssh run start", "command", command)

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
	case <-runCtx.Done():
		opts.Logger.DebugContext(runCtx, "ssh run canceled during NewSession", "err", runCtx.Err(), "duration", time.Since(start))
		go func() {
			if res := <-sessCh; res.err == nil {
				_ = res.sess.Close()
			}
		}()
		return Result{}, runCtx.Err()
	case res := <-sessCh:
		if res.err != nil {
			opts.Logger.DebugContext(runCtx, "ssh run session error", "err", res.err, "duration", time.Since(start))
			return Result{}, res.err
		}
		sess = res.sess
	}

	var stdout, stderr safeBuffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(command)
	}()

	select {
	case <-runCtx.Done():
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
		opts.Logger.DebugContext(runCtx, "ssh run canceled",
			"err", runCtx.Err(), "duration", time.Since(start),
			"stdout_len", len(out),
			"stderr_len", len(errOut),
		)
		return Result{
			Stdout: out,
			Stderr: errOut,
		}, runCtx.Err()
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
				opts.Logger.DebugContext(runCtx, "ssh run exited",
					"exit_code", res.ExitCode, "duration", dur,
					"stdout_len", len(res.Stdout),
					"stderr_len", len(res.Stderr),
				)
			} else {
				opts.Logger.DebugContext(runCtx, "ssh run error", "err", err, "duration", dur)
				return res, err
			}
		} else {
			opts.Logger.DebugContext(runCtx, "ssh run success",
				"duration", dur,
				"stdout_len", len(res.Stdout),
				"stderr_len", len(res.Stderr),
			)
		}
		return res, nil
	}
}

func (r Result) Text() string {
	if r.Error != "" {
		return "error: " + r.Error
	}
	if r.ExitCode != 0 {
		out := strings.TrimSpace(r.Stdout + r.Stderr)
		if out == "" {
			return fmt.Sprintf("exited with code %d", r.ExitCode)
		}
		return out
	}
	return r.Stdout
}

// Quote returns a shell-escaped version of s, safe to use as a single argument
// in a POSIX shell command (e.g. via ssh).
func Quote(s string) string {
	return shellquote.Join(s)
}
