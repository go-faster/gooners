package grafana

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

func TestSessionManager(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "grafana-mcp-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	sm := NewSessionManager(tempDir)

	s := &DashboardSession{
		DashboardID: "test-dash-id",
		Title:       "Test Dashboard",
	}

	sm.Add(s)

	retrieved, err := sm.Get("test-dash-id")
	require.NoError(t, err)
	assert.Equal(t, "Test Dashboard", retrieved.Title)

	// Test persistence by reloading
	sm2 := NewSessionManager(tempDir)
	retrieved2, err := sm2.Get("test-dash-id")
	require.NoError(t, err)
	assert.Equal(t, "Test Dashboard", retrieved2.Title)

	list := sm2.List()
	require.Len(t, list, 1)
	assert.Equal(t, "test-dash-id", list[0].DashboardID)

	sm2.Delete("test-dash-id")
	_, err = sm2.Get("test-dash-id")
	assert.Error(t, err)
}

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
	tempDir, err := os.MkdirTemp("", "grafana-mcp-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

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

	handler := exportDashboardHandler(sm, nil)
	_, res, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-123",
		Save:        false,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.DashboardJSON)
	assert.False(t, res.Saved)

	// Check fields in exported JSON
	var raw map[string]any
	err = json.Unmarshal([]byte(res.DashboardJSON), &raw)
	require.NoError(t, err)

	assert.Equal(t, "Production Service Health", raw["title"])
	assert.Equal(t, "prod-service-health", raw["uid"])
	assert.Equal(t, []any{"production", "service"}, raw["tags"])

	templating := raw["templating"].(map[string]any)
	list := templating["list"].([]any)
	assert.Len(t, list, 1)
	v := list[0].(map[string]any)
	assert.Equal(t, "env", v["name"])
	assert.Equal(t, "query", v["type"])

	panels := raw["panels"].([]any)
	assert.Len(t, panels, 1)
	p := panels[0].(map[string]any)
	assert.Equal(t, "HTTP Requests Rate", p["title"])
	assert.Equal(t, "timeseries", p["type"])
}

func TestQueryRefID(t *testing.T) {
	assert.Equal(t, "A", queryRefID(0))
	assert.Equal(t, "Z", queryRefID(25))
	assert.Equal(t, "AA", queryRefID(26))
	assert.Equal(t, "AB", queryRefID(27))
	assert.Equal(t, "BA", queryRefID(52))
	assert.Equal(t, "ZZ", queryRefID(701))
	assert.Equal(t, "AAA", queryRefID(702))
}
