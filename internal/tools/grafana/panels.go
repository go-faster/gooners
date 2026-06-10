package grafana

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type AddPanelReq struct {
	DashboardID string   `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Title       string   `json:"title" jsonschema:"The title of the panel"`
	Description string   `json:"description,omitempty" jsonschema:"Optional panel description"`
	Type        string   `json:"type" jsonschema:"The panel type (e.g. timeseries, stat, gauge, table)"`
	RowID       string   `json:"row_id,omitempty" jsonschema:"Optional row ID to group the panel under"`
	W           *int     `json:"w,omitempty" jsonschema:"Optional width (1-24)"`
	H           *int     `json:"h,omitempty" jsonschema:"Optional height"`
	X           *int     `json:"x,omitempty" jsonschema:"Optional X position (0-23)"`
	Y           *int     `json:"y,omitempty" jsonschema:"Optional Y position"`
	Unit        string   `json:"unit,omitempty" jsonschema:"Optional unit (e.g. short, percent, bytes)"`
	Decimals    *float64 `json:"decimals,omitempty" jsonschema:"Optional decimal precision"`
	ReduceCalcs []string `json:"reduce_calcs,omitempty" jsonschema:"Optional calculation/reducers for stat/gauge panels (e.g. mean, lastNotNull, max)"`
	// timeseries visual
	FillOpacity *float64 `json:"fill_opacity,omitempty" jsonschema:"Optional fill opacity (0-100) for timeseries"`
	LineWidth   *float64 `json:"line_width,omitempty" jsonschema:"Optional line width for timeseries"`
	Stacking    string   `json:"stacking,omitempty" jsonschema:"Stacking mode: none, normal, percent"`
	AxisSoftMin *float64 `json:"axis_soft_min,omitempty" jsonschema:"Optional soft min for Y axis"`
	AxisSoftMax *float64 `json:"axis_soft_max,omitempty" jsonschema:"Optional soft max for Y axis"`
	// gauge field-level bounds
	GaugeMin *float64 `json:"gauge_min,omitempty" jsonschema:"Optional min for gauge"`
	GaugeMax *float64 `json:"gauge_max,omitempty" jsonschema:"Optional max for gauge"`
	// legend (visual effect only on timeseries; stored for stat/gauge/table but ignored at export time)
	LegendDisplayMode string `json:"legend_display_mode,omitempty" jsonschema:"Legend display: list, table, hidden (visual effect only on timeseries)"`
	LegendPlacement   string `json:"legend_placement,omitempty" jsonschema:"Legend placement: bottom, right (visual effect only on timeseries)"`
}

type AddPanelRes struct {
	PanelID string            `json:"panel_id"`
	GridPos dashboard.GridPos `json:"grid_pos"`
}

type PanelSpec struct {
	Title       string          `json:"title" jsonschema:"The title of the panel"`
	Description string          `json:"description,omitempty" jsonschema:"Optional panel description"`
	Type        string          `json:"type" jsonschema:"The panel type (e.g. timeseries, stat, gauge, table)"`
	W           *int            `json:"w,omitempty" jsonschema:"Optional width (1-24)"`
	H           *int            `json:"h,omitempty" jsonschema:"Optional height"`
	X           *int            `json:"x,omitempty" jsonschema:"Optional X position (0-23)"`
	Y           *int            `json:"y,omitempty" jsonschema:"Optional Y position"`
	Unit        string          `json:"unit,omitempty" jsonschema:"Optional unit (e.g. short, percent, bytes)"`
	Decimals    *float64        `json:"decimals,omitempty" jsonschema:"Optional decimal precision"`
	ReduceCalcs []string        `json:"reduce_calcs,omitempty" jsonschema:"Optional calculation/reducers for stat/gauge panels (e.g. mean, lastNotNull, max)"`
	Queries     []QuerySpec     `json:"queries,omitempty" jsonschema:"Optional queries to attach to the panel"`
	Thresholds  []ThresholdSpec `json:"thresholds,omitempty" jsonschema:"Optional thresholds to add to stat/gauge panels"`
	// timeseries visual
	FillOpacity *float64 `json:"fill_opacity,omitempty" jsonschema:"Optional fill opacity (0-100) for timeseries"`
	LineWidth   *float64 `json:"line_width,omitempty" jsonschema:"Optional line width for timeseries"`
	Stacking    string   `json:"stacking,omitempty" jsonschema:"Stacking mode: none, normal, percent"`
	AxisSoftMin *float64 `json:"axis_soft_min,omitempty" jsonschema:"Optional soft min for Y axis"`
	AxisSoftMax *float64 `json:"axis_soft_max,omitempty" jsonschema:"Optional soft max for Y axis"`
	// gauge field-level bounds
	GaugeMin *float64 `json:"gauge_min,omitempty" jsonschema:"Optional min for gauge"`
	GaugeMax *float64 `json:"gauge_max,omitempty" jsonschema:"Optional max for gauge"`
	// legend (visual effect only on timeseries; stored for stat/gauge/table but ignored at export time)
	LegendDisplayMode string `json:"legend_display_mode,omitempty" jsonschema:"Legend display: list, table, hidden (visual effect only on timeseries)"`
	LegendPlacement   string `json:"legend_placement,omitempty" jsonschema:"Legend placement: bottom, right (visual effect only on timeseries)"`
}

type ThresholdSpec struct {
	Value *float64 `json:"value,omitempty" jsonschema:"Optional threshold value (nil for base)"`
	Color string   `json:"color" jsonschema:"The color for the threshold"`
}

type AddPanelsBatchReq struct {
	DashboardID string      `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	RowID       string      `json:"row_id,omitempty" jsonschema:"Optional row ID to group the panels under"`
	Panels      []PanelSpec `json:"panels" jsonschema:"List of panel specifications"`
}

