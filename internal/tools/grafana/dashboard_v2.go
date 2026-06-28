package grafana

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/dashboardv2"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
)

func buildDashboardJSONV2(s *DashboardSession) ([]byte, error) {
	dbBuilder := dashboardv2.NewDashboardBuilder(s.Title)
	tags := s.Tags
	if s.Model != "" {
		tags = append(tags, "created-by:"+s.Model)
	}
	if len(tags) > 0 {
		dbBuilder.Tags(tags)
	}
	if s.Description != "" {
		dbBuilder.Description(s.Description)
	} else if s.Model != "" {
		dbBuilder.Description("Created by " + s.Model)
	}
	if s.Tooltip != 0 {
		dbBuilder.CursorSync(dashboardCursorSyncV2(s.Tooltip))
	}
	if s.TimeFrom != "" || s.TimeTo != "" || s.Refresh != "" {
		from := s.TimeFrom
		if from == "" {
			from = "now-6h"
		}
		to := s.TimeTo
		if to == "" {
			to = "now"
		}
		timeSettings := dashboardv2.NewTimeSettingsBuilder().From(from).To(to)
		if s.Refresh != "" {
			timeSettings.AutoRefresh(s.Refresh)
		}
		dbBuilder.TimeSettings(timeSettings)
	}

	for _, v := range s.Variables {
		switch v.Type {
		case "query":
			vb := dashboardv2.NewQueryVariableBuilder(v.Name).
				Refresh(dashboardv2.VariableRefreshOnDashboardLoad)
			if v.Query != "" {
				vb.Definition(v.Query)
				vb.Query(v2DataQueryBuilder{
					Expr:           v.Query,
					DatasourceUID:  v.DatasourceUID,
					DatasourceType: v.DatasourceType,
				})
			}
			if v.Label != "" {
				vb.Label(v.Label)
			}
			if v.Multi {
				vb.Multi(v.Multi)
			}
			if v.IncludeAll {
				vb.IncludeAll(v.IncludeAll)
			}
			if v.Regex != "" {
				vb.Regex(v.Regex)
			}
			if v.Sort != 0 {
				vb.Sort(variableSortV2(v.Sort))
			}
			dbBuilder.QueryVariable(vb)
		case "custom":
			vb := dashboardv2.NewCustomVariableBuilder(v.Name)
			if v.Query != "" {
				vb.Query(v.Query)
			}
			if v.Label != "" {
				vb.Label(v.Label)
			}
			if v.Multi {
				vb.Multi(v.Multi)
			}
			if v.IncludeAll {
				vb.IncludeAll(v.IncludeAll)
			}
			dbBuilder.CustomVariable(vb)
		case "datasource":
			vb := dashboardv2.NewDatasourceVariableBuilder(v.Name)
			if v.Query != "" {
				vb.PluginId(v.Query)
			}
			dbBuilder.DatasourceVariable(vb)
		}
	}

	grid := dashboardv2.NewGridBuilder()
	for _, r := range recomputeRowPositions(s.Rows) {
		for _, p := range r.Panels {
			key := panelElementKey(p)
			dbBuilder.Panel(key, buildPanelV2(p))
			grid.Item(gridItemForPanel(key, p))
		}
	}
	for _, p := range s.Panels {
		key := panelElementKey(p)
		dbBuilder.Panel(key, buildPanelV2(p))
		grid.Item(gridItemForPanel(key, p))
	}
	dbBuilder.GridLayout(grid)

	name := s.UID
	if name == "" {
		name = s.DashboardID
	}
	manifest, err := dashboardv2.Manifest(name, dbBuilder).Build()
	if err != nil {
		return nil, fmt.Errorf("building v2 dashboard: %w", err)
	}
	dashboardJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling v2 dashboard: %w", err)
	}
	return dashboardJSON, nil
}

