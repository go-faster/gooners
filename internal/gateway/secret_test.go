// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretResolver_Value(t *testing.T) {
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", Value: "v"}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "v", v)
}

func TestSecretResolver_Env(t *testing.T) {
	t.Setenv("X", "fromenv")
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", Env: "X"}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "fromenv", v)
}

func TestSecretResolver_File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(p, []byte("fileval\n"), 0o600))
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", File: p}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "fileval", v)
}

func TestSecretResolver_Missing(t *testing.T) {
	r, err := NewSecretResolver(nil, slog.Default())
	require.NoError(t, err)
	_, err = r.Resolve(context.Background(), "nope")
	require.ErrorIs(t, err, ErrSecretNotFound)
}

func TestSecretResolver_Interpolate(t *testing.T) {
	r, _ := NewSecretResolver([]SecretConfig{{Name: "k", Value: "vv"}}, slog.Default())
	out, err := Interpolate(t.Context(), "a{secret:k}b", r)
	require.NoError(t, err)
	require.Equal(t, "avvb", out)

	_, err = Interpolate(t.Context(), "a{secret:missing}b", r)
	require.Error(t, err)
}

func TestSecretResolver_File_Rotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(p, []byte("old\n"), 0o600))
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", File: p}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "old", v)

	require.NoError(t, os.WriteFile(p, []byte("new\n"), 0o600))
	v, err = r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "new", v)
}

func TestSecretResolver_File_FallbackOnReadError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(p, []byte("good\n"), 0o600))
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", File: p}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "good", v)

	require.NoError(t, os.Remove(p))
	v, err = r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "good", v)
}

func TestSecretResolver_Env_FallbackOnEmpty(t *testing.T) {
	name := "E_" + t.Name()
	require.NoError(t, os.Setenv(name, "v1"))
	t.Cleanup(func() { _ = os.Unsetenv(name) })
	r, err := NewSecretResolver([]SecretConfig{{Name: "s", Env: name}}, slog.Default())
	require.NoError(t, err)
	v, err := r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "v1", v)

	require.NoError(t, os.Setenv(name, ""))
	v, err = r.Resolve(context.Background(), "s")
	require.NoError(t, err)
	require.Equal(t, "v1", v)
}

func TestSecretResolver_Command_NoCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command secret source uses sh, not available on Windows")
	}
	dir := t.TempDir()
	cnt := filepath.Join(dir, "cnt")
	require.NoError(t, os.WriteFile(cnt, []byte("0\n"), 0o600))
	// command reads and increments a counter file to guarantee fresh different value each run
	cmd := `sh -c 'f="` + cnt + `"; n=$(cat "$f"); n=$((n+1)); echo "$n" > "$f"; echo "$n"'`
	r, _ := NewSecretResolver([]SecretConfig{{Name: "c", Command: cmd}}, slog.Default())
	v1, _ := r.Resolve(context.Background(), "c")
	v2, _ := r.Resolve(context.Background(), "c")
	require.NotEqual(t, v1, v2)
}
