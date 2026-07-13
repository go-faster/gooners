package effect

import (
	"net"
	"net/url"
	"strings"

	"github.com/go-faster/errors"
)

// HTTPPolicy decides which HTTP egress is allowed.
//
// It is checked in three places, because any one of them alone is bypassable:
// on the outgoing request, on every redirect the server asks us to follow, and
// on the IP the hostname actually resolved to (an allowed name whose DNS
// answer points at 169.254.169.254 is not an allowed destination).
type HTTPPolicy struct {
	// AllowHosts is the destination allowlist. An entry is a hostname
	// ("grafana.internal"), a host:port ("grafana.internal:3000"), a subdomain
	// wildcard ("*.example.com"), or "*" for any host.
	//
	// An entry carrying a port pins the destination to that port, which is
	// what an upstream on localhost needs: without it, "localhost" would allow
	// every other service on the loopback interface, and an SSRF against a
	// Grafana on :3000 could walk the whole local port range.
	//
	// The zero value allows nothing. That is deliberate — a client is built
	// for a known upstream, so its allowlist is the upstream it was configured
	// with, and a request anywhere else is a bug or an attack.
	AllowHosts []string
	// AllowLinkLocal permits link-local destinations (169.254.0.0/16,
	// fe80::/10). These are blocked by default because that range holds the
	// cloud instance-metadata service (169.254.169.254), whose credentials are
	// the usual prize in an SSRF.
	AllowLinkLocal bool
}

// CheckURL reports whether u is a destination this policy allows.
func (p HTTPPolicy) CheckURL(u *url.URL) error {
	if u == nil {
		return errors.Wrap(ErrDenied, "no URL")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return errors.Wrapf(ErrDenied, "scheme %q is not allowed (only http and https)", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return errors.Wrap(ErrDenied, "URL has no host")
	}
	if !p.allowsHost(host, u.Host) {
		return errors.Wrapf(ErrDenied, "host %q is not in the egress allowlist", u.Host)
	}
	// A URL may name an IP outright, which never reaches the resolver and so
	// would never reach CheckAddr's dial-time check.
	if ip := net.ParseIP(host); ip != nil {
		return p.CheckIP(ip)
	}
	return nil
}

// CheckIP reports whether ip is a destination this policy allows. It runs
// after DNS resolution, so a hostname cannot smuggle a blocked address in
// through its A record.
func (p HTTPPolicy) CheckIP(ip net.IP) error {
	if p.AllowLinkLocal {
		return nil
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return errors.Wrapf(ErrDenied, "link-local address %s is not allowed (cloud metadata range)", ip)
	}
	return nil
}

// allowsHost matches the URL's hostname (no port) and its authority (host with
// port, when the URL carries one) against the allowlist. An entry with a port
// only matches the authority; an entry without one matches any port.
func (p HTTPPolicy) allowsHost(host, hostPort string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	hostPort = strings.ToLower(hostPort)
	for _, allow := range p.AllowHosts {
		allow = strings.ToLower(allow)
		switch {
		case allow == "*":
			return true
		case strings.HasPrefix(allow, "*."):
			if suffix := allow[1:]; strings.HasSuffix(host, suffix) {
				return true
			}
		case strings.Contains(allow, ":"):
			// Also compare against host, so a bare IPv6 literal ("::1") is not
			// mistaken for a host:port entry.
			if allow == hostPort || allow == host {
				return true
			}
		case allow == host:
			return true
		}
	}
	return false
}
