package prometheus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"alert-system/internal/serviceobs/provider"
)

const (
	discoveryMaxMetrics   = 200
	discoveryMaxLabels    = 200
	discoveryMaxSeries    = 500
	discoveryMatchPattern = `{__name__=~".+"}`
)

// DiscoveryManifest 是 bounded 只读 discovery 摘要。
type DiscoveryManifest struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Metrics     []MetricMeta `json:"metrics"`
	Labels      []LabelMeta  `json:"labels"`
	// TimeBoundsApprox 为 true 表示起止时间来自有限采样而非完整 retention。
	TimeBoundsApprox bool         `json:"time_bounds_approx"`
	EarliestSample   time.Time    `json:"earliest_sample,omitempty"`
	LatestSample     time.Time    `json:"latest_sample,omitempty"`
	Capabilities     Capabilities `json:"capabilities"`
	Digest           string       `json:"digest"`
}

// MetricMeta 描述 metric 名称与类型。
type MetricMeta struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	Unit string `json:"unit,omitempty"`
}

// LabelMeta 描述 label 与近似 cardinality。
type LabelMeta struct {
	Name              string `json:"name"`
	SeriesCardinality int    `json:"series_cardinality,omitempty"`
}

// Capabilities 记录已证实的 provider 能力。
type Capabilities struct {
	QueryLimitSupported bool `json:"query_limit_supported"`
}

// DiscoverManifest 从官方 metadata/series/labels API 构建 manifest。
func DiscoverManifest(ctx context.Context, client *provider.Client, opts DiscoveryOptions) (DiscoveryManifest, error) {
	if client == nil {
		return DiscoveryManifest{}, fmt.Errorf("%w: client 未配置", ErrUpstreamReadFailed)
	}
	maxMetrics := opts.MaxMetrics
	if maxMetrics <= 0 {
		maxMetrics = discoveryMaxMetrics
	}
	maxLabels := opts.MaxLabels
	if maxLabels <= 0 {
		maxLabels = discoveryMaxLabels
	}
	maxSeries := opts.MaxSeries
	if maxSeries <= 0 {
		maxSeries = discoveryMaxSeries
	}

	metrics, err := fetchMetadata(ctx, client, maxMetrics)
	if err != nil {
		return DiscoveryManifest{}, err
	}
	labels, seriesCount, earliest, latest, err := fetchSeriesSummary(ctx, client, maxSeries)
	if err != nil {
		return DiscoveryManifest{}, err
	}
	labelNames, err := fetchLabelNames(ctx, client, maxLabels)
	if err != nil {
		return DiscoveryManifest{}, err
	}
	labelMeta := mergeLabelMeta(labelNames, labels, seriesCount)

	manifest := DiscoveryManifest{
		GeneratedAt:      time.Now().UTC(),
		Metrics:          metrics,
		Labels:           labelMeta,
		TimeBoundsApprox: true,
		Capabilities: Capabilities{
			QueryLimitSupported: opts.QueryLimitSupported,
		},
	}
	if !earliest.IsZero() {
		manifest.EarliestSample = earliest
	}
	if !latest.IsZero() {
		manifest.LatestSample = latest
	}
	digest, err := canonicalDigest(manifest)
	if err != nil {
		return DiscoveryManifest{}, err
	}
	manifest.Digest = digest
	return manifest, nil
}

// DiscoveryOptions 控制 discovery 上界与能力标记。
type DiscoveryOptions struct {
	MaxMetrics          int
	MaxLabels           int
	MaxSeries           int
	QueryLimitSupported bool
}

func fetchMetadata(ctx context.Context, client *provider.Client, limit int) ([]MetricMeta, error) {
	resp, err := client.Get(ctx, "/api/v1/metadata", nil)
	if err != nil {
		return nil, MapProviderError(err)
	}
	if err := MapHTTPStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	var envelope struct {
		Status string                     `json:"status"`
		Data   map[string][]metadataEntry `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil || envelope.Status != "success" {
		return nil, ErrParseFailed
	}
	names := make([]string, 0, len(envelope.Data))
	for name := range envelope.Data {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > limit {
		return nil, fmt.Errorf("%w: metrics=%d", ErrCardinalityExceeded, len(names))
	}
	out := make([]MetricMeta, 0, len(names))
	for _, name := range names {
		meta := MetricMeta{Name: name}
		if entries := envelope.Data[name]; len(entries) > 0 {
			meta.Type = entries[0].Type
			meta.Unit = entries[0].Unit
		}
		out = append(out, meta)
	}
	return out, nil
}

type metadataEntry struct {
	Type string `json:"type"`
	Unit string `json:"unit"`
	Help string `json:"help"`
}

func fetchSeriesSummary(
	ctx context.Context, client *provider.Client, limit int,
) (map[string]int, int, time.Time, time.Time, error) {
	form := url.Values{}
	form.Set("match[]", discoveryMatchPattern)
	resp, err := client.PostForm(ctx, "/api/v1/series", form)
	if err != nil {
		return nil, 0, time.Time{}, time.Time{}, MapProviderError(err)
	}
	if err := MapHTTPStatus(resp.StatusCode); err != nil {
		return nil, 0, time.Time{}, time.Time{}, err
	}
	var envelope struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil || envelope.Status != "success" {
		return nil, 0, time.Time{}, time.Time{}, ErrParseFailed
	}
	if len(envelope.Data) > limit {
		return nil, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: series=%d", ErrCardinalityExceeded, len(envelope.Data))
	}
	labelCounts := map[string]int{}
	for _, series := range envelope.Data {
		for key := range series {
			if key == "__name__" {
				continue
			}
			labelCounts[key]++
		}
	}
	return labelCounts, len(envelope.Data), time.Time{}, time.Time{}, nil
}

func fetchLabelNames(ctx context.Context, client *provider.Client, limit int) ([]string, error) {
	resp, err := client.Get(ctx, "/api/v1/labels", nil)
	if err != nil {
		return nil, MapProviderError(err)
	}
	if err := MapHTTPStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	var envelope struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil || envelope.Status != "success" {
		return nil, ErrParseFailed
	}
	sort.Strings(envelope.Data)
	if len(envelope.Data) > limit {
		return nil, fmt.Errorf("%w: labels=%d", ErrCardinalityExceeded, len(envelope.Data))
	}
	return envelope.Data, nil
}

func mergeLabelMeta(names []string, counts map[string]int, seriesCount int) []LabelMeta {
	out := make([]LabelMeta, 0, len(names))
	for _, name := range names {
		meta := LabelMeta{Name: name}
		if count, ok := counts[name]; ok {
			meta.SeriesCardinality = count
		}
		out = append(out, meta)
	}
	if seriesCount > 0 && len(out) == 0 {
		out = append(out, LabelMeta{Name: "__series_count__", SeriesCardinality: seriesCount})
	}
	return out
}

func canonicalDigest(manifest DiscoveryManifest) (string, error) {
	copy := manifest
	copy.Digest = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// RedactManifestForLog 返回不含敏感路径的摘要字符串。
func RedactManifestForLog(manifest DiscoveryManifest) string {
	return fmt.Sprintf("metrics=%d labels=%d digest=%s",
		len(manifest.Metrics), len(manifest.Labels), shortDigest(manifest.Digest))
}

func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
