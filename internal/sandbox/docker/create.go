package docker

import (
	"context"
	"strings"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox"
)

// pullTimeout bounds an image pull so an unreachable registry cannot hang
// Create indefinitely.
const pullTimeout = 5 * time.Minute

// createRetryBudget bounds the total wall-clock time spent retrying the
// transient rootfs-mount race in Create's doc comment. A fixed try count runs
// out under load because some rootless-overlayfs daemons take seconds — and
// the async snapshot GC of a just-removed container can lag into the next
// container's start — so this uses a time budget (backoff's exponential
// schedule of 500ms up to 60s) that reliably outlasts that settle window.
const createRetryBudget = 45 * time.Second

// Create pulls spec.Image if absent, creates a container hardened per
// Runner's Policy, injects the architecture-matched sandbox-agent binary
// (before the container ever starts), and starts it. The container's PID 1
// is an idle placeholder (see idleCmd in container.go) - the actual sandbox
// agent is started later, per-Dial, over `docker exec`.
//
// The whole create+inject+start sequence (not just the failing step alone)
// is retried on a transient rootfs-mount race some rootless-overlayfs Docker
// daemons exhibit ("device or resource busy" mounting a container's rootfs
// when a Start lands immediately after another container's teardown reused
// the same snapshot ID window): AutoRemove reaps a container whose Start
// failed, so a stale ID from a previous attempt can't just be retried in
// place - each attempt must be a fresh container.
func (r *Runner) Create(ctx context.Context, spec sandbox.Spec) (*sandbox.Sandbox, error) {
	arch, err := r.ensureImage(ctx, spec.Image)
	if err != nil {
		return nil, err
	}

	agentBin, err := resolveAgentBinary(r.agentDir, arch)
	if err != nil {
		return nil, err
	}

	labels := r.labels()
	createOpts, err := containerOptions(spec, r.policy, labels)
	if err != nil {
		return nil, err
	}

	return backoff.Retry(ctx, func() (*sandbox.Sandbox, error) {
		// Serialize the mount-sensitive window; see Runner.createMu.
		r.createMu.Lock()
		sb, err := r.createOnce(ctx, spec, agentBin, createOpts, labels)
		r.createMu.Unlock()
		if err != nil && !isTransientMountError(err) {
			return nil, backoff.Permanent(err)
		}
		return sb, err
	}, backoff.WithMaxElapsedTime(createRetryBudget))
}

// createOnce is one attempt at Create's full sequence: it always either
// returns a started sandbox or cleans up any container it made before
// returning the error.
func (r *Runner) createOnce(
	ctx context.Context,
	spec sandbox.Spec,
	agentBin string,
	createOpts client.ContainerCreateOptions,
	labels map[string]string,
) (*sandbox.Sandbox, error) {
	createRes, err := r.cli.ContainerCreate(ctx, createOpts)
	if err != nil {
		return nil, errors.Wrap(err, "create sandbox container")
	}
	id := createRes.ID

	tarBuf, err := buildAgentTar(agentBin)
	if err != nil {
		r.destroyBestEffort(ctx, id)
		return nil, err
	}

	// CopyToContainer works on a created-but-not-started container, and
	// doing it here (rather than after Start) avoids racing an entrypoint
	// that might exit immediately. It requires a writable rootfs, which is
	// why Policy.ReadOnlyRootfs is false for now (see container.go TODO).
	if _, err := r.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{
		DestinationPath: "/",
		Content:         tarBuf,
	}); err != nil {
		r.destroyBestEffort(ctx, id)
		return nil, errors.Wrap(err, "copy sandbox agent into container")
	}

	if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		r.destroyBestEffort(ctx, id)
		return nil, errors.Wrap(err, "start sandbox container")
	}

	return &sandbox.Sandbox{
		ID:        id,
		Image:     spec.Image,
		Network:   spec.Network,
		CreatedAt: time.Now(),
		Labels:    labels,
	}, nil
}

// ensureImage returns image's Architecture (e.g. "amd64", "arm64"), pulling
// it first if not already present locally. The allow-list gate on image
// already happened in Manager, via Policy.Validate, before Create was ever
// called; this does not repeat that check.
func (r *Runner) ensureImage(ctx context.Context, image string) (string, error) {
	inspect, err := r.cli.ImageInspect(ctx, image)
	if err == nil {
		return inspect.Architecture, nil
	}
	if !cerrdefs.IsNotFound(err) {
		return "", errors.Wrap(err, "inspect sandbox image")
	}

	pullCtx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	resp, err := r.cli.ImagePull(pullCtx, image, client.ImagePullOptions{})
	if err != nil {
		return "", errors.Wrap(err, "pull sandbox image")
	}
	// Wait drains the progress stream (required - an unread pull response
	// body otherwise leaks) and reports the final pull error, if any.
	if err := resp.Wait(pullCtx); err != nil {
		return "", errors.Wrap(err, "pull sandbox image")
	}

	inspect, err = r.cli.ImageInspect(ctx, image)
	if err != nil {
		return "", errors.Wrap(err, "inspect sandbox image after pull")
	}
	return inspect.Architecture, nil
}

// isTransientMountError reports whether err looks like the rootless-overlayfs
// snapshotter race described in Create's doc comment ("device or resource
// busy" mounting a container's rootfs) rather than a real, permanent
// failure.
func isTransientMountError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "resource busy") || strings.Contains(msg, "resource temporarily unavailable")
}

// destroyBestEffort is used on Create's own error paths, after a container
// exists but before Create can return it to the caller: nobody else knows
// about this ID yet, so Create must clean it up itself.
func (r *Runner) destroyBestEffort(ctx context.Context, id string) {
	if err := r.Destroy(ctx, id); err != nil {
		r.logger.Warn("sandbox/docker: cleanup after failed create did not remove container", "id", id, "err", err)
	}
}
