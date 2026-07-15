package grafana

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-faster/sdk/gold"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func TestBuildPanel(t *testing.T) {
	decimals := 2.0
	val := 80.0
	p := &PanelEntry{
		ID:    "panel-1",
		Title: "CPU Usage",
		Type:  "timeseries",
		GridPos: dashboard.GridPos{
			W: 12,
			H: 8,
			X: 0,
			Y: 0,
		},
		Unit:     "percent",
		Decimals: &decimals,
		Queries: []QueryEntry{
			{
				RefID:         "A",
				DatasourceUID: "ds-prom",
				Expr:          "cpu_usage_percent",
				LegendFormat:  "{{cpu}}",
			},
		},
		Thresholds: []dashboard.Threshold{
			{
				Value: nil,
				Color: "green",
			},
			{
				Value: &val,
				Color: "red",
			},
		},
	}

	pb := buildPanel(p)
	require.NotNil(t, pb)

	builtPanel, err := pb.Build()
	require.NoError(t, err)

	assert.Equal(t, "CPU Usage", *builtPanel.Title)
	assert.Equal(t, uint32(12), builtPanel.GridPos.W)
	assert.Equal(t, uint32(8), builtPanel.GridPos.H)

	// Verify target exists
	assert.Len(t, builtPanel.Targets, 1)
}

func TestExportDashboard(t *testing.T) {
	tempDir := t.TempDir()

	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "dash-123",
		Title:       "Production Service Health",
		UID:         "prod-service-health",
		Tags:        []string{"production", "service"},
		TimeFrom:    "now-1h",
		TimeTo:      "now",
		Variables: []VariableSpec{
			{
				Name:          "env",
				Type:          "query",
				Query:         "label_values(up, job)",
				DatasourceUID: "ds-prom",
			},
		},
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

	outPath := filepath.Join(tempDir, "out.json")
	handler := exportDashboardHandler(sm, nil, effect.Root(tempDir))
	_, res, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-123",
		Save:        false,
		OutputPath:  outPath,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.OutputPath)
	assert.False(t, res.Saved)

	data, err := os.ReadFile(res.OutputPath)
	require.NoError(t, err)

	// Normalize to pretty-printed JSON for stable comparison.
	var raw any
	require.NoError(t, json.Unmarshal(data, &raw))
	pretty, err := json.MarshalIndent(raw, "", "  ")
	require.NoError(t, err)
	gold.Str(t, string(pretty)+"\n", "export_dashboard.json")

	// roundtrip via parser
	imported, err := parseDashboardToSession(data, "imp-1")
	require.NoError(t, err)
	assert.Equal(t, "Production Service Health", imported.Title)
	assert.Equal(t, "prod-service-health", imported.UID)
	assert.Len(t, imported.Tags, 2)
	assert.Len(t, imported.Variables, 1)
	assert.Equal(t, "env", imported.Variables[0].Name)
	assert.Equal(t, "query", imported.Variables[0].Type)
	assert.Equal(t, "label_values(up, job)", imported.Variables[0].Query)
	assert.Len(t, imported.Panels, 1)
	assert.Equal(t, "HTTP Requests Rate", imported.Panels[0].Title)
	assert.Equal(t, "timeseries", imported.Panels[0].Type)
	assert.Equal(t, uint32(24), imported.Panels[0].GridPos.W)
}
