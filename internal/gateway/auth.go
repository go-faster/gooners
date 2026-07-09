// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const defaultOAuthTokenTTL = time.Hour

// HTTPMiddleware returns middleware that enforces optional inbound gateway auth.
//
// The static shared-secret check (cfg.Header/cfg.Value) grants unrestricted access,
// same as before OAuth existed: it is the trusted-operator credential. OAuth-issued
// bearer tokens are scope-restricted instead; verifying them through auth.RequireBearerToken
// stores their *auth.TokenInfo on the request context in the exact form the MCP SDK's
// streamable HTTP transport already looks for (see auth.TokenInfoFromContext), so every
// tool-call handler sees the caller's granted scopes with no extra plumbing on our side.
func (g *Gateway) HTTPMiddleware() func(http.Handler) http.Handler {
	cfg := g.cfg.Auth
	if !cfg.Enabled {
		return nil
	}
	oauth := newOAuthState(cfg.OAuth, g.cfg.Upstreams)
	return func(next http.Handler) http.Handler {
		stripped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Do not let gateway credentials leak into downstream handlers/transports.
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
			if oauth != nil && oauth.serve(g, w, r) {
				return
			}

			expected, err := Interpolate(r.Context(), cfg.Value, g.resolver)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			if constantTimeEqual(r.Header.Get(cfg.Header), expected) {
				stripped.ServeHTTP(w, r)
				return
			}
			if oauth == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mcpgateway"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			bearer.ServeHTTP(w, r)
		})
	}
}

func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

type oauthState struct {
	cfg      OAuthConfig
	tokenTTL time.Duration
	// derivedScopes are the "mcp:<upstream>" and "mcp:<upstream>:<name>" scopes
	// computed from the gateway's configured upstreams; see scopes().
	derivedScopes []string

	mu     sync.Mutex
	codes  map[string]oauthCode
	tokens map[string]auth.TokenInfo
}

type oauthCode struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Expires             time.Time
}

func newOAuthState(cfg OAuthConfig, upstreams []UpstreamConfig) *oauthState {
	if !cfg.Enabled {
		return nil
	}
	ttl := defaultOAuthTokenTTL
	if parsed, err := parseOptionalDuration(cfg.TokenTTL); err == nil && parsed > 0 {
		ttl = parsed
	}
	return &oauthState{
		cfg:           cfg,
		tokenTTL:      ttl,
		derivedScopes: derivedUpstreamScopes(upstreams),
		codes:         map[string]oauthCode{},
		tokens:        map[string]auth.TokenInfo{},
	}
}

// derivedUpstreamScopes computes the "mcp:<upstream>" base scope (full access to
// that upstream) and "mcp:<upstream>:<name>" sub-scope for every configured
// ScopeConfig, for every upstream.
func derivedUpstreamScopes(upstreams []UpstreamConfig) []string {
	var out []string
	for _, u := range upstreams {
		out = append(out, upstreamScope(u.Name))
		for _, sc := range u.Tools.Scopes {
			out = append(out, upstreamSubScope(u.Name, sc.Name))
		}
	}
	return out
}

func upstreamScope(upstream string) string {
	return "mcp:" + upstream
}

func upstreamSubScope(upstream, name string) string {
	return "mcp:" + upstream + ":" + name
}

func (o *oauthState) serve(g *Gateway, w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource":
		o.serveProtectedResourceMetadata(w, r)
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		o.serveAuthorizationServerMetadata(w, r)
	case "/jwks":
		o.serveJWKS(w, r)
	case "/authorize":
		o.serveAuthorize(g, w, r)
	case "/token":
		o.serveToken(w, r)
	default:
		return false
	}
	return true
}

func (o *oauthState) serveProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if !allowMetadataRequest(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 o.cfg.Resource,
		"authorization_servers":    []string{o.cfg.Issuer},
		"scopes_supported":         o.scopes(),
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "mcpgateway",
	})
}

func (o *oauthState) serveAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if !allowMetadataRequest(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.cfg.Issuer,
		"authorization_endpoint":                o.cfg.Issuer + "/authorize",
		"token_endpoint":                        o.cfg.Issuer + "/token",
		"jwks_uri":                              o.cfg.Issuer + "/jwks",
		"scopes_supported":                      o.scopes(),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
	})
}

