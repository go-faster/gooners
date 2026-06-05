// Package fs registers MCP tools for remote file operations (ls, cat, upload, download, grep).
package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/go-faster/gooners/internal/tools/mcputil"
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
	Download(ctx context.Context, sessionID, remotePath, localPath string) (string, error)
	DownloadStatus(ctx context.Context, sessionID, downloadID string) (session.DownloadStatusResponse, error)
	Run(ctx context.Context, sessionID string, cmd string) (sshutil.Result, error)
	RunWithOptions(ctx context.Context, sessionID string, cmd string, opts sshutil.RunOptions) (sshutil.Result, error)
}

func Register(s *mcp.Server, p SessionProvider, uploadRoot string) {
	mcputil.Register(s, mcputil.ToolDef{Name: "ls", Description: "List directory contents on remote machine using SFTP (returns JSON) with shell fallback.", Flags: mcputil.ReadOnly}, lsHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "cat", Description: "Read file contents (truncated) from remote using SFTP with shell fallback.", Flags: mcputil.ReadOnly}, catHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "grep", Description: "Search file contents on remote.", Flags: mcputil.ReadOnly}, grepHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "find", Description: "Find files/directories on remote.", Flags: mcputil.ReadOnly}, findHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "stat", Description: "Stat a path on remote using SFTP (returns JSON) with shell fallback.", Flags: mcputil.ReadOnly}, statHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "du", Description: "Get directory or file size (disk usage).", Flags: mcputil.ReadOnly}, duHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "truncate", Description: "Truncate file to given size on remote via SFTP.", Flags: mcputil.Destructive}, truncateHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "write_file", Description: "Write or overwrite a file on remote via SFTP.", Flags: mcputil.Destructive}, writeFileHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "upload_file", Description: "Upload a local file asynchronously to remote path via SFTP. Local path must be within the server's working directory. Returns an upload_id."}, uploadFileHandler(p, uploadRoot))
	mcputil.Register(s, mcputil.ToolDef{Name: "upload_status", Description: "Check the status of an asynchronous file upload.", Flags: mcputil.ReadOnly}, uploadStatusHandler(p))
	mcputil.Register(s, mcputil.ToolDef{Name: "download_file", Description: "Download a remote file asynchronously to local path via SFTP. Local path must be within the server's working directory. Returns a download_id."}, downloadFileHandler(p, uploadRoot))
	mcputil.Register(s, mcputil.ToolDef{Name: "download_status", Description: "Check the status of an asynchronous file download.", Flags: mcputil.ReadOnly}, downloadStatusHandler(p))
}

type fileInfo struct {
	Name    string      `json:"name"`
	Size    int64       `json:"size"`
	Mode    os.FileMode `json:"mode"`
	ModTime time.Time   `json:"mod_time"`
	IsDir   bool        `json:"is_dir"`
}

type lsParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string `json:"path" jsonschema:"Directory path to list"`
	All       bool   `json:"all,omitempty" jsonschema:"Include hidden files (like ls -a)"`
}

func lsHandler(p SessionProvider) mcp.ToolHandlerFor[lsParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args lsParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and path are required")
		}

		sftpClient, err := p.SFTP(ctx, args.SessionID)
		if err != nil {
			cmd := "ls -l"
			if args.All {
				cmd += "a"
			}
			cmd += " " + sshutil.Quote(args.Path)
			res, runErr := p.Run(ctx, args.SessionID, cmd)
			if runErr != nil {
				res.Error = runErr.Error()
			}
			cr := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
				IsError: runErr != nil,
			}
			return cr, mcputil.CommandResult{Text: res.Text()}, nil
		}
		defer func() { _ = sftpClient.Close() }()

		infos, err := sftpClient.ReadDir(args.Path)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}

		var files []fileInfo
		for _, info := range infos {
			if !args.All && strings.HasPrefix(info.Name(), ".") {
				continue
			}
			files = append(files, fileInfo{
				Name:    info.Name(),
				Size:    info.Size(),
				Mode:    info.Mode(),
				ModTime: info.ModTime(),
				IsDir:   info.IsDir(),
			})
		}

		b, _ := json.MarshalIndent(files, "", "  ")
		resText := string(b)
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: resText}},
		}
		return cr, mcputil.CommandResult{Text: resText}, nil
	}
}

type catParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"File path to read"`
	MaxBytes  float64 `json:"max_bytes,omitempty" jsonschema:"Maximum number of bytes to return"`
}

func catHandler(p SessionProvider) mcp.ToolHandlerFor[catParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args catParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and path are required")
		}
		maxBytes := args.MaxBytes
		if maxBytes <= 0 || maxBytes > maxCatBytes {
			maxBytes = maxCatBytes
		}

		sftpClient, err := p.SFTP(ctx, args.SessionID)
		if err != nil {
			cmd := fmt.Sprintf("head -c %d %s", int64(maxBytes), sshutil.Quote(args.Path))
			res, runErr := p.Run(ctx, args.SessionID, cmd)
			if runErr != nil {
				res.Error = runErr.Error()
			}
			cr := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
				IsError: runErr != nil,
			}
			return cr, mcputil.CommandResult{Text: res.Text()}, nil
		}
		defer func() { _ = sftpClient.Close() }()

		f, err := sftpClient.Open(args.Path)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}
		defer func() { _ = f.Close() }()

		data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}

		text := string(data)
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}
		return cr, mcputil.CommandResult{Text: text}, nil
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

func grepHandler(p SessionProvider) mcp.ToolHandlerFor[grepParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args grepParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Pattern == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id, pattern and path are required")
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
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

type findParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"Directory path to search in"`
	Name      string  `json:"name,omitempty" jsonschema:"File name pattern to match (e.g. *.txt)"`
	Type      string  `json:"type,omitempty" jsonschema:"File type (f=file, d=directory)"`
	MaxDepth  float64 `json:"max_depth,omitempty" jsonschema:"Maximum depth of directories to search"`
}

func findHandler(p SessionProvider) mcp.ToolHandlerFor[findParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args findParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and path are required")
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
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

type statParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string `json:"path" jsonschema:"File or directory path to stat"`
}

func statHandler(p SessionProvider) mcp.ToolHandlerFor[statParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args statParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and path are required")
		}

		sftpClient, err := p.SFTP(ctx, args.SessionID)
		if err != nil {
			cmd := "stat " + sshutil.Quote(args.Path)
			res, runErr := p.Run(ctx, args.SessionID, cmd)
			if runErr != nil {
				res.Error = runErr.Error()
			}
			cr := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
				IsError: runErr != nil,
			}
			return cr, mcputil.CommandResult{Text: res.Text()}, nil
		}
		defer func() { _ = sftpClient.Close() }()

		info, err := sftpClient.Stat(args.Path)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}

		fi := fileInfo{
			Name:    path.Base(args.Path),
			Size:    info.Size(),
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
			IsDir:   info.IsDir(),
		}

		b, _ := json.MarshalIndent(fi, "", "  ")
		resText := string(b)
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: resText}},
		}
		return cr, mcputil.CommandResult{Text: resText}, nil
	}
}

type duParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"Path to measure size for"`
	Human     bool    `json:"human,omitempty" jsonschema:"Use human readable sizes (du -h)"`
	Summarize bool    `json:"summarize,omitempty" jsonschema:"Summarize result (du -s)"`
	MaxDepth  float64 `json:"max_depth,omitempty" jsonschema:"Limit directory depth (du -d)"`
	All       bool    `json:"all,omitempty" jsonschema:"Include individual files (du -a)"`
}

func duHandler(p SessionProvider) mcp.ToolHandlerFor[duParams, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args duParams) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.CommandResult{}, fmt.Errorf("session_id and path are required")
		}
		cmd := "du"
		if args.Human {
			cmd += " -h"
		}
		if args.Summarize {
			cmd += " -s"
		}
		if args.All {
			cmd += " -a"
		}
		if args.MaxDepth > 0 {
			cmd += fmt.Sprintf(" -d %d", int64(args.MaxDepth))
		}
		cmd += " " + sshutil.Quote(args.Path)
		res, err := p.Run(ctx, args.SessionID, cmd)
		if err != nil {
			res.Error = err.Error()
		}
		cr := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
			IsError: err != nil,
		}
		return cr, mcputil.CommandResult{Text: res.Text()}, nil
	}
}

type truncateParams struct {
	SessionID string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path      string  `json:"path" jsonschema:"Remote path to truncate"`
	Size      float64 `json:"size" jsonschema:"New size in bytes (0 to empty file)"`
}

func truncateHandler(p SessionProvider) mcp.ToolHandlerFor[truncateParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args truncateParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" || args.Path == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id and path are required")
		}
		if args.Size < 0 {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("size must be >= 0")
		}

		timeout := 60.0
		truncCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		sftpClient, err := p.SFTP(truncCtx, args.SessionID)
		if err != nil {
			return nil, mcputil.SuccessResult{}, err
		}
		defer func() { _ = sftpClient.Close() }()
		if err := sftpClient.Truncate(args.Path, int64(args.Size)); err != nil {
			return nil, mcputil.SuccessResult{}, err
		}

		out := mcputil.SuccessResult{OK: true}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(out)}},
		}, out, nil
	}
}

type writeFileParams struct {
	SessionID  string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	Path       string  `json:"path" jsonschema:"Remote path to write to"`
	Content    string  `json:"content" jsonschema:"File content to write"`
	Mode       string  `json:"mode,omitempty" jsonschema:"File permissions (e.g. 0644)"`
	TimeoutSec float64 `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds"`
}

func writeFileHandler(p SessionProvider) mcp.ToolHandlerFor[writeFileParams, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args writeFileParams) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		if args.SessionID == "" || args.Path == "" || args.Content == "" {
			return nil, mcputil.SuccessResult{}, fmt.Errorf("session_id and path are required")
		}
		timeout := 60.0
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		writeCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		sftpClient, err := p.SFTP(writeCtx, args.SessionID)
		if err != nil {
			return nil, mcputil.SuccessResult{}, err
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
			return nil, mcputil.SuccessResult{}, err
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write([]byte(args.Content)); err != nil {
			return nil, mcputil.SuccessResult{}, err
		}
		if err := f.Chmod(mode); err != nil {
			return nil, mcputil.SuccessResult{}, err
		}

		out := mcputil.SuccessResult{OK: true}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(out)}},
		}, out, nil
	}
}

type uploadFileParams struct {
	SessionID  string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	LocalPath  string  `json:"local_path" jsonschema:"Local path on the MCP server to upload from"`
	RemotePath string  `json:"remote_path" jsonschema:"Remote path to upload to"`
	TimeoutSec float64 `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds for queuing the request"`
}

func uploadFileHandler(p SessionProvider, workDir string) mcp.ToolHandlerFor[uploadFileParams, mcputil.UploadResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args uploadFileParams) (*mcp.CallToolResult, mcputil.UploadResult, error) {
		if args.SessionID == "" || args.LocalPath == "" || args.RemotePath == "" {
			return nil, mcputil.UploadResult{}, fmt.Errorf("session_id, local_path and remote_path are required")
		}
		timeout := 60.0
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		uploadCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		safePath, err := withinDir(workDir, args.LocalPath)
		if err != nil {
			return nil, mcputil.UploadResult{}, err
		}
		uploadID, err := p.Upload(uploadCtx, args.SessionID, safePath, args.RemotePath)
		if err != nil {
			return nil, mcputil.UploadResult{}, err
		}
		out := mcputil.UploadResult{OK: true, UploadID: uploadID}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(out)}},
		}, out, nil
	}
}

