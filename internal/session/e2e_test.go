package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func newSudoTestContainer(t *testing.T) (addr, user, password string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image: "lscr.io/linuxserver/openssh-server:latest",
		Env: map[string]string{
			"USER_NAME":       "test",
			"PASSWORD_ACCESS": "true",
			"USER_PASSWORD":   "secret",
			"SUDO_ACCESS":     "true",
		},
		ExposedPorts: []string{"2222/tcp"},
		WaitingFor:   wait.ForListeningPort("2222/tcp").WithStartupTimeout(90 * time.Second),
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		// Common when running in environments without Docker (CI without daemon, etc.)
		t.Skipf("skipping sudo integration test: could not start container: %v", err)
	}

	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})

	host, err := ctr.Host(ctx)
	require.NoError(t, err)

	port, err := ctr.MappedPort(ctx, "2222/tcp")
	require.NoError(t, err)

	addr = fmt.Sprintf("%s:%s", host, port.Port())
	user = "test"
	password = "secret"

	return addr, user, password
}

func TestSudoExec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
		return
	}

	addr, user, password := newSudoTestContainer(t)

	p := NewPool()
	ctx := t.Context()

	go p.Run(ctx)

	id, err := p.OpenCfg(ctx, Config{
		Machine:    addr,
		User:       user,
		Password:   password,
		KnownHosts: "insecure",
	})
	require.NoError(t, err)

	// Execute a command with Sudo and SudoPassword.
	// Since the container requires "secret" for sudo, this should succeed.
	res := p.Exec(ctx, ExecRequest{
		SessionID:    id,
		Command:      "whoami",
		Sudo:         true,
		SudoPassword: password,
	})

	require.NoError(t, res.Err)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, "root\n", res.Stdout)

	// Execute with missing sudo password should fail
	resFail := p.Exec(ctx, ExecRequest{
		SessionID:    id,
		Command:      "whoami",
		Sudo:         true,
		SudoPassword: "wrong_password",
	})

	require.NotEqual(t, 0, resFail.ExitCode)
}
