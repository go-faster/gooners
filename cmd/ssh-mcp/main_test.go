package main

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRunServerUnknownTransport(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	err := runServer(context.Background(), s, "bad", ":0")
	require.ErrorContains(t, err, `unknown transport: "bad"`)
}
