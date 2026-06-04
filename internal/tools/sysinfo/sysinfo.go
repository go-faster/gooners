// Package sysinfo registers MCP tools for system information (network, OS, users).
package sysinfo

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type Runner interface {
	Run(ctx context.Context, sessionID string, cmd string) (sshutil.Result, error)
}

func Register(s *mcp.Server, p Runner) {
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_uptime", Description: "Show system uptime and load average.", Flags: mcputil.ReadOnly}, simpleCmdHandler(p, "uptime"))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_mem", Description: "Show free and used memory (free -h).", Flags: mcputil.ReadOnly}, simpleCmdHandler(p, "free -h"))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_net_addrs", Description: "Show network interfaces and IP addresses (returns JSON if supported, or text fallback).", Flags: mcputil.ReadOnly}, netAddrsHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "sys_os_info", Description: "Show OS release info and kernel version.", Flags: mcputil.ReadOnly}, simpleCmdHandler(p, "cat /etc/os-release 2>/dev/null || cat /etc/redhat-release 2>/dev/null || echo 'os-release: not found'; uname -a"))
}

type sessionParam struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
}

func simpleCmdHandler(p Runner, cmd string) mcp.ToolHandlerFor[sessionParam, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sessionParam) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
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

type netAddrsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Iface     string `json:"iface,omitempty" jsonschema:"Network interface name (e.g. eth0)"`
}

func netAddrsHandler(p Runner) mcp.ToolHandlerFor[netAddrsParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args netAddrsParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "ip -j addr show"
		if args.Iface != "" {
			cmd += " dev " + sshutil.Quote(args.Iface)
		}
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err == nil && res.ExitCode != 0 {
			cmd = "ip addr show"
			if args.Iface != "" {
				cmd += " dev " + sshutil.Quote(args.Iface)
			}
			res, err = p.Run(ctx, args.SessionID, cmd)
		}
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
