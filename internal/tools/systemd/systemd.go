package systemd

import (
	"context"
	"fmt"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func Register(s *server.MCPServer, p *session.Pool) {
	s.AddTool(mcp.NewTool("systemctl_status",
		mcp.WithDescription("Show status of a systemd unit."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit", mcp.Required()),
	), statusHandler(p))

	s.AddTool(mcp.NewTool("systemctl_list_units",
		mcp.WithDescription("List systemd units."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("state"),
		mcp.WithString("type"),
	), listUnitsHandler(p))

	s.AddTool(mcp.NewTool("systemctl_start",
		mcp.WithDescription("Start a systemd unit (uses sudo -n)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit", mcp.Required()),
	), mutatingHandler(p, "start"))

	s.AddTool(mcp.NewTool("systemctl_stop",
		mcp.WithDescription("Stop a systemd unit (uses sudo -n)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit", mcp.Required()),
	), mutatingHandler(p, "stop"))

	s.AddTool(mcp.NewTool("systemctl_restart",
		mcp.WithDescription("Restart a systemd unit (uses sudo -n)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit", mcp.Required()),
	), mutatingHandler(p, "restart"))

	s.AddTool(mcp.NewTool("systemctl_reload",
		mcp.WithDescription("Reload a systemd unit (uses sudo -n)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit", mcp.Required()),
	), mutatingHandler(p, "reload"))

	s.AddTool(mcp.NewTool("journald_tail",
		mcp.WithDescription("Query recent journal entries."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("unit"),
		mcp.WithString("since"),
		mcp.WithString("until"),
		mcp.WithNumber("lines"),
		mcp.WithString("grep"),
		mcp.WithString("priority"),
	), journalHandler(p))
}

func statusHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		unit := req.GetString("unit", "")
		if id == "" || unit == "" {
			return mcp.NewToolResultError("session_id and unit are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "systemctl status " + sshutil.Quote(unit)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func listUnitsHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "systemctl list-units"
		if st := req.GetString("state", ""); st != "" {
			cmd += " --state=" + sshutil.Quote(st)
		}
		if t := req.GetString("type", ""); t != "" {
			cmd += " --type=" + sshutil.Quote(t)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func mutatingHandler(p *session.Pool, action string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		unit := req.GetString("unit", "")
		if id == "" || unit == "" {
			return mcp.NewToolResultError("session_id and unit are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "sudo -n systemctl " + action + " " + sshutil.Quote(unit)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func journalHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		lines := req.GetFloat("lines", 0)
		if lines <= 0 || lines > 10_000 {
			lines = 100
		}
		cmd := fmt.Sprintf("journalctl --no-pager -n %d", int64(lines))
		if u := req.GetString("unit", ""); u != "" {
			cmd += " -u " + sshutil.Quote(u)
		}
		if since := req.GetString("since", ""); since != "" {
			cmd += " --since=" + sshutil.Quote(since)
		}
		if until := req.GetString("until", ""); until != "" {
			cmd += " --until=" + sshutil.Quote(until)
		}
		if g := req.GetString("grep", ""); g != "" {
			cmd += " -g " + sshutil.Quote(g)
		}
		if pri := req.GetString("priority", ""); pri != "" {
			cmd += " -p " + sshutil.Quote(pri)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}
