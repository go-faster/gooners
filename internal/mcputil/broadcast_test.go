package mcputil

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBroadcastWarningNoSessions(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	BroadcastWarning(s, "component", "message")
}

// connectLogClient connects a client that reports every log message it gets,
// on the given protocol revision.
func connectLogClient(t *testing.T, s *mcp.Server, version string) (sess *mcp.ClientSession, loggers <-chan string) {
	t.Helper()

	serverTr, clientTr := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(t.Context(), serverTr) }()

	got := make(chan string, 1)
	//nolint:staticcheck // SA1019: the test covers the deprecated MCP logging path.
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, &mcp.ClientOptions{
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			got <- req.Params.Logger
		},
	})
	sess, err := client.Connect(t.Context(), clientTr, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	return sess, got
}

func TestBroadcastWarning(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	sess, got := connectLogClient(t, s, "2025-11-25")

	//nolint:staticcheck // SA1019: the test covers the deprecated MCP logging path.
	if err := sess.SetLoggingLevel(t.Context(), &mcp.SetLoggingLevelParams{Level: "warning"}); err != nil {
		t.Fatal(err)
	}
	BroadcastWarning(s, "component", "message")

	select {
	case logger := <-got:
		if logger != "component" {
			t.Fatalf("logger = %q, want %q", logger, "component")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warning was not delivered")
	}
}

// Revision 2026-07-28 has no session-wide logging level, so a warning that
// is not tied to a request reaches nobody.
func TestBroadcastWarningStateless(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	_, got := connectLogClient(t, s, "2026-07-28")

	BroadcastWarning(s, "component", "message")

	select {
	case logger := <-got:
		t.Fatalf("unexpected warning from %q", logger)
	case <-time.After(100 * time.Millisecond):
	}
}
