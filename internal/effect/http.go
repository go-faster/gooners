package effect

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/go-faster/errors"
)

// Doer performs HTTP requests. [net/http.Client] satisfies it, and so does a
// test fake: taking a Doer rather than building a client inline is what makes
// an HTTP-calling package testable without a live server.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPOptions configures [NewHTTPClient].
type HTTPOptions struct {
	// Policy is the egress allowlist. Its zero value allows nothing.
	Policy HTTPPolicy
	// Timeout bounds a whole request. Zero means no timeout, which is what a
	// streaming transport (SSE, streamable HTTP) needs.
	Timeout time.Duration
	// TLSClientConfig, if set, replaces the default TLS configuration (custom
	// CA bundle, client certificates).
	TLSClientConfig *tls.Config
}

// NewHTTPClient returns a client that can only reach destinations the policy
// allows. The check runs on the request, on each redirect, and on the address
// the dialer actually connects to.
func NewHTTPClient(opts HTTPOptions) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.TLSClientConfig != nil {
		transport.TLSClientConfig = opts.TLSClientConfig
	}
	transport.DialContext = policyDialer(opts.Policy).DialContext

	return &http.Client{
		Timeout:   opts.Timeout,
		Transport: &policyTransport{base: transport, policy: opts.Policy},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			// A 302 to another host is a request we never vetted; an upstream
			// that can redirect us anywhere is an upstream that can read any
			// host we can reach.
			return opts.Policy.CheckURL(req.URL)
		},
	}
}

// policyTransport vets the request URL before the request leaves.
type policyTransport struct {
	base   http.RoundTripper
	policy HTTPPolicy
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.policy.CheckURL(req.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// policyDialer vets the resolved address, closing the gap between the name we
// allowed and the address it points at.
func policyDialer(policy HTTPPolicy) *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errors.Wrapf(err, "parse dial address %q", address)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return errors.Wrapf(ErrDenied, "dial address %q is not an IP", address)
			}
			return policy.CheckIP(ip)
		},
	}
}

// AllowHostOf returns rawURL's authority as a single-entry allowlist, for a
// client whose only legitimate destination is the upstream it was configured
// with. The port is kept when the URL carries one, so an upstream on
// localhost:3000 does not imply the rest of the loopback interface.
//
// An empty or unparseable URL yields no entries, i.e. a client that can reach
// nothing — an unconfigured upstream should fail closed.
func AllowHostOf(rawURL string) []string {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return []string{u.Host}
}
