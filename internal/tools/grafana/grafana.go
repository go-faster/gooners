// Package grafana registers MCP tools to build, verify, and save Grafana dashboards.
package grafana

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// Session specs and models.

type DashboardSession struct {
	DashboardID string         `json:"dashboard_id"`
	Title       string         `json:"title"`
	Version     string         `json:"version,omitempty"`
	UID         string         `json:"uid,omitempty"`
	Model       string         `json:"model,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	TimeFrom    string         `json:"time_from,omitempty"`
	TimeTo      string         `json:"time_to,omitempty"`
	Description string         `json:"description,omitempty"`
	Refresh     string         `json:"refresh,omitempty"`
	Tooltip     int            `json:"tooltip,omitempty"`
	Variables   []VariableSpec `json:"variables,omitempty"`
	Rows        []*RowEntry    `json:"rows,omitempty"`
	Panels      []*PanelEntry  `json:"panels,omitempty"`
	NextX       uint32         `json:"next_x"`
	NextY       uint32         `json:"next_y"`
	LineHeight  uint32         `json:"line_height"`
	NextPanelID int            `json:"next_panel_id,omitempty"`
	NextRowID   int            `json:"next_row_id,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	TouchedAt   time.Time      `json:"touched_at"`
}

const (
	dashboardVersionV1 = "v1"
	dashboardVersionV2 = "v2"
)

func normalizeDashboardVersion(version string) (string, error) {
	switch strings.ToLower(version) {
	case "", dashboardVersionV1:
		return dashboardVersionV1, nil
	case dashboardVersionV2:
		return dashboardVersionV2, nil
	default:
		return "", fmt.Errorf("unsupported dashboard version %q", version)
	}
}

type VariableSpec struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // "query", "custom", "datasource", etc.
	Query          string `json:"query,omitempty"`
	DatasourceUID  string `json:"datasource_uid,omitempty"`
	DatasourceType string `json:"datasource_type,omitempty"`
	Label          string `json:"label,omitempty"`
	Multi          bool   `json:"multi,omitempty"`
	IncludeAll     bool   `json:"include_all,omitempty"`
	Regex          string `json:"regex,omitempty"`
	Sort           int    `json:"sort,omitempty"`
}

type RowEntry struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Collapsed  bool          `json:"collapsed"`
	Panels     []*PanelEntry `json:"panels,omitempty"`
	Y          uint32        `json:"y"`
	NextX      uint32        `json:"next_x"`
	NextY      uint32        `json:"next_y"`
	LineHeight uint32        `json:"line_height"`
}

type PanelEntry struct {
	ID          string                `json:"id"`
	Title       string                `json:"title"`
	Description string                `json:"description,omitempty"`
	Type        string                `json:"type"` // "timeseries", "stat", "gauge", "table", etc.
	GridPos     dashboard.GridPos     `json:"grid_pos"`
	Unit        string                `json:"unit,omitempty"`
	Decimals    *float64              `json:"decimals,omitempty"`
	Queries     []QueryEntry          `json:"queries,omitempty"`
	Thresholds  []dashboard.Threshold `json:"thresholds,omitempty"`
	ReduceCalcs []string              `json:"reduce_calcs,omitempty"`
	// timeseries visual
	FillOpacity *float64 `json:"fill_opacity,omitempty"`
	LineWidth   *float64 `json:"line_width,omitempty"`
	Stacking    string   `json:"stacking,omitempty"` // "none" | "normal" | "percent"
	AxisSoftMin *float64 `json:"axis_soft_min,omitempty"`
	AxisSoftMax *float64 `json:"axis_soft_max,omitempty"`
	// gauge field-level bounds
	GaugeMin *float64 `json:"gauge_min,omitempty"`
	GaugeMax *float64 `json:"gauge_max,omitempty"`
	// legend (all types)
	LegendDisplayMode string `json:"legend_display_mode,omitempty"` // "list" | "table" | "hidden"
	LegendPlacement   string `json:"legend_placement,omitempty"`    // "bottom" | "right"
}

type QueryEntry struct {
	RefID          string `json:"ref_id"`
	DatasourceUID  string `json:"datasource_uid"`
	DatasourceType string `json:"datasource_type"`
	Expr           string `json:"expr"`
	LegendFormat   string `json:"legend_format,omitempty"`
	Instant        bool   `json:"instant,omitempty"`
	Format         string `json:"format,omitempty"` // "time_series" | "table" | "heatmap"
	Hide           bool   `json:"hide,omitempty"`
}

