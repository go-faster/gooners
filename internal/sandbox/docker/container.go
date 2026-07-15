package docker

import (
	"sort"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox"
)

// idleCmd keeps a sandbox container's PID 1 alive indefinitely, without
// relying on `sleep infinity` (unsupported by some minimal busybox/coreutils
// builds): a shell loop is supported everywhere the sandbox agent's own
// shell requirement already assumes a shell exists (see
// internal/sandbox/agent.findShell). The image's own ENTRYPOINT/CMD are
// irrelevant here - the actual per-session workload runs over `docker exec`
// via Dial, never as this container's PID 1.
var idleCmd = []string{"sh", "-c", "while true; do sleep 3600; done"}

// containerOptions maps a validated [sandbox.Spec] and the operator's
// [sandbox.Policy] into a Docker container-create request. It is a pure
// function - no client, no Docker daemon involved - so it can be
// table-and-golden-tested without a daemon.
//
// Hardening (CapDrop, NoNewPrivileges, resource limits, RuntimeHandler, User)
// comes only from policy, never from spec: spec is agent-facing and already
// passed through [sandbox.Policy.Validate] by the time it reaches here, but
// containerOptions re-asserts the boundary by simply never reading hardening
// fields from spec at all.
func containerOptions(spec sandbox.Spec, policy sandbox.Policy, labels map[string]string) (client.ContainerCreateOptions, error) {
	networkMode, err := dockerNetworkMode(spec.Network)
	if err != nil {
		return client.ContainerCreateOptions{}, err
	}

	var securityOpt []string
	if policy.NoNewPrivileges {
		securityOpt = append(securityOpt, "no-new-privileges:true")
	}

	cfg := &container.Config{
		Image: spec.Image,
		// Clear the image's own ENTRYPOINT/CMD: the container's PID 1 is
		// just an idle placeholder (see idleCmd), not the sandboxed
		// workload.
		Entrypoint: []string{},
		Cmd:        idleCmd,
		Env:        envSlice(spec.Env),
		WorkingDir: spec.Workdir,
		Labels:     labels,
		User:       policy.User,
	}

	pidsLimit := policy.PidsLimit
	hostCfg := &container.HostConfig{
		NetworkMode: networkMode,
		AutoRemove:  true,
		CapDrop:     policy.DropCaps,
		SecurityOpt: securityOpt,
		Runtime:     policy.RuntimeHandler,
		// ReadonlyRootfs is always false for now: CopyToContainer (how the
		// agent binary is injected) requires a writable rootfs.
		// TODO(follow-up): populate a named volume with the agent binary
		// once, mount it read-only at /.gooners, and make this policy value
		// meaningful.
		ReadonlyRootfs: policy.ReadOnlyRootfs,
		Resources: container.Resources{
			Memory:    policy.MemoryBytes,
			NanoCPUs:  int64(policy.CPUs * 1e9),
			PidsLimit: &pidsLimit,
		},
	}

	return client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hostCfg,
	}, nil
}

// dockerNetworkMode maps a [sandbox.Network] tier to a Docker NetworkMode.
// The agent cannot influence this - only [sandbox.Policy.AllowedNetworks]
// decides which tiers are reachable at all, and that check already happened
// in [sandbox.Policy.Validate] before spec reaches here.
func dockerNetworkMode(n sandbox.Network) (container.NetworkMode, error) {
	switch n {
	case sandbox.NetworkNone, "":
		// No NIC at all - the whole point: SSH still works because it rides
		// the container's exec/attach stream, not the network.
		return container.NetworkMode("none"), nil
	case sandbox.NetworkOpen:
		return container.NetworkMode("bridge"), nil
	case sandbox.NetworkEgressProxy:
		return "", errors.Errorf("sandbox/docker: network tier %q is not implemented yet (follow-up)", n)
	default:
		return "", errors.Errorf("sandbox/docker: unknown network tier %q", n)
	}
}

// envSlice converts a Spec's env map into Docker's "KEY=VALUE" slice form,
// sorted for deterministic output (golden-testable, reproducible container
// creates).
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
