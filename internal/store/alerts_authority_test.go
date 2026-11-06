//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// TestAlertmanagerAuthority_ReconcileLifecycle 固化 Alertmanager 权威镜像最重要的
// 失败语义：响应状态与来源状态正交；一次成功快照缺失只会进入 unknown；只有连续
// 成功快照缺失才能恢复；API 失败本身既不推进 snapshot，也不伪造 resolved；迟到的
// 旧事件不能覆盖更新的来源事实。
func TestAlertmanagerAuthority_ReconcileLifecycle(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alerts := NewAlertRepo()
	sources := NewAlertSourceRepo()
	integrations := NewIntegrationRepo()

	var sourceID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		created, err := integrations.Create(ctx, tx, Integration{
			Kind:            "alertmanager",
			Name:            "authority-reconcile-test",
			HookIDHash:      "test-hook-id-hash",
			DefaultSeverity: string(domain.SeverityWarning),
			Enabled:         true,
			Config: json.RawMessage(`{
				"api_url":"http://alertmanager.test",
				"reconcile_interval_seconds":30,
				"stale_after_seconds":120,
				"missing_confirmations":2
			}`),
		})
		if err != nil {
			return err
		}
		sourceID = created.ID
		return nil
	})
	if err != nil {
		t.Fatalf("创建 Alertmanager source 失败: %v", err)
	}

	startsAt := time.Now().UTC().Truncate(time.Millisecond)
	updatedAt := startsAt.Add(time.Minute)
	event := domain.AlertEvent{
		Fingerprint:         "local-derived-fingerprint",
		UpstreamFingerprint: "am-fingerprint-01",
		RuleID:              "HostDown",
		SignalKind:          domain.SignalKindMetric,
		Severity:            domain.SeverityCritical,
		Status:              domain.StatusFiring,
		Title:               "HostDown: node-1",
		Labels:              map[string]string{"alertname": "HostDown", "instance": "node-1"},
		Source:              "alertmanager",
		ResourceRef:         map[string]string{"host.name": "node-1"},
		ResolvePolicy:       domain.ResolvePolicySource,
		SignalContext:       json.RawMessage(`{"generator_url":"http://prometheus.test/graph"}`),
		StartsAt:            startsAt,
		SourceUpdatedAt:     updatedAt,
		SuppressionState:    domain.SuppressionStateSilenced,
		SuppressionDetails:  map[string][]string{"silenced_by": {"silence-01"}},
	}

	var alertID string
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		generation, err := sources.BeginSuccessfulSnapshot(ctx, tx, sourceID, updatedAt)
		if err != nil {
			return err
		}
		alert, created, _, err := alerts.UpsertAuthoritativeOccurrence(ctx, tx, event, sourceID, updatedAt, generation)
		if err != nil {
			return err
		}
		if !created {
			t.Fatal("首次来源快照应创建 occurrence")
		}
		alertID = alert.ID
		return nil
	})
	if err != nil {
		t.Fatalf("应用首次 Alertmanager 快照失败: %v", err)
	}

	// Ack 只能改变平台响应轴，不能篡改 Alertmanager 的 firing/silenced 事实。
	ackAt := updatedAt.Add(time.Second)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, err := alerts.GetByID(ctx, tx, alertID)
		if err != nil {
			return err
		}
		updated, _, err := domain.Ack(alert, "", "integration-test", ackAt)
		if err != nil {
			return err
		}
		return alerts.UpdateStatus(ctx, tx, updated)
	})
	if err != nil {
		t.Fatalf("Ack 权威 Alert 失败: %v", err)
	}
	assertAuthorityAxes(t, st, alertID, domain.SourceStateFiring, domain.ResponseStateAcknowledged, domain.SuppressionStateSilenced, 0)

	// 第一次完整快照缺失：只能 unknown，不能根据“没看见”立即宣布恢复。
	missingAt1 := updatedAt.Add(2 * time.Minute)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		generation, err := sources.BeginSuccessfulSnapshot(ctx, tx, sourceID, missingAt1)
		if err != nil {
			return err
		}
		changed, err := alerts.MarkMissingAfterSnapshot(ctx, tx, sourceID, generation, 2, missingAt1)
		if err != nil {
			return err
		}
		if len(changed) != 1 || changed[0].SourceState != domain.SourceStateUnknown {
			t.Fatalf("首次缺失 changed = %+v, want 单条 unknown", changed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("应用首次缺失快照失败: %v", err)
	}
	assertAuthorityAxes(t, st, alertID, domain.SourceStateUnknown, domain.ResponseStateAcknowledged, domain.SuppressionStateSilenced, 1)

	// 对账 HTTP 失败只记录 source 健康事实，不允许调用缺失处理。
	failureAt := missingAt1.Add(time.Minute)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return sources.RecordFailure(ctx, tx, sourceID, failureAt, errors.New("upstream 503"))
	})
	if err != nil {
		t.Fatalf("记录对账失败状态失败: %v", err)
	}
	assertAuthorityAxes(t, st, alertID, domain.SourceStateUnknown, domain.ResponseStateAcknowledged, domain.SuppressionStateSilenced, 1)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		source, err := sources.GetByID(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if source.SnapshotVersion != 2 || source.ConsecutiveFailures != 1 || source.Health(failureAt) != AlertSourceHealthDegraded {
			t.Fatalf("失败后 source = %+v, want snapshot=2/failures=1/degraded", source)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验 source 失败水位失败: %v", err)
	}

	// 第二次成功快照仍缺失，达到确认门槛后才由来源对账恢复。
	missingAt2 := failureAt.Add(time.Minute)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		generation, err := sources.BeginSuccessfulSnapshot(ctx, tx, sourceID, missingAt2)
		if err != nil {
			return err
		}
		changed, err := alerts.MarkMissingAfterSnapshot(ctx, tx, sourceID, generation, 2, missingAt2)
		if err != nil {
			return err
		}
		if len(changed) != 1 || changed[0].SourceState != domain.SourceStateResolved {
			t.Fatalf("第二次缺失 changed = %+v, want 单条 resolved", changed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("应用第二次缺失快照失败: %v", err)
	}
	assertAuthorityAxes(t, st, alertID, domain.SourceStateResolved, domain.ResponseStateAcknowledged, domain.SuppressionStateSilenced, 2)

	// 即便后续快照中收到同 occurrence，较旧 updatedAt 也不能把 resolved 回滚为 firing。
	stale := event
	stale.SourceUpdatedAt = updatedAt.Add(-time.Second)
	stale.SuppressionState = domain.SuppressionStateNone
	stale.SuppressionDetails = nil
	staleAt := missingAt2.Add(time.Minute)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		generation, err := sources.BeginSuccessfulSnapshot(ctx, tx, sourceID, staleAt)
		if err != nil {
			return err
		}
		alert, created, _, err := alerts.UpsertAuthoritativeOccurrence(ctx, tx, stale, sourceID, staleAt, generation)
		if err != nil {
			return err
		}
		if created || alert.ID != alertID || alert.SourceState != domain.SourceStateResolved {
			t.Fatalf("迟到事件不应新建或回滚 occurrence: %+v, created=%v", alert, created)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("应用迟到事件失败: %v", err)
	}
	assertAuthorityAxes(t, st, alertID, domain.SourceStateResolved, domain.ResponseStateAcknowledged, domain.SuppressionStateSilenced, 0)
}

// TestGrafanaOccurrenceIdempotentReplay 证明 Grafana 载荷沿用同一 occurrence
// 幂等键（source_id + upstream_fingerprint + starts_at）：重复投递不产生第二行。
func TestGrafanaOccurrenceIdempotentReplay(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alerts := NewAlertRepo()
	integrations := NewIntegrationRepo()

	var sourceID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		created, err := integrations.Create(ctx, tx, Integration{
			Kind:            domain.SourceKindGrafana,
			Name:            "grafana-idempotency-test",
			HookIDHash:      "test-grafana-hook-hash",
			DefaultSeverity: string(domain.SeverityWarning),
			Enabled:         true,
			Config:          json.RawMessage(`{"api_url":"http://grafana.test"}`),
		})
		if err != nil {
			return err
		}
		sourceID = created.ID
		return nil
	}); err != nil {
		t.Fatalf("创建 Grafana source 失败: %v", err)
	}

	startsAt := time.Now().UTC().Truncate(time.Millisecond)
	event := domain.AlertEvent{
		UpstreamFingerprint: "gf-fingerprint-01",
		RuleID:              "HighMemoryUsage",
		SignalKind:          domain.SignalKindMetric,
		Severity:            domain.SeverityWarning,
		Status:              domain.StatusFiring,
		Title:               "HighMemoryUsage",
		Labels:              map[string]string{"alertname": "HighMemoryUsage"},
		Source:              domain.SourceKindGrafana,
		ResolvePolicy:       domain.ResolvePolicySource,
		StartsAt:            startsAt,
		SourceUpdatedAt:     startsAt,
		SuppressionState:    domain.SuppressionStateNone,
	}

	var firstID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, created, _, err := alerts.UpsertAuthoritativeOccurrence(ctx, tx, event, sourceID, startsAt, 0)
		if err != nil {
			return err
		}
		if !created {
			t.Fatal("首次 Grafana occurrence 应创建")
		}
		firstID = alert.ID
		return nil
	}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, created, _, err := alerts.UpsertAuthoritativeOccurrence(ctx, tx, event, sourceID, startsAt.Add(time.Second), 0)
		if err != nil {
			return err
		}
		if created {
			t.Fatal("同 fingerprint+starts_at 重复投递不得新建行")
		}
		if alert.ID != firstID {
			t.Fatalf("重复投递落到不同 ID: %s vs %s", alert.ID, firstID)
		}
		return nil
	}); err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}

	var count int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE source_integration_id = $1::uuid AND upstream_fingerprint = $2`,
		sourceID, event.UpstreamFingerprint,
	).Scan(&count); err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("Grafana occurrence 行数 = %d, want 1", count)
	}
}

func assertAuthorityAxes(
	t *testing.T,
	st *Store,
	alertID string,
	wantSource domain.SourceState,
	wantResponse domain.ResponseState,
	wantSuppression domain.SuppressionState,
	wantMisses int,
) {
	t.Helper()
	err := st.WithTx(context.Background(), func(tx pgx.Tx) error {
		alert, err := NewAlertRepo().GetByID(context.Background(), tx, alertID)
		if err != nil {
			return err
		}
		if alert.SourceState != wantSource || alert.ResponseState != wantResponse || alert.SuppressionState != wantSuppression || alert.ReconcileMissCount != wantMisses {
			t.Fatalf(
				"axes=(%s,%s,%s,miss=%d), want=(%s,%s,%s,miss=%d)",
				alert.SourceState, alert.ResponseState, alert.SuppressionState, alert.ReconcileMissCount,
				wantSource, wantResponse, wantSuppression, wantMisses,
			)
		}
		if alert.Status != domain.ProjectLegacyStatus(alert.SourceState, alert.ResponseState) {
			t.Fatalf("legacy status=%s 与三轴投影不一致", alert.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("读取 Alert 权威轴失败: %v", err)
	}
}
