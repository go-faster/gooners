// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-faster/errors"
	"go.uber.org/zap"
)

// Collision records one name collision after prefix application.
type Collision struct {
	Upstream   string
	Tool       string
	ResultName string
}

// CollisionsError is returned by DetectCollisions when final names overlap.
type CollisionsError struct {
	// Kind labels what kind of item collided (e.g. "tool", "prompt",
	// "resource", "resource template"), for error message wording.
	Kind      string
	Conflicts []Collision
	// Counts maps upstream name to the number of items it returned
	// (only populated for upstreams involved in a collision).
	Counts map[string]int
}

// maxCollisionExamples caps how many colliding names are rendered in Error()
// so a config with thousands of collisions doesn't produce an unreadable message.
const maxCollisionExamples = 5

func (e *CollisionsError) Error() string {
	kind := e.Kind
	if kind == "" {
		kind = "tool"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d %s name collisions", len(e.Conflicts), kind)

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
			fmt.Fprintf(&sb, " %s (%d %ss)", up, e.Counts[up], kind)
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
// kind labels the item type for the resulting error message (e.g. "tool", "resource").
func DetectCollisions(kind string, prefixes map[string]string, toolSets map[string][]string) error {
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
		return errors.Wrap(&CollisionsError{Kind: kind, Conflicts: conflicts, Counts: counts}, kind+" name collisions")
	}
	return nil
}

// collisionSet accumulates named items per upstream for one item kind (tool,
// prompt, resource, resource template) across Build's listing pass, then
// checks them for collisions once all upstreams have been added. It keeps
// the raw item alongside its name so identical duplicates (see check) can be
// distinguished from genuine conflicts.
type collisionSet[T any] struct {
	kind       string
	equal      func(a, b T) bool
	sets       map[string][]string
	byUpstream map[string]map[string]T
}

func newCollisionSet[T any](kind string, equal func(a, b T) bool) *collisionSet[T] {
	return &collisionSet[T]{
		kind:       kind,
		equal:      equal,
		sets:       map[string][]string{},
		byUpstream: map[string]map[string]T{},
	}
}

// add registers items for one upstream. name extracts the raw (unprefixed)
// identifier for an item (tool/prompt name or resource URI); keep, if
// non-nil, filters which items participate (used for tools.allow/deny).
func (c *collisionSet[T]) add(upstream string, items []T, name func(T) string, keep func(T) bool) {
	byName := make(map[string]T, len(items))
	for _, it := range items {
		if keep != nil && !keep(it) {
			continue
		}
		n := name(it)
		c.sets[upstream] = append(c.sets[upstream], n)
		byName[n] = it
	}
	c.byUpstream[upstream] = byName
}

// check runs DetectCollisions and, if it fails, drops any collision groups
// whose items are byte-identical across all owners before deciding whether
// to return an error.
func (c *collisionSet[T]) check(logger *zap.Logger, prefixes map[string]string) error {
	err := DetectCollisions(c.kind, prefixes, c.sets)
	if err == nil {
		return nil
	}
	var ce *CollisionsError
	if !errors.As(err, &ce) {
		return err
	}
	if hard := c.dropIdentical(logger, ce); hard != nil {
		return errors.Wrap(hard, c.kind+" name collisions")
	}
	return nil
}

// dropIdentical removes collision groups where every owner's item is
// byte-identical: such overlaps are harmless (the same item exposed
// redundantly by more than one upstream, e.g. vendored docs baked into
// multiple copies of the same image) and are logged as a warning rather
// than failing the build. It returns nil if no hard (non-identical)
// collisions remain, or a new CollisionsError containing only the genuine
// conflicts.
func (c *collisionSet[T]) dropIdentical(logger *zap.Logger, ce *CollisionsError) *CollisionsError {
	var hard []Collision
	for i := 0; i < len(ce.Conflicts); {
		j := i
		for j < len(ce.Conflicts) && ce.Conflicts[j].ResultName == ce.Conflicts[i].ResultName {
			j++
		}
		group := ce.Conflicts[i:j]
		if c.identicalGroup(group) {
			owners := make([]string, 0, len(group))
			for _, cf := range group {
				owners = append(owners, cf.Upstream+":"+cf.Tool)
			}
			logger.Warn(c.kind+" collision ignored: identical definitions",
				zap.String("result_name", group[0].ResultName),
				zap.Strings("owners", owners),
			)
		} else {
			hard = append(hard, group...)
		}
		i = j
	}
	if len(hard) == 0 {
		return nil
	}
	counts := make(map[string]int, len(hard))
	for _, cf := range hard {
		if _, ok := counts[cf.Upstream]; !ok {
			counts[cf.Upstream] = len(c.byUpstream[cf.Upstream])
		}
	}
	return &CollisionsError{Kind: c.kind, Conflicts: hard, Counts: counts}
}

func (c *collisionSet[T]) identicalGroup(group []Collision) bool {
	var (
		first     T
		haveFirst bool
	)
	for _, cf := range group {
		item, ok := c.byUpstream[cf.Upstream][cf.Tool]
		if !ok {
			return false
		}
		if !haveFirst {
			first, haveFirst = item, true
			continue
		}
		if !c.equal(first, item) {
			return false
		}
	}
	return true
}
