// Package mcputil provides shared MCP server initialization and event logic.
package mcputil

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerConfig contains configuration for creating a new standardized MCP server.
type ServerConfig struct {
	Name          string
	Instructions  string
	Logger        *slog.Logger
	Prompts       []*mcp.Prompt
	PromptHandler mcp.PromptHandler
}

// NewServer creates a new MCP server with standard options and versioning.
func NewServer(cfg ServerConfig) *mcp.Server {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}

	opts := &mcp.ServerOptions{
		Instructions: cfg.Instructions,
		Logger:       cfg.Logger,
	}

	s := mcp.NewServer(&mcp.Implementation{Name: cfg.Name, Version: version}, opts)

	if len(cfg.Prompts) > 0 && cfg.PromptHandler != nil {
		for _, p := range cfg.Prompts {
			s.AddPrompt(p, cfg.PromptHandler)
		}
	}

	return s
}

// BroadcastWarning sends a warning log message to all connected MCP clients.
//
// It relies on MCP logging, which revision 2026-07-28 deprecates (SEP-2577)
// without a protocol replacement: the advice is stderr or OpenTelemetry, and
// neither reaches the agent. A client receives the message only if it is on an
// older revision and has set a logging level; for everyone else this is a
// no-op.
func BroadcastWarning(s *mcp.Server, component, message string) {
	//nolint:staticcheck // SA1019: MCP logging is deprecated with no replacement that reaches the client.
	params := &mcp.LoggingMessageParams{
		Level:  "warning",
		Logger: component,
		Data:   json.RawMessage(fmt.Appendf(nil, `{"message": %q}`, message)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for session := range s.Sessions() {
		//nolint:staticcheck // SA1019: see above.
		_ = session.Log(ctx, params)
	}
}
