// Package main is the entrypoint for the opencode-handoff-mcp MCP server.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/opencode"
)

const defaultLocalOpencodeURL = "http://localhost:4096"

type repeatFlag []string

func (f *repeatFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
		ocode     opencodeCfg
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)
	ocode.Register(flag.CommandLine)

	flag.Parse()

	closeLog, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, closeClient, err := ocode.Create(ctx, logger)
	if err != nil {
		slog.Error("create opencode client", "err", err)
		os.Exit(1)
	}
	defer closeClient()

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "opencode-handoff-mcp",
		Instructions: "You are connected to opencode-handoff-mcp. Use these tools to delegate coding tasks to opencode agents, monitor their sessions, and answer permission or clarification requests when needed.",
		Logger:       logger.With("component", "mcp-sdk"),
	})
	mgr := opencode.NewManagerWithStateDir(ctx, client, logger.With("component", "opencode-manager"), ocode.StateDir)
	opencode.Register(s, client, mgr)

	if err := transport.Run(ctx, "opencode-handoff-mcp", s, logger.WithGroup("transport")); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}

func envDefault(name, fallback string) string {
	return cmp.Or(os.Getenv(name), fallback)
}

func defaultStateDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "opencode-handoff-mcp", "jobs")
	}
	return ""
}

type opencodeCfg struct {
	Mode             string
	DefaultDirectory string
	RequestTimeout   time.Duration
	SyncTimeout      time.Duration
	StateDir         string

	BaseURL  string
	Username string
	Password string

	Env  repeatFlag
	Args repeatFlag
}

func (o *opencodeCfg) Register(_ *flag.FlagSet) {
	flag.StringVar(&o.Mode, "mode", os.Getenv("OPENCODE_MODE"), "opencode connection mode: local or remote; auto-selects local when -opencode-url is empty (env: OPENCODE_MODE)")
	flag.StringVar(&o.DefaultDirectory, "default-directory", os.Getenv("OPENCODE_DIRECTORY"), "default x-opencode-directory value (env: OPENCODE_DIRECTORY)")
	flag.DurationVar(&o.RequestTimeout, "request-timeout", 30*time.Second, "timeout for regular opencode HTTP calls")
	flag.DurationVar(&o.SyncTimeout, "sync-timeout", 5*time.Minute, "timeout for blocking prompt calls (session message POST) used by handoff_run and handoff_fire")
	flag.StringVar(&o.StateDir, "state-dir", envDefault("OPENCODE_HANDOFF_STATE_DIR", defaultStateDir()), "directory for persisting job state across restarts (env: OPENCODE_HANDOFF_STATE_DIR)")
	flag.StringVar(&o.BaseURL, "opencode-url", os.Getenv("OPENCODE_URL"), "opencode server base URL (env: OPENCODE_URL); defaults to http://localhost:4096 in local mode")
	flag.StringVar(&o.Username, "opencode-username", envDefault("OPENCODE_USERNAME", "opencode"), "opencode basic auth username (env: OPENCODE_USERNAME)")
	flag.StringVar(&o.Password, "opencode-password", os.Getenv("OPENCODE_PASSWORD"), "opencode basic auth password (env: OPENCODE_PASSWORD)")
	flag.Var(&o.Env, "opencode-env", "environment variable for local opencode serve, in KEY=VALUE form; may be repeated")
	flag.Var(&o.Args, "opencode-arg", "argument passed to local opencode serve; may be repeated")
}

func (o *opencodeCfg) Create(ctx context.Context, lg *slog.Logger) (*opencode.Client, func(), error) {
	mode := strings.TrimSpace(o.Mode)
	baseURL := strings.TrimSpace(o.BaseURL)
	if mode == "" {
		mode = "remote"
		if baseURL == "" {
			mode = "local"
		}
	}
	if baseURL == "" && mode == "local" {
		baseURL = defaultLocalOpencodeURL
	}
	clean := func() {}

	switch mode {
	case "local":
		client, stop, err := o.createLocal(ctx, baseURL, lg)
		if err != nil {
			return nil, clean, err
		}
		return client, stop, nil
	case "remote":
		client, err := o.createRemote(ctx, baseURL)
		if err != nil {
			return nil, clean, err
		}
		return client, clean, nil
	default:
		return nil, clean, fmt.Errorf("unknown mode %q: expected local or remote", mode)
	}
}

func (o *opencodeCfg) createRemote(_ context.Context, baseURL string) (*opencode.Client, error) {
	if o.BaseURL == "" {
		return nil, errors.New("remote mode requires -opencode-url or OPENCODE_URL")
	}
	return opencode.NewClient(
		opencode.Config{
			BaseURL:          baseURL,
			Username:         o.Username,
			Password:         o.Password,
			DefaultDirectory: o.DefaultDirectory,
			SyncTimeout:      o.SyncTimeout,
		},
		o.RequestTimeout,
	)
}

func (o *opencodeCfg) createLocal(ctx context.Context, defaultBaseURL string, lg *slog.Logger) (_ *opencode.Client, _ func(), rerr error) {
	baseURL, cleanup, err := o.startLocal(ctx, defaultBaseURL, lg)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if rerr != nil {
			cleanup()
		}
	}()

	client, err := opencode.NewClient(
		opencode.Config{
			BaseURL:          baseURL,
			Username:         o.Username,
			Password:         o.Password,
			DefaultDirectory: o.DefaultDirectory,
			SyncTimeout:      o.SyncTimeout,
		},
		o.RequestTimeout,
	)
	if err != nil {
		return nil, nil, err
	}
	return client, cleanup, nil
}

func (o *opencodeCfg) startLocal(ctx context.Context, defaultBaseURL string, lg *slog.Logger) (baseURL string, stop func(), _ error) {
	baseURL = defaultBaseURL
	argv := append([]string{"serve"}, o.Args...)
	env := append([]string(nil), o.Env...)

	if baseURL == defaultLocalOpencodeURL {
		// Find a free port. There is a small TOCTOU window between Close and
		// opencode's Listen; in practice this is acceptable since port conflicts
		// are rare and opencode will fail to start with a clear error.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", nil, fmt.Errorf("listen for random port: %w", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

		argv = append(argv, "--port", strconv.Itoa(port), "--mdns-domain", fmt.Sprintf("opencode-handoff-%d.local", port))
		env = append(env, fmt.Sprintf("OPENCODE_MCP_INSTANCE=opencode-handoff-%d", port))
	}

	cmd := exec.CommandContext(ctx, "opencode", argv...) //nolint:gosec // Operator-controlled local opencode invocation.
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	setProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return "", func() {}, fmt.Errorf("start opencode serve: %w", err)
	}
	lg.Info("started local opencode serve", "pid", cmd.Process.Pid, "args", argv)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	stop = func() {
		if cmd.Process == nil {
			return
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-done:
			if err != nil {
				lg.Debug("local opencode serve exited", "err", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}

	return baseURL, stop, nil
}
