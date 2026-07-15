// Package main is the entrypoint for sandbox-agent: the SSH+SFTP server
// injected into sandbox containers, driven over its own stdin/stdout rather
// than a network listener. See internal/sandbox/agent.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/sandbox/streamconn"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-agent: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	// r must be the same *bufio.Reader passed to streamconn.New below: it
	// buffers bytes of the SSH handshake that immediately follows the
	// preamble line, and reading os.Stdin directly here would lose them.
	r := bufio.NewReader(os.Stdin)

	preamble, err := agent.ReadPreamble(r)
	if err != nil {
		return errors.Wrap(err, "read preamble")
	}
	hostKey, err := ssh.ParsePrivateKey([]byte(preamble.HostKeyPEM))
	if err != nil {
		return errors.Wrap(err, "parse host key")
	}
	authorizedKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(preamble.AuthorizedKey)) //nolint:dogsled // ssh.ParseAuthorizedKey's comment/options/rest are unused
	if err != nil {
		return errors.Wrap(err, "parse authorized key")
	}

	conn := streamconn.New(r, os.Stdout, streamconn.Options{})
	return agent.Serve(context.Background(), conn, agent.Config{
		HostKey:       hostKey,
		AuthorizedKey: authorizedKey,
		Shell:         preamble.Shell,
		Workdir:       preamble.Workdir,
		Version:       version,
	})
}
