package alertsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

func TestClientFetchIncludesSuppressedAndUsesBearerEnv(t *testing.T) {
	t.Setenv("TEST_ALERTMANAGER_TOKEN", "secret-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/v2/alerts" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		for _, key := range []string{"active", "silenced", "inhibited", "unprocessed"} {
			if r.URL.Query().Get(key) != "true" {
				t.Fatalf("query %s=%q", key, r.URL.Query().Get(key))
			}
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"annotations":{"summary":"CPU high"},"receivers":[{"name":"ops"}],"fingerprint":"abc123","startsAt":"2026-08-02T01:00:00Z","updatedAt":"2026-08-02T01:01:00Z","endsAt":"2026-08-02T01:06:00Z","generatorURL":"https://prom.example/graph","labels":{"alertname":"HighCPU","severity":"critical","instance":"node-1"},"status":{"state":"suppressed","silencedBy":["silence-1"],"inhibitedBy":[],"mutedBy":[]}}]`))
	}))
	defer server.Close()

	source := store.AlertSource{
		ID: "source-1", Kind: domain.SourceKindAlertmanager, DefaultSeverity: "warning",
		Config: store.AlertmanagerSourceConfig{
			APIURL: server.URL + "/prefix", BearerTokenEnv: "TEST_ALERTMANAGER_TOKEN",
			ReconcileIntervalSeconds: 30, StaleAfterSeconds: 120, MissingConfirmations: 3,
		},
	}
	events, err := NewClient(server.Client()).Fetch(context.Background(), source)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	event := events[0]
	if event.UpstreamFingerprint != "abc123" || event.Status != domain.StatusFiring {
		t.Fatalf("event identity/status=%+v", event)
	}
	if event.SuppressionState != domain.SuppressionStateSilenced {
		t.Fatalf("suppression=%s", event.SuppressionState)
	}
	if got := event.SuppressionDetails["silenced_by"]; len(got) != 1 || got[0] != "silence-1" {
		t.Fatalf("silenced_by=%v", got)
	}
}

func TestClientFetchFailureDoesNotBecomeEmptySnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := store.AlertSource{
		Config: store.AlertmanagerSourceConfig{
			APIURL: server.URL, ReconcileIntervalSeconds: 30, StaleAfterSeconds: 120, MissingConfirmations: 3,
		},
	}
	if events, err := NewClient(server.Client()).Fetch(context.Background(), source); err == nil || events != nil {
		t.Fatalf("503 必须返回 error 且不能伪装成空快照: events=%v err=%v", events, err)
	}
}

func TestClientFetch_GrafanaUsesGrafanaAlertmanagerPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/alertmanager/grafana/api/v2/alerts" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"annotations":{"summary":"Grafana probe"},"fingerprint":"gf-1","startsAt":"2026-09-19T01:00:00Z","updatedAt":"2026-09-19T01:01:00Z","endsAt":"0001-01-01T00:00:00Z","labels":{"alertname":"GrafanaProbe","severity":"warning"},"status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}}]`))
	}))
	defer server.Close()

	source := store.AlertSource{
		Kind: domain.SourceKindGrafana, DefaultSeverity: "warning",
		Config: store.AlertmanagerSourceConfig{
			APIURL: server.URL, ReconcileIntervalSeconds: 30, StaleAfterSeconds: 120, MissingConfirmations: 3,
		},
	}
	events, err := NewClient(server.Client()).Fetch(context.Background(), source)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 1 || events[0].Source != domain.SourceKindGrafana || events[0].UpstreamFingerprint != "gf-1" {
		t.Fatalf("grafana snapshot event=%+v", events)
	}
}

func TestValidateConfig_RejectsUnknownKind(t *testing.T) {
	cfg := store.AlertmanagerSourceConfig{
		APIURL: "http://am.example", ReconcileIntervalSeconds: 30, StaleAfterSeconds: 120, MissingConfirmations: 3,
	}
	if err := ValidateConfig("prometheus", cfg); err == nil {
		t.Fatal("未知 kind 应被拒绝")
	}
	if err := ValidateConfig(domain.SourceKindGrafana, cfg); err != nil {
		t.Fatalf("grafana 应被接受: %v", err)
	}
	if err := ValidateConfig(domain.SourceKindAlertmanager, cfg); err != nil {
		t.Fatalf("alertmanager 应被接受: %v", err)
	}
}
