package systemd

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sshutil"
)

type mockRunner struct {
	run func(sessionID, cmd string) (sshutil.Result, error)
}

func (m mockRunner) Run(_ context.Context, sessionID, cmd string) (sshutil.Result, error) {
	if m.run == nil {
		return sshutil.Result{}, fmt.Errorf("Run should not be called")
	}
	return m.run(sessionID, cmd)
}

func TestStatusHandler(t *testing.T) {
	r := mockRunner{run: func(sessionID, cmd string) (sshutil.Result, error) {
		require.Equal(t, "session-1", sessionID)
		require.Equal(t, "systemctl status 'nginx service.service'", cmd)
		return sshutil.Result{Stdout: "active"}, nil
	}}

	res, cr, err := statusHandler(r)(context.Background(), &mcp.CallToolRequest{}, systemdBaseParams{
		SessionID: "session-1",
		Unit:      "nginx service.service",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, "active", cr.Text)
}

func TestListUnitsHandler(t *testing.T) {
	r := mockRunner{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "systemctl list-units --output=json --state=failed --type=service", cmd)
		return sshutil.Result{Stdout: "[]"}, nil
	}}

	_, cr, err := listUnitsHandler(r)(context.Background(), &mcp.CallToolRequest{}, listUnitsParams{
		SessionID: "session-1",
		State:     "failed",
		Type:      "service",
	})
	require.NoError(t, err)
	require.Equal(t, "[]", cr.Text)
}

func TestMutatingHandler_RunError(t *testing.T) {
	r := mockRunner{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "sudo -n systemctl restart app.service", cmd)
		return sshutil.Result{Stderr: "denied"}, fmt.Errorf("exit status 1")
	}}

	res, cr, err := mutatingHandler(r, "restart")(context.Background(), &mcp.CallToolRequest{}, systemdBaseParams{
		SessionID: "session-1",
		Unit:      "app.service",
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "error: exit status 1", cr.Text)
}

func TestJournalHandler_JSONLines(t *testing.T) {
	r := mockRunner{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "journalctl -o json --no-pager -n 25 -u app.service --since='1 hour ago' -p err", cmd)
		return sshutil.Result{Stdout: "{\"MESSAGE\":\"one\"}\n{\"MESSAGE\":\"two\"}\n"}, nil
	}}

	res, cr, err := journalHandler(r)(context.Background(), &mcp.CallToolRequest{}, journalParams{
		SessionID: "session-1",
		Unit:      "app.service",
		Since:     "1 hour ago",
		Lines:     25,
		Priority:  "err",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.JSONEq(t, `[{"MESSAGE":"one"},{"MESSAGE":"two"}]`, cr.Text)
}

func TestJournalHandler_ClampedLines(t *testing.T) {
	r := mockRunner{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "journalctl -o json --no-pager -n 100", cmd)
		return sshutil.Result{Stdout: ""}, nil
	}}

	_, cr, err := journalHandler(r)(context.Background(), &mcp.CallToolRequest{}, journalParams{SessionID: "session-1", Lines: 10001})
	require.NoError(t, err)
	require.JSONEq(t, `[]`, cr.Text)
}
