package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *server.MCPServer, p *session.Pool) {
	s.AddTool(mcp.NewTool("ssh_open",
		mcp.WithDescription("Open SSH connection using defaults from ~/.ssh/config and keys."),
		mcp.WithString("machine", mcp.Required(), mcp.Description("host, user@host, host:port etc.")),
	), openHandler(p))

	s.AddTool(mcp.NewTool("ssh_open_cfg",
		mcp.WithDescription("Open SSH connection with explicit parameters."),
		mcp.WithString("machine", mcp.Required()),
		mcp.WithString("user"),
		mcp.WithNumber("port"),
		mcp.WithString("key_path"),
		mcp.WithString("password"),
		mcp.WithNumber("timeout_s"),
		mcp.WithString("known_hosts"),
	), openCfgHandler(p))

	s.AddTool(mcp.NewTool("ssh_close",
		mcp.WithDescription("Close an open SSH session."),
		mcp.WithString("session_id", mcp.Required()),
	), closeHandler(p))

	s.AddTool(mcp.NewTool("ssh_list",
		mcp.WithDescription("List all currently open SSH sessions."),
	), listHandler(p))

	s.AddTool(mcp.NewTool("ssh_list_machines",
		mcp.WithDescription("List known machines from ~/.ssh/config (and Includes). Returns only connection name and username."),
	), listMachinesHandler())

	s.AddTool(mcp.NewTool("ssh_exec",
		mcp.WithDescription("Execute a command on an open SSH session."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("command", mcp.Required()),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_s"),
	), execHandler(p, false))

	s.AddTool(mcp.NewTool("ssh_sudo_exec",
		mcp.WithDescription("Execute a command with sudo -n on an open SSH session. Requires passwordless sudo on the target."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("command", mcp.Required()),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_s"),
	), execHandler(p, true))

	s.AddTool(mcp.NewTool("ssh_once_exec",
		mcp.WithDescription("Open a temporary SSH session, run one command, then close it."),
		mcp.WithString("machine", mcp.Required()),
		mcp.WithString("command", mcp.Required()),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_s"),
	), onceHandler(p))
}

func openHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		machine := req.GetString("machine", "")
		if machine == "" {
			return mcp.NewToolResultError("machine is required"), nil
		}
		id, err := p.Open(machine)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, _ := json.Marshal(map[string]string{"session_id": id})
		return mcp.NewToolResultText(string(b)), nil
	}
}

func openCfgHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := session.Config{
			Machine:    req.GetString("machine", ""),
			User:       req.GetString("user", ""),
			KeyPath:    req.GetString("key_path", ""),
			Password:   req.GetString("password", ""),
			KnownHosts: req.GetString("known_hosts", ""),
		}
		if portVal := req.GetFloat("port", 0); portVal > 0 {
			cfg.Port = int(portVal)
		}
		if t := req.GetFloat("timeout_s", 0); t > 0 {
			cfg.TimeoutSec = int(t)
		}
		if cfg.Machine == "" {
			return mcp.NewToolResultError("machine is required"), nil
		}
		id, err := p.OpenCfg(cfg)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, _ := json.Marshal(map[string]string{"session_id": id})
		return mcp.NewToolResultText(string(b)), nil
	}
}

func closeHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		_ = p.Close(id)
		b, _ := json.Marshal(map[string]bool{"ok": true})
		return mcp.NewToolResultText(string(b)), nil
	}
}

func listHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		list := p.List()
		b, _ := json.Marshal(list)
		return mcp.NewToolResultText(string(b)), nil
	}
}

func execHandler(p *session.Pool, sudo bool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		cmd := req.GetString("command", "")
		cwd := req.GetString("cwd", "")
		if id == "" || cmd == "" {
			return mcp.NewToolResultError("session_id and command are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		full := cmd
		if cwd != "" {
			full = "cd " + sshutil.Quote(cwd) + " && " + cmd
		}
		if sudo {
			full = "sudo -n -- sh -c " + sshutil.Quote(full)
		}
		t := req.GetFloat("timeout_s", 0)
		c := ctx
		if t > 0 {
			var cancel context.CancelFunc
			c, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
			defer cancel()
		}
		res, err := sshutil.Run(c, client, full)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func onceHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		machine := req.GetString("machine", "")
		cmd := req.GetString("command", "")
		cwd := req.GetString("cwd", "")
		if machine == "" || cmd == "" {
			return mcp.NewToolResultError("machine and command are required"), nil
		}
		id, err := p.Open(machine)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer p.Close(id) //nolint:errcheck // close in defer for once handler, error not actionable
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		full := cmd
		if cwd != "" {
			full = "cd " + sshutil.Quote(cwd) + " && " + cmd
		}
		t := req.GetFloat("timeout_s", 0)
		c := ctx
		if t > 0 {
			var cancel context.CancelFunc
			c, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
			defer cancel()
		}
		res, err := sshutil.Run(c, client, full)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func listMachinesHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		machines := session.ListMachines()
		b, err := json.Marshal(machines)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}
