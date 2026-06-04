// Package disk registers MCP tools for disk and filesystem inspection (lsblk, mounts).
package disk

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "disk_lsblk",
		Description: "List block devices with their sizes, types, and mount points (lsblk).",
		Flags:       mcputil.ReadOnly,
	}, lsblkHandler(p))
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "disk_mounts",
		Description: "Show current mounts with filesystem type and options (/proc/mounts).",
		Flags:       mcputil.ReadOnly,
	}, mountsHandler(p))
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "disk_df",
		Description: "Report disk space usage (df -h).",
		Flags:       mcputil.ReadOnly,
	}, dfHandler(p))
}

type lsblkParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Device    string `json:"device,omitempty" jsonschema:"Block device name (e.g. sda)"`
}

type mountsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	FSType    string `json:"fstype,omitempty" jsonschema:"Filter by filesystem type"`
}

type dfParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string `json:"path,omitempty" jsonschema:"Path to check disk usage for"`
}

func lsblkHandler(p *session.Pool) mcp.ToolHandlerFor[lsblkParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args lsblkParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,LABEL,UUID"
		if args.Device != "" {
			cmd += " " + sshutil.Quote(args.Device)
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

func mountsHandler(p *session.Pool) mcp.ToolHandlerFor[mountsParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args mountsParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "cat /proc/mounts"
		if args.FSType != "" {
			cmd += " | awk '$3 == " + sshutil.Quote(args.FSType) + "'"
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

func dfHandler(p *session.Pool) mcp.ToolHandlerFor[dfParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args dfParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "df -h"
		if args.Path != "" {
			cmd += " " + sshutil.Quote(args.Path)
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
