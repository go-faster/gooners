package sshutil

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func newTestServer(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)

				for newChan := range chans {
					if newChan.ChannelType() != "session" {
						newChan.Reject(ssh.UnknownChannelType, "unknown")
						continue
					}
					ch, creqs, err := newChan.Accept()
					if err != nil {
						continue
					}
					go func(ch ssh.Channel, reqs <-chan *ssh.Request) {
						defer ch.Close()
						for req := range reqs {
							if req.Type != "exec" {
								continue
							}
							req.Reply(true, nil)

							var payload struct{ Command string }
							ssh.Unmarshal(req.Payload, &payload)

							if payload.Command == "success" {
								fmt.Fprint(ch, "success stdout")
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
								return
							}

							if payload.Command == "error_hang" {
								fmt.Fprint(ch, "error stdout")
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{1}))
								// Block forever to simulate a hanging background daemon
								time.Sleep(1 * time.Hour)
								return
							}

							if payload.Command == "timeout_hang" {
								fmt.Fprint(ch, "hanging")
								// Block forever to simulate a hanging command
								time.Sleep(1 * time.Hour)
								return
							}

							// Write something to stdout and stderr
							fmt.Fprint(ch, "partial stdout")
							fmt.Fprint(ch.Stderr(), "partial stderr")

							// Sleep a bit so the client times out
							time.Sleep(500 * time.Millisecond)

							// Send exit status (it shouldn't be reached if client times out first)
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
						}
					}(ch, creqs)
				}
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func TestRunWithTimeout(t *testing.T) {
	addr := newTestServer(t)

	clientCfg := &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, clientCfg)
	require.NoError(t, err)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	res, err := Run(ctx, client, "sleep 1")

	// Expecting context deadline exceeded
	require.Error(t, err)
	require.Equal(t, context.DeadlineExceeded, err)

	// Result should still have the output written before timeout
	require.Equal(t, "partial stdout", res.Stdout)
	require.Equal(t, "partial stderr", res.Stderr)
}

func TestRunWithExitErrorAndHang(t *testing.T) {
	addr := newTestServer(t)

	clientCfg := &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, clientCfg)
	require.NoError(t, err)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Use a command that triggers the mock server to send an exit-status but not close the channel.
	// Actually, our mock server already sends exit-status 0, then waits, then closes.
	// Let's modify the mock server to look for "error_hang" and send exit-status 1, and never close.
	res, err := Run(ctx, client, "error_hang")

	require.Error(t, err)
	require.Equal(t, context.DeadlineExceeded, err)
	require.Equal(t, "error stdout", res.Stdout)
}

func TestRunTimeoutsAndRecovery(t *testing.T) {
	addr := newTestServer(t)

	clientCfg := &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, clientCfg)
	require.NoError(t, err)
	defer client.Close()

	// 1. Run first command successfully
	ctxSuccess, cancelSuccess := context.WithTimeout(context.Background(), 1*time.Second)
	res, err := Run(ctxSuccess, client, "success")
	cancelSuccess()
	require.NoError(t, err)
	require.Equal(t, "success stdout", res.Stdout)

	// 2. Run 10 timeouts
	for range 10 {
		ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Millisecond)
		resTimeout, errTimeout := Run(ctxTimeout, client, "timeout_hang")
		cancelTimeout()
		require.Error(t, errTimeout)
		require.Equal(t, context.DeadlineExceeded, errTimeout)
		require.Equal(t, "hanging", resTimeout.Stdout)
	}

	// 3. The next command should run normally without blocking or failing
	ctxRecovery, cancelRecovery := context.WithTimeout(context.Background(), 1*time.Second)
	resRecovery, errRecovery := Run(ctxRecovery, client, "success")
	cancelRecovery()
	require.NoError(t, errRecovery)
	require.Equal(t, "success stdout", resRecovery.Stdout)
}
