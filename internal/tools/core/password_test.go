package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvPasswordProvider(t *testing.T) {
	t.Setenv("TEST_SSH_PASS", "secret")

	p := &EnvPasswordProvider{VarName: "TEST_SSH_PASS"}
	pwd, err := p.Password(context.Background(), "any-machine")
	require.NoError(t, err)
	require.Equal(t, "secret", pwd)
}

func TestEnvPasswordProvider_empty(t *testing.T) {
	t.Setenv("TEST_SSH_PASS_EMPTY", "")

	p := &EnvPasswordProvider{VarName: "TEST_SSH_PASS_EMPTY"}
	_, err := p.Password(context.Background(), "any-machine")
	require.Error(t, err)
}

func TestFilePasswordProvider(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "pass")
	require.NoError(t, os.WriteFile(tmp, []byte("hunter2\n"), 0o600))

	p := &FilePasswordProvider{Path: tmp}
	pwd, err := p.Password(context.Background(), "any-machine")
	require.NoError(t, err)
	require.Equal(t, "hunter2", pwd)
}

func TestConfigFilePasswordProvider(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "passwords")
	content := "# comment\nweb-01 = hunter2\ndb-01 = s3cr3t\n"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o600))

	p := &ConfigFilePasswordProvider{Path: tmp}

	t.Run("known machine", func(t *testing.T) {
		pwd, err := p.Password(context.Background(), "web-01")
		require.NoError(t, err)
		require.Equal(t, "hunter2", pwd)
	})

	t.Run("another known machine", func(t *testing.T) {
		pwd, err := p.Password(context.Background(), "db-01")
		require.NoError(t, err)
		require.Equal(t, "s3cr3t", pwd)
	})

	t.Run("unknown machine returns ErrPasswordNotFound", func(t *testing.T) {
		_, err := p.Password(context.Background(), "unknown")
		require.ErrorIs(t, err, ErrPasswordNotFound)
	})

	t.Run("user@host:port falls back to host", func(t *testing.T) {
		// Config has "web-01 = hunter2"; lookup via "root@web-01:22" must match.
		pwd, err := p.Password(context.Background(), "root@web-01:22")
		require.NoError(t, err)
		require.Equal(t, "hunter2", pwd)
	})

	t.Run("host:port falls back to host", func(t *testing.T) {
		pwd, err := p.Password(context.Background(), "web-01:222")
		require.NoError(t, err)
		require.Equal(t, "hunter2", pwd)
	})
}

func TestMachineKeys(t *testing.T) {
	tests := []struct {
		machine string
		want    []string
	}{
		{"192.168.1.1", []string{"192.168.1.1"}},
		{"192.168.1.1:222", []string{"192.168.1.1:222", "192.168.1.1"}},
		{"root@192.168.1.1:222", []string{"root@192.168.1.1:222", "192.168.1.1:222", "192.168.1.1"}},
		{"root@192.168.1.1", []string{"root@192.168.1.1", "192.168.1.1"}},
	}
	for _, tc := range tests {
		t.Run(tc.machine, func(t *testing.T) {
			require.Equal(t, tc.want, machineKeys(tc.machine))
		})
	}
}

func TestCommandPasswordProvider(t *testing.T) {
	p := &CommandPasswordProvider{Command: "echo"}
	pwd, err := p.Password(context.Background(), "web-01")
	require.NoError(t, err)
	require.Equal(t, "web-01", pwd)

	// Second call should return cached result.
	pwd2, err := p.Password(context.Background(), "web-01")
	require.NoError(t, err)
	require.Equal(t, pwd, pwd2)

	// Different machine gets its own entry.
	pwd3, err := p.Password(context.Background(), "db-01")
	require.NoError(t, err)
	require.Equal(t, "db-01", pwd3)
}
