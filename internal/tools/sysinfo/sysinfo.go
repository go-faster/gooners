// Package sysinfo registers MCP tools for system information (network, OS, users).
package sysinfo

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcp.AddTool(s, &mcp.Tool{Name: "sys_net_addrs", Description: "Show network interface addresses and link status (ip addr show)."}, netAddrsHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "sys_os_info", Description: "Get OS info: kernel (uname -a), distribution (/etc/os-release), hostname."}, osInfoHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "sys_uptime", Description: "Show system uptime and load averages."}, uptimeHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "sys_mem", Description: "Show memory and swap usage (free -h)."}, memHandler(p))
}

type sysSessionParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Iface     string `json:"iface,omitempty" jsonschema:"Specific network interface (e.g. eth0)"`
}

func netAddrsHandler(p *session.Pool) mcp.ToolHandlerFor[sysSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sysSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		cmd := "ip addr show"
		if args.Iface != "" {
			cmd += " dev " + sshutil.Quote(args.Iface)
		}
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func osInfoHandler(p *session.Pool) mcp.ToolHandlerFor[sysSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sysSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		cmd := "hostname; echo '---'; uname -a; echo '---'; cat /etc/os-release 2>/dev/null || cat /etc/redhat-release 2>/dev/null || echo 'os-release: not found'"
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func uptimeHandler(p *session.Pool) mcp.ToolHandlerFor[sysSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sysSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		res, err := p.Run(ctx, args.SessionID, "uptime")
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func memHandler(p *session.Pool) mcp.ToolHandlerFor[sysSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sysSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		res, err := p.Run(ctx, args.SessionID, "free -h")
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}
