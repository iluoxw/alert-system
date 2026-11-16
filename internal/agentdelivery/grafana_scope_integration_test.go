//go:build integration

package agentdelivery

import (
	"alert-system/internal/alertsource"
	"alert-system/internal/domain"
	"alert-system/internal/ingest"
	"alert-system/internal/store"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGrafanaScopedWebhookSnapshotAndPersistentAnalysisDedup(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	var mu sync.Mutex
	body := fmt.Sprintf(`[{"labels":{"alertname":"IAC Go 系统错误率超过 1%%","product":"IAC","sre.rule.id":"iac-go-system-errors","severity":"critical"},"annotations":{},"fingerprint":"iac-fp","startsAt":%q,"updatedAt":%q,"status":{"state":"active"}}]`, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer upstream.Close()
	cfg := store.AlertmanagerSourceConfig{APIURL: upstream.URL, LabelScope: domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "IAC"}}, RuleIDAllowlist: []string{"iac-go-system-errors"}}
	config, _ := json.Marshal(cfg)
	hook := uuid.NewString()
	source := ""
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		in, err := store.NewIntegrationRepo().Create(ctx, tx, store.Integration{Kind: "grafana", Name: "scoped-iac", Enabled: true, HookIDHash: fmt.Sprintf("%x", sha256.Sum256([]byte(hook))), DefaultSeverity: "warning", Config: config})
		if err == nil {
			source = in.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	q := NewQueue(st)
	p := NewProjector(st, q, Routing{SourceIDs: []string{source}, PublicRuleIDs: []string{"iac-go-system-errors"}, Fallback: Rule{Name: "iac", Role: "iac", TTL: time.Hour, MaxAttempts: 3}}, nil)
	registry := ingest.NewRegistry()
	registry.Register(ingest.NewGrafanaAdapter(domain.SeverityWarning))
	member := map[string]any{"status": "firing", "labels": map[string]string{"alertname": "IAC Go 系统错误率超过 1%", "product": "IAC", "sre.rule.id": "iac-go-system-errors", "severity": "critical", "secret": "do-not-export"}, "annotations": map[string]string{"summary": "do-not-export"}, "startsAt": at, "endsAt": time.Time{}, "fingerprint": "iac-fp"}
	other := map[string]any{"status": "firing", "labels": map[string]string{"alertname": "Other", "product": "OTHER", "sre.rule.id": "other-rule"}, "startsAt": at, "fingerprint": "other"}
	raw, _ := json.Marshal(map[string]any{"status": "firing", "receiver": "private", "groupKey": "private-group", "alerts": []any{member, other}})
	post := func(record ingest.BatchRecorder, b []byte) int {
		t.Helper()
		srv := ingest.NewServer(registry, ingest.NewPGStore(st).WithBatchRecorder(record), nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/ingest/v1/hook/"+hook, strings.NewReader(string(b))))
		return w.Code
	}
	count := func(table string, want int) {
		t.Helper()
		var got int
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%d want%d err%v", table, got, want, err)
		}
	}
	if code := post(func(c context.Context, tx pgx.Tx, id string, b json.RawMessage, e []domain.AlertEvent) error {
		if err := p.Record(c, tx, id, b, e); err != nil {
			return err
		}
		return errors.New("rollback")
	}, raw); code != 500 {
		t.Fatal(code)
	}
	count("alert_events", 0)
	count("agent_batches", 0)
	count("agent_grafana_occurrences", 0)
	if code := post(p.Record, raw); code != 202 {
		t.Fatal(code)
	}
	count("alert_events", 1)
	count("agent_batches", 1)
	count("agent_grafana_occurrences", 1)
	if found, err := p.projectOne(ctx); !found || err != nil {
		t.Fatal(found, err)
	}
	var envelope Envelope
	if err := st.Pool.QueryRow(ctx, `SELECT envelope FROM agent_messages`).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	var payload projectedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if envelope.Source != "grafana" || payload.Origin != "webhook" || len(payload.Alerts) != 1 || payload.Alerts[0].RuleID != "iac-go-system-errors" {
		t.Fatalf("%+v %+v", envelope, payload)
	}
	ref := payload.Alerts[0].Ref
	if strings.Contains(string(envelope.Payload), "do-not-export") {
		t.Fatal("敏感值泄露")
	}
	// 模拟消息留存清理后迟到webhook，持久收据仍阻止重复分析。
	if _, err := st.Pool.Exec(ctx, `DELETE FROM agent_messages`); err != nil {
		t.Fatal(err)
	}
	if code := post(p.Record, raw); code != 202 {
		t.Fatal(code)
	}
	count("agent_batches", 0)
	r := alertsource.NewReconciler(st, store.NewAlertSourceRepo(), store.NewAlertRepo(), store.NewTimelineRepo(), nil, nil).WithBatchRecorder(p.RecordSnapshot)
	if _, err := r.ReconcileSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	count("alerts", 1)
	count("agent_batches", 0)
	if _, err := r.ReconcileSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	count("agent_batches", 0)
	// 已有occurrence后拒绝范围变更；不把缩小过滤视为恢复。
	changed := cfg
	changed.LabelScope = domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "OTHER"}}
	changedRaw, _ := json.Marshal(changed)
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return store.NewIntegrationRepo().Update(ctx, tx, store.Integration{ID: source, Kind: "grafana", Name: "scoped-iac", Enabled: true, DefaultSeverity: "warning", Config: changedRaw})
	}); !errors.Is(err, store.ErrSourceScopeChanged) {
		t.Fatalf("scope change=%v", err)
	}
	// 无法归属的NoData连续出现不推进快照水位，也不错误解除旧firing。
	mu.Lock()
	body = fmt.Sprintf(`[{"labels":{"alertname":"DatasourceNoData"},"fingerprint":"no-data","startsAt":%q,"status":{"state":"active"}}]`, at.Format(time.RFC3339Nano))
	mu.Unlock()
	for i := 0; i < 4; i++ {
		if _, err := r.ReconcileSource(ctx, source); !errors.Is(err, store.ErrSourceScopeUnverifiable) {
			t.Fatalf("NoData=%v", err)
		}
	}
	var state string
	if err := st.Pool.QueryRow(ctx, `SELECT source_state FROM alerts`).Scan(&state); err != nil || state != "firing" {
		t.Fatal(state, err)
	}
	// 真正完整空快照经过缺失确认后resolved，补投分析仅一次，member ref与webhook一致。
	mu.Lock()
	body = "[]"
	mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := r.ReconcileSource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	count("agent_batches", 1)
	count("agent_grafana_occurrences", 2)
	if _, err := p.projectOne(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT envelope FROM agent_messages`).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if envelope.EventType != "alert.resolved" || payload.Origin != "snapshot" || payload.Alerts[0].Ref != ref {
		t.Fatalf("%+v %+v", envelope, payload)
	}
	member["status"] = "resolved"
	member["endsAt"] = time.Now()
	raw, _ = json.Marshal(map[string]any{"status": "resolved", "receiver": "private", "groupKey": "private-group", "alerts": []any{member}})
	if code := post(p.Record, raw); code != 202 {
		t.Fatal(code)
	}
	count("agent_batches", 0)
}

func TestGrafanaReconcileRejectsConcurrentScopeChange(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; fmt.Fprint(w, "[]") }))
	defer upstream.Close()
	cfg := store.AlertmanagerSourceConfig{APIURL: upstream.URL, LabelScope: domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "IAC"}}}
	raw, _ := json.Marshal(cfg)
	source := ""
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		in, err := store.NewIntegrationRepo().Create(ctx, tx, store.Integration{Kind: "grafana", Name: "scope-change", Enabled: true, DefaultSeverity: "warning", Config: raw})
		if err == nil {
			source = in.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := alertsource.NewReconciler(st, store.NewAlertSourceRepo(), store.NewAlertRepo(), store.NewTimelineRepo(), nil, nil)
	done := make(chan error, 1)
	go func() { _, err := r.ReconcileSource(ctx, source); done <- err }()
	<-started
	cfg.LabelScope = domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "OTHER"}}
	raw, _ = json.Marshal(cfg)
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return store.NewIntegrationRepo().Update(ctx, tx, store.Integration{ID: source, Kind: "grafana", Name: "scope-change", Enabled: true, DefaultSeverity: "warning", Config: raw})
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrSourceScopeChanged) {
		t.Fatalf("old snapshot applied after config change: %v", err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM alert_source_status WHERE snapshot_version>0`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestGrafanaSnapshotFirstAnalysisAndKnownNoDataKeepsBusinessUnknown(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Second)
	payload := func(name, fp string) string {
		b, _ := json.Marshal([]map[string]any{{"labels": map[string]string{"alertname": name, "product": "IAC", "sre.rule.id": "iac-go-system-errors", "severity": "critical"}, "annotations": map[string]string{}, "fingerprint": fp, "startsAt": at, "updatedAt": at, "status": map[string]string{"state": "active"}}})
		return string(b)
	}
	var mu sync.Mutex
	body := payload("IAC Go 系统错误率超过 1%", "business")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); defer mu.Unlock(); fmt.Fprint(w, body) }))
	defer upstream.Close()
	cfg := store.AlertmanagerSourceConfig{APIURL: upstream.URL, LabelScope: domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "IAC"}}, RuleIDAllowlist: []string{"iac-go-system-errors"}}
	raw, _ := json.Marshal(cfg)
	source := ""
	hook := uuid.NewString()
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		in, e := store.NewIntegrationRepo().Create(ctx, tx, store.Integration{Kind: "grafana", Name: "snapshot-first", Enabled: true, DefaultSeverity: "warning", HookIDHash: fmt.Sprintf("%x", sha256.Sum256([]byte(hook))), Config: raw})
		if e == nil {
			source = in.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	p := NewProjector(st, NewQueue(st), Routing{SourceIDs: []string{source}, PublicRuleIDs: []string{"iac-go-system-errors"}, Fallback: Rule{Name: "iac", Role: "iac", TTL: time.Hour, MaxAttempts: 3}}, nil)
	r := alertsource.NewReconciler(st, store.NewAlertSourceRepo(), store.NewAlertRepo(), store.NewTimelineRepo(), nil, nil).WithBatchRecorder(p.RecordSnapshot)
	for i := 0; i < 3; i++ {
		if _, err := r.ReconcileSource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_batches`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if found, err := p.projectOne(ctx); !found || err != nil {
		t.Fatal(found, err)
	}
	var env Envelope
	if err := st.Pool.QueryRow(ctx, `SELECT envelope FROM agent_messages`).Scan(&env); err != nil {
		t.Fatal(err)
	}
	var gp projectedPayload
	if err := json.Unmarshal(env.Payload, &gp); err != nil || gp.Origin != "snapshot" || env.Source != "grafana" || env.EventType != "alert.firing" {
		t.Fatal(string(env.Payload), err)
	}
	// 迟到的真实webhook不得再次派发同一occurrence/phase。
	registry := ingest.NewRegistry()
	registry.Register(ingest.NewGrafanaAdapter(domain.SeverityWarning))
	srv := ingest.NewServer(registry, ingest.NewPGStore(st).WithBatchRecorder(p.Record), nil)
	raw, _ = json.Marshal(map[string]any{"status": "firing", "receiver": "native", "groupKey": "native-group", "alerts": []map[string]any{{"status": "firing", "labels": map[string]string{"alertname": "IAC Go 系统错误率超过 1%", "product": "IAC", "sre.rule.id": "iac-go-system-errors", "severity": "critical"}, "fingerprint": "business", "startsAt": at}}})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/ingest/v1/hook/"+hook, strings.NewReader(string(raw))))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_batches`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}

	// 普通规则丢失/改变归属也必须fail-closed，不能过滤成空快照累计误恢复。
	for _, labels := range []map[string]string{
		{"alertname": "IAC Go 系统错误率超过 1%", "sre.rule.id": "iac-go-system-errors"},
		{"alertname": "IAC Go 系统错误率超过 1%", "product": "OTHER", "sre.rule.id": "iac-go-system-errors"},
		{"alertname": "IAC Go 系统错误率超过 1%", "product": "OTHER", "sre.rule.id": "different-rule"},
		{"alertname": "IAC Go 系统错误率超过 1%"},
	} {
		malformed, _ := json.Marshal([]map[string]any{{"labels": labels, "fingerprint": "business", "startsAt": at, "status": map[string]string{"state": "active"}}})
		mu.Lock()
		body = string(malformed)
		mu.Unlock()
		for i := 0; i < 4; i++ {
			if _, err := r.ReconcileSource(ctx, source); !errors.Is(err, store.ErrSourceScopeUnverifiable) {
				t.Fatalf("malformed snapshot accepted: %v", err)
			}
		}
		var state string
		if err := st.Pool.QueryRow(ctx, `SELECT source_state FROM alerts WHERE upstream_fingerprint='business'`).Scan(&state); err != nil || state != "firing" {
			t.Fatal("malformed误解除", state, err)
		}
		badWebhook, _ := json.Marshal(map[string]any{"status": "firing", "alerts": []map[string]any{{"status": "firing", "labels": labels, "fingerprint": "business", "startsAt": at}}})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/ingest/v1/hook/"+hook, strings.NewReader(string(badWebhook))))
		if w.Code != 422 {
			t.Fatalf("malformed webhook=%d", w.Code)
		}
	}
	// 可归属NoData入库并触发技术分析，但原业务告警只能unknown，不能推断恢复。
	mu.Lock()
	body = payload("DatasourceNoData", "nodata")
	mu.Unlock()
	for i := 0; i < 5; i++ {
		if _, err := r.ReconcileSource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var misses int
	if err := st.Pool.QueryRow(ctx, `SELECT source_state,reconcile_miss_count FROM alerts WHERE upstream_fingerprint='business'`).Scan(&state, &misses); err != nil || state != "unknown" || misses != 0 {
		t.Fatalf("NoData误解除: state=%s misses=%d err=%v", state, misses, err)
	}
	mu.Lock()
	body = payload("DatasourceError", "query-error")
	mu.Unlock()
	for i := 0; i < 4; i++ {
		if _, err := r.ReconcileSource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Pool.QueryRow(ctx, `SELECT source_state FROM alerts WHERE upstream_fingerprint='business'`).Scan(&state); err != nil || state != "unknown" {
		t.Fatal(state, err)
	}
}

func TestGrafanaConcurrentReorderedGroupsHaveOneAnalysisReceipt(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var source string
	if err := st.Pool.QueryRow(ctx, `INSERT INTO integrations(kind,name,hook_id_hash) VALUES('grafana','concurrent-grafana','local-concurrency-hash') RETURNING id::text`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	p := NewProjector(st, NewQueue(st), Routing{SourceIDs: []string{source}, PublicRuleIDs: []string{"iac-rule"}, Fallback: Rule{Name: "iac", Role: "iac", TTL: time.Hour, MaxAttempts: 3}}, nil)
	first := domain.AlertEvent{Source: "grafana", RuleID: "IAC", Labels: map[string]string{"sre.rule.id": "iac-rule"}, UpstreamFingerprint: "a", StartsAt: time.Now().UTC(), Status: domain.StatusFiring, Severity: domain.SeverityCritical}
	second := first
	second.UpstreamFingerprint = "b"
	raw := json.RawMessage(`{"status":"firing","receiver":"iac","groupKey":"same"}`)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		reverse := i%2 == 1
		go func() {
			events := []domain.AlertEvent{first, second}
			if reverse {
				events[0], events[1] = events[1], events[0]
			}
			done <- st.WithTx(ctx, func(tx pgx.Tx) error { return p.Record(ctx, tx, source, raw, events) })
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var receipts, batches int
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_grafana_occurrences),(SELECT count(*) FROM agent_batches)`).Scan(&receipts, &batches); err != nil || receipts != 2 || batches != 1 {
		t.Fatal(receipts, batches, err)
	}
	// 34独立于runtime33；SQL down/up保留旧Source及待投影batch，收据回滚会被删除。
	for _, suffix := range []string{"down", "up"} {
		sql, err := os.ReadFile("../../migrations/000034_grafana_analysis_receipts." + suffix + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	var sources int
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM integrations),(SELECT count(*) FROM agent_batches),(SELECT count(*) FROM agent_grafana_occurrences)`).Scan(&sources, &batches, &receipts); err != nil || sources != 1 || batches != 1 || receipts != 0 {
		t.Fatal(sources, batches, receipts, err)
	}
}
