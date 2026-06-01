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

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func Register(s *mcp.Server, p SessionProvider, uploadRoot string) {
	mcp.AddTool(s, &mcp.Tool{Name: "ls", Description: "List directory contents on remote machine."}, lsHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "cat", Description: "Read file contents (truncated) from remote."}, catHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "grep", Description: "Search file contents on remote."}, grepHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "find", Description: "Find files/directories on remote."}, findHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "stat", Description: "Stat a path on remote."}, statHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "write_file", Description: "Write or overwrite a file on remote via SFTP."}, writeFileHandler(p))
	mcp.AddTool(s, &mcp.Tool{Name: "upload_file", Description: "Upload a local file asynchronously to remote path via SFTP. Returns an upload_id."}, uploadFileHandler(p, uploadRoot))
	mcp.AddTool(s, &mcp.Tool{Name: "upload_status", Description: "Check the status of an asynchronous file upload."}, uploadStatusHandler(p))
}

type lsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string `json:"path" jsonschema:"Directory path to list"`
	Long      bool   `json:"long,omitempty" jsonschema:"Return long format (like ls -l)"`
	All       bool   `json:"all,omitempty" jsonschema:"Include hidden files (like ls -a)"`
}

func lsHandler(p SessionProvider) mcp.ToolHandlerFor[lsParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args lsParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, nil, fmt.Errorf("session_id and path are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "ls"
		if args.Long {
			cmd += " -l"
		}
		if args.All {
			cmd += " -a"
		}
		cmd += " " + sshutil.Quote(args.Path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
				IsError: true,
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
		}, nil, nil
	}
}

type catParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"File path to read"`
	MaxBytes  float64 `json:"max_bytes,omitempty" jsonschema:"Maximum number of bytes to return"`
}

func catHandler(p SessionProvider) mcp.ToolHandlerFor[catParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args catParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, nil, fmt.Errorf("session_id and path are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		maxBytes := args.MaxBytes
		if maxBytes <= 0 || maxBytes > maxCatBytes {
			maxBytes = maxCatBytes
		}
		cmd := fmt.Sprintf("head -c %d %s", int64(maxBytes), sshutil.Quote(args.Path))
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

type grepParams struct {
	SessionID       string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Pattern         string  `json:"pattern" jsonschema:"Search pattern/regex"`
	Path            string  `json:"path" jsonschema:"File or directory to search in"`
	Recursive       bool    `json:"recursive,omitempty" jsonschema:"Search recursively"`
	CaseInsensitive bool    `json:"case_insensitive,omitempty" jsonschema:"Ignore case"`
	MaxLines        float64 `json:"max_lines,omitempty" jsonschema:"Maximum matching lines to return"`
}

func grepHandler(p SessionProvider) mcp.ToolHandlerFor[grepParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args grepParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Pattern == "" || args.Path == "" {
			return nil, nil, fmt.Errorf("session_id, pattern and path are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "grep"
		if args.Recursive {
			cmd += " -r"
		}
		if args.CaseInsensitive {
			cmd += " -i"
		}
		if args.MaxLines > 0 {
			cmd += fmt.Sprintf(" -m %d", int64(args.MaxLines))
		}
		cmd += " " + sshutil.Quote(args.Pattern) + " " + sshutil.Quote(args.Path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

type findParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"Directory path to search in"`
	Name      string  `json:"name,omitempty" jsonschema:"File name pattern to match (e.g. *.txt)"`
	Type      string  `json:"type,omitempty" jsonschema:"File type (f=file, d=directory)"`
	MaxDepth  float64 `json:"max_depth,omitempty" jsonschema:"Maximum depth of directories to search"`
}

func findHandler(p SessionProvider) mcp.ToolHandlerFor[findParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args findParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, nil, fmt.Errorf("session_id and path are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "find " + sshutil.Quote(args.Path)
		if args.Name != "" {
			cmd += " -name " + sshutil.Quote(args.Name)
		}
		if args.Type != "" {
			cmd += " -type " + sshutil.Quote(args.Type)
		}
		if args.MaxDepth > 0 {
			cmd += fmt.Sprintf(" -maxdepth %d", int64(args.MaxDepth))
		}
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

type statParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string `json:"path" jsonschema:"File or directory path to stat"`
}

func statHandler(p SessionProvider) mcp.ToolHandlerFor[statParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args statParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, nil, fmt.Errorf("session_id and path are required")
		}
		client, err := p.Get(ctx, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd := "stat " + sshutil.Quote(args.Path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			res.Error = err.Error()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}, IsError: true}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}}}, nil, nil
	}
}

type writeFileParams struct {
	SessionID  string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path       string  `json:"path" jsonschema:"Remote path to write to"`
	Content    string  `json:"content" jsonschema:"File content to write"`
	Mode       string  `json:"mode,omitempty" jsonschema:"File permissions (e.g. 0644)"`
	TimeoutSec float64 `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
}

func writeFileHandler(p SessionProvider) mcp.ToolHandlerFor[writeFileParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args writeFileParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.Path == "" || args.Content == "" {
			return nil, nil, fmt.Errorf("session_id and path are required")
		}
		c := ctx
		if args.TimeoutSec > 0 {
			var cancel context.CancelFunc
			c, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutSec)*time.Second)
			defer cancel()
		}

		sftpClient, err := p.SFTP(c, args.SessionID)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = sftpClient.Close() }()

		mode := os.FileMode(0o644)
		if args.Mode != "" {
			var m uint32
			if _, err := fmt.Sscanf(args.Mode, "%o", &m); err == nil {
				mode = os.FileMode(m)
			}
		}

		f, err := sftpClient.Create(args.Path)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write([]byte(args.Content)); err != nil {
			return nil, nil, err
		}
		if err := f.Chmod(mode); err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: mustJSONOk()}}}, nil, nil
	}
}

type uploadFileParams struct {
	SessionID  string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	LocalPath  string  `json:"local_path" jsonschema:"Local path on the MCP server to upload from"`
	RemotePath string  `json:"remote_path" jsonschema:"Remote path to upload to"`
	TimeoutSec float64 `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
}

func uploadFileHandler(p SessionProvider, uploadRoot string) mcp.ToolHandlerFor[uploadFileParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args uploadFileParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.LocalPath == "" || args.RemotePath == "" {
			return nil, nil, fmt.Errorf("session_id, local_path and remote_path are required")
		}
		c := ctx
		if args.TimeoutSec > 0 {
			var cancel context.CancelFunc
			c, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutSec)*time.Second)
			defer cancel()
		}
		safePath, err := withinDir(uploadRoot, args.LocalPath)
		if err != nil {
			return nil, nil, err
		}
		uploadID, err := p.Upload(c, args.SessionID, safePath, args.RemotePath)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(map[string]any{"ok": true, "upload_id": uploadID})}},
		}, nil, nil
	}
}

type uploadStatusParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	UploadID  string `json:"upload_id" jsonschema:"The upload ID returned by upload_file"`
}

func uploadStatusHandler(p SessionProvider) mcp.ToolHandlerFor[uploadStatusParams, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args uploadStatusParams) (*mcp.CallToolResult, any, error) {
		if args.SessionID == "" || args.UploadID == "" {
			return nil, nil, fmt.Errorf("session_id and upload_id are required")
		}
		status, err := p.UploadStatus(ctx, args.SessionID, args.UploadID)
		if err != nil {
			return nil, nil, err
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
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(res)}}}, nil, nil
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustJSONOk() string {
	b, _ := json.Marshal(map[string]bool{"ok": true})
	return string(b)
}
