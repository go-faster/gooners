// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"

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
	seen := map[string]string{} // resultName -> upstream
	var conflicts []Collision
	for up, tools := range toolSets {
		pfx := prefixes[up]
		for _, t := range tools {
			res := NamespaceName(pfx, t)
			if prev, ok := seen[res]; ok {
				conflicts = append(conflicts,
					Collision{Upstream: prev, Tool: "", ResultName: res},
					Collision{Upstream: up, Tool: t, ResultName: res},
				)
			}
			seen[res] = up
		}
	}
	if len(conflicts) > 0 {
		return errors.Wrap(&CollisionsError{Conflicts: conflicts}, "tool name collisions")
	}
	return nil
}
