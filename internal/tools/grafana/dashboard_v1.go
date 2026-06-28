package grafana

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/gauge"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/table"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"
)

func buildDashboardJSON(s *DashboardSession, version string) ([]byte, error) {
	switch version {
	case dashboardVersionV1:
		return buildDashboardJSONV1(s)
	case dashboardVersionV2:
		return buildDashboardJSONV2(s)
	default:
		return nil, fmt.Errorf("unsupported dashboard version %q", version)
	}
}

func buildDashboardJSONV1(s *DashboardSession) ([]byte, error) {
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
	if s.Description != "" {
		dbBuilder.Description(s.Description)
	} else if s.Model != "" {
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
	if s.Refresh != "" {
		dbBuilder.Refresh(s.Refresh)
	}
	if s.Tooltip != 0 {
		dbBuilder.Tooltip(dashboard.DashboardCursorSync(s.Tooltip))
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
				vb.Sort(dashboard.VariableSort(v.Sort))
			}
			vb.Refresh(dashboard.VariableRefresh(1))
			dbBuilder.WithVariable(vb)

		case "custom":
			vb := dashboard.NewCustomVariableBuilder(v.Name)
			if v.Query != "" {
				vb.Values(dashboard.StringOrMap{String: new(v.Query)})
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
			dbBuilder.WithVariable(vb)

		case "datasource":
			vb := dashboard.NewDatasourceVariableBuilder(v.Name)
			if v.Query != "" {
				vb.Type(v.Query)
			}
			dbBuilder.WithVariable(vb)
		}
	}

	// Add rows. Recompute Y positions before building so that rows created
	// before their panels are placed correctly (row headers would otherwise
	// stack at consecutive Y values instead of following panel content).
	for _, r := range recomputeRowPositions(s.Rows) {
		rowBuilder := dashboard.NewRowBuilder(r.Title).
			Collapsed(r.Collapsed).
			GridPos(dashboard.GridPos{Y: r.Y, W: 24, H: 1})
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
		return nil, fmt.Errorf("building v1 dashboard: %w", err)
	}
	dashboardJSON, err := json.MarshalIndent(dashboardObj, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling v1 dashboard: %w", err)
	}
	return dashboardJSON, nil
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
		if q.Instant {
			dq.Instant()
		}
		if q.Format != "" {
			dq.Format(prometheus.PromQueryFormat(q.Format))
		}
		if q.Hide {
			dq.Hide(true)
		}
		targets = append(targets, dq)
	}

	var thresholdsConfig cog.Builder[dashboard.ThresholdsConfig]
	if len(p.Thresholds) > 0 {
		slices.SortFunc(p.Thresholds, func(a, b dashboard.Threshold) int {
			if a.Value == nil {
				if b.Value == nil {
					return 0
				}
				return -1
			}
			if b.Value == nil {
				return 1
			}
			return cmp.Compare(*a.Value, *b.Value)
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
		if p.FillOpacity != nil {
			pb.FillOpacity(*p.FillOpacity)
		}
		if p.LineWidth != nil {
			pb.LineWidth(*p.LineWidth)
		}
		if p.Stacking != "" {
			pb.Stacking(common.NewStackingConfigBuilder().Mode(common.StackingMode(p.Stacking)))
		}
		if p.AxisSoftMin != nil {
			pb.AxisSoftMin(*p.AxisSoftMin)
		}
		if p.AxisSoftMax != nil {
			pb.AxisSoftMax(*p.AxisSoftMax)
		}
		legend := buildLegend(p)
		if legend != nil {
			pb.Legend(legend)
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
		ro := buildReduceOptions(p)
		if ro != nil {
			pb.ReduceOptions(ro)
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
		if p.GaugeMin != nil {
			pb.Min(*p.GaugeMin)
		}
		if p.GaugeMax != nil {
			pb.Max(*p.GaugeMax)
		}
		ro := buildReduceOptions(p)
		if ro != nil {
			pb.ReduceOptions(ro)
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

func buildLegend(p *PanelEntry) *common.VizLegendOptionsBuilder {
	if p.LegendDisplayMode == "" && p.LegendPlacement == "" && len(p.ReduceCalcs) == 0 {
		return nil
	}
	lb := common.NewVizLegendOptionsBuilder()
	if p.LegendDisplayMode != "" {
		lb.DisplayMode(common.LegendDisplayMode(p.LegendDisplayMode))
	}
	if p.LegendPlacement != "" {
		lb.Placement(common.LegendPlacement(p.LegendPlacement))
	}
	if len(p.ReduceCalcs) > 0 {
		lb.Calcs(p.ReduceCalcs)
	}
	return lb
}
