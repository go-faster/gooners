// Package router provides host and path based request routing.
package router

import "strings"

// Router routes requests based on HTTP host and URL path.
type Router[T any] struct {
	hosts map[string]*node[T]
	any   *node[T]
}

// New creates a new Router.
func New[T any]() *Router[T] {
	return &Router[T]{
		hosts: make(map[string]*node[T]),
		any:   newNode[T](),
	}
}

type node[T any] struct {
	children map[byte]*node[T]
	value    *T
	isRoute  bool
}

func newNode[T any]() *node[T] {
	return &node[T]{
		children: make(map[byte]*node[T]),
	}
}

// Add registers a route with the given host and path prefix.
// An empty host matches any host.
func (r *Router[T]) Add(host, path string, value T) {
	n := r.any
	if host != "" {
		host = strings.ToLower(host)
		if r.hosts[host] == nil {
			r.hosts[host] = newNode[T]()
		}
		n = r.hosts[host]
	}

	for i := 0; i < len(path); i++ {
		c := path[i]
		if n.children[c] == nil {
			n.children[c] = newNode[T]()
		}
		n = n.children[c]
	}
	n.value = &value
	n.isRoute = true
}

// Lookup finds the best matching route for the given host and path.
// It matches the exact host first, then falls back to any host.
// Within a host, it finds the longest valid path prefix.
func (r *Router[T]) Lookup(host, path string) (v T, ok bool) {
	host = strings.ToLower(host)

	if hostNode := r.hosts[host]; hostNode != nil {
		if val, found := lookupNode(hostNode, path); found {
			return val, true
		}
	}
	return lookupNode(r.any, path)
}

func lookupNode[T any](n *node[T], path string) (v T, ok bool) {
	var best *T

	if n.isRoute {
		if isBoundary(path, 0) {
			best = n.value
		}
	}

	curr := n
	for i := 0; i < len(path); i++ {
		curr = curr.children[path[i]]
		if curr == nil {
			break
		}
		if curr.isRoute {
			// i + 1 is the match length
			if isBoundary(path, i+1) {
				best = curr.value
			}
		}
	}

	if best != nil {
		return *best, true
	}
	return v, false
}

// isBoundary checks if a prefix match is structurally valid.
// This prevents "/staging-mcp" from matching a "/staging" route.
func isBoundary(path string, matchLen int) bool {
	if matchLen == len(path) {
		return true
	}
	if matchLen > 0 && path[matchLen-1] == '/' {
		return true
	}
	if matchLen < len(path) && path[matchLen] == '/' {
		return true
	}
	return false
}
