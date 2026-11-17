// Package iacbroker exposes fixed, read-only IAC PRO observation queries.
// Callers choose IDs, never command text, SQL, URLs, credentials or scope.
package iacbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	Schema     = "iac_evidence_v1"
	AppID      = "80beff42ab0b41819f9c1c3c3ac95413308e70eb"
	MaxQueries = 6
	maxOutput  = 256 * 1024
	maxSamples = 400
	selector   = `app_id="` + AppID + `",cluster="prd60",namespace="iac-pro-pro",job="psdcloud-app-monitor"`
	business   = selector + `,route!~"/heartbeat|/ready|/live|/metrics|static"`
)

type Scope struct {
	Environment string `json:"environment"`
	AppID       string `json:"app_id"`
	Cluster     string `json:"cluster"`
	Namespace   string `json:"namespace"`
}
type Provenance struct {
	PackageManifestSHA256 string `json:"package_manifest_sha256,omitempty"`
	CLIHash               string `json:"cli_sha256"`
	SourceRevision        string `json:"source_revision"`
	Query                 string `json:"query"`
	Window                string `json:"window"`
}
type Evidence struct {
	Schema     string          `json:"schema"`
	QueryID    string          `json:"query_id"`
	Scope      Scope           `json:"scope"`
	ObservedAt time.Time       `json:"observed_at"`
	Status     string          `json:"status"` // ok means a valid observation, never a service-health verdict.
	ReasonCode string          `json:"reason_code,omitempty"`
	Data       json.RawMessage `json:"data"`
	Provenance Provenance      `json:"provenance"`
}
type Bundle struct {
	Schema  string     `json:"schema"`
	Items   []Evidence `json:"items"`
	Partial bool       `json:"partial"`
}
type Options struct {
	CCloud         *CCloudOptions
	Binary         string
	SourceRevision string
	Timeout        time.Duration
}
type command struct{ source, expr string }

var catalog = map[string]command{
	"sources":        {},
	"ccloud_l4":      {},
	"entry_coverage": {},
	"vip_coverage":   {},
	"freshness":      {"thanos", `max(time()-timestamp(iac_http_requests_total{` + selector + `}))`},
	"traffic":        {"thanos", `sum(rate(iac_http_requests_total{` + business + `}[5m]))`},
	"errors":         {"thanos", `100*sum(rate(iac_http_requests_total{` + business + `,result="system_error"}[5m]))/sum(rate(iac_http_requests_total{` + business + `}[5m]))`},
	"latency":        {"thanos", `histogram_quantile(0.99,sum by(le)(rate(iac_http_request_duration_seconds_bucket{` + business + `}[5m])))`},
	"commit_unknown": {"thanos", `sum(increase(iac_commit_unknown_total{` + selector + `}[5m]))`},
	"audit_dead":     {"thanos", `max(iac_audit_outbox_dead_letter{` + selector + `})`},
	"audit_lag":      {"thanos", `max(iac_audit_outbox_oldest_age_seconds{` + selector + `})`},
	"redis_timeouts": {"thanos", `sum(increase(iac_dependency_errors_total{` + selector + `,dependency="redis",error_class="timeout"}[5m]))`},
	"mysql_running":  {"obs", `max(mysql_threads{service_name="iac-shared-prod-mysql",kind="running",deployment_environment_name="pro"})`},
}

// QueryIDs are the complete public command allowlist; a new ID needs code review.
func QueryIDs() []string {
	return []string{"sources", "freshness", "traffic", "errors", "latency", "commit_unknown", "audit_dead", "audit_lag", "redis_timeouts", "mysql_running", "entry_coverage", "vip_coverage", "ccloud_l4"}
}

