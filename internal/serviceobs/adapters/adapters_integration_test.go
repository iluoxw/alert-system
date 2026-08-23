package adapters_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/adapters/prometheus"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/provider"
	"alert-system/internal/serviceobs/reconcile"
)

// ReadAt 只透传 adapter 注入时钟；零结果/失败不得用样本时间伪造 observed_at。
func TestBatchBReadAtUsesAdapterClockNotSampleTime(t *testing.T) {
	fixedReadAt := time.Date(2026, 8, 23, 2, 9, 22, 0, time.UTC)
	sampleTime := time.Unix(1700000000, 0).UTC()
	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(empty)
	}))
	defer server.Close()
	endpoint, err := provider.ParseEndpoint(server.URL, provider.ModeLoopbackTest)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	client, err := provider.NewClient(provider.ClientOptions{Endpoint: endpoint})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	adapter := prometheus.NewAdapter(prometheus.Options{
		Client: client,
		Now:    func() time.Time { return fixedReadAt },
	})
	rangeSeconds := 60
	facts, err := adapter.ReadSignals(context.Background(), serviceobs.SignalRequest{
		Purpose: serviceobs.PurposeDirectorySummary,
		Bindings: []integration.CompiledBinding{{
			Key: "signal_rate", Kind: integration.KindSignal, TemplateRef: "counter_rate.v1",
			SourceKey: integration.SourceKeyPrometheus, Capability: integration.CapabilityPrometheus,
			RequiredPermission: integration.PermissionMetricsRead,
			Params: integration.TemplateParams{
				Metric: "http_requests_total", RangeSeconds: &rangeSeconds,
			},
		}},
		Limits: serviceobs.SourceLimits{MaxSeries: 10},
		To:     sampleTime,
	})
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if !facts.ReadAt.Equal(fixedReadAt) {
		t.Fatalf("ReadAt=%v want adapter clock %v", facts.ReadAt, fixedReadAt)
	}
	if len(facts.Signals) != 1 || !facts.Signals[0].ObservedAt.IsZero() {
		t.Fatalf("zero series 不得填 observed_at: %+v", facts.Signals[0])
	}
}

// overlay reconciliation 只合并 observed hints，declared domain 边界不变。
func TestBatchBOverlayReconcilePreservesDeclaredBoundary(t *testing.T) {
	declared := []serviceobs.TopologyRelation{{
		RelationID: "decl-1", SourceEntityID: "svc-a", TargetEntityID: "svc-b",
		Kind: "depends_on", Provenance: model.ProvenanceDeclared,
		MergedRelationIDs: []string{"decl-1"},
	}}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	result := reconcile.ReconcileOverlay(reconcile.Request{
		Scope:             reconcile.Scope{ServiceID: "service-1", ProfileKey: "example", ProfileVersion: 1},
		Now:               now,
		DeclaredRelations: declared,
		AliasResolution:   serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{}, Issues: []serviceobs.ReconciliationIssue{}},
	})
	if len(result.Relations) != 1 || result.Relations[0].RelationID != "decl-1" {
		t.Fatalf("declared boundary changed: %+v", result.Relations)
	}
	if len(result.Inventory) != 0 {
		t.Fatalf("无 hint 时不应扩展 inventory: %+v", result.Inventory)
	}
}
