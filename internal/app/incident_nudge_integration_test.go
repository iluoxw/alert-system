//go:build integration

package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// 本文件是集成测试，setupTestStore/applyMigrations 复用 retention_test.go 里
// 已定义的版本（同包、同 build tag）。实测运行：
//   go test -tags=integration ./internal/app/... -run TestIncidentNudgeHandler -v

func nudgeMustUser(t *testing.T, ctx context.Context, st *store.Store, username string) *store.User {
	t.Helper()
	users := store.NewUserRepo()
	var u *store.User
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		u, err = users.Create(ctx, tx, store.User{Username: username, PasswordHash: "x", DisplayName: username})
		return err
	})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", username, err)
	}
	return u
}

func nudgeMustFault(t *testing.T, ctx context.Context, st *store.Store, key string, status domain.FaultStatus) domain.Fault {
	t.Helper()
	faults := store.NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var fault domain.Fault
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		fault, _, err = faults.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: key, Scope: domain.FaultScopeCapability, Status: status,
			Severity: domain.SeverityCritical, Kind: "nudge_test_fault",
			Title: key, EntityRef: key, Confidence: 0.9, FirstDetected: now, LastEvaluated: now,
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建 Fault %s 失败: %v", key, err)
	}
	return fault
}

// nudgeMustIncident 直接用 store 层建 Incident + primary Fault link，跳过
// incident.Service 的宣布门槛校验——本文件只测 incident_recovery_nudge handler
// 本身的判定与续期逻辑，不重复覆盖 Declare 的门槛语义（那是 internal/incident 的
// service_integration_test.go 的职责）。
func nudgeMustIncident(t *testing.T, ctx context.Context, st *store.Store, actorID, faultID string, status domain.IncidentStatus) domain.Incident {
	t.Helper()
	incidents := store.NewIncidentRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var incident domain.Incident
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		incident, err = incidents.Create(ctx, tx, domain.Incident{
			Status: domain.IncidentStatusDeclared, Severity: domain.SeverityCritical,
			Title: "nudge-test-incident", DeclaredBy: actorID,
			CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/nudge",
			DeclaredAt: now,
		})
		if err != nil {
			return err
		}
		if _, err := incidents.LinkFault(ctx, tx, incident.ID, faultID, domain.IncidentFaultPrimary, now); err != nil {
			return err
		}
		if status != domain.IncidentStatusDeclared {
			incident, err = incidents.UpdateStatus(ctx, tx, incident.ID, domain.IncidentStatusDeclared, status, now)
		}
		return err
	})
	if err != nil {
		t.Fatalf("创建测试 Incident 失败: %v", err)
	}
	return incident
}

// nudgeMustAlert 建一条 Alert 作为证据，status 决定是否已解除（本文件只测非
// 权威来源分支：status=resolved 时对应"看 alerts.status"这条判定路径）。
func nudgeMustAlert(t *testing.T, ctx context.Context, st *store.Store, fingerprint, alertname string, status domain.Status) domain.Alert {
	t.Helper()
	alerts := store.NewAlertRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var alert domain.Alert
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		alert, _, err = alerts.UpsertByFingerprint(ctx, tx, domain.AlertEvent{
			Fingerprint: fingerprint, RuleID: "rule", SignalKind: domain.SignalKindMetric,
			Severity: domain.SeverityCritical, Status: domain.StatusFiring, Title: alertname,
			Labels: map[string]string{"alertname": alertname}, Source: "test",
			ResolvePolicy: domain.ResolvePolicyManual, StartsAt: now,
		}, "")
		if err != nil {
			return err
		}
		if status == domain.StatusResolved {
			alert.Status = domain.StatusResolved
			alert.EndsAt = now
			if err := alerts.UpdateStatus(ctx, tx, alert); err != nil {
				return err
			}
		}
		alert, err = alerts.GetByID(ctx, tx, alert.ID)
		return err
	})
	if err != nil {
		t.Fatalf("创建测试 Alert %s 失败: %v", fingerprint, err)
	}
	return alert
}

