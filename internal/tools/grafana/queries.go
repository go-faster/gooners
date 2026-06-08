package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type QuerySpec struct {
	DatasourceUID  string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional type of the datasource (e.g. prometheus, loki)"`
	Expr           string `json:"expr" jsonschema:"The query expression"`
	LegendFormat   string `json:"legend_format,omitempty" jsonschema:"Optional legend format"`
}

type AddQueryReq struct {
	DashboardID    string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID        string `json:"panel_id" jsonschema:"The ID of the panel"`
	DatasourceUID  string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional type of the datasource (e.g. prometheus, loki)"`
	Expr           string `json:"expr" jsonschema:"The query expression"`
	LegendFormat   string `json:"legend_format,omitempty" jsonschema:"Optional legend format"`
}

type AddQueryRes struct {
	QueryRef      string `json:"query_ref"`
	SuggestedUnit string `json:"suggested_unit,omitempty"`
}

// extractMetricName parses expr as MetricsQL/PromQL and returns the first
// metric name found in the AST. Returns "" on parse failure or when no metric
// name is present (e.g. a pure scalar expression).
func extractMetricName(expr string) string {
	e, err := metricsql.Parse(expr)
	if err != nil {
		return ""
	}
	var name string
	metricsql.VisitAll(e, func(node metricsql.Expr) {
		if name != "" {
			return
		}
		me, ok := node.(*metricsql.MetricExpr)
		if !ok || len(me.LabelFilterss) == 0 {
			return
		}
		for _, f := range me.LabelFilterss[0] {
			if f.Label == "__name__" && !f.IsNegative && !f.IsRegexp {
				name = f.Value
				return
			}
		}
	})
	return name
}

func suggestUnit(metricName, promUnit, _ string) string {
	u := strings.ToLower(promUnit)
	switch u {
	case "bytes", "byte":
		return "bytes"
	case "seconds", "second", "s":
		return "s"
	case "milliseconds", "millisecond", "ms":
		return "ms"
	case "microseconds", "microsecond", "us":
		return "µs"
	case "nanoseconds", "nanosecond", "ns":
		return "ns"
	case "percent", "pct":
		return "percent"
	case "ratio":
		return "percentunit"
	}

	m := strings.ToLower(metricName)
	if strings.HasSuffix(m, "_bytes") || strings.HasSuffix(m, "_bytes_total") {
		return "bytes"
	}
	if strings.HasSuffix(m, "_seconds") || strings.HasSuffix(m, "_seconds_total") {
		return "s"
	}
	if strings.HasSuffix(m, "_percent") || strings.HasSuffix(m, "_pct") {
		return "percent"
	}
	if strings.HasSuffix(m, "_ratio") {
		return "percentunit"
	}
	if strings.HasSuffix(m, "_total") || strings.HasSuffix(m, "_count") {
		return "short"
	}

	return ""
}

func queryRefID(idx int) string {
	var s string
	for idx >= 0 {
		s = string(rune('A'+(idx%26))) + s
		idx = (idx / 26) - 1
	}
	return s
}

func addQueryHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[AddQueryReq, AddQueryRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args AddQueryReq) (*mcp.CallToolResult, AddQueryRes, error) {
		dsType := args.DatasourceType
		if dsType == "" {
			dsType = "prometheus"
			if gc != nil {
				info, err := gc.GetDatasourceByUID(ctx, args.DatasourceUID)
				if err == nil && info != nil {
					dsType = info.Type
				}
			}
		}

		var suggestedUnit string
		if dsType == "prometheus" {
			metricName := extractMetricName(args.Expr)
			if metricName != "" && gc != nil {
				rawMetadata, err := gc.LookupMetricMetadata(ctx, args.DatasourceUID, metricName)
				if err == nil {
					var metaResp struct {
						Status string `json:"status"`
						Data   map[string][]struct {
							Type string `json:"type"`
							Help string `json:"help"`
							Unit string `json:"unit"`
						} `json:"data"`
					}
					if json.Unmarshal([]byte(rawMetadata), &metaResp) == nil && metaResp.Status == "success" {
						if list, ok := metaResp.Data[metricName]; ok && len(list) > 0 {
							suggestedUnit = suggestUnit(metricName, list[0].Unit, list[0].Type)
						}
					}
				}
				if suggestedUnit == "" {
					suggestedUnit = suggestUnit(metricName, "", "")
				}
			}
		}

		var refID string
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, _, _ := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}
			refID = queryRefID(len(p.Queries))
			p.Queries = append(p.Queries, QueryEntry{
				RefID:          refID,
				DatasourceUID:  args.DatasourceUID,
				DatasourceType: dsType,
				Expr:           args.Expr,
				LegendFormat:   args.LegendFormat,
			})
			return nil
		})
		if err != nil {
			return nil, AddQueryRes{}, err
		}
		return nil, AddQueryRes{QueryRef: refID, SuggestedUnit: suggestedUnit}, nil
	}
}

type UpdateQueryReq struct {
	DashboardID    string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID        string `json:"panel_id" jsonschema:"The ID of the panel"`
	QueryRef       string `json:"query_ref" jsonschema:"The reference ID of the query (e.g. A, B, C)"`
	Expr           string `json:"expr,omitempty" jsonschema:"Optional new query expression"`
	LegendFormat   string `json:"legend_format,omitempty" jsonschema:"Optional new legend format"`
	DatasourceUID  string `json:"datasource_uid,omitempty" jsonschema:"Optional new datasource UID"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional new datasource type"`
}

func updateQueryHandler(sm *SessionManager) mcp.ToolHandlerFor[UpdateQueryReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args UpdateQueryReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, _, _ := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}
			for i := range p.Queries {
				if p.Queries[i].RefID != args.QueryRef {
					continue
				}
				if args.Expr != "" {
					p.Queries[i].Expr = args.Expr
				}
				if args.LegendFormat != "" {
					p.Queries[i].LegendFormat = args.LegendFormat
				}
				if args.DatasourceUID != "" {
					p.Queries[i].DatasourceUID = args.DatasourceUID
				}
				if args.DatasourceType != "" {
					p.Queries[i].DatasourceType = args.DatasourceType
				}
				return nil
			}
			return fmt.Errorf("query_ref %s not found on panel %s", args.QueryRef, args.PanelID)
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type DeleteQueryReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string `json:"panel_id" jsonschema:"The ID of the panel"`
	QueryRef    string `json:"query_ref" jsonschema:"The reference ID of the query to remove (e.g. A, B, C)"`
}

func deleteQueryHandler(sm *SessionManager) mcp.ToolHandlerFor[DeleteQueryReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args DeleteQueryReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			p, _, _ := s.findPanel(args.PanelID)
			if p == nil {
				return fmt.Errorf("panel_id %s not found", args.PanelID)
			}
			for i, q := range p.Queries {
				if q.RefID == args.QueryRef {
					p.Queries = append(p.Queries[:i], p.Queries[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("query_ref %s not found on panel %s", args.QueryRef, args.PanelID)
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}
