package core

import (
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/session"
)

// registeredToolNames registers opts onto a fresh server and returns the
// sorted list of tool names an MCP client sees, via an in-memory transport
// (no stdio/network involved).
func registeredToolNames(t *testing.T, opts RegisterOptions) []string {
	t.Helper()

	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	p := session.NewPool(session.PoolOptions{})
	Register(s, p, opts)

	serverTr, clientTr := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(t.Context(), serverTr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(t.Context(), clientTr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	res, err := sess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)

	names := make([]string, len(res.Tools))
	for i, tl := range res.Tools {
		names[i] = tl.Name
	}
	sort.Strings(names)
	return names
}

// TestRegister_ToolSet guards against accidentally adding, removing, or
// renaming a ssh-mcp tool: Register's set must stay exactly what it has
// always been, since a future sandbox-mcp binary composes a strict subset of
// the RegisterXxx funcs and relies on this set being the ground truth.
func TestRegister_ToolSet(t *testing.T) {
	want := []string{
		"ssh_close",
		"ssh_exec",
		"ssh_list",
		"ssh_list_machines",
		"ssh_once_exec",
		"ssh_open",
		"ssh_open_cfg",
		"ssh_ping",
		"ssh_read_output",
		"ssh_save_output",
		"ssh_sudo_exec",
	}
	sort.Strings(want)
	require.Equal(t, want, registeredToolNames(t, RegisterOptions{}))
}

func TestRegister_ToolSet_DisableSudo(t *testing.T) {
	want := []string{
		"ssh_close",
		"ssh_exec",
		"ssh_list",
		"ssh_list_machines",
		"ssh_once_exec",
		"ssh_open",
		"ssh_open_cfg",
		"ssh_ping",
		"ssh_read_output",
		"ssh_save_output",
	}
	sort.Strings(want)
	require.Equal(t, want, registeredToolNames(t, RegisterOptions{DisableSudo: true}))
}