func buildPanelV2(p *PanelEntry) cog.Builder[dashboardv2.PanelKind] {
	queryGroup := dashboardv2.NewQueryGroupBuilder()
	for _, q := range p.Queries {
		queryGroup.Target(dashboardv2.NewTargetBuilder().
			RefId(q.RefID).
			Hidden(q.Hide).
			Query(v2DataQueryBuilder{
				Expr:           q.Expr,
				DatasourceUID:  q.DatasourceUID,
				DatasourceType: q.DatasourceType,
				LegendFormat:   q.LegendFormat,
				Instant:        q.Instant,
				Format:         q.Format,
			}))
	}

	fieldConfig := *dashboardv2.NewFieldConfigSource()
	if p.Unit != "" {
		fieldConfig.Defaults.Unit = &p.Unit
	}
	if p.Decimals != nil {
		fieldConfig.Defaults.Decimals = p.Decimals
	}
	if p.GaugeMin != nil {
		fieldConfig.Defaults.Min = p.GaugeMin
	}
	if p.GaugeMax != nil {
		fieldConfig.Defaults.Max = p.GaugeMax
	}
	if len(p.Thresholds) > 0 {
		thresholds := make([]dashboardv2.Threshold, 0, len(p.Thresholds))
		for _, threshold := range p.Thresholds {
			thresholds = append(thresholds, dashboardv2.Threshold{
				Value: threshold.Value,
				Color: threshold.Color,
			})
		}
		fieldConfig.Defaults.Thresholds = &dashboardv2.ThresholdsConfig{
			Mode:  dashboardv2.ThresholdsModeAbsolute,
			Steps: thresholds,
		}
	}

	options := map[string]any{}
	if len(p.ReduceCalcs) > 0 {
		options["reduceOptions"] = map[string]any{"calcs": p.ReduceCalcs}
	}
	if p.FillOpacity != nil {
		options["fillOpacity"] = *p.FillOpacity
	}
	if p.LineWidth != nil {
		options["lineWidth"] = *p.LineWidth
	}
	if p.Stacking != "" {
		options["stacking"] = map[string]any{"mode": p.Stacking}
	}
	if p.LegendDisplayMode != "" || p.LegendPlacement != "" {
		legend := map[string]any{}
		if p.LegendDisplayMode != "" {
			legend["displayMode"] = p.LegendDisplayMode
		}
		if p.LegendPlacement != "" {
			legend["placement"] = p.LegendPlacement
		}
		options["legend"] = legend
	}

	viz := dashboardv2.NewVizConfigKindBuilder().
		Group(p.Type).
		Version("v0").
		Options(options).
		FieldConfig(fieldConfig)
	panel := dashboardv2.NewPanelBuilder().
		Title(p.Title).
		Data(queryGroup).
		Visualization(viz)
	if p.Description != "" {
		panel.Description(p.Description)
	}
	return panel
}

func panelElementKey(p *PanelEntry) string {
	if p.ID != "" {
		return "panel-" + p.ID
	}
	return "panel-" + strings.ToLower(strings.ReplaceAll(p.Title, " ", "-"))
}

func gridItemForPanel(key string, p *PanelEntry) cog.Builder[dashboardv2.GridLayoutItemKind] {
	return dashboardv2.GridItem(key).
		X(int64(p.GridPos.X)).
		Y(int64(p.GridPos.Y)).
		Width(int64(p.GridPos.W)).
		Height(int64(p.GridPos.H))
}

func dashboardCursorSyncV2(tooltip int) dashboardv2.DashboardCursorSync {
	switch tooltip {
	case 1:
		return dashboardv2.DashboardCursorSyncCrosshair
	case 2:
		return dashboardv2.DashboardCursorSyncTooltip
	default:
		return dashboardv2.DashboardCursorSyncOff
	}
}

func variableSortV2(sort int) dashboardv2.VariableSort {
	switch sort {
	case 1:
		return dashboardv2.VariableSortAlphabeticalAsc
	case 2:
		return dashboardv2.VariableSortAlphabeticalDesc
	case 3:
		return dashboardv2.VariableSortNumericalAsc
	case 4:
		return dashboardv2.VariableSortNumericalDesc
	case 5:
		return dashboardv2.VariableSortAlphabeticalCaseInsensitiveAsc
	case 6:
		return dashboardv2.VariableSortAlphabeticalCaseInsensitiveDesc
	default:
		return dashboardv2.VariableSortDisabled
	}
}

type v2DataQueryBuilder struct {
	Expr           string
	DatasourceUID  string
	DatasourceType string
	LegendFormat   string
	Instant        bool
	Format         string
}

func (b v2DataQueryBuilder) Build() (dashboardv2.DataQueryKind, error) {
	dsType := b.DatasourceType
	if dsType == "" {
		dsType = "prometheus"
	}
	dq := prometheus.NewDataqueryBuilder().Expr(b.Expr)
	if b.LegendFormat != "" {
		dq.LegendFormat(b.LegendFormat)
	}
	if b.Instant {
		dq.Instant()
	}
	if b.Format != "" {
		dq.Format(prometheus.PromQueryFormat(b.Format))
	}
	spec, err := dq.Build()
	if err != nil {
		return dashboardv2.DataQueryKind{}, err
	}
	query := dashboardv2.DataQueryKind{
		Kind:    "DataQuery",
		Group:   dsType,
		Version: "v0",
		Spec:    spec,
	}
	if b.DatasourceUID != "" {
		query.Datasource = &dashboardv2.Dashboardv2DataQueryKindDatasource{Name: &b.DatasourceUID}
	}
	return query, nil
}
