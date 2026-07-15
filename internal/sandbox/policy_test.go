package sandbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicy_setDefaults(t *testing.T) {
	var p Policy
	p.setDefaults()

	require.Equal(t, "alpine:latest", p.DefaultImage)
	require.Equal(t, []string{"alpine:latest"}, p.AllowedImages)
	require.Equal(t, []Network{NetworkNone}, p.AllowedNetworks)
	require.Equal(t, int64(512*1024*1024), p.MemoryBytes)
	require.InDelta(t, 1.0, p.CPUs, 0)
	require.Equal(t, int64(256), p.PidsLimit)
	require.Equal(t, []string{"ALL"}, p.DropCaps)
	require.True(t, p.NoNewPrivileges)
	require.False(t, p.ReadOnlyRootfs)
	require.Equal(t, 15*time.Minute, p.IdleTimeout)
	require.Equal(t, 5, p.MaxPerOwner)
	require.Equal(t, "default", p.Deployment)
}

func TestPolicy_setDefaults_PreservesExplicitValues(t *testing.T) {
	p := Policy{
		DefaultImage:    "registry.internal/base:v1",
		AllowedImages:   []string{"registry.internal/*"},
		AllowedNetworks: []Network{NetworkOpen},
		MemoryBytes:     1 << 30,
		CPUs:            2,
		PidsLimit:       64,
		DropCaps:        []string{"ALL", "SYS_PTRACE"},
		ReadOnlyRootfs:  true,
		IdleTimeout:     5 * time.Minute,
		MaxPerOwner:     2,
		Deployment:      "prod",
	}
	p.setDefaults()

	require.Equal(t, "registry.internal/base:v1", p.DefaultImage)
	require.Equal(t, []string{"registry.internal/*"}, p.AllowedImages)
	require.Equal(t, []Network{NetworkOpen}, p.AllowedNetworks)
	require.Equal(t, int64(1<<30), p.MemoryBytes)
	require.InDelta(t, 2.0, p.CPUs, 0)
	require.Equal(t, int64(64), p.PidsLimit)
	require.Equal(t, []string{"ALL", "SYS_PTRACE"}, p.DropCaps)
	require.True(t, p.ReadOnlyRootfs)
	require.Equal(t, 5*time.Minute, p.IdleTimeout)
	require.Equal(t, 2, p.MaxPerOwner)
	require.Equal(t, "prod", p.Deployment)
}

func TestPolicy_setDefaults_NoNewPrivilegesAlwaysTrue(t *testing.T) {
	p := Policy{NoNewPrivileges: false}
	p.setDefaults()
	require.True(t, p.NoNewPrivileges, "NoNewPrivileges is hardening, not a configurable toggle")
}

func TestPolicy_Validate(t *testing.T) {
	cases := []struct {
		name      string
		policy    Policy
		spec      Spec
		want      Spec
		wantError string
	}{
		{
			name:   "empty spec uses policy defaults",
			policy: Policy{DefaultImage: "alpine:3.20"},
			spec:   Spec{},
			want:   Spec{Image: "alpine:3.20", Network: NetworkNone},
		},
		{
			name: "explicit image within allow-list glob",
			policy: Policy{
				DefaultImage:  "alpine:3.20",
				AllowedImages: []string{"alpine:*", "registry.internal/*"},
			},
			spec: Spec{Image: "registry.internal/tools:latest"},
			want: Spec{Image: "registry.internal/tools:latest", Network: NetworkNone},
		},
		{
			name: "image not in allow-list is rejected",
			policy: Policy{
				DefaultImage:  "alpine:3.20",
				AllowedImages: []string{"alpine:*"},
			},
			spec:      Spec{Image: "docker.io/library/ubuntu:latest"},
			wantError: `image "docker.io/library/ubuntu:latest" is not in the allowed image list`,
		},
		{
			name: "explicit allowed network tier",
			policy: Policy{
				DefaultImage:    "alpine:3.20",
				AllowedNetworks: []Network{NetworkNone, NetworkOpen},
			},
			spec: Spec{Network: NetworkOpen},
			want: Spec{Image: "alpine:3.20", Network: NetworkOpen},
		},
		{
			name: "network tier not allowed is rejected",
			policy: Policy{
				DefaultImage:    "alpine:3.20",
				AllowedNetworks: []Network{NetworkNone},
			},
			spec:      Spec{Network: NetworkOpen},
			wantError: `network tier "open" is not allowed`,
		},
		{
			name:   "env and workdir pass through untouched",
			policy: Policy{DefaultImage: "alpine:3.20"},
			spec:   Spec{Env: map[string]string{"FOO": "bar"}, Workdir: "/work"},
			want:   Spec{Image: "alpine:3.20", Network: NetworkNone, Env: map[string]string{"FOO": "bar"}, Workdir: "/work"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.policy.Validate(tc.spec)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
