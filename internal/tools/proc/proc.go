// Package proc registers MCP tools for process inspection and management (ps, kill).
package proc

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

// validPID returns true if s is a non-empty string of digits (positive integer).
func validPID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validSignal returns the canonical signal name/number if safe, or "" if not.
// Accepts numeric signals and a whitelist of names (with or without SIG prefix).
func validSignal(s string) string {
	if s == "" {
		return "TERM"
	}
	allDigits := true
	for _, c := range s {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return s
	}
	upper := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(s), "SIG"))
	known := map[string]bool{
		"TERM": true, "KILL": true, "HUP": true, "INT": true,
		"QUIT": true, "USR1": true, "USR2": true, "STOP": true,
		"CONT": true, "ABRT": true,
	}
	if known[upper] {
		return upper
	}
	return ""
}

func Register(s *mcp.Server, p session.Provider) {
	mcp.AddTool(s, &mcp.Tool{Name: "proc_list", Description: "List running processes (ps aux). Optional user and grep filter."}, listHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "proc_info", Description: "Show details for a process: /proc/<pid>/status, cmdline, exe, cwd."}, infoHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "proc_lsof", Description: "List open files for a process. Uses lsof if available, falls back to /proc/<pid>/fd."}, lsofHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "proc_kill", Description: "Send a signal to a process (uses sudo -n). Default signal: TERM."}, killHandler(p))
}

type procListParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	User      string  `json:"user,omitempty" jsonschema:"Filter by username"`
	Filter    string  `json:"filter,omitempty" jsonschema:"grep filter for process names"`
	MaxLines  float64 `json:"max_lines,omitempty" jsonschema:"Maximum lines of output"`
}

func listHandler(p session.Provider) mcp.ToolHandlerFor[procListParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args procListParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "ps aux"
		if args.User != "" {
			cmd = "ps -u " + sshutil.Quote(args.User) + " aux"
		}
		if args.Filter != "" {
			cmd = "(" + cmd + ") | grep -i " + sshutil.Quote(args.Filter)
		}
		if args.MaxLines > 0 {
			cmd += fmt.Sprintf(" | head -n %d", int64(args.MaxLines))
		}
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

type procPIDParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	PID       string `json:"pid" jsonschema:"Process ID"`
}

func infoHandler(p session.Provider) mcp.ToolHandlerFor[procPIDParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args procPIDParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.PID == "" {
			return nil, nil, fmt.Errorf("session_id and pid are required")
		}
		if !validPID(args.PID) {
			return nil, nil, fmt.Errorf("pid must be a positive integer")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := fmt.Sprintf(
			"echo '=== status ===' && cat /proc/%s/status 2>/dev/null && "+
				"echo '=== cmdline ===' && tr '\\0' ' ' < /proc/%s/cmdline 2>/dev/null && echo && "+
				"echo '=== exe ===' && readlink /proc/%s/exe 2>/dev/null && "+
				"echo '=== cwd ===' && readlink /proc/%s/cwd 2>/dev/null",
			args.PID, args.PID, args.PID, args.PID,
		)
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func lsofHandler(p session.Provider) mcp.ToolHandlerFor[procPIDParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args procPIDParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.PID == "" {
			return nil, nil, fmt.Errorf("session_id and pid are required")
		}
		if !validPID(args.PID) {
			return nil, nil, fmt.Errorf("pid must be a positive integer")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := fmt.Sprintf("lsof -p %s 2>/dev/null || ls -la /proc/%s/fd 2>/dev/null", args.PID, args.PID)
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

type killParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	PID       string `json:"pid" jsonschema:"Process ID"`
	Signal    string `json:"signal,omitempty" jsonschema:"Signal to send (e.g. TERM, KILL)"`
}

func killHandler(p session.Provider) mcp.ToolHandlerFor[killParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args killParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.PID == "" {
			return nil, nil, fmt.Errorf("session_id and pid are required")
		}
		if !validPID(args.PID) {
			return nil, nil, fmt.Errorf("pid must be a positive integer")
		}
		sig := validSignal(args.Signal)
		if sig == "" {
			return nil, nil, fmt.Errorf("unknown signal; use a number or one of: TERM KILL HUP INT QUIT USR1 USR2 STOP CONT ABRT")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := fmt.Sprintf("sudo -n kill -%s %s", sig, args.PID)
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil || res.ExitCode != 0,
		}, nil, nil
	}
}
