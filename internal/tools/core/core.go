// Package core registers the core MCP tools for opening SSH sessions and running commands.
package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type RegisterOptions struct {
	// DisableSudo prevents registration of the ssh_sudo_exec tool.
	DisableSudo bool
	// Passwords is a server-level provider for both SSH login passwords and sudo
	// passwords. It is keyed by machine name and consulted when no per-call
	// password is supplied. Supports env, file, config-file, and exec sources.
	Passwords PasswordProvider
	Logger    *slog.Logger
}

func (opts *RegisterOptions) logger() *slog.Logger {
	if opts.Logger == nil {
		return slog.Default()
	}
	return opts.Logger
}

func Register(s *mcp.Server, p *session.Pool, opts RegisterOptions) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_open",
		Description: "Open SSH connection using defaults from ~/.ssh/config and keys. Falls back to a server-level password source if configured.",
	}, openHandler(p, opts.Passwords, opts.logger()))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_open_cfg",
		Description: "Open SSH connection with explicit parameters.",
	}, openCfgHandler(p, opts.logger()))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_close",
		Description: "Close an open SSH session.",
		Flags:       mcputil.Destructive,
	}, closeHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_list",
		Description: "List all currently open SSH sessions.",
		Flags:       mcputil.ReadOnly,
	}, listHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_list_machines",
		Description: "List known machines from ~/.ssh/config (and Includes). Returns only connection name and username.",
		Flags:       mcputil.ReadOnly,
	}, listMachinesHandler())

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_exec",
		Description: "Execute a command on an open SSH session. Prefer specialized tools (cat, grep, find, ls, stat, du, proc_list, etc.) over this when they cover the task.",
	}, execHandler(p, false, nil, opts.logger()))

	if !opts.DisableSudo {
		mcputil.Register(s, mcputil.ToolDef{
			Name:        "ssh_sudo_exec",
			Description: "Execute a command with sudo on an open SSH session. Prefer specialized tools when they cover the task; use this only when elevated privileges are required. If sudo requires a password, pass it via sudo_password or configure a server-level source (-password-file/-env/-config/-cmd). Otherwise uses sudo -n.",
		}, execHandler(p, true, opts.Passwords, opts.logger()))
	}

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_once_exec",
		Description: "Open a temporary SSH session, run one command, then close it. Prefer specialized tools (cat, grep, find, ls, stat, du, proc_list, etc.) over this when they cover the task.",
	}, onceHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_ping",
		Description: "Check if an SSH session is alive by sending a keepalive ping.",
		Flags:       mcputil.ReadOnly,
	}, pingHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_read_output",
		Description: "Read the content of a truncated execution output spool file.",
		Flags:       mcputil.ReadOnly,
	}, readOutputHandler(p))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "ssh_save_output",
		Description: "Move/save a truncated execution output spool file to a persistent local file path. Note: this consumes and invalidates the spool ID.",
	}, saveOutputHandler(p))
}

type openParams struct {
	Machine string `json:"machine" jsonschema:"host, user@host, host:port etc."`
}

func openHandler(p *session.Pool, passwords PasswordProvider, logger *slog.Logger) mcp.ToolHandlerFor[openParams, mcputil.SessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openParams) (*mcp.CallToolResult, mcputil.SessionResult, error) {
		if args.Machine == "" {
			return nil, mcputil.SessionResult{}, fmt.Errorf("machine is required")
		}

		openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		var pwd string
		if passwords != nil {
			logger.Debug("resolving SSH password via configured provider", "machine", args.Machine)
			var pwdErr error
			pwd, pwdErr = passwords.Password(openCtx, args.Machine)
			if pwdErr != nil && !errors.Is(pwdErr, ErrPasswordNotFound) {
				logger.Error("failed to resolve SSH password", "machine", args.Machine, "err", pwdErr)
				return nil, mcputil.SessionResult{}, fmt.Errorf("resolving SSH password: %w", pwdErr)
			}
			if pwd == "" {
				logger.Debug("no password returned by provider (fallback to keys/agent)", "machine", args.Machine)
			} else {
				logger.Debug("password successfully resolved via provider", "machine", args.Machine)
			}
		}
		var res session.OpenResult
		var err error
		if pwd != "" {
			res, err = p.OpenCfg(openCtx, session.Config{Machine: args.Machine, Password: pwd})
		} else {
			res, err = p.Open(openCtx, args.Machine)
		}
		if err != nil {
			logger.Error("failed to open SSH session", "machine", args.Machine, "err", err)
			return nil, mcputil.SessionResult{}, err
		}
		logger.Info("opened SSH session", "machine", args.Machine, "session_id", res.ID)
		return nil, mcputil.SessionResult{SessionID: res.ID, UserAgent: res.UserAgent, Banner: res.Banner, Platform: res.Platform}, nil
	}
}

