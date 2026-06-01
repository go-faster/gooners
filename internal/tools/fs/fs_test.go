package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/session"
)

type dummyPool struct {
	client *ssh.Client
}

func (p *dummyPool) Get(ctx context.Context, id string) (*ssh.Client, error) {
	return p.client, nil
}

func (p *dummyPool) SFTP(ctx context.Context, id string) (*sftp.Client, error) {
	if p.client == nil {
		return nil, fmt.Errorf("no client in dummy pool")
	}
	return sftp.NewClient(p.client)
}

func (p *dummyPool) Upload(ctx context.Context, sessionID, localPath, remotePath string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err == nil {
		_ = os.WriteFile(remotePath, data, 0o644)
	}
	return "upload-123", nil
}

func (p *dummyPool) UploadStatus(ctx context.Context, sessionID, uploadID string) (session.UploadStatusResponse, error) {
	return session.UploadStatusResponse{
		UploadID:      uploadID,
		BytesUploaded: 100,
		TotalBytes:    100,
		Percent:       100,
		Done:          true,
	}, nil
}

func parseResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "unexpected error result: %v", res)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected *TextContent, got %T", res.Content[0])
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &data), "failed to unmarshal JSON, text: %q", text.Text)
	return data
}

func TestLSHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "ls -l -a '/foo bar'" {
			return "total 0\n", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := lsHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, lsParams{
		SessionID: "test_id",
		Path:      "/foo bar",
		Long:      true,
		All:       true,
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "total 0\n", data["stdout"])
}

func TestCatHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "head -c 10485760 '/foo bar.txt'" {
			return "hello world", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := catHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, catParams{
		SessionID: "test_id",
		Path:      "/foo bar.txt",
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "hello world", data["stdout"])
}

func TestGrepHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "grep -r -i -m 10 'search pat' /foo" {
			return "found", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := grepHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, grepParams{
		SessionID:       "test_id",
		Pattern:         "search pat",
		Path:            "/foo",
		Recursive:       true,
		CaseInsensitive: true,
		MaxLines:        10,
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "found", data["stdout"])
}

func TestFindHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "find /foo -name \\*.txt -type f -maxdepth 2" {
			return "/foo/bar.txt", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := findHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, findParams{
		SessionID: "test_id",
		Path:      "/foo",
		Name:      "*.txt",
		Type:      "f",
		MaxDepth:  2,
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "/foo/bar.txt", data["stdout"])
}

func TestStatHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "stat /foo" {
			return "stat info", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := statHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, statParams{
		SessionID: "test_id",
		Path:      "/foo",
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "stat info", data["stdout"])
}

func TestDUHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		if cmd == "du -h -s -d 1 '/foo bar'" {
			return "4.0K\t/foo bar\n", 0
		}
		return "unexpected command: " + cmd, 1
	})
	defer cleanup()

	handler := duHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, duParams{
		SessionID: "test_id",
		Path:      "/foo bar",
		Human:     true,
		Summarize: true,
		MaxDepth:  1,
	})
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "4.0K\t/foo bar\n", data["stdout"])
}

func TestWriteFileHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	tmpFile := filepath.Join(t.TempDir(), "test_write.txt")
	handler := writeFileHandler(&dummyPool{client: client})

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, writeFileParams{
		SessionID: "test_id",
		Path:      tmpFile,
		Content:   "hello write",
		Mode:      "0644",
	})
	require.NoError(t, err)
	require.False(t, res.IsError)

	data := parseResult(t, res)
	require.Equal(t, true, data["ok"])

	content, err := os.ReadFile(tmpFile)
	require.NoError(t, err)
	require.Equal(t, "hello write", string(content))
}

func TestTruncateHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	tmpFile := filepath.Join(t.TempDir(), "test_truncate.txt")
	require.NoError(t, os.WriteFile(tmpFile, []byte("hello truncate"), 0o644))

	handler := truncateHandler(&dummyPool{client: client})
	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, truncateParams{
		SessionID: "test_id",
		Path:      tmpFile,
		Size:      5,
	})
	require.NoError(t, err)
	require.False(t, res.IsError)

	data := parseResult(t, res)
	require.Equal(t, true, data["ok"])

	content, err := os.ReadFile(tmpFile)
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
}

func TestUploadFileHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	tmpRoot := t.TempDir()
	handler := uploadFileHandler(&dummyPool{client: client}, tmpRoot)

	localPath := filepath.Join(tmpRoot, "local.txt")
	require.NoError(t, os.WriteFile(localPath, []byte("local content"), 0o644))

	remotePath := filepath.Join(t.TempDir(), "remote.txt")

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, uploadFileParams{
		SessionID:  "test_id",
		LocalPath:  localPath,
		RemotePath: remotePath,
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected error: %v", res)

	data := parseResult(t, res)
	require.Equal(t, true, data["ok"])
	require.Equal(t, "upload-123", data["upload_id"])

	content, err := os.ReadFile(remotePath)
	require.NoError(t, err)
	require.Equal(t, "local content", string(content))
}

func TestUploadFileHandler_Security(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	tmpRoot := t.TempDir()
	handler := uploadFileHandler(&dummyPool{client: client}, tmpRoot)

	// Create a file OUTSIDE the allowed root
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret content"), 0o644))

	remotePath := filepath.Join(t.TempDir(), "remote.txt")

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, uploadFileParams{
		SessionID:  "test_id",
		LocalPath:  outsideFile,
		RemotePath: remotePath,
	})
	require.Error(t, err, "expected error due to security bounds check")
	require.Nil(t, res)

	// relative path traversal
	res, _, err = handler(context.Background(), &mcp.CallToolRequest{}, uploadFileParams{
		SessionID:  "test_id",
		LocalPath:  filepath.Join(tmpRoot, "..", "outside.txt"),
		RemotePath: remotePath,
	})
	require.Error(t, err, "expected error due to relative path traversal")
	require.Nil(t, res)
}

// Retain TestWithinDir at the bottom
func TestWithinDir(t *testing.T) {
	root, err := filepath.Abs(filepath.Join(os.TempDir(), "gooners_test_root"))
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "normal inside",
			path:    filepath.Join(root, "foo.txt"),
			wantErr: false,
		},
		{
			name:    "nested inside",
			path:    filepath.Join(root, "dir", "foo.txt"),
			wantErr: false,
		},
		{
			name:    "directory traversal outside",
			path:    filepath.Join(root, "..", "foo.txt"),
			wantErr: true,
		},
		{
			name:    "directory traversal with tricky name",
			path:    filepath.Join(root, "dir", "..", "..", "foo.txt"),
			wantErr: true,
		},
		{
			name:    "absolute outside",
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "just root",
			path:    root,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withinDir(root, tt.path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(got, root), "withinDir() got = %v, must be within %v", got, root)
		})
	}
}

func TestUploadStatusHandler(t *testing.T) {
	handler := uploadStatusHandler(&dummyPool{})

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, uploadStatusParams{
		SessionID: "test_id",
		UploadID:  "upload-123",
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected error: %v", res)

	data := parseResult(t, res)
	require.Equal(t, true, data["ok"])
	require.Equal(t, "upload-123", data["upload_id"])
	require.Equal(t, float64(100), data["percent"])
	require.Equal(t, true, data["done"])
}
