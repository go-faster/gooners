// Package gatewaytransport builds mcp.Transport implementations from gateway config.
package gatewaytransport

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Build constructs a transport. interpolate is called for env/headers values (supports {secret:NAME}).
func Build(_ context.Context, kind string, command []string, url string, env, headers map[string]string, interpolate func(string) (string, error)) (mcp.Transport, func() error, error) {
	switch kind {
	case "stdio":
		return buildStdio(command, env, interpolate)
	case "http":
		return buildHTTP(url, headers, interpolate)
	case "sse":
		return buildSSE(url, headers, interpolate)
	default:
		return nil, nil, errors.Errorf("unknown upstream kind %q", kind)
	}
}

func buildStdio(command []string, env map[string]string, interpolate func(string) (string, error)) (mcp.Transport, func() error, error) {
	if len(command) == 0 {
		return nil, nil, errors.New("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // G204: command comes from operator TOML config (stdio upstream)
	if len(env) > 0 {
		overrides := map[string]string{}
		for k, v := range env {
			if interpolate != nil {
				iv, err := interpolate(v)
				if err != nil {
					return nil, nil, errors.Wrapf(err, "interpolate env %q", v)
				}
				overrides[k] = iv
			} else {
				overrides[k] = v
			}
		}
		base := os.Environ()
		keep := make([]string, 0, len(base))
		for _, kv := range base {
			if idx := strings.IndexByte(kv, '='); idx > 0 {
				k := kv[:idx]
				if _, ok := overrides[k]; ok {
					continue
				}
			}
			keep = append(keep, kv)
		}
		for k, v := range overrides {
			keep = append(keep, k+"="+v)
		}
		cmd.Env = keep
	}
	cleanup := func() error {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	return &mcp.CommandTransport{Command: cmd}, cleanup, nil
}

func buildHTTP(url string, headers map[string]string, interpolate func(string) (string, error)) (mcp.Transport, func() error, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	if len(headers) > 0 {
		cl.Transport = &headerRT{base: http.DefaultTransport, headers: headers, interpolate: interpolate}
	}
	return &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: cl}, func() error { return nil }, nil
}

func buildSSE(url string, headers map[string]string, interpolate func(string) (string, error)) (mcp.Transport, func() error, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	if len(headers) > 0 {
		cl.Transport = &headerRT{base: http.DefaultTransport, headers: headers, interpolate: interpolate}
	}
	return &mcp.SSEClientTransport{Endpoint: url, HTTPClient: cl}, func() error { return nil }, nil
}

type headerRT struct {
	base        http.RoundTripper
	headers     map[string]string
	interpolate func(string) (string, error)
}

func (h *headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		iv := v
		if h.interpolate != nil {
			var err error
			iv, err = h.interpolate(v)
			if err != nil {
				return nil, errors.Wrapf(err, "interpolate header %q", v)
			}
		}
		req.Header.Set(k, iv)
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
