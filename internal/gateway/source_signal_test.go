//go:build unix

package gateway

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileSource_WatchSignal(t *testing.T) {
	src, err := NewFileSource(FileSourceOptions{
		Path:    writeConfig(t, testConfigTOML),
		Signals: []os.Signal{syscall.SIGUSR1},
	})
	require.NoError(t, err)

	ch := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Watch(ctx, ch) }()

	// Signal delivery races with the signal.Notify inside Watch, so resend
	// until the handler is installed rather than sleeping a fixed grace period.
	deadline := time.After(5 * time.Second)
	for {
		require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGUSR1))
		select {
		case <-ch:
			cancel()
			require.NoError(t, <-done)
			return
		case <-deadline:
			t.Fatal("no signal reported")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A signal must reload even when the file bytes are unchanged: the secrets it
// interpolates may have moved underneath it.
func TestFileSource_WatchSignalIgnoresHash(t *testing.T) {
	path := writeConfig(t, testConfigTOML)
	src, err := NewFileSource(FileSourceOptions{Path: path, Signals: []os.Signal{syscall.SIGUSR2}})
	require.NoError(t, err)
	_, err = src.Load(t.Context())
	require.NoError(t, err)

	ch := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Watch(ctx, ch) }()

	deadline := time.After(5 * time.Second)
	for {
		require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGUSR2))
		select {
		case <-ch:
			cancel()
			require.NoError(t, <-done)
			return
		case <-deadline:
			t.Fatal("no signal reported")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
