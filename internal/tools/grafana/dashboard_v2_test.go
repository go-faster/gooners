package grafana

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportDashboardV2(t *testing.T) {
	tempDir := t.TempDir()

	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "dash-v2",
		Title:       "Production Service Health",
		Version:     dashboardVersionV2,
		UID:         "prod-service-health",
		Tags:        []string{"production", "service"},
		TimeFrom:    "now-1h",
		TimeTo:      "now",
		Panels: []*PanelEntry{
			{
				ID:    "panel-ts",
				Title: "HTTP Requests Rate",
				Type:  "timeseries",
				GridPos: dashboard.GridPos{
					W: 24,
					H: 8,
					X: 0,
					Y: 0,
				},
				Queries: []QueryEntry{
					{
						RefID:         "A",
						DatasourceUID: "ds-prom",
						Expr:          `sum(rate(http_requests_total{job="api"}[5m]))`,
					},
				},
			},
		},
	}
	sm.Add(s)

	outPath := filepath.Join(tempDir, "out-v2.json")
	_, res, err := exportDashboardHandler(sm, nil)(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-v2",
		OutputPath:  outPath,
	})
	require.NoError(t, err)
	assert.Equal(t, dashboardVersionV2, res.Version)
	assert.Equal(t, outPath, res.OutputPath)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, "dashboard.grafana.app/v2", raw["apiVersion"])
	assert.Equal(t, "Dashboard", raw["kind"])

	spec, ok := raw["spec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Production Service Health", spec["title"])

	elements, ok := spec["elements"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, elements, "panel-panel-ts")
}
