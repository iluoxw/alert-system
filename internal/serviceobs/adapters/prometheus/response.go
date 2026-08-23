package prometheus

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParsedResponse 是 Prometheus query API 的规范化结果。
type ParsedResponse struct {
	ResultType string
	Warnings   []string
	// Series 按稳定 label 指纹排序。
	Series []SampleSeries
	// Scalar 在 resultType=scalar 时有值。
	Scalar *ScalarSample
}

// SampleSeries 是一条带 label 的样本序列或 instant 点。
type SampleSeries struct {
	Labels map[string]string
	// Value 是 instant/vector 的最新值。
	Value float64
	// Timestamp 是样本时间。
	Timestamp time.Time
	// Points 是 matrix/range 的全部样本，按时间升序。
	Points []SamplePoint
}

// SamplePoint 是 range 样本点。
type SamplePoint struct {
	Timestamp time.Time
	Value     float64
}

// ScalarSample 是 scalar 结果。
type ScalarSample struct {
	Timestamp time.Time
	Value     float64
}

type promEnvelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

type promData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type promVectorItem struct {
	Metric map[string]string `json:"metric"`
	Value  json.RawMessage   `json:"value"`
}

type promMatrixItem struct {
	Metric map[string]string `json:"metric"`
	Values []json.RawMessage `json:"values"`
}

// ParseQueryResponse 解析官方 Prometheus query/query_range envelope。
func ParseQueryResponse(body []byte, maxSeries int) (ParsedResponse, error) {
	var envelope promEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ParsedResponse{}, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	if envelope.Status != "success" {
		return ParsedResponse{}, fmt.Errorf("%w: upstream status=%s", ErrUpstreamReadFailed, envelope.Status)
	}
	warnings := envelope.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	var data promData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return ParsedResponse{}, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	switch data.ResultType {
	case "vector":
		series, err := parseVector(data.Result, maxSeries)
		if err != nil {
			return ParsedResponse{}, err
		}
		return ParsedResponse{ResultType: data.ResultType, Warnings: warnings, Series: series}, nil
	case "matrix":
		series, err := parseMatrix(data.Result, maxSeries)
		if err != nil {
			return ParsedResponse{}, err
		}
		return ParsedResponse{ResultType: data.ResultType, Warnings: warnings, Series: series}, nil
	case "scalar":
		scalar, err := parseScalar(data.Result)
		if err != nil {
			return ParsedResponse{}, err
		}
		return ParsedResponse{ResultType: data.ResultType, Warnings: warnings, Scalar: scalar}, nil
	default:
		return ParsedResponse{}, fmt.Errorf("%w: resultType=%s", ErrUnsupportedResult, data.ResultType)
	}
}

func parseVector(raw json.RawMessage, maxSeries int) ([]SampleSeries, error) {
	var items []promVectorItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: vector 解码失败", ErrParseFailed)
	}
	if maxSeries > 0 && len(items) > maxSeries {
		return nil, fmt.Errorf("%w: series=%d", ErrCardinalityExceeded, len(items))
	}
	out := make([]SampleSeries, 0, len(items))
	for _, item := range items {
		ts, value, err := parseSamplePair(item.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, SampleSeries{
			Labels:    copyLabels(item.Metric),
			Value:     value,
			Timestamp: ts,
		})
	}
	sortSeries(out)
	return out, nil
}

func parseMatrix(raw json.RawMessage, maxSeries int) ([]SampleSeries, error) {
	var items []promMatrixItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: matrix 解码失败", ErrParseFailed)
	}
	if maxSeries > 0 && len(items) > maxSeries {
		return nil, fmt.Errorf("%w: series=%d", ErrCardinalityExceeded, len(items))
	}
	out := make([]SampleSeries, 0, len(items))
	for _, item := range items {
		points := make([]SamplePoint, 0, len(item.Values))
		for _, rawPoint := range item.Values {
			ts, value, err := parseSamplePair(rawPoint)
			if err != nil {
				return nil, err
			}
			points = append(points, SamplePoint{Timestamp: ts, Value: value})
		}
		if len(points) == 0 {
			continue
		}
		last := points[len(points)-1]
		out = append(out, SampleSeries{
			Labels:    copyLabels(item.Metric),
			Value:     last.Value,
			Timestamp: last.Timestamp,
			Points:    points,
		})
	}
	sortSeries(out)
	return out, nil
}

func parseScalar(raw json.RawMessage) (*ScalarSample, error) {
	ts, value, err := parseSamplePair(raw)
	if err != nil {
		return nil, err
	}
	return &ScalarSample{Timestamp: ts, Value: value}, nil
}

func parseSamplePair(raw json.RawMessage) (time.Time, float64, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return time.Time{}, 0, fmt.Errorf("%w: 样本对格式错误", ErrParseFailed)
	}
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: 时间戳非法", ErrParseFailed)
	}
	var valueStr string
	if err := json.Unmarshal(pair[1], &valueStr); err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: 样本值非法", ErrParseFailed)
	}
	value, err := parseSampleValue(valueStr)
	if err != nil {
		return time.Time{}, 0, err
	}
	return time.Unix(int64(ts), int64((ts-float64(int64(ts)))*1e9)).UTC(), value, nil
}

func parseSampleValue(raw string) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: 非数字样本", ErrInvalidSample)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: NaN/Inf", ErrInvalidSample)
	}
	return value, nil
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sortSeries(series []SampleSeries) {
	sort.SliceStable(series, func(i, j int) bool {
		return seriesFingerprint(series[i].Labels) < seriesFingerprint(series[j].Labels)
	})
}

func seriesFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, "\x1e")
}
