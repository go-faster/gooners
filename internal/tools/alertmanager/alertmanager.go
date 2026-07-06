package alertmanager

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register registers all alertmanager-mcp tools on s.
func Register(s *mcp.Server, c *Client) {
	registerAlertTools(s, c)
	registerSilenceTools(s, c)
	registerReceiverTools(s, c)
	registerStatusTools(s, c)
	registerMatcherTools(s)
	registerPromQLTools(s, c)
}
