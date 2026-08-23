package prometheus_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/adapters/prometheus"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/provider"
)

func loadJSONFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *provider.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := provider.ParseEndpoint(server.URL, provider.ModeLoopbackTest)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	client, err := provider.NewClient(provider.ClientOptions{Endpoint: endpoint})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func hugepagesBinding(key string) integration.CompiledBinding {
	return integration.CompiledBinding{
		Key:                key,
		Kind:               integration.KindSignal,
		TemplateRef:        "gauge_ratio_percent.v1",
		SourceKey:          integration.SourceKeyPrometheus,
		Capability:         integration.CapabilityPrometheus,
		RequiredPermission: integration.PermissionMetricsRead,
		Params: integration.TemplateParams{
			TotalMetric: "node_memory_HugePages_Total",
			FreeMetric:  "node_memory_HugePages_Free",
			Labels:      []integration.LabelEquality{{Name: "job", Value: "node-exporter"}},
			GroupBy:     []string{"instance"},
		},
	}
}

func TestReadSignals_hugepagesGolden(t *testing.T) {
	totalBody := []byte(`{
  "status":"success",
  "data":{"resultType":"vector","result":[
    {"metric":{"instance":"10.0.0.1:9100"},"value":[1700000000,"100"]},
    {"metric":{"instance":"10.0.0.2:9100"},"value":[1700000000,"200"]},
    {"metric":{"instance":"10.0.0.3:9100"},"value":[1700000000,"0"]}
  ]}
}`)
	freeBody := []byte(`{
  "status":"success",
  "data":{"resultType":"vector","result":[
    {"metric":{"instance":"10.0.0.1:9100"},"value":[1700000000,"9"]},
    {"metric":{"instance":"10.0.0.2:9100"},"value":[1700000000,"40"]}
  ]}
}`)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		query := r.Form.Get("query")
		switch {
		case strings.Contains(query, "HugePages_Total"):
			_, _ = w.Write(totalBody)
		case strings.Contains(query, "HugePages_Free"):
			_, _ = w.Write(freeBody)
		default:
			t.Fatalf("unexpected query: %s", query)
		}
	})
	adapter := prometheus.NewAdapter(prometheus.Options{Client: client})
	facts, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeOverview,
		Bindings: []integration.CompiledBinding{
			hugepagesBinding("signal_hugepages"),
		},
		Limits: serviceobs.SourceLimits{MaxSeries: 100},
		To:     time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if len(facts.Signals) != 1 {
		t.Fatalf("signals = %+v", facts.Signals)
	}
	signal := facts.Signals[0]
	if signal.Availability != model.SignalAvailabilityAvailable {
		t.Fatalf("availability = %s", signal.Availability)
	}
	if signal.Value == nil || *signal.Value != 91 {
		t.Fatalf("worst value = %v, want 91", *signal.Value)
	}
	if !signal.FreshnessUnknown {
		t.Fatal("expected partial coverage freshness unknown")
	}
	if facts.Source.State != model.AvailabilityUnknown {
		t.Fatalf("source state = %s, want unknown partial", facts.Source.State)
	}
}

func TestReadSignals_noSeriesEmptySource(t *testing.T) {
	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(empty)
	})
	adapter := prometheus.NewAdapter(prometheus.Options{Client: client})
	facts, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeDirectorySummary,
		Bindings: []integration.CompiledBinding{
			prometheusBinding("counter_rate.v1", integration.TemplateParams{
				Metric:       "http_requests_total",
				RangeSeconds: intPtr(60),
			}),
		},
		Limits: serviceobs.SourceLimits{MaxSeries: 10},
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if facts.Signals[0].UnknownReason != serviceobs.ReasonNoSeries {
		t.Fatalf("reason = %s", facts.Signals[0].UnknownReason)
	}
	if facts.Source.State != model.AvailabilityEmpty {
		t.Fatalf("source = %s", facts.Source.State)
	}
}

