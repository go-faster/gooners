package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

func TestSessionManager(t *testing.T) {
	tempDir := t.TempDir()

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
	handler := exportDashboardHandler(sm, nil)
	_, res, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-123",
		Save:        false,
		OutputPath:  outPath,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.OutputPath)
	assert.False(t, res.Saved)

	// Check fields in exported JSON
	var raw map[string]any
	data, err := os.ReadFile(res.OutputPath)
	require.NoError(t, err)
	err = json.Unmarshal(data, &raw)
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

func TestQueryRefID(t *testing.T) {
	assert.Equal(t, "A", queryRefID(0))
	assert.Equal(t, "Z", queryRefID(25))
	assert.Equal(t, "AA", queryRefID(26))
	assert.Equal(t, "AB", queryRefID(27))
	assert.Equal(t, "BA", queryRefID(52))
	assert.Equal(t, "ZZ", queryRefID(701))
	assert.Equal(t, "AAA", queryRefID(702))
}

func TestRowLayout(t *testing.T) {
	tempDir := t.TempDir()

	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "dash-layout",
		Title:       "Layout Test",
	}
	sm.Add(s)

	// Add row
	rowHandler := addRowHandler(sm)
	_, rowRes, err := rowHandler(context.Background(), nil, AddRowReq{
		DashboardID: "dash-layout",
		Title:       "Stats Row",
	})
	require.NoError(t, err)
	rowID := rowRes.RowID

	// Add 5 stat panels (default width 6, height 4)
	panelHandler := addPanelHandler(sm)

	var panels []AddPanelRes
	for i := range 5 {
		_, pRes, err := panelHandler(context.Background(), nil, AddPanelReq{
			DashboardID: "dash-layout",
			Title:       fmt.Sprintf("Stat %d", i),
			Type:        "stat",
			RowID:       rowID,
		})
		require.NoError(t, err)
		panels = append(panels, pRes)
	}

	// First 4 panels should be side-by-side on Y=1 (row is Y=0, height 1)
	// X should be: 0, 6, 12, 18
	assert.Equal(t, uint32(6), panels[0].GridPos.W)
	assert.Equal(t, uint32(4), panels[0].GridPos.H)
	assert.Equal(t, uint32(0), panels[0].GridPos.X)
	assert.Equal(t, uint32(1), panels[0].GridPos.Y)

	assert.Equal(t, uint32(6), panels[1].GridPos.W)
	assert.Equal(t, uint32(6), panels[1].GridPos.X)
	assert.Equal(t, uint32(1), panels[1].GridPos.Y)

	assert.Equal(t, uint32(12), panels[2].GridPos.X)
	assert.Equal(t, uint32(1), panels[2].GridPos.Y)

	assert.Equal(t, uint32(18), panels[3].GridPos.X)
	assert.Equal(t, uint32(1), panels[3].GridPos.Y)

	// 5th panel should wrap to Y = 1 + 4 = 5, X = 0
	assert.Equal(t, uint32(0), panels[4].GridPos.X)
	assert.Equal(t, uint32(5), panels[4].GridPos.Y)
}

func TestAddPanelsBatch(t *testing.T) {
	tempDir := t.TempDir()

	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "dash-batch",
		Title:       "Batch Test",
	}
	sm.Add(s)

	batchHandler := addPanelsBatchHandler(sm, nil)

	_, batchRes, err := batchHandler(context.Background(), nil, AddPanelsBatchReq{
		DashboardID: "dash-batch",
		Panels: []PanelSpec{
			{
				Title: "Memory usage",
				Type:  "stat",
				Unit:  "bytes",
				Queries: []QuerySpec{
					{
						DatasourceUID: "prom-ds",
						Expr:          "go_memstats_alloc_bytes",
						LegendFormat:  "{{class}}",
					},
				},
				Thresholds: []ThresholdSpec{
					{
						Value: nil,
						Color: "green",
					},
					{
						Value: func() *float64 { v := 1000.0; return &v }(),
						Color: "red",
					},
				},
				ReduceCalcs: []string{"mean"},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, batchRes.PanelIDs, 1)

	// Fetch state to verify
	state, err := sm.Get("dash-batch")
	require.NoError(t, err)
	require.Len(t, state.Panels, 1)

	p := state.Panels[0]
	assert.Equal(t, "Memory usage", p.Title)
	assert.Equal(t, "stat", p.Type)
	assert.Equal(t, "bytes", p.Unit)
	require.Len(t, p.Queries, 1)
	assert.Equal(t, "go_memstats_alloc_bytes", p.Queries[0].Expr)
	assert.Equal(t, "{{class}}", p.Queries[0].LegendFormat)
	require.Len(t, p.Thresholds, 2)
	assert.Nil(t, p.Thresholds[0].Value)
	assert.Equal(t, "green", p.Thresholds[0].Color)
	assert.NotNil(t, p.Thresholds[1].Value)
	assert.Equal(t, 1000.0, *p.Thresholds[1].Value)
	assert.Equal(t, "red", p.Thresholds[1].Color)
	assert.Equal(t, []string{"mean"}, p.ReduceCalcs)
}

func TestExtractMetricName(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		// Standard PromQL
		{`http_requests_total`, "http_requests_total"},
		{`go_memstats_alloc_bytes`, "go_memstats_alloc_bytes"},
		{`sum(rate(http_requests_total{job="api"}[5m]))`, "http_requests_total"},
		{`sum by(pod) (container_memory_working_set_bytes)`, "container_memory_working_set_bytes"},
		{`histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`, "http_request_duration_seconds_bucket"},
		// Metric names that start with known keywords (must not be stripped)
		{`without_cache_hits`, "without_cache_hits"},
		{`by_service_bytes`, "by_service_bytes"},
		{`on_call_total`, "on_call_total"},
		// VictoriaMetrics/MetricsQL extensions — functions not in standard PromQL
		{`histogram_share(0.9, rate(request_duration_seconds_bucket[5m]))`, "request_duration_seconds_bucket"},
		{`aggr_over_time("sum", node_memory_bytes[1h])`, "node_memory_bytes"},
		// Empty / invalid
		{`1 + 1`, ""},
		{`not_valid{{`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			assert.Equal(t, tt.want, extractMetricName(tt.expr))
		})
	}
}

