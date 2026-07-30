package oncall

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// fakeTx 让 WithTx 直接以 nil pgx.Tx 调用 fn：本包测试里的 fake 仓储都不真正使用
// tx 参数，只是透传满足签名（与 notify/sender_test.go 的 fakeTx 同一模式）。
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}

// settableClock 是 Clock 的可控实现，测试用它精确推进"当前时间"而不依赖 wall-clock。
type settableClock struct{ t time.Time }

func (c *settableClock) Now() time.Time { return c.t }

// ---- fake ScheduleRepo / ShiftRepo（Resolver 依赖） ----

type fakeScheduleRepo struct {
	byTeam map[string][]store.Schedule
}

func (f *fakeScheduleRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.Schedule, error) {
	return f.byTeam[teamID], nil
}

type fakeShiftRepo struct {
	byScheduleID map[string][]store.Shift
}

func (f *fakeShiftRepo) ListInWindow(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, limit int) ([]store.Shift, error) {
	var out []store.Shift
	for _, sh := range f.byScheduleID[scheduleID] {
		if sh.StartsAt.Before(to) && sh.EndsAt.After(from) {
			out = append(out, sh)
		}
	}
	return out, nil
}

// ---- fake AlertRepo / TimelineRepo / EscalationPolicyRepo / TimerScheduler / Notifier（Chain 依赖） ----

type fakeAlertRepo struct {
	alerts map[string]domain.Alert
}

func (f *fakeAlertRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	a, ok := f.alerts[id]
	if !ok {
		return domain.Alert{}, store.ErrAlertNotFound
	}
	return a, nil
}

type fakeTimelineRepo struct {
	entries map[string][]domain.TimelineEntry
}

func newFakeTimelineRepo() *fakeTimelineRepo {
	return &fakeTimelineRepo{entries: map[string][]domain.TimelineEntry{}}
}

func (f *fakeTimelineRepo) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.entries[entry.AlertID] = append(f.entries[entry.AlertID], entry)
	return "entry-id", nil
}

func (f *fakeTimelineRepo) ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error) {
	out := make([]domain.TimelineEntry, len(f.entries[alertID]))
	copy(out, f.entries[alertID])
	return out, nil
}

type fakeEscalationPolicyRepo struct {
	byTeam map[string][]store.EscalationPolicy
}

func (f *fakeEscalationPolicyRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.EscalationPolicy, error) {
	return f.byTeam[teamID], nil
}

type scheduleCall struct {
	kind, refID string
	fireAt      time.Time
	payload     []byte
}

type cancelCall struct{ kind, refID string }

type fakeTimerScheduler struct {
	scheduleCalls []scheduleCall
	cancelCalls   []cancelCall
}

func (f *fakeTimerScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	f.scheduleCalls = append(f.scheduleCalls, scheduleCall{kind: kind, refID: refID, fireAt: fireAt, payload: payload})
	return nil
}

func (f *fakeTimerScheduler) Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error {
	f.cancelCalls = append(f.cancelCalls, cancelCall{kind: kind, refID: refID})
	return nil
}

type notifyCall struct {
	alertID string
	level   int
	userIDs []string
}

type fakeNotifier struct {
	calls []notifyCall
}

func (f *fakeNotifier) NotifyEscalation(ctx context.Context, tx pgx.Tx, alert domain.Alert, level int, targets []OnCallEntry) error {
	ids := make([]string, len(targets))
	for i, e := range targets {
		ids[i] = e.UserID
	}
	f.calls = append(f.calls, notifyCall{alertID: alert.ID, level: level, userIDs: ids})
	return nil
}

// ===================== Resolver.OnCallAt =====================

