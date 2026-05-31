package sshutil

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/kballard/go-shellquote"
	"golang.org/x/crypto/ssh"
)

type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

func Run(ctx context.Context, client *ssh.Client, command string) (Result, error) {
	sess, err := client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close() //nolint:errcheck // session close error not actionable on defer path

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(command)
	}()

	select {
	case <-ctx.Done():
		//nolint:errcheck // close on context cancel, error not actionable
		sess.Close()
		return Result{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}, ctx.Err()
	case err := <-done:
		res := Result{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}
		if err != nil {
			if e, ok := err.(*ssh.ExitError); ok {
				res.ExitCode = e.ExitStatus()
			} else {
				return res, err
			}
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