// Profile is advice for the worker prompt; Query independently enforces the allowlist.
func Profile(rule string) []string {
	switch rule {
	case "iac-sre-oma-canary":
		return []string{"sources", "freshness", "traffic", "errors"}
	case "iac-api-probe-not-all-200", "iac-api-probe-delay-high":
		return []string{"sources", "freshness", "traffic", "errors", "entry_coverage"}
	case "iac-redis002-vip-ras-down":
		return []string{"sources", "freshness", "traffic", "redis_timeouts", "vip_coverage"}
	case "iac-go-system-errors":
		return []string{"sources", "freshness", "traffic", "errors", "redis_timeouts", "mysql_running"}
	case "iac-go-latency-p99":
		return []string{"sources", "freshness", "traffic", "latency", "redis_timeouts", "mysql_running"}
	case "iac-go-traffic-zero":
		return []string{"sources", "freshness", "traffic", "errors"}
	case "iac-go-metrics-stale":
		return []string{"sources", "freshness", "traffic"}
	case "iac-go-commit-unknown":
		return []string{"sources", "freshness", "commit_unknown", "mysql_running"}
	case "iac-go-outbox-dead", "iac-go-outbox-lag":
		return []string{"sources", "freshness", "audit_dead", "audit_lag"}
	default:
		return []string{}
	}
}

// ProfileWithCCloud preserves the six-query budget and shared coverage placeholders.
func ProfileWithCCloud(rule string) []string {
	out := Profile(rule)
	if len(out) == 0 {
		return out
	}
	out = append([]string{}, out...)
	if len(out) == MaxQueries {
		out = out[1:]
	}
	return append(out, "ccloud_l4")
}

type Broker struct {
	ccloud                 *ccloudRuntime
	binary, revision, hash string
	timeout                time.Duration
	gate                   chan struct{}
}

func New(o Options) (*Broker, error) {
	if !filepath.IsAbs(o.Binary) {
		return nil, fmt.Errorf("iacbroker: binary must be an absolute path")
	}
	info, err := os.Stat(o.Binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("iacbroker: executable unavailable")
	}
	if o.Timeout == 0 {
		o.Timeout = 45 * time.Second
	}
	if o.Timeout < time.Second || o.Timeout > 90*time.Second {
		return nil, fmt.Errorf("iacbroker: timeout must be within [1s,90s]")
	}
	f, err := os.Open(o.Binary)
	if err != nil {
		return nil, fmt.Errorf("iacbroker: executable unavailable")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("iacbroker: executable hash failed")
	}
	b := &Broker{binary: o.Binary, revision: o.SourceRevision, hash: hex.EncodeToString(h.Sum(nil)), timeout: o.Timeout, gate: make(chan struct{}, 1)}
	if o.CCloud != nil {
		b.ccloud = newCCloud(*o.CCloud, CCloudManifestSHA256)
	}
	return b, nil
}

