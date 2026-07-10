// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"slices"
	"strings"

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
	// ToolCounts maps upstream name to the number of tools it returned
	// (only populated for upstreams involved in a collision).
	ToolCounts map[string]int
}

// maxCollisionExamples caps how many colliding names are rendered in Error()
// so a config with thousands of collisions doesn't produce an unreadable message.
const maxCollisionExamples = 5

func (e *CollisionsError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d tool name collisions after prefixing", len(e.Conflicts))

	var (
		resultNames []string
		byResult    = map[string][]Collision{}
		upstreams   = map[string]struct{}{}
	)
	for _, c := range e.Conflicts {
		if _, ok := byResult[c.ResultName]; !ok {
			resultNames = append(resultNames, c.ResultName)
		}
		byResult[c.ResultName] = append(byResult[c.ResultName], c)
		upstreams[c.Upstream] = struct{}{}
	}
	slices.Sort(resultNames)

	if len(upstreams) > 0 {
		names := make([]string, 0, len(upstreams))
		for up := range upstreams {
			names = append(names, up)
		}
		slices.Sort(names)
		fmt.Fprintf(&sb, " across upstreams:")
		for _, up := range names {
			fmt.Fprintf(&sb, " %s (%d tools)", up, e.ToolCounts[up])
		}
	}

	if n := len(resultNames); n > 0 {
		fmt.Fprintf(&sb, "; examples:")
		shown := min(n, maxCollisionExamples)
		for _, res := range resultNames[:shown] {
			owners := make([]string, 0, len(byResult[res]))
			for _, c := range byResult[res] {
				owners = append(owners, fmt.Sprintf("%s:%s", c.Upstream, c.Tool))
			}
			fmt.Fprintf(&sb, " %q<-[%s]", res, strings.Join(owners, ", "))
		}
		if n > shown {
			fmt.Fprintf(&sb, " (and %d more)", n-shown)
		}
	}

	return sb.String()
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
		counts := make(map[string]int, len(colliding))
		for _, res := range colliding {
			for _, o := range owners[res] {
				if _, ok := counts[o.upstream]; !ok {
					counts[o.upstream] = len(toolSets[o.upstream])
				}
			}
		}
		return errors.Wrap(&CollisionsError{Conflicts: conflicts, ToolCounts: counts}, "tool name collisions")
	}
	return nil
}