// TestResolver_OnCallAt 覆盖：普通班次、跨天班次（23:00-次日07:00）、班次空洞、
// 起含终不含边界、非 UTC 输入的规范化。
func TestResolver_OnCallAt(t *testing.T) {
	const teamID = "team-1"
	const scheduleID = "sched-1"
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	schedules := &fakeScheduleRepo{byTeam: map[string][]store.Schedule{
		teamID: {{ID: scheduleID, TeamID: teamID, Name: "primary"}},
	}}
	shifts := &fakeShiftRepo{byScheduleID: map[string][]store.Shift{
		scheduleID: {
			// 白班 07:00-19:00。
			{ID: "shift-day", ScheduleID: scheduleID, UserID: "alice", StartsAt: day.Add(7 * time.Hour), EndsAt: day.Add(19 * time.Hour)},
			// 夜班 23:00-次日07:00（跨天），与白班之间留出 19:00-23:00 的空洞。
			{ID: "shift-night", ScheduleID: scheduleID, UserID: "bob", StartsAt: day.Add(23 * time.Hour), EndsAt: day.Add(31 * time.Hour)},
		},
	}}
	r := NewResolver(schedules, shifts)

	nonUTC := time.FixedZone("UTC+8", 8*3600)

	cases := []struct {
		name string
		at   time.Time
		want []string
	}{
		{"白班中段", day.Add(10 * time.Hour), []string{"alice"}},
		{"白班起点含", day.Add(7 * time.Hour), []string{"alice"}},
		{"白班终点不含_落入空洞", day.Add(19 * time.Hour), nil},
		{"班次空洞_无人值班", day.Add(21 * time.Hour), nil},
		{"夜班起点含", day.Add(23 * time.Hour), []string{"bob"}},
		{"跨天夜班中段_次日05点", day.Add(29 * time.Hour), []string{"bob"}},
		{"跨天夜班终点不含", day.Add(31 * time.Hour), nil},
		{"非UTC输入按同一时刻规范化", day.Add(10 * time.Hour).In(nonUTC), []string{"alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := r.OnCallAt(context.Background(), nil, teamID, tc.at)
			if err != nil {
				t.Fatalf("OnCallAt() error = %v", err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.UserID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("OnCallAt(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// TestResolver_OnCallAt_NoSchedule 覆盖团队从未配置值班表的情形：不是错误，只是无人值班。
func TestResolver_OnCallAt_NoSchedule(t *testing.T) {
	r := NewResolver(&fakeScheduleRepo{byTeam: map[string][]store.Schedule{}}, &fakeShiftRepo{byScheduleID: map[string][]store.Shift{}})
	entries, err := r.OnCallAt(context.Background(), nil, "team-无值班表", time.Now())
	if err != nil {
		t.Fatalf("OnCallAt() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("OnCallAt() = %v, want 空", entries)
	}
}

// ===================== Chain：三级升级链时序 =====================

func newTestChain(alerts *fakeAlertRepo, timeline *fakeTimelineRepo, policies *fakeEscalationPolicyRepo,
	resolver *Resolver, sched *fakeTimerScheduler, notifier *fakeNotifier, now time.Time) *Chain {
	c := NewChain(fakeTx{}, alerts, timeline, policies, resolver, sched, notifier)
	c.clock = &settableClock{t: now}
	return c
}

// TestChain_ThreeLevelEscalation 覆盖：Start 注册第 0 级、逐级推进（未 Ack 时按
// wait_seconds 注册下一级）、同一级重投的幂等性、Ack 后停止（不再通知/不再注册）。
func TestChain_ThreeLevelEscalation(t *testing.T) {
	const alertID = "alert-1"
	const teamID = "team-1"
	const scheduleID = "sched-1"
	start := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

	resolver := NewResolver(
		&fakeScheduleRepo{byTeam: map[string][]store.Schedule{teamID: {{ID: scheduleID, TeamID: teamID}}}},
		&fakeShiftRepo{byScheduleID: map[string][]store.Shift{
			scheduleID: {{ID: "shift-1", ScheduleID: scheduleID, UserID: "alice", StartsAt: start.Add(-time.Hour), EndsAt: start.Add(48 * time.Hour)}},
		}},
	)
	policies := &fakeEscalationPolicyRepo{byTeam: map[string][]store.EscalationPolicy{
		teamID: {{ID: "policy-1", TeamID: teamID, Levels: []store.EscalationLevel{
			{WaitSeconds: 0, Targets: []string{teamID}},
			{WaitSeconds: 300, Targets: []string{teamID}},
			{WaitSeconds: 600, Targets: []string{teamID}},
		}}},
	}}
	alerts := &fakeAlertRepo{alerts: map[string]domain.Alert{
		alertID: {ID: alertID, TeamID: teamID, Status: domain.StatusFiring},
	}}
	timeline := newFakeTimelineRepo()
	sched := &fakeTimerScheduler{}
	notifier := &fakeNotifier{}
	clock := &settableClock{t: start}
	chain := NewChain(fakeTx{}, alerts, timeline, policies, resolver, sched, notifier)
	chain.clock = clock

	// Start：第 0 级 wait=0，应立即注册定时器。
	if err := chain.Start(context.Background(), nil, alerts.alerts[alertID]); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(sched.scheduleCalls) != 1 || !sched.scheduleCalls[0].fireAt.Equal(start) {
		t.Fatalf("Start() 未按预期注册第 0 级定时器: %+v", sched.scheduleCalls)
	}

	// 第 0 级触发：通知 alice，注册第 1 级（+300s）。
	payload0, _ := json.Marshal(escalationPayload{Level: 0})
	if err := chain.Handle(context.Background(), alertID, payload0); err != nil {
		t.Fatalf("Handle(level0) error = %v", err)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].level != 0 || !reflect.DeepEqual(notifier.calls[0].userIDs, []string{"alice"}) {
		t.Fatalf("level0 通知不正确: %+v", notifier.calls)
	}
	wantLevel1FireAt := start.Add(300 * time.Second)
	if len(sched.scheduleCalls) != 2 || !sched.scheduleCalls[1].fireAt.Equal(wantLevel1FireAt) {
		t.Fatalf("level0 后未正确注册第 1 级: %+v", sched.scheduleCalls)
	}

	// 幂等：重投同一个 level0 payload（模拟 at-least-once 重投），不应重复通知、不应重复注册。
	if err := chain.Handle(context.Background(), alertID, payload0); err != nil {
		t.Fatalf("Handle(level0 重投) error = %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("重投后 notifier 调用次数 = %d, want 1", len(notifier.calls))
	}
	if len(sched.scheduleCalls) != 2 {
		t.Fatalf("重投后 scheduleCalls 数量 = %d, want 2", len(sched.scheduleCalls))
	}

	// 第 1 级触发（仍未 Ack）：通知，注册第 2 级（+600s）。
	clock.t = wantLevel1FireAt
	payload1, _ := json.Marshal(escalationPayload{Level: 1})
	if err := chain.Handle(context.Background(), alertID, payload1); err != nil {
		t.Fatalf("Handle(level1) error = %v", err)
	}
	if len(notifier.calls) != 2 || notifier.calls[1].level != 1 {
		t.Fatalf("level1 通知不正确: %+v", notifier.calls)
	}
	wantLevel2FireAt := wantLevel1FireAt.Add(600 * time.Second)
	if len(sched.scheduleCalls) != 3 || !sched.scheduleCalls[2].fireAt.Equal(wantLevel2FireAt) {
		t.Fatalf("level1 后未正确注册第 2 级: %+v", sched.scheduleCalls)
	}

	// Ack：第 2 级触发时告警已 acknowledged，应停止——不通知、不再注册定时器。
	acked := alerts.alerts[alertID]
	acked.Status = domain.StatusAcknowledged
	alerts.alerts[alertID] = acked

	clock.t = wantLevel2FireAt
	payload2, _ := json.Marshal(escalationPayload{Level: 2})
	if err := chain.Handle(context.Background(), alertID, payload2); err != nil {
		t.Fatalf("Handle(level2, acked) error = %v", err)
	}
	if len(notifier.calls) != 2 {
		t.Fatalf("Ack 后 notifier 调用次数 = %d, want 2（不应再通知）", len(notifier.calls))
	}
	if len(sched.scheduleCalls) != 3 {
		t.Fatalf("Ack 后 scheduleCalls 数量 = %d, want 3（不应再注册）", len(sched.scheduleCalls))
	}
}

// TestChain_Silenced_Stops 覆盖静默停止条件：与 Ack/Resolved 同等对待，直接停止、不留痕迹。
func TestChain_Silenced_Stops(t *testing.T) {
	const alertID, teamID = "alert-4", "team-4"
	resolver := NewResolver(&fakeScheduleRepo{byTeam: map[string][]store.Schedule{}}, &fakeShiftRepo{byScheduleID: map[string][]store.Shift{}})
	policies := &fakeEscalationPolicyRepo{byTeam: map[string][]store.EscalationPolicy{
		teamID: {{ID: "p", TeamID: teamID, Levels: []store.EscalationLevel{{WaitSeconds: 0, Targets: []string{teamID}}}}},
	}}
	alerts := &fakeAlertRepo{alerts: map[string]domain.Alert{
		alertID: {ID: alertID, TeamID: teamID, Status: domain.StatusFiring, SilencedBy: []string{"silence-1"}},
	}}
	timeline := newFakeTimelineRepo()
	sched := &fakeTimerScheduler{}
	notifier := &fakeNotifier{}
	chain := newTestChain(alerts, timeline, policies, resolver, sched, notifier, time.Now())

	payload, _ := json.Marshal(escalationPayload{Level: 0})
	if err := chain.Handle(context.Background(), alertID, payload); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(notifier.calls) != 0 || len(sched.scheduleCalls) != 0 || len(timeline.entries[alertID]) != 0 {
		t.Fatalf("静默告警应直接停止且不留痕迹: notifier=%d sched=%d timeline=%d",
			len(notifier.calls), len(sched.scheduleCalls), len(timeline.entries[alertID]))
	}
}

// TestChain_NobodyOnCall_RecordsWarningAndContinues 覆盖班次空洞：落时间线警告，
// 但不中断链路，仍按 wait_seconds 注册下一级。
func TestChain_NobodyOnCall_RecordsWarningAndContinues(t *testing.T) {
	const alertID, teamID = "alert-2", "team-2"
	// 团队没有配置任何值班表 -> OnCallAt 恒返回空，模拟"无人可通知"。
	resolver := NewResolver(&fakeScheduleRepo{byTeam: map[string][]store.Schedule{}}, &fakeShiftRepo{byScheduleID: map[string][]store.Shift{}})
	policies := &fakeEscalationPolicyRepo{byTeam: map[string][]store.EscalationPolicy{
		teamID: {{ID: "p", TeamID: teamID, Levels: []store.EscalationLevel{
			{WaitSeconds: 0, Targets: []string{teamID}},
			{WaitSeconds: 60, Targets: []string{teamID}},
		}}},
	}}
	alerts := &fakeAlertRepo{alerts: map[string]domain.Alert{
		alertID: {ID: alertID, TeamID: teamID, Status: domain.StatusFiring},
	}}
	timeline := newFakeTimelineRepo()
	sched := &fakeTimerScheduler{}
	notifier := &fakeNotifier{}
	chain := newTestChain(alerts, timeline, policies, resolver, sched, notifier, time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC))

	payload0, _ := json.Marshal(escalationPayload{Level: 0})
	if err := chain.Handle(context.Background(), alertID, payload0); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if len(notifier.calls) != 0 {
		t.Fatalf("无人可通知时不应调用 Notifier，got %d 次", len(notifier.calls))
	}
	entries := timeline.entries[alertID]
	if len(entries) != 1 || entries[0].Actor != escalationSystemActor || entries[0].Detail[escalationDetailWarn] != warnNobodyOnCall {
		t.Fatalf("未正确落无人可通知的警告: %+v", entries)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("scheduleCalls 数量 = %d, want 1（无人可通知不应中断链路）", len(sched.scheduleCalls))
	}
}

// TestChain_LevelBeyondPolicy_RecordsExhaustedWarning 覆盖升级链耗尽/策略未配置：
// 落"链路耗尽"警告，且不再注册定时器（自然结束）。
func TestChain_LevelBeyondPolicy_RecordsExhaustedWarning(t *testing.T) {
	const alertID, teamID = "alert-3", "team-3"
	resolver := NewResolver(&fakeScheduleRepo{byTeam: map[string][]store.Schedule{}}, &fakeShiftRepo{byScheduleID: map[string][]store.Shift{}})
	policies := &fakeEscalationPolicyRepo{byTeam: map[string][]store.EscalationPolicy{
		teamID: {{ID: "p", TeamID: teamID, Levels: []store.EscalationLevel{{WaitSeconds: 0, Targets: []string{teamID}}}}},
	}}
	alerts := &fakeAlertRepo{alerts: map[string]domain.Alert{
		alertID: {ID: alertID, TeamID: teamID, Status: domain.StatusFiring},
	}}
	timeline := newFakeTimelineRepo()
	sched := &fakeTimerScheduler{}
	notifier := &fakeNotifier{}
	chain := newTestChain(alerts, timeline, policies, resolver, sched, notifier, time.Now())

	payload, _ := json.Marshal(escalationPayload{Level: 5}) // 越界：策略只有 1 级
	if err := chain.Handle(context.Background(), alertID, payload); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	entries := timeline.entries[alertID]
	if len(entries) != 1 || entries[0].Detail[escalationDetailWarn] != warnChainExhausted {
		t.Fatalf("未正确落链路耗尽警告: %+v", entries)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("链路耗尽后不应再注册定时器, got %d", len(sched.scheduleCalls))
	}
}

// TestChain_UnconfiguredPolicy_Start_NoOp 覆盖团队未配置升级策略：Start 不应报错、
// 也不应注册任何定时器（告警仍走常规通知策略，只是没有人员升级）。
func TestChain_UnconfiguredPolicy_Start_NoOp(t *testing.T) {
	resolver := NewResolver(&fakeScheduleRepo{}, &fakeShiftRepo{})
	policies := &fakeEscalationPolicyRepo{byTeam: map[string][]store.EscalationPolicy{}}
	sched := &fakeTimerScheduler{}
	chain := newTestChain(&fakeAlertRepo{alerts: map[string]domain.Alert{}}, newFakeTimelineRepo(), policies, resolver, sched, &fakeNotifier{}, time.Now())

	alert := domain.Alert{ID: "alert-6", TeamID: "team-无策略", Status: domain.StatusFiring}
	if err := chain.Start(context.Background(), nil, alert); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("未配置升级策略时 Start() 不应注册定时器, got %d", len(sched.scheduleCalls))
	}
}

// TestChain_Stop_CancelsTimer 覆盖 Stop：供 Ack/Resolve 动作主动取消待触发升级定时器。
func TestChain_Stop_CancelsTimer(t *testing.T) {
	sched := &fakeTimerScheduler{}
	chain := newTestChain(&fakeAlertRepo{alerts: map[string]domain.Alert{}}, newFakeTimelineRepo(),
		&fakeEscalationPolicyRepo{}, NewResolver(&fakeScheduleRepo{}, &fakeShiftRepo{}), sched, &fakeNotifier{}, time.Now())

	if err := chain.Stop(context.Background(), nil, "alert-5"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if len(sched.cancelCalls) != 1 || sched.cancelCalls[0].kind != TimerKindPersonnelEscalation || sched.cancelCalls[0].refID != "alert-5" {
		t.Fatalf("Cancel 调用不正确: %+v", sched.cancelCalls)
	}
}