func TestReadSignals_partialBindingFailure(t *testing.T) {
	okBody := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"10"]}]}}`)
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(okBody)
	})
	adapter := prometheus.NewAdapter(prometheus.Options{Client: client})
	facts, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeDirectorySummary,
		Bindings: []integration.CompiledBinding{
			prometheusBinding("counter_rate.v1", integration.TemplateParams{
				Metric:       "bad_metric",
				RangeSeconds: intPtr(60),
			}),
			prometheusBinding("counter_rate.v1", integration.TemplateParams{
				Metric:       "good_metric",
				RangeSeconds: intPtr(60),
			}),
		},
		Limits: serviceobs.SourceLimits{MaxSeries: 10},
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if facts.Signals[0].Availability != model.SignalAvailabilityUnknown {
		t.Fatalf("first signal = %+v", facts.Signals[0])
	}
	if facts.Signals[1].Availability != model.SignalAvailabilityAvailable {
		t.Fatalf("second signal = %+v", facts.Signals[1])
	}
	if facts.Source.State != model.AvailabilityUnknown {
		t.Fatalf("source = %s", facts.Source.State)
	}
}

func TestReadSignals_dedupSameTemplateParams(t *testing.T) {
	var calls atomic.Int32
	body := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"10"]}]}}`)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(body)
	})
	params := integration.TemplateParams{
		Metric:       "shared_metric",
		RangeSeconds: intPtr(60),
	}
	adapter := prometheus.NewAdapter(prometheus.Options{Client: client})
	_, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeDirectorySummary,
		Bindings: []integration.CompiledBinding{
			prometheusBinding("counter_rate.v1", params),
			{
				Key:                "binding-copy",
				Kind:               integration.KindSignal,
				TemplateRef:        "counter_rate.v1",
				SourceKey:          integration.SourceKeyPrometheus,
				Capability:         integration.CapabilityPrometheus,
				RequiredPermission: integration.PermissionMetricsRead,
				Params:             params,
			},
		},
		Limits: serviceobs.SourceLimits{MaxSeries: 10},
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestReadSignals_bindingMismatchFailClosed(t *testing.T) {
	adapter := prometheus.NewAdapter(prometheus.Options{Client: newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})})
	binding := prometheusBinding("counter_rate.v1", integration.TemplateParams{
		Metric:       "http_requests_total",
		RangeSeconds: intPtr(60),
	})
	binding.Kind = integration.KindJourney
	_, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose:  serviceobs.PurposeOverview,
		Bindings: []integration.CompiledBinding{binding},
	})
	if err == nil {
		t.Fatal("expected binding mismatch error")
	}
}

func TestReadSignals_sparklineOnOverview(t *testing.T) {
	instant := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[100,"5"]}]}}`)
	matrix := []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[100,"1"],[160,"2"],[220,"3"]]}]}}`)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write(matrix)
			return
		}
		_, _ = w.Write(instant)
	})
	adapter := prometheus.NewAdapter(prometheus.Options{Client: client})
	facts, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeOverview,
		Bindings: []integration.CompiledBinding{
			prometheusBinding("counter_rate.v1", integration.TemplateParams{
				Metric:       "http_requests_total",
				RangeSeconds: intPtr(60),
			}),
		},
		WindowSeconds: 3600,
		Limits:        serviceobs.SourceLimits{MaxSeries: 10},
		To:            time.Unix(220, 0).UTC(),
		From:          time.Unix(100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if len(facts.Signals[0].Sparkline) != 3 {
		t.Fatalf("sparkline = %v", facts.Signals[0].Sparkline)
	}
}

func TestDiscoverManifest_bounded(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/metadata"):
			_, _ = w.Write(loadJSONFixture(t, "metadata_success.json"))
		case strings.HasSuffix(r.URL.Path, "/series"):
			_, _ = w.Write(loadJSONFixture(t, "series_success.json"))
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write(loadJSONFixture(t, "labels_success.json"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})
	manifest, err := prometheus.DiscoverManifest(context.Background(), client, prometheus.DiscoveryOptions{
		QueryLimitSupported: true,
	})
	if err != nil {
		t.Fatalf("DiscoverManifest: %v", err)
	}
	if len(manifest.Metrics) != 2 || manifest.Digest == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if !manifest.Capabilities.QueryLimitSupported {
		t.Fatal("expected query limit capability")
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "Authorization") || strings.Contains(string(raw), "10.0.0.1") {
		t.Fatalf("manifest leaked sensitive/sample data: %s", raw)
	}
}