func nudgeLinkEvidence(t *testing.T, ctx context.Context, st *store.Store, faultID, alertID, decidedBy string, role domain.EvidenceRole) {
	t.Helper()
	faults := store.NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := faults.UpsertHumanEvidence(ctx, tx, domain.FaultEvidence{
			FaultID: faultID, AlertID: alertID, Role: role, Rationale: "test",
			Confidence: 0.9, DecidedBy: decidedBy, DecidedAt: now,
		})
		return err
	})
	if err != nil {
		t.Fatalf("关联证据失败(fault=%s alert=%s): %v", faultID, alertID, err)
	}
}

// pendingTimerFireAt 查询 kind+refID 当前 pending 记录的 fire_at；不存在返回 found=false。
func pendingTimerFireAt(t *testing.T, ctx context.Context, st *store.Store, kind, refID string) (time.Time, bool) {
	t.Helper()
	var fireAt time.Time
	err := st.Pool.QueryRow(ctx, `
		SELECT fire_at FROM timers WHERE kind = $1 AND ref_id = $2 AND status = 'pending'
	`, kind, refID).Scan(&fireAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return time.Time{}, false
		}
		t.Fatalf("查询 pending 定时器失败: %v", err)
	}
	return fireAt, true
}

// countNotifySendTimers 统计 notify_send 定时器里 ref_id 前缀匹配 incidentID 的
// pending 行数——notifySendRefID 用 "\x1e" 拼接业务元组，前缀匹配足够定位到
// 这条 Incident 恢复提醒产生的唯一一条发送意图。
func countNotifySendTimers(t *testing.T, ctx context.Context, st *store.Store, incidentID string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM timers WHERE kind = 'notify_send' AND status = 'pending' AND ref_id LIKE $1
	`, incidentID+"%").Scan(&n); err != nil {
		t.Fatalf("统计 notify_send 定时器失败: %v", err)
	}
	return n
}

func latestNotifySendBody(t *testing.T, ctx context.Context, st *store.Store, incidentID string) (string, int64) {
	t.Helper()
	var payload []byte
	if err := st.Pool.QueryRow(ctx, `
		SELECT payload FROM timers WHERE kind = 'notify_send' AND status = 'pending' AND ref_id LIKE $1
		ORDER BY id DESC LIMIT 1
	`, incidentID+"%").Scan(&payload); err != nil {
		t.Fatalf("查询 notify_send payload 失败: %v", err)
	}
	var req notify.SendRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("反序列化 notify_send payload 失败: %v", err)
	}
	return req.Msg.Body, req.Round
}

// TestIncidentNudgeHandler_DisabledIntervalNoop 覆盖：interval<=0 时 handler
// 直接无副作用返回，不产生任何通知意图，也不续挂下一轮。
func TestIncidentNudgeHandler_DisabledIntervalNoop(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-disabled-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-disabled-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusMonitoring)
	alert := nudgeMustAlert(t, ctx, st, "nudge-disabled-fp", "AlertA", domain.StatusResolved)
	nudgeLinkEvidence(t, ctx, st, fault.ID, alert.ID, actor.ID, domain.EvidenceRoleDirectImpact)

	fakeSched := &recordingScheduler{}
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: fakeSched, nudgeInterval: 0, now: time.Now}, fakeSched, 0)
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, incident.ID, nil)
	}); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if len(fakeSched.scheduled) != 0 {
		t.Fatalf("关闭配置时不应续挂, got %+v", fakeSched.scheduled)
	}
	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 0 {
		t.Fatalf("关闭配置时不应发送通知, got %d", n)
	}
}

// TestIncidentNudgeHandler_NotAllClearedReschedulesWithoutNotify 覆盖：证据未
// 全部解除——不发通知，但仍在同一事务内续挂下一轮。
func TestIncidentNudgeHandler_NotAllClearedReschedulesWithoutNotify(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-partial-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-partial-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusMonitoring)
	clearedAlert := nudgeMustAlert(t, ctx, st, "nudge-partial-fp-1", "AlertCleared", domain.StatusResolved)
	firingAlert := nudgeMustAlert(t, ctx, st, "nudge-partial-fp-2", "AlertStillFiring", domain.StatusFiring)
	nudgeLinkEvidence(t, ctx, st, fault.ID, clearedAlert.ID, actor.ID, domain.EvidenceRoleDirectImpact)
	nudgeLinkEvidence(t, ctx, st, fault.ID, firingAlert.ID, actor.ID, domain.EvidenceRoleDirectImpact)

	interval := time.Hour
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: scheduler.New(st, time.Second), nudgeInterval: 0, now: time.Now},
		scheduler.New(st, time.Second), interval)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, incident.ID, nil)
	}); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 0 {
		t.Fatalf("证据未全部解除不应发送通知, got %d", n)
	}
	if _, found := pendingTimerFireAt(t, ctx, st, IncidentRecoveryNudgeTimerKind, incident.ID); !found {
		t.Fatalf("证据未全部解除仍应续挂下一轮")
	}
}

// TestIncidentNudgeHandler_AllClearedQueuesNudgeWithFireTimeRound 覆盖：全部有效
// 证据已解除——发送提醒，Round 使用本次 fire 时刻，文案包含人工确认提示、
// 不包含"已恢复"字样，并列出证据 alertname。
func TestIncidentNudgeHandler_AllClearedQueuesNudgeWithFireTimeRound(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-cleared-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-cleared-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusMonitoring)
	alert := nudgeMustAlert(t, ctx, st, "nudge-cleared-fp", "E2BNomadAgentDown", domain.StatusResolved)
	nudgeLinkEvidence(t, ctx, st, fault.ID, alert.ID, actor.ID, domain.EvidenceRoleDirectImpact)

	fireAt := time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)
	interval := time.Hour
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: scheduler.New(st, time.Second), nudgeInterval: 0, now: func() time.Time { return fireAt }},
		scheduler.New(st, time.Second), interval)
	h.now = func() time.Time { return fireAt }

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, incident.ID, nil)
	}); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 1 {
		t.Fatalf("证据全部解除应恰好发送一条通知, got %d", n)
	}
	body, round := latestNotifySendBody(t, ctx, st, incident.ID)
	if round != fireAt.UnixNano() {
		t.Fatalf("Round 应等于本次 fire 时刻, got %d want %d", round, fireAt.UnixNano())
	}
	if !strings.Contains(body, "告警解除可能源于采集目标下线或服务发现注销，请人工确认实际恢复后再推进 Incident") {
		t.Fatalf("文案缺少人工确认提示: %s", body)
	}
	if strings.Contains(body, "已恢复") {
		t.Fatalf("文案不应出现'已恢复'字样: %s", body)
	}
	if !strings.Contains(body, "E2BNomadAgentDown") {
		t.Fatalf("文案应列出证据 alertname: %s", body)
	}
	if _, found := pendingTimerFireAt(t, ctx, st, IncidentRecoveryNudgeTimerKind, incident.ID); !found {
		t.Fatalf("发送提醒后仍应续挂下一轮")
	}
}

// TestIncidentNudgeHandler_ResolvedIncidentNoReschedule 覆盖：Incident 已 resolved
// 时 handler 直接返回，不发送、不续挂（正常路径由 CancelRecoveryNudge 在事务内
// 取消，这里验证兜底分支同样安全）。
func TestIncidentNudgeHandler_ResolvedIncidentNoReschedule(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-resolved-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-resolved-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusResolved)
	alert := nudgeMustAlert(t, ctx, st, "nudge-resolved-fp", "AlertA", domain.StatusResolved)
	nudgeLinkEvidence(t, ctx, st, fault.ID, alert.ID, actor.ID, domain.EvidenceRoleDirectImpact)

	sched := scheduler.New(st, time.Second)
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: sched, nudgeInterval: 0, now: time.Now}, sched, time.Hour)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, incident.ID, nil)
	}); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 0 {
		t.Fatalf("已 resolved 的 Incident 不应发送通知, got %d", n)
	}
	if _, found := pendingTimerFireAt(t, ctx, st, IncidentRecoveryNudgeTimerKind, incident.ID); found {
		t.Fatalf("已 resolved 的 Incident 不应续挂")
	}
}

// TestIncidentNudgeHandler_ExcludedEvidenceIgnored 覆盖：人工判为 excluded 的
// 证据不参与判定——即便它仍在 firing，只要其余有效证据全部解除就应发送提醒。
func TestIncidentNudgeHandler_ExcludedEvidenceIgnored(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-excluded-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-excluded-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusMonitoring)
	clearedAlert := nudgeMustAlert(t, ctx, st, "nudge-excluded-fp-1", "AlertCleared", domain.StatusResolved)
	excludedAlert := nudgeMustAlert(t, ctx, st, "nudge-excluded-fp-2", "AlertExcludedStillFiring", domain.StatusFiring)
	nudgeLinkEvidence(t, ctx, st, fault.ID, clearedAlert.ID, actor.ID, domain.EvidenceRoleDirectImpact)
	nudgeLinkEvidence(t, ctx, st, fault.ID, excludedAlert.ID, actor.ID, domain.EvidenceRoleExcluded)

	sched := scheduler.New(st, time.Second)
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: sched, nudgeInterval: 0, now: time.Now}, sched, time.Hour)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, incident.ID, nil)
	}); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 1 {
		t.Fatalf("排除 excluded 证据后其余全部解除应发送提醒, got %d", n)
	}
	body, _ := latestNotifySendBody(t, ctx, st, incident.ID)
	if strings.Contains(body, "AlertExcludedStillFiring") {
		t.Fatalf("excluded 证据不应出现在文案里: %s", body)
	}
}

// TestIncidentNudgeHandler_DuplicateFireNoDuplicateIntent 覆盖：同一条
// incident_recovery_nudge 定时器因崩溃重投被再次处理时（模拟两次独立事务各自
// 调用 Handle），不产生重复的 notify_send 发送意图——notifySendRefID 不含
// round 维度，Schedule 对同 (kind, refID) 是替换语义，天然收敛成一条 pending 记录。
func TestIncidentNudgeHandler_DuplicateFireNoDuplicateIntent(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := nudgeMustUser(t, ctx, st, "nudge-dup-actor")
	fault := nudgeMustFault(t, ctx, st, "nudge-dup-fault", domain.FaultStatusActive)
	incident := nudgeMustIncident(t, ctx, st, actor.ID, fault.ID, domain.IncidentStatusMonitoring)
	alert := nudgeMustAlert(t, ctx, st, "nudge-dup-fp", "AlertA", domain.StatusResolved)
	nudgeLinkEvidence(t, ctx, st, fault.ID, alert.ID, actor.ID, domain.EvidenceRoleDirectImpact)

	fireAt := time.Now().UTC()
	sched := scheduler.New(st, time.Second)
	h := newIncidentNudgeHandler(store.NewIncidentRepo(), store.NewFaultRepo(), store.NewAlertRepo(),
		&IncidentOutbox{scheduler: sched, nudgeInterval: 0, now: func() time.Time { return fireAt }},
		sched, time.Hour)
	h.now = func() time.Time { return fireAt }

	run := func() {
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			return h.Handle(ctx, tx, incident.ID, nil)
		}); err != nil {
			t.Fatalf("Handle 失败: %v", err)
		}
	}
	run()
	run() // 模拟同一次 fire 被重复投递

	if n := countNotifySendTimers(t, ctx, st, incident.ID); n != 1 {
		t.Fatalf("重复投递同一 fire 不应产生重复发送意图, got %d", n)
	}
}

// recordingScheduler 只用于验证"关闭配置时 handler 完全不调用 Schedule"，不需要
// 真的落库；其余测试直接用真实 *scheduler.Scheduler，让 Schedule 的替换/去重语义
// 参与断言（更贴近真实行为）。
type recordingScheduler struct {
	scheduled []string
}

func (s *recordingScheduler) Schedule(_ context.Context, _ pgx.Tx, kind, refID string, _ time.Time, _ []byte) error {
	s.scheduled = append(s.scheduled, kind+"/"+refID)
	return nil
}

func (s *recordingScheduler) ScheduleIfAbsent(_ context.Context, _ pgx.Tx, _, _ string, _ time.Time, _ []byte) (bool, error) {
	return false, nil
}

func (s *recordingScheduler) Cancel(_ context.Context, _ pgx.Tx, _, _ string) error { return nil }
