package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-faster/sdk/gold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

func TestMain(m *testing.M) {
	gold.Init()
	os.Exit(m.Run())
}

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

	t.Run("no-row: stat+timeseries placed side-by-side", func(t *testing.T) {
		// Simulates the common pattern: stat (w=6) + timeseries (w=18) on the same line.
		s := &DashboardSession{}
		w6, w18 := 6, 18
		stat := placePanel(s, nil, "stat", &w6, nil, nil, nil)
		ts := placePanel(s, nil, "timeseries", &w18, nil, nil, nil)
		assert.Equal(t, uint32(0), stat.X)
		assert.Equal(t, uint32(6), ts.X, "timeseries must start right after stat")
		assert.Equal(t, stat.Y, ts.Y, "stat and timeseries must share the same Y")
	})

	t.Run("no-row: wraps when NextX+W exceeds 24", func(t *testing.T) {
		s := &DashboardSession{}
		w6, w18 := 6, 18
		placePanel(s, nil, "stat", &w6, nil, nil, nil)
		placePanel(s, nil, "timeseries", &w18, nil, nil, nil)
		// Second pair should wrap to a new line.
		stat2 := placePanel(s, nil, "stat", &w6, nil, nil, nil)
		assert.Equal(t, uint32(0), stat2.X, "second stat must wrap to X=0")
		assert.Equal(t, uint32(8), stat2.Y, "second stat Y = first line height (8)")
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

	// Golden file covers the full exported JSON structure.
	var raw any
	require.NoError(t, json.Unmarshal(data, &raw))
	pretty, err := json.MarshalIndent(raw, "", "  ")
	require.NoError(t, err)
	gold.Str(t, string(pretty)+"\n", "roundtrip_dashboard.json")

	// Verify the parser correctly reconstructs the session from the JSON.
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
	ir0 := imported.Rows[0]
	assert.Equal(t, "Flat Row", ir0.Title)
	assert.False(t, ir0.Collapsed)
	require.Len(t, ir0.Panels, 1)

	ip1 := ir0.Panels[0]
	assert.Equal(t, "Panel 1", ip1.Title)
	assert.Equal(t, "stat", ip1.Type)
	assert.Equal(t, "bytes", ip1.Unit)
	require.NotNil(t, ip1.Decimals)
	assert.Equal(t, 2.0, *ip1.Decimals)
	assert.Equal(t, []string{"lastNotNull", "mean"}, ip1.ReduceCalcs)
	require.Len(t, ip1.Thresholds, 2)
	assert.Nil(t, ip1.Thresholds[0].Value)
	assert.Equal(t, "green", ip1.Thresholds[0].Color)
	require.NotNil(t, ip1.Thresholds[1].Value)
	assert.Equal(t, 80.0, *ip1.Thresholds[1].Value)
	assert.Equal(t, "red", ip1.Thresholds[1].Color)
	require.Len(t, ip1.Queries, 1)
	assert.Equal(t, "up", ip1.Queries[0].Expr)

	ir1 := imported.Rows[1]
	assert.Equal(t, "Collapsed Row", ir1.Title)
	assert.True(t, ir1.Collapsed)
	require.Len(t, ir1.Panels, 1)

	ip2 := ir1.Panels[0]
	assert.Equal(t, "Panel 2", ip2.Title)
	assert.Equal(t, "timeseries", ip2.Type)
	require.Len(t, ip2.Queries, 1)
	assert.Equal(t, "rate(http_requests[5m])", ip2.Queries[0].Expr)
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

type grafanaTestRoute struct {
	Status   int
	Response string
	Check    func(*http.Request)
}

func newGrafanaTestServer(t *testing.T, routes map[string]grafanaTestRoute) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.URL.Path]
		if !ok {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if route.Check != nil {
			route.Check(r)
		}
		if route.Status != 0 {
			w.WriteHeader(route.Status)
		}
		_, _ = w.Write([]byte(route.Response))
	}))
}

