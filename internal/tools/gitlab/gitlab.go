// Package gitlab registers MCP tools for GitLab issues, merge requests,
// releases and repository browsing.
//
// It talks to the GitLab API directly rather than shelling out to the glab
// CLI, which is what lets every tool take a project argument: glab's own MCP
// server derives the project from the git remote of its working directory and
// so cannot run outside a checkout.
package gitlab

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/mcputil"
)

// Instructions is the server-level guidance sent to clients.
const Instructions = `You are connected to gitlab-mcp. Use these tools to read and write GitLab issues, merge requests and releases, and to browse repository files.

Every tool takes a project argument, a path like group/project or a numeric ID. It is optional only when the server was started with a default project; repo_search finds the path when you do not know it.

Issues and merge requests are addressed by their project-scoped number (the #123 or !123 shown in the UI), not by their global ID. On issue_update and mr_update, labels replaces the whole set while add_labels and remove_labels edit it.

These tools cannot merge, approve, or delete anything.`

// Register registers all gitlab-mcp tools and resources on s.
func Register(s *mcp.Server, c *Client) {
	registerIssueTools(s, c)
	registerMergeRequestTools(s, c)
	registerReleaseTools(s, c)
	registerRepoTools(s, c)
	registerResources(s, c)
}

// SessionServerOptions configures [NewSessionServer].
type SessionServerOptions struct {
	// Clients builds the per-credential GitLab clients.
	Clients *ClientSet
	// Mode decides whose credential a session uses.
	Mode AuthMode
	// Server is the base MCP server configuration; one server is built per
	// session from it.
	Server mcputil.ServerConfig
	// Logger records rejected sessions. Defaults to [slog.Default].
	Logger *slog.Logger
}

func (o *SessionServerOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// NewSessionServer returns the getServer function for
// [mcpcmd.RunOptions.Handler]. It runs once per MCP session, so a session's
// credential is fixed for its lifetime rather than re-read per tool call. That
// makes the session ID as sensitive as the token: a later request on the same
// session runs as whoever opened it, whatever header it carries.
//
// A nil return makes the SDK refuse the session with 400.
func NewSessionServer(opts SessionServerOptions) func(*http.Request) *mcp.Server {
	opts.setDefaults()

	return func(r *http.Request) *mcp.Server {
		c, ok := opts.clientFor(r)
		if !ok {
			return nil
		}

		s := mcputil.NewServer(opts.Server)
		Register(s, c)
		return s
	}
}

// clientFor resolves the client a session runs as, or reports that the session
// must be refused.
func (o SessionServerOptions) clientFor(r *http.Request) (*Client, bool) {
	token := ClientToken(r)
	switch o.Mode {
	case AuthServer:
		token = o.Clients.Token()
	case AuthClientOptional:
		if token == "" {
			token = o.Clients.Token()
		}
	case AuthClientRequired:
		if token == "" {
			o.Logger.Warn("refusing session with no client token", "remote", remoteOf(r))
			return nil, false
		}
	}

	c, err := o.Clients.For(token)
	if err != nil {
		o.Logger.Error("failed to build gitlab client for session", "err", err)
		return nil, false
	}
	return c, true
}

// remoteOf identifies a rejected caller in a log line without touching any
// header it sent.
func remoteOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.RemoteAddr
}
