// Package core registers the core MCP tools for opening SSH sessions and running commands.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type RegisterOptions struct {
	// DisableSudo prevents registration of the ssh_sudo_exec tool.
	DisableSudo bool
	// SudoPassword is a server-level fallback used when the per-call sudo_password
	// field is empty. Supports file, env, and credential-command sources.
	SudoPassword SudoPasswordProvider
}

func Register(s *mcp.Server, p *session.Pool, opts RegisterOptions) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_open",
		Description: "Open SSH connection using defaults from ~/.ssh/config and keys.",
	}, openHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_open_cfg",
		Description: "Open SSH connection with explicit parameters.",
	}, openCfgHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_close",
		Description: "Close an open SSH session.",
		Flags:       mcputil.Destructive,
	}, closeHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_list",
		Description: "List all currently open SSH sessions.",
		Flags:       mcputil.ReadOnly,
	}, listHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_list_machines",
		Description: "List known machines from ~/.ssh/config (and Includes). Returns only connection name and username.",
		Flags:       mcputil.ReadOnly,
	}, listMachinesHandler())

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_exec",
		Description: "Execute a command on an open SSH session. Prefer specialized tools (cat, grep, find, ls, stat, du, proc_list, etc.) over this when they cover the task.",
	}, execHandler(p, false, nil))

	if !opts.DisableSudo {
		mcputil.Register(s, mcputil.ToolDef{
			Name:        "ssh_sudo_exec",
			Description: "Execute a command with sudo on an open SSH session. Prefer specialized tools when they cover the task; use this only when elevated privileges are required. If sudo requires a password, pass it via sudo_password or configure a server-level source (-sudo-password-file/-env/-cmd). Otherwise uses sudo -n.",
		}, execHandler(p, true, opts.SudoPassword))
	}

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_once_exec",
		Description: "Open a temporary SSH session, run one command, then close it. Prefer specialized tools (cat, grep, find, ls, stat, du, proc_list, etc.) over this when they cover the task.",
	}, onceHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_ping",
		Description: "Check if an SSH session is alive by sending a keepalive ping.",
		Flags:       mcputil.ReadOnly,
	}, pingHandler(p))
}

type openParams struct {
	Machine string `json:"machine" jsonschema:"host, user@host, host:port etc."`
}

func openHandler(p *session.Pool) mcp.ToolHandlerFor[openParams, mcputil.SessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openParams) (*mcp.CallToolResult, mcputil.SessionResult, error) {
		if args.Machine == "" {
			return nil, mcputil.SessionResult{}, fmt.Errorf("machine is required")
		}

		openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		id, err := p.Open(openCtx, args.Machine)
		if err != nil {
			return nil, mcputil.SessionResult{}, err
		}
		return nil, mcputil.SessionResult{SessionID: id}, nil
	}
}

type openCfgParams struct {
	Machine    string `json:"machine" jsonschema:"Host to connect to"`
	User       string `json:"user,omitempty" jsonschema:"SSH username"`
	Port       int    `json:"port,omitempty" jsonschema:"SSH port"`
	KeyPath    string `json:"key_path,omitempty" jsonschema:"Path to private key"`
	Password   string `json:"password,omitempty" jsonschema:"SSH password"`
	TimeoutSec int    `json:"timeout_s,omitempty" jsonschema:"Connection timeout in seconds"`
	KnownHosts string `json:"known_hosts,omitempty" jsonschema:"Path to known_hosts file"`
}

func openCfgHandler(p *session.Pool) mcp.ToolHandlerFor[openCfgParams, mcputil.SessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openCfgParams) (*mcp.CallToolResult, mcputil.SessionResult, error) {
		if args.Machine == "" {
			return nil, mcputil.SessionResult{}, fmt.Errorf("machine is required")
		}
		cfg := session.Config{
			Machine:    args.Machine,
			User:       args.User,
			Port:       args.Port,
			KeyPath:    args.KeyPath,
			Password:   args.Password,
			TimeoutSec: args.TimeoutSec,
			KnownHosts: args.KnownHosts,
		}
		timeout := 30
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		openCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		id, err := p.OpenCfg(openCtx, cfg)
		if err != nil {
			return nil, mcputil.SessionResult{}, err
		}
		return nil, mcputil.SessionResult{SessionID: id}, nil
	}
}

type closeParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session to close"`
}

func closeHandler(p *session.Pool) mcp.ToolHandlerFor[closeParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args closeParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id is required")
		}
		_ = p.Close(ctx, args.SessionID)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func listHandler(p *session.Pool) mcp.ToolHandlerFor[struct{}, mcputil.SessionsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcputil.SessionsResult, error) {
		list, err := p.List(ctx)
		if err != nil {
			return nil, mcputil.SessionsResult{}, err
		}
		return nil, mcputil.SessionsResult{Sessions: list}, nil
	}
}

