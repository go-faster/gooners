package grafana

import (
	"context"
	"fmt"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type ResolveDatasourceReq struct {
	Name string `json:"name" jsonschema:"The name of the datasource"`
}

type ResolveDatasourceRes struct {
	UID  string `json:"uid"`
	Type string `json:"type"`
}

func resolveDatasourceHandler(gc *GrafanaClient) mcp.ToolHandlerFor[ResolveDatasourceReq, ResolveDatasourceRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ResolveDatasourceReq) (*mcp.CallToolResult, ResolveDatasourceRes, error) {
		if gc == nil {
			return nil, ResolveDatasourceRes{}, fmt.Errorf("grafana client not configured")
		}
		info, err := gc.ResolveDatasource(ctx, args.Name)
		if err != nil {
			return nil, ResolveDatasourceRes{}, err
		}
		return nil, ResolveDatasourceRes{UID: info.UID, Type: info.Type}, nil
	}
}

type VerifyQueryReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Query         string `json:"query" jsonschema:"The query expression"`
	QueryType     string `json:"query_type,omitempty" jsonschema:"Query type: instant or range (default range)"`
}

func verifyQueryHandler(gc *GrafanaClient) mcp.ToolHandlerFor[VerifyQueryReq, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args VerifyQueryReq) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if gc == nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("grafana client not configured")
		}
		qType := args.QueryType
		if qType == "" {
			qType = "range"
		}
		res, err := gc.VerifyQuery(ctx, args.DatasourceUID, args.Query, qType)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}
		return nil, mcputil.CommandResult{Text: res}, nil
	}
}

type SearchMetricsReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Match         string `json:"match" jsonschema:"The match pattern for metrics"`
}

type SearchMetricsRes struct {
	Metrics []string `json:"metrics"`
}

func searchMetricsHandler(gc *GrafanaClient) mcp.ToolHandlerFor[SearchMetricsReq, SearchMetricsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args SearchMetricsReq) (*mcp.CallToolResult, SearchMetricsRes, error) {
		if gc == nil {
			return nil, SearchMetricsRes{}, fmt.Errorf("grafana client not configured")
		}
		metrics, err := gc.SearchMetrics(ctx, args.DatasourceUID, args.Match)
		if err != nil {
			return nil, SearchMetricsRes{}, err
		}
		return nil, SearchMetricsRes{Metrics: metrics}, nil
	}
}

type LookupLabelsReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Match         string `json:"match,omitempty" jsonschema:"Optional match selector e.g. {__name__=\"go_goroutines\"}"`
}

type LookupLabelsRes struct {
	Labels []string `json:"labels"`
}

func lookupLabelsHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupLabelsReq, LookupLabelsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupLabelsReq) (*mcp.CallToolResult, LookupLabelsRes, error) {
		if gc == nil {
			return nil, LookupLabelsRes{}, fmt.Errorf("grafana client not configured")
		}
		labels, err := gc.LookupLabels(ctx, args.DatasourceUID, args.Match)
		if err != nil {
			return nil, LookupLabelsRes{}, err
		}
		return nil, LookupLabelsRes{Labels: labels}, nil
	}
}

type LookupLabelValuesReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Label         string `json:"label" jsonschema:"The label name"`
	Match         string `json:"match,omitempty" jsonschema:"Optional PromQL series selector to restrict values, e.g. {job='myapp'}"`
}

type LookupLabelValuesRes struct {
	Values []string `json:"values"`
}

func lookupLabelValuesHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupLabelValuesReq, LookupLabelValuesRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupLabelValuesReq) (*mcp.CallToolResult, LookupLabelValuesRes, error) {
		if gc == nil {
			return nil, LookupLabelValuesRes{}, fmt.Errorf("grafana client not configured")
		}
		values, err := gc.LookupLabelValues(ctx, args.DatasourceUID, args.Label, args.Match)
		if err != nil {
			return nil, LookupLabelValuesRes{}, err
		}
		return nil, LookupLabelValuesRes{Values: values}, nil
	}
}

type LookupMetricMetadataReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Metric        string `json:"metric" jsonschema:"The metric name"`
}

func lookupMetricMetadataHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupMetricMetadataReq, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupMetricMetadataReq) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if gc == nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("grafana client not configured")
		}
		res, err := gc.LookupMetricMetadata(ctx, args.DatasourceUID, args.Metric)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}
		return nil, mcputil.CommandResult{Text: res}, nil
	}
}

type ParsePromQLReq struct {
	Query string `json:"query" jsonschema:"The PromQL/MetricsQL expression to parse"`
}

func parsePromQLHandler() mcp.ToolHandlerFor[ParsePromQLReq, mcputil.CommandResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args ParsePromQLReq) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		expr, err := metricsql.Parse(args.Query)
		if err != nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("parse error: %w", err)
		}
		return nil, mcputil.CommandResult{Text: string(expr.AppendString(nil))}, nil
	}
}
