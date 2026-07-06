package alertmanager

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// ValidatePromQLQueryReq is the input to validate_promql_query.
type ValidatePromQLQueryReq struct {
	Expr string `json:"expr" jsonschema:"The PromQL expression to validate"`
}

// ValidatePromQLQueryRes is the response from validate_promql_query.
type ValidatePromQLQueryRes struct {
	Valid       bool     `json:"valid"`
	MetricNames []string `json:"metric_names,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// EvaluatePromQLQueryReq is the input to evaluate_promql_query.
type EvaluatePromQLQueryReq struct {
	Expr  string `json:"expr" jsonschema:"The PromQL expression to evaluate"`
	Time  string `json:"time,omitempty" jsonschema:"RFC3339 timestamp for an instant query; defaults to now. Ignored if start/end are set."`
	Start string `json:"start,omitempty" jsonschema:"RFC3339 start time for a range query"`
	End   string `json:"end,omitempty" jsonschema:"RFC3339 end time for a range query"`
	Step  string `json:"step,omitempty" jsonschema:"Step duration for a range query, e.g. 30s, 1m (default 1m)"`
}

// PromQLValueStats contains statistics about a query result.
type PromQLValueStats struct {
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Mean     float64 `json:"mean"`
	LastMean float64 `json:"last_mean"`
}

// EvaluatePromQLQueryRes is the response from evaluate_promql_query.
type EvaluatePromQLQueryRes struct {
	ResultType  string            `json:"result_type,omitempty"` // vector, matrix, scalar, string
	SeriesCount int               `json:"series_count,omitempty"`
	Values      *PromQLValueStats `json:"values,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
	Warning     string            `json:"warning,omitempty"` // for non-fatal notes, e.g. "no data"
}

func validatePromQLQueryHandler() mcp.ToolHandlerFor[ValidatePromQLQueryReq, ValidatePromQLQueryRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args ValidatePromQLQueryReq) (*mcp.CallToolResult, ValidatePromQLQueryRes, error) {
		e, err := metricsql.Parse(args.Expr)
		if err != nil {
			return nil, ValidatePromQLQueryRes{Valid: false, Error: err.Error()}, nil
		}

		metricNames := make(map[string]bool)
		metricsql.VisitAll(e, func(node metricsql.Expr) {
			me, ok := node.(*metricsql.MetricExpr)
			if !ok || len(me.LabelFilterss) == 0 {
				return
			}
			for _, f := range me.LabelFilterss[0] {
				if f.Label == "__name__" && !f.IsNegative && !f.IsRegexp {
					metricNames[f.Value] = true
				}
			}
		})

		names := make([]string, 0, len(metricNames))
		for name := range metricNames {
			names = append(names, name)
		}
		sort.Strings(names)

		return nil, ValidatePromQLQueryRes{Valid: true, MetricNames: names}, nil
	}
}

func parseTimeOrNow(s string) (time.Time, error) {
	if s == "" {
		return time.Now(), nil
	}
	return time.Parse(time.RFC3339, s)
}

func computeStats(values []float64) *PromQLValueStats {
	if len(values) == 0 {
		return nil
	}
	var lo, hi, sum float64
	count := 0
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		if count == 0 {
			lo = v
			hi = v
		} else {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		sum += v
		count++
	}
	if count == 0 {
		return nil
	}
	return &PromQLValueStats{
		Min:  lo,
		Max:  hi,
		Mean: sum / float64(count),
	}
}

func evaluatePromQLQueryHandler(c *Client) mcp.ToolHandlerFor[EvaluatePromQLQueryReq, EvaluatePromQLQueryRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args EvaluatePromQLQueryReq) (*mcp.CallToolResult, EvaluatePromQLQueryRes, error) {
		if !c.HasPrometheus() {
			return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("no Prometheus URL configured; evaluate_promql_query requires -prometheus-url (or PROMETHEUS_URL) to be set")
		}

		// Validate PromQL syntax first
		if _, err := metricsql.Parse(args.Expr); err != nil {
			return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("invalid PromQL: %w", err)
		}

		var (
			val      model.Value
			warnings promv1.Warnings
			queryErr error
		)

		if args.Start != "" && args.End != "" {
			// Range query
			start, err := time.Parse(time.RFC3339, args.Start)
			if err != nil {
				return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("invalid start time: %w", err)
			}
			end, err := time.Parse(time.RFC3339, args.End)
			if err != nil {
				return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("invalid end time: %w", err)
			}
			step := 1 * time.Minute
			if args.Step != "" {
				step, err = time.ParseDuration(args.Step)
				if err != nil {
					return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("invalid step: %w", err)
				}
			}
			val, warnings, queryErr = c.Prometheus().QueryRange(ctx, args.Expr, promv1.Range{Start: start, End: end, Step: step})
		} else {
			// Instant query
			ts, err := parseTimeOrNow(args.Time)
			if err != nil {
				return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("invalid time: %w", err)
			}
			val, warnings, queryErr = c.Prometheus().Query(ctx, args.Expr, ts)
		}

		if queryErr != nil {
			return nil, EvaluatePromQLQueryRes{}, fmt.Errorf("query prometheus: %w", queryErr)
		}

		result := EvaluatePromQLQueryRes{
			ResultType: val.Type().String(),
		}

		if len(warnings) > 0 {
			result.Warnings = []string(warnings)
		}

		switch v := val.(type) {
		case model.Matrix:
			result.SeriesCount = len(v)
			if len(v) == 0 {
				result.Warning = "no data"
				return nil, result, nil
			}

			// Collect all sample values for min/max/mean
			var allValues []float64
			var lastValues []float64

			for _, series := range v {
				if len(series.Values) == 0 {
					continue
				}
				for _, sample := range series.Values {
					if !math.IsNaN(float64(sample.Value)) {
						allValues = append(allValues, float64(sample.Value))
					}
				}
				// Last value of each series for LastMean
				lastVal := float64(series.Values[len(series.Values)-1].Value)
				if !math.IsNaN(lastVal) {
					lastValues = append(lastValues, lastVal)
				}
			}

			if stats := computeStats(allValues); stats != nil {
				result.Values = stats
				if lastStats := computeStats(lastValues); lastStats != nil {
					result.Values.LastMean = lastStats.Mean
				}
			}

		case model.Vector:
			result.SeriesCount = len(v)
			if len(v) == 0 {
				result.Warning = "no data"
				return nil, result, nil
			}

			var values []float64
			for _, sample := range v {
				if !math.IsNaN(float64(sample.Value)) {
					values = append(values, float64(sample.Value))
				}
			}

			if stats := computeStats(values); stats != nil {
				result.Values = stats
				result.Values.LastMean = stats.Mean
			}

		case *model.Scalar:
			result.SeriesCount = 1
			val := float64(v.Value)
			if !math.IsNaN(val) {
				result.Values = &PromQLValueStats{
					Min:      val,
					Max:      val,
					Mean:     val,
					LastMean: val,
				}
			}

		default:
			result.Warning = "unsupported result type for summary"
		}

		return nil, result, nil
	}
}

func registerPromQLTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "validate_promql_query",
		Description: "Validates PromQL syntax offline (no network call) and lists the metric names referenced in the expression.",
		Flags:       mcputil.ReadOnly,
	}, validatePromQLQueryHandler())

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "evaluate_promql_query",
		Description: "Runs a PromQL instant or range query against the configured Prometheus and returns a compact statistical summary (series count, min/max/mean) instead of raw samples, to keep context usage low. Requires a Prometheus URL to be configured; errors clearly if not.",
		Flags:       mcputil.ReadOnly,
	}, evaluatePromQLQueryHandler(c))
}