type openCfgParams struct {
	Machine    string `json:"machine" jsonschema:"Host to connect to"`
	User       string `json:"user,omitempty" jsonschema:"SSH username"`
	Port       int    `json:"port,omitempty" jsonschema:"SSH port"`
	KeyPath    string `json:"key_path,omitempty" jsonschema:"Path to private key"`
	Password   string `json:"password,omitempty" jsonschema:"SSH password"`
	TimeoutSec int    `json:"timeout_s,omitempty" jsonschema:"Connection timeout in seconds"`
	KnownHosts string `json:"known_hosts,omitempty" jsonschema:"Path to known_hosts file"`
}

func openCfgHandler(p *session.Pool, logger *slog.Logger) mcp.ToolHandlerFor[openCfgParams, mcputil.SessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openCfgParams) (*mcp.CallToolResult, mcputil.SessionResult, error) {
		if args.Machine == "" {
			return nil, mcputil.SessionResult{}, fmt.Errorf("machine is required")
		}

		cfg := session.Config{
			Machine:    args.Machine,
			User:       args.User,
			Port:       args.Port,
			KeyPath:    args.KeyPath,
			Password:   args.Password,
			TimeoutSec: args.TimeoutSec,
			KnownHosts: args.KnownHosts,
		}

		timeout := 30
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}

		openCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		res, err := p.OpenCfg(openCtx, cfg)
		if err != nil {
			logger.Error("failed to open explicit SSH session", "machine", cfg.Machine, "user", cfg.User, "err", err)
			return nil, mcputil.SessionResult{}, err
		}
		logger.Info("opened explicit SSH session", "machine", cfg.Machine, "session_id", res.ID)

		result := mcputil.SessionResult{
			SessionID: res.ID,
			UserAgent: res.UserAgent,
			Banner:    res.Banner,
			Platform:  res.Platform,
		}
		return nil, result, nil
	}
}

type closeParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session to close"`
}

func closeHandler(p *session.Pool) mcp.ToolHandlerFor[closeParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args closeParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id is required")
		}
		_ = p.Close(ctx, args.SessionID)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func listHandler(p *session.Pool) mcp.ToolHandlerFor[struct{}, mcputil.SessionsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcputil.SessionsResult, error) {
		list, err := p.List(ctx)
		if err != nil {
			return nil, mcputil.SessionsResult{}, err
		}
		return nil, mcputil.SessionsResult{Sessions: list}, nil
	}
}

func listMachinesHandler() mcp.ToolHandlerFor[struct{}, mcputil.MachinesResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcputil.MachinesResult, error) {
		machines := session.ListMachines()
		return nil, mcputil.MachinesResult{Machines: machines}, nil
	}
}

