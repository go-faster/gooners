// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNamespaceName(t *testing.T) {
	require.Equal(t, "foo", NamespaceName("", "foo"))
	require.Equal(t, "prod.foo", NamespaceName("prod.", "foo"))
}

func TestDetectCollisions_NoPrefix(t *testing.T) {
	err := DetectCollisions(nil, map[string][]string{"a": {"x"}, "b": {"y"}})
	require.NoError(t, err)
}

func TestDetectCollisions_Collide(t *testing.T) {
	err := DetectCollisions(map[string]string{"a": "", "b": ""}, map[string][]string{"a": {"x"}, "b": {"x"}})
	require.Error(t, err)
	var ce *CollisionsError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Conflicts, 2)
}

func TestDetectCollisions_DistinctPrefix(t *testing.T) {
	err := DetectCollisions(map[string]string{"a": "a.", "b": "b."}, map[string][]string{"a": {"x"}, "b": {"x"}})
	require.NoError(t, err)
}

func TestDetectCollisions_Prompts(t *testing.T) {
	// Prompts use DetectCollisions with empty prefix map (namespaced via NamespaceName before calling).
	err := DetectCollisions(map[string]string{}, map[string][]string{"a": {"p1"}, "b": {"p2"}})
	require.NoError(t, err)

	err = DetectCollisions(map[string]string{}, map[string][]string{"a": {"p"}, "b": {"p"}})
	require.Error(t, err)
	var ce *CollisionsError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Conflicts, 2)
}
