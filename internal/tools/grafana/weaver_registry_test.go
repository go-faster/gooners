package grafana

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverTelemetryRegistryFS_ParsesWeaverHTTPMetricsSample(t *testing.T) {
	// Copied from github.com/open-telemetry/weaver:
	// crates/weaver_codegen_test/semconv_registry/metrics/http.yaml.
	metrics, err := discoverTelemetryRegistryFS(os.DirFS("_testdata"))
	require.NoError(t, err)

	assert.Equal(t, []MetricInfo{
		{
			Name:       "http.server.request.duration",
			Instrument: "histogram",
			Unit:       "s",
			Brief:      "Duration of HTTP server requests.",
		},
		{
			Name:       "http.client.request.duration",
			Instrument: "histogram",
			Unit:       "s",
			Brief:      "Duration of HTTP client requests.",
		},
		{
			Name:       "http.client.active_requests",
			Instrument: "updowncounter",
			Unit:       "{request}",
			Brief:      "Number of active HTTP requests.",
			Attributes: []string{"http.request.method", "server.address", "server.port", "url.scheme"},
		},
	}, metrics)
}

func TestDiscoverTelemetryRegistryFS(t *testing.T) {
	metrics, err := discoverTelemetryRegistryFS(fstest.MapFS{
		"metrics.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: metric.http.server.duration
    type: metric
    metric_name: http.server.duration
    instrument: histogram
    unit: s
    brief: Duration of HTTP server requests.
    attributes:
      - ref: http.request.method
      - id: http.response.status_code
        type: string
  - id: non.metric.group
    type: attribute_group
    attributes:
      - id: foo
`)},
	})
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	metric := metrics[0]
	assert.Equal(t, "http.server.duration", metric.Name)
	assert.Equal(t, "histogram", metric.Instrument)
	assert.Equal(t, "s", metric.Unit)
	assert.Equal(t, "Duration of HTTP server requests.", metric.Brief)
	assert.ElementsMatch(t, []string{"http.request.method", "http.response.status_code"}, metric.Attributes)
}

func TestDiscoverTelemetryRegistryFS_EmptyResultIsNonNil(t *testing.T) {
	metrics, err := discoverTelemetryRegistryFS(fstest.MapFS{})
	require.NoError(t, err)
	assert.NotNil(t, metrics)
	assert.Empty(t, metrics)
}

func TestDiscoverTelemetryRegistryFS_WalksNestedFilesAndSkipsIgnoredDirs(t *testing.T) {
	metrics, err := discoverTelemetryRegistryFS(fstest.MapFS{
		"registry/http.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: http.server.duration
    type: metric
`)},
		"registry/runtime/runtime.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: process.runtime.go.goroutines
    type: metric
`)},
		"registry/vendor/ignored.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: vendor.metric
    type: metric
`)},
		"registry/node_modules/ignored.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: node.modules.metric
    type: metric
`)},
		"registry/.cache/ignored.yaml": &fstest.MapFile{Data: []byte(`
groups:
  - id: hidden.metric
    type: metric
`)},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []MetricInfo{
		{Name: "http.server.duration"},
		{Name: "process.runtime.go.goroutines"},
	}, metrics)
}

func TestDiscoverTelemetryRegistry_RootNamedVendorIsScanned(t *testing.T) {
	parentDir := t.TempDir()
	rootDir := filepath.Join(parentDir, "vendor")
	require.NoError(t, os.Mkdir(rootDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "metrics.yaml"), []byte(`
groups:
  - id: root.vendor.metric
    type: metric
`), 0o644))

	metrics, err := discoverTelemetryRegistry(parentDir, rootDir)
	require.NoError(t, err)
	assert.Equal(t, []MetricInfo{{Name: "root.vendor.metric"}}, metrics)
}

func TestDiscoverTelemetryRegistry_NoPathUsesWorkingDirectory(t *testing.T) {
	tempDir := t.TempDir()

	metrics, err := discoverTelemetryRegistry(tempDir, "")
	require.NoError(t, err)
	assert.NotNil(t, metrics)
	assert.Empty(t, metrics)
}

func TestDiscoverTelemetryRegistry_RejectsPathOutsideWorkingDirectory(t *testing.T) {
	parentDir := t.TempDir()
	tempDir := filepath.Join(parentDir, "app")
	outsideDir := filepath.Join(parentDir, "app2")
	require.NoError(t, os.Mkdir(tempDir, 0o755))
	require.NoError(t, os.Mkdir(outsideDir, 0o755))

	_, err := discoverTelemetryRegistry(tempDir, outsideDir)
	require.Error(t, err)
	assert.ErrorContains(t, err, "path must be inside the working directory")
}

func TestDiscoverTelemetryRegistry_ReturnsMissingPathError(t *testing.T) {
	tempDir := t.TempDir()

	_, err := discoverTelemetryRegistry(tempDir, "missing")
	require.Error(t, err)
	assert.ErrorContains(t, err, "error walking path")
}

func TestDiscoverTelemetryRegistry_SkipsSymlinkedYAML(t *testing.T) {
	tempDir := t.TempDir()
	outsideDir := t.TempDir()

	outsideFile := filepath.Join(outsideDir, "metrics.yaml")
	require.NoError(t, os.WriteFile(outsideFile, []byte(`
groups:
  - id: outside.metric
    type: metric
`), 0o644))
	err := os.Symlink(outsideFile, filepath.Join(tempDir, "metrics.yaml"))
	if err != nil {
		t.Skipf("symlink is not supported: %v", err)
	}

	metrics, err := discoverTelemetryRegistry(tempDir, tempDir)
	require.NoError(t, err)
	assert.Empty(t, metrics)
}

func TestDiscoverTelemetryRegistryFS_SkipsSymlinkedYAML(t *testing.T) {
	metrics, err := discoverTelemetryRegistryFS(fstest.MapFS{
		"metrics.yaml": &fstest.MapFile{
			Mode: fs.ModeSymlink,
			Data: []byte(`
groups:
  - id: symlink.metric
    type: metric
`),
		},
	})
	require.NoError(t, err)
	assert.Empty(t, metrics)
}
