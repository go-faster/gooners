// Package e2e provides shared helpers and end-to-end tests for the ssh-mcp server
// using testcontainers and the official MCP Go SDK client over in-memory transport.
package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NewSudoTestContainer starts a linuxserver/openssh-server container with sudo access
// configured for user "test" / password "secret".
func NewSudoTestContainer(ctx context.Context) (addr, user, password string, cleanup func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
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
		WaitingFor:   wait.ForListeningPort("2222/tcp").WithStartupTimeout(120 * time.Second),
	}

	genReq := testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	}

	// Install required CLI tools after the container reports ready (port listening).
	// These are used by various ssh-mcp tools (lsof, ip, lsblk, ps, free, etc).
	// Use sh -c to allow || true so a missing package doesn't fail the entire test startup
	// (some images or apk may vary); tests will still fail later with clear errors if cmds missing.
	installCmd := testcontainers.NewRawCommand([]string{
		"/bin/sh", "-c",
		"apk --no-cache add lsof iproute2 util-linux procps-ng sudo || true; echo 'test ALL=(ALL) NOPASSWD: ALL' >> /etc/sudoers || true",
	})
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

	return addr, user, password, cleanup, nil
}