func (s *DashboardSession) findPanel(panelID string) (*PanelEntry, *RowEntry, int) {
	idx := slices.IndexFunc(s.Panels, func(p *PanelEntry) bool {
		return p.ID == panelID
	})
	if idx >= 0 {
		return s.Panels[idx], nil, idx
	}
	for _, r := range s.Rows {
		idx := slices.IndexFunc(r.Panels, func(p *PanelEntry) bool {
			return p.ID == panelID
		})
		if idx >= 0 {
			return r.Panels[idx], r, idx
		}
	}
	return nil, nil, -1
}

func (s *DashboardSession) findRow(rowID string) *RowEntry {
	idx := s.findRowIndex(rowID)
	if idx < 0 {
		return nil
	}
	return s.Rows[idx]
}

func (s *DashboardSession) findRowIndex(rowID string) int {
	return slices.IndexFunc(s.Rows, func(r *RowEntry) bool {
		return r.ID == rowID
	})
}

func (s *DashboardSession) newPanelID() string {
	s.NextPanelID++
	return fmt.Sprintf("p%d", s.NextPanelID)
}

func (s *DashboardSession) newRowID() string {
	s.NextRowID++
	return fmt.Sprintf("r%d", s.NextRowID)
}

func parseDashboardToSession(dashJSON []byte, sessionID string) (*DashboardSession, error) {
	var dash map[string]any
	if err := json.Unmarshal(dashJSON, &dash); err != nil {
		return nil, err
	}
	s := &DashboardSession{
		DashboardID: sessionID,
		Title:       getString(dash, "title"),
		Version:     dashboardVersionV1,
		UID:         getString(dash, "uid"),
		Tags:        getStringSlice(dash, "tags"),
		Description: getString(dash, "description"),
		Refresh:     getString(dash, "refresh"),
		CreatedAt:   time.Now(),
		TouchedAt:   time.Now(),
	}
	if tt := getFloat(dash, "graphTooltip"); tt != 0 {
		s.Tooltip = int(tt)
	}
	if t, ok := dash["time"].(map[string]any); ok {
		s.TimeFrom = getString(t, "from")
		s.TimeTo = getString(t, "to")
	}
	if templ, ok := dash["templating"].(map[string]any); ok {
		if list, ok := templ["list"].([]any); ok {
			for _, lv := range list {
				if vm, ok := lv.(map[string]any); ok {
					vs := VariableSpec{
						Name:       getString(vm, "name"),
						Type:       getString(vm, "type"),
						Query:      getString(vm, "query"),
						Label:      getString(vm, "label"),
						Multi:      getBool(vm, "multi"),
						IncludeAll: getBool(vm, "includeAll"),
						Regex:      getString(vm, "regex"),
					}
					if sortv := getFloat(vm, "sort"); sortv != 0 {
						vs.Sort = int(sortv)
					}
					if ds, ok := vm["datasource"].(map[string]any); ok {
						vs.DatasourceUID = getString(ds, "uid")
						vs.DatasourceType = getString(ds, "type")
					}
					s.Variables = append(s.Variables, vs)
				}
			}
		}
	}
	panelsRaw, _ := dash["panels"].([]any)
	var topPanels []*PanelEntry
	var rows []*RowEntry
	i := 0
	for i < len(panelsRaw) {
		pmap, ok := panelsRaw[i].(map[string]any)
		if !ok {
			i++
			continue
		}
		ptype := getString(pmap, "type")
		if ptype == "row" {
			row := &RowEntry{
				ID:        s.newRowID(),
				Title:     getString(pmap, "title"),
				Collapsed: getBool(pmap, "collapsed"),
			}
			if gp, ok := pmap["gridPos"].(map[string]any); ok {
				row.Y = uint32(getFloat(gp, "y"))
			}
			row.NextX = 0
			row.NextY = row.Y + 1
			row.LineHeight = 0
			subs := []any{}
			if sps, ok := pmap["panels"].([]any); ok {
				subs = sps
			}
			if row.Collapsed || len(subs) > 0 {
				for _, sp := range subs {
					if spm, ok := sp.(map[string]any); ok {
						if pe := parsePanelEntry(s.newPanelID(), spm); pe != nil {
							row.Panels = append(row.Panels, pe)
						}
					}
				}
			} else {
				i++
				for i < len(panelsRaw) {
					next, ok := panelsRaw[i].(map[string]any)
					if !ok {
						i++
						continue
					}
					if getString(next, "type") == "row" {
						break
					}
					if pe := parsePanelEntry(s.newPanelID(), next); pe != nil {
						row.Panels = append(row.Panels, pe)
					}
					i++
				}
				for _, p := range row.Panels {
					if p.GridPos.Y+p.GridPos.H > row.NextY {
						row.NextY = p.GridPos.Y + p.GridPos.H
					}
					// Approximation to restore layout tightly for the last row-line
					if p.GridPos.X+p.GridPos.W > row.NextX {
						row.NextX = p.GridPos.X + p.GridPos.W
					}
					if p.GridPos.H > row.LineHeight {
						row.LineHeight = p.GridPos.H
					}
				}
				rows = append(rows, row)
				if row.NextY > s.NextY {
					s.NextY = row.NextY
				}
				continue
			}
			for _, p := range row.Panels {
				if p.GridPos.Y+p.GridPos.H > row.NextY {
					row.NextY = p.GridPos.Y + p.GridPos.H
				}
				if p.GridPos.X+p.GridPos.W > row.NextX {
					row.NextX = p.GridPos.X + p.GridPos.W
				}
				if p.GridPos.H > row.LineHeight {
					row.LineHeight = p.GridPos.H
				}
			}
			rows = append(rows, row)
			if row.NextY > s.NextY {
				s.NextY = row.NextY
			}
		} else {
			if pe := parsePanelEntry(s.newPanelID(), pmap); pe != nil {
				topPanels = append(topPanels, pe)
				if pe.GridPos.Y+pe.GridPos.H > s.NextY {
					s.NextY = pe.GridPos.Y + pe.GridPos.H
				}
			}
		}
		i++
	}
	s.Rows = rows
	s.Panels = topPanels
	for _, r := range s.Rows {
		if r.NextY > s.NextY {
			s.NextY = r.NextY
		}
	}
	return s, nil
}

