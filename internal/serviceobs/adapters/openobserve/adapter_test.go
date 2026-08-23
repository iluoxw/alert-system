package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/observe"
	"alert-system/internal/serviceobs/provider"
)

func TestAdapterEmptyVsSchemaError(t *testing.T) {
	t.Parallel()
	emptyBody := readFixture(t, "search_empty.json")
	schemaBody := readFixture(t, "search_schema_error.json")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/default/_search":
			var req searchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if strings.Contains(req.Query.SQL, "missing_field") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(schemaBody)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(emptyBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL)
	windowFrom, windowTo := testWindow()
	focus := &model.Focus{Kind: "sandbox_id", Value: "sbx-123"}

	goodBinding := sampleLogsBinding("sandbox_id", "e2b_logs")
	badBinding := sampleLogsBinding("missing_field", "e2b_logs")
	badBinding.Key = "bad_binding"

	req := serviceobs.FocusRequest{
		ServiceID: "svc_test",
		From:      windowFrom,
		To:        windowTo,
		Focus:     focus,
		Limits:    serviceobs.SourceLimits{MaxRows: 50, Timeout: 2 * time.Second},
		Bindings:  []integration.CompiledBinding{goodBinding, badBinding},
	}

	facts, err := adapter.ReadLogs(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if facts.Focus != serviceobs.FocusEmpty {
		t.Fatalf("focus=%q want empty", facts.Focus)
	}
	if facts.Source.State != model.AvailabilityUnknown {
		t.Fatalf("source state=%q want unknown partial degrade", facts.Source.State)
	}
	if len(facts.EvidenceRefs) != 0 {
		t.Fatalf("expected no evidence refs on empty hits")
	}
}

func TestAdapterUpstreamAuthFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":401,"message":"unauthorized"}`)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL)
	windowFrom, windowTo := testWindow()
	req := serviceobs.FocusRequest{
		From:  windowFrom,
		To:    windowTo,
		Focus: &model.Focus{Kind: "sandbox_id", Value: "sbx-123"},
		Bindings: []integration.CompiledBinding{
			sampleLogsBinding("sandbox_id", "e2b_logs"),
		},
		Limits: serviceobs.SourceLimits{Timeout: 2 * time.Second, MaxRows: 10},
	}
	_, err := adapter.ReadLogs(context.Background(), req)
	if !errors.Is(err, observe.ErrUpstreamAuthFailed) {
		t.Fatalf("err=%v want ErrUpstreamAuthFailed", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d want 1", calls.Load())
	}
}

func TestAdapterReadLogsEvidenceRedaction(t *testing.T) {
	t.Parallel()
	body := readFixture(t, "search_logs_hit.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL)
	windowFrom, windowTo := testWindow()
	facts, err := adapter.ReadLogs(context.Background(), serviceobs.FocusRequest{
		ServiceID: "svc_test",
		From:      windowFrom,
		To:        windowTo,
		Focus:     &model.Focus{Kind: "sandbox_id", Value: "sbx-123"},
		Bindings:  []integration.CompiledBinding{sampleLogsBinding("sandbox_id", "e2b_logs")},
		Limits:    serviceobs.SourceLimits{Timeout: 2 * time.Second, MaxRows: 10},
	})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if facts.Focus != serviceobs.FocusMatched {
		t.Fatalf("focus=%q", facts.Focus)
	}
	if len(facts.EvidenceRefs) != 1 {
		t.Fatalf("evidence count=%d", len(facts.EvidenceRefs))
	}
	ref := facts.EvidenceRefs[0]
	raw, _ := json.Marshal(ref)
	text := string(raw)
	for _, forbidden := range []string{"secret payload", "Authorization", "SELECT "} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("evidence leaked %q in %s", forbidden, text)
		}
	}
	if ref.Locator.QueryTemplateRef != "logs_by_focus.v1" {
		t.Fatalf("locator template=%q", ref.Locator.QueryTemplateRef)
	}
}

func TestAdapterReadTracesJourneyTopologyDedup(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	body := readFixture(t, "search_trace_hit.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL)
	windowFrom, windowTo := testWindow()
	binding := sampleTraceBinding("sandbox_id", "default")
	dup := binding
	dup.Key = "trace_dup"
	req := serviceobs.FocusRequest{
		ServiceID: "svc_test",
		From:      windowFrom,
		To:        windowTo,
		Focus:     &model.Focus{Kind: "sandbox_id", Value: "sbx-123"},
		Bindings:  []integration.CompiledBinding{binding, dup},
		Limits:    serviceobs.SourceLimits{Timeout: 2 * time.Second, MaxRows: 25},
	}
	facts, err := adapter.ReadTraces(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadTraces: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d want deduped single query", calls.Load())
	}
	if facts.Focus != serviceobs.FocusMatched {
		t.Fatalf("focus=%q", facts.Focus)
	}
	if len(facts.JourneyStages) != 1 || facts.JourneyStages[0].StageKey != "create" {
		t.Fatalf("journey stages=%+v", facts.JourneyStages)
	}
	if facts.JourneyStages[0].State != model.JourneyStageStateOK {
		t.Fatalf("stage state=%q", facts.JourneyStages[0].State)
	}
	if len(facts.TopologyHints) != 1 {
		t.Fatalf("topology hints=%+v", facts.TopologyHints)
	}
	instances := ProjectInstanceHints(binding, mustParseHits(t, body), facts.EvidenceRefs, time.Now().UTC())
	if len(instances) != 0 {
		t.Fatalf("expected no instance hints without instance mappings")
	}
}

func TestAdapterDuplicateCallerMappingYieldsNoTopology(t *testing.T) {
	t.Parallel()
	body := readFixture(t, "search_trace_hit.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL)
	windowFrom, windowTo := testWindow()
	binding := sampleTraceBinding("sandbox_id", "default")
	binding.Params.IdentityMappings = append(binding.Params.IdentityMappings, integration.IdentityMapping{
		Field: "service_name", Namespace: "service", Role: integration.IdentityRoleCaller,
	})
	req := serviceobs.FocusRequest{
		From:     windowFrom,
		To:       windowTo,
		Focus:    &model.Focus{Kind: "sandbox_id", Value: "sbx-123"},
		Bindings: []integration.CompiledBinding{binding},
		Limits:   serviceobs.SourceLimits{Timeout: 2 * time.Second, MaxRows: 25},
	}
	facts, err := adapter.ReadTraces(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadTraces: %v", err)
	}
	if len(facts.TopologyHints) != 0 {
		t.Fatalf("expected zero topology hints, got %+v", facts.TopologyHints)
	}
	if len(facts.Issues) == 0 {
		t.Fatal("expected reconciliation issue for duplicate caller mapping")
	}
}

func TestMapHTTPErrorRedaction(t *testing.T) {
	t.Parallel()
	err := mapHTTPError(http.StatusBadRequest, []byte(`{"message":"unknown field sandbox_id"}`))
	if !errors.Is(err, ErrSchemaError) {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "sandbox_id") {
		t.Fatalf("error leaked field name: %v", err)
	}
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	endpoint, err := provider.ParseEndpoint(baseURL, provider.ModeLoopbackTest)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	client, err := provider.NewClient(provider.ClientOptions{
		Endpoint:         endpoint,
		Timeout:          2 * time.Second,
		MaxResponseBytes: 1 << 20,
		MaxConcurrent:    4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	adapter, err := New(Config{Client: client, Organization: "default"})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	return adapter
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func testWindow() (from, to time.Time) {
	from = time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	return from, from.Add(5 * time.Minute)
}

func mustParseHits(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	resp, err := parseSearchResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return resp.Hits
}
