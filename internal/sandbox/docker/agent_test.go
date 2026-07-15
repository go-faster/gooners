package docker

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sandbox/agent"
)

func TestResolveAgentBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		// isExecutableFile checks Unix permission bits and DefaultAgentDir is
		// a Unix-only path: both are only meaningful for the Linux containers
		// sandbox-agent actually runs in, not for the host running this test.
		t.Skipf("skipping: resolveAgentBinary assumes Unix executable bits and paths, not meaningful on %s", runtime.GOOS)
	}

	dir := t.TempDir()
	amd64Path := filepath.Join(dir, "amd64", "sandbox-agent")
	require.NoError(t, os.MkdirAll(filepath.Dir(amd64Path), 0o755))
	require.NoError(t, os.WriteFile(amd64Path, []byte("#!/bin/sh\n"), 0o755))

	// Present but not executable: must not be picked.
	arm64Path := filepath.Join(dir, "arm64", "sandbox-agent")
	require.NoError(t, os.MkdirAll(filepath.Dir(arm64Path), 0o755))
	require.NoError(t, os.WriteFile(arm64Path, []byte("not executable"), 0o644))

	t.Run("found in agent dir", func(t *testing.T) {
		got, err := resolveAgentBinary(dir, "amd64")
		require.NoError(t, err)
		require.Equal(t, amd64Path, got)
	})

	t.Run("present but not executable is rejected", func(t *testing.T) {
		_, err := resolveAgentBinary(dir, "arm64")
		require.Error(t, err)
	})

	t.Run("missing architecture with no host-arch fallback available", func(t *testing.T) {
		_, err := resolveAgentBinary(dir, "riscv64")
		require.Error(t, err)
	})

	t.Run("empty agentDir defaults to DefaultAgentDir", func(t *testing.T) {
		_, err := resolveAgentBinary("", "some-arch-that-does-not-exist")
		require.Error(t, err)
		require.Contains(t, err.Error(), DefaultAgentDir)
	})

	t.Run("PATH fallback only applies to the host's own architecture", func(t *testing.T) {
		// A directory with nothing in it for either "amd64" or "arm64": the
		// PATH fallback must only kick in when the requested arch equals
		// runtime.GOARCH, never for an arbitrary foreign arch.
		empty := t.TempDir()
		other := "amd64"
		if runtime.GOARCH == "amd64" {
			other = "arm64"
		}
		_, err := resolveAgentBinary(empty, other)
		require.Error(t, err, "must not fall back to PATH for a non-host architecture")
	})
}

func TestBuildAgentTar(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "sandbox-agent")
	content := []byte("fake-agent-binary-content")
	require.NoError(t, os.WriteFile(binPath, content, 0o644)) // deliberately not 0755 on disk

	buf, err := buildAgentTar(binPath)
	require.NoError(t, err)

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	var sawDir, sawFile bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		switch hdr.Name {
		case ".gooners/":
			sawDir = true
			require.Equal(t, int64(0o755), hdr.Mode)
			require.Equal(t, byte(tar.TypeDir), hdr.Typeflag)
		case ".gooners/sandbox-agent":
			sawFile = true
			// The binary must be mode 0755 in the tar regardless of its
			// mode on disk: it later runs as the image's own USER, not root.
			require.Equal(t, int64(0o755), hdr.Mode)
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			require.Equal(t, content, data)
		}
	}
	require.True(t, sawDir, "tar must contain a /.gooners directory entry")
	require.True(t, sawFile, "tar must contain the agent binary at "+agent.DefaultPath)
}

func TestIsIdempotentDestroyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is idempotent", err: nil, want: true},
		{name: "not found is idempotent", err: cerrdefs.ErrNotFound.WithMessage("no such container"), want: true},
		{name: "conflict (removal in progress) is idempotent", err: cerrdefs.ErrConflict.WithMessage("removal of container is already in progress"), want: true},
		{name: "other errors are not idempotent", err: cerrdefs.ErrInternal.WithMessage("daemon on fire"), want: false},
		{name: "plain error is not idempotent", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isIdempotentDestroyError(tt.err))
		})
	}
}
