// Package sysinfo registers MCP tools for system information (network, OS, users).
package sysinfo

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_net_addrs", Description: "Show network interface addresses and link status (ip addr show).", Flags: mcputil.ReadOnly}, netAddrsHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_os_info", Description: "Get OS info: kernel (uname -a), distribution (/etc/os-release), hostname.", Flags: mcputil.ReadOnly}, osInfoHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_uptime", Description: "Show system uptime and load averages.", Flags: mcputil.ReadOnly}, uptimeHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_mem", Description: "Show memory and swap usage (free -h).", Flags: mcputil.ReadOnly}, memHandler(p))
}

type sessionParam struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
}

type netAddrsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Iface     string `json:"iface,omitempty" jsonschema:"Specific network interface (e.g. eth0)"`
}

func netAddrsHandler(p *session.Pool) mcp.ToolHandlerFor[netAddrsParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args netAddrsParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "ip addr show"
		if args.Iface != "" {
			cmd += " dev " + sshutil.Quote(args.Iface)
		}
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

func osInfoHandler(p *session.Pool) mcp.ToolHandlerFor[sessionParam, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sessionParam) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "hostname; echo '---'; uname -a; echo '---'; cat /etc/os-release 2>/dev/null || cat /etc/redhat-release 2>/dev/null || echo 'os-release: not found'"
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

func uptimeHandler(p *session.Pool) mcp.ToolHandlerFor[sessionParam, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sessionParam) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		res, err := p.Run(ctx, args.SessionID, "uptime")
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

func memHandler(p *session.Pool) mcp.ToolHandlerFor[sessionParam, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sessionParam) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		res, err := p.Run(ctx, args.SessionID, "free -h")
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}
