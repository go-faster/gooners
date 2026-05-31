package fs

import (
	"crypto/rand"
	"crypto/rsa"
	"net"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

type mockExecHandler func(cmd string) (string, int)

func setupMockSSHServer(t *testing.T, execHandler mockExecHandler) (*ssh.Client, func()) {
	t.Helper()
	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "failed to generate private key")

	signer, err := ssh.NewSignerFromKey(privateKey)
	require.NoError(t, err, "failed to create signer")

	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "failed to listen")

	done := make(chan struct{})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					t.Errorf("failed to accept: %v", err)
					return
				}
			}

			sConn, chans, reqs, err := ssh.NewServerConn(conn, config)
			if err != nil {
				t.Errorf("failed to handshake: %v", err)
				continue
			}

			go ssh.DiscardRequests(reqs)

			go func() {
				for newChannel := range chans {
					if newChannel.ChannelType() != "session" {
						if err := newChannel.Reject(ssh.UnknownChannelType, "unknown channel type"); err != nil {
							t.Logf("reject channel: %v", err)
						}
						continue
					}

					channel, requests, err := newChannel.Accept()
					if err != nil {
						t.Errorf("could not accept channel: %v", err)
						continue
					}

					go func() {
						for req := range requests {
							switch req.Type {
							case "exec":
								var payload struct {
									Command string
								}
								if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
									t.Errorf("failed to parse payload: %v", err)
									if err := req.Reply(false, nil); err != nil {
										t.Logf("reply false: %v", err)
									}
									continue
								}
								if err := req.Reply(true, nil); err != nil {
									t.Logf("reply true: %v", err)
								}

								out, exitCode := execHandler(payload.Command)
								if _, err := channel.Write([]byte(out)); err != nil {
									t.Logf("write output: %v", err)
								}
								if _, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ uint32 }{uint32(exitCode)})); err != nil {
									t.Logf("send exit-status: %v", err)
								}
								if err := channel.Close(); err != nil {
									t.Logf("close exec channel: %v", err)
								}
							case "subsystem":
								var payload struct {
									Name string
								}
								if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
									if err := req.Reply(false, nil); err != nil {
										t.Logf("reply false: %v", err)
									}
									continue
								}
								if payload.Name == "sftp" {
									if err := req.Reply(true, nil); err != nil {
										t.Logf("reply true: %v", err)
									}
									server, err := sftp.NewServer(channel)
									if err != nil {
										t.Errorf("sftp server init error: %v", err)
										return
									}
									if err := server.Serve(); err != nil {
										t.Logf("sftp serve: %v", err)
									}
									if err := channel.Close(); err != nil {
										t.Logf("close sftp channel: %v", err)
									}
								} else {
									if err := req.Reply(false, nil); err != nil {
										t.Logf("reply false: %v", err)
									}
								}
							default:
								if err := req.Reply(false, nil); err != nil {
									t.Logf("reply false: %v", err)
								}
							}
						}
					}()
				}
			}()
			_ = sConn
		}
	}()

	clientConfig := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.Password("test")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}

	client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig)
	require.NoError(t, err, "failed to connect to mock server")

	cleanup := func() {
		close(done)
		if err := client.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
		if err := listener.Close(); err != nil {
			t.Logf("close listener: %v", err)
		}
	}

	return client, cleanup
}
