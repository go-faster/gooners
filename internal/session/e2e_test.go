package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/e2e"
)

func TestSudoExec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
		return
	}

	addr, user, password, cleanup, err := e2e.NewSudoTestContainer(t.Context(), e2e.ContainerOpts{SudoRequirePassword: true})
	if err != nil {
		t.Skipf("skipping sudo integration test: could not start container: %v", err)
	}
	t.Cleanup(cleanup)

	p := NewPool(PoolOptions{CommandTimeout: 0})
	ctx := t.Context()

	go p.RunLoop(ctx)

	openRes, err := p.OpenCfg(ctx, Config{
		Machine:    addr,
		User:       user,
		Password:   password,
		KnownHosts: "insecure",
	})
	require.NoError(t, err)
	id := openRes.ID

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

	// Wrong password must fail — container is started with SudoRequirePassword.
	resFail := p.Exec(ctx, ExecRequest{
		SessionID:    id,
		Command:      "whoami",
		Sudo:         true,
		SudoPassword: "wrong_password",
	})
	require.NotEqual(t, 0, resFail.ExitCode)
}
