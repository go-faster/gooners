// Package fs registers MCP tools for remote file operations (ls, cat, upload, download, grep).
package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
)

const (
	maxCatBytes  = 10 * 1024 * 1024 // 10 MiB
	maxGrepLines = 10_000
)

// withinDir resolves path to an absolute path and verifies it is inside root.
func withinDir(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("resolving relative path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside allowed upload directory %q", path, root)
	}
	return abs, nil
}

type SessionProvider interface {
	Get(ctx context.Context, id string) (*ssh.Client, error)
	SFTP(ctx context.Context, id string) (*sftp.Client, error)
	Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error)
	UploadStatus(ctx context.Context, sessionID, uploadID string) (session.UploadStatusResponse, error)
}

func Register(s *server.MCPServer, p SessionProvider, uploadRoot string) {
	s.AddTool(mcp.NewTool("ls",
		mcp.WithDescription("List directory contents on remote machine."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithBoolean("long"),
		mcp.WithBoolean("all"),
	), lsHandler(p))

	s.AddTool(mcp.NewTool("cat",
		mcp.WithDescription("Read file contents (truncated) from remote."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("max_bytes"),
	), catHandler(p))

	s.AddTool(mcp.NewTool("grep",
		mcp.WithDescription("Search file contents on remote."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("pattern", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithBoolean("recursive"),
		mcp.WithBoolean("case_insensitive"),
		mcp.WithNumber("max_lines"),
	), grepHandler(p))

	s.AddTool(mcp.NewTool("find",
		mcp.WithDescription("Find files/directories on remote."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("name"),
		mcp.WithString("type"),
		mcp.WithNumber("max_depth"),
	), findHandler(p))

	s.AddTool(mcp.NewTool("stat",
		mcp.WithDescription("Stat a path on remote."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
	), statHandler(p))

	s.AddTool(mcp.NewTool("write_file",
		mcp.WithDescription("Write or overwrite a file on remote via SFTP."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("content", mcp.Required()),
		mcp.WithString("mode"),
	), writeFileHandler(p))

	s.AddTool(mcp.NewTool("upload_file",
		mcp.WithDescription("Upload a local file asynchronously to remote path via SFTP. Returns an upload_id."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("local_path", mcp.Required()),
		mcp.WithString("remote_path", mcp.Required()),
		mcp.WithNumber("timeout_s", mcp.Description("Timeout in seconds")),
	), uploadFileHandler(p, uploadRoot))

	s.AddTool(mcp.NewTool("upload_status",
		mcp.WithDescription("Check the status of an asynchronous file upload."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("upload_id", mcp.Required()),
	), uploadStatusHandler(p))
}

func lsHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "ls"
		if req.GetBool("long", false) {
			cmd += " -l"
		}
		if req.GetBool("all", false) {
			cmd += " -a"
		}
		cmd += " " + sshutil.Quote(path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func catHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		maxBytes := req.GetFloat("max_bytes", 0)
		if maxBytes <= 0 || maxBytes > maxCatBytes {
			maxBytes = maxCatBytes
		}
		cmd := fmt.Sprintf("head -c %d %s", int64(maxBytes), sshutil.Quote(path))
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func grepHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		pattern := req.GetString("pattern", "")
		path := req.GetString("path", "")
		if id == "" || pattern == "" || path == "" {
			return mcp.NewToolResultError("session_id, pattern and path are required"), nil
		}
		client, err := p.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "grep"
		if req.GetBool("recursive", false) {
			cmd += " -r"
		}
		if req.GetBool("case_insensitive", false) {
			cmd += " -i"
		}
		maxLines := req.GetFloat("max_lines", 0)
		if maxLines > 0 {
			cmd += fmt.Sprintf(" -m %d", int64(maxLines))
		}
		cmd += " " + sshutil.Quote(pattern) + " " + sshutil.Quote(path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func findHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "find " + sshutil.Quote(path)
		if n := req.GetString("name", ""); n != "" {
			cmd += " -name " + sshutil.Quote(n)
		}
		if t := req.GetString("type", ""); t != "" {
			cmd += " -type " + sshutil.Quote(t)
		}
		if d := req.GetFloat("max_depth", 0); d > 0 {
			cmd += fmt.Sprintf(" -maxdepth %d", int64(d))
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func statHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "stat " + sshutil.Quote(path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return mcp.NewToolResultError(res.Text()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func writeFileHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		content := req.GetString("content", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}

		timeoutSec := req.GetFloat("timeout_s", 0)
		if timeoutSec > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
			defer cancel()
		}

		sftpClient, err := p.SFTP(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer sftpClient.Close() //nolint:errcheck // sftp close error not actionable

		modeStr := req.GetString("mode", "")
		mode := os.FileMode(0o644)
		if modeStr != "" {
			var m uint32
			if _, err := fmt.Sscanf(modeStr, "%o", &m); err == nil {
				mode = os.FileMode(m)
			}
		}

		f, err := sftpClient.Create(path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer f.Close() //nolint:errcheck // file close error not actionable on defer
		if _, err := f.Write([]byte(content)); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := f.Chmod(mode); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, _ := jsonForOk()
		return mcp.NewToolResultText(b), nil
	}
}

func uploadFileHandler(p SessionProvider, uploadRoot string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		local := req.GetString("local_path", "")
		remote := req.GetString("remote_path", "")
		if id == "" || local == "" || remote == "" {
			return mcp.NewToolResultError("session_id, local_path and remote_path are required"), nil
		}

		timeoutSec := req.GetFloat("timeout_s", 0)
		if timeoutSec > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
			defer cancel()
		}

		safePath, err := withinDir(uploadRoot, local)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		uploadID, err := p.Upload(ctx, id, safePath, remote)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		res := map[string]any{
			"ok":        true,
			"upload_id": uploadID,
		}
		b, _ := json.Marshal(res)
		return mcp.NewToolResultText(string(b)), nil
	}
}

func uploadStatusHandler(p SessionProvider) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		uploadID := req.GetString("upload_id", "")
		if id == "" || uploadID == "" {
			return mcp.NewToolResultError("session_id and upload_id are required"), nil
		}

		status, err := p.UploadStatus(ctx, id, uploadID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		res := map[string]any{
			"ok":             true,
			"upload_id":      status.UploadID,
			"bytes_uploaded": status.BytesUploaded,
			"total_bytes":    status.TotalBytes,
			"percent":        status.Percent,
			"done":           status.Done,
		}
		if status.Err != nil {
			res["error"] = status.Err.Error()
			res["ok"] = false
		}

		b, _ := json.Marshal(res)
		return mcp.NewToolResultText(string(b)), nil
	}
}

func jsonForOk() (string, error) {
	b, err := json.Marshal(map[string]bool{"ok": true})
	return string(b), err
}