type uploadStatusParams struct {
	SessionID string `json:"session_id" jsonschema:"The ID of the SSH session"`
	UploadID  string `json:"upload_id" jsonschema:"The upload ID returned by upload_file"`
}

func uploadStatusHandler(p SessionProvider) mcp.ToolHandlerFor[uploadStatusParams, mcputil.UploadStatusResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args uploadStatusParams) (*mcp.CallToolResult, mcputil.UploadStatusResult, error) {
		if args.SessionID == "" || args.UploadID == "" {
			return nil, mcputil.UploadStatusResult{}, fmt.Errorf("session_id and upload_id are required")
		}
		status, err := p.UploadStatus(ctx, args.SessionID, args.UploadID)
		if err != nil {
			return nil, mcputil.UploadStatusResult{}, err
		}
		sr := mcputil.UploadStatusResult{
			OK:            true,
			UploadID:      status.UploadID,
			BytesUploaded: status.BytesUploaded,
			TotalBytes:    status.TotalBytes,
			Percent:       status.Percent,
			Done:          status.Done,
		}
		if status.Err != nil {
			sr.Error = status.Err.Error()
			sr.OK = false
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(sr)}},
		}, sr, nil
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type downloadFileParams struct {
	SessionID  string  `json:"session_id" jsonschema:"The ID of the SSH session"`
	RemotePath string  `json:"remote_path" jsonschema:"Remote path to download from"`
	LocalPath  string  `json:"local_path" jsonschema:"Local path on the MCP server to download to"`
	TimeoutSec float64 `json:"timeout_s,omitempty" jsonschema:"Timeout in seconds for queuing the request"`
}

func downloadFileHandler(p SessionProvider, workDir string) mcp.ToolHandlerFor[downloadFileParams, mcputil.DownloadResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args downloadFileParams) (*mcp.CallToolResult, mcputil.DownloadResult, error) {
		if args.SessionID == "" || args.LocalPath == "" || args.RemotePath == "" {
			return nil, mcputil.DownloadResult{}, fmt.Errorf("session_id, local_path and remote_path are required")
		}
		timeout := 60.0
		if args.TimeoutSec > 0 {
			timeout = args.TimeoutSec
		}
		downloadCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		safePath, err := withinDir(workDir, args.LocalPath)
		if err != nil {
			return nil, mcputil.DownloadResult{}, err
		}
		downloadID, err := p.Download(downloadCtx, args.SessionID, args.RemotePath, safePath)
		if err != nil {
			return nil, mcputil.DownloadResult{}, err
		}
		out := mcputil.DownloadResult{OK: true, DownloadID: downloadID}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(out)}},
		}, out, nil
	}
}

type downloadStatusParams struct {
	SessionID  string `json:"session_id" jsonschema:"The ID of the SSH session"`
	DownloadID string `json:"download_id" jsonschema:"The download ID returned by download_file"`
}

func downloadStatusHandler(p SessionProvider) mcp.ToolHandlerFor[downloadStatusParams, mcputil.DownloadStatusResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args downloadStatusParams) (*mcp.CallToolResult, mcputil.DownloadStatusResult, error) {
		if args.SessionID == "" || args.DownloadID == "" {
			return nil, mcputil.DownloadStatusResult{}, fmt.Errorf("session_id and download_id are required")
		}
		status, err := p.DownloadStatus(ctx, args.SessionID, args.DownloadID)
		if err != nil {
			return nil, mcputil.DownloadStatusResult{}, err
		}
		sr := mcputil.DownloadStatusResult{
			OK:              true,
			DownloadID:      status.DownloadID,
			BytesDownloaded: status.BytesDownloaded,
			TotalBytes:      status.TotalBytes,
			Percent:         status.Percent,
			Done:            status.Done,
		}
		if status.Err != nil {
			sr.Error = status.Err.Error()
			sr.OK = false
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(sr)}},
		}, sr, nil
	}
}
