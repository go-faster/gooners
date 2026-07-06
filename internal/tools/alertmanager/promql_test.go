package alertmanager

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidatePromQLQuery_Valid(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		wantMetrics []string
	}{
		{
			name:        "rate_instant",
			expr:        `rate(http_requests_total{job="api"}[5m])`,
			wantMetrics: []string{"http_requests_total"},
		},
		{
			name:        "simple_metric",
			expr:        `up`,
			wantMetrics: []string{"up"},
		},
		{
			name:        "multiple_metrics",
			expr:        `rate(http_requests_total[5m]) + rate(http_errors_total[5m])`,
			wantMetrics: []string{"http_errors_total", "http_requests_total"},
		},
		{
			name:        "aggregation",
			expr:        `sum(rate(requests_total[1m]))`,
			wantMetrics: []string{"requests_total"},
		},
		{
			name:        "metric_with_label_filters",
			expr:        `up{job="prometheus",instance="localhost:9090"}`,
			wantMetrics: []string{"up"},
		},
		{
			name:        "nested_aggregations",
			expr:        `topk(10, sort_desc(sum(rate(metric_name[5m])) by (instance)))`,
			wantMetrics: []string{"metric_name"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := validatePromQLQueryHandler()
			_, res, err := handler(context.Background(), nil, ValidatePromQLQueryReq{Expr: tt.expr})
			require.NoError(t, err)
			require.True(t, res.Valid)
			require.Empty(t, res.Error)
			require.Equal(t, tt.wantMetrics, res.MetricNames)
		})
	}
}

func TestValidatePromQLQuery_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{
			name:    "unbalanced_parens",
			expr:    `sum(http_requests_total`,
			wantErr: true,
		},
		{
			name:    "invalid_function",
			expr:    `invalid_func(metric)`,
			wantErr: true,
		},
		{
			name:    "malformed_range",
			expr:    `rate(metric[invalid])`,
			wantErr: true,
		},
		{
			name:    "empty_expression",
			expr:    ``,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := validatePromQLQueryHandler()
			_, res, _ := handler(context.Background(), nil, ValidatePromQLQueryReq{Expr: tt.expr})
			require.False(t, res.Valid)
			require.NotEmpty(t, res.Error)
		})
	}
}

func TestComputeStats_Empty(t *testing.T) {
	stats := computeStats([]float64{})
	require.Nil(t, stats)
}

func TestComputeStats_SingleValue(t *testing.T) {
	stats := computeStats([]float64{42.0})
	require.NotNil(t, stats)
	require.Equal(t, 42.0, stats.Min)
	require.Equal(t, 42.0, stats.Max)
	require.Equal(t, 42.0, stats.Mean)
}

func TestComputeStats_MultipleValues(t *testing.T) {
	stats := computeStats([]float64{1.0, 2.0, 3.0, 4.0, 5.0})
	require.NotNil(t, stats)
	require.Equal(t, 1.0, stats.Min)
	require.Equal(t, 5.0, stats.Max)
	require.Equal(t, 3.0, stats.Mean) // (1+2+3+4+5)/5
}

func TestComputeStats_WithNaN(t *testing.T) {
	values := []float64{1.0, math.NaN(), 3.0, 5.0}
	stats := computeStats(values)
	require.NotNil(t, stats)
	require.Equal(t, 1.0, stats.Min)
	require.Equal(t, 5.0, stats.Max)
	require.Equal(t, 3.0, stats.Mean) // (1+3+5)/3, NaN is skipped
}

func TestComputeStats_AllNaN(t *testing.T) {
	values := []float64{math.NaN(), math.NaN(), math.NaN()}
	stats := computeStats(values)
	require.Nil(t, stats)
}

func TestComputeStats_WithZero(t *testing.T) {
	values := []float64{0.0, 10.0, 20.0}
	stats := computeStats(values)
	require.NotNil(t, stats)
	require.Equal(t, 0.0, stats.Min)
	require.Equal(t, 20.0, stats.Max)
	require.InDelta(t, 10.0, stats.Mean, 0.001) // (0+10+20)/3
}

func TestComputeStats_NegativeValues(t *testing.T) {
	values := []float64{-5.0, -2.0, 0.0, 3.0}
	stats := computeStats(values)
	require.NotNil(t, stats)
	require.Equal(t, -5.0, stats.Min)
	require.Equal(t, 3.0, stats.Max)
	require.InDelta(t, -1.0, stats.Mean, 0.001) // (-5-2+0+3)/4
}

