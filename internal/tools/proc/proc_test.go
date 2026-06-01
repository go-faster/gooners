package proc

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

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
	handler := killHandler(nil) // dummyPool not needed if it fails validation early

	req := mcp.CallToolRequest{}
	args := map[string]any{
		"session_id": "test_id",
		"pid":        "1; rm -rf /",
		"signal":     "TERM",
	}
	req.Params.Arguments = args

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error for invalid PID, got none")

	args["pid"] = "1"
	args["signal"] = "TERM; rm -rf /"
	res, err = handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error for invalid signal, got none")
}
