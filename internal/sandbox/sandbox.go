package sandbox

import "time"

// Sandbox is a created, backend-specific sandbox instance.
type Sandbox struct {
	// ID is the backend-specific identifier (e.g. a Docker container ID),
	// opaque to callers.
	ID        string
	Image     string
	Network   Network
	CreatedAt time.Time
	// Labels are backend-specific metadata (e.g.
	// dev.gooners.{sandbox,deployment,instance,session}) used for the
	// startup orphan sweep and [Runner.List] filtering.
	Labels map[string]string
}

// Filter selects a subset of sandboxes for [Runner.List].
type Filter struct {
	// Deployment, if set, matches only sandboxes labeled with this
	// deployment. Scoping by deployment is what stops two sandbox-mcp
	// processes on one container host from reaping each other's sandboxes.
	Deployment string
	// Labels, if non-empty, matches only sandboxes whose Labels are a
	// superset of this map.
	Labels map[string]string
}
