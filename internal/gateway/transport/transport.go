// Package gatewaytransport builds mcp.Transport implementations from gateway config.
package gatewaytransport

import (
	"context"
	"net/http"
	"os/exec"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Build constructs a transport. resolve is called for {secret:NAME} in env/headers.
func Build(_ context.Context, kind string, command []string, url string, env, headers map[string]string, resolve func(string) (string, error)) (mcp.Transport, func() error, error) {
	switch kind {
	case "stdio":
		return buildStdio(command, env, resolve)
	case "http":
		return buildHTTP(url, headers, resolve)
	case "sse":
		return buildSSE(url, headers, resolve)
	default:
		return nil, nil, errors.Errorf("unknown upstream kind %q", kind)
	}
}

func buildStdio(command []string, env map[string]string, resolve func(string) (string, error)) (mcp.Transport, func() error, error) {
	if len(command) == 0 {
		return nil, nil, errors.New("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // G204: command comes from operator TOML config (stdio upstream)
	if len(env) > 0 {
		out := make([]string, 0, len(env))
		for k, v := range env {
			iv := v
			if resolve != nil {
				iv = mustResolve(v, resolve)
			}
			out = append(out, k+"="+iv)
		}
		cmd.Env = out
	}
	cleanup := func() error {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	return &mcp.CommandTransport{Command: cmd}, cleanup, nil
}

func buildHTTP(url string, headers map[string]string, resolve func(string) (string, error)) (mcp.Transport, func() error, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	if len(headers) > 0 {
		cl.Transport = &headerRT{base: http.DefaultTransport, headers: headers, resolve: resolve}
	}
	return &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: cl}, func() error { return nil }, nil
}

func buildSSE(url string, headers map[string]string, resolve func(string) (string, error)) (mcp.Transport, func() error, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	if len(headers) > 0 {
		cl.Transport = &headerRT{base: http.DefaultTransport, headers: headers, resolve: resolve}
	}
	return &mcp.SSEClientTransport{Endpoint: url, HTTPClient: cl}, func() error { return nil }, nil
}

type headerRT struct {
	base    http.RoundTripper
	headers map[string]string
	resolve func(string) (string, error)
}

func (h *headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		iv := v
		if h.resolve != nil {
			iv = mustResolve(v, h.resolve)
		}
		req.Header.Set(k, iv)
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func mustResolve(v string, r func(string) (string, error)) string {
	// simplistic: only support top level {secret:NAME} for headers/env in this scaffold
	// full regex lives in secret.go; here we delegate via the passed func when the whole value is a secret ref
	if r == nil {
		return v
	}
	// try a very small parser for the common case {secret:NAME}
	if len(v) > 9 && v[:9] == "{secret:" && v[len(v)-1] == '}' {
		name := v[9 : len(v)-1]
		if out, err := r(name); err == nil {
			return out
		}
	}
	return v
}
