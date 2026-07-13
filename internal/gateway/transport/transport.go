// Package gatewaytransport builds mcp.Transport implementations from gateway config.
package gatewaytransport

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/effect"
)

// Options configures [Build].
type Options struct {
	// Kind is the upstream kind: "stdio", "http" or "sse".
	Kind string
	// Command is the stdio upstream's argv.
	Command []string
	// URL is the http/sse upstream's endpoint.
	URL string
	// Env overrides environment variables of a stdio upstream.
	Env map[string]string
	// Headers are injected into every http/sse request.
	Headers map[string]string
	// StripHeaders are removed from every http/sse request before Headers are
	// applied.
	StripHeaders []string
	// Interpolate expands env and header values (supports {secret:NAME}).
	Interpolate func(string) (string, error)
	// HTTPClient is used by http/sse upstreams. If nil, it is an
	// [effect.NewHTTPClient] allowing egress to URL's host alone: an upstream
	// may only be reached at its own endpoint. It carries no timeout, because
	// both transports stream.
	HTTPClient *http.Client
}

func (o *Options) setDefaults() {
	if o.HTTPClient == nil && (o.Kind == "http" || o.Kind == "sse") {
		o.HTTPClient = effect.NewHTTPClient(effect.HTTPOptions{
			Policy: effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(o.URL)},
		})
	}
}

// Build constructs a transport and a cleanup to run after the session closes.
func Build(_ context.Context, opts Options) (mcp.Transport, func() error, error) {
	opts.setDefaults()
	switch opts.Kind {
	case "stdio":
		return buildStdio(opts.Command, opts.Env, opts.Interpolate)
	case "http":
		cl := clientWithHeaders(opts.HTTPClient, opts.Headers, opts.StripHeaders, opts.Interpolate)
		return &mcp.StreamableClientTransport{Endpoint: opts.URL, HTTPClient: cl}, noCleanup, nil
	case "sse":
		cl := clientWithHeaders(opts.HTTPClient, opts.Headers, opts.StripHeaders, opts.Interpolate)
		return &mcp.SSEClientTransport{Endpoint: opts.URL, HTTPClient: cl}, noCleanup, nil
	default:
		return nil, nil, errors.Errorf("unknown upstream kind %q", opts.Kind)
	}
}

func noCleanup() error { return nil }

// clientWithHeaders returns a copy of cl whose transport injects and strips
// headers on top of cl's own (policy-enforcing) transport.
func clientWithHeaders(cl *http.Client, headers map[string]string, stripHeaders []string, interpolate func(string) (string, error)) *http.Client {
	if len(headers) == 0 && len(stripHeaders) == 0 {
		return cl
	}
	base := cl.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *cl
	out.Transport = &headerRT{base: base, headers: headers, stripHeaders: stripHeaders, interpolate: interpolate}
	return &out
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
	// cleanup is called after the session closes. By that point CommandTransport
	// has already sent SIGTERM and waited; this is a belt-and-suspenders SIGKILL
	// in case the graceful shutdown stalled.
	cleanup := func() error {
		if p := cmd.Process; p != nil {
			if err := p.Kill(); err != nil {
				return err
			}
			// Reap the process to avoid a zombie.
			_ = cmd.Wait()
		}
		return nil
	}
	return &mcp.CommandTransport{Command: cmd}, cleanup, nil
}

type headerRT struct {
	base         http.RoundTripper
	headers      map[string]string
	stripHeaders []string
	interpolate  func(string) (string, error)
}

func (h *headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for _, header := range h.stripHeaders {
		req.Header.Del(header)
	}
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
