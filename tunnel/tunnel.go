// Package tunnel provides tunneling listeners.
//
// Providers register themselves, so a binary links only the ones it imports.
// The alternative, one package knowing every provider, would put an SDK the
// size of ngrok's into every binary that wanted a cloudflared subprocess.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
)

// Options configuration for the tunnel listener.
type Options struct {
	Type   string // "http", "tcp"
	Config string // expose config file (for cloudflared)
	Name   string // expose tunnel name (for cloudflared)
	Logger *slog.Logger
}

func (opts *Options) setDefaults() {
	if opts.Type == "" {
		opts.Type = "http"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
}

// ListenFunc opens a listener reachable through one provider.
type ListenFunc func(ctx context.Context, opts Options) (net.Listener, error)

var (
	mu        sync.RWMutex
	providers = map[string]ListenFunc{}
)

// Register makes a provider available to [Listen] under every name given,
// so a provider can keep an alias without the dispatch knowing about it.
//
// It is meant for a provider package's init and panics on a duplicate name:
// two implementations answering to one name is a build mistake, not a
// condition to resolve at runtime.
func Register(fn ListenFunc, names ...string) {
	mu.Lock()
	defer mu.Unlock()

	for _, name := range names {
		if _, ok := providers[name]; ok {
			panic("tunnel: provider " + name + " registered twice")
		}
		providers[name] = fn
	}
}

// Providers lists the registered provider names, sorted.
func Providers() []string {
	mu.RLock()
	defer mu.RUnlock()

	return slices.Sorted(maps.Keys(providers))
}

// Listen creates a [net.Listener] that exposes a local port via the named
// provider.
//
// A provider that exists but was not imported reads as unknown here, so the
// error names what is actually linked in rather than what the code could
// support.
func Listen(ctx context.Context, provider string, opts Options) (net.Listener, error) {
	opts.setDefaults()

	mu.RLock()
	fn, ok := providers[provider]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown expose provider %q, have %v", provider, Providers())
	}

	return fn(ctx, opts)
}
