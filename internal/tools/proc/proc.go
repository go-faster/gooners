package proc

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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

func Register(s *server.MCPServer, p session.Provider) {
	s.AddTool(mcp.NewTool("proc_list",
		mcp.WithDescription("List running processes (ps aux). Optional user and grep filter."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("user"),
		mcp.WithString("filter"),
		mcp.WithNumber("max_lines"),
	), listHandler(p))

	s.AddTool(mcp.NewTool("proc_info",
		mcp.WithDescription("Show details for a process: /proc/<pid>/status, cmdline, exe, cwd."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("pid", mcp.Required()),
	), infoHandler(p))

	s.AddTool(mcp.NewTool("proc_lsof",
		mcp.WithDescription("List open files for a process. Uses lsof if available, falls back to /proc/<pid>/fd."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("pid", mcp.Required()),
	), lsofHandler(p))

	s.AddTool(mcp.NewTool("proc_kill",
		mcp.WithDescription("Send a signal to a process (uses sudo -n). Default signal: TERM."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("pid", mcp.Required()),
		mcp.WithString("signal"),
	), killHandler(p))
}

func listHandler(p session.Provider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "ps aux"
		if u := req.GetString("user", ""); u != "" {
			cmd = "ps -u " + sshutil.Quote(u) + " aux"
		}
		if f := req.GetString("filter", ""); f != "" {
			cmd = "(" + cmd + ") | grep -i " + sshutil.Quote(f)
		}
		if n := req.GetFloat("max_lines", 0); n > 0 {
			cmd += fmt.Sprintf(" | head -n %d", int64(n))
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func infoHandler(p session.Provider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		pid := req.GetString("pid", "")
		if id == "" || pid == "" {
			return mcp.NewToolResultError("session_id and pid are required"), nil
		}
		if !validPID(pid) {
			return mcp.NewToolResultError("pid must be a positive integer"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := fmt.Sprintf(
			"echo '=== status ===' && cat /proc/%s/status 2>/dev/null && "+
				"echo '=== cmdline ===' && tr '\\0' ' ' < /proc/%s/cmdline 2>/dev/null && echo && "+
				"echo '=== exe ===' && readlink /proc/%s/exe 2>/dev/null && "+
				"echo '=== cwd ===' && readlink /proc/%s/cwd 2>/dev/null",
			pid, pid, pid, pid,
		)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func lsofHandler(p session.Provider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		pid := req.GetString("pid", "")
		if id == "" || pid == "" {
			return mcp.NewToolResultError("session_id and pid are required"), nil
		}
		if !validPID(pid) {
			return mcp.NewToolResultError("pid must be a positive integer"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := fmt.Sprintf(
			"lsof -p %s 2>/dev/null || ls -la /proc/%s/fd 2>/dev/null",
			pid, pid,
		)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func killHandler(p session.Provider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		pid := req.GetString("pid", "")
		if id == "" || pid == "" {
			return mcp.NewToolResultError("session_id and pid are required"), nil
		}
		if !validPID(pid) {
			return mcp.NewToolResultError("pid must be a positive integer"), nil
		}
		sig := validSignal(req.GetString("signal", ""))
		if sig == "" {
			return mcp.NewToolResultError("unknown signal; use a number or one of: TERM KILL HUP INT QUIT USR1 USR2 STOP CONT ABRT"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := fmt.Sprintf("sudo -n kill -%s %s", sig, pid)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}
