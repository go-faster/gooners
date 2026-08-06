// Package main is the entrypoint for the gitlab-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"golang.org/x/sync/errgroup"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/effect"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/gitlab"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
		blobFlags cmdutil.BlobFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)
	blobFlags.Register(flag.CommandLine)

	var (
		baseURL = flag.String("gitlab-url", os.Getenv("GITLAB_URL"), "GitLab instance URL; defaults to the glab CLI's configured host, then https://gitlab.com")
		token   = flag.String("gitlab-token", os.Getenv("GITLAB_TOKEN"), "GitLab API token; defaults to the glab CLI's stored token")
		project = flag.String("project", os.Getenv("GITLAB_PROJECT"), "default project (group/project) for tool calls that omit one")

		glabConfigDir = flag.String("glab-config-dir", "", "glab CLI config directory to read credentials from; defaults to glab's own location")
		noGlabConfig  = flag.Bool("no-glab-config", false, "do not read credentials from the glab CLI config")

		assetsDir = flag.String("assets-dir", "", "directory the release asset tools may read and write; they are disabled when unset")

		auth         = flag.String("auth", "server", "credential source: server (the configured token for everyone), client (each caller sends its own), client-optional (caller's token, else the configured one)")
		authInsecure = flag.Bool("auth-insecure", false, "allow caller-supplied tokens over plaintext HTTP")
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

	mode, err := gitlab.ParseAuthMode(*auth)
	if err != nil {
		// A mistyped flag deserves the accepted values, not a stack trace.
		logger.Error("invalid -auth", "value", *auth, "want", "server, client or client-optional")
		os.Exit(1)
	}
	clientAuth := mode != gitlab.AuthServer

	// A caller's token can only arrive on a header, and stdio has none, so
	// client auth over stdio would authenticate as nobody. Refusing beats a
	// server that silently reads every project as anonymous.
	if clientAuth && transport.Transport == "stdio" {
		logger.Error("client auth requires an HTTP transport", "auth", mode.String(), "transport", transport.Transport)
		os.Exit(1)
	}
	// Passthrough puts a credential on the wire on every session.
	if clientAuth && transport.TLSCertFile == "" && !*authInsecure {
		logger.Error("client auth over plaintext sends tokens in the clear; pass -tls-cert-file/-tls-key-file, or -auth-insecure to accept it", "auth", mode.String())
		os.Exit(1)
	}

	cfg := gitlab.Config{
		BaseURL:        *baseURL,
		Token:          *token,
		DefaultProject: *project,
	}

	// The glab CLI is the likeliest place a token already exists, so an
	// operator who has run `glab auth login` needs no further setup. Explicit
	// flags and environment still win.
	//
	// Under -auth=client the server holds no credential, so the only thing
	// worth reading from the glab config is the instance URL.
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
			if cfg.Token == "" && glabToken != "" && mode != gitlab.AuthClientRequired {
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

	// Where a downloaded asset goes when the agent cannot read this server's
	// filesystem. Unset leaves a store that refuses, naming the flag.
	blobStore, runBlob, err := blobFlags.Setup(cmdutil.BlobOptions{
		Name:   "gitlab-mcp",
		Logger: logger.With("component", "blob"),
	})
	if err != nil {
		logger.Error("invalid blob store configuration", "err", err)
		os.Exit(1)
	}
	cfg.Blob = blobStore

	switch {
	case mode == gitlab.AuthClientRequired:
		// Nothing should have set it, but an explicit -gitlab-token plus
		// -auth=client is a contradiction worth naming rather than obeying.
		if cfg.Token != "" {
			logger.Warn("ignoring the configured token: every session must present its own")
			cfg.Token = ""
		}
	case cfg.Token == "":
		logger.Warn("no GitLab token configured; only public projects will be readable")
	}

	clients, err := gitlab.NewClientSet(cfg)
	if err != nil {
		logger.Error("failed to create gitlab client", "err", err)
		os.Exit(1)
	}

	logger.Info("gitlab-mcp configured", "url", cfg.BaseURL, "auth", mode.String())

	handler := gitlab.NewSessionServer(gitlab.SessionServerOptions{
		Clients: clients,
		Mode:    mode,
		Server: mcputil.ServerConfig{
			Name:         "gitlab-mcp",
			Instructions: gitlab.Instructions,
			Logger:       logger.With("component", "mcp-sdk"),
		},
		Logger: logger.With("component", "auth"),
	})

	// The blob store serves on its own listener, so it runs alongside the MCP
	// transport rather than inside it; either one failing stops the other.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return runBlob(ctx) })
	g.Go(func() error {
		defer cancel()
		return transport.Run(ctx, cmdutil.RunOptions{
			Name:    "gitlab-mcp",
			Handler: handler,
			Logger:  logger.With("component", "transport"),
		})
	})
	if err := g.Wait(); err != nil {
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
