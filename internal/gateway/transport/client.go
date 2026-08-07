package gatewaytransport

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/go-faster/errors"
)

// newUpstreamClient returns the HTTP client for an http/sse upstream, and the
// cleanup that aborts whatever it still has in flight.
//
// The cleanup is the http/sse counterpart of the stdio cleanup's SIGKILL.
// Without it an upstream that accepts a request and never answers holds it
// forever: these clients deliberately carry no timeout because both transports
// stream, so closing the session cannot reclaim the request, the goroutine
// waiting on it, or the connection. A drain timeout bounds how long the gateway
// *waits*; this is what makes giving up actually free anything.
func newUpstreamClient(cl *http.Client, headers map[string]string, stripHeaders []string, interpolate func(string) (string, error)) (client *http.Client, cleanup func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	base := cl.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *cl
	out.Transport = &upstreamRoundTripper{
		base:         base,
		headers:      headers,
		stripHeaders: stripHeaders,
		interpolate:  interpolate,
		shutdown:     ctx,
	}
	return &out, func() error {
		cancel()
		return nil
	}
}

// upstreamRoundTripper decorates every request to an http/sse upstream: it
// applies the configured header strip/inject on top of the policy-enforcing
// transport underneath, and binds the request's lifetime to the upstream's
// shutdown.
type upstreamRoundTripper struct {
	base         http.RoundTripper
	headers      map[string]string
	stripHeaders []string
	interpolate  func(string) (string, error)
	shutdown     context.Context
}

func (t *upstreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Cancellation is released when the body is closed rather than when
	// RoundTrip returns: a streaming response is still arriving long after
	// that, and canceling early would break SSE and streamable HTTP outright.
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.shutdown, cancel)
	release := func() {
		stop()
		cancel()
	}

	req = req.Clone(ctx)
	for _, header := range t.stripHeaders {
		req.Header.Del(header)
	}
	for k, v := range t.headers {
		iv := v
		if t.interpolate != nil {
			var err error
			if iv, err = t.interpolate(v); err != nil {
				release()
				return nil, errors.Wrapf(err, "interpolate header %q", v)
			}
		}
		req.Header.Set(k, iv)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &abortBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// abortBody releases the request's cancellation once the caller is done reading.
type abortBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *abortBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
