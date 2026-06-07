// Package main is the entrypoint for the ssh-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/disk"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/proc"
	"github.com/go-faster/gooners/internal/tools/sysinfo"
	"github.com/go-faster/gooners/internal/tools/systemd"
)

func runServer(ctx context.Context, s *mcp.Server, transport, addr string) error {
	handler := func(*http.Request) *mcp.Server { return s }
	switch transport {
	case "stdio", "":
		slog.Info("starting ssh-mcp on stdio transport")
		return s.Run(ctx, &mcp.StdioTransport{})

	case "streamable-http":
		handler := mcp.NewStreamableHTTPHandler(handler, &mcp.StreamableHTTPOptions{
			Logger: slog.Default(),
		})
		slog.Info("starting ssh-mcp on streamable-http transport", "at", fmt.Sprintf("http://%s/mcp", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("streamable-http server exited with error: %w", err)
		}

	case "sse":
		handler := mcp.NewSSEHandler(handler, nil)
		slog.Info("starting ssh-mcp on SSE transport", "at", fmt.Sprintf("http://%s", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("sse server exited with error: %w", err)
		}

	default:
		return fmt.Errorf("unknown transport: %q", transport)
	}
	return nil
}

func main() {
	logLevel := slog.LevelInfo
	flag.TextVar(&logLevel, "log-level", &logLevel, "log level: debug, info, warn, error")
	var (
		logFile   = flag.String("log-file", "", "path to log file (enables structured debug logging)")
		logFormat = flag.String("log-format", "text", "log format: text, json")

		transport               = flag.String("transport", "stdio", "transport: stdio, streamable-http, sse")
		addr                    = flag.String("addr", ":8080", "listen address for HTTP transports (streamable-http, sse)")
		disableSudo             = flag.Bool("disable-sudo", false, "do not register the ssh_sudo_exec tool")
		disableSpecializedTools = flag.Bool("disable-specialized-tools", false, "register only core SSH tools: session management, exec, and file transfer")
		passwordFile            = flag.String("password-file", "", "file containing a password for all machines (re-read on each use)")
		passwordEnv             = flag.String("password-env", "", "env var containing a password for all machines")
		passwordConfig          = flag.String("password-config", "", "key=value config file mapping machine names to passwords (re-read on each use)")
		passwordCmd             = flag.String("password-cmd", "", "command called with machine name as first argument; stdout is used as the password (cached per machine)")
		commandTimeout          = flag.Duration("command-timeout", 10*time.Second, "default command timeout")
	)
	flag.Parse()

	out := os.Stdout
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Fatalf("opening log file: %v", err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}

	opts := &slog.HandlerOptions{Level: logLevel}
	var handler slog.Handler
	switch *logFormat {
	case "json":
		handler = slog.NewJSONHandler(out, opts)
	case "text", "":
		handler = slog.NewTextHandler(out, opts)
	default:
		log.Fatalf("unknown log format: %q", *logFormat)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)

	uploadRoot, err := os.Getwd()
	if err != nil {
		slog.Error("getting working directory", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var passwords core.PasswordProvider
	switch {
	case *passwordFile != "":
		passwords = &core.FilePasswordProvider{Path: *passwordFile}
		logger.Info("configured password provider", "type", "file", "path", *passwordFile)
	case *passwordEnv != "":
		passwords = &core.EnvPasswordProvider{VarName: *passwordEnv}
		logger.Info("configured password provider", "type", "env", "var", *passwordEnv)
	case *passwordConfig != "":
		passwords = &core.ConfigFilePasswordProvider{Path: *passwordConfig}
		logger.Info("configured password provider", "type", "config", "path", *passwordConfig)
	case *passwordCmd != "":
		passwords = &core.CommandPasswordProvider{Command: *passwordCmd}
		logger.Info("configured password provider", "type", "command")
	default:
		logger.Debug("no password provider configured")
	}

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "ssh-mcp",
		Instructions: "You are connected to ssh-mcp. Use these tools to safely query and manage remote machine state over SSH.",
		Logger:       logger.With("component", "mcp-sdk"),
		Prompts: []*mcp.Prompt{
			{
				Name:        "troubleshoot-ssh",
				Description: "Start a debugging session for a remote server via SSH",
				Arguments: []*mcp.PromptArgument{
					{Name: "machine", Description: "Name of the machine to connect to", Required: true},
				},
			},
		},
		PromptHandler: mcp.PromptHandler(func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			if req.Params.Name == "troubleshoot-ssh" {
				return &mcp.GetPromptResult{
					Description: "Instructions for using SSH tools to debug.",
					Messages: []*mcp.PromptMessage{
						{
							Role: "user",
							Content: &mcp.TextContent{
								Text: "Use `ssh_exec` and systemd tools to analyze the server.",
							},
						},
					},
				}, nil
			}
			return nil, fmt.Errorf("unknown prompt: %q", req.Params.Name)
		}),
	})

	pool := session.NewPool(session.PoolOptions{
		CommandTimeout: *commandTimeout,
		Logger:         logger,
		OnDisconnect: func(machine string, err error) {
			mcputil.BroadcastWarning(s, "ssh-mcp", fmt.Sprintf("SSH session to %s disconnected: %v", machine, err))
		},
	})
	go pool.RunLoop(ctx)

	logger.Debug("registering MCP tools")
	core.Register(s, pool, core.RegisterOptions{DisableSudo: *disableSudo, Passwords: passwords})
	if *disableSpecializedTools {
		fs.RegisterFileTransfer(s, pool, uploadRoot)
	} else {
		fs.Register(s, pool, uploadRoot)
		systemd.Register(s, pool)
		sysinfo.Register(s, pool)
		proc.Register(s, pool)
		disk.Register(s, pool)
	}
	logger.Info("MCP tools registered successfully", "disable_sudo", *disableSudo, "disable_specialized_tools", *disableSpecializedTools, "upload_root", uploadRoot)

	if err := runServer(ctx, s, *transport, *addr); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
