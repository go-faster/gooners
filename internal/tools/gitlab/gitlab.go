// Package gitlab registers MCP tools for GitLab issues, merge requests,
// releases and repository browsing.
//
// It talks to the GitLab API directly rather than shelling out to the glab
// CLI, which is what lets every tool take a project argument: glab's own MCP
// server derives the project from the git remote of its working directory and
// so cannot run outside a checkout.
package gitlab

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