type AddPanelsBatchRes struct {
	PanelIDs []string `json:"panel_ids"`
}

// placePanel computes the GridPos for a new panel using a flow layout.
//
// The layout tracks two cursors — NextX (column) and NextY (max extent) — at
// both the row level (r != nil) and the session level (r == nil), mirroring
// Grafana's own grid model.
//
// Row context (r != nil):
//   - Panels are placed left-to-right starting at r.NextX, r.NextY.
//   - r.LineHeight tracks the tallest panel on the current line.
//   - When r.NextX + w > 24 the line wraps: r.NextY advances by r.LineHeight,
//     r.NextX and r.LineHeight reset to zero.
//   - s.NextY is kept as the global max extent (max of y+h across all panels).
//
// Session context (r == nil):
//   - s.NextX and s.LineHeight play the same role as r.NextX / r.LineHeight.
//   - s.NextY is the global max extent, not the current-line start.
//   - Because of that, the current line's Y is derived as s.NextY − s.LineHeight
//     (zero when the line is fresh, because s.LineHeight == 0).
//   - On wrap, s.NextX and s.LineHeight reset; s.NextY is already correct and
//     does not need to be advanced (it already accounts for the previous line).
//
// Explicit x/y override the cursor entirely; the max-extent invariant on s.NextY
// is still maintained so subsequent auto-placed panels don't overlap.
func placePanel(s *DashboardSession, r *RowEntry, ptype string, wOpt, hOpt, xOpt, yOpt *int) dashboard.GridPos {
	w := uint32(24)
	if wOpt != nil {
		w = uint32(*wOpt)
	} else if ptype == "stat" || ptype == "gauge" {
		w = 6
	}

	h := uint32(8)
	if hOpt != nil {
		h = uint32(*hOpt)
	} else if ptype == "stat" || ptype == "gauge" {
		h = 4
	}

	if r != nil {
		return placePanelInRow(s, r, w, h, xOpt, yOpt)
	}
	return placePanelInDashboard(s, w, h, xOpt, yOpt)
}

func placePanelInRow(s *DashboardSession, r *RowEntry, w, h uint32, xOpt, yOpt *int) dashboard.GridPos {
	var x, y uint32
	if r.NextY == 0 {
		r.NextY = s.NextY
	}
	if xOpt != nil {
		x = uint32(*xOpt)
	} else {
		if r.NextX+w > 24 {
			if r.LineHeight == 0 {
				r.LineHeight = h
			}
			r.NextY += r.LineHeight
			r.NextX = 0
			r.LineHeight = 0
		}
		x = r.NextX
		r.NextX += w
		if h > r.LineHeight {
			r.LineHeight = h
		}
	}

	if yOpt != nil {
		y = uint32(*yOpt)
	} else {
		y = r.NextY
	}

	if y+h > s.NextY {
		s.NextY = y + h
	}

	return dashboard.GridPos{
		W: w,
		H: h,
		X: x,
		Y: y,
	}
}

