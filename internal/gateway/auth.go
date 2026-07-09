// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// HTTPMiddleware returns middleware that enforces optional inbound gateway auth.
func (g *Gateway) HTTPMiddleware() func(http.Handler) http.Handler {
	cfg := g.cfg.Auth
	if !cfg.Enabled {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, err := g.authenticateRequest(r.Context(), r, cfg)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mcpgateway"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}

			// Do not let gateway credentials leak into downstream handlers/transports.
			r = r.Clone(r.Context())
			r.Header.Del(cfg.Header)
			next.ServeHTTP(w, r)
		})
	}
}

func (g *Gateway) authenticateRequest(ctx context.Context, r *http.Request, cfg AuthConfig) (bool, error) {
	expected, err := Interpolate(ctx, cfg.Value, g.resolver)
	if err != nil {
		return false, err
	}
	return constantTimeEqual(r.Header.Get(cfg.Header), expected), nil
}

func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
