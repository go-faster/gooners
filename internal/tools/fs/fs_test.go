package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

type dummyPool struct {
	client *ssh.Client
}

func (p *dummyPool) Get(id string) (*ssh.Client, error) {
	return p.client, nil
}

func parseResult(t *testing.T, res *mcp.CallToolResult) map[string]interface{} {
	t.Helper()
	require.False(t, res.IsError, "unexpected error result: %v", res)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var data map[string]interface{}
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
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "test_id",
		"path":       "/foo bar",
		"long":       true,
		"all":        true,
	}

	res, err := handler(context.Background(), req)
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
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "test_id",
		"path":       "/foo bar.txt",
	}

	res, err := handler(context.Background(), req)
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
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id":       "test_id",
		"pattern":          "search pat",
		"path":             "/foo",
		"recursive":        true,
		"case_insensitive": true,
		"max_lines":        10.0,
	}

	res, err := handler(context.Background(), req)
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
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "test_id",
		"path":       "/foo",
		"name":       "*.txt",
		"type":       "f",
		"max_depth":  2.0,
	}

	res, err := handler(context.Background(), req)
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
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "test_id",
		"path":       "/foo",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)

	data := parseResult(t, res)
	require.Equal(t, "stat info", data["stdout"])
}

func TestWriteFileHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	handler := writeFileHandler(&dummyPool{client: client})
	req := mcp.CallToolRequest{}

	// Create a temporary file to use as the "remote" path.
	// Our mock sftp server uses the real local filesystem!
	tmpRemote := filepath.Join(t.TempDir(), "remote.txt")

	req.Params.Arguments = map[string]interface{}{
		"session_id": "test_id",
		"path":       tmpRemote,
		"content":    "hello sftp",
		"mode":       "600",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected error: %v", res)

	content, err := os.ReadFile(tmpRemote)
	require.NoError(t, err)
	require.Equal(t, "hello sftp", string(content))

	info, err := os.Stat(tmpRemote)
	require.NoError(t, err)

	// Check mode (might need masking depending on umask, but checking if owner read/write is enough usually)
	require.EqualValues(t, 0600, info.Mode().Perm()&0600, "unexpected mode: %v", info.Mode())
}

func TestUploadFileHandler(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(cmd string) (string, int) {
		return "", 0
	})
	defer cleanup()

	tmpRoot := t.TempDir()
	handler := uploadFileHandler(&dummyPool{client: client}, tmpRoot)

	localPath := filepath.Join(tmpRoot, "local.txt")
	require.NoError(t, os.WriteFile(localPath, []byte("local content"), 0644))

	remotePath := filepath.Join(t.TempDir(), "remote.txt")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id":  "test_id",
		"local_path":  localPath,
		"remote_path": remotePath,
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected error: %v", res)

	data := parseResult(t, res)
	require.Equal(t, true, data["ok"])
	require.Equal(t, float64(13), data["bytes_uploaded"])
	require.Equal(t, float64(13), data["total_bytes"])
	require.Equal(t, float64(100), data["percent"])

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
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret content"), 0644))

	remotePath := filepath.Join(t.TempDir(), "remote.txt")

	req := mcp.CallToolRequest{}
	args := map[string]interface{}{
		"session_id":  "test_id",
		"local_path":  outsideFile, // Should fail!
		"remote_path": remotePath,
	}
	req.Params.Arguments = args

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error due to security bounds check")

	// Also test relative path traversal
	args["local_path"] = filepath.Join(tmpRoot, "..", "outside.txt")
	res, err = handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error due to relative path traversal")
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
