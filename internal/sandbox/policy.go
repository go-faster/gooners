package sandbox

import (
	"path"
	"slices"
	"time"

	"github.com/go-faster/errors"
)

// Policy is server-side sandbox configuration. Every field here is set by the
// operator (e.g. in cmd/sandbox-mcp's own config) and is never settable by a
// caller's [Spec] - that separation is what makes [Policy.Validate] a trust
// boundary rather than a formality.
type Policy struct {
	// DefaultImage is used when Spec.Image is empty.
	DefaultImage string
	// AllowedImages are path.Match glob patterns; Spec.Image (after
	// defaulting) must match at least one.
	AllowedImages []string
	// AllowedNetworks are the network tiers callers may request via
	// Spec.Network.
	AllowedNetworks []Network

	MemoryBytes int64
	CPUs        float64
	PidsLimit   int64

	// ReadOnlyRootfs mounts the container rootfs read-only. Default false:
	// the first Docker backend injects the agent binary via
	// CopyToContainer, which requires a writable rootfs.
	ReadOnlyRootfs bool
	// DropCaps are Linux capabilities to drop. Default ["ALL"].
	DropCaps []string
	// NoNewPrivileges is always true: it is hardening, not a caller or even
	// operator choice.
	NoNewPrivileges bool
	// RuntimeHandler selects an alternative container runtime (e.g. "runsc"
	// for gVisor, "kata" for Kata Containers). Empty uses the backend's
	// default runtime.
	RuntimeHandler string
	// User is the user the sandboxed process runs as (backend-specific
	// syntax, e.g. Docker's "uid[:gid]"). Empty uses the image's default.
	User string

	// IdleTimeout is how long a sandbox may sit with no SSH activity before
	// it is torn down.
	IdleTimeout time.Duration
	// MaxPerOwner caps sandboxes alive at once per owner.
	MaxPerOwner int
	// Deployment scopes sandbox labels so that two sandbox-mcp processes
	// sharing one container host or Docker socket never reap each other's
	// sandboxes on startup.
	Deployment string
}

func (p *Policy) setDefaults() {
	if p.DefaultImage == "" {
		p.DefaultImage = "alpine:latest"
	}
	if len(p.AllowedImages) == 0 {
		p.AllowedImages = []string{p.DefaultImage}
	}
	if len(p.AllowedNetworks) == 0 {
		p.AllowedNetworks = []Network{NetworkNone}
	}
	if p.MemoryBytes <= 0 {
		p.MemoryBytes = 512 * 1024 * 1024 // 512MiB
	}
	if p.CPUs <= 0 {
		p.CPUs = 1
	}
	if p.PidsLimit <= 0 {
		p.PidsLimit = 256
	}
	if len(p.DropCaps) == 0 {
		p.DropCaps = []string{"ALL"}
	}
	// Not a togglable option: always on, regardless of what a caller sets.
	p.NoNewPrivileges = true
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 15 * time.Minute
	}
	if p.MaxPerOwner <= 0 {
		p.MaxPerOwner = 5
	}
	if p.Deployment == "" {
		p.Deployment = "default"
	}
}

// Validate applies Policy defaults and returns a copy of spec with defaults
// filled in, or an error if spec asks for something Policy doesn't allow.
// Nothing in spec can widen what Policy permits.
func (p *Policy) Validate(spec Spec) (Spec, error) {
	p.setDefaults()

	out := spec
	if out.Image == "" {
		out.Image = p.DefaultImage
	}
	if !p.imageAllowed(out.Image) {
		return Spec{}, errors.Errorf("sandbox: image %q is not in the allowed image list", out.Image)
	}

	if out.Network == "" {
		out.Network = NetworkNone
	}
	if !p.networkAllowed(out.Network) {
		return Spec{}, errors.Errorf("sandbox: network tier %q is not allowed", out.Network)
	}

	return out, nil
}

func (p *Policy) imageAllowed(image string) bool {
	for _, pattern := range p.AllowedImages {
		if ok, err := path.Match(pattern, image); err == nil && ok {
			return true
		}
	}
	return false
}

func (p *Policy) networkAllowed(n Network) bool {
	return slices.Contains(p.AllowedNetworks, n)
}
