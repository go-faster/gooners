package proc

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sshutil"
)

type mockProvider struct {
	run func(sessionID, cmd string) (sshutil.Result, error)
}

func (m mockProvider) Run(_ context.Context, sessionID, cmd string) (sshutil.Result, error) {
	if m.run == nil {
		return sshutil.Result{}, fmt.Errorf("Run should not be called")
	}
	return m.run(sessionID, cmd)
}

func TestValidPID(t *testing.T) {
	tests := []struct {
		pid   string
		valid bool
	}{
		{"1", true},
		{"1234", true},
		{"99999", true},
		{"", false},
		{"0", true}, // Technically ok by regex, though init is 1
		{"-1", false},
		{"1a", false},
		{"a1", false},
		{"1 2", false},
		{"1\n2", false},
		{"1;rm", false},
	}

	for _, tt := range tests {
		t.Run(tt.pid, func(t *testing.T) {
			got := validPID(tt.pid)
			require.Equal(t, tt.valid, got)
		})
	}
}

func TestValidSignal(t *testing.T) {
	tests := []struct {
		sig  string
		want string
	}{
		{"", "TERM"},
		{"TERM", "TERM"},
		{"SIGTERM", "TERM"},
		{"sigterm", "TERM"},
		{"9", "9"},
		{"15", "15"},
		{"KILL", "KILL"},
		{"HUP", "HUP"},
		{"UNKNOWN", ""},
		{"TERM;rm", ""},
		{"1;rm", ""},
		{"-9", ""},
	}

	for _, tt := range tests {
		t.Run(tt.sig, func(t *testing.T) {
			got := validSignal(tt.sig)
			require.Equal(t, tt.want, got)
		})
	}
}

// Ensure security barriers hold in KillHandler
func TestKillHandler_Security(t *testing.T) {
	handler := killHandler(nil)

	_, _, err := handler(context.Background(), &mcp.CallToolRequest{}, killParams{
		SessionID: "test_id",
		PID:       "1; rm -rf /",
		Signal:    "TERM",
	})
	require.Error(t, err) // validation error returned directly (becomes tool error)

	_, _, err = handler(context.Background(), &mcp.CallToolRequest{}, killParams{
		SessionID: "test_id",
		PID:       "1",
		Signal:    "TERM; rm -rf /",
	})
	require.Error(t, err)
}

func TestListHandler(t *testing.T) {
	p := mockProvider{run: func(sessionID, cmd string) (sshutil.Result, error) {
		require.Equal(t, "session-1", sessionID)
		require.Equal(t, "(ps -u alice aux) | grep -i nginx | head -n 5", cmd)
		return sshutil.Result{Stdout: "alice 123 nginx"}, nil
	}}

	res, cr, err := listHandler(p)(context.Background(), &mcp.CallToolRequest{}, procListParams{
		SessionID: "session-1",
		User:      "alice",
		Filter:    "nginx",
		MaxLines:  5,
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, "alice 123 nginx", cr.Text)
}

func TestInfoHandler(t *testing.T) {
	p := mockProvider{run: func(sessionID, cmd string) (sshutil.Result, error) {
		require.Equal(t, "session-1", sessionID)
		require.Contains(t, cmd, "cat /proc/123/status")
		require.Contains(t, cmd, "readlink /proc/123/cwd")
		return sshutil.Result{Stdout: "status"}, nil
	}}

	_, cr, err := infoHandler(p)(context.Background(), &mcp.CallToolRequest{}, procPIDParams{SessionID: "session-1", PID: "123"})
	require.NoError(t, err)
	require.Equal(t, "status", cr.Text)
}

func TestLsofHandler_InvalidPID(t *testing.T) {
	_, _, err := lsofHandler(mockProvider{})(context.Background(), &mcp.CallToolRequest{}, procPIDParams{SessionID: "session-1", PID: "x"})
	require.Error(t, err)
}

func TestKillHandler_RunError(t *testing.T) {
	p := mockProvider{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "sudo -n kill -KILL 123", cmd)
		return sshutil.Result{Stderr: "denied", ExitCode: 1}, fmt.Errorf("exit status 1")
	}}

	res, cr, err := killHandler(p)(context.Background(), &mcp.CallToolRequest{}, killParams{
		SessionID: "session-1",
		PID:       "123",
		Signal:    "SIGKILL",
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "error: exit status 1", cr.Text)
}

func TestKillHandler_OnlyExitCodeError(t *testing.T) {
	p := mockProvider{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "sudo -n kill -KILL 123", cmd)
		return sshutil.Result{Stderr: "denied", ExitCode: 1}, nil
	}}

	res, cr, err := killHandler(p)(context.Background(), &mcp.CallToolRequest{}, killParams{
		SessionID: "session-1",
		PID:       "123",
		Signal:    "SIGKILL",
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "denied", cr.Text)
}
