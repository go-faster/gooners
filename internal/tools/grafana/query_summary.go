package grafana

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// QuerySummary is the structured result returned by verify_query.
// It describes the shape and health of the data without returning raw values,
// keeping LLM context usage proportional to insight rather than data volume.
type QuerySummary struct {
	SeriesCount  int               `json:"series_count"`
	ResultType   string            `json:"result_type"`             // "matrix", "vector", "streams"
	TimeRange    string            `json:"time_range,omitempty"`    // e.g. "09:00:00Z → 10:00:00Z (1h0m0s)"
	AvgInterval  string            `json:"avg_interval,omitempty"`  // mean gap between consecutive points
	DataAge      string            `json:"data_age,omitempty"`      // time since newest data point
	PointsTotal  int               `json:"points_total,omitempty"`  // total sample count across all series
	CommonLabels map[string]string `json:"common_labels,omitempty"` // labels with the same value in every series
	Dimensions   map[string]int    `json:"dimensions,omitempty"`    // varying labels → count of distinct values
	Values       *ValueStats       `json:"values,omitempty"`        // aggregate numeric statistics
	Warning      string            `json:"warning,omitempty"`
}

// ValueStats holds aggregate statistics computed across all series and all time points.
type ValueStats struct {
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	Mean       float64 `json:"mean"`
	LastMean   float64 `json:"last_mean"`             // mean of each series' last sample
	LastStddev float64 `json:"last_stddev,omitempty"` // stdev of last values; omitted for single series
}

// analyzeLabels splits label maps into constant (common) and varying (dimensions).
// __name__ is excluded — it is the metric name, not a useful selector dimension.
func analyzeLabels(allLabels []map[string]string) (common map[string]string, dimensions map[string]int) {
	if len(allLabels) == 0 {
		return nil, nil
	}
	byKey := map[string]map[string]struct{}{}
	for _, labels := range allLabels {
		for k, v := range labels {
			if k == "__name__" {
				continue
			}
			if byKey[k] == nil {
				byKey[k] = map[string]struct{}{}
			}
			byKey[k][v] = struct{}{}
		}
	}
	for k, vs := range byKey {
		if len(vs) == 1 {
			if common == nil {
				common = make(map[string]string)
			}
			for v := range vs {
				common[k] = v
			}
		} else {
			if dimensions == nil {
				dimensions = make(map[string]int)
			}
			dimensions[k] = len(vs)
		}
	}
	return common, dimensions
}

// promSample is a [timestamp_float, value_string] pair from the Prometheus API.
type promSample [2]json.RawMessage

func (s promSample) timestamp() (float64, error) {
	var t float64
	err := json.Unmarshal(s[0], &t)
	return t, err
}

func (s promSample) floatValue() (float64, error) {
	var raw string
	if err := json.Unmarshal(s[1], &raw); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(raw, 64)
}

// --- Prometheus ---

type promResponse struct {
	Status string          `json:"status"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data"`
}

type promDataType struct {
	ResultType string `json:"resultType"`
}

type promMatrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values []promSample      `json:"values"`
}

type promVectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  promSample        `json:"value"`
}

func summarizePrometheusResponse(body []byte, now time.Time) (*QuerySummary, error) {
	var resp promResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding prometheus response: %w", err)
	}
	if resp.Status != "success" {
		return &QuerySummary{Warning: fmt.Sprintf("query failed: %s", resp.Error)}, nil
	}

	var dt promDataType
	if err := json.Unmarshal(resp.Data, &dt); err != nil {
		return nil, fmt.Errorf("decoding result type: %w", err)
	}

	switch dt.ResultType {
	case "matrix":
		var data struct {
			Result []promMatrixSeries `json:"result"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, fmt.Errorf("decoding matrix result: %w", err)
		}
		return summarizePromMatrix(data.Result, now), nil

	case "vector":
		var data struct {
			Result []promVectorSample `json:"result"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, fmt.Errorf("decoding vector result: %w", err)
		}
		return summarizePromVector(data.Result, now), nil

	default:
		return &QuerySummary{ResultType: dt.ResultType, Warning: "unsupported result type for summary"}, nil
	}
}

func summarizePromMatrix(series []promMatrixSeries, now time.Time) *QuerySummary {
	s := &QuerySummary{SeriesCount: len(series), ResultType: "matrix"}
	if len(series) == 0 {
		s.Warning = "query returned no data"
		return s
	}

	var (
		allLabels      []map[string]string
		pointsTotal    int
		minVal, maxVal = math.Inf(1), math.Inf(-1)
		sumVal         float64
		countVal       int
		lastVals       []float64
		minTS, maxTS   = math.Inf(1), math.Inf(-1)
		intervalSum    float64
		intervalCount  int
	)

	for _, ser := range series {
		allLabels = append(allLabels, ser.Metric)
		pointsTotal += len(ser.Values)

		var serFirstTS, serLastTS float64
		var serLast float64
		hasLast := false

		for i, sample := range ser.Values {
			ts, err := sample.timestamp()
			if err != nil {
				continue
			}
			minTS = math.Min(minTS, ts)
			maxTS = math.Max(maxTS, ts)
			if i == 0 {
				serFirstTS = ts
			}
			serLastTS = ts

			v, err := sample.floatValue()
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			minVal = math.Min(minVal, v)
			maxVal = math.Max(maxVal, v)
			sumVal += v
			countVal++
			serLast = v
			hasLast = true
		}

		if hasLast {
			lastVals = append(lastVals, serLast)
		}
		if len(ser.Values) >= 2 {
			intervalSum += (serLastTS - serFirstTS) / float64(len(ser.Values)-1)
			intervalCount++
		}
	}

	s.PointsTotal = pointsTotal
	s.CommonLabels, s.Dimensions = analyzeLabels(allLabels)

	if !math.IsInf(minTS, 1) {
		tMin := time.Unix(int64(minTS), 0).UTC()
		tMax := time.Unix(int64(maxTS), 0).UTC()
		s.TimeRange = fmt.Sprintf("%s → %s (%s)",
			tMin.Format("15:04:05Z"), tMax.Format("15:04:05Z"),
			tMax.Sub(tMin).Round(time.Second))
		s.DataAge = now.Sub(tMax).Round(time.Second).String()
	}
	if intervalCount > 0 {
		avgSec := intervalSum / float64(intervalCount)
		s.AvgInterval = (time.Duration(avgSec * float64(time.Second))).Round(time.Second).String()
	}
	if countVal > 0 {
		s.Values = buildValueStats(minVal, maxVal, sumVal/float64(countVal), lastVals)
	}
	return s
}

