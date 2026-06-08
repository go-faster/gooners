// Package grafana registers MCP tools to build, verify, and save Grafana dashboards.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/gauge"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/table"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// Session specs and models.

type DashboardSession struct {
	DashboardID string         `json:"dashboard_id"`
	Title       string         `json:"title"`
	UID         string         `json:"uid,omitempty"`
	Model       string         `json:"model,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	TimeFrom    string         `json:"time_from,omitempty"`
	TimeTo      string         `json:"time_to,omitempty"`
	Variables   []VariableSpec `json:"variables,omitempty"`
	Rows        []*RowEntry    `json:"rows,omitempty"`
	Panels      []*PanelEntry  `json:"panels,omitempty"`
	NextX       uint32         `json:"next_x"`
	NextY       uint32         `json:"next_y"`
	LineHeight  uint32         `json:"line_height"`
	CreatedAt   time.Time      `json:"created_at"`
	TouchedAt   time.Time      `json:"touched_at"`
}

type VariableSpec struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // "query", "custom", "datasource", etc.
	Query          string `json:"query,omitempty"`
	DatasourceUID  string `json:"datasource_uid,omitempty"`
	DatasourceType string `json:"datasource_type,omitempty"`
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
}

type QueryEntry struct {
	RefID          string `json:"ref_id"`
	DatasourceUID  string `json:"datasource_uid"`
	DatasourceType string `json:"datasource_type"`
	Expr           string `json:"expr"`
	LegendFormat   string `json:"legend_format,omitempty"`
}

func (s *DashboardSession) findPanel(panelID string) (*PanelEntry, *RowEntry, int) {
	for i, p := range s.Panels {
		if p.ID == panelID {
			return p, nil, i
		}
	}
	for _, r := range s.Rows {
		for i, p := range r.Panels {
			if p.ID == panelID {
				return p, r, i
			}
		}
	}
	return nil, nil, -1
}

func (s *DashboardSession) findRow(rowID string) *RowEntry {
	for _, r := range s.Rows {
		if r.ID == rowID {
			return r
		}
	}
	return nil
}

func (s *DashboardSession) findRowIndex(rowID string) int {
	for i, r := range s.Rows {
		if r.ID == rowID {
			return i
		}
	}
	return -1
}

func parseDashboardToSession(dashJSON []byte, sessionID string) (*DashboardSession, error) {
	var dash map[string]any
	if err := json.Unmarshal(dashJSON, &dash); err != nil {
		return nil, err
	}
	s := &DashboardSession{
		DashboardID: sessionID,
		Title:       getString(dash, "title"),
		UID:         getString(dash, "uid"),
		Tags:        getStringSlice(dash, "tags"),
		CreatedAt:   time.Now(),
		TouchedAt:   time.Now(),
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
						Name:  getString(vm, "name"),
						Type:  getString(vm, "type"),
						Query: getString(vm, "query"),
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
			rowID := uuid.New().String()
			row := &RowEntry{
				ID:        rowID,
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
						if pe := parsePanelEntry(spm); pe != nil {
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
					if pe := parsePanelEntry(next); pe != nil {
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
			if pe := parsePanelEntry(pmap); pe != nil {
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

func parsePanelEntry(pmap map[string]any) *PanelEntry {
	pe := &PanelEntry{
		ID:          uuid.New().String(),
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
				}
				if ds, ok := tm["datasource"].(map[string]any); ok {
					q.DatasourceUID = getString(ds, "uid")
					q.DatasourceType = getString(ds, "type")
				}
				pe.Queries = append(pe.Queries, q)
			}
		}
	}
	if fc, ok := pmap["fieldConfig"].(map[string]any); ok {
		if defs, ok := fc["defaults"].(map[string]any); ok {
			if u, ok := defs["unit"].(string); ok {
				pe.Unit = u
			}
			if defs["decimals"] != nil {
				f := getFloat(defs, "decimals")
				pe.Decimals = &f
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
		}
	}
	if opts, ok := pmap["options"].(map[string]any); ok {
		if leg, ok := opts["legend"].(map[string]any); ok {
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
	// 3.1 Construction Tools
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_dashboard",
		Description: "Initializes a new dashboard building session. Pass your model name in the 'model' field — it is recorded on the session and added as a tag and description on export. Omit or pass empty string to opt out.",
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
		Description: "Finalizes and compiles the dashboard. By default, this only validates the dashboard can be built. Use 'save' to push directly to Grafana, or 'output_path' to write the JSON to a local file.",
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
}

// Handler implementations

type AddDashboardReq struct {
	Name  string   `json:"name" jsonschema:"The title of the dashboard"`
	UID   string   `json:"uid,omitempty" jsonschema:"Optional unique ID for the dashboard"`
	Tags  []string `json:"tags,omitempty" jsonschema:"Optional tags for the dashboard"`
	Model string   `json:"model,omitempty" jsonschema:"Your model name (e.g. 'claude-sonnet-4-6'). Recorded on the session and added as a 'created-by:<model>' tag on export. Pass an empty string to opt out."`
}

type AddDashboardRes struct {
	DashboardID string `json:"dashboard_id"`
}

func addDashboardHandler(sm *SessionManager) mcp.ToolHandlerFor[AddDashboardReq, AddDashboardRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddDashboardReq) (*mcp.CallToolResult, AddDashboardRes, error) {
		if args.Name == "" {
			return nil, AddDashboardRes{}, fmt.Errorf("name is required")
		}
		id := uuid.New().String()
		s := &DashboardSession{
			DashboardID: id,
			Title:       args.Name,
			UID:         args.UID,
			Model:       args.Model,
			Tags:        args.Tags,
			CreatedAt:   time.Now(),
			TouchedAt:   time.Now(),
		}
		sm.Add(s)
		return nil, AddDashboardRes{DashboardID: id}, nil
	}
}

type ListSessionsRes struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	DashboardID string    `json:"dashboard_id"`
	Title       string    `json:"title"`
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
			res.Sessions[i] = SessionInfo{
				DashboardID: s.DashboardID,
				Title:       s.Title,
				Model:       s.Model,
				TouchedAt:   s.TouchedAt,
			}
		}
		return nil, res, nil
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
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func getDashboardStateHandler(sm *SessionManager) mcp.ToolHandlerFor[GetDashboardStateReq, *DashboardSession] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args GetDashboardStateReq) (*mcp.CallToolResult, *DashboardSession, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, nil, err
		}
		return nil, s, nil
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
	URL        string `json:"url,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
}

func exportDashboardHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[ExportDashboardReq, ExportDashboardRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ExportDashboardReq) (*mcp.CallToolResult, ExportDashboardRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, ExportDashboardRes{}, err
		}

		dbBuilder := dashboard.NewDashboardBuilder(s.Title)
		if s.UID != "" {
			dbBuilder.Uid(s.UID)
		}
		tags := s.Tags
		if s.Model != "" {
			tags = append(tags, "created-by:"+s.Model)
		}
		if len(tags) > 0 {
			dbBuilder.Tags(tags)
		}
		if s.Model != "" {
			dbBuilder.Description("Created by " + s.Model)
		}
		if s.TimeFrom != "" || s.TimeTo != "" {
			from := s.TimeFrom
			if from == "" {
				from = "now-6h"
			}
			to := s.TimeTo
			if to == "" {
				to = "now"
			}
			dbBuilder.Time(from, to)
		}

		// Variables
		for _, v := range s.Variables {
			switch v.Type {
			case "query":
				vb := dashboard.NewQueryVariableBuilder(v.Name)
				if v.DatasourceUID != "" {
					dsType := v.DatasourceType
					if dsType == "" {
						dsType = "prometheus"
					}
					vb.Datasource(common.DataSourceRef{
						Uid:  new(v.DatasourceUID),
						Type: new(dsType),
					})
				}
				if v.Query != "" {
					vb.Query(dashboard.StringOrMap{String: new(v.Query)})
				}
				vb.Refresh(dashboard.VariableRefresh(1))
				dbBuilder.WithVariable(vb)

			case "custom":
				vb := dashboard.NewCustomVariableBuilder(v.Name)
				if v.Query != "" {
					vb.Values(dashboard.StringOrMap{String: new(v.Query)})
				}
				dbBuilder.WithVariable(vb)

			case "datasource":
				vb := dashboard.NewDatasourceVariableBuilder(v.Name)
				if v.Query != "" {
					vb.Type(v.Query)
				}
				dbBuilder.WithVariable(vb)
			}
		}

		// Add rows
		for _, r := range s.Rows {
			rowBuilder := dashboard.NewRowBuilder(r.Title).Collapsed(r.Collapsed)
			if r.Collapsed {
				for _, p := range r.Panels {
					pBuilder := buildPanel(p)
					rowBuilder.WithPanel(pBuilder)
				}
				dbBuilder.WithRow(rowBuilder)
			} else {
				dbBuilder.WithRow(rowBuilder)
				for _, p := range r.Panels {
					pBuilder := buildPanel(p)
					dbBuilder.WithPanel(pBuilder)
				}
			}
		}

		// Add top-level panels
		for _, p := range s.Panels {
			pBuilder := buildPanel(p)
			dbBuilder.WithPanel(pBuilder)
		}

		dashboardObj, err := dbBuilder.Build()
		if err != nil {
			return nil, ExportDashboardRes{}, fmt.Errorf("building dashboard: %w", err)
		}

		dashboardJSON, err := json.MarshalIndent(dashboardObj, "", "  ")
		if err != nil {
			return nil, ExportDashboardRes{}, fmt.Errorf("marshaling dashboard: %w", err)
		}
		slog.Debug("exported dashboard", "dashboard_id", s.DashboardID)

		res := ExportDashboardRes{
			UID: s.UID,
		}

		if args.OutputPath != "" {
			if err := os.WriteFile(args.OutputPath, dashboardJSON, 0o600); err != nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("writing dashboard to file: %w", err)
			}
			res.OutputPath = args.OutputPath
		}

		if args.Save {
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

func buildPanel(p *PanelEntry) cog.Builder[dashboard.Panel] {
	var targets []cog.Builder[variants.Dataquery]
	for _, q := range p.Queries {
		dq := prometheus.NewDataqueryBuilder().
			Expr(q.Expr).
			RefId(q.RefID)
		if q.DatasourceUID != "" {
			dsType := q.DatasourceType
			if dsType == "" {
				dsType = "prometheus"
			}
			dq.Datasource(common.DataSourceRef{
				Uid:  new(q.DatasourceUID),
				Type: new(dsType),
			})
		}
		if q.LegendFormat != "" {
			dq.LegendFormat(q.LegendFormat)
		}
		targets = append(targets, dq)
	}

	var thresholdsConfig cog.Builder[dashboard.ThresholdsConfig]
	if len(p.Thresholds) > 0 {
		sort.Slice(p.Thresholds, func(i, j int) bool {
			if p.Thresholds[i].Value == nil {
				return true
			}
			if p.Thresholds[j].Value == nil {
				return false
			}
			return *p.Thresholds[i].Value < *p.Thresholds[j].Value
		})
		thresholdsConfig = dashboard.NewThresholdsConfigBuilder().
			Mode(dashboard.ThresholdsModeAbsolute).
			Steps(p.Thresholds)
	}

	switch p.Type {
	case "timeseries":
		pb := timeseries.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		if len(p.ReduceCalcs) > 0 {
			pb.Legend(common.NewVizLegendOptionsBuilder().Calcs(p.ReduceCalcs))
		}
		return pb

	case "stat":
		pb := stat.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		if len(p.ReduceCalcs) > 0 {
			pb.ReduceOptions(common.NewReduceDataOptionsBuilder().Calcs(p.ReduceCalcs))
		}
		return pb

	case "gauge":
		pb := gauge.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		if len(p.ReduceCalcs) > 0 {
			pb.ReduceOptions(common.NewReduceDataOptionsBuilder().Calcs(p.ReduceCalcs))
		}
		return pb

	case "table":
		pb := table.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		if len(p.ReduceCalcs) > 0 {
			pb.Footer(common.NewTableFooterOptionsBuilder().
				Show(true).
				Reducer(p.ReduceCalcs))
		}
		return pb

	default:
		pb := dashboard.NewPanelBuilder().
			Type(p.Type).
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		return pb
	}
}
