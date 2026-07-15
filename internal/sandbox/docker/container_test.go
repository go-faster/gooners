package docker

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/go-faster/sdk/gold"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sandbox"
)

func TestMain(m *testing.M) {
	gold.Init()
	os.Exit(m.Run())
}

func TestContainerOptions(t *testing.T) {
	tests := []struct {
		name   string
		spec   sandbox.Spec
		policy sandbox.Policy
	}{
		{
			name: "default policy, none network",
			spec: sandbox.Spec{Image: "alpine:latest"},
			policy: sandbox.Policy{
				DropCaps:        []string{"ALL"},
				NoNewPrivileges: true,
				MemoryBytes:     512 * 1024 * 1024,
				CPUs:            1,
				PidsLimit:       256,
			},
		},
		{
			name: "open network, env, workdir, custom hardening",
			spec: sandbox.Spec{
				Image:   "python:3.12-slim",
				Network: sandbox.NetworkOpen,
				Env:     map[string]string{"B_VAR": "2", "A_VAR": "1"},
				Workdir: "/work",
			},
			policy: sandbox.Policy{
				DropCaps:        []string{"ALL"},
				NoNewPrivileges: true,
				MemoryBytes:     1024 * 1024 * 1024,
				CPUs:            2,
				PidsLimit:       512,
				RuntimeHandler:  "runsc",
				User:            "1000:1000",
			},
		},
		{
			name:   "egress-proxy tier is rejected as not implemented",
			spec:   sandbox.Spec{Image: "alpine:latest", Network: sandbox.NetworkEgressProxy},
			policy: sandbox.Policy{DropCaps: []string{"ALL"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := map[string]string{
				"dev.gooners.sandbox":    "true",
				"dev.gooners.deployment": "test-deployment",
				"dev.gooners.instance":   "instance-1",
				"dev.gooners.session":    "session-1",
			}

			opts, err := containerOptions(tt.spec, tt.policy, labels)
			if tt.spec.Network == sandbox.NetworkEgressProxy {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			pretty, err := json.MarshalIndent(opts, "", "  ")
			require.NoError(t, err)
			gold.Str(t, string(pretty)+"\n", "container_options_"+safeName(tt.name)+".json")
		})
	}
}

func TestContainerOptions_Invariants(t *testing.T) {
	labels := map[string]string{"dev.gooners.sandbox": "true"}
	policy := sandbox.Policy{
		DropCaps:        []string{"ALL"},
		NoNewPrivileges: true,
		MemoryBytes:     256 * 1024 * 1024,
		CPUs:            0.5,
		PidsLimit:       128,
		RuntimeHandler:  "runsc",
	}

	opts, err := containerOptions(sandbox.Spec{Image: "alpine:latest"}, policy, labels)
	require.NoError(t, err)

	require.Equal(t, "none", string(opts.HostConfig.NetworkMode))
	require.Equal(t, []string{"ALL"}, opts.HostConfig.CapDrop)
	require.Contains(t, opts.HostConfig.SecurityOpt, "no-new-privileges:true")
	require.NotNil(t, opts.HostConfig.PidsLimit)
	require.Equal(t, int64(128), *opts.HostConfig.PidsLimit)
	require.Equal(t, int64(256*1024*1024), opts.HostConfig.Memory)
	require.Equal(t, "runsc", opts.HostConfig.Runtime)
	require.True(t, opts.HostConfig.AutoRemove)
	require.False(t, opts.HostConfig.ReadonlyRootfs)
	require.Equal(t, labels, opts.Config.Labels)
}

func TestEnvSlice(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{name: "empty", env: nil, want: nil},
		{
			name: "sorted deterministically",
			env:  map[string]string{"B": "2", "A": "1", "C": "3"},
			want: []string{"A=1", "B=2", "C=3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, envSlice(tt.env))
		})
	}
}

func TestDockerNetworkMode(t *testing.T) {
	tests := []struct {
		name    string
		network sandbox.Network
		want    string
		wantErr bool
	}{
		{name: "empty defaults to none", network: "", want: "none"},
		{name: "none", network: sandbox.NetworkNone, want: "none"},
		{name: "open maps to bridge", network: sandbox.NetworkOpen, want: "bridge"},
		{name: "egress-proxy not implemented", network: sandbox.NetworkEgressProxy, wantErr: true},
		{name: "unknown tier rejected", network: sandbox.Network("bogus"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dockerNetworkMode(tt.network)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, string(got))
		})
	}
}

func safeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			if len(out) > 0 && out[len(out)-1] != '_' {
				out = append(out, '_')
			}
		}
	}
	return string(out)
}
