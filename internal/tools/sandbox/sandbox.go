// Package sandbox registers sandbox_open and sandbox_close: the only two
// tools sandbox-mcp adds on top of the tool subset it composes from
// internal/tools/core and friends (see cmd/sandbox-mcp).
package sandbox

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// Manager is the subset of *sandbox.Manager Register needs.
type Manager interface {
	Open(ctx context.Context, spec sandbox.Spec) (sandbox.OpenResult, error)
	Close(ctx context.Context, sessionID string) error
}

// Register adds sandbox_open and sandbox_close to s.
func Register(s *mcp.Server, m Manager) {
	mcputil.Register(s, mcputil.ToolDef{
		Name: "sandbox_open",
		Description: "Create a fresh, isolated sandbox container and open an SSH session into it. " +
			"The returned session_id works with every other SSH tool in this server exactly like a " +
			"session from ssh_open - ssh_exec, ssh_sudo_exec, ssh_ping, ssh_read_output, ssh_save_output, " +
			"cat, ls, find, grep, stat, du, upload_file, write_file, proc_*, disk_*, sys_*, and so on. " +
			"Each call gets its own container: nothing persists between sandbox_open calls, and other " +
			"conversations' sandboxes are never visible or reachable from this one. Call sandbox_close " +
			"when done to free resources immediately; idle sandboxes are torn down automatically otherwise.",
	}, openHandler(m))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "sandbox_close",
		Description: "Tear down a sandbox: closes its SSH session and destroys its container.",
		Flags:       mcputil.Destructive,
	}, closeHandler(m))
}

type openParams struct {
	Image   string            `json:"image,omitempty" jsonschema:"Container image to use; defaults to the server's configured default image"`
	Network string            `json:"network,omitempty" jsonschema:"Network tier to request: none (default, no NIC at all) or open (full egress). Must be allowed by server policy"`
	Env     map[string]string `json:"env,omitempty" jsonschema:"Additional environment variables for the sandboxed process"`
	Workdir string            `json:"workdir,omitempty" jsonschema:"Working directory inside the sandbox"`
}

func openHandler(m Manager) mcp.ToolHandlerFor[openParams, mcputil.SandboxResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args openParams) (*mcp.CallToolResult, mcputil.SandboxResult, error) {
		spec := sandbox.Spec{
			Image:   args.Image,
			Network: sandbox.Network(args.Network),
			Env:     args.Env,
			Workdir: args.Workdir,
		}
		res, err := m.Open(ctx, spec)
		if err != nil {
			return nil, mcputil.SandboxResult{}, err
		}
		return nil, mcputil.SandboxResult{
			SessionID: res.ID,
			Label:     res.Label,
			Image:     res.Image,
			Network:   string(res.Network),
		}, nil
	}
}

type closeParams struct {
	SessionID string `json:"session_id" jsonschema:"The session_id returned by sandbox_open"`
}

func closeHandler(m Manager) mcp.ToolHandlerFor[closeParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args closeParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id is required")
		}
		if err := m.Close(ctx, args.SessionID); err != nil {
			return nil, mcputil.SuccessResult{}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}
