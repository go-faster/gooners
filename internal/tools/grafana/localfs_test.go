package grafana

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func newExportSession(t *testing.T) *SessionManager {
	t.Helper()
	sm := NewSessionManager(t.TempDir())
	sm.Add(&DashboardSession{
		DashboardID: "dash-1",
		Title:       "Test",
		Version:     dashboardVersionV1,
		CreatedAt:   time.Now(),
		TouchedAt:   time.Now(),
	})
	return sm
}

// TestExportDashboardConfinesOutputPath: output_path is agent-controlled, so
// an unconfined os.WriteFile there is an arbitrary host write — the same bug
// class as the old ssh_save_output. The provider is what prevents it; the
// handler has no path check to forget.
func TestExportDashboardConfinesOutputPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "authorized_keys")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))

	handler := exportDashboardHandler(newExportSession(t), nil, effect.Root(root))

	_, _, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-1",
		OutputPath:  target,
	})
	require.ErrorIs(t, err, effect.ErrOutsideRoot)

	_, _, err = handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-1",
		OutputPath:  filepath.Join("..", "..", "escape.json"),
	})
	require.ErrorIs(t, err, effect.ErrOutsideRoot)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "original", string(data), "export escaped its root")

	// A path inside the root still works.
	_, res, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-1",
		OutputPath:  "out.json",
	})
	require.NoError(t, err)
	require.Equal(t, "out.json", res.OutputPath)
	require.FileExists(t, filepath.Join(root, "out.json"))
}

// TestImportDashboardConfinesFilePath: file_path is agent-controlled too, so
// without a provider import_dashboard reads any file the process can (an SSH
// key, /etc/passwd) and hands it back through the dashboard.
func TestImportDashboardConfinesFilePath(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600))

	handler := importDashboardHandler(NewSessionManager(t.TempDir()), nil, effect.Root(root))

	_, _, err := handler(context.Background(), nil, ImportDashboardReq{FilePath: secret})
	require.ErrorIs(t, err, effect.ErrOutsideRoot)

	// A dashboard inside the root imports normally.
	require.NoError(t, os.WriteFile(filepath.Join(root, "dash.json"), []byte(`{"title":"OK","panels":[]}`), 0o600))
	_, res, err := handler(context.Background(), nil, ImportDashboardReq{FilePath: "dash.json"})
	require.NoError(t, err)
	require.Equal(t, "OK", res.Title)
}

// TestDashboardFileToolsDeniedWithoutLocalFS: RegisterOptions defaults to no
// local file access, so a server that never configures it refuses both rather
// than reaching the host filesystem.
func TestDashboardFileToolsDeniedWithoutLocalFS(t *testing.T) {
	var opts RegisterOptions
	opts.setDefaults()

	_, _, err := exportDashboardHandler(newExportSession(t), nil, opts.LocalFS)(
		context.Background(), nil, ExportDashboardReq{DashboardID: "dash-1", OutputPath: "out.json"},
	)
	require.ErrorIs(t, err, effect.ErrDenied)

	_, _, err = importDashboardHandler(NewSessionManager(t.TempDir()), nil, opts.LocalFS)(
		context.Background(), nil, ImportDashboardReq{FilePath: "dash.json"},
	)
	require.ErrorIs(t, err, effect.ErrDenied)
}
