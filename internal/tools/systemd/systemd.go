// Package systemd registers MCP tools for inspecting systemd units.
package systemd

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_status", Description: "Show status of a systemd unit.", Flags: mcputil.ReadOnly}, statusHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_list_units", Description: "List systemd units.", Flags: mcputil.ReadOnly}, listUnitsHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_start", Description: "Start a systemd unit (uses sudo -n)."}, mutatingHandler(p, "start"))
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_stop", Description: "Stop a systemd unit (uses sudo -n).", Flags: mcputil.Destructive}, mutatingHandler(p, "stop"))
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_restart", Description: "Restart a systemd unit (uses sudo -n).", Flags: mcputil.Destructive}, mutatingHandler(p, "restart"))
	mcputil.Register(s, mcputil.ToolDef{Name: "systemctl_reload", Description: "Reload a systemd unit (uses sudo -n)."}, mutatingHandler(p, "reload"))
	mcputil.Register(s, mcputil.ToolDef{Name: "journald_tail", Description: "Query recent journal entries.", Flags: mcputil.ReadOnly}, journalHandler(p))
}

type systemdBaseParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Unit      string `json:"unit" jsonschema:"Name of the systemd unit (e.g. nginx.service)"`
}

func statusHandler(p *session.Pool) mcp.ToolHandlerFor[systemdBaseParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args systemdBaseParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Unit == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and unit are required")
		}
		cmd := "systemctl status " + sshutil.Quote(args.Unit)
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

type listUnitsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	State     string `json:"state,omitempty" jsonschema:"Filter by unit state (e.g. active, failed)"`
	Type      string `json:"type,omitempty" jsonschema:"Filter by unit type (e.g. service, timer)"`
}

func listUnitsHandler(p *session.Pool) mcp.ToolHandlerFor[listUnitsParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args listUnitsParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		cmd := "systemctl list-units"
		if args.State != "" {
			cmd += " --state=" + sshutil.Quote(args.State)
		}
		if args.Type != "" {
			cmd += " --type=" + sshutil.Quote(args.Type)
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

func mutatingHandler(p *session.Pool, action string) mcp.ToolHandlerFor[systemdBaseParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args systemdBaseParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Unit == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and unit are required")
		}
		cmd := "sudo -n systemctl " + action + " " + sshutil.Quote(args.Unit)
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

type journalParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Unit      string  `json:"unit,omitempty" jsonschema:"Filter by systemd unit"`
	Since     string  `json:"since,omitempty" jsonschema:"Show entries starting from this time/date"`
	Until     string  `json:"until,omitempty" jsonschema:"Show entries up to this time/date"`
	Lines     float64 `json:"lines,omitempty" jsonschema:"Number of lines to show"`
	Grep      string  `json:"grep,omitempty" jsonschema:"Filter output by regular expression"`
	Priority  string  `json:"priority,omitempty" jsonschema:"Filter by priority (e.g. err, warning)"`
}

func journalHandler(p *session.Pool) mcp.ToolHandlerFor[journalParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args journalParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id is required")
		}
		lines := args.Lines
		if lines <= 0 || lines > 10_000 {
			lines = 100
		}
		cmd := fmt.Sprintf("journalctl --no-pager -n %d", int64(lines))
		if args.Unit != "" {
			cmd += " -u " + sshutil.Quote(args.Unit)
		}
		if args.Since != "" {
			cmd += " --since=" + sshutil.Quote(args.Since)
		}
		if args.Until != "" {
			cmd += " --until=" + sshutil.Quote(args.Until)
		}
		if args.Grep != "" {
			cmd += " -g " + sshutil.Quote(args.Grep)
		}
		if args.Priority != "" {
			cmd += " -p " + sshutil.Quote(args.Priority)
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