func parsePanelEntry(id string, pmap map[string]any) *PanelEntry {
	pe := &PanelEntry{
		ID:          id,
		Title:       getString(pmap, "title"),
		Description: getString(pmap, "description"),
		Type:        getString(pmap, "type"),
	}
	if gp, ok := pmap["gridPos"].(map[string]any); ok {
		pe.GridPos = dashboard.GridPos{
			X: uint32(getFloat(gp, "x")),
			Y: uint32(getFloat(gp, "y")),
			W: uint32(getFloat(gp, "w")),
			H: uint32(getFloat(gp, "h")),
		}
	}
	if targets, ok := pmap["targets"].([]any); ok {
		for _, t := range targets {
			if tm, ok := t.(map[string]any); ok {
				q := QueryEntry{
					RefID:        getString(tm, "refId"),
					Expr:         getString(tm, "expr"),
					LegendFormat: getString(tm, "legendFormat"),
					Instant:      getBool(tm, "instant"),
					Format:       getString(tm, "format"),
					Hide:         getBool(tm, "hide"),
				}
				if ds, ok := tm["datasource"].(map[string]any); ok {
					q.DatasourceUID = getString(ds, "uid")
					q.DatasourceType = getString(ds, "type")
				}
				pe.Queries = append(pe.Queries, q)
			}
		}
	}
	// fieldConfig (merged): defaults (unit/decimals/thresholds/min/max + timeseries custom) + overrides
	if fc, ok := pmap["fieldConfig"].(map[string]any); ok {
		if defs, ok := fc["defaults"].(map[string]any); ok {
			if u, ok := defs["unit"].(string); ok {
				pe.Unit = u
			}
			if defs["decimals"] != nil {
				f := getFloat(defs, "decimals")
				pe.Decimals = &f
			}
			if minv, ok := defs["min"].(float64); ok {
				pe.GaugeMin = &minv
			}
			if maxv, ok := defs["max"].(float64); ok {
				pe.GaugeMax = &maxv
			}
			if ths, ok := defs["thresholds"].(map[string]any); ok {
				if steps, ok := ths["steps"].([]any); ok {
					for _, st := range steps {
						if sm, ok := st.(map[string]any); ok {
							var val *float64
							if sm["value"] != nil {
								f := getFloat(sm, "value")
								val = &f
							}
							col := getString(sm, "color")
							if col != "" {
								pe.Thresholds = append(pe.Thresholds, dashboard.Threshold{Value: val, Color: col})
							}
						}
					}
				}
			}
			// timeseries custom fields are flat under custom (per SDK): fillOpacity, lineWidth, stacking{ mode }, axisSoftMin, axisSoftMax
			if custom, ok := defs["custom"].(map[string]any); ok {
				if fo, ok := custom["fillOpacity"].(float64); ok {
					pe.FillOpacity = &fo
				}
				if lw, ok := custom["lineWidth"].(float64); ok {
					pe.LineWidth = &lw
				}
				if sm, ok := custom["stacking"].(map[string]any); ok {
					if mode, ok := sm["mode"].(string); ok && mode != "" && mode != "none" {
						pe.Stacking = mode
					}
				}
				if smin, ok := custom["axisSoftMin"].(float64); ok {
					pe.AxisSoftMin = &smin
				}
				if smax, ok := custom["axisSoftMax"].(float64); ok {
					pe.AxisSoftMax = &smax
				}
			}
		}
		if overrides, ok := fc["overrides"].([]any); ok {
			for _, ov := range overrides {
				if om, ok := ov.(map[string]any); ok {
					if matcher, ok := om["matcher"].(map[string]any); ok {
						if getString(matcher, "id") == "byName" && getString(matcher, "options") == "Value" {
							if props, ok := om["properties"].([]any); ok {
								for _, pr := range props {
									if pm, ok := pr.(map[string]any); ok && getString(pm, "id") == "min" {
										if v, ok := pm["value"].(float64); ok {
											pe.GaugeMin = &v
										}
									}
									if pm, ok := pr.(map[string]any); ok && getString(pm, "id") == "max" {
										if v, ok := pm["value"].(float64); ok {
											pe.GaugeMax = &v
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	// options (legend + reduce calcs)
	if opts, ok := pmap["options"].(map[string]any); ok {
		if leg, ok := opts["legend"].(map[string]any); ok {
			if dm, ok := leg["displayMode"].(string); ok && dm != "" {
				pe.LegendDisplayMode = dm
			}
			if pl, ok := leg["placement"].(string); ok && pl != "" {
				pe.LegendPlacement = pl
			}
			if calcs, ok := leg["calcs"].([]any); ok {
				for _, c := range calcs {
					if s, ok := c.(string); ok {
						pe.ReduceCalcs = append(pe.ReduceCalcs, s)
					}
				}
			}
		}
		if ro, ok := opts["reduceOptions"].(map[string]any); ok {
			if calcs, ok := ro["calcs"].([]any); ok {
				for _, c := range calcs {
					if s, ok := c.(string); ok {
						pe.ReduceCalcs = append(pe.ReduceCalcs, s)
					}
				}
			}
		}
		// For stat/gauge/table, legend display/placement may be under reduceOptions or top-level options (legacy)
		if pe.LegendDisplayMode == "" {
			if dm, ok := opts["legend"].(string); ok && dm != "" {
				pe.LegendDisplayMode = dm
			}
		}
	}
	return pe
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBool(m map[string]any, k string) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func getFloat(m map[string]any, k string) float64 {
	if v, ok := m[k]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
		if i, ok := v.(int); ok {
			return float64(i)
		}
		if i, ok := v.(int64); ok {
			return float64(i)
		}
	}
	return 0
}

func getStringSlice(m map[string]any, k string) []string {
	if v, ok := m[k].([]any); ok {
		var res []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				res = append(res, s)
			}
		}
		return res
	}
	return nil
}

// Tool implementation.

func Register(s *mcp.Server, sm *SessionManager, gc *GrafanaClient) {
	registerResources(s, sm)

	// 3.1 Construction Tools
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_dashboard",
		Description: "Initializes a new dashboard building session. Prefer version='v1' unless v2 output is explicitly required. Pass your model name in the 'model' field — it is recorded on the session and added as a tag and description on export. Omit or pass empty string to opt out.",
	}, addDashboardHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_dashboard_sessions",
		Description: "Returns active dashboard_ids with their titles and timestamps.",
		Flags:       mcputil.ReadOnly,
	}, listSessionsHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "import_dashboard",
		Description: "Fetches an existing dashboard by UID from Grafana, or from a local file path, and starts a new editable session from it. Works for dashboards created with this tool (roundtrippable). Provide exactly one of uid or file_path.",
		Flags:       mcputil.ReadOnly,
	}, importDashboardHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_param",
		Description: "Adds a template variable/parameter to the dashboard (e.g. cluster, namespace).",
	}, addParamHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "set_time_range",
		Description: "Sets the default time range for the dashboard (e.g. now-6h to now).",
	}, setTimeRangeHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_row",
		Description: "Adds a standard Grafana row for grouping panels.",
	}, addRowHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_panel",
		Description: "Adds a panel to the dashboard. Supports unit, decimals, and reduce_calcs directly. Row-aware auto-layout places stat/gauge panels side-by-side (W=6) and wraps if they exceed 24 columns.",
	}, addPanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_panels_batch",
		Description: "Adds multiple panels to a dashboard or a row in a single batch operation. Avoids multiple roundtrips by allowing specifications of queries, thresholds, units, decimals, and calculation options (reduce_calcs) directly.",
	}, addPanelsBatchHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "update_panel",
		Description: "Updates properties of an existing panel without rebuilding. Supports updating title, description, type, unit, decimals, and reduce_calcs. Changing type resets reduce_calcs to empty.",
	}, updatePanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "delete_panel",
		Description: "Removes a panel from the ongoing dashboard session.",
		Flags:       mcputil.Destructive,
	}, deletePanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "move_panel",
		Description: "Moves a panel to a different row or to dashboard top-level. Uses auto-layout in the destination when no explicit x/y are given. Pass row_id='' to move to top-level.",
	}, movePanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_query",
		Description: "Attaches a query to an existing panel. Returns a suggested_unit using Prometheus metadata. Use legend_format for legend labels (e.g. '{{class}}', '{{pod}}'). Call lookup_metric_metadata first if you are unsure of the metric's unit.",
	}, addQueryHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "update_query",
		Description: "Edits an existing query on a panel. Identify the query by its ref ID (e.g. A, B, C). Pass only the fields to change.",
	}, updateQueryHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "delete_query",
		Description: "Removes a query from a panel by its ref ID (e.g. A, B, C).",
		Flags:       mcputil.Destructive,
	}, deleteQueryHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_threshold",
		Description: "Adds a color threshold to stat/gauge panels. Base threshold is automatically created on panel creation.",
	}, addThresholdHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "update_dashboard",
		Description: "Edits metadata on the current dashboard session: title, uid, and/or tags.",
	}, updateDashboardHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "update_row",
		Description: "Edits a row's title and/or collapsed state.",
	}, updateRowHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "delete_row",
		Description: "Removes a row. By default all panels inside are discarded. Pass keep_panels=true to promote them to dashboard top-level instead.",
		Flags:       mcputil.Destructive,
	}, deleteRowHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "move_row",
		Description: "Reorders a row relative to another. Pass before_row_id to insert before it, or empty string to move the row to the end.",
	}, moveRowHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "get_dashboard_state",
		Description: "Returns the current in-progress structure of the dashboard.",
		Flags:       mcputil.ReadOnly,
	}, getDashboardStateHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "export_dashboard",
		Description: "Finalizes and compiles the dashboard using the session's schema version. By default, this only validates the dashboard can be built. Use 'save' to push v1 dashboards directly to Grafana, or 'output_path' to write v1/v2 JSON to a local file.",
	}, exportDashboardHandler(sm, gc))

	// 3.2 Discovery & Verification Tools
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "resolve_datasource",
		Description: "Resolves a datasource name to its UID and type.",
		Flags:       mcputil.ReadOnly,
	}, resolveDatasourceHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "verify_query",
		Description: "Validates a query against the datasource.",
		Flags:       mcputil.ReadOnly,
	}, verifyQueryHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "search_metrics",
		Description: "Finds metric names matching a pattern.",
		Flags:       mcputil.ReadOnly,
	}, searchMetricsHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_labels",
		Description: "Fetches labels for a given selector/metric.",
		Flags:       mcputil.ReadOnly,
	}, lookupLabelsHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_label_values",
		Description: "Fetches available values for a specific label.",
		Flags:       mcputil.ReadOnly,
	}, lookupLabelValuesHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_metric_metadata",
		Description: "Returns metric type (counter/gauge/histogram) and help string.",
		Flags:       mcputil.ReadOnly,
	}, lookupMetricMetadataHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "parse_promql",
		Description: "Parses a PromQL/MetricsQL expression offline and returns a syntax error or the normalized expression. Use this to catch syntax errors when Grafana is not configured. Note: Grafana duration macros like $__rate_interval are not valid PromQL durations and will be stripped by the parser — substitute real values (e.g. 5m) before calling this tool.",
		Flags:       mcputil.ReadOnly,
	}, parsePromQLHandler())

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "discover_telemetry_registry",
		Description: "Parses Weaver OpenTelemetry YAML files (semantic conventions) to discover metrics, their types, units, and attributes. By default, it searches the current working directory.",
		Flags:       mcputil.ReadOnly,
	}, discoverTelemetryRegistryHandler())
}

