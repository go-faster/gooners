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

func (m *mockRunner) Run(ctx context.Context, sessionID string, cmd string) (sshutil.Result, error) {
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
