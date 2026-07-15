// Package main is the entrypoint for the ssh-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/effect"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/disk"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/proc"
	"github.com/go-faster/gooners/internal/tools/sysinfo"
	"github.com/go-faster/gooners/internal/tools/systemd"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)
	var (
		disableSudo             = flag.Bool("disable-sudo", false, "do not register the ssh_sudo_exec tool")
		disableSpecializedTools = flag.Bool("disable-specialized-tools", false, "register only core SSH tools: session management, exec, and file transfer")
		passwordFile            = flag.String("password-file", "", "file containing a password for all machines (re-read on each use)")
		passwordEnv             = flag.String("password-env", "", "env var containing a password for all machines")
		passwordConfig          = flag.String("password-config", "", "key=value config file mapping machine names to passwords (re-read on each use)")
		passwordCmd             = flag.String("password-cmd", "", "command called with machine name as first argument; stdout is used as the password (cached per machine)")
		commandTimeout          = flag.Duration("command-timeout", 10*time.Second, "default command timeout")
	)
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

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
		// Every host file the tools may touch — upload sources, download
		// targets, saved output, stdin_file — confined to the working
		// directory, in one place rather than per tool.
		LocalFS: effect.Root(uploadRoot),
		OnDisconnect: func(machine string, err error) {
			mcputil.BroadcastWarning(s, "ssh-mcp", fmt.Sprintf("SSH session to %s disconnected: %v", machine, err))
		},
	})
	go pool.RunLoop(ctx)

	logger.Debug("registering MCP tools")
	core.Register(s, pool, core.RegisterOptions{DisableSudo: *disableSudo, Passwords: passwords})
	if *disableSpecializedTools {
		fs.RegisterFileTransfer(s, pool)
	} else {
		fs.Register(s, pool)
		systemd.Register(s, pool)
		sysinfo.Register(s, pool)
		proc.Register(s, pool)
		disk.Register(s, pool)
	}
	logger.Info("MCP tools registered successfully", "disable_sudo", *disableSudo, "disable_specialized_tools", *disableSpecializedTools, "upload_root", uploadRoot)

	if err := transport.Run(ctx, cmdutil.RunOptions{
		Name:   "ssh-mcp",
		Server: s,
		Logger: logger.With("component", "transport"),
	}); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
