package main

import (
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/cmdutil"
)

func TestRunServerUnknownTransport(t *testing.T) {
	var (
		transport = cmdutil.TransportFlags{
			Transport: "bad",
			Addr:      ":0",
		}
		s = mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	)

	err := transport.Run(t.Context(), cmdutil.RunOptions{Name: "test", Server: s, Logger: slog.Default()})
	require.ErrorContains(t, err, `unknown transport: "bad"`)
}
