package gitlab

import (
	"net/http"
	"strings"

	"github.com/go-faster/errors"
)

// AuthMode decides where the GitLab credential for a session comes from.
//
// Only the credential varies. The instance URL is always the operator's: a
// server that let a caller supply both the host and the token would be a
// shared server, but one that let a caller supply the host while the server
// supplies the token would be a credential-exfiltration endpoint.
type AuthMode int

const (
	// AuthServer authenticates every session as [Config.Token]. It is the
	// default, and on an HTTP transport it makes the server a
	// shared-credential proxy: whoever reaches the port acts as that token.
	AuthServer AuthMode = iota
	// AuthClientOptional prefers the caller's token and falls back to
	// [Config.Token] when there is none.
	AuthClientOptional
	// AuthClientRequired refuses a session that presents no token. The
	// server holds no credential of its own.
	AuthClientRequired
)

// ParseAuthMode maps the -auth flag value onto an [AuthMode].
func ParseAuthMode(s string) (AuthMode, error) {
	switch s {
	case "", "server":
		return AuthServer, nil
	case "client":
		return AuthClientRequired, nil
	case "client-optional":
		return AuthClientOptional, nil
	default:
		return 0, errors.Errorf("unknown auth mode %q: want server, client or client-optional", s)
	}
}

// String implements [fmt.Stringer].
func (m AuthMode) String() string {
	switch m {
	case AuthServer:
		return "server"
	case AuthClientOptional:
		return "client-optional"
	case AuthClientRequired:
		return "client"
	default:
		return "unknown"
	}
}

// ClientToken reads the caller's GitLab credential from an MCP HTTP request.
//
// Both spellings are accepted: Authorization is what an MCP client with any
// notion of auth already sends, PRIVATE-TOKEN is what GitLab itself uses. A
// nil request means stdio, which has no headers and so no caller token.
func ClientToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := strings.TrimSpace(r.Header.Get("PRIVATE-TOKEN")); v != "" {
		return v
	}
	if rest, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}
