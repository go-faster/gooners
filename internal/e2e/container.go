// Package e2e provides shared helpers and end-to-end tests for the ssh-mcp server
// using testcontainers and the official MCP Go SDK client over in-memory transport.
package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/ssh"
)

// ContainerOpts configures NewSudoTestContainer behavior.
type ContainerOpts struct {
	// SudoRequirePassword strips NOPASSWD from sudoers so that sudo prompts for
	// the user's password. Default (false) keeps the image's passwordless sudo,
	// which is required by tools like proc_kill that use "sudo -n".
	SudoRequirePassword bool

	// PreferECDSA patches sshd_config to advertise ECDSA host keys before
	// ED25519. Use this to simulate a server whose preferred key type differs
	// from what is stored in a client's known_hosts file.
	PreferECDSA bool
}

// NewSudoTestContainer starts a linuxserver/openssh-server container with sudo access
// configured for user "test" / password "secret".
func NewSudoTestContainer(ctx context.Context, opts ContainerOpts) (addr, user, password string, cleanup func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
		WaitingFor:   wait.ForListeningPort("2222/tcp").WithStartupTimeout(2 * time.Minute),
	}

	genReq := testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	}

	// Install required CLI tools after the container reports ready (port listening).
	// These are used by various ssh-mcp tools (lsof, ip, lsblk, ps, free, etc).
	// Use sh -c to allow || true so a missing package doesn't fail the entire test startup
	// (some images or apk may vary); tests will still fail later with clear errors if cmds missing.
	installScript := "apk --no-cache add lsof iproute2 util-linux procps-ng sudo || true; echo 'test ALL=(ALL) NOPASSWD: ALL' >> /etc/sudoers || true"
	if opts.SudoRequirePassword {
		// Remove NOPASSWD from every sudoers file so sudo prompts for a password.
		installScript += "; find /etc/sudoers.d/ -type f -exec sed -i 's/ NOPASSWD:/ /g' {} + 2>/dev/null; sed -i 's/ NOPASSWD:/ /g' /etc/sudoers 2>/dev/null || true"
	}
	if opts.PreferECDSA {
		// Reorder HostKeyAlgorithms so ECDSA is advertised before ED25519.
		// This simulates a server whose preferred key type differs from what the
		// client has in known_hosts (the scenario that triggered the key-mismatch bug).
		installScript += "; echo 'HostKeyAlgorithms ecdsa-sha2-nistp256,ecdsa-sha2-nistp384,ecdsa-sha2-nistp521,ssh-ed25519,rsa-sha2-512,rsa-sha2-256' >> /config/ssh/sshd_config" +
			"; pkill -HUP sshd || kill -HUP $(cat /var/run/sshd.pid 2>/dev/null) || true"
	}
	installCmd := testcontainers.NewRawCommand([]string{"/bin/sh", "-c", installScript})
	if err := testcontainers.WithAfterReadyCommand(installCmd)(&genReq); err != nil {
		return "", "", "", nil, fmt.Errorf("configuring after-ready install: %w", err)
	}

	ctr, err := testcontainers.GenericContainer(ctx, genReq)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("starting container: %w", err)
	}

	cleanup = func() {
		_ = ctr.Terminate(context.Background())
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		cleanup()
		return "", "", "", nil, fmt.Errorf("getting host: %w", err)
	}

	port, err := ctr.MappedPort(ctx, "2222/tcp")
	if err != nil {
		cleanup()
		return "", "", "", nil, fmt.Errorf("getting mapped port: %w", err)
	}

	addr = fmt.Sprintf("%s:%s", host, port.Port())
	user = "test"
	password = "secret"
	if err := waitForSSHReady(ctx, addr, user, password); err != nil {
		cleanup()
		return "", "", "", nil, err
	}

	return addr, user, password, cleanup, nil
}

func waitForSSHReady(ctx context.Context, addr, user, password string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Minute)
	}

	var lastErr error
	for time.Now().Before(deadline) {
		client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{ssh.Password(password)},
			HostKeyCallback: func(_ string, _ net.Addr, _ ssh.PublicKey) error {
				return nil
			},
			Timeout: 5 * time.Second,
		})
		if err == nil {
			_ = client.Close()
			return nil
		}
		lastErr = err

		// These transient handshake errors happen when the container port is open
		// before sshd has finished reloading after setup commands.
		if !isTransientSSHDialError(err) {
			return fmt.Errorf("waiting for ssh readiness: %w", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for ssh readiness: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("waiting for ssh readiness timed out: %w", lastErr)
}

func isTransientSSHDialError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout")
}