func TestSuggestUnit(t *testing.T) {
	tests := []struct {
		metric   string
		promUnit string
		want     string
	}{
		{"go_memstats_alloc_bytes", "", "bytes"},
		{"process_cpu_seconds_total", "", "s"},
		{"cpu_usage_percent", "", "percent"},
		{"http_requests_total", "", "short"},
		{"foo_ratio", "", "percentunit"},
		// Prometheus metadata unit takes priority
		{"foo", "bytes", "bytes"},
		{"foo", "seconds", "s"},
		// Compound suffixes
		{"request_duration_seconds_total", "", "s"},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			assert.Equal(t, tt.want, suggestUnit(tt.metric, tt.promUnit, ""))
		})
	}
}

func TestPlacePanel(t *testing.T) {
	t.Run("stat defaults W=6 H=4", func(t *testing.T) {
		s := &DashboardSession{}
		pos := placePanel(s, nil, "stat", nil, nil, nil, nil)
		assert.Equal(t, uint32(6), pos.W)
		assert.Equal(t, uint32(4), pos.H)
		assert.Equal(t, uint32(0), pos.X)
		assert.Equal(t, uint32(0), pos.Y)
		assert.Equal(t, uint32(4), s.NextY, "NextY must advance by H")
	})

	t.Run("timeseries defaults W=24 H=8", func(t *testing.T) {
		s := &DashboardSession{}
		pos := placePanel(s, nil, "timeseries", nil, nil, nil, nil)
		assert.Equal(t, uint32(24), pos.W)
		assert.Equal(t, uint32(8), pos.H)
		assert.Equal(t, uint32(8), s.NextY)
	})

	t.Run("explicit H without Y still advances NextY", func(t *testing.T) {
		s := &DashboardSession{}
		h := 12
		pos := placePanel(s, nil, "timeseries", nil, &h, nil, nil)
		assert.Equal(t, uint32(12), pos.H)
		assert.Equal(t, uint32(0), pos.Y)
		assert.Equal(t, uint32(12), s.NextY, "NextY must advance even when only H is explicit")
	})

	t.Run("explicit Y does not regress NextY", func(t *testing.T) {
		s := &DashboardSession{NextY: 10}
		y := 2
		pos := placePanel(s, nil, "timeseries", nil, nil, nil, &y)
		assert.Equal(t, uint32(2), pos.Y)
		assert.Equal(t, uint32(10), s.NextY, "placing behind existing cursor must not regress NextY")
	})

	t.Run("row: panels flow side-by-side", func(t *testing.T) {
		s := &DashboardSession{NextY: 1}
		r := &RowEntry{NextY: 1}
		p1 := placePanel(s, r, "stat", nil, nil, nil, nil)
		p2 := placePanel(s, r, "stat", nil, nil, nil, nil)
		assert.Equal(t, uint32(0), p1.X)
		assert.Equal(t, uint32(6), p2.X)
		assert.Equal(t, p1.Y, p2.Y, "both panels in same row should share Y")
	})

	t.Run("row: wraps when NextX+W exceeds 24", func(t *testing.T) {
		s := &DashboardSession{NextY: 1}
		r := &RowEntry{NextY: 1}
		// Fill 4 stat panels (4×6=24)
		for range 4 {
			placePanel(s, r, "stat", nil, nil, nil, nil)
		}
		p5 := placePanel(s, r, "stat", nil, nil, nil, nil)
		assert.Equal(t, uint32(0), p5.X, "5th panel must wrap to X=0")
		assert.Equal(t, uint32(5), p5.Y, "5th panel Y = row base (1) + lineHeight (4)")
	})

	t.Run("row: LineHeight tracks tallest panel in row", func(t *testing.T) {
		s := &DashboardSession{NextY: 1}
		r := &RowEntry{NextY: 1}
		bigH := 12
		placePanel(s, r, "stat", nil, &bigH, nil, nil) // W=6, H=12
		placePanel(s, r, "stat", nil, nil, nil, nil)   // W=6, H=4  (default)
		// Fill rest of row: 3 more stat panels to trigger wrap (6+6+6*3=30 > 24)
		for range 2 {
			placePanel(s, r, "stat", nil, nil, nil, nil)
		}
		p := placePanel(s, r, "stat", nil, nil, nil, nil) // triggers wrap
		assert.Equal(t, uint32(0), p.X)
		assert.Equal(t, uint32(13), p.Y, "wrap Y = row base (1) + max LineHeight (12)")
	})
}

