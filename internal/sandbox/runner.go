package sandbox

import (
	"context"
	"net"
)

// Runner creates and drives sandbox containers on a specific backend
// (Docker, Kubernetes, ...). It knows nothing about SSH: Dial only hands
// back the sandbox agent's stdio as a net.Conn, and the caller drives the
// SSH handshake over it (see internal/sandbox/agent and
// internal/sandbox/streamconn).
type Runner interface {
	Create(ctx context.Context, spec Spec) (*Sandbox, error)
	// Dial starts the sandbox agent inside id and returns its stdio as a
	// net.Conn.
	Dial(ctx context.Context, id string) (net.Conn, error)
	// Destroy removes the sandbox. It is idempotent: destroying an already
	// removed (or never-created) ID is not an error.
	Destroy(ctx context.Context, id string) error
	List(ctx context.Context, f Filter) ([]*Sandbox, error)
	Close() error
}
