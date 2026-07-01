// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/go-faster/errors"
	"github.com/kballard/go-shellquote"
)

// ErrSecretNotFound is returned when a requested secret has no configured source.
var ErrSecretNotFound = errors.New("secret not found")

// SecretSource holds the possible sources for a secret value (first non-empty wins).
type SecretSource struct {
	Value   string
	Env     string
	File    string
	Command string
}

// SecretResolver resolves named secrets.
type SecretResolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

type secretResolver struct {
	sources  map[string]SecretSource
	lastGood map[string]string
	mu       sync.Mutex
}

// NewSecretResolver builds a resolver from config. Duplicate names are an error.
func NewSecretResolver(cfgs []SecretConfig) (SecretResolver, error) {
	m := map[string]SecretSource{}
	for _, c := range cfgs {
		if _, ok := m[c.Name]; ok {
			return nil, errors.New("duplicate secret name")
		}
		m[c.Name] = SecretSource{Value: c.Value, Env: c.Env, File: c.File, Command: c.Command}
	}
	return &secretResolver{sources: m, lastGood: map[string]string{}}, nil
}

func (r *secretResolver) Resolve(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	src, ok := r.sources[name]
	r.mu.Unlock()
	if !ok {
		return "", errors.Wrapf(ErrSecretNotFound, "secret %q", name)
	}

	val, err := r.resolveSource(ctx, name, src)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.lastGood[name] = val
		return val, nil
	}
	if v, ok := r.lastGood[name]; ok {
		slog.Default().Warn("secret read failed, using last good value", "name", name, "error", err)
		return v, nil
	}
	return "", err
}

// resolveSource reads the secret value from its source without holding any lock.
func (r *secretResolver) resolveSource(_ context.Context, name string, src SecretSource) (string, error) {
	switch {
	case src.Value != "":
		return src.Value, nil
	case src.Env != "":
		v := os.Getenv(src.Env)
		if v == "" {
			return "", errors.Wrapf(ErrSecretNotFound, "env %s empty for secret %q", src.Env, name)
		}
		return v, nil
	case src.File != "":
		b, readErr := os.ReadFile(src.File)
		if readErr != nil {
			return "", errors.Wrapf(readErr, "read secret file for %q", name)
		}
		return strings.TrimRight(string(b), "\n"), nil
	case src.Command != "":
		argv, parseErr := shellquote.Split(src.Command)
		switch {
		case parseErr != nil:
			return "", errors.Wrapf(parseErr, "parse command for secret %q", name)
		case len(argv) == 0:
			return "", errors.New("empty command for secret")
		}
		argv = append(argv, name)
		out, cmdErr := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // G204: argv comes from operator TOML config, not user input
		if cmdErr != nil {
			return "", errors.Wrapf(cmdErr, "run command for secret %q", name)
		}
		return strings.TrimRight(string(out), "\n"), nil
	default:
		return "", errors.Wrapf(ErrSecretNotFound, "secret %q has no source", name)
	}
}

// isValidSecretName reports whether every byte of s is a valid secret-name character ([A-Za-z0-9_.-]).
func isValidSecretName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		b := s[i]
		if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
			b == '_' || b == '.' || b == '-') {
			return false
		}
	}
	return true
}

// parseSecretRef attempts to parse a "{secret:NAME}" token (with optional
// surrounding whitespace) starting at position i in s. It returns the secret
// name and the index just past the closing '}', or ("", 0) if no valid token
// starts at i.
func parseSecretRef(s string, i int) (name string, end int) {
	if i >= len(s) || s[i] != '{' {
		return "", 0
	}
	// Find the matching closing brace.
	closeBraceIdx := strings.IndexByte(s[i+1:], '}')
	if closeBraceIdx < 0 {
		return "", 0
	}
	inner := strings.TrimSpace(s[i+1 : i+1+closeBraceIdx])
	// Split on the first ':' to separate keyword from name.
	keyword, rest, ok := strings.Cut(inner, ":")
	if !ok || strings.TrimSpace(keyword) != "secret" {
		return "", 0
	}
	n := strings.TrimSpace(rest)
	if !isValidSecretName(n) {
		return "", 0
	}
	return n, i + 1 + closeBraceIdx + 1
}

// extractSecretRefs yields each secret name referenced in s via the {secret:NAME}
// syntax. Duplicates are preserved in order.
func extractSecretRefs(s string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i := 0; i < len(s); i++ {
			if s[i] != '{' {
				continue
			}
			name, end := parseSecretRef(s, i)
			if end == 0 {
				continue
			}
			if !yield(name) {
				return
			}
			i = end - 1 // -1 because the loop will i++
		}
	}
}

// Interpolate replaces {secret:NAME} (with optional whitespace) using the resolver.
func Interpolate(ctx context.Context, s string, r SecretResolver) (string, error) {
	if r == nil {
		if strings.Contains(s, "{secret:") {
			return "", errors.New("secrets present but no resolver")
		}
		return s, nil
	}

	var (
		b    strings.Builder
		merr error
		i    int
	)
	for i < len(s) {
		if s[i] != '{' {
			b.WriteByte(s[i])
			i++
			continue
		}
		name, end := parseSecretRef(s, i)
		if end == 0 {
			// not a valid secret ref — copy the '{' literally
			b.WriteByte(s[i])
			i++
			continue
		}
		v, err := r.Resolve(ctx, name)
		if err != nil {
			merr = err
			// copy the original token verbatim on error
			b.WriteString(s[i:end])
		} else {
			b.WriteString(v)
		}
		i = end
	}
	if merr != nil {
		return "", merr
	}
	return b.String(), nil
}
