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
func BroadcastWarning(s *mcp.Server, component, message string) {
	params := &mcp.LoggingMessageParams{
		Level:  "warning",
		Logger: component,
		Data:   json.RawMessage(fmt.Appendf(nil, `{"message": %q}`, message)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for session := range s.Sessions() {
		_ = session.Log(ctx, params)
	}
}