// Handler implementations

type AddDashboardReq struct {
	Name    string   `json:"name" jsonschema:"The title of the dashboard"`
	Version string   `json:"version,omitempty" jsonschema:"Dashboard schema version to export: v1 or v2. Defaults to v1, which is preferred unless v2 is explicitly needed."`
	UID     string   `json:"uid,omitempty" jsonschema:"Optional unique ID for the dashboard"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional tags for the dashboard"`
	Model   string   `json:"model,omitempty" jsonschema:"Your model name (e.g. 'claude-sonnet-4-6'). Recorded on the session and added as a 'created-by:<model>' tag on export. Pass an empty string to opt out."`
}

type AddDashboardRes struct {
	DashboardID string `json:"dashboard_id"`
	Version     string `json:"version"`
}

func addDashboardHandler(sm *SessionManager) mcp.ToolHandlerFor[AddDashboardReq, AddDashboardRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddDashboardReq) (*mcp.CallToolResult, AddDashboardRes, error) {
		if args.Name == "" {
			return nil, AddDashboardRes{}, fmt.Errorf("name is required")
		}
		version, err := normalizeDashboardVersion(args.Version)
		if err != nil {
			return nil, AddDashboardRes{}, err
		}
		id := uuid.New().String()
		s := &DashboardSession{
			DashboardID: id,
			Title:       args.Name,
			Version:     version,
			UID:         args.UID,
			Model:       args.Model,
			Tags:        args.Tags,
			CreatedAt:   time.Now(),
			TouchedAt:   time.Now(),
		}
		sm.Add(s)
		return nil, AddDashboardRes{DashboardID: id, Version: version}, nil
	}
}