func listMachinesHandler() mcp.ToolHandlerFor[struct{}, mcputil.MachinesResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcputil.MachinesResult, error) {
		machines := session.ListMachines()
		return nil, mcputil.MachinesResult{Machines: machines}, nil
	}
}

type execParams struct {
	SessionID    string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Command      string `json:"command" jsonschema:"Command to execute"`
	Description  string `json:"description,omitempty" jsonschema:"Optional description of what this command will do (appended as a comment)"`
	Cwd          string `json:"cwd,omitempty" jsonschema:"Working directory for the command"`
	TimeoutSec   int    `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
	SudoPassword string `json:"sudo_password,omitempty" jsonschema:"Sudo password if required"`
}

func execHandler(p *session.Pool, sudo bool, sudoPasswd SudoPasswordProvider) mcp.ToolHandlerFor[execParams, mcputil.ExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args execParams) (*mcp.CallToolResult, mcputil.ExecResult, error) {
		if args.SessionID == "" || args.Command == "" {
			return nil, mcputil.ExecResult{}, fmt.Errorf("session_id and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, mcputil.ExecResult{}, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		sudoPwd := args.SudoPassword
		if sudoPwd == "" && sudo && sudoPasswd != nil {
			pwd, err := sudoPasswd.Password(ctx)
			if err != nil {
				return nil, mcputil.ExecResult{}, fmt.Errorf("resolving sudo password: %w", err)
			}
			sudoPwd = pwd
		}
		timeout := p.CommandTimeout()
		if args.TimeoutSec > 0 {
			timeout = time.Duration(args.TimeoutSec) * time.Second
		}
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res := p.Exec(execCtx, session.ExecRequest{
			SessionID:    args.SessionID,
			Command:      args.Command,
			Description:  args.Description,
			Cwd:          args.Cwd,
			Sudo:         sudo,
			SudoPassword: sudoPwd,
		})
		return execResult(res)
	}
}

type onceParams struct {
	Machine     string `json:"machine" jsonschema:"Host to connect to"`
	Command     string `json:"command" jsonschema:"Command to execute"`
	Description string `json:"description,omitempty" jsonschema:"Optional description of what this command will do (appended as a comment)"`
	Cwd         string `json:"cwd,omitempty" jsonschema:"Working directory for the command"`
	TimeoutSec  int    `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
}

func onceHandler(p *session.Pool) mcp.ToolHandlerFor[onceParams, mcputil.ExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args onceParams) (*mcp.CallToolResult, mcputil.ExecResult, error) {
		if args.Machine == "" || args.Command == "" {
			return nil, mcputil.ExecResult{}, fmt.Errorf("machine and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, mcputil.ExecResult{}, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		timeout := p.CommandTimeout()
		if args.TimeoutSec > 0 {
			timeout = time.Duration(args.TimeoutSec) * time.Second
		}
		onceCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		id, err := p.Open(onceCtx, args.Machine)
		if err != nil {
			return nil, mcputil.ExecResult{}, err
		}
		defer func() { _ = p.Close(ctx, id) }() // Use parent context for closing

		res := p.Exec(onceCtx, session.ExecRequest{
			SessionID:   id,
			Command:     args.Command,
			Description: args.Description,
			Cwd:         args.Cwd,
		})
		return execResult(res)
	}
}

//nolint:unparam // satisfies mcp.ToolHandlerFor signature pattern even if unused
func execResult(res session.ExecResponse) (*mcp.CallToolResult, mcputil.ExecResult, error) {
	e := mcputil.ExecResult{
		Stdout: res.Stdout,
		Stderr: res.Stderr,
	}
	if res.ExitCode != 0 {
		e.ExitCode = res.ExitCode
	}
	if res.Err != nil {
		e.Error = res.Err.Error()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(e)}},
		IsError: res.Err != nil,
	}, e, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type pingParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session to ping"`
}

func pingHandler(p *session.Pool) mcp.ToolHandlerFor[pingParams, mcputil.PingResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args pingParams) (*mcp.CallToolResult, mcputil.PingResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.PingResult{}, fmt.Errorf("session_id is required")
		}

		pingCtx, cancel := context.WithTimeout(ctx, p.CommandTimeout())
		defer cancel()

		dur, err := p.Ping(pingCtx, args.SessionID)
		if err != nil {
			return nil, mcputil.PingResult{}, fmt.Errorf("ping failed: %w", err)
		}
		return nil, mcputil.PingResult{
			Status: "ok",
			Time:   dur.String(),
		}, nil
	}
}
