package main

import (
	"context"
	"log/slog"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/session"
)

// fakeSandboxManager is a minimal sandboxtools.Manager double: registerTools
// only needs something to hand to sandboxtools.Register, and this test
// never calls sandbox_open/sandbox_close for real.
type fakeSandboxManager struct{}

func (fakeSandboxManager) Open(context.Context, sandbox.Spec) (sandbox.OpenResult, error) {
	return sandbox.OpenResult{}, nil
}

func (fakeSandboxManager) Close(context.Context, string) error { return nil }

// registeredToolNames registers every tool sandbox-mcp exposes onto a fresh
// server and returns the sorted tool names an MCP client sees, over an
// in-memory transport (no stdio/network, no Docker).
func registeredToolNames(t *testing.T) []string {
	t.Helper()

	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	pool := session.NewPool(session.PoolOptions{})
	registerTools(s, pool, fakeSandboxManager{}, false, slog.New(slog.DiscardHandler))

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

// TestRegisterTools_ExcludesSandboxEscapeAndTokenLeakTools is the regression
// test the plan calls for explicitly: a mistake here is a sandbox escape
// (ssh_open/ssh_open_cfg/ssh_once_exec would let the agent SSH out to any
// host this process can reach), a capability-token leak (ssh_list returns
// every session in the process to every caller), or a host-crossing covert
// channel (upload_file/download_file share one uploadRoot directory across
// unrelated sandboxes). It also covers the tools dropped purely to reduce
// surface (fs/proc/disk/sysinfo) and ssh_save_output, now redundant with
// closeSession's unconditional spool cleanup. None of these must ever be
// registered by sandbox-mcp.
func TestRegisterTools_ExcludesSandboxEscapeAndTokenLeakTools(t *testing.T) {
	names := registeredToolNames(t)

	forbidden := []string{
		"ssh_open",          // would let the sandbox SSH out to an arbitrary host
		"ssh_open_cfg",      // same escape, with explicit parameters
		"ssh_once_exec",     // same escape, one-shot
		"ssh_list",          // leaks every other conversation's session_id
		"ssh_list_machines", // pointless in a container, and lists host config
		"systemctl_status",  // systemd tools are meaningless in a container
		"ssh_save_output",   // redundant: closeSession already deletes spools on every teardown
		"upload_file",       // host-crossing: local path is on the sandbox-mcp host, not the container
		"download_file",     // same host-crossing concern as upload_file
		"ls",                // fs tools are redundant with ssh_exec against a disposable container
		"cat",               // same redundancy as ls
		"proc_list",         // proc/disk/sysinfo are pure surface reduction: exec covers them
		"disk_df",           // same reasoning as proc_list
		"sys_mem",           // same reasoning as proc_list
	}
	for _, name := range forbidden {
		require.NotContains(t, names, name, "sandbox-mcp must never register %s", name)
	}
}

func TestRegisterTools_ToolSet(t *testing.T) {
	names := registeredToolNames(t)

	want := []string{
		"sandbox_open",
		"sandbox_close",
		"ssh_close",
		"ssh_exec",
		"ssh_sudo_exec",
		"ssh_ping",
		"ssh_read_output",
	}
	for _, name := range want {
		require.Contains(t, names, name)
	}
	require.Len(t, names, len(want), "sandbox-mcp's tool surface changed; update this expectation deliberately: %v", names)
}

func TestRegisterTools_DisableSudo(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	pool := session.NewPool(session.PoolOptions{})
	registerTools(s, pool, fakeSandboxManager{}, true, slog.New(slog.DiscardHandler))

	serverTr, clientTr := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(t.Context(), serverTr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(t.Context(), clientTr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	res, err := sess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)

	for _, tl := range res.Tools {
		require.NotEqual(t, "ssh_sudo_exec", tl.Name)
	}
}

func TestSplitCommaList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "single", in: "alpine:latest", want: []string{"alpine:latest"}},
		{name: "multiple with spaces", in: "a, b ,c", want: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, splitCommaList(tt.in))
		})
	}
}

func TestParseNetworks(t *testing.T) {
	got := parseNetworks("none,open")
	require.Equal(t, []sandbox.Network{sandbox.NetworkNone, sandbox.NetworkOpen}, got)
}
