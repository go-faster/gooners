// Package agent implements the SSH+SFTP server that runs inside a sandbox
// container, driven over its exec/attach stdio stream rather than a network
// listener.
package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/go-faster/errors"
)

// Magic prefixes the single configuration line written to the agent's stdin
// before the SSH handshake begins.
const Magic = "GOONERS1"

// DefaultPath is where the agent binary is injected inside the sandbox.
const DefaultPath = "/.gooners/sandbox-agent"

// Preamble configures a sandbox agent. It is written as one base64 JSON line.
// Keys travel here rather than via argv (world-readable in `ps`) or env (not
// settable on a Kubernetes exec).
type Preamble struct {
	Version       int    `json:"v"`
	HostKeyPEM    string `json:"host_key"`       // the agent's SSH host key
	AuthorizedKey string `json:"authorized_key"` // the only key allowed to log in
	Shell         string `json:"shell,omitempty"`
	Workdir       string `json:"workdir,omitempty"`
}

// WritePreamble writes p to w as one line: "<Magic> <base64(json)>\n".
func WritePreamble(w io.Writer, p Preamble) error {
	data, err := json.Marshal(p)
	if err != nil {
		return errors.Wrap(err, "marshal preamble")
	}
	line := Magic + " " + base64.StdEncoding.EncodeToString(data) + "\n"
	if _, err := io.WriteString(w, line); err != nil {
		return errors.Wrap(err, "write preamble")
	}
	return nil
}

// ReadPreamble reads and parses a preamble line written by [WritePreamble].
//
// r must be the same *bufio.Reader the caller then hands to the transport
// (e.g. streamconn.New): the SSH handshake follows immediately after the
// newline, and any bytes bufio has already buffered past it must not be
// dropped.
func ReadPreamble(r *bufio.Reader) (Preamble, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return Preamble{}, errors.Wrap(err, "read preamble line")
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")

	prefix := Magic + " "
	if !strings.HasPrefix(line, prefix) {
		return Preamble{}, errors.Errorf("preamble: missing magic %q", Magic)
	}

	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, prefix))
	if err != nil {
		return Preamble{}, errors.Wrap(err, "decode preamble")
	}

	var p Preamble
	if err := json.Unmarshal(data, &p); err != nil {
		return Preamble{}, errors.Wrap(err, "unmarshal preamble")
	}
	return p, nil
}
