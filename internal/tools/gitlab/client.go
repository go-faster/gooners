package gitlab

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-faster/errors"
	gl "gitlab.com/gitlab-org/api/client-go/v2"

	"github.com/go-faster/gooners/internal/effect"
)

// DefaultBaseURL is the GitLab SaaS instance, used when Config.BaseURL is unset.
const DefaultBaseURL = "https://gitlab.com"

// Config configures the GitLab API client.
type Config struct {
	// BaseURL is the GitLab instance, e.g. https://gitlab.example.com. Empty
	// means [DefaultBaseURL].
	BaseURL string
	// Token authenticates every request. Empty means unauthenticated, which
	// can still read public projects.
	Token string

	// DefaultProject is used by tools whose project argument is empty. It is
	// what makes a server pinned to one project convenient without making
	// every other project unreachable; unlike glab, an explicit project always
	// wins and none is required.
	DefaultProject string

	Timeout time.Duration

	// FS is the only host filesystem the release asset tools can reach. A nil
	// FS denies them, which is the right default for a server that has no
	// business touching local files.
	FS effect.FS

	// HTTPClient performs every GitLab request. If nil, it is an
	// [effect.NewHTTPClient] whose egress allowlist is exactly the configured
	// instance, so neither the API client nor an asset download can reach
	// another host.
	//
	// It exists as a seam for tests; production code leaves it nil.
	HTTPClient *http.Client
}

func (c *Config) setDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.FS == nil {
		c.FS = effect.Deny("gitlab-mcp was started without a local directory; pass -assets-dir to enable release asset tools")
	}
}

// Client wraps the GitLab API client with the configuration the tools need.
type Client struct {
	gl  *gl.Client
	cfg Config

	// http performs asset downloads, which are plain GETs against URLs the API
	// hands back rather than API calls.
	http effect.Doer
}

// NewClient builds a Client from cfg. It returns an error if BaseURL is
// malformed.
func NewClient(cfg Config) (*Client, error) {
	cfg.setDefaults()

	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, errors.Wrap(err, "parse GitLab URL")
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.Errorf("GitLab URL must be absolute (e.g. https://gitlab.example.com), got %q", cfg.BaseURL)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = effect.NewHTTPClient(effect.HTTPOptions{
			Policy:  effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(cfg.BaseURL)},
			Timeout: cfg.Timeout,
		})
	}

	c, err := gl.NewClient(cfg.Token,
		gl.WithBaseURL(cfg.BaseURL),
		gl.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create GitLab client")
	}

	return &Client{gl: c, cfg: cfg, http: httpClient}, nil
}

// project resolves the project a tool call targets: the argument if given,
// otherwise Config.DefaultProject. Unlike glab's MCP server, which derives the
// project from the git remote of its working directory and so cannot run
// outside a repository, an empty argument is an error only when no default was
// configured.
func (c *Client) project(arg string) (string, error) {
	if p := strings.TrimSpace(arg); p != "" {
		return p, nil
	}
	if c.cfg.DefaultProject != "" {
		return c.cfg.DefaultProject, nil
	}
	return "", errors.New("project is required: pass a project path like group/project, or start the server with -project")
}

// webURL builds an absolute instance URL from a path the API returned relative.
func (c *Client) webURL(path string) string {
	return strings.TrimSuffix(c.cfg.BaseURL, "/") + "/" + strings.TrimPrefix(path, "/")
}
