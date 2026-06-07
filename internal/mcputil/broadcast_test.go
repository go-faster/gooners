package mcputil

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBroadcastWarningNoSessions(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	BroadcastWarning(s, "component", "message")
}