func TestEvaluatePromQLQuery_NoPrometheus(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	// Create client without PrometheusURL
	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	_, _, err = handler(context.Background(), nil, EvaluatePromQLQueryReq{Expr: "up"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no Prometheus URL")
}

func TestEvaluatePromQLQuery_InvalidSyntax(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: failServer.URL,
		PrometheusURL:   "http://unused.invalid",
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	_, _, err = handler(context.Background(), nil, EvaluatePromQLQueryReq{Expr: "sum(foo"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid PromQL")
}

func TestEvaluatePromQLQuery_InstantVector_HappyPath(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Return an instant vector response
		response := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{
							"__name__": "up",
							"job":      "api",
						},
						"value": []any{
							float64(1704067200),
							"1.5",
						},
					},
				},
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	_, res, err := handler(context.Background(), nil, EvaluatePromQLQueryReq{Expr: "up"})
	require.NoError(t, err)
	require.Equal(t, "vector", res.ResultType)
	require.Equal(t, 1, res.SeriesCount)
	require.NotNil(t, res.Values)
	require.InDelta(t, 1.5, res.Values.Mean, 0.001)
	require.InDelta(t, 1.5, res.Values.Min, 0.001)
	require.InDelta(t, 1.5, res.Values.Max, 0.001)
	require.InDelta(t, 1.5, res.Values.LastMean, 0.001)
}

func TestEvaluatePromQLQuery_Matrix_HappyPath(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Return a matrix response for range query
		response := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "matrix",
				"result": []map[string]any{
					{
						"metric": map[string]string{
							"job": "api",
						},
						"values": [][]any{
							{float64(1704067200), "1.0"},
							{float64(1704067260), "2.0"},
						},
					},
				},
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	start := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	end := time.Now().UTC().Format(time.RFC3339)
	_, res, err := handler(context.Background(), nil, EvaluatePromQLQueryReq{
		Expr:  `rate(http_requests_total[5m])`,
		Start: start,
		End:   end,
		Step:  "1m",
	})
	require.NoError(t, err)
	require.Equal(t, "matrix", res.ResultType)
	require.Equal(t, 1, res.SeriesCount)
	require.NotNil(t, res.Values)
	// Mean of all values: (1.0 + 2.0) / 2 = 1.5
	require.InDelta(t, 1.5, res.Values.Mean, 0.001)
	// Mean of last values: 2.0 / 1 = 2.0
	require.InDelta(t, 2.0, res.Values.LastMean, 0.001)
}

func TestEvaluatePromQLQuery_EmptyVector(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		response := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     []map[string]any{},
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	_, res, err := handler(context.Background(), nil, EvaluatePromQLQueryReq{Expr: "nonexistent_metric"})
	require.NoError(t, err)
	require.Equal(t, "vector", res.ResultType)
	require.Equal(t, 0, res.SeriesCount)
	require.Equal(t, "no data", res.Warning)
	require.Nil(t, res.Values)
}

func TestEvaluatePromQLQuery_ValidTimeFormat(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		response := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{"__name__": "up"},
						"value":  []any{float64(1704067200), "1.0"},
					},
				},
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)

	// Test with explicit time in RFC3339 format
	testTime := time.Now().UTC().Format(time.RFC3339)
	_, res, err := handler(context.Background(), nil, EvaluatePromQLQueryReq{
		Expr: "up",
		Time: testTime,
	})
	require.NoError(t, err)
	require.Equal(t, "vector", res.ResultType)
}

func TestEvaluatePromQLQuery_InvalidStartTime(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	_, _, err = handler(context.Background(), nil, EvaluatePromQLQueryReq{
		Expr:  "up",
		Start: "invalid-time",
		End:   time.Now().UTC().Format(time.RFC3339),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid start")
}

func TestEvaluatePromQLQuery_InvalidStep(t *testing.T) {
	promServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer promServer.Close()

	client, err := NewClient(Config{
		AlertmanagerURL: "http://unused.invalid",
		PrometheusURL:   promServer.URL,
	})
	require.NoError(t, err)

	handler := evaluatePromQLQueryHandler(client)
	start := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	end := time.Now().UTC().Format(time.RFC3339)
	_, _, err = handler(context.Background(), nil, EvaluatePromQLQueryReq{
		Expr:  "up",
		Start: start,
		End:   end,
		Step:  "invalid-duration",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid step")
}

// Test parseTimeOrNow helper
func TestParseTimeOrNow_Empty(t *testing.T) {
	ts, err := parseTimeOrNow("")
	require.NoError(t, err)
	// Should return something close to now (within a second)
	require.WithinDuration(t, time.Now(), ts, 1*time.Second)
}

func TestParseTimeOrNow_Valid(t *testing.T) {
	timeStr := "2024-01-01T12:00:00Z"
	ts, err := parseTimeOrNow(timeStr)
	require.NoError(t, err)
	require.Equal(t, "2024-01-01T12:00:00Z", ts.Format(time.RFC3339))
}

func TestParseTimeOrNow_Invalid(t *testing.T) {
	_, err := parseTimeOrNow("not-a-time")
	require.Error(t, err)
}