func TestGrafanaMCPNewFixes(t *testing.T) {
	// 1. Test timeseries & table ReduceCalcs in buildPanel
	decimals := 1.0
	tsPanel := &PanelEntry{
		ID:          "ts-1",
		Title:       "TimeSeries Panel",
		Type:        "timeseries",
		Decimals:    &decimals,
		ReduceCalcs: []string{"lastNotNull", "mean"},
	}
	tsBuilder := buildPanel(tsPanel)
	require.NotNil(t, tsBuilder)
	tsBuilt, err := tsBuilder.Build()
	require.NoError(t, err)
	assert.Equal(t, "TimeSeries Panel", *tsBuilt.Title)
	// We can't directly inspect internal VizLegendOptions, but we know it built without errors

	tablePanel := &PanelEntry{
		ID:          "tbl-1",
		Title:       "Table Panel",
		Type:        "table",
		ReduceCalcs: []string{"max"},
	}
	tblBuilder := buildPanel(tablePanel)
	require.NotNil(t, tblBuilder)
	tblBuilt, err := tblBuilder.Build()
	require.NoError(t, err)
	assert.Equal(t, "Table Panel", *tblBuilt.Title)

	// 3. Test concurrent/race condition fix
	tempDir := t.TempDir()
	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "race-dash",
		Title:       "Race Test",
	}
	sm.Add(s)

	// Launch concurrent panel additions; collect errors via channel to avoid
	// calling require inside a goroutine (t.FailNow exits the goroutine, not
	// the test).
	errs := make(chan error, 5)
	for i := range 5 {
		go func(id int) {
			h := addPanelHandler(sm)
			_, _, err := h(context.Background(), nil, AddPanelReq{
				DashboardID: "race-dash",
				Title:       fmt.Sprintf("Panel %d", id),
				Type:        "stat",
			})
			errs <- err
		}(i)
	}
	for range 5 {
		require.NoError(t, <-errs)
	}

	finalSession, err := sm.Get("race-dash")
	require.NoError(t, err)
	// Should have exactly 5 panels, and no race/corruption occurred
	assert.Len(t, finalSession.Panels, 5)
}

