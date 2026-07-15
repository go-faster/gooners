package docker

import (
	"context"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox"
)

// Destroy removes id. It is idempotent: not-found and "removal already in
// progress" (AutoRemove racing an explicit Destroy) are both treated as
// success, never as an error.
func (r *Runner) Destroy(ctx context.Context, id string) error {
	_, err := r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err == nil || isIdempotentDestroyError(err) {
		return nil
	}
	return errors.Wrap(err, "remove sandbox container")
}

// isIdempotentDestroyError reports whether err from ContainerRemove means
// "the container is already gone or already being removed" - i.e. Destroy
// already achieved its goal, one way or another.
func isIdempotentDestroyError(err error) bool {
	if err == nil {
		return true
	}
	return cerrdefs.IsNotFound(err) || cerrdefs.IsConflict(err)
}

// List returns every sandbox container labeled [sandbox.LabelSandbox]=true,
// optionally narrowed by f.Deployment and f.Labels. Docker's label filter is
// AND, not OR: every requested label must match, so the empty Filter{} case
// still never returns a container this package didn't create.
func (r *Runner) List(ctx context.Context, f sandbox.Filter) ([]*sandbox.Sandbox, error) {
	filters := client.Filters{}
	filters.Add("label", sandbox.LabelSandbox+"=true")
	if f.Deployment != "" {
		filters.Add("label", sandbox.LabelDeployment+"="+f.Deployment)
	}
	for k, v := range f.Labels {
		filters.Add("label", k+"="+v)
	}

	res, err := r.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, errors.Wrap(err, "list sandbox containers")
	}

	out := make([]*sandbox.Sandbox, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, &sandbox.Sandbox{
			ID:        c.ID,
			Image:     c.Image,
			Network:   networkFromDockerMode(c.HostConfig.NetworkMode),
			CreatedAt: time.Unix(c.Created, 0),
			Labels:    c.Labels,
		})
	}
	return out, nil
}

// networkFromDockerMode is List's best-effort reverse of
// [dockerNetworkMode]: informational only (Runner.List's own Filter
// matching never depends on it), so an unrecognized mode just passes
// through instead of erroring.
func networkFromDockerMode(mode string) sandbox.Network {
	switch mode {
	case "none":
		return sandbox.NetworkNone
	case "", "default", "bridge":
		return sandbox.NetworkOpen
	default:
		return sandbox.Network(mode)
	}
}
