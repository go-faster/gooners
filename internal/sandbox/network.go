// Package sandbox defines the backend-neutral surface for per-session
// container sandboxes: the Spec agents can request, the Policy operators
// enforce, and the Runner interface a backend (Docker, Kubernetes, ...)
// implements.
package sandbox

// Network is a network isolation tier. The agent cannot influence which tier
// a sandbox runs in - only the Policy decides which tiers are allowed at all.
type Network string

const (
	// NetworkNone is the default: no NIC, loopback only. SSH still works
	// because it rides the container's exec/attach stream, not the network.
	NetworkNone Network = "none"
	// NetworkEgressProxy gives an internal bridge with no default route; the
	// sole exit is a CONNECT proxy with a domain allowlist. Not implemented
	// by any Runner yet.
	NetworkEgressProxy Network = "egress-proxy"
	// NetworkOpen gives full egress. Opt-in only.
	NetworkOpen Network = "open"
)