func placePanelInDashboard(s *DashboardSession, w, h uint32, xOpt, yOpt *int) dashboard.GridPos {
	var x, y uint32
	if xOpt != nil {
		x = uint32(*xOpt)
	} else {
		if s.NextX+w > 24 {
			// s.NextY already reflects the max extent of the current line.
			// Just reset the per-line counters to start a new line there.
			s.NextX = 0
			s.LineHeight = 0
		}
		x = s.NextX
		s.NextX += w
	}

	// Compute Y before updating LineHeight to avoid using the new height.
	switch {
	case yOpt != nil:
		y = uint32(*yOpt)
	case s.LineHeight > 0:
		// Current line Y = NextY minus the height already recorded for this line.
		y = s.NextY - s.LineHeight
	default:
		// Fresh line: start at the current max extent.
		y = s.NextY
	}

	if h > s.LineHeight {
		s.LineHeight = h
	}
	if y+h > s.NextY {
		s.NextY = y + h
	}

	return dashboard.GridPos{
		W: w,
		H: h,
		X: x,
		Y: y,
	}
}

func addPanelHandler(sm *SessionManager) mcp.ToolHandlerFor[AddPanelReq, AddPanelRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddPanelReq) (*mcp.CallToolResult, AddPanelRes, error) {
		var gridPos dashboard.GridPos
		panelID := uuid.New().String()

		if len(args.ReduceCalcs) > 0 {
			switch args.Type {
			case "timeseries", "stat", "gauge", "table":
			default:
				return nil, AddPanelRes{}, fmt.Errorf("reduce_calcs is not supported for panel type %q", args.Type)
			}
		}

		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			var r *RowEntry
			if args.RowID != "" {
				r = s.findRow(args.RowID)
				if r == nil {
					return fmt.Errorf("row_id %s not found in dashboard", args.RowID)
				}
			}

			gridPos = placePanel(s, r, args.Type, args.W, args.H, args.X, args.Y)

			panel := &PanelEntry{
				ID:                panelID,
				Title:             args.Title,
				Description:       args.Description,
				Type:              args.Type,
				GridPos:           gridPos,
				Unit:              args.Unit,
				Decimals:          args.Decimals,
				ReduceCalcs:       args.ReduceCalcs,
				FillOpacity:       args.FillOpacity,
				LineWidth:         args.LineWidth,
				Stacking:          args.Stacking,
				AxisSoftMin:       args.AxisSoftMin,
				AxisSoftMax:       args.AxisSoftMax,
				GaugeMin:          args.GaugeMin,
				GaugeMax:          args.GaugeMax,
				LegendDisplayMode: args.LegendDisplayMode,
				LegendPlacement:   args.LegendPlacement,
			}

			if args.Type == "stat" || args.Type == "gauge" {
				panel.Thresholds = []dashboard.Threshold{{
					Value: nil,
					Color: "green",
				}}
			}

			if r != nil {
				r.Panels = append(r.Panels, panel)
			} else {
				s.Panels = append(s.Panels, panel)
			}
			return nil
		})
		if err != nil {
			return nil, AddPanelRes{}, err
		}
		return nil, AddPanelRes{PanelID: panelID, GridPos: gridPos}, nil
	}
}

func addPanelsBatchHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[AddPanelsBatchReq, AddPanelsBatchRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args AddPanelsBatchReq) (*mcp.CallToolResult, AddPanelsBatchRes, error) {
		dsUIDs := make(map[string]bool)
		for _, ps := range args.Panels {
			for _, q := range ps.Queries {
				if q.DatasourceUID != "" && q.DatasourceType == "" {
					dsUIDs[q.DatasourceUID] = true
				}
			}
		}

		dsTypes := make(map[string]string)
		for uid := range dsUIDs {
			dsType := "prometheus"
			if gc != nil {
				info, err := gc.GetDatasourceByUID(ctx, uid)
				if err == nil && info != nil {
					dsType = info.Type
				}
			}
			dsTypes[uid] = dsType
		}

		var panelIDs []string
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			var r *RowEntry
			if args.RowID != "" {
				r = s.findRow(args.RowID)
				if r == nil {
					return fmt.Errorf("row_id %s not found in dashboard", args.RowID)
				}
			}

			for _, ps := range args.Panels {
				gridPos := placePanel(s, r, ps.Type, ps.W, ps.H, ps.X, ps.Y)

				panelID := uuid.New().String()
				panel := &PanelEntry{
					ID:                panelID,
					Title:             ps.Title,
					Description:       ps.Description,
					Type:              ps.Type,
					GridPos:           gridPos,
					Unit:              ps.Unit,
					Decimals:          ps.Decimals,
					ReduceCalcs:       ps.ReduceCalcs,
					FillOpacity:       ps.FillOpacity,
					LineWidth:         ps.LineWidth,
					Stacking:          ps.Stacking,
					AxisSoftMin:       ps.AxisSoftMin,
					AxisSoftMax:       ps.AxisSoftMax,
					GaugeMin:          ps.GaugeMin,
					GaugeMax:          ps.GaugeMax,
					LegendDisplayMode: ps.LegendDisplayMode,
					LegendPlacement:   ps.LegendPlacement,
				}

				for idx, q := range ps.Queries {
					dsType := q.DatasourceType
					if dsType == "" {
						var ok bool
						dsType, ok = dsTypes[q.DatasourceUID]
						if !ok {
							dsType = "prometheus"
						}
					}
					refID := queryRefID(idx)
					panel.Queries = append(panel.Queries, QueryEntry{
						RefID:          refID,
						DatasourceUID:  q.DatasourceUID,
						DatasourceType: dsType,
						Expr:           q.Expr,
						LegendFormat:   q.LegendFormat,
						Instant:        q.Instant,
						Format:         q.Format,
						Hide:           q.Hide,
					})
				}

				if len(ps.Thresholds) > 0 {
					for _, t := range ps.Thresholds {
						panel.Thresholds = append(panel.Thresholds, dashboard.Threshold{
							Value: t.Value,
							Color: t.Color,
						})
					}
				} else if ps.Type == "stat" || ps.Type == "gauge" {
					panel.Thresholds = []dashboard.Threshold{{
						Value: nil,
						Color: "green",
					}}
				}

				if r != nil {
					r.Panels = append(r.Panels, panel)
				} else {
					s.Panels = append(s.Panels, panel)
				}
				panelIDs = append(panelIDs, panelID)
			}
			return nil
		})
		if err != nil {
			return nil, AddPanelsBatchRes{}, err
		}
		return nil, AddPanelsBatchRes{PanelIDs: panelIDs}, nil
	}
}