func summarizePromVector(samples []promVectorSample, now time.Time) *QuerySummary {
	s := &QuerySummary{SeriesCount: len(samples), ResultType: "vector"}
	if len(samples) == 0 {
		s.Warning = "query returned no data"
		return s
	}

	var (
		allLabels      []map[string]string
		minVal, maxVal = math.Inf(1), math.Inf(-1)
		sumVal         float64
		countVal       int
		lastVals       []float64
		maxTS          float64
	)

	for _, sample := range samples {
		allLabels = append(allLabels, sample.Metric)

		ts, err := sample.Value.timestamp()
		if err == nil {
			maxTS = math.Max(maxTS, ts)
		}

		v, err := sample.Value.floatValue()
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		minVal = math.Min(minVal, v)
		maxVal = math.Max(maxVal, v)
		sumVal += v
		countVal++
		lastVals = append(lastVals, v)
	}

	s.CommonLabels, s.Dimensions = analyzeLabels(allLabels)

	if maxTS > 0 {
		s.DataAge = now.Sub(time.Unix(int64(maxTS), 0).UTC()).Round(time.Second).String()
	}
	if countVal > 0 {
		s.Values = buildValueStats(minVal, maxVal, sumVal/float64(countVal), lastVals)
	}
	return s
}

// --- Loki ---

type lokiResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

type lokiStreamEntry struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // [ns_timestamp_string, log_line]
}

func summarizeLokiResponse(body []byte, now time.Time) (*QuerySummary, error) {
	var resp lokiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding loki response: %w", err)
	}

	var dt promDataType
	if err := json.Unmarshal(resp.Data, &dt); err != nil {
		return nil, fmt.Errorf("decoding result type: %w", err)
	}

	switch dt.ResultType {
	case "streams":
		var data struct {
			Result []lokiStreamEntry `json:"result"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, fmt.Errorf("decoding streams result: %w", err)
		}
		return summarizeLokiStreams(data.Result, now), nil

	case "matrix":
		// Loki metric queries return a Prometheus-compatible matrix.
		var data struct {
			Result []promMatrixSeries `json:"result"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, fmt.Errorf("decoding loki matrix result: %w", err)
		}
		return summarizePromMatrix(data.Result, now), nil

	default:
		return &QuerySummary{ResultType: dt.ResultType, Warning: "unsupported result type for summary"}, nil
	}
}

func summarizeLokiStreams(streams []lokiStreamEntry, now time.Time) *QuerySummary {
	s := &QuerySummary{SeriesCount: len(streams), ResultType: "streams"}
	if len(streams) == 0 {
		s.Warning = "query returned no data"
		return s
	}

	var (
		allLabels  []map[string]string
		linesTotal int
		maxTSNs    int64
	)

	for _, stream := range streams {
		allLabels = append(allLabels, stream.Stream)
		linesTotal += len(stream.Values)
		for _, v := range stream.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err == nil && ns > maxTSNs {
				maxTSNs = ns
			}
		}
	}

	s.PointsTotal = linesTotal
	s.CommonLabels, s.Dimensions = analyzeLabels(allLabels)

	if maxTSNs > 0 {
		newest := time.Unix(0, maxTSNs).UTC()
		s.DataAge = now.Sub(newest).Round(time.Second).String()
	}
	return s
}

// --- shared helpers ---

func buildValueStats(minVal, maxVal, mean float64, lastVals []float64) *ValueStats {
	vs := &ValueStats{
		Min:  roundSig(minVal),
		Max:  roundSig(maxVal),
		Mean: roundSig(mean),
	}
	if len(lastVals) > 0 {
		var sum float64
		for _, v := range lastVals {
			sum += v
		}
		lastMean := sum / float64(len(lastVals))
		vs.LastMean = roundSig(lastMean)

		if len(lastVals) > 1 {
			var variance float64
			for _, v := range lastVals {
				d := v - lastMean
				variance += d * d
			}
			vs.LastStddev = roundSig(math.Sqrt(variance / float64(len(lastVals))))
		}
	}
	return vs
}

// roundSig rounds to 6 significant figures to avoid floating-point noise in output.
func roundSig(v float64) float64 {
	if v == 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	mag := math.Pow(10, math.Floor(math.Log10(math.Abs(v)))-5)
	return math.Round(v/mag) * mag
}
