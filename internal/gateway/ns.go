// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"slices"

	"github.com/go-faster/errors"
)

// Collision records one name collision after prefix application.
type Collision struct {
	Upstream   string
	Tool       string
	ResultName string
}

// CollisionsError is returned by DetectCollisions when final tool names overlap.
type CollisionsError struct {
	Conflicts []Collision
}

func (e *CollisionsError) Error() string {
	return fmt.Sprintf("%d tool name collisions after prefixing", len(e.Conflicts))
}

// NamespaceName returns the gateway-exposed tool name for an upstream tool.
func NamespaceName(prefix, toolName string) string {
	if prefix == "" {
		return toolName
	}
	return prefix + toolName
}

// DetectCollisions returns an error if any resulting names collide across upstreams.
func DetectCollisions(prefixes map[string]string, toolSets map[string][]string) error {
	names := make([]string, 0, len(toolSets))
	for up := range toolSets {
		names = append(names, up)
	}
	slices.Sort(names)

	type owner struct {
		upstream string
		tool     string
	}
	owners := map[string][]owner{}

	for _, up := range names {
		pfx := prefixes[up]
		for _, t := range toolSets[up] {
			res := NamespaceName(pfx, t)
			owners[res] = append(owners[res], owner{upstream: up, tool: t})
		}
	}

	var colliding []string
	for res, os := range owners {
		if len(os) > 1 {
			colliding = append(colliding, res)
		}
	}
	slices.Sort(colliding)

	var conflicts []Collision
	for _, res := range colliding {
		for _, o := range owners[res] {
			conflicts = append(conflicts, Collision{Upstream: o.upstream, Tool: o.tool, ResultName: res})
		}
	}
	if len(conflicts) > 0 {
		return errors.Wrap(&CollisionsError{Conflicts: conflicts}, "tool name collisions")
	}
	return nil
}
