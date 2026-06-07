package mcputil_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/mcputil"
)

func TestNewServer(t *testing.T) {
	called := false
	cfg := mcputil.ServerConfig{
		Name:         "test-server",
		Instructions: "test instructions",
		Prompts: []*mcp.Prompt{
			{Name: "test-prompt", Description: "desc"},
		},
		PromptHandler: mcp.PromptHandler(func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			called = true
			return &mcp.GetPromptResult{Description: "ok"}, nil
		}),
	}

	s := mcputil.NewServer(cfg)
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
	_ = called // keep compiler happy
}
