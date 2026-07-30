package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"alert-system/internal/domain"
)

// loadGolden 读取 testdata 下的 golden payload 文件。
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 golden 文件 %s 失败: %v", name, err)
	}
	return body
}

// mustParseTime 是测试专用的时间解析辅助，输入非法直接 Fatal。
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("解析时间 %s 失败: %v", s, err)
	}
	return tm
}

// signalContextField 从 event.SignalContext 里取出指定 key 的值，供断言使用。
func signalContextField(t *testing.T, event domain.AlertEvent, key string) any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(event.SignalContext, &fields); err != nil {
		t.Fatalf("解析 SignalContext 失败: %v", err)
	}
	return fields[key]
}

func TestAlertmanagerAdapter_Firing(t *testing.T) {
	adapter := NewAlertmanagerAdapter(domain.SeverityWarning)
	if adapter.Kind() != "alertmanager" {
		t.Fatalf("Kind() = %q，期望 alertmanager", adapter.Kind())
	}

	events, err := adapter.Parse(loadGolden(t, "alertmanager_firing.json"))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1", len(events))
	}
	e := events[0]

	if e.RuleID != "HighCPUUsage" {
		t.Errorf("RuleID = %q，期望 HighCPUUsage", e.RuleID)
	}
	if e.Severity != domain.SeverityCritical || e.SeverityFallback {
		t.Errorf("Severity/Fallback = %q/%v，期望 critical/false", e.Severity, e.SeverityFallback)
	}
	if e.Status != domain.StatusFiring {
		t.Errorf("Status = %q，期望 firing", e.Status)
	}
	if e.Title != "节点 CPU 使用率超过 90%" {
		t.Errorf("Title = %q，期望取 annotations.summary", e.Title)
	}
	if e.ResolvePolicy != domain.ResolvePolicySource {
		t.Errorf("ResolvePolicy = %q，期望 source", e.ResolvePolicy)
	}
	if e.SignalKind != domain.SignalKindMetric {
		t.Errorf("SignalKind = %q，期望 metric", e.SignalKind)
	}
	if !e.EndsAt.IsZero() {
		t.Errorf("firing 状态下 EndsAt 应为零值，实际 %v", e.EndsAt)
	}
	if !e.StartsAt.Equal(mustParseTime(t, "2026-07-30T02:00:00Z")) {
		t.Errorf("StartsAt = %v，期望 2026-07-30T02:00:00Z", e.StartsAt)
	}
	if e.Labels["instance"] != "10.0.0.5:9100" || e.Labels["alertname"] != "HighCPUUsage" {
		t.Errorf("Labels 应原样保留上游 label，实际 %+v", e.Labels)
	}
	wantResourceRef := map[string]string{
		"service.name":           "node-exporter",
		"host.name":              "node-1",
		"service.instance.id":    "10.0.0.5:9100",
		"deployment.environment": "prod",
	}
	for k, v := range wantResourceRef {
		if e.ResourceRef[k] != v {
			t.Errorf("ResourceRef[%q] = %q，期望 %q（完整 ResourceRef: %+v）", k, e.ResourceRef[k], v, e.ResourceRef)
		}
	}
	if got := signalContextField(t, e, "vendor_fingerprint"); got != "abc123" {
		t.Errorf("signal_context.vendor_fingerprint = %v，期望 abc123", got)
	}
}

func TestAlertmanagerAdapter_Resolved(t *testing.T) {
	adapter := NewAlertmanagerAdapter(domain.SeverityWarning)
	events, err := adapter.Parse(loadGolden(t, "alertmanager_resolved.json"))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1", len(events))
	}
	e := events[0]

	if e.Status != domain.StatusResolved {
		t.Errorf("Status = %q，期望 resolved", e.Status)
	}
	want := mustParseTime(t, "2026-07-30T02:10:00Z")
	if !e.EndsAt.Equal(want) {
		t.Errorf("EndsAt = %v，期望 %v（resolved 状态应透传 endsAt）", e.EndsAt, want)
	}
}

