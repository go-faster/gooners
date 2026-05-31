package sysinfo

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *server.MCPServer, p *session.Pool) {
	s.AddTool(mcp.NewTool("sys_net_addrs",
		mcp.WithDescription("Show network interface addresses and link status (ip addr show)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("iface"),
	), netAddrsHandler(p))

	s.AddTool(mcp.NewTool("sys_os_info",
		mcp.WithDescription("Get OS info: kernel (uname -a), distribution (/etc/os-release), hostname."),
		mcp.WithString("session_id", mcp.Required()),
	), osInfoHandler(p))

	s.AddTool(mcp.NewTool("sys_uptime",
		mcp.WithDescription("Show system uptime and load averages."),
		mcp.WithString("session_id", mcp.Required()),
	), uptimeHandler(p))

	s.AddTool(mcp.NewTool("sys_mem",
		mcp.WithDescription("Show memory and swap usage (free -h)."),
		mcp.WithString("session_id", mcp.Required()),
	), memHandler(p))
}

func netAddrsHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "ip addr show"
		if iface := req.GetString("iface", ""); iface != "" {
			cmd += " dev " + sshutil.Quote(iface)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func osInfoHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "hostname; echo '---'; uname -a; echo '---'; cat /etc/os-release 2>/dev/null || cat /etc/redhat-release 2>/dev/null || echo 'os-release: not found'"
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func uptimeHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res, err := sshutil.Run(ctx, client, "uptime")
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func memHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res, err := sshutil.Run(ctx, client, "free -h")
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}
