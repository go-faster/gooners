package docker

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox/agent"
)

// DefaultAgentDir is where sandbox-mcp looks for per-architecture
// sandbox-agent binaries, laid out as <DefaultAgentDir>/<arch>/sandbox-agent
// (arch uses Docker/Go's GOARCH naming: amd64, arm64). The sandbox-mcp
// goreleaser Docker image ships both architectures' binaries at this path.
const DefaultAgentDir = "/usr/local/libexec/gooners/sandbox-agent"

// resolveAgentBinary returns the path to the sandbox-agent binary matching
// arch (a sandbox image's Architecture, e.g. from ImageInspect): it prefers
// <agentDir>/<arch>/sandbox-agent, and falls back to a PATH lookup - but the
// PATH fallback is only valid when arch matches the host's own architecture,
// since a bare `sandbox-agent` on $PATH is necessarily a single-arch build.
//
// Deliberately do NOT go:embed the agent binary here: two architectures'
// worth of static binaries add ~20MB to every binary in this module and
// would break `go build ./...` from a clean checkout with no prior build.
func resolveAgentBinary(agentDir, arch string) (string, error) {
	if agentDir == "" {
		agentDir = DefaultAgentDir
	}
	candidate := filepath.Join(agentDir, arch, "sandbox-agent")
	if isExecutableFile(candidate) {
		return candidate, nil
	}

	if arch == runtime.GOARCH {
		if p, err := exec.LookPath("sandbox-agent"); err == nil {
			return p, nil
		}
	}

	return "", errors.Errorf(
		"sandbox/docker: no sandbox-agent binary for architecture %q (looked for %s, and PATH for a host-arch build)",
		arch, candidate,
	)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// buildAgentTar reads the sandbox-agent binary at binaryPath and returns an
// in-memory tar archive suitable for [client.CopyToContainer]: a
// world-executable /.gooners directory and the binary at
// [agent.DefaultPath], mode 0755 - it later runs as the image's own USER,
// not root.
func buildAgentTar(binaryPath string) (*bytes.Buffer, error) {
	data, err := os.ReadFile(binaryPath) //nolint:gosec // operator-configured agent binary path (-sandbox-agent-path)
	if err != nil {
		return nil, errors.Wrap(err, "read sandbox-agent binary")
	}

	dir := strings.TrimPrefix(strings.TrimSuffix(agentDirInTar(), "/"), "/")
	file := strings.TrimPrefix(agent.DefaultPath, "/")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name:     dir + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		return nil, errors.Wrap(err, "write tar dir header")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: file,
		Mode: 0o755,
		Size: int64(len(data)),
	}); err != nil {
		return nil, errors.Wrap(err, "write tar file header")
	}
	if _, err := tw.Write(data); err != nil {
		return nil, errors.Wrap(err, "write tar file content")
	}
	if err := tw.Close(); err != nil {
		return nil, errors.Wrap(err, "close tar writer")
	}
	return &buf, nil
}

// agentDirInTar returns the parent directory of agent.DefaultPath.
func agentDirInTar() string {
	idx := strings.LastIndex(agent.DefaultPath, "/")
	if idx <= 0 {
		return "/"
	}
	return agent.DefaultPath[:idx]
}
