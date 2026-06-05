package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/kballard/go-shellquote"
)

// ErrPasswordNotFound is returned by PasswordProvider when no password is
// configured for the requested machine. Callers should treat this as
// "no password available" rather than a hard error.
var ErrPasswordNotFound = errors.New("no password configured for machine")

// PasswordProvider resolves a password for a given machine.
// Implementations must be safe for concurrent use.
type PasswordProvider interface {
	Password(ctx context.Context, machine string) (string, error)
}

// EnvPasswordProvider reads the password from an environment variable.
// The machine argument is ignored; the same password is returned for all machines.
type EnvPasswordProvider struct {
	VarName string
}

func (p *EnvPasswordProvider) Password(_ context.Context, _ string) (string, error) {
	v := os.Getenv(p.VarName)
	if v == "" {
		return "", fmt.Errorf("env var %q is empty or unset", p.VarName)
	}
	return v, nil
}

// FilePasswordProvider reads the password from a file, stripping a trailing newline.
// The file is re-read on every call so rotation is picked up automatically.
// The machine argument is ignored; the same password is returned for all machines.
type FilePasswordProvider struct {
	Path string
}

func (p *FilePasswordProvider) Password(_ context.Context, _ string) (string, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("reading password file %q: %w", p.Path, err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// ConfigFilePasswordProvider reads passwords from a key=value config file keyed by
// machine name. Lines starting with '#' and blank lines are ignored. The file is
// re-read on every call so edits are picked up without restarting the server.
//
// Format:
//
//	# comment
//	web-01 = hunter2
//	db-01  = s3cr3t
type ConfigFilePasswordProvider struct {
	Path string
}

func (p *ConfigFilePasswordProvider) Password(_ context.Context, machine string) (string, error) {
	f, err := os.Open(p.Path)
	if err != nil {
		return "", fmt.Errorf("opening password config %q: %w", p.Path, err)
	}
	defer func() { _ = f.Close() }()

	keys := machineKeys(machine)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if slices.Contains(keys, k) {
			return strings.TrimSpace(v), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading password config %q: %w", p.Path, err)
	}
	return "", fmt.Errorf("%w: %s", ErrPasswordNotFound, machine)
}

// machineKeys returns the lookup keys to try for a machine string in order of
// specificity: exact → user@host → host:port → host. This lets a bare hostname entry in
// the config file match any user@ prefix or non-standard port.
//
//	"root@192.168.1.1:222" → ["root@192.168.1.1:222", "root@192.168.1.1", "192.168.1.1:222", "192.168.1.1"]
//	"192.168.1.1"          → ["192.168.1.1"]
func machineKeys(machine string) []string {
	keys := []string{machine}

	hostPort := machine
	userPrefix := ""
	if before, after, found := strings.Cut(machine, "@"); found {
		hostPort = after
		userPrefix = before + "@"
	}

	if host, _, err := net.SplitHostPort(hostPort); err == nil && host != hostPort {
		if userPrefix != "" {
			keys = append(keys, userPrefix+host)
		}
		if userPrefix != "" {
			// Add hostPort (e.g. 192.168.1.1:222) after user@host
			keys = append(keys, hostPort)
		}
		keys = append(keys, host)
	} else if userPrefix != "" {
		keys = append(keys, hostPort)
	}

	return keys
}

// CommandPasswordProvider runs a command with the machine name appended as the
// first argument and uses its stdout as the password. Results are cached per
// machine after the first successful invocation.
type CommandPasswordProvider struct {
	Command string
	mu      sync.Mutex
	cache   map[string]string
}

func (p *CommandPasswordProvider) Password(_ context.Context, machine string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.cache[machine]; ok {
		return v, nil
	}
	argv, err := shellquote.Split(p.Command)
	if err != nil {
		return "", fmt.Errorf("parsing password command %q: %w", p.Command, err)
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("password command is empty")
	}
	argv = append(argv, machine)
	// Exec directly without a shell to prevent command injection.
	// Shell features (pipes, redirects, variable expansion) are not supported;
	// use a wrapper script if needed.
	// Not using CommandContext so a canceled request context doesn't abort the
	// credential helper and poison the cache for subsequent calls.
	out, err := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // G204: argv[0] is operator-supplied config, not user input
	if err != nil {
		return "", fmt.Errorf("password command %q (machine %q): %w", p.Command, machine, err)
	}
	pwd := strings.TrimRight(string(out), "\n")
	if p.cache == nil {
		p.cache = make(map[string]string)
	}
	p.cache[machine] = pwd
	return pwd, nil
}
