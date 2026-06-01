// Package core registers the core MCP tools for opening SSH sessions and running commands.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
)

type RegisterOptions struct {
	// DisableSudo prevents registration of the ssh_sudo_exec tool.
	DisableSudo bool
	// SudoPassword is a server-level fallback used when the per-call sudo_password
	// field is empty. Supports file, env, and credential-command sources.
	SudoPassword SudoPasswordProvider
}

func Register(s *mcp.Server, p *session.Pool, opts RegisterOptions) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_open",
		Description: "Open SSH connection using defaults from ~/.ssh/config and keys.",
	}, openHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_open_cfg",
		Description: "Open SSH connection with explicit parameters.",
	}, openCfgHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_close",
		Description: "Close an open SSH session.",
	}, closeHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_list",
		Description: "List all currently open SSH sessions.",
	}, listHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_list_machines",
		Description: "List known machines from ~/.ssh/config (and Includes). Returns only connection name and username.",
	}, listMachinesHandler())

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_exec",
		Description: "Execute a command on an open SSH session.",
	}, execHandler(p, false, nil))

	if !opts.DisableSudo {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "ssh_sudo_exec",
			Description: "Execute a command with sudo on an open SSH session. If sudo requires a password, pass it via sudo_password or configure a server-level source (-sudo-password-file/-env/-cmd). Otherwise uses sudo -n.",
		}, execHandler(p, true, opts.SudoPassword))
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_once_exec",
		Description: "Open a temporary SSH session, run one command, then close it.",
	}, onceHandler(p))
}

type openParams struct {
	Machine string `json:"machine" jsonschema:"host, user@host, host:port etc."`
}

func openHandler(p *session.Pool) mcp.ToolHandlerFor[openParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openParams) (*mcp.CallToolResult, any, error) {
		if args.Machine == "" {
			return nil, nil, fmt.Errorf("machine is required")
		}

		openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		id, err := p.Open(openCtx, args.Machine)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"session_id": id}, nil
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

func openCfgHandler(p *session.Pool) mcp.ToolHandlerFor[openCfgParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openCfgParams) (*mcp.CallToolResult, any, error) {
		if args.Machine == "" {
			return nil, nil, fmt.Errorf("machine is required")
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
			return nil, nil, err
		}
		return nil, map[string]string{"session_id": id}, nil
	}
}

type closeParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session to close"`
}

func closeHandler(p *session.Pool) mcp.ToolHandlerFor[closeParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args closeParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		_ = p.Close(ctx, args.SessionID)
		return nil, map[string]bool{"ok": true}, nil
	}
}

func listHandler(p *session.Pool) mcp.ToolHandlerFor[struct{}, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		list, err := p.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		return nil, list, nil
	}
}

func listMachinesHandler() mcp.ToolHandlerFor[struct{}, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		machines := session.ListMachines()
		return nil, machines, nil
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

func execHandler(p *session.Pool, sudo bool, sudoPasswd SudoPasswordProvider) mcp.ToolHandlerFor[execParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args execParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Command == "" {
			return nil, nil, fmt.Errorf("session_id and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, nil, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		sudoPwd := args.SudoPassword
		if sudoPwd == "" && sudo && sudoPasswd != nil {
			pwd, err := sudoPasswd.Password(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving sudo password: %w", err)
			}
			sudoPwd = pwd
		}
		timeout := 60
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
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

func onceHandler(p *session.Pool) mcp.ToolHandlerFor[onceParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args onceParams) (*mcp.CallToolResult, any, error) {
		if args.Machine == "" || args.Command == "" {
			return nil, nil, fmt.Errorf("machine and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, nil, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		timeout := 60
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		onceCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		id, err := p.Open(onceCtx, args.Machine)
		if err != nil {
			return nil, nil, err
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
func execResult(res session.ExecResponse) (*mcp.CallToolResult, any, error) {
	obj := map[string]any{"stdout": res.Stdout, "stderr": res.Stderr}
	if res.ExitCode != 0 {
		obj["exit_code"] = res.ExitCode
	}
	if res.Err != nil {
		obj["error"] = res.Err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(obj)}},
			IsError: true,
		}, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(obj)}},
	}, nil, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
