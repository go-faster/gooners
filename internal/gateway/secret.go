// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
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

func (r *secretResolver) Resolve(_ context.Context, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	src, ok := r.sources[name]
	if !ok {
		return "", errors.Wrapf(ErrSecretNotFound, "secret %q", name)
	}

	var val string
	var err error
	switch {
	case src.Value != "":
		val = src.Value
	case src.Env != "":
		val = os.Getenv(src.Env)
		if val == "" {
			err = errors.Wrapf(ErrSecretNotFound, "env %s empty for secret %q", src.Env, name)
		}
	case src.File != "":
		b, readErr := os.ReadFile(src.File)
		if readErr != nil {
			err = errors.Wrapf(readErr, "read secret file for %q", name)
		} else {
			val = strings.TrimRight(string(b), "\n")
		}
	case src.Command != "":
		argv, parseErr := shellquote.Split(src.Command)
		switch {
		case parseErr != nil:
			err = errors.Wrapf(parseErr, "parse command for secret %q", name)
		case len(argv) == 0:
			err = errors.New("empty command for secret")
		default:
			argv = append(argv, name)
			out, cmdErr := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // G204: argv comes from operator TOML config, not user input
			if cmdErr != nil {
				err = errors.Wrapf(cmdErr, "run command for secret %q", name)
			} else {
				val = strings.TrimRight(string(out), "\n")
			}
		}
	default:
		err = errors.Wrapf(ErrSecretNotFound, "secret %q has no source", name)
	}

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

// Interpolate replaces {secret:NAME} (with optional whitespace) using the resolver.
func Interpolate(s string, r SecretResolver) (string, error) {
	if r == nil {
		if strings.Contains(s, "{secret:") {
			return "", errors.New("secrets present but no resolver")
		}
		return s, nil
	}
	re := regexp.MustCompile(`\{\s*secret\s*:\s*([A-Za-z0-9_.-]+)\s*\}`)
	var merr error
	out := re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		v, err := r.Resolve(context.Background(), sub[1])
		if err != nil {
			merr = err
			return m
		}
		return v
	})
	if merr != nil {
		return "", merr
	}
	return out, nil
}