type ListSessionsRes struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	DashboardID string    `json:"dashboard_id"`
	Title       string    `json:"title"`
	Version     string    `json:"version"`
	Model       string    `json:"model,omitempty"`
	TouchedAt   time.Time `json:"touched_at"`
}

func listSessionsHandler(sm *SessionManager) mcp.ToolHandlerFor[struct{}, ListSessionsRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListSessionsRes, error) {
		sessions := sm.List()
		res := ListSessionsRes{
			Sessions: make([]SessionInfo, len(sessions)),
		}
		for i, s := range sessions {
			res.Sessions[i] = sessionInfo(s)
		}
		return nil, res, nil
	}
}

func sessionInfo(s *DashboardSession) SessionInfo {
	return SessionInfo{
		DashboardID: s.DashboardID,
		Title:       s.Title,
		Version:     cmp.Or(s.Version, dashboardVersionV1),
		Model:       s.Model,
		TouchedAt:   s.TouchedAt,
	}
}

type ImportDashboardReq struct {
	UID      string `json:"uid,omitempty" jsonschema:"The UID of the dashboard in Grafana to import for editing"`
	FilePath string `json:"file_path,omitempty" jsonschema:"The file path to import the dashboard from"`
}

type ImportDashboardRes struct {
	DashboardID string `json:"dashboard_id"`
	Title       string `json:"title,omitempty"`
}

func importDashboardHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[ImportDashboardReq, ImportDashboardRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ImportDashboardReq) (*mcp.CallToolResult, ImportDashboardRes, error) {
		var raw []byte
		var err error
		switch {
		case args.FilePath != "":
			raw, err = os.ReadFile(args.FilePath)
			if err != nil {
				return nil, ImportDashboardRes{}, fmt.Errorf("reading dashboard file: %w", err)
			}
		case args.UID != "":
			if gc == nil {
				return nil, ImportDashboardRes{}, fmt.Errorf("grafana client not configured")
			}
			raw, err = gc.GetDashboardByUID(ctx, args.UID)
			if err != nil {
				return nil, ImportDashboardRes{}, fmt.Errorf("fetching dashboard: %w", err)
			}
		default:
			return nil, ImportDashboardRes{}, fmt.Errorf("either uid or file_path must be provided")
		}

		id := uuid.New().String()
		sess, err := parseDashboardToSession(raw, id)
		if err != nil {
			return nil, ImportDashboardRes{}, fmt.Errorf("parsing dashboard: %w", err)
		}
		if sess.UID == "" {
			sess.UID = args.UID
		}
		sm.Add(sess)
		return nil, ImportDashboardRes{DashboardID: id, Title: sess.Title}, nil
	}
}

type AddParamReq struct {
	DashboardID    string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Name           string `json:"name" jsonschema:"The name of the variable"`
	Type           string `json:"type" jsonschema:"The type of the variable (e.g. query, custom, datasource)"`
	Query          string `json:"query,omitempty" jsonschema:"The query expression or values"`
	DatasourceUID  string `json:"datasource_uid,omitempty" jsonschema:"Optional datasource UID"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional datasource type"`
	Label          string `json:"label,omitempty" jsonschema:"Optional display label for the variable"`
	Multi          bool   `json:"multi,omitempty" jsonschema:"Allow multiple selections"`
	IncludeAll     bool   `json:"include_all,omitempty" jsonschema:"Include an 'All' option"`
	Regex          string `json:"regex,omitempty" jsonschema:"Optional regex filter for query variables"`
	Sort           int    `json:"sort,omitempty" jsonschema:"Sort order: 0=disabled, 1=alpha asc, 2=alpha desc, 3=numeric asc, 4=numeric desc, 5=alpha case-insensitive asc, 6=alpha case-insensitive desc"`
}

func addParamHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[AddParamReq, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args AddParamReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		dsType := args.DatasourceType
		if dsType == "" && args.DatasourceUID != "" {
			dsType = "prometheus"
			if gc != nil {
				info, err := gc.GetDatasourceByUID(ctx, args.DatasourceUID)
				if err == nil && info != nil {
					dsType = info.Type
				}
			}
		}

		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			s.Variables = append(s.Variables, VariableSpec{
				Name:           args.Name,
				Type:           args.Type,
				Query:          args.Query,
				DatasourceUID:  args.DatasourceUID,
				DatasourceType: dsType,
				Label:          args.Label,
				Multi:          args.Multi,
				IncludeAll:     args.IncludeAll,
				Regex:          args.Regex,
				Sort:           args.Sort,
			})
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type SetTimeRangeReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	From        string `json:"from" jsonschema:"The start time (e.g. now-6h)"`
	To          string `json:"to" jsonschema:"The end time (e.g. now)"`
}

func setTimeRangeHandler(sm *SessionManager) mcp.ToolHandlerFor[SetTimeRangeReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args SetTimeRangeReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			s.TimeFrom = args.From
			s.TimeTo = args.To
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type UpdateDashboardReq struct {
	DashboardID string   `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Title       string   `json:"title,omitempty" jsonschema:"Optional new title"`
	UID         string   `json:"uid,omitempty" jsonschema:"Optional new UID"`
	Tags        []string `json:"tags,omitempty" jsonschema:"Optional new tag list (replaces existing)"`
	Description string   `json:"description,omitempty" jsonschema:"Optional description (stored as model field override)"`
	Refresh     string   `json:"refresh,omitempty" jsonschema:"Optional auto-refresh interval (e.g. \"30s\", \"1m\")"`
	Tooltip     int      `json:"tooltip,omitempty" jsonschema:"Optional cursor sync mode: 0=off, 1=crosshair, 2=shared crosshair"`
}

func updateDashboardHandler(sm *SessionManager) mcp.ToolHandlerFor[UpdateDashboardReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args UpdateDashboardReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			if args.Title != "" {
				s.Title = args.Title
			}
			if args.UID != "" {
				s.UID = args.UID
			}
			if args.Tags != nil {
				s.Tags = args.Tags
			}
			if args.Description != "" {
				s.Description = args.Description
			}
			if args.Refresh != "" {
				s.Refresh = args.Refresh
			}
			if args.Tooltip != 0 {
				s.Tooltip = args.Tooltip
			}
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

// DashboardStateRes wraps the session with a pre-rendered layout view.
// The Layout field uses recomputed Y positions — the same positions that will
// appear in the exported Grafana JSON — so it can be used to verify row
// membership and spot gaps before calling export_dashboard.
type DashboardStateRes struct {
	*DashboardSession
	// Layout is a band-format spatial summary of the dashboard.
	// Rows are listed in render order; panels within each row are listed
	// left-to-right per Y-band. GAP entries show unfilled grid space.
	// Y values reflect the positions that will be used on export.
	Layout string `json:"layout"`
}

func getDashboardStateHandler(sm *SessionManager) mcp.ToolHandlerFor[GetDashboardStateReq, DashboardStateRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args GetDashboardStateReq) (*mcp.CallToolResult, DashboardStateRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, DashboardStateRes{}, err
		}
		return nil, DashboardStateRes{
			DashboardSession: s,
			Layout:           renderLayout(s),
		}, nil
	}
}

type GetDashboardStateReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
}

type ExportDashboardReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Save        bool   `json:"save,omitempty" jsonschema:"If true, saves the dashboard directly to Grafana API"`
	FolderUID   string `json:"folder_uid,omitempty" jsonschema:"Optional folder UID to save the dashboard under"`
	OutputPath  string `json:"output_path,omitempty" jsonschema:"Optional file path to save the dashboard JSON to. If not absolute, it will be relative to the server's working directory."`
}

type ExportDashboardRes struct {
	Saved      bool   `json:"saved"`
	UID        string `json:"uid"`
	Version    string `json:"version"`
	URL        string `json:"url,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
}

func exportDashboardHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[ExportDashboardReq, ExportDashboardRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ExportDashboardReq) (*mcp.CallToolResult, ExportDashboardRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, ExportDashboardRes{}, err
		}

		version, err := normalizeDashboardVersion(s.Version)
		if err != nil {
			return nil, ExportDashboardRes{}, err
		}

		dashboardJSON, err := buildDashboardJSON(s, version)
		if err != nil {
			return nil, ExportDashboardRes{}, err
		}
		slog.Debug("exported dashboard", "dashboard_id", s.DashboardID, "version", version)

		res := ExportDashboardRes{
			UID:     s.UID,
			Version: version,
		}

		if args.OutputPath != "" {
			if err := os.WriteFile(args.OutputPath, dashboardJSON, 0o600); err != nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("writing dashboard to file: %w", err)
			}
			res.OutputPath = args.OutputPath
		}

		if args.Save {
			if version != dashboardVersionV1 {
				return nil, ExportDashboardRes{}, fmt.Errorf("saving %s dashboards is not supported by the legacy Grafana dashboard API", version)
			}
			if gc == nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("grafana client not configured, cannot save dashboard")
			}
			saveRes, err := gc.SaveDashboard(ctx, dashboardJSON, args.FolderUID)
			if err != nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("saving dashboard to Grafana: %w", err)
			}
			res.Saved = true
			res.UID = saveRes.UID
			res.URL = saveRes.URL
		}

		return nil, res, nil
	}
}

