// Package gatewaytransport builds mcp.Transport implementations from gateway config.
package gatewaytransport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuild_Stdio(t *testing.T) {
	tr, _, err := Build(context.Background(), "stdio", []string{"true"}, "", nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestBuild_BadKind(t *testing.T) {
	_, _, err := Build(context.Background(), "nope", nil, "", nil, nil, nil)
	require.Error(t, err)
}

func TestBuild_Interpolate(t *testing.T) {
	resolve := func(name string) (string, error) {
		if name == "k" {
			return "v", nil
		}
		return "", nil
	}
	tr, _, err := Build(context.Background(), "stdio", []string{"true"}, "", map[string]string{"X": "{secret:k}"}, nil, resolve)
	require.NoError(t, err)
	require.NotNil(t, tr)
}