func TestGrafanaClient(t *testing.T) {
	ctx := context.Background()
	var seenAuth string
	recordAuth := func(r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
	}
	server := newGrafanaTestServer(t, map[string]grafanaTestRoute{
		"/api/datasources/uid/prom": {
			Response: `{"uid":"prom","type":"prometheus","name":"Prometheus"}`,
			Check:    recordAuth,
		},
		"/api/datasources/name/Prometheus": {
			Response: `{"uid":"prom","type":"prometheus","name":"Prometheus"}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/label/__name__/values": {
			Response: `{"status":"success","data":["up","process_cpu_seconds_total"]}`,
			Check: func(r *http.Request) {
				assert.Equal(t, `up`, r.URL.Query().Get("match[]"))
			},
		},
		"/api/datasources/proxy/uid/prom/api/v1/labels": {
			Response: `{"status":"success","data":["job","instance"]}`,
			Check: func(r *http.Request) {
				assert.Equal(t, `{__name__="up"}`, r.URL.Query().Get("match[]"))
			},
		},
		"/api/datasources/proxy/uid/prom/api/v1/label/job/values": {
			Response: `{"status":"success","data":["api","worker"]}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/metadata": {
			Response: `{"status":"success","data":{"up":[{"type":"gauge","help":"Up","unit":""}]}}`,
			Check: func(r *http.Request) {
				assert.Equal(t, "up", r.URL.Query().Get("metric"))
			},
		},
		"/api/datasources/proxy/uid/prom/api/v1/query": {
			Response: `{"status":"success","data":{"result":[]}}`,
			Check: func(r *http.Request) {
				assert.Equal(t, "up", r.URL.Query().Get("query"))
			},
		},
		"/api/datasources/proxy/uid/prom/api/v1/query_range": {
			Response: `{"status":"success","data":{"result":[]}}`,
			Check: func(r *http.Request) {
				assert.Equal(t, "up", r.URL.Query().Get("query"))
			},
		},
		"/api/datasources/proxy/uid/loki/loki/api/v1/query": {
			Response: `{"status":"success","data":{"result":[]}}`,
		},
		"/api/datasources/proxy/uid/loki/loki/api/v1/query_range": {
			Response: `{"status":"success","data":{"result":[]}}`,
		},
		"/api/dashboards/db": {
			Response: `{"id":1,"uid":"saved","url":"/d/saved","status":"success","version":2}`,
			Check: func(r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				var req SaveDashboardReq
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				assert.True(t, req.Overwrite)
				assert.Equal(t, "folder", req.FolderUID)
			},
		},
		"/api/dashboards/uid/dash": {
			Response: `{"dashboard":{"title":"Imported","panels":[]}}`,
		},
		"/error": {
			Status:   http.StatusBadGateway,
			Response: "bad",
		},
	})
	t.Cleanup(server.Close)

	c := NewGrafanaClient(server.URL+"/", "token", "", "")
	ds, err := c.GetDatasourceByUID(ctx, "prom")
	require.NoError(t, err)
	assert.Equal(t, &DatasourceInfo{UID: "prom", Type: "prometheus", Name: "Prometheus"}, ds)
	assert.Equal(t, "Bearer token", seenAuth)

	ds, err = c.ResolveDatasource(ctx, "Prometheus")
	require.NoError(t, err)
	assert.Equal(t, "prom", ds.UID)

	metrics, err := c.SearchMetrics(ctx, "prom", "up")
	require.NoError(t, err)
	assert.Equal(t, []string{"up", "process_cpu_seconds_total"}, metrics)

	labels, err := c.LookupLabels(ctx, "prom", `{__name__="up"}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"job", "instance"}, labels)

	values, err := c.LookupLabelValues(ctx, "prom", "job", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"api", "worker"}, values)

	metadata, err := c.LookupMetricMetadata(ctx, "prom", "up")
	require.NoError(t, err)
	assert.Contains(t, metadata, `"status":"success"`)

	raw, err := c.VerifyPrometheusQuery(ctx, "prom", "up", "instant")
	require.NoError(t, err)
	assert.Contains(t, raw, `"success"`)
	raw, err = c.VerifyPrometheusQuery(ctx, "prom", "up", "range")
	require.NoError(t, err)
	assert.Contains(t, raw, `"success"`)
	raw, err = c.VerifyLokiQuery(ctx, "loki", `{job="api"}`, "instant")
	require.NoError(t, err)
	assert.Contains(t, raw, `"success"`)
	raw, err = c.VerifyLokiQuery(ctx, "loki", `{job="api"}`, "range")
	require.NoError(t, err)
	assert.Contains(t, raw, `"success"`)

	saveRes, err := c.SaveDashboard(ctx, []byte(`{"title":"Saved"}`), "folder")
	require.NoError(t, err)
	assert.Equal(t, "saved", saveRes.UID)

	dash, err := c.GetDashboardByUID(ctx, "dash")
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"Imported","panels":[]}`, string(dash))

	c.User = "user"
	c.Password = "pass"
	c.Token = ""
	_, err = c.GetDatasourceByUID(ctx, "prom")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(seenAuth, "Basic "))

	_, err = NewGrafanaClient("", "", "", "").GetDatasourceByUID(ctx, "prom")
	require.ErrorContains(t, err, "base URL")
	err = c.getJSON(ctx, "/error", &DatasourceInfo{})
	require.ErrorContains(t, err, "HTTP error 502")
	_, err = c.getRaw(ctx, "/error")
	require.ErrorContains(t, err, "HTTP error 502")
	_, err = c.SaveDashboard(ctx, []byte(`not-json`), "")
	require.ErrorContains(t, err, "parsing dashboard JSON")
}