func buildReduceOptions(p *PanelEntry) *common.ReduceDataOptionsBuilder {
	if len(p.ReduceCalcs) == 0 {
		return nil
	}
	return common.NewReduceDataOptionsBuilder().Calcs(p.ReduceCalcs)
}

// recomputeRowPositions returns adjusted row copies with Y positions derived
// from actual panel content rather than the snapshot taken at row-creation time.
// Models commonly create all rows first and add panels later, which leaves row
// headers stacked at Y=0,1,2 regardless of how tall prior rows' panels are.
func recomputeRowPositions(rows []*RowEntry) []*RowEntry {
	result := make([]*RowEntry, len(rows))
	currentY := uint32(0)
	for i, r := range rows {
		delta := int(currentY) - int(r.Y)
		rowCopy := *r
		rowCopy.Y = currentY
		currentY++ // row header is 1 unit tall

		if r.Collapsed {
			// Collapsed rows hide their panels; they don't consume vertical space.
			rowCopy.Panels = r.Panels
			result[i] = &rowCopy
			continue
		}

		if delta != 0 && len(r.Panels) > 0 {
			panelsCopy := make([]*PanelEntry, len(r.Panels))
			for j, p := range r.Panels {
				pc := *p
				pc.GridPos.Y = uint32(int(p.GridPos.Y) + delta)
				panelsCopy[j] = &pc
			}
			rowCopy.Panels = panelsCopy
		}

		for _, p := range rowCopy.Panels {
			if bottom := p.GridPos.Y + p.GridPos.H; bottom > currentY {
				currentY = bottom
			}
		}
		result[i] = &rowCopy
	}
	return result
}

// renderLayout produces a band-format spatial summary of the dashboard.
// It calls recomputeRowPositions so Y values match what export_dashboard will emit.
func renderLayout(s *DashboardSession) string {
	var b strings.Builder
	b.WriteString("grid: 24 cols\n")

	for _, r := range recomputeRowPositions(s.Rows) {
		extra := ""
		if r.Collapsed {
			extra = " [collapsed]"
		}
		fmt.Fprintf(&b, "\nrow %q [y=%d id=%s%s]\n", r.Title, r.Y, r.ID, extra)
		writePanelBands(&b, r.Panels)
	}

	if len(s.Panels) > 0 {
		b.WriteString("\n(no row — panels rendered outside any row section)\n")
		writePanelBands(&b, s.Panels)
	}

	return b.String()
}

// writePanelBands writes panels grouped by Y-band, sorted left-to-right, with
// explicit GAP entries for unoccupied grid columns.
func writePanelBands(b *strings.Builder, panels []*PanelEntry) {
	if len(panels) == 0 {
		b.WriteString("  (empty)\n")
		return
	}

	sorted := slices.Clone(panels)
	slices.SortFunc(sorted, func(a, b *PanelEntry) int {
		if c := cmp.Compare(a.GridPos.Y, b.GridPos.Y); c != 0 {
			return c
		}
		return cmp.Compare(a.GridPos.X, b.GridPos.X)
	})

	i := 0
	for i < len(sorted) {
		y := sorted[i].GridPos.Y
		cursor := uint32(0)
		for i < len(sorted) && sorted[i].GridPos.Y == y {
			p := sorted[i]
			if p.GridPos.X > cursor {
				fmt.Fprintf(b, "  GAP [x=%d w=%d y=%d]\n", cursor, p.GridPos.X-cursor, y)
			}
			fmt.Fprintf(b, "  [x=%-2d w=%-2d h=%-2d y=%-2d] %s %q (%s)\n",
				p.GridPos.X, p.GridPos.W, p.GridPos.H, p.GridPos.Y, p.ID, p.Title, p.Type)
			cursor = p.GridPos.X + p.GridPos.W
			i++
		}
		if cursor < 24 {
			fmt.Fprintf(b, "  GAP [x=%d w=%d y=%d]\n", cursor, 24-cursor, y)
		}
	}
}