func (b *Broker) Collect(ctx context.Context, ids []string) (Bundle, error) {
	out := Bundle{Schema: Schema, Items: []Evidence{}}
	if len(ids) == 0 || len(ids) > MaxQueries {
		return out, fmt.Errorf("iacbroker: expected 1 to 6 query IDs")
	}
	seen := map[string]bool{}
	// Validate the entire request before running any command.
	for _, id := range ids {
		if _, ok := catalog[id]; !ok || seen[id] {
			return out, fmt.Errorf("iacbroker: invalid or duplicate query ID")
		}
		seen[id] = true
	}
	for _, id := range ids {
		item, err := b.Query(ctx, id)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, item)
		out.Partial = out.Partial || item.Status != "ok"
	}
	return out, nil
}
func (b *Broker) Query(ctx context.Context, id string) (Evidence, error) {
	spec, ok := catalog[id]
	if !ok {
		return Evidence{}, fmt.Errorf("iacbroker: query ID is not allowed")
	}
	item := Evidence{Schema: Schema, QueryID: id, Scope: Scope{"pro", AppID, "prd60", "iac-pro-pro"}, ObservedAt: time.Now().UTC(), Status: "unknown", Data: json.RawMessage(`null`), Provenance: Provenance{CLIHash: b.hash, SourceRevision: b.revision, Query: spec.expr, Window: "instant observation; fixed 5m PromQL rate/increase where applicable"}}
	if id == "ccloud_l4" {
		return b.queryCCloud(ctx, item)
	}
	if id == "entry_coverage" || id == "vip_coverage" {
		item.ReasonCode = "shared_probe_not_collected"
		item.Data = json.RawMessage(`{"coverage":"not_collected","note":"Go PRO application metrics do not establish five-site probe or Redis VIP availability"}`)
		return item, nil
	}
	if ctx.Err() != nil {
		item.ReasonCode = "cancelled"
		return item, nil
	}
	// Global per-broker gate also bounds callers that do not use worker concurrency.
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-ctx.Done():
		item.ReasonCode = "cancelled"
		return item, nil
	}
	if ctx.Err() != nil {
		item.ReasonCode = "cancelled"
		return item, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	args := []string{"sources", "--probe"}
	if id != "sources" {
		args = []string{"query", "prom", "--source", spec.source, "--expr", spec.expr}
	}
	cmd := exec.CommandContext(runCtx, b.binary, args...)
	// Never inherit arbitrary config paths, shell, cloud/OMA credentials, or executable search paths.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "TZ=UTC"}
	if auth, exists := os.LookupEnv("OBS_AUTH"); exists && (spec.source == "obs" || id == "sources") {
		cmd.Env = append(cmd.Env, "OBS_AUTH="+auth)
	}
	stdout := &boundedWriter{limit: maxOutput}
	stderr := &boundedWriter{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	item.ObservedAt = time.Now().UTC()
	if runCtx.Err() != nil {
		item.ReasonCode = "deadline_or_cancelled"
		return item, nil
	}
	if stdout.exceeded || stderr.exceeded {
		item.ReasonCode = "output_limit"
		return item, nil
	}
	if err != nil {
		item.ReasonCode = "cli_failed"
		return item, nil
	}
	if id == "sources" {
		item.Data, item.Status, item.ReasonCode = parseSources(stdout.buf.Bytes())
		return item, nil
	}
	item.Data, item.Status, item.ReasonCode = parseSeries(stdout.buf.Bytes(), item.ObservedAt)
	return item, nil
}

type boundedWriter struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	left := w.limit - w.buf.Len()
	if len(p) > left {
		w.exceeded = true
		p = p[:left]
	}
	_, _ = w.buf.Write(p)
	if w.exceeded {
		return n, errors.New("output limit")
	}
	return n, nil
}

// Numeric series only: labels/free-text errors are deliberately not exposed to the model.
func parseSeries(raw []byte, now time.Time) (json.RawMessage, string, string) {
	var rows []struct {
		Metric map[string]string
		Values []struct{ T, V float64 }
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return json.RawMessage(`null`), "unknown", "invalid_json"
	}
	samples := []map[string]float64{}
	partial := false
	for _, row := range rows {
		for _, v := range row.Values {
			if len(samples) >= maxSamples {
				return json.RawMessage(`null`), "unknown", "sample_limit"
			}
			if math.IsNaN(v.V) || math.IsInf(v.V, 0) || v.T <= 0 || v.T > float64(now.Unix())+5 || float64(now.Unix())-v.T > 180 {
				partial = true
				continue
			}
			samples = append(samples, map[string]float64{"timestamp": v.T, "value": v.V})
		}
	}
	if len(samples) == 0 {
		return json.RawMessage(`null`), "unknown", "no_fresh_samples"
	}
	out, _ := json.Marshal(map[string]any{"samples": samples, "note": "observation only; zero is not a complete health verdict"})
	if partial {
		return out, "unknown", "partial_or_stale_samples"
	}
	return out, "ok", ""
}
func parseSources(raw []byte) (json.RawMessage, string, string) {
	var rows []struct{ Name, Auth, Probe string }
	if err := json.Unmarshal(raw, &rows); err != nil {
		return json.RawMessage(`null`), "unknown", "invalid_json"
	}
	out := []map[string]any{}
	all := len(rows) == 2
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Name != "thanos" && row.Name != "obs" {
			all = false
			continue
		}
		if seen[row.Name] {
			all = false
		}
		seen[row.Name] = true
		ok := strings.HasPrefix(row.Probe, "ok (")
		all = all && ok
		out = append(out, map[string]any{"name": row.Name, "reachable": ok, "credential_configured": row.Auth == "none" || strings.HasPrefix(row.Auth, "configured (")})
	}
	data, _ := json.Marshal(map[string]any{"sources": out, "note": "connectivity probe only, not application health"})
	if !all {
		return data, "unknown", "source_unavailable"
	}
	return data, "ok", ""
}