type execParams struct {
	SessionID    string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Command      string `json:"command" jsonschema:"Command to execute"`
	Description  string `json:"description,omitempty" jsonschema:"Optional description of what this command will do (appended as a comment)"`
	Cwd          string `json:"cwd,omitempty" jsonschema:"Working directory for the command"`
	TimeoutSec   int    `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
	SudoPassword string `json:"sudo_password,omitempty" jsonschema:"Sudo password if required"`
}

func execHandler(p *session.Pool, sudo bool, passwords PasswordProvider, logger *slog.Logger) mcp.ToolHandlerFor[execParams, mcputil.ExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args execParams) (*mcp.CallToolResult, mcputil.ExecResult, error) {
		if args.SessionID == "" || args.Command == "" {
			return nil, mcputil.ExecResult{}, fmt.Errorf("session_id and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, mcputil.ExecResult{}, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		sudoPwd := args.SudoPassword
		if sudoPwd == "" && sudo && passwords != nil {
			logger.Debug("resolving sudo password via provider for exec", "session", args.SessionID)
			machine, err := p.Machine(ctx, args.SessionID)
			if err != nil {
				logger.Warn("could not lookup machine name for session", "session", args.SessionID, "err", err)
				return nil, mcputil.ExecResult{}, fmt.Errorf("looking up session machine: %w", err)
			}
			pwd, err := passwords.Password(ctx, machine)
			if err != nil && !errors.Is(err, ErrPasswordNotFound) {
				logger.Error("failed to resolve sudo password", "machine", machine, "err", err)
				return nil, mcputil.ExecResult{}, fmt.Errorf("resolving sudo password: %w", err)
			}
			if pwd == "" {
				logger.Debug("no sudo password returned by provider (exec will use sudo -n)", "machine", machine)
			} else {
				logger.Debug("sudo password successfully resolved via provider", "machine", machine)
			}
			sudoPwd = pwd
		}
		timeout := p.CommandTimeout()
		if args.TimeoutSec > 0 {
			timeout = time.Duration(args.TimeoutSec) * time.Second
		}
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res := p.Exec(execCtx, session.ExecRequest{
			SessionID:    args.SessionID,
			Command:      args.Command,
			Description:  args.Description,
			Cwd:          args.Cwd,
			Sudo:         sudo,
			SudoPassword: sudoPwd,
		})
		if res.Err != nil {
			logger.Warn("command execution completed with error", "session_id", args.SessionID, "sudo", sudo, "err", res.Err)
		} else {
			logger.Debug("command execution successful", "session_id", args.SessionID, "sudo", sudo)
		}
		return execResult(res)
	}
}

type onceParams struct {
	Machine     string `json:"machine" jsonschema:"Host to connect to"`
	Command     string `json:"command" jsonschema:"Command to execute"`
	Description string `json:"description,omitempty" jsonschema:"Optional description of what this command will do (appended as a comment)"`
	Cwd         string `json:"cwd,omitempty" jsonschema:"Working directory for the command"`
	TimeoutSec  int    `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
}

func onceHandler(p *session.Pool) mcp.ToolHandlerFor[onceParams, mcputil.ExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args onceParams) (*mcp.CallToolResult, mcputil.ExecResult, error) {
		if args.Machine == "" || args.Command == "" {
			return nil, mcputil.ExecResult{}, fmt.Errorf("machine and command are required")
		}
		if len(args.Command) > 50000 {
			return nil, mcputil.ExecResult{}, fmt.Errorf("command exceeds maximum allowed length of 50000 characters")
		}
		timeout := p.CommandTimeout()
		if args.TimeoutSec > 0 {
			timeout = time.Duration(args.TimeoutSec) * time.Second
		}
		onceCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		openRes, err := p.Open(onceCtx, args.Machine)
		if err != nil {
			return nil, mcputil.ExecResult{}, err
		}
		defer func() { _ = p.Close(ctx, openRes.ID) }() // Use parent context for closing

		res := p.Exec(onceCtx, session.ExecRequest{
			SessionID:   openRes.ID,
			Command:     args.Command,
			Description: args.Description,
			Cwd:         args.Cwd,
		})
		return execResult(res)
	}
}

//nolint:unparam // satisfies mcp.ToolHandlerFor signature pattern even if unused
func execResult(res session.ExecResponse) (*mcp.CallToolResult, mcputil.ExecResult, error) {
	e := mcputil.ExecResult{
		Stdout:        res.Stdout,
		Stderr:        res.Stderr,
		StdoutSize:    res.StdoutSize,
		StderrSize:    res.StderrSize,
		StdoutSpoolID: res.StdoutSpoolID,
		StderrSpoolID: res.StderrSpoolID,
	}
	if res.StdoutSpoolID != "" || res.StderrSpoolID != "" {
		e.Message = "Some output streams were truncated. Use specialized tools like ssh_read_output or ssh_save_output to access the full content."
	}
	if res.ExitCode != 0 {
		e.ExitCode = res.ExitCode
	}
	if res.Err != nil {
		e.Error = res.Err.Error()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(e)}},
		IsError: res.Err != nil,
	}, e, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type pingParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session to ping"`
}

