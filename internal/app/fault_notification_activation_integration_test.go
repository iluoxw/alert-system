//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/mapping"
	"alert-system/internal/notify"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
	"github.com/jackc/pgx/v5"
)

type activationVersionBarrierTx struct {
	pgx.Tx
	selected chan struct{}
	release  chan struct{}
	once     bool
}
type activationVersionBarrierRow struct {
	pgx.Row
	gate func()
}

func (r activationVersionBarrierRow) Scan(out ...any) error {
	e := r.Row.Scan(out...)
	if e == nil {
		r.gate()
	}
	return e
}
func (tx *activationVersionBarrierTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if !tx.once && strings.Contains(sql, "FROM faults WHERE correlation_key = $1") {
		tx.once = true
		return activationVersionBarrierRow{row, func() { close(tx.selected); <-tx.release }}
	}
	return row
}

func TestFaultNotificationConcurrentActivationVersion(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := store.NewFaultRepo()
	topology := store.NewTopologyRepo()
	repo := store.NewFaultNotificationRepo()
	p := mapping.NewProjector(store.NewAlertMappingRepo(), store.NewServiceCatalogRepo(), topology, faults, nil)
	sched := scheduler.New(st, time.Millisecond)
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "secret", "")
	o.deliveries = repo
	var alert domain.Alert
	var f domain.Fault
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, e := topology.ImportSnapshot(ctx, tx, domain.TopologySnapshot{SchemaVersion: 1, Source: "review", ObservedAt: now, Nodes: []domain.ResourceNode{{ExternalRef: "gateway", Name: "Gateway", Kind: domain.ResourceNodeKindInfrastructureCapability}}})
		if e != nil {
			return e
		}
		node, e := topology.GetByExternalRef(ctx, tx, "review", "gateway")
		if e != nil {
			return e
		}
		alert, _, e = store.NewAlertRepo().UpsertByFingerprint(ctx, tx, domain.AlertEvent{Fingerprint: "review-fp", ResolvePolicy: domain.ResolvePolicySource, RuleID: "test", Severity: domain.SeverityCritical, Status: domain.StatusFiring, Title: "test", Source: "generic", StartsAt: now, Labels: map[string]string{"sre.entity.key": "review:gateway", "sre.fault.kind": "unavailable", "sre.evidence.role": "direct_impact"}}, "")
		if e != nil {
			return e
		}
		sum := sha256.Sum256([]byte("review:gateway\x00unavailable"))
		key := "fault:review:gateway:unavailable:" + hex.EncodeToString(sum[:6])
		f, _, e = faults.Upsert(ctx, tx, domain.Fault{CorrelationKey: key, Scope: domain.FaultScopeCapability, Status: domain.FaultStatusCandidate, Severity: domain.SeverityCritical, Kind: "unavailable", Title: "candidate", ResourceNodeID: node.ID, EntityRef: node.EntityKey, Confidence: 1, FirstDetected: now, LastEvaluated: now})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	alert.Source = "alertmanager"
	alert.SourceIntegrationID = "source"
	alert.UpstreamFingerprint = "fp"
	alert.SourceState = domain.SourceStateFiring
	selected, release := make(chan struct{}), make(chan struct{})
	staleDone := make(chan error, 1)
	go func() {
		staleDone <- st.WithTx(ctx, func(tx pgx.Tx) error {
			wrapped := &activationVersionBarrierTx{Tx: tx, selected: selected, release: release}
			result, e := p.Project(ctx, wrapped, alert)
			if e != nil {
				return e
			}
			return o.TriggerFromMappingResult(ctx, wrapped, result)
		})
	}()
	select {
	case <-selected:
	case <-time.After(3 * time.Second):
		t.Fatal("未读到旧candidate")
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, e := p.Project(ctx, tx, alert)
		if e != nil {
			return e
		}
		return o.TriggerFromMappingResult(ctx, tx, result)
	}); err != nil {
		t.Fatal(err)
	}
	ch := &notificationTestChannel{}
	notify.Register(ch)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	sender.BeforeFaultSend = o.BeforeSend
	sender.OnFaultResult = o.RecordResult
	sendLatest := func() {
		var req notify.SendRequest
		var raw []byte
		if err := st.Pool.QueryRow(ctx, `SELECT payload FROM timers WHERE kind='notify_send' AND payload->>'fault_notification_id' IN (SELECT id::text FROM fault_notification_deliveries ) ORDER BY fire_at DESC LIMIT 1`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if err := sender.Send(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	sendLatest()
	close(release)
	if err := <-staleDone; err != nil {
		t.Fatal(err)
	}
	sendLatest()
	var deliveryCount int
	var version, watched int64
	if e := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1),activation_version,(SELECT epoch FROM fault_notification_watches WHERE fault_id=$1) FROM faults WHERE id=$1`, f.ID).Scan(&deliveryCount, &version, &watched); e != nil {
		t.Fatal(e)
	}
	if deliveryCount != 1 || version != 2 || watched != 2 {
		t.Fatalf("重复旧Transition改变激活批次: deliveries=%d version=%d watched=%d", deliveryCount, version, watched)
	}
	if ch.calls.Load() != 1 {
		t.Fatalf("同一candidate→active转移被发送%d次，fault=%s", ch.calls.Load(), f.ID)
	}
}

func TestFaultNotificationSilentReactivationVersion(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := store.NewFaultRepo()
	topology := store.NewTopologyRepo()
	repo := store.NewFaultNotificationRepo()
	p := mapping.NewProjector(store.NewAlertMappingRepo(), store.NewServiceCatalogRepo(), topology, faults, nil)
	sched := scheduler.New(st, time.Millisecond)
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "secret", "")
	o.deliveries = repo
	var alert domain.Alert
	var f domain.Fault
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, e := topology.ImportSnapshot(ctx, tx, domain.TopologySnapshot{SchemaVersion: 1, Source: "review", ObservedAt: now, Nodes: []domain.ResourceNode{{ExternalRef: "gateway", Name: "Gateway", Kind: domain.ResourceNodeKindInfrastructureCapability}}})
		if e != nil {
			return e
		}
		node, e := topology.GetByExternalRef(ctx, tx, "review", "gateway")
		if e != nil {
			return e
		}
		alert, _, e = store.NewAlertRepo().UpsertByFingerprint(ctx, tx, domain.AlertEvent{Fingerprint: "review-fp", ResolvePolicy: domain.ResolvePolicySource, RuleID: "test", Severity: domain.SeverityCritical, Status: domain.StatusFiring, Title: "test", Source: "generic", StartsAt: now, Labels: map[string]string{"sre.entity.key": "review:gateway", "sre.fault.kind": "unavailable", "sre.evidence.role": "direct_impact"}}, "")
		if e != nil {
			return e
		}
		sum := sha256.Sum256([]byte("review:gateway\x00unavailable"))
		key := "fault:review:gateway:unavailable:" + hex.EncodeToString(sum[:6])
		f, _, e = faults.Upsert(ctx, tx, domain.Fault{CorrelationKey: key, Scope: domain.FaultScopeCapability, Status: domain.FaultStatusCandidate, Severity: domain.SeverityCritical, Kind: "unavailable", Title: "candidate", ResourceNodeID: node.ID, EntityRef: node.EntityKey, Confidence: 1, FirstDetected: now, LastEvaluated: now})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	alert.Source = "alertmanager"
	alert.SourceIntegrationID = "source"
	alert.UpstreamFingerprint = "fp"
	alert.SourceState = domain.SourceStateFiring

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, e := p.Project(ctx, tx, alert)
		if e != nil {
			return e
		}
		return o.TriggerFromMappingResult(ctx, tx, result)
	}); err != nil {
		t.Fatal(err)
	}
	ch := &notificationTestChannel{}
	notify.Register(ch)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	sender.BeforeFaultSend = o.BeforeSend
	sender.OnFaultResult = o.RecordResult
	var req notify.SendRequest
	var raw []byte
	if err := st.Pool.QueryRow(ctx, `SELECT payload FROM timers WHERE kind='notify_send' ORDER BY fire_at DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := faults.UpdateLifecycleStatus(ctx, tx, f.ID, domain.FaultStatusActive, domain.FaultStatusMonitoring, time.Now())
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// 与HTTP reprocess完全一致：调用Project，不调用通知回调。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, e := p.Project(ctx, tx, alert)
		if e == nil && result.Transition != domain.FaultTransitionEnteredActive {
			t.Fatal("fixture未重激活")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// 旧watch虽然due，也不能把静默重激活的新领域版本自动登记。
	o.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if e := st.WithTx(ctx, func(tx pgx.Tx) error { return o.HandleReconcile(ctx, tx, "global", nil) }); e != nil {
		t.Fatal(e)
	}
	// 模拟Due先取旧候选、然后静默Project才提交：逐项锁内仍必须重新校验版本。
	if e := st.WithTx(ctx, func(tx pgx.Tx) error { return o.queue(ctx, tx, f, false, true) }); e != nil {
		t.Fatal(e)
	}
	var version, watched int64
	var deliveryCount int
	var notified bool
	if e := st.Pool.QueryRow(ctx, `SELECT activation_version,(SELECT epoch FROM fault_notification_watches WHERE fault_id=$1),(SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1),owner_notified_at IS NOT NULL FROM faults WHERE id=$1`, f.ID).Scan(&version, &watched, &deliveryCount, &notified); e != nil {
		t.Fatal(e)
	}
	if version != 3 || watched != 2 || deliveryCount != 1 || notified {
		t.Fatalf("静默重激活被后台登记/保留旧送达: version=%d watched=%d deliveries=%d notified=%v", version, watched, deliveryCount, notified)
	}
	var preview FaultNotificationPreview
	if e := st.WithTx(ctx, func(tx pgx.Tx) error { var e error; preview, e = o.Preview(ctx, tx, f.ID, false); return e }); e != nil {
		t.Fatal(e)
	}
	if len(preview.Targets) != 1 || preview.Targets[0].Action != "queue" {
		t.Fatalf("静默新版本预览错误: %+v", preview)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error { var e error; preview, e = o.Submit(ctx, tx, f.ID, false, "", "admin"); return e }); err != nil {
		t.Fatal(err)
	}
	if len(preview.Targets) != 1 || preview.Targets[0].Action != "queued" {
		t.Fatalf("新激活批次显式补投错误复用上轮结果: %+v", preview)
	}
	if e := st.Pool.QueryRow(ctx, `SELECT payload FROM timers WHERE kind='notify_send' ORDER BY fire_at DESC LIMIT 1`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(raw, &req); e != nil {
		t.Fatal(e)
	}
	if e := sender.Send(ctx, req); e != nil {
		t.Fatal(e)
	}
	if ch.calls.Load() != 2 {
		t.Fatal("真实新激活批次没有独立通知")
	}

}
