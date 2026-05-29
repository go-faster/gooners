package proc

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
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
			if got := validPID(tt.pid); got != tt.valid {
				t.Errorf("validPID(%q) = %v, want %v", tt.pid, got, tt.valid)
			}
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
			if got := validSignal(tt.sig); got != tt.want {
				t.Errorf("validSignal(%q) = %v, want %v", tt.sig, got, tt.want)
			}
		})
	}
}

// Ensure security barriers hold in KillHandler
func TestKillHandler_Security(t *testing.T) {
	handler := killHandler(nil) // dummyPool not needed if it fails validation early

	req := mcp.CallToolRequest{}
	args := map[string]interface{}{
		"session_id": "test_id",
		"pid":        "1; rm -rf /",
		"signal":     "TERM",
	}
	req.Params.Arguments = args

	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected error for invalid PID, got none")
	}

	args["pid"] = "1"
	args["signal"] = "TERM; rm -rf /"
	res, err = handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected error for invalid signal, got none")
	}
}
