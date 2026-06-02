// Package disk registers MCP tools for disk and filesystem inspection (lsblk, mounts).
package disk

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcp.AddTool(s, &mcp.Tool{Name: "disk_lsblk", Description: "List block devices with their sizes, types, and mount points (lsblk)."}, lsblkHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "disk_mounts", Description: "Show current mounts with filesystem type and options (/proc/mounts)."}, mountsHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "disk_df", Description: "Report disk space usage (df -h)."}, dfHandler(p))
}

type diskSessionParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Device    string `json:"device,omitempty" jsonschema:"Block device name (e.g. sda)"`
	FSType    string `json:"fstype,omitempty" jsonschema:"Filter by filesystem type"`
	Path      string `json:"path,omitempty" jsonschema:"Path to check disk usage for"`
}

func lsblkHandler(p *session.Pool) mcp.ToolHandlerFor[diskSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args diskSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,LABEL,UUID"
		if args.Device != "" {
			cmd += " " + sshutil.Quote(args.Device)
		}
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func mountsHandler(p *session.Pool) mcp.ToolHandlerFor[diskSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args diskSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "cat /proc/mounts"
		if args.FSType != "" {
			cmd += " | awk '$3 == " + sshutil.Quote(args.FSType) + "'"
		}
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}

func dfHandler(p *session.Pool) mcp.ToolHandlerFor[diskSessionParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args diskSessionParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "df -h"
		if args.Path != "" {
			cmd += " " + sshutil.Quote(args.Path)
		}
		res, err := sshutil.Run(ctx, client, cmd, sshutil.RunOptions{})
		if err != nil {
			res.Error = err.Error()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}, nil, nil
	}
}
