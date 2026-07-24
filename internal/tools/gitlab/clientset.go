package gitlab

import (
	"crypto/sha256"
	"net/http"
	"sync"

	"github.com/go-faster/gooners/internal/effect"
)

// maxCachedClients bounds the per-token client cache. Tokens are caller-supplied
// on an open port, so the cache is attacker-driven input; past this many
// distinct credentials it is dropped wholesale rather than grown.
const maxCachedClients = 128

// ClientSet hands out a [Client] per credential, sharing everything in [Config]
// that is not the credential — including the HTTP client, so distinct tokens
// share one connection pool and one egress allowlist.
type ClientSet struct {
	cfg  Config
	http *http.Client

	mu sync.Mutex
	// Keyed by SHA-256 so no credential is used as a map key.
	clients map[[32]byte]*Client
}

// NewClientSet validates cfg once and returns a set that builds clients from
// it on demand. cfg.Token is used for the server's own credential, if it has
// one; see [AuthMode].
func NewClientSet(cfg Config) (*ClientSet, error) {
	cfg.setDefaults()

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = effect.NewHTTPClient(effect.HTTPOptions{
			Policy:  effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(cfg.BaseURL)},
			Timeout: cfg.Timeout,
		})
	}
	cfg.HTTPClient = httpClient

	// Fail at startup on a malformed BaseURL rather than on the first tool
	// call of the first session.
	if _, err := NewClient(cfg); err != nil {
		return nil, err
	}

	return &ClientSet{
		cfg:     cfg,
		http:    httpClient,
		clients: make(map[[32]byte]*Client),
	}, nil
}

// Token is the server's own credential, empty when it has none.
func (cs *ClientSet) Token() string { return cs.cfg.Token }

// For returns the client authenticating as token, building it on first use.
// Sessions presenting the same token share a client.
func (cs *ClientSet) For(token string) (*Client, error) {
	key := sha256.Sum256([]byte(token))

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if c, ok := cs.clients[key]; ok {
		return c, nil
	}

	cfg := cs.cfg
	cfg.Token = token
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}

	if len(cs.clients) >= maxCachedClients {
		clear(cs.clients)
	}
	cs.clients[key] = c
	return c, nil
}
