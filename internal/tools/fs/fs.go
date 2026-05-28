package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/sftp"
)

func Register(s *server.MCPServer, p *session.Pool) {
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
		mcp.WithDescription("Upload a local file to remote path via SFTP."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("local_path", mcp.Required()),
		mcp.WithString("remote_path", mcp.Required()),
	), uploadFileHandler(p))
}

func lsHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(id)
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
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func catHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		max := req.GetFloat("max_bytes", 0)
		if max <= 0 {
			max = 65536
		}
		cmd := fmt.Sprintf("head -c %d %s", int64(max), sshutil.Quote(path))
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func grepHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		pattern := req.GetString("pattern", "")
		path := req.GetString("path", "")
		if id == "" || pattern == "" || path == "" {
			return mcp.NewToolResultError("session_id, pattern and path are required"), nil
		}
		client, err := p.Get(id)
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
		max := req.GetFloat("max_lines", 0)
		if max > 0 {
			cmd += fmt.Sprintf(" -m %d", int64(max))
		}
		cmd += " " + sshutil.Quote(pattern) + " " + sshutil.Quote(path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func findHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(id)
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
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func statHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		cmd := "stat " + sshutil.Quote(path)
		res, err := sshutil.Run(ctx, client, cmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text()), nil
	}
}

func writeFileHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		path := req.GetString("path", "")
		content := req.GetString("content", "")
		if id == "" || path == "" {
			return mcp.NewToolResultError("session_id and path are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sftpClient, err := sftp.NewClient(client)
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

func uploadFileHandler(p *session.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("session_id", "")
		local := req.GetString("local_path", "")
		remote := req.GetString("remote_path", "")
		if id == "" || local == "" || remote == "" {
			return mcp.NewToolResultError("session_id, local_path and remote_path are required"), nil
		}
		client, err := p.Get(id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer sftpClient.Close() //nolint:errcheck // sftp close error not actionable

		src, err := os.Open(local)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer src.Close() //nolint:errcheck // src close error not actionable

		dst, err := sftpClient.Create(remote)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer dst.Close() //nolint:errcheck // dst close error not actionable

		if _, err := io.Copy(dst, src); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, _ := jsonForOk()
		return mcp.NewToolResultText(b), nil
	}
}

func jsonForOk() (string, error) {
	b, err := json.Marshal(map[string]bool{"ok": true})
	return string(b), err
}
