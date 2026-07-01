// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestUpstream_BuildTools(t *testing.T) {
	u := &Upstream{cfg: UpstreamConfig{Tools: ToolsConfig{Prefix: "p."}}}
	tools := []*mcp.Tool{{Name: "x", Description: "d"}}
	got := u.BuildTools(tools)
	require.Equal(t, "p.x", got[0].Name)
}

func TestUpstream_Filter(t *testing.T) {
	u := &Upstream{cfg: UpstreamConfig{Tools: ToolsConfig{Allow: []string{"a*"}, Deny: []string{"ab*"}}}}
	require.True(t, u.allowed("ac"))
	require.False(t, u.allowed("ab"))
	require.False(t, u.allowed("x"))
}

func TestUpstream_Trim(t *testing.T) {
	require.Equal(t, "abc…", TrimDescription("abcdef", 3))
}

func TestUpstream_InMemory(t *testing.T) {
	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "hello", Description: "say hi", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil
	})
	go srv.Run(context.Background(), st)

	u := &Upstream{cfg: UpstreamConfig{Name: "u1"}}
	// wire in-memory directly
	u.client = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess

	tools, err := u.ListTools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	res, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "hello"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)

	_ = u.Close(t.Context())
}

func TestUpstream_Reconnect_AfterSessionDrops(t *testing.T) {
	clientTr1, cancel1 := newToolServer(t, "up1")
	clientTr2, cancel2 := newToolServer(t, "up2")
	defer cancel2()

	transports := make(chan mcp.Transport, 2)
	transports <- clientTr1
	transports <- clientTr2
	oldBuildTransport := BuildTransport
	BuildTransport = func(ctx context.Context, _ UpstreamConfig, _ SecretResolver) (mcp.Transport, func() error, error) {
		select {
		case tr := <-transports:
			return tr, func() error { return nil }, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	t.Cleanup(func() { BuildTransport = oldBuildTransport })

	reconnected := make(chan string, 1)
	u, err := NewUpstream(UpstreamConfig{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}, UpstreamOptions{
		KeepAlive:        -1,
		ReconnectInitial: 10 * time.Millisecond,
		ReconnectMax:     10 * time.Millisecond,
		OnReconnect: func(_ context.Context, upstreamName string) error {
			reconnected <- upstreamName
			return nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, u.Connect(t.Context()))
	t.Cleanup(func() { _ = u.Close(t.Context()) })

	firstSession := u.currentSession()
	require.NotNil(t, firstSession)
	cancel1()

	require.Eventually(t, func() bool {
		select {
		case name := <-reconnected:
			return name == "u1" && u.currentSession() != nil && u.currentSession() != firstSession
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestUpstream_Supervisor_ExitsOnContextCancel(t *testing.T) {
	clientTr, cancelServer := newToolServer(t, "up")
	defer cancelServer()

	var builds atomic.Int32
	oldBuildTransport := BuildTransport
	BuildTransport = func(context.Context, UpstreamConfig, SecretResolver) (mcp.Transport, func() error, error) {
		builds.Add(1)
		return clientTr, func() error { return nil }, nil
	}
	t.Cleanup(func() { BuildTransport = oldBuildTransport })

	ctx, cancel := context.WithCancel(t.Context())
	u, err := NewUpstream(UpstreamConfig{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}, UpstreamOptions{
		KeepAlive:        -1,
		ReconnectInitial: 10 * time.Millisecond,
		ReconnectMax:     10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, u.Connect(ctx))

	u.mu.RLock()
	done := u.supervisorDone
	u.mu.RUnlock()
	require.NotNil(t, done)
	cancel()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond)
	require.Equal(t, int32(1), builds.Load())
	_ = u.Close(t.Context())
}

func TestUpstream_Close_Idempotent(t *testing.T) {
	clientTr, cancelServer := newToolServer(t, "up")
	defer cancelServer()

	oldBuildTransport := BuildTransport
	BuildTransport = func(context.Context, UpstreamConfig, SecretResolver) (mcp.Transport, func() error, error) {
		return clientTr, func() error { return nil }, nil
	}
	t.Cleanup(func() { BuildTransport = oldBuildTransport })

	u, err := NewUpstream(UpstreamConfig{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}, UpstreamOptions{KeepAlive: -1})
	require.NoError(t, err)
	require.NoError(t, u.Connect(t.Context()))
	require.NoError(t, u.Close(t.Context()))
	require.NoError(t, u.Close(t.Context()))
}

func TestUpstream_Backoff_Doubles(t *testing.T) {
	tests := []struct {
		name    string
		current time.Duration
		initial time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{name: "zero uses initial", current: 0, initial: time.Second, max: 30 * time.Second, want: 2 * time.Second},
		{name: "doubles", current: time.Second, initial: time.Second, max: 30 * time.Second, want: 2 * time.Second},
		{name: "caps", current: 20 * time.Second, initial: time.Second, max: 30 * time.Second, want: 30 * time.Second},
		{name: "overflow caps", current: time.Duration(1<<63 - 1), initial: time.Second, max: 30 * time.Second, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nextBackoff(tt.current, tt.initial, tt.max))
		})
	}
}

func newToolServer(t *testing.T, serverName string) (mcp.Transport, context.CancelFunc) {
	t.Helper()
	serverTr, clientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "0"}, nil)
	toolName := "hello"
	srv.AddTool(&mcp.Tool{Name: toolName, Description: toolName, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = srv.Run(ctx, serverTr) }()
	return clientTr, cancel
}
