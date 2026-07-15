package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	internalsandbox "github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/tools/sandbox"
)

type fakeManager struct {
	openResult internalsandbox.OpenResult
	openErr    error
	closeErr   error

	lastOpenSpec internalsandbox.Spec
	lastCloseID  string
	closeCalls   int
}

func (f *fakeManager) Open(_ context.Context, spec internalsandbox.Spec) (internalsandbox.OpenResult, error) {
	f.lastOpenSpec = spec
	return f.openResult, f.openErr
}

func (f *fakeManager) Close(_ context.Context, sessionID string) error {
	f.lastCloseID = sessionID
	f.closeCalls++
	return f.closeErr
}

func newTestServer(t *testing.T, m *fakeManager) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	sandbox.Register(s, m)

	serverTr, clientTr := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(t.Context(), serverTr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(t.Context(), clientTr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestSandboxOpen(t *testing.T) {
	m := &fakeManager{
		openResult: internalsandbox.OpenResult{
			Image:   "alpine:latest",
			Network: internalsandbox.NetworkNone,
		},
	}
	m.openResult.ID = "sandbox-session-1"
	m.openResult.Label = "sandbox-happy-turing"

	sess := newTestServer(t, m)

	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "sandbox_open",
		Arguments: map[string]any{
			"image":   "custom:latest",
			"network": "open",
			"env":     map[string]any{"FOO": "bar"},
			"workdir": "/work",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.Equal(t, "custom:latest", m.lastOpenSpec.Image)
	require.Equal(t, internalsandbox.NetworkOpen, m.lastOpenSpec.Network)
	require.Equal(t, "/work", m.lastOpenSpec.Workdir)
	require.Equal(t, "bar", m.lastOpenSpec.Env["FOO"])

	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out))
	require.Equal(t, "sandbox-session-1", out["session_id"])
	require.Equal(t, "sandbox-happy-turing", out["label"])
	require.Equal(t, "alpine:latest", out["image"])
	require.Equal(t, "none", out["network"])
}

func TestSandboxOpen_ManagerError(t *testing.T) {
	m := &fakeManager{openErr: errors.New("policy rejected image")}
	sess := newTestServer(t, m)

	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "sandbox_open",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

func TestSandboxClose(t *testing.T) {
	m := &fakeManager{}
	sess := newTestServer(t, m)

	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "sandbox_close",
		Arguments: map[string]any{"session_id": "sandbox-session-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, "sandbox-session-1", m.lastCloseID)
	require.Equal(t, 1, m.closeCalls)
}

func TestSandboxClose_MissingSessionID(t *testing.T) {
	m := &fakeManager{}
	sess := newTestServer(t, m)

	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "sandbox_close",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, 0, m.closeCalls)
}