func pingHandler(p *session.Pool) mcp.ToolHandlerFor[pingParams, mcputil.PingResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args pingParams) (*mcp.CallToolResult, mcputil.PingResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.PingResult{}, fmt.Errorf("session_id is required")
		}

		pingCtx, cancel := context.WithTimeout(ctx, p.CommandTimeout())
		defer cancel()

		dur, err := p.Ping(pingCtx, args.SessionID)
		if err != nil {
			return nil, mcputil.PingResult{}, fmt.Errorf("ping failed: %w", err)
		}
		return nil, mcputil.PingResult{
			Status: "ok",
			Time:   dur.String(),
		}, nil
	}
}

type readOutputParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	SpoolID   string `json:"spool_id" jsonschema:"The opaque spool ID of the truncated output"`
	Lines     int    `json:"lines,omitempty" jsonschema:"Number of lines to read (default 100, max 500)"`
	FromEnd   bool   `json:"from_end,omitempty" jsonschema:"Read from the end of the file (tail behavior) instead of the beginning"`
}

func readOutputHandler(p *session.Pool) mcp.ToolHandlerFor[readOutputParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args readOutputParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.SpoolID == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and spool_id are required")
		}
		lines := args.Lines
		if lines <= 0 {
			lines = 100
		}
		if lines > 500 {
			lines = 500
		}

		path, err := p.GetSpool(ctx, args.SessionID, args.SpoolID)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}

		var content string
		if args.FromEnd {
			content, err = readTail(path, lines, 16384)
		} else {
			content, err = readHead(path, lines, 16384)
		}
		if err != nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("reading spool output: %w", err)
		}

		return nil, mcputil.CommandResult{Text: content}, nil
	}
}

type saveOutputParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	SpoolID   string `json:"spool_id" jsonschema:"The opaque spool ID to save"`
	LocalPath string `json:"local_path" jsonschema:"The destination local file path to save the spool output to"`
}

func saveOutputHandler(p *session.Pool) mcp.ToolHandlerFor[saveOutputParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args saveOutputParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" || args.SpoolID == "" || args.LocalPath == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id, spool_id, and local_path are required")
		}

		path, err := p.GetSpool(ctx, args.SessionID, args.SpoolID)
		if err != nil {
			return nil, mcputil.SuccessResult{}, err
		}

		if err := renameOrCopy(path, args.LocalPath); err != nil {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("saving spool output: %w", err)
		}

		_ = p.DeleteSpool(ctx, args.SessionID, args.SpoolID)

		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func readHead(path string, maxLines int, maxBytes int64) (string, error) {
	//nolint:gosec // path is validated by spool registry
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	reader := io.LimitReader(f, maxBytes)
	r := bufio.NewReader(reader)
	lineCount := 0
	truncated := false
	for lineCount < maxLines {
		line, err := r.ReadString('\n')
		if line != "" {
			buf.WriteString(line)
			lineCount++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}

	if lineCount == maxLines {
		// Check if there is more data
		_, err := r.ReadByte()
		if err == nil {
			truncated = true
		} else if !errors.Is(err, io.EOF) {
			return "", err
		}
	}

	var dummy [1]byte
	n, _ := f.Read(dummy[:])
	if n > 0 {
		truncated = true
	}

	out := buf.String()
	if truncated {
		out += "\n... [Output truncated due to size/line limit] ..."
	}
	return out, nil
}

func readTail(path string, maxLines int, maxBytes int64) (string, error) {
	//nolint:gosec // path is validated by spool registry
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	size := stat.Size()
	offset := int64(0)
	truncated := false
	if size > maxBytes {
		offset = size - maxBytes
		truncated = true
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if offset > 0 && len(lines) > 0 && len(lines[0]) == 0 {
		lines = lines[1:]
	}

	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}

	outBytes := bytes.Join(lines, []byte("\n"))
	out := string(outBytes)
	if truncated {
		out = "... [Output truncated due to size/line limit] ...\n" + out
	}
	return out, nil
}

func renameOrCopy(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	//nolint:gosec // src is validated by spool registry
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	//nolint:gosec // user destination path
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}

	_ = in.Close()
	return os.Remove(src)
}