func allowMetadataRequest(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	if r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func (o *oauthState) serveAuthorize(g *Gateway, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		o.renderAuthorize(w, r.URL.RawQuery, "")
	case http.MethodPost:
		o.handleAuthorizePost(g, w, r)
	default:
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func (o *oauthState) renderAuthorize(w http.ResponseWriter, rawQuery, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = authorizePage.Execute(w, struct {
		Message string
		Query   string
	}{
		Message: message,
		Query:   rawQuery,
	})
}

func (o *oauthState) handleAuthorizePost(g *Gateway, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	q, err := url.ParseQuery(r.Form.Get("query"))
	if err != nil {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	// redirect_uri must be validated against the configured allowlist before it is
	// used for any redirect (including error redirects): an attacker-supplied
	// redirect_uri would otherwise let them exfiltrate authorization codes to an
	// origin they control (RFC 6749 §3.1.2.3 / RFC 9700 §4.1.4).
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" || !o.validRedirectURI(redirectURI) {
		http.Error(w, "invalid or unregistered redirect_uri", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" {
		o.redirectOAuthError(w, q, "unsupported_response_type")
		return
	}
	if !o.validClient(q.Get("client_id")) {
		o.redirectOAuthError(w, q, "unauthorized_client")
		return
	}
	if q.Get("code_challenge") == "" {
		o.redirectOAuthError(w, q, "invalid_request")
		return
	}
	expected, err := Interpolate(r.Context(), g.cfg.Auth.Value, g.resolver)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if !constantTimeEqual("Bearer "+r.Form.Get("token"), expected) && !constantTimeEqual(r.Form.Get("token"), expected) {
		o.renderAuthorize(w, r.Form.Get("query"), "Invalid gateway token")
		return
	}
	code, err := randomToken()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	o.mu.Lock()
	o.codes[code] = oauthCode{
		ClientID:            q.Get("client_id"),
		RedirectURI:         redirectURI,
		Scope:               o.normalizeScope(q.Get("scope")),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Expires:             time.Now().Add(5 * time.Minute),
	}
	o.mu.Unlock()

	redir, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	values := redir.Query()
	values.Set("code", code)
	if state := q.Get("state"); state != "" {
		values.Set("state", state)
	}
	redir.RawQuery = values.Encode()
	http.Redirect(w, r, redir.String(), http.StatusFound) //nolint:gosec // G710: redirectURI is checked against o.cfg.RedirectURIs above
}

func (o *oauthState) redirectOAuthError(w http.ResponseWriter, q url.Values, code string) {
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, code, http.StatusBadRequest)
		return
	}
	redir, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, code, http.StatusBadRequest)
		return
	}
	values := redir.Query()
	values.Set("error", code)
	if state := q.Get("state"); state != "" {
		values.Set("state", state)
	}
	redir.RawQuery = values.Encode()
	w.Header().Set("Location", redir.String())
	w.WriteHeader(http.StatusFound)
}

func (o *oauthState) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	code := r.Form.Get("code")
	o.mu.Lock()
	stored, ok := o.codes[code]
	if ok {
		delete(o.codes, code)
	}
	o.mu.Unlock()
	if !ok || time.Now().After(stored.Expires) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if stored.RedirectURI != r.Form.Get("redirect_uri") || !o.validClient(r.Form.Get("client_id")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if !verifyPKCE(stored.CodeChallenge, stored.CodeChallengeMethod, r.Form.Get("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	accessToken, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	expires := time.Now().Add(o.tokenTTL)
	o.mu.Lock()
	o.tokens[accessToken] = auth.TokenInfo{
		Scopes:     strings.Fields(stored.Scope),
		Expiration: expires,
	}
	o.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(o.tokenTTL.Seconds()),
		"scope":        stored.Scope,
	})
}

func (o *oauthState) serveJWKS(w http.ResponseWriter, r *http.Request) {
	if !allowMetadataRequest(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
}

// verifyAccessToken implements auth.TokenVerifier for tokens issued by /token.
func (o *oauthState) verifyAccessToken(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	o.mu.Lock()
	info, ok := o.tokens[token]
	if ok && time.Now().After(info.Expiration) {
		delete(o.tokens, token)
		ok = false
	}
	o.mu.Unlock()
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &info, nil
}

func (o *oauthState) validClient(clientID string) bool {
	return o.cfg.ClientID == "" || clientID == o.cfg.ClientID
}

func (o *oauthState) validRedirectURI(redirectURI string) bool {
	return slices.Contains(o.cfg.RedirectURIs, redirectURI)
}

// scopes returns the scope set advertised in metadata and granted when a client
// requests no explicit scope. cfg.Scopes, if set, overrides the derived
// "mcp:<upstream>[:<name>]" scopes for backward compatibility with a flat,
// manually maintained scope list.
func (o *oauthState) scopes() []string {
	if len(o.cfg.Scopes) > 0 {
		return o.cfg.Scopes
	}
	if len(o.derivedScopes) > 0 {
		return o.derivedScopes
	}
	return []string{"mcp"}
}

func (o *oauthState) normalizeScope(scope string) string {
	if scope != "" {
		return scope
	}
	return strings.Join(o.scopes(), " ")
}

func (o *oauthState) resourceMetadataURL() string {
	return o.cfg.Issuer + "/.well-known/oauth-protected-resource"
}

func verifyPKCE(challenge, method, verifier string) bool {
	if challenge == "" {
		// PKCE is mandatory: the authorize endpoint refuses to issue a code without
		// a code_challenge, so an empty stored challenge here means the request was
		// tampered with or forged and must be rejected rather than waved through.
		return false
	}
	if verifier == "" {
		return false
	}
	if method == "" || method == "plain" {
		return constantTimeEqual(verifier, challenge)
	}
	if method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	encoded := base64.RawURLEncoding.EncodeToString(sum[:])
	return constantTimeEqual(encoded, challenge)
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

var authorizePage = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Authorize mcpgateway</title></head>
<body>
<h1>Authorize mcpgateway</h1>
{{if .Message}}<p style="color: red">{{.Message}}</p>{{end}}
<form method="post" action="/authorize">
  <input type="hidden" name="query" value="{{.Query}}">
  <label>Gateway token <input type="password" name="token" autofocus></label>
  <button type="submit">Authorize</button>
</form>
</body>
</html>`))
