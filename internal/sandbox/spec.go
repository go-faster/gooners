package sandbox

// Spec is the agent-facing description of a sandbox to create. Every field
// is subject to [Policy.Validate] before it reaches a Runner: nothing here is
// trusted as-is.
type Spec struct {
	// Image is the container image to run. Empty selects Policy.DefaultImage.
	Image string
	// Network is the requested network tier. Empty selects [NetworkNone].
	Network Network
	// Env are additional environment variables for the sandboxed process.
	Env map[string]string
	// Workdir is the working directory inside the sandbox.
	Workdir string
}
