// Package mcputil provides a generic helper for registering MCP tools with proper
// annotations (ReadOnlyHint, DestructiveHint) and defines structured result types
// used across tool packages to enable automatic InputSchema/OutputSchema inference
// while maintaining exact backwards-compatibility with JSON schemas.
package mcputil

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
)

// ToolFlags defines traits/characteristics of a tool using a binary flag set.
type ToolFlags uint32

const (
	// ReadOnly indicates the tool only performs read operations and has no side effects.
	ReadOnly ToolFlags = 1 << iota
	// Destructive indicates the tool can delete, modify, or stop system components.
	Destructive
)

// Has returns true if the flags contain the specified target flag.
func (f ToolFlags) Has(target ToolFlags) bool {
	return f&target != 0
}

// ToolDef holds metadata for registering an MCP tool with annotations.
type ToolDef struct {
	Name        string
	Description string
	Flags       ToolFlags
}

// Register is a generic helper to add a typed tool with annotations for hints.
func Register[In, Out any](s *mcp.Server, def ToolDef, handler mcp.ToolHandlerFor[In, Out]) {
	readOnly := def.Flags.Has(ReadOnly)
	ann := &mcp.ToolAnnotations{
		ReadOnlyHint: readOnly,
	}
	if !readOnly {
		d := def.Flags.Has(Destructive)
		ann.DestructiveHint = &d
	}
	orig := handler
	wrapped := func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, Out, error) {
		session := req.Session
		slog.Info("called tool handler", "tool", def.Name, "session_id", session.ID())
		return orig(ctx, req, args)
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		Annotations: ann,
	}, wrapped)
}

// Common result types for tool outputs, enabling structured output schemas.

// SessionResult is returned by ssh_open, ssh_open_cfg etc.
type SessionResult struct {
	SessionID string `json:"session_id"`
	UserAgent string `json:"user_agent,omitempty"`
	Banner    string `json:"banner,omitempty"`
	Platform  string `json:"platform,omitempty" jsonschema:"Detected OS platform (may be imprecise)"`
}

// SuccessResult for close etc.
type SuccessResult struct {
	OK bool `json:"ok"`
}

// SessionsResult for list.
type SessionsResult struct {
	Sessions []session.SessionInfo `json:"sessions"`
}

// MachinesResult for list_machines.
type MachinesResult struct {
	Machines []session.Machine `json:"machines"`
}

// ExecResult for exec commands that return stdout etc. Matches the shape used in core.
type ExecResult struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	StdoutSize    int64  `json:"stdout_size,omitempty"`
	StderrSize    int64  `json:"stderr_size,omitempty"`
	StdoutSpoolID string `json:"stdout_spool_id,omitempty"`
	StderrSpoolID string `json:"stderr_spool_id,omitempty"`
	Message       string `json:"message,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Error         string `json:"error,omitempty"`
}

// CommandResult for tools that run a command and return its output (disk, fs, proc, sysinfo, systemd).
type CommandResult struct {
	Text string `json:"text"`
}

// UploadResult for upload start.
type UploadResult struct {
	OK       bool   `json:"ok"`
	UploadID string `json:"upload_id"`
}

// UploadStatusResult for upload status.
type UploadStatusResult struct {
	OK              bool    `json:"ok"`
	UploadID        string  `json:"upload_id"`
	BytesUploaded   int64   `json:"bytes_uploaded"`
	TotalBytes      int64   `json:"total_bytes"`
	Percent         float64 `json:"percent"`
	InstantSpeedBPS float64 `json:"instant_speed_bps"`
	AverageSpeedBPS float64 `json:"average_speed_bps"`
	DurationSeconds float64 `json:"duration_seconds"`
	ETASeconds      float64 `json:"eta_seconds"`
	Done            bool    `json:"done"`
	Error           string  `json:"error,omitempty"`
}

// DownloadResult for download start.
type DownloadResult struct {
	OK         bool   `json:"ok"`
	DownloadID string `json:"download_id"`
}

// DownloadStatusResult for download status.
type DownloadStatusResult struct {
	OK              bool    `json:"ok"`
	DownloadID      string  `json:"download_id"`
	BytesDownloaded int64   `json:"bytes_downloaded"`
	TotalBytes      int64   `json:"total_bytes"`
	Percent         float64 `json:"percent"`
	InstantSpeedBPS float64 `json:"instant_speed_bps"`
	AverageSpeedBPS float64 `json:"average_speed_bps"`
	DurationSeconds float64 `json:"duration_seconds"`
	ETASeconds      float64 `json:"eta_seconds"`
	Done            bool    `json:"done"`
	Error           string  `json:"error,omitempty"`
}

// PingResult for ssh_ping.
type PingResult struct {
	Status string `json:"status"`
	Time   string `json:"time"`
}
