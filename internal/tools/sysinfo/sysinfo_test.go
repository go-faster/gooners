package sysinfo

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sshutil"
)

type mockRunner struct {
	runFunc func(cmd string) (sshutil.Result, error)
}

func (m *mockRunner) Run(ctx context.Context, sessionID, cmd string) (sshutil.Result, error) {
	if m.runFunc == nil {
		return sshutil.Result{}, fmt.Errorf("Run should not be called")
	}
	return m.runFunc(cmd)
}

func TestNetAddrsHandler_Fallback(t *testing.T) {
	runner := &mockRunner{
		runFunc: func(cmd string) (sshutil.Result, error) {
			if cmd == "ip -j addr show" {
				return sshutil.Result{
					Stdout:   "illegal option -j\n",
					ExitCode: 1,
				}, nil
			}
			if cmd == "ip addr show" {
				return sshutil.Result{
					Stdout:   "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\n",
					ExitCode: 0,
				}, nil
			}
			return sshutil.Result{}, fmt.Errorf("unexpected command: %s", cmd)
		},
	}

	handler := netAddrsHandler(runner)
	res, cr, err := handler(context.Background(), &mcp.CallToolRequest{}, netAddrsParams{
		SessionID: "test_session",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Contains(t, cr.Text, "1: lo: <LOOPBACK")
}

func TestNetAddrsHandler_SuccessJSON(t *testing.T) {
	runner := &mockRunner{
		runFunc: func(cmd string) (sshutil.Result, error) {
			if cmd == "ip -j addr show dev eth0" {
				return sshutil.Result{
					Stdout:   `[{"ifname":"eth0"}]`,
					ExitCode: 0,
				}, nil
			}
			return sshutil.Result{}, fmt.Errorf("unexpected command: %s", cmd)
		},
	}

	handler := netAddrsHandler(runner)
	res, cr, err := handler(context.Background(), &mcp.CallToolRequest{}, netAddrsParams{
		SessionID: "test_session",
		Iface:     "eth0",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, `[{"ifname":"eth0"}]`, cr.Text)
}

func TestSimpleCmdHandler(t *testing.T) {
	runner := &mockRunner{
		runFunc: func(cmd string) (sshutil.Result, error) {
			require.Equal(t, "uptime", cmd)
			return sshutil.Result{Stdout: "up 1 day"}, nil
		},
	}

	res, cr, err := simpleCmdHandler(runner, "uptime")(context.Background(), &mcp.CallToolRequest{}, sessionParam{
		SessionID: "test_session",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, "up 1 day", cr.Text)
}

func TestSimpleCmdHandler_RunError(t *testing.T) {
	runner := &mockRunner{
		runFunc: func(cmd string) (sshutil.Result, error) {
			require.Equal(t, "free -h", cmd)
			return sshutil.Result{Stderr: "missing"}, fmt.Errorf("not found")
		},
	}

	res, cr, err := simpleCmdHandler(runner, "free -h")(context.Background(), &mcp.CallToolRequest{}, sessionParam{
		SessionID: "test_session",
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "error: not found", cr.Text)
}

func TestSimpleCmdHandler_RequiresSessionID(t *testing.T) {
	_, _, err := simpleCmdHandler(&mockRunner{}, "uptime")(context.Background(), &mcp.CallToolRequest{}, sessionParam{})
	require.Error(t, err)
}