func TestParseDashboardRoundtrip(t *testing.T) {
	tempDir := t.TempDir()
	sm := NewSessionManager(tempDir)
	s := &DashboardSession{
		DashboardID: "dash-roundtrip",
		Title:       "Roundtrip Test",
		UID:         "uid-roundtrip",
		TimeFrom:    "now-24h",
		TimeTo:      "now",
		Tags:        []string{"tag1", "tag2"},
	}
	s.Variables = []VariableSpec{
		{Name: "var1", Type: "custom", Query: "a,b,c"},
	}

	// Flat row
	r1 := &RowEntry{
		ID:        "row-1",
		Title:     "Flat Row",
		Collapsed: false,
		Y:         0,
		NextY:     5,
		Panels: []*PanelEntry{
			{
				ID:          "p1",
				Title:       "Panel 1",
				Type:        "stat",
				GridPos:     dashboard.GridPos{X: 0, Y: 1, W: 12, H: 4},
				Unit:        "bytes",
				Decimals:    func() *float64 { f := 2.0; return &f }(),
				ReduceCalcs: []string{"lastNotNull", "mean"},
				Thresholds: []dashboard.Threshold{
					{Value: nil, Color: "green"},
					{Value: func() *float64 { f := 80.0; return &f }(), Color: "red"},
				},
				Queries: []QueryEntry{
					{RefID: "A", Expr: "up", DatasourceUID: "ds1", DatasourceType: "prometheus"},
				},
			},
		},
	}

	// Collapsed row
	r2 := &RowEntry{
		ID:        "row-2",
		Title:     "Collapsed Row",
		Collapsed: true,
		Y:         5,
		NextY:     6,
		Panels: []*PanelEntry{
			{
				ID:      "p2",
				Title:   "Panel 2",
				Type:    "timeseries",
				GridPos: dashboard.GridPos{X: 0, Y: 6, W: 24, H: 8},
				Queries: []QueryEntry{
					{RefID: "B", Expr: "rate(http_requests[5m])"},
				},
			},
		},
	}
	s.Rows = []*RowEntry{r1, r2}
	sm.Add(s)

	outPath := filepath.Join(tempDir, "out-roundtrip.json")
	handler := exportDashboardHandler(sm, nil)
	_, res, err := handler(context.Background(), nil, ExportDashboardReq{
		DashboardID: "dash-roundtrip",
		Save:        false,
		OutputPath:  outPath,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(res.OutputPath)
	require.NoError(t, err)

	imported, err := parseDashboardToSession(data, "dash-roundtrip")
	require.NoError(t, err)

	assert.Equal(t, "Roundtrip Test", imported.Title)
	assert.Equal(t, "uid-roundtrip", imported.UID)
	assert.Equal(t, "now-24h", imported.TimeFrom)
	assert.Equal(t, "now", imported.TimeTo)
	assert.Equal(t, []string{"tag1", "tag2"}, imported.Tags)

	require.Len(t, imported.Variables, 1)
	assert.Equal(t, "var1", imported.Variables[0].Name)

	require.Len(t, imported.Rows, 2)
	assert.Equal(t, "Flat Row", imported.Rows[0].Title)
	assert.False(t, imported.Rows[0].Collapsed)
	require.Len(t, imported.Rows[0].Panels, 1)

	p1 := imported.Rows[0].Panels[0]
	assert.Equal(t, "Panel 1", p1.Title)
	assert.Equal(t, "stat", p1.Type)
	assert.Equal(t, "bytes", p1.Unit)
	assert.NotNil(t, p1.Decimals)
	assert.Equal(t, 2.0, *p1.Decimals)
	assert.Equal(t, []string{"lastNotNull", "mean"}, p1.ReduceCalcs)
	require.Len(t, p1.Thresholds, 2)
	assert.Nil(t, p1.Thresholds[0].Value)
	assert.Equal(t, "green", p1.Thresholds[0].Color)
	assert.NotNil(t, p1.Thresholds[1].Value)
	assert.Equal(t, 80.0, *p1.Thresholds[1].Value)
	assert.Equal(t, "red", p1.Thresholds[1].Color)
	require.Len(t, p1.Queries, 1)
	assert.Equal(t, "up", p1.Queries[0].Expr)

	assert.Equal(t, "Collapsed Row", imported.Rows[1].Title)
	assert.True(t, imported.Rows[1].Collapsed)
	require.Len(t, imported.Rows[1].Panels, 1)

	p2 := imported.Rows[1].Panels[0]
	assert.Equal(t, "Panel 2", p2.Title)
	assert.Equal(t, "timeseries", p2.Type)
	require.Len(t, p2.Queries, 1)
	assert.Equal(t, "rate(http_requests[5m])", p2.Queries[0].Expr)
}

func TestImportDashboardHandler(t *testing.T) {
	tempDir := t.TempDir()
	sm := NewSessionManager(tempDir)

	dashboardJSON := `{
		"title": "File Import Test",
		"uid": "file-123",
		"panels": []
	}`

	filePath := filepath.Join(tempDir, "dash.json")
	require.NoError(t, os.WriteFile(filePath, []byte(dashboardJSON), 0o600))

	handler := importDashboardHandler(sm, nil)

	// Test file path import
	_, res, err := handler(context.Background(), nil, ImportDashboardReq{
		FilePath: filePath,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.DashboardID)
	assert.Equal(t, "File Import Test", res.Title)

	// Verify it was saved to the session manager
	sess, err := sm.Get(res.DashboardID)
	require.NoError(t, err)
	assert.Equal(t, "File Import Test", sess.Title)
	assert.Equal(t, "file-123", sess.UID)

	// Test failure on missing file
	_, _, err = handler(context.Background(), nil, ImportDashboardReq{
		FilePath: filepath.Join(tempDir, "does-not-exist.json"),
	})
	require.ErrorContains(t, err, "reading dashboard file")

	// Test failure when neither provided
	_, _, err = handler(context.Background(), nil, ImportDashboardReq{})
	require.ErrorContains(t, err, "either uid or file_path must be provided")
}