func TestAlertmanagerAdapter_BatchWithSeverityFallback(t *testing.T) {
	adapter := NewAlertmanagerAdapter(domain.SeverityWarning)
	events, err := adapter.Parse(loadGolden(t, "alertmanager_batch.json"))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("事件数 = %d，期望 2（一次 webhook 多条告警应返回多条事件）", len(events))
	}

	first, second := events[0], events[1]
	if first.RuleID != "HighCPUUsage" || first.SeverityFallback {
		t.Errorf("第 1 条 = %+v，期望 RuleID=HighCPUUsage 且不兜底", first)
	}
	if second.RuleID != "DiskAlmostFull" {
		t.Errorf("第 2 条 RuleID = %q，期望 DiskAlmostFull", second.RuleID)
	}
	if !second.SeverityFallback || second.Severity != domain.SeverityWarning {
		t.Errorf("第 2 条缺失 severity label，期望按默认等级兜底并标记 SeverityFallback=true，实际 Severity=%q Fallback=%v", second.Severity, second.SeverityFallback)
	}
	if second.Title != "磁盘剩余空间不足 10%" {
		t.Errorf("第 2 条 Title = %q，期望取 annotations.description（无 summary 时兜底）", second.Title)
	}
}

func TestGrafanaAdapter_Firing(t *testing.T) {
	adapter := NewGrafanaAdapter(domain.SeverityWarning)
	if adapter.Kind() != "grafana" {
		t.Fatalf("Kind() = %q，期望 grafana", adapter.Kind())
	}

	events, err := adapter.Parse(loadGolden(t, "grafana_firing.json"))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1", len(events))
	}
	e := events[0]

	if e.RuleID != "HighMemoryUsage" {
		t.Errorf("RuleID = %q，期望 HighMemoryUsage", e.RuleID)
	}
	if e.Severity != domain.SeverityWarning || e.SeverityFallback {
		t.Errorf("Severity/Fallback = %q/%v，期望 warning/false", e.Severity, e.SeverityFallback)
	}
	if e.Status != domain.StatusFiring {
		t.Errorf("Status = %q，期望 firing", e.Status)
	}
	if !e.EndsAt.IsZero() {
		t.Errorf("firing 状态下 EndsAt 应为零值，实际 %v", e.EndsAt)
	}
	wantResourceRef := map[string]string{
		"service.name":        "checkout-api", // service 优先于 job（本样本无 job）
		"service.instance.id": "10.0.0.7:9100",
		"k8s.namespace.name":  "prod",
		"k8s.pod.name":        "checkout-api-7f8b9c-abcde",
	}
	for k, v := range wantResourceRef {
		if e.ResourceRef[k] != v {
			t.Errorf("ResourceRef[%q] = %q，期望 %q（完整 ResourceRef: %+v）", k, e.ResourceRef[k], v, e.ResourceRef)
		}
	}
	if got := signalContextField(t, e, "dashboard_url"); got != "http://grafana.example.com/d/abc123" {
		t.Errorf("signal_context.dashboard_url = %v，期望 dashboardURL 透传", got)
	}
	if got := signalContextField(t, e, "org_id"); got != float64(1) {
		t.Errorf("signal_context.org_id = %v，期望 1", got)
	}
}

func TestGrafanaAdapter_Resolved(t *testing.T) {
	adapter := NewGrafanaAdapter(domain.SeverityWarning)
	events, err := adapter.Parse(loadGolden(t, "grafana_resolved.json"))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1", len(events))
	}
	e := events[0]

	if e.Status != domain.StatusResolved {
		t.Errorf("Status = %q，期望 resolved", e.Status)
	}
	want := mustParseTime(t, "2026-07-30T03:20:00Z")
	if !e.EndsAt.Equal(want) {
		t.Errorf("EndsAt = %v，期望 %v（resolved 状态应透传 endsAt）", e.EndsAt, want)
	}
}

func TestVendorAdapters_RejectEmptyAlerts(t *testing.T) {
	am := NewAlertmanagerAdapter(domain.SeverityWarning)
	if _, err := am.Parse([]byte(`{"status":"firing","alerts":[]}`)); err == nil {
		t.Error("alerts 为空数组时 Parse 应返回错误")
	}

	gf := NewGrafanaAdapter(domain.SeverityWarning)
	if _, err := gf.Parse([]byte(`{"status":"firing","alerts":[]}`)); err == nil {
		t.Error("alerts 为空数组时 Parse 应返回错误")
	}
}

func TestVendorAdapters_RegisteredInRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewAlertmanagerAdapter(domain.SeverityWarning))
	reg.Register(NewGrafanaAdapter(domain.SeverityWarning))

	if _, ok := reg.Get("alertmanager"); !ok {
		t.Error("alertmanager adapter 应已注册")
	}
	if _, ok := reg.Get("grafana"); !ok {
		t.Error("grafana adapter 应已注册")
	}
}
