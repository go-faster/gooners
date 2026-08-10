package main

import (
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/mcpcmd"
)

func TestRunServerUnknownTransport(t *testing.T) {
	var (
		transport = mcpcmd.TransportFlags{
			Transport: "bad",
			Addr:      ":0",
		}
		s = mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	)

	err := transport.Run(t.Context(), mcpcmd.RunOptions{Name: "test", Server: s, Logger: slog.Default()})
	require.ErrorContains(t, err, `unknown transport: "bad"`)
}
