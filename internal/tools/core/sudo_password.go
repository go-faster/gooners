package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kballard/go-shellquote"
)

// SudoPasswordProvider resolves a sudo password from a configured source.
// Implementations must be safe for concurrent use.
type SudoPasswordProvider interface {
	Password(ctx context.Context) (string, error)
}

// EnvPasswordProvider reads the password from an environment variable.
type EnvPasswordProvider struct {
	VarName string
}

func (p *EnvPasswordProvider) Password(_ context.Context) (string, error) {
	v := os.Getenv(p.VarName)
	if v == "" {
		return "", fmt.Errorf("env var %q is empty or unset", p.VarName)
	}
	return v, nil
}

// FilePasswordProvider reads the password from a file, stripping a trailing newline.
// The file is re-read on every call so rotation is picked up automatically.
type FilePasswordProvider struct {
	Path string
}

func (p *FilePasswordProvider) Password(_ context.Context) (string, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("reading sudo password file %q: %w", p.Path, err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// CommandPasswordProvider runs a shell command and uses its stdout as the password.
// The result is cached after the first successful invocation so the command is
// not re-executed on every sudo call.
type CommandPasswordProvider struct {
	Command string
	mu      sync.Mutex
	cached  string
	ok      bool
}

func (p *CommandPasswordProvider) Password(_ context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ok {
		return p.cached, nil
	}
	argv, err := shellquote.Split(p.Command)
	if err != nil {
		return "", fmt.Errorf("parsing sudo password command %q: %w", p.Command, err)
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("sudo password command is empty")
	}
	// Exec directly without a shell to prevent command injection.
	// Note: shell features (pipes, redirects, variable expansion) are not supported;
	// use a wrapper script if needed.
	// Not using CommandContext so a canceled request context doesn't abort the
	// credential helper and poison the cache for subsequent calls.
	out, err := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // G204: argv[0] is operator-supplied config, not user input
	if err != nil {
		return "", fmt.Errorf("sudo password command %q: %w", p.Command, err)
	}
	p.cached = strings.TrimRight(string(out), "\n")
	p.ok = true
	return p.cached, nil
}
