package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/effect"
)

// After a canceled exec the cleanup waits for the command to finish and logs
// the error it finished with instead of dropping it.
func TestPoolCleanupExecLogsRunError(t *testing.T) {
	var logs strings.Builder
	spoolFS := effect.Root(t.TempDir())
	p := NewPool(PoolOptions{
		Logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SpoolFS: spoolFS,
	})

	srv := newTestServer(t)
	client, err := ssh.Dial("tcp", srv.addr, &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	require.NoError(t, err)

	done := make(chan error, 1)
	done <- errors.New("killed by signal")

	const threshold = 1 << 20
	stdout := NewSpoolingBuffer(spoolFS, "s1", threshold)
	stderr := NewSpoolingBuffer(spoolFS, "s1", threshold)
	p.cleanupExec(context.Background(), sess, ExecRequest{SessionID: "s1"}, "sleep 60", done, stdout, stderr)

	require.Contains(t, logs.String(), "command exited after cancel")
	require.Contains(t, logs.String(), "killed by signal")
}
