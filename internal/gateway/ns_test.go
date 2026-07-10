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
	err := DetectCollisions("tool", nil, map[string][]string{"a": {"x"}, "b": {"y"}})
	require.NoError(t, err)
}

func TestDetectCollisions_Collide(t *testing.T) {
	err := DetectCollisions("tool", map[string]string{"a": "", "b": ""}, map[string][]string{"a": {"x"}, "b": {"x"}})
	require.Error(t, err)
	var ce *CollisionsError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Conflicts, 2)
}

func TestDetectCollisions_DistinctPrefix(t *testing.T) {
	err := DetectCollisions("tool", map[string]string{"a": "a.", "b": "b."}, map[string][]string{"a": {"x"}, "b": {"x"}})
	require.NoError(t, err)
}

func TestDetectCollisions_Prompts(t *testing.T) {
	// Prompts use DetectCollisions with empty prefix map (namespaced via NamespaceName before calling).
	err := DetectCollisions("tool", map[string]string{}, map[string][]string{"a": {"p1"}, "b": {"p2"}})
	require.NoError(t, err)

	err = DetectCollisions("tool", map[string]string{}, map[string][]string{"a": {"p"}, "b": {"p"}})
	require.Error(t, err)
	var ce *CollisionsError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Conflicts, 2)
}

func TestDetectCollisions_ThreeWay(t *testing.T) {
	err := DetectCollisions("tool", map[string]string{"a": "", "b": "", "c": ""}, map[string][]string{"a": {"x"}, "b": {"x"}, "c": {"x"}})
	require.Error(t, err)
	var ce *CollisionsError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Conflicts, 3)
	for _, c := range ce.Conflicts {
		require.NotEmpty(t, c.Upstream)
		require.NotEmpty(t, c.Tool)
		require.Equal(t, "x", c.Tool)
		require.Equal(t, "x", c.ResultName)
	}
	require.Equal(t, []string{"a", "b", "c"}, []string{ce.Conflicts[0].Upstream, ce.Conflicts[1].Upstream, ce.Conflicts[2].Upstream})
}

func TestDetectCollisions_Deterministic(t *testing.T) {
	prefixes := map[string]string{"d": "", "a": "", "c": "", "b": ""}
	toolSets := map[string][]string{"d": {"tool2"}, "a": {"tool1"}, "c": {"tool1"}, "b": {"tool2"}}
	var want []Collision
	for i := range 20 {
		err := DetectCollisions("tool", prefixes, toolSets)
		require.Error(t, err)
		var ce *CollisionsError
		require.ErrorAs(t, err, &ce)
		if i == 0 {
			want = append([]Collision(nil), ce.Conflicts...)
			require.Equal(t, []Collision{
				{Upstream: "a", Tool: "tool1", ResultName: "tool1"},
				{Upstream: "c", Tool: "tool1", ResultName: "tool1"},
				{Upstream: "b", Tool: "tool2", ResultName: "tool2"},
				{Upstream: "d", Tool: "tool2", ResultName: "tool2"},
			}, ce.Conflicts)
			continue
		}
		require.Equal(t, want, ce.Conflicts)
	}
}
