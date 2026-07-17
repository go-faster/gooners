// Package main is the entrypoint for the gitlab-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/effect"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/gitlab"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)

	var (
		baseURL = flag.String("gitlab-url", os.Getenv("GITLAB_URL"), "GitLab instance URL; defaults to the glab CLI's configured host, then https://gitlab.com")
		token   = flag.String("gitlab-token", os.Getenv("GITLAB_TOKEN"), "GitLab API token; defaults to the glab CLI's stored token")
		project = flag.String("project", os.Getenv("GITLAB_PROJECT"), "default project (group/project) for tool calls that omit one")

		glabConfigDir = flag.String("glab-config-dir", "", "glab CLI config directory to read credentials from; defaults to glab's own location")
		noGlabConfig  = flag.Bool("no-glab-config", false, "do not read credentials from the glab CLI config")

		assetsDir = flag.String("assets-dir", "", "directory the release asset tools may read and write; they are disabled when unset")
	)
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := gitlab.Config{
		BaseURL:        *baseURL,
		Token:          *token,
		DefaultProject: *project,
	}

	// The glab CLI is the likeliest place a token already exists, so an
	// operator who has run `glab auth login` needs no further setup. Explicit
	// flags and environment still win.
	if !*noGlabConfig {
		dir := *glabConfigDir
		if dir == "" {
			dir = gitlab.GlabConfigDir()
		}
		glabCfg, err := gitlab.LoadGlabConfig(dir)
		if err != nil {
			logger.Warn("could not read glab config", "dir", dir, "err", err)
		} else {
			// The host we look up is the one being configured, so an explicit
			// -gitlab-url still picks up its matching stored token.
			glabURL, glabToken := glabCfg.Resolve(hostOf(cfg.BaseURL))
			if cfg.BaseURL == "" && glabURL != "" {
				cfg.BaseURL = glabURL
				logger.Info("using GitLab instance from glab config", "url", glabURL)
			}
			if cfg.Token == "" && glabToken != "" {
				cfg.Token = glabToken
				logger.Info("using GitLab token from glab config", "host", hostOf(cfg.BaseURL))
			}
		}
	}

	// What the release asset tools may touch is decided here, at startup, not
	// by the paths an agent later passes. Unset means they touch nothing.
	if *assetsDir != "" {
		cfg.FS = effect.Root(*assetsDir)
	}

	if cfg.Token == "" {
		logger.Warn("no GitLab token configured; only public projects will be readable")
	}

	c, err := gitlab.NewClient(cfg)
	if err != nil {
		logger.Error("failed to create gitlab client", "err", err)
		os.Exit(1)
	}

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "gitlab-mcp",
		Instructions: gitlab.Instructions,
		Logger:       logger.With("component", "mcp-sdk"),
	})

	gitlab.Register(s, c)

	if err := transport.Run(ctx, cmdutil.RunOptions{
		Name:   "gitlab-mcp",
		Server: s,
		Logger: logger.With("component", "transport"),
	}); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}

// hostOf extracts the hostname of a URL, so the glab config can be indexed by
// it. An empty or bare URL yields "", which makes Resolve fall back to glab's
// own default host.
func hostOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