type UpdatePanelReq struct {
	DashboardID string   `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string   `json:"panel_id" jsonschema:"The ID of the panel"`
	Title       string   `json:"title,omitempty" jsonschema:"Optional new title"`
	Description string   `json:"description,omitempty" jsonschema:"Optional new description"`
	Type        string   `json:"type,omitempty" jsonschema:"Optional new panel type (e.g. timeseries, stat, gauge, table). Changing type resets reduce_calcs to empty."`
	Unit        string   `json:"unit,omitempty" jsonschema:"Optional unit (e.g. short, percent, bytes)"`
	Decimals    *float64 `json:"decimals,omitempty" jsonschema:"Optional decimal precision"`
	ReduceCalcs []string `json:"reduce_calcs,omitempty" jsonschema:"Optional calculation/reducers for stat/gauge panels (e.g. mean, lastNotNull, max)"`
	// timeseries visual
	FillOpacity *float64 `json:"fill_opacity,omitempty" jsonschema:"Optional fill opacity (0-100) for timeseries"`
	LineWidth   *float64 `json:"line_width,omitempty" jsonschema:"Optional line width for timeseries"`
	Stacking    string   `json:"stacking,omitempty" jsonschema:"Stacking mode: none, normal, percent"`
	AxisSoftMin *float64 `json:"axis_soft_min,omitempty" jsonschema:"Optional soft min for Y axis"`
	AxisSoftMax *float64 `json:"axis_soft_max,omitempty" jsonschema:"Optional soft max for Y axis"`
	// gauge field-level bounds
	GaugeMin *float64 `json:"gauge_min,omitempty" jsonschema:"Optional min for gauge"`
	GaugeMax *float64 `json:"gauge_max,omitempty" jsonschema:"Optional max for gauge"`
	// legend (visual effect only on timeseries; stored for stat/gauge/table but ignored at export time)
	LegendDisplayMode string `json:"legend_display_mode,omitempty" jsonschema:"Legend display: list, table, hidden (visual effect only on timeseries)"`
	LegendPlacement   string `json:"legend_placement,omitempty" jsonschema:"Legend placement: bottom, right (visual effect only on timeseries)"`
}

func updatePanelHandler(sm *SessionManager) mcp.ToolHandlerFor[UpdatePanelReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args UpdatePanelReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, _, _ := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}

			if args.Title != "" {
				p.Title = args.Title
			}
			if args.Description != "" {
				p.Description = args.Description
			}
			if args.Type != "" && args.Type != p.Type {
				p.Type = args.Type
				p.ReduceCalcs = nil
			}
			if args.Unit != "" {
				p.Unit = args.Unit
			}
			if args.Decimals != nil {
				p.Decimals = args.Decimals
			}
			if args.ReduceCalcs != nil {
				p.ReduceCalcs = args.ReduceCalcs
			}
			if args.FillOpacity != nil {
				p.FillOpacity = args.FillOpacity
			}
			if args.LineWidth != nil {
				p.LineWidth = args.LineWidth
			}
			if args.Stacking != "" {
				p.Stacking = args.Stacking
			}
			if args.AxisSoftMin != nil {
				p.AxisSoftMin = args.AxisSoftMin
			}
			if args.AxisSoftMax != nil {
				p.AxisSoftMax = args.AxisSoftMax
			}
			if args.GaugeMin != nil {
				p.GaugeMin = args.GaugeMin
			}
			if args.GaugeMax != nil {
				p.GaugeMax = args.GaugeMax
			}
			if args.LegendDisplayMode != "" {
				p.LegendDisplayMode = args.LegendDisplayMode
			}
			if args.LegendPlacement != "" {
				p.LegendPlacement = args.LegendPlacement
			}
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type DeletePanelReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string `json:"panel_id" jsonschema:"The ID of the panel"`
}

func deletePanelHandler(sm *SessionManager) mcp.ToolHandlerFor[DeletePanelReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args DeletePanelReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, r, idx := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}

			if r != nil {
				r.Panels = append(r.Panels[:idx], r.Panels[idx+1:]...)
			} else {
				s.Panels = append(s.Panels[:idx], s.Panels[idx+1:]...)
			}
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type AddThresholdReq struct {
	DashboardID string  `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string  `json:"panel_id" jsonschema:"The ID of the panel"`
	Value       float64 `json:"value" jsonschema:"The threshold value"`
	Color       string  `json:"color" jsonschema:"The color for the threshold"`
}

func addThresholdHandler(sm *SessionManager) mcp.ToolHandlerFor[AddThresholdReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddThresholdReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, _, _ := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}
			val := args.Value
			p.Thresholds = append(p.Thresholds, dashboard.Threshold{
				Value: &val,
				Color: args.Color,
			})
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type MovePanelReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string `json:"panel_id" jsonschema:"The ID of the panel to move"`
	RowID       string `json:"row_id,omitempty" jsonschema:"Target row ID. Empty string moves the panel to dashboard top-level."`
	X           *int   `json:"x,omitempty" jsonschema:"Optional explicit X position in the destination. Omit to use auto-layout."`
	Y           *int   `json:"y,omitempty" jsonschema:"Optional explicit Y position in the destination. Omit to use auto-layout."`
}

func movePanelHandler(sm *SessionManager) mcp.ToolHandlerFor[MovePanelReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args MovePanelReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, srcRow, idx := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}

			// Determine destination row (nil = top-level).
			var dstRow *RowEntry
			if args.RowID != "" {
				dstRow = s.findRow(args.RowID)
				if dstRow == nil {
					return fmt.Errorf("row_id %s not found", args.RowID)
				}
			}

			// Don't move to the same container.
			if srcRow == dstRow {
				return nil
			}

			// Remove from source.
			if srcRow != nil {
				srcRow.Panels = append(srcRow.Panels[:idx], srcRow.Panels[idx+1:]...)
			} else {
				s.Panels = append(s.Panels[:idx], s.Panels[idx+1:]...)
			}

			// Compute new grid position using auto-layout (or explicit coords).
			wOpt := new(int(p.GridPos.W))
			hOpt := new(int(p.GridPos.H))
			p.GridPos = placePanel(s, dstRow, p.Type, wOpt, hOpt, args.X, args.Y)

			// Append to destination.
			if dstRow != nil {
				dstRow.Panels = append(dstRow.Panels, p)
			} else {
				s.Panels = append(s.Panels, p)
			}
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}
