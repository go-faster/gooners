package disk

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *server.MCPServer, p *session.Pool) {
	s.AddTool(mcp.NewTool("disk_lsblk",
		mcp.WithDescription("List block devices with their sizes, types, and mount points (lsblk)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("device"),
	), lsblkHandler(p))

	s.AddTool(mcp.NewTool("disk_mounts",
		mcp.WithDescription("Show current mounts with filesystem type and options (/proc/mounts)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("fstype"),
	), mountsHandler(p))

	s.AddTool(mcp.NewTool("disk_df",
		mcp.WithDescription("Report disk space usage (df -h)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path"),
	), dfHandler(p))
}

func lsblkHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,LABEL,UUID"
		if dev := req.GetString("device", ""); dev != "" {
			cmd += " " + sshutil.Quote(dev)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func mountsHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "cat /proc/mounts"
		if fs := req.GetString("fstype", ""); fs != "" {
			cmd += " | awk '$3 == " + sshutil.Quote(fs) + "'"
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func dfHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		if id == "" {
			return mcp.NewToolResultError("session_id is required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "df -h"
		if path := req.GetString("path", ""); path != "" {
			cmd += " " + sshutil.Quote(path)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}
