package mcpauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// SecretResolver returns the credential a request is compared against.
//
// It is called per request rather than once at construction so a secret held in
// an external store, or a config that reloads, is honored without rebuilding
// the middleware. Returning an error fails the request closed with 503: an
// unreachable secret store must not be the same as no secret.
type SecretResolver func(context.Context) (string, error)

// Options carries what the middleware cannot read off Config.
type Options struct {
	// Name identifies this server in the Bearer realm, the OAuth metadata and
	// the consent page. Defaults to "mcp".
	Name string
	// Secret resolves Config.Value. Nil compares against Config.Value verbatim,
	// which is what a server reading its credential straight from the
	// environment wants.
	Secret SecretResolver
	// Scopes are advertised in metadata and granted to a client that requests
	// none. Config.OAuth.Scopes overrides them; both empty falls back to "mcp".
	//
	// It is a field rather than config because a server that proxies others
	// derives its scopes from what it proxies.
	Scopes []string
}

func (o *Options) setDefaults() {
	if o.Name == "" {
		o.Name = "mcp"
	}
}

// Middleware returns middleware enforcing cfg, or nil when cfg is disabled.
//
// A nil return is the caller's signal that nothing is enforced, which is why it
// is nil rather than a pass-through: a caller that means to require auth can
// check for it.
//
// The static shared-secret check grants unrestricted access. OAuth-issued
// bearer tokens are scope-restricted instead, and verifying them through
// auth.RequireBearerToken stores their *auth.TokenInfo on the request context
// in the form the MCP SDK's streamable HTTP transport already looks for (see
// auth.TokenInfoFromContext), so tool handlers see the caller's granted scopes
// with no extra plumbing.
func Middleware(cfg Config, opts Options) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return nil
	}
	opts.setDefaults()

	secret := opts.Secret
	if secret == nil {
		value := cfg.Value
		secret = func(context.Context) (string, error) { return value, nil }
	}

	oauth := newOAuthState(cfg.OAuth, opts)

	return func(next http.Handler) http.Handler {
		stripped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Do not let the gateway credential leak into downstream handlers
			// and transports.
			r = r.Clone(r.Context())
			r.Header.Del(cfg.Header)
			next.ServeHTTP(w, r)
		})

		var bearer http.Handler
		if oauth != nil {
			bearer = auth.RequireBearerToken(oauth.verifyAccessToken, &auth.RequireBearerTokenOptions{
				ResourceMetadataURL: oauth.resourceMetadataURL(),
			})(stripped)
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oauth != nil && oauth.serve(secret, w, r) {
				return
			}

			expected, err := secret(r.Context())
			if err != nil {
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)

				return
			}
			if constantTimeEqual(r.Header.Get(cfg.Header), expected) {
				stripped.ServeHTTP(w, r)

				return
			}
			if oauth == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+opts.Name+`"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

				return
			}
			bearer.ServeHTTP(w, r)
		})
	}
}

// constantTimeEqual compares two credentials without leaking their length
// through timing, which comparing them directly would.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))

	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
