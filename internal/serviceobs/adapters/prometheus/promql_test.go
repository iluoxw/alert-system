package prometheus_test

import (
	"testing"

	"alert-system/internal/serviceobs/adapters/prometheus"
	"alert-system/internal/serviceobs/integration"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

func prometheusBinding(templateRef string, params integration.TemplateParams) integration.CompiledBinding {
	return integration.CompiledBinding{
		Key:                "binding-" + templateRef,
		Kind:               integration.KindSignal,
		TemplateRef:        templateRef,
		SourceKey:          integration.SourceKeyPrometheus,
		Capability:         integration.CapabilityPrometheus,
		RequiredPermission: integration.PermissionMetricsRead,
		Params:             params,
	}
}

func TestPromQLTemplate_counter_rate_v1(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "traces_spanmetrics_calls_total",
		RangeSeconds: intPtr(300),
		Labels: []integration.LabelEquality{
			{Name: "service_name", Value: "e2b-api"},
			{Name: "peer_service", Value: "e2b-orchestrator"},
		},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	want := `rate(traces_spanmetrics_calls_total{peer_service="e2b-orchestrator",service_name="e2b-api"}[300s])`
	if len(plan.Queries) != 1 || plan.Queries[0].Expr != want {
		t.Fatalf("expr = %q, want %q", plan.Queries[0].Expr, want)
	}
}

func TestPromQLTemplate_counter_rate_v1_groupBy(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(60),
		GroupBy:      []string{"instance"},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	want := `sum by (instance) (rate(http_requests_total[60s]))`
	if plan.Queries[0].Expr != want {
		t.Fatalf("expr = %q, want %q", plan.Queries[0].Expr, want)
	}
}

func TestPromQLTemplate_counter_ratio_percent_v1(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("counter_ratio_percent.v1", integration.TemplateParams{
		NumeratorMetric:   "traces_spanmetrics_calls_total",
		DenominatorMetric: "traces_spanmetrics_calls_total",
		RangeSeconds:      intPtr(300),
		CommonLabels: []integration.LabelEquality{
			{Name: "service_name", Value: "e2b-api"},
			{Name: "peer_service", Value: "e2b-orchestrator"},
		},
		NumeratorLabels: []integration.LabelEquality{
			{Name: "status_code", Value: "STATUS_CODE_ERROR"},
		},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	if len(plan.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(plan.Queries))
	}
	wantNum := `rate(traces_spanmetrics_calls_total{peer_service="e2b-orchestrator",service_name="e2b-api",status_code="STATUS_CODE_ERROR"}[300s])`
	wantDen := `rate(traces_spanmetrics_calls_total{peer_service="e2b-orchestrator",service_name="e2b-api"}[300s])`
	if plan.Queries[0].Expr != wantNum || plan.Queries[1].Expr != wantDen {
		t.Fatalf("queries = %#v", plan.Queries)
	}
}

func TestPromQLTemplate_histogram_quantile_rate_v1(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("histogram_quantile_rate.v1", integration.TemplateParams{
		BucketMetric: "traces_spanmetrics_latency_bucket",
		Quantile:     floatPtr(0.95),
		RangeSeconds: intPtr(300),
		Labels: []integration.LabelEquality{
			{Name: "service_name", Value: "e2b-api"},
		},
		GroupBy: []string{"instance"},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	want := `histogram_quantile(0.95, sum by (instance, le) (rate(traces_spanmetrics_latency_bucket{service_name="e2b-api"}[300s])))`
	if plan.Queries[0].Expr != want {
		t.Fatalf("expr = %q, want %q", plan.Queries[0].Expr, want)
	}
}

func TestPromQLTemplate_gauge_max_v1(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("gauge_max.v1", integration.TemplateParams{
		Metric: "node_load1",
		Labels: []integration.LabelEquality{{Name: "job", Value: "node-exporter"}},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	want := `node_load1{job="node-exporter"}`
	if plan.Queries[0].Expr != want {
		t.Fatalf("expr = %q, want %q", plan.Queries[0].Expr, want)
	}
}

func TestPromQLTemplate_gauge_max_v1_groupBy(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("gauge_max.v1", integration.TemplateParams{
		Metric:  "node_load1",
		GroupBy: []string{"instance"},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	want := `max by (instance) (node_load1)`
	if plan.Queries[0].Expr != want {
		t.Fatalf("expr = %q, want %q", plan.Queries[0].Expr, want)
	}
}

func TestPromQLTemplate_gauge_ratio_percent_v1(t *testing.T) {
	plan, err := prometheus.BuildQueryPlan(prometheusBinding("gauge_ratio_percent.v1", integration.TemplateParams{
		TotalMetric: "node_memory_HugePages_Total",
		FreeMetric:  "node_memory_HugePages_Free",
		Labels:      []integration.LabelEquality{{Name: "job", Value: "node-exporter"}},
		GroupBy:     []string{"instance"},
	}))
	if err != nil {
		t.Fatalf("BuildQueryPlan: %v", err)
	}
	if len(plan.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(plan.Queries))
	}
	wantTotal := `max by (instance) (node_memory_HugePages_Total{job="node-exporter"})`
	wantFree := `max by (instance) (node_memory_HugePages_Free{job="node-exporter"})`
	if plan.Queries[0].Expr != wantTotal || plan.Queries[1].Expr != wantFree {
		t.Fatalf("queries = %#v", plan.Queries)
	}
}

func TestPromQLInjection_rejectsMetric(t *testing.T) {
	cases := []integration.TemplateParams{
		{Metric: `up{job="x"}`, RangeSeconds: intPtr(60)},
		{Metric: "rate(x[1m])", RangeSeconds: intPtr(60)},
		{TotalMetric: `a"} or b{`, FreeMetric: "node_memory_HugePages_Free"},
	}
	for i, params := range cases {
		ref := "counter_rate.v1"
		if params.TotalMetric != "" {
			ref = "gauge_ratio_percent.v1"
		}
		_, err := prometheus.BuildQueryPlan(prometheusBinding(ref, params))
		if err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestPromQLInjection_rejectsLabelValue(t *testing.T) {
	_, err := prometheus.BuildQueryPlan(prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(60),
		Labels:       []integration.LabelEquality{{Name: "job", Value: `x",instance="y`}},
	}))
	if err == nil {
		t.Fatal("expected injection error")
	}
}

func TestPromQLInjection_rejectsDuplicateGroupBy(t *testing.T) {
	_, err := prometheus.BuildQueryPlan(prometheusBinding("gauge_max.v1", integration.TemplateParams{
		Metric:  "node_load1",
		GroupBy: []string{"instance", "instance"},
	}))
	if err == nil {
		t.Fatal("expected duplicate group_by error")
	}
}

func TestPromQLInjection_rejectsDynamicFocusLabel(t *testing.T) {
	_, err := prometheus.BuildQueryPlan(prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(60),
		Labels:       []integration.LabelEquality{{Name: "sandbox_id", Value: "abc"}},
	}))
	if err == nil {
		t.Fatal("expected focus label rejection")
	}
}

func TestPromQLInjection_rejectsDegenerateRatio(t *testing.T) {
	_, err := prometheus.BuildQueryPlan(prometheusBinding("counter_ratio_percent.v1", integration.TemplateParams{
		NumeratorMetric:   "calls_total",
		DenominatorMetric: "calls_total",
		RangeSeconds:      intPtr(60),
		Labels: []integration.LabelEquality{
			{Name: "service_name", Value: "svc"},
		},
	}))
	if err == nil {
		t.Fatal("expected degenerate ratio error")
	}
}

func TestPromQLInjection_rejectsLongRange(t *testing.T) {
	_, err := prometheus.BuildQueryPlan(prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(86401),
	}))
	if err == nil {
		t.Fatal("expected long range error")
	}
}

func TestPromQLInjection_rejectsBindingMismatch(t *testing.T) {
	binding := prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(60),
	})
	binding.SourceKey = integration.SourceKeyOpenObserveLogs
	_, err := prometheus.BuildQueryPlan(binding)
	if err == nil {
		t.Fatal("expected binding mismatch")
	}
}