func TestGrafanaClientVerifyQuery(t *testing.T) {
	ctx := context.Background()
	server := newGrafanaTestServer(t, map[string]grafanaTestRoute{
		"/api/datasources/uid/prom": {
			Response: `{"uid":"prom","type":"prometheus","name":"Prometheus"}`,
		},
		"/api/datasources/uid/loki": {
			Response: `{"uid":"loki","type":"loki","name":"Loki"}`,
		},
		"/api/datasources/uid/unknown": {
			Response: `{"uid":"unknown","type":"tempo","name":"Tempo"}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/query_range": {
			Response: `{"status":"success"}`,
		},
		"/api/datasources/proxy/uid/loki/loki/api/v1/query_range": {
			Response: `{"status":"success"}`,
		},
	})
	t.Cleanup(server.Close)
	c := NewGrafanaClient(server.URL, "", "", "")

	res, err := c.VerifyQuery(ctx, "prom", "up", "range")
	require.NoError(t, err)
	assert.Contains(t, res, "success")
	res, err = c.VerifyQuery(ctx, "loki", `{job="api"}`, "range")
	require.NoError(t, err)
	assert.Contains(t, res, "success")
	_, err = c.VerifyQuery(ctx, "unknown", "trace", "range")
	require.ErrorContains(t, err, "unsupported datasource type")
}

func TestDashboardHandlers(t *testing.T) {
	ctx := context.Background()
	sm := NewSessionManager(t.TempDir())

	addDashboard := addDashboardHandler(sm)
	_, _, err := addDashboard(ctx, nil, AddDashboardReq{})
	require.ErrorContains(t, err, "name is required")
	_, dash, err := addDashboard(ctx, nil, AddDashboardReq{
		Name:  "Handlers",
		UID:   "handlers",
		Tags:  []string{"test"},
		Model: "gpt-5.5",
	})
	require.NoError(t, err)
	require.NotEmpty(t, dash.DashboardID)

	_, listed, err := listSessionsHandler(sm)(ctx, nil, struct{}{})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	assert.Equal(t, "Handlers", listed.Sessions[0].Title)

	_, ok, err := addParamHandler(sm, nil)(ctx, nil, AddParamReq{
		DashboardID:   dash.DashboardID,
		Name:          "job",
		Type:          "query",
		Query:         "label_values(up, job)",
		DatasourceUID: "prom",
	})
	require.NoError(t, err)
	assert.True(t, ok.OK)

	_, ok, err = setTimeRangeHandler(sm)(ctx, nil, SetTimeRangeReq{DashboardID: dash.DashboardID, From: "now-1h", To: "now"})
	require.NoError(t, err)
	assert.True(t, ok.OK)

	_, row, err := addRowHandler(sm)(ctx, nil, AddRowReq{DashboardID: dash.DashboardID, Title: "Row", Collapsed: true})
	require.NoError(t, err)
	require.NotEmpty(t, row.RowID)

	decimals := 2.0
	_, panel, err := addPanelHandler(sm)(ctx, nil, AddPanelReq{
		DashboardID: dash.DashboardID,
		Title:       "CPU",
		Type:        "stat",
		RowID:       row.RowID,
		Unit:        "percent",
		Decimals:    &decimals,
		ReduceCalcs: []string{"lastNotNull"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, panel.PanelID)

	_, _, err = addPanelHandler(sm)(ctx, nil, AddPanelReq{
		DashboardID: dash.DashboardID,
		Title:       "Bad",
		Type:        "text",
		ReduceCalcs: []string{"mean"},
	})
	require.ErrorContains(t, err, "reduce_calcs")

	_, ok, err = updatePanelHandler(sm)(ctx, nil, UpdatePanelReq{
		DashboardID: dash.DashboardID,
		PanelID:     panel.PanelID,
		Title:       "CPU updated",
		Description: "description",
		Unit:        "short",
		ReduceCalcs: []string{"max"},
	})
	require.NoError(t, err)
	assert.True(t, ok.OK)

	_, query, err := addQueryHandler(sm, nil)(ctx, nil, AddQueryReq{
		DashboardID:   dash.DashboardID,
		PanelID:       panel.PanelID,
		DatasourceUID: "prom",
		Expr:          "process_cpu_seconds_total",
		LegendFormat:  "{{instance}}",
	})
	require.NoError(t, err)
	assert.Equal(t, "A", query.QueryRef)
	assert.Empty(t, query.SuggestedUnit)

	_, ok, err = addThresholdHandler(sm)(ctx, nil, AddThresholdReq{
		DashboardID: dash.DashboardID,
		PanelID:     panel.PanelID,
		Value:       90,
		Color:       "red",
	})
	require.NoError(t, err)
	assert.True(t, ok.OK)

	_, state, err := getDashboardStateHandler(sm)(ctx, nil, GetDashboardStateReq(dash))
	require.NoError(t, err)
	assert.Equal(t, "Handlers", state.Title)
	require.Len(t, state.Rows, 1)
	require.Len(t, state.Rows[0].Panels, 1)
	assert.Equal(t, "CPU updated", state.Rows[0].Panels[0].Title)

	_, ok, err = deletePanelHandler(sm)(ctx, nil, DeletePanelReq{DashboardID: dash.DashboardID, PanelID: panel.PanelID})
	require.NoError(t, err)
	assert.True(t, ok.OK)
	_, _, err = deletePanelHandler(sm)(ctx, nil, DeletePanelReq{DashboardID: dash.DashboardID, PanelID: "missing"})
	require.ErrorContains(t, err, "panel_id missing not found")
}

func TestDiscoveryHandlers(t *testing.T) {
	ctx := context.Background()
	server := newGrafanaTestServer(t, map[string]grafanaTestRoute{
		"/api/datasources/name/Prometheus": {
			Response: `{"uid":"prom","type":"prometheus","name":"Prometheus"}`,
		},
		"/api/datasources/uid/prom": {
			Response: `{"uid":"prom","type":"prometheus","name":"Prometheus"}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/query_range": {
			Response: `{"status":"success"}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/label/__name__/values": {
			Response: `{"status":"success","data":["up"]}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/labels": {
			Response: `{"status":"success","data":["job"]}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/label/job/values": {
			Response: `{"status":"success","data":["api"]}`,
		},
		"/api/datasources/proxy/uid/prom/api/v1/metadata": {
			Response: `{"status":"success","data":{}}`,
		},
	})
	t.Cleanup(server.Close)
	gc := NewGrafanaClient(server.URL, "", "", "")

	_, ds, err := resolveDatasourceHandler(gc)(ctx, nil, ResolveDatasourceReq{Name: "Prometheus"})
	require.NoError(t, err)
	assert.Equal(t, ResolveDatasourceRes{UID: "prom", Type: "prometheus"}, ds)
	_, _, err = resolveDatasourceHandler(nil)(ctx, nil, ResolveDatasourceReq{Name: "Prometheus"})
	require.ErrorContains(t, err, "not configured")

	_, verify, err := verifyQueryHandler(gc)(ctx, nil, VerifyQueryReq{DatasourceUID: "prom", Query: "up"})
	require.NoError(t, err)
	assert.Contains(t, verify.Text, "success")
	_, _, err = verifyQueryHandler(nil)(ctx, nil, VerifyQueryReq{})
	require.ErrorContains(t, err, "not configured")

	_, metrics, err := searchMetricsHandler(gc)(ctx, nil, SearchMetricsReq{DatasourceUID: "prom"})
	require.NoError(t, err)
	assert.Equal(t, []string{"up"}, metrics.Metrics)
	_, _, err = searchMetricsHandler(nil)(ctx, nil, SearchMetricsReq{})
	require.ErrorContains(t, err, "not configured")

	_, labels, err := lookupLabelsHandler(gc)(ctx, nil, LookupLabelsReq{DatasourceUID: "prom"})
	require.NoError(t, err)
	assert.Equal(t, []string{"job"}, labels.Labels)
	_, _, err = lookupLabelsHandler(nil)(ctx, nil, LookupLabelsReq{})
	require.ErrorContains(t, err, "not configured")

	_, values, err := lookupLabelValuesHandler(gc)(ctx, nil, LookupLabelValuesReq{DatasourceUID: "prom", Label: "job"})
	require.NoError(t, err)
	assert.Equal(t, []string{"api"}, values.Values)
	_, _, err = lookupLabelValuesHandler(nil)(ctx, nil, LookupLabelValuesReq{})
	require.ErrorContains(t, err, "not configured")

	_, metadata, err := lookupMetricMetadataHandler(gc)(ctx, nil, LookupMetricMetadataReq{DatasourceUID: "prom", Metric: "up"})
	require.NoError(t, err)
	assert.Contains(t, metadata.Text, "success")
	_, _, err = lookupMetricMetadataHandler(nil)(ctx, nil, LookupMetricMetadataReq{})
	require.ErrorContains(t, err, "not configured")
}

func TestBuildPanelVariants(t *testing.T) {
	threshold := 80.0
	for _, tt := range []struct {
		name     string
		typeName string
	}{
		{name: "gauge", typeName: "gauge"},
		{name: "table", typeName: "table"},
		{name: "unknown", typeName: "bargauge"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decimals := 1.0
			builder := buildPanel(&PanelEntry{
				Title:       "Panel",
				Description: "desc",
				Type:        tt.typeName,
				GridPos:     dashboard.GridPos{W: 6, H: 4, X: 1, Y: 2},
				Unit:        "bytes",
				Decimals:    &decimals,
				ReduceCalcs: []string{"mean"},
				Queries: []QueryEntry{{
					RefID:          "A",
					DatasourceUID:  "prom",
					DatasourceType: "prometheus",
					Expr:           "go_memstats_alloc_bytes",
					LegendFormat:   "{{instance}}",
				}},
				Thresholds: []dashboard.Threshold{
					{Value: &threshold, Color: "red"},
					{Value: nil, Color: "green"},
				},
			})
			panel, err := builder.Build()
			require.NoError(t, err)
			assert.Equal(t, "Panel", *panel.Title)
			assert.Equal(t, uint32(6), panel.GridPos.W)
		})
	}
}
