// Package systemd registers MCP tools for inspecting systemd units.
package systemd

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

func Register(s *mcp.Server, p *session.Pool) {
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_status", Description: "Show status of a systemd unit."}, statusHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_list_units", Description: "List systemd units."}, listUnitsHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_start", Description: "Start a systemd unit (uses sudo -n)."}, mutatingHandler(p, "start"))
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_stop", Description: "Stop a systemd unit (uses sudo -n)."}, mutatingHandler(p, "stop"))
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_restart", Description: "Restart a systemd unit (uses sudo -n)."}, mutatingHandler(p, "restart"))
	mcp.AddTool(s, &mcp.Tool{Name: "systemctl_reload", Description: "Reload a systemd unit (uses sudo -n)."}, mutatingHandler(p, "reload"))
	mcp.AddTool(s, &mcp.Tool{Name: "journald_tail", Description: "Query recent journal entries."}, journalHandler(p))
}

type systemdBaseParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Unit      string `json:"unit" jsonschema:"Name of the systemd unit (e.g. nginx.service)"`
}

func statusHandler(p *session.Pool) mcp.ToolHandlerFor[systemdBaseParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args systemdBaseParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Unit == "" {
			return nil, nil, fmt.Errorf("session_id and unit are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "systemctl status " + sshutil.Quote(args.Unit)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

type listUnitsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	State     string `json:"state,omitempty" jsonschema:"Filter by unit state (e.g. active, failed)"`
	Type      string `json:"type,omitempty" jsonschema:"Filter by unit type (e.g. service, timer)"`
}

func listUnitsHandler(p *session.Pool) mcp.ToolHandlerFor[listUnitsParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args listUnitsParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "systemctl list-units"
		if args.State != "" {
			cmd += " --state=" + sshutil.Quote(args.State)
		}
		if args.Type != "" {
			cmd += " --type=" + sshutil.Quote(args.Type)
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

func mutatingHandler(p *session.Pool, action string) mcp.ToolHandlerFor[systemdBaseParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args systemdBaseParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Unit == "" {
			return nil, nil, fmt.Errorf("session_id and unit are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "sudo -n systemctl " + action + " " + sshutil.Quote(args.Unit)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
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

func journalHandler(p *session.Pool) mcp.ToolHandlerFor[journalParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args journalParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" {
			return nil, nil, fmt.Errorf("session_id is required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
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
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}
