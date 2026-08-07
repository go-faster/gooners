package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testConfigTOML = `
[server]
name = "gw"

[[upstream]]
name = "u1"
kind = "stdio"
command = ["true"]
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestFileSource_Load(t *testing.T) {
	src, err := NewFileSource(FileSourceOptions{Path: writeConfig(t, testConfigTOML)})
	require.NoError(t, err)

	cfg, err := src.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "gw", cfg.Server.Name)
	require.Len(t, cfg.Upstreams, 1)
}

func TestFileSource_LoadErrors(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		src, err := NewFileSource(FileSourceOptions{Path: filepath.Join(t.TempDir(), "absent.toml")})
		require.NoError(t, err)
		_, err = src.Load(t.Context())
		require.Error(t, err)
	})
	t.Run("invalid toml", func(t *testing.T) {
		src, err := NewFileSource(FileSourceOptions{Path: writeConfig(t, "not toml at all = ")})
		require.NoError(t, err)
		_, err = src.Load(t.Context())
		require.Error(t, err)
	})
	t.Run("invalid config", func(t *testing.T) {
		src, err := NewFileSource(FileSourceOptions{Path: writeConfig(t, "[server]\nname = \"gw\"\n")})
		require.NoError(t, err)
		_, err = src.Load(t.Context())
		require.ErrorContains(t, err, "at least one upstream")
	})
}

func TestNewFileSource_RequiresPath(t *testing.T) {
	_, err := NewFileSource(FileSourceOptions{})
	require.ErrorContains(t, err, "path is required")
}

func TestFileSource_Changed(t *testing.T) {
	path := writeConfig(t, testConfigTOML)
	src, err := NewFileSource(FileSourceOptions{Path: path})
	require.NoError(t, err)

	// First observation has no baseline yet, so any readable file is a change.
	require.True(t, src.changed())
	require.False(t, src.changed())

	// A rewrite with identical bytes must not churn upstream connections.
	require.NoError(t, os.WriteFile(path, []byte(testConfigTOML), 0o600))
	require.False(t, src.changed())

	require.NoError(t, os.WriteFile(path, []byte(testConfigTOML+"\n[redact]\nenabled = true\n"), 0o600))
	require.True(t, src.changed())

	// An unreadable file is not reported as a change; its return is.
	require.NoError(t, os.Remove(path))
	require.False(t, src.changed())
	require.NoError(t, os.WriteFile(path, []byte(testConfigTOML), 0o600))
	require.True(t, src.changed())
}

func TestFileSource_LoadResetsBaseline(t *testing.T) {
	path := writeConfig(t, testConfigTOML)
	src, err := NewFileSource(FileSourceOptions{Path: path})
	require.NoError(t, err)

	_, err = src.Load(t.Context())
	require.NoError(t, err)
	require.False(t, src.changed())
}

func TestFileSource_WatchPolls(t *testing.T) {
	path := writeConfig(t, testConfigTOML)
	src, err := NewFileSource(FileSourceOptions{
		Path:     path,
		Interval: time.Millisecond,
		Signals:  []os.Signal{},
	})
	require.NoError(t, err)
	_, err = src.Load(t.Context())
	require.NoError(t, err)

	ch := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Watch(ctx, ch) }()

	require.NoError(t, os.WriteFile(path, []byte(testConfigTOML+"\n[redact]\nenabled = true\n"), 0o600))
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no change reported")
	}

	cancel()
	require.NoError(t, <-done)
}

// Watch must return when its context ends even if nobody reads the channel.
func TestFileSource_WatchUnblocksOnCancel(t *testing.T) {
	src, err := NewFileSource(FileSourceOptions{
		Path:     writeConfig(t, testConfigTOML),
		Interval: time.Millisecond,
		Signals:  []os.Signal{},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- src.Watch(ctx, make(chan struct{})) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}
