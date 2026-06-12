package disk

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

func TestLsblkHandler(t *testing.T) {
	r := mockRunner{run: func(sessionID, cmd string) (sshutil.Result, error) {
		require.Equal(t, "session-1", sessionID)
		require.Equal(t, "lsblk -J -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,LABEL,UUID '/dev/sda 1'", cmd)
		return sshutil.Result{Stdout: `{"blockdevices":[]}`}, nil
	}}

	res, cr, err := lsblkHandler(r)(context.Background(), &mcp.CallToolRequest{}, lsblkParams{
		SessionID: "session-1",
		Device:    "/dev/sda 1",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, `{"blockdevices":[]}`, cr.Text)
}

func TestDfHandler_RunError(t *testing.T) {
	r := mockRunner{run: func(_, cmd string) (sshutil.Result, error) {
		require.Equal(t, "df -h '/var log'", cmd)
		return sshutil.Result{Stdout: "partial"}, fmt.Errorf("boom")
	}}

	res, cr, err := dfHandler(r)(context.Background(), &mcp.CallToolRequest{}, dfParams{
		SessionID: "session-1",
		Path:      "/var log",
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "error: boom", cr.Text)
}

func TestDfHandler_RequiresSessionID(t *testing.T) {
	_, _, err := dfHandler(mockRunner{})(context.Background(), &mcp.CallToolRequest{}, dfParams{})
	require.Error(t, err)
}
