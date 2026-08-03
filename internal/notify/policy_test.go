package notify

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// registerPolicyTestChannels 注册本文件测试场景用到的渠道类型（email/im/phone），
// 供 Engine.Start 的 R13.6 校验通过；phone 渠道本期无真实 Channel 实现，这里注册一个
// fake 满足"critical 样例测试用 fake channel 注册 phone"的要求（硬约束原文）。
func registerPolicyTestChannels() {
	Register(&fakeChannel{kind: "email"})
	Register(&fakeChannel{kind: "im"})
	Register(&fakeChannel{kind: "phone"})
}

// fakeClock 是 Clock 的可编程测试替身，精确控制"当前时间"而无需等待 wall-clock。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// fakePolicyAlertRepo 是 AlertRepo 的内存实现。
type fakePolicyAlertRepo struct {
	alerts map[string]domain.Alert
	err    error
}

func (f *fakePolicyAlertRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	if f.err != nil {
		return domain.Alert{}, f.err
	}
	a, ok := f.alerts[id]
	if !ok {
		return domain.Alert{}, store.ErrAlertNotFound
	}
	return a, nil
}

// fakePolicyTimeline 是 TimelineRepo 的内存实现。
type fakePolicyTimeline struct {
	entries   []domain.TimelineEntry
	appendErr error
	listErr   error
}

func (f *fakePolicyTimeline) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	if f.appendErr != nil {
		return "", f.appendErr
	}
	f.entries = append(f.entries, entry)
	return "tl-policy", nil
}

func (f *fakePolicyTimeline) ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.TimelineEntry
	for _, e := range f.entries {
		if e.AlertID == alertID {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakePolicyRepo 是 PolicyRepo 的内存实现。
type fakePolicyRepo struct {
	byTeam    map[string][]store.NotificationPolicy
	global    *store.NotificationPolicy
	teamErr   error
	globalErr error
}

func (f *fakePolicyRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.NotificationPolicy, error) {
	if f.teamErr != nil {
		return nil, f.teamErr
	}
	return f.byTeam[teamID], nil
}

func (f *fakePolicyRepo) GetGlobalDefault(ctx context.Context, tx pgx.Tx) (*store.NotificationPolicy, error) {
	if f.globalErr != nil {
		return nil, f.globalErr
	}
	if f.global == nil {
		return nil, store.ErrNotFound
	}
	return f.global, nil
}

// policyScheduleCall 记录一次 fakePolicyScheduler.Schedule 调用的参数。
type policyScheduleCall struct {
	kind, refID string
	fireAt      time.Time
	payload     []byte
}

// fakePolicyScheduler 是 TimerScheduler 的内存实现，只记录调用，不模拟真实的
// pending 唯一性替换语义（本文件的测试都是手工按 payload 直接调 Handle 模拟"到期"，
// 不依赖 Schedule 的替换/去重行为）。
type fakePolicyScheduler struct {
	scheduled       []policyScheduleCall
	canceled        []string
	scheduleErr     error
	cancelErr       error
	lockUnavailable bool
	exclusiveLocks  []string
}

func (f *fakePolicyScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	if f.scheduleErr != nil {
		return f.scheduleErr
	}
	f.scheduled = append(f.scheduled, policyScheduleCall{kind: kind, refID: refID, fireAt: fireAt, payload: append([]byte(nil), payload...)})
	return nil
}

func (f *fakePolicyScheduler) Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.canceled = append(f.canceled, refID)
	return nil
}

func (f *fakePolicyScheduler) TryLockRefShared(ctx context.Context, tx pgx.Tx, refID string) (bool, error) {
	return !f.lockUnavailable, nil
}

func (f *fakePolicyScheduler) LockRefExclusive(ctx context.Context, tx pgx.Tx, refID string) error {
	f.exclusiveLocks = append(f.exclusiveLocks, refID)
	return nil
}

// lastScheduled 返回最近一次 Schedule 调用，没有调用过时返回 nil。
func (f *fakePolicyScheduler) lastScheduled() *policyScheduleCall {
	if len(f.scheduled) == 0 {
		return nil
	}
	return &f.scheduled[len(f.scheduled)-1]
}

// dispatchCall 记录一次 fakeDispatcher.DispatchStep 调用的参数。
type dispatchCall struct {
	alertID  string
	step     int
	channels []string
}

// fakeDispatcher 是 Dispatcher 的测试替身，只记账调用参数，不做任何真实发送。
type fakeDispatcher struct {
	calls []dispatchCall
	err   error
}

func (f *fakeDispatcher) DispatchStep(ctx context.Context, tx pgx.Tx, alert domain.Alert, step int, channels []string) error {
	f.calls = append(f.calls, dispatchCall{alertID: alert.ID, step: step, channels: append([]string(nil), channels...)})
	return f.err
}

// newTestEngine 组装一个用于测试的 Engine，各依赖均为内存 fake，clock 可注入。
func newTestEngine(alerts *fakePolicyAlertRepo, timeline *fakePolicyTimeline, policies *fakePolicyRepo, sched *fakePolicyScheduler, dispatcher *fakeDispatcher, clock Clock) *Engine {
	e := NewEngine(alerts, timeline, policies, sched, dispatcher)
	e.clock = clock
	return e
}

// unmarshalStep 从 Schedule 记录的 payload 里取出 step 字段，测试断言用。
func unmarshalStep(t *testing.T, payload []byte) int {
	t.Helper()
	var p stepPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("反序列化 payload 失败: %v", err)
	}
	return p.Step
}

// ============================================================================
// resolvePolicy：R13.5 三级求值链
// ============================================================================

// ① 命中团队下按 matcher 绑定的业务策略：即使团队默认策略也存在，绑定策略优先。
func TestResolvePolicy_BindingMatcherWins(t *testing.T) {
	binding := store.NotificationPolicy{
		ID:       "p-binding",
		TeamID:   "team-a",
		Matchers: domain.MatcherSet{{Name: "service", Op: domain.MatchEqual, Value: "checkout"}},
	}
	teamDefault := store.NotificationPolicy{ID: "p-team-default", TeamID: "team-a"}
	policies := &fakePolicyRepo{byTeam: map[string][]store.NotificationPolicy{
		"team-a": {teamDefault, binding},
	}}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, policies, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	alert := domain.Alert{TeamID: "team-a", Labels: map[string]string{"service": "checkout"}}
	got, err := e.resolvePolicy(context.Background(), nil, alert)
	if err != nil {
		t.Fatalf("resolvePolicy() 返回意外错误: %v", err)
	}
	if got.ID != "p-binding" {
		t.Fatalf("resolvePolicy() = %q, want 绑定策略 p-binding", got.ID)
	}
}

// ② 无绑定策略命中时回退团队默认策略（Matchers 为空的那条）。
func TestResolvePolicy_TeamDefaultFallback(t *testing.T) {
	binding := store.NotificationPolicy{
		ID:       "p-binding",
		TeamID:   "team-a",
		Matchers: domain.MatcherSet{{Name: "service", Op: domain.MatchEqual, Value: "checkout"}},
	}
	teamDefault := store.NotificationPolicy{ID: "p-team-default", TeamID: "team-a"}
	policies := &fakePolicyRepo{byTeam: map[string][]store.NotificationPolicy{
		"team-a": {teamDefault, binding},
	}}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, policies, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	alert := domain.Alert{TeamID: "team-a", Labels: map[string]string{"service": "other"}}
	got, err := e.resolvePolicy(context.Background(), nil, alert)
	if err != nil {
		t.Fatalf("resolvePolicy() 返回意外错误: %v", err)
	}
	if got.ID != "p-team-default" {
		t.Fatalf("resolvePolicy() = %q, want 团队默认 p-team-default", got.ID)
	}
}

// ③ 团队下无任何策略（既无绑定也无团队默认）时回退全局默认。
func TestResolvePolicy_GlobalDefaultFallback(t *testing.T) {
	global := store.NotificationPolicy{ID: "p-global"}
	policies := &fakePolicyRepo{global: &global}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, policies, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	alert := domain.Alert{TeamID: "team-b"}
	got, err := e.resolvePolicy(context.Background(), nil, alert)
	if err != nil {
		t.Fatalf("resolvePolicy() 返回意外错误: %v", err)
	}
	if got.ID != "p-global" {
		t.Fatalf("resolvePolicy() = %q, want 全局默认 p-global", got.ID)
	}
}

// ④ 未路由（TeamID 为空）的告警直接落到全局默认，不查团队策略。
func TestResolvePolicy_UnroutedAlert_UsesGlobalDefault(t *testing.T) {
	global := store.NotificationPolicy{ID: "p-global"}
	policies := &fakePolicyRepo{global: &global}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, policies, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	got, err := e.resolvePolicy(context.Background(), nil, domain.Alert{})
	if err != nil {
		t.Fatalf("resolvePolicy() 返回意外错误: %v", err)
	}
	if got.ID != "p-global" {
		t.Fatalf("resolvePolicy() = %q, want 全局默认 p-global", got.ID)
	}
}

// ⑤ 三级都未命中返回 ErrNoPolicy。
func TestResolvePolicy_NoPolicyFound_ErrNoPolicy(t *testing.T) {
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{}, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	_, err := e.resolvePolicy(context.Background(), nil, domain.Alert{TeamID: "team-x"})
	if !errors.Is(err, ErrNoPolicy) {
		t.Fatalf("resolvePolicy() 错误 = %v, want ErrNoPolicy", err)
	}
}

// ============================================================================
// Engine.Start
// ============================================================================

// ⑥ 未命中任何策略：Start 不报错、不注册定时器。
func TestEngine_Start_NoPolicy_NoOp(t *testing.T) {
	sched := &fakePolicyScheduler{}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{}, sched, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	err := e.Start(context.Background(), nil, domain.Alert{ID: "a1", TeamID: "team-x"}, time.Time{})
	if err != nil {
		t.Fatalf("Start() 返回意外错误: %v", err)
	}
	if len(sched.scheduled) != 0 {
		t.Fatalf("未命中策略不应注册定时器，实际 %+v", sched.scheduled)
	}
}

// ⑦ 命中策略但该 severity 未配置升级序列：Start 不报错、不注册定时器。
func TestEngine_Start_NoStepsForSeverity_NoOp(t *testing.T) {
	global := store.NotificationPolicy{ID: "p-global", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{WaitSeconds: 0, Channels: []string{"email"}}},
	}}
	sched := &fakePolicyScheduler{}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{global: &global}, sched, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	err := e.Start(context.Background(), nil, domain.Alert{ID: "a1", Severity: domain.SeverityCritical}, time.Time{})
	if err != nil {
		t.Fatalf("Start() 返回意外错误: %v", err)
	}
	if len(sched.scheduled) != 0 {
		t.Fatalf("该 severity 无升级序列不应注册定时器，实际 %+v", sched.scheduled)
	}
}

// ⑧ 策略引用未注册渠道：Start 报错（R13.6：配置问题运行时也不静默跳过）。
func TestEngine_Start_UnregisteredChannel_Errors(t *testing.T) {
	global := store.NotificationPolicy{ID: "p-global", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{WaitSeconds: 0, Channels: []string{"sms-not-registered"}}},
	}}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{global: &global}, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	err := e.Start(context.Background(), nil, domain.Alert{ID: "a1", Severity: domain.SeverityWarning}, time.Time{})
	if !errors.Is(err, ErrChannelNotRegistered) {
		t.Fatalf("Start() 错误 = %v, want 包裹 ErrChannelNotRegistered", err)
	}
}

// ============================================================================
// 需求样例场景（R13.1）：fake clock 精确控制升级序列的推进
// ============================================================================

// ⑨ warning: email -> (5min 未 Ack) -> IM。
func TestEngine_WarningScenario_EmailThenIM(t *testing.T) {
	registerPolicyTestChannels()

	policy := store.NotificationPolicy{ID: "p-warning", Steps: store.NotificationSteps{
		domain.SeverityWarning: {
			{WaitSeconds: 0, Channels: []string{"email"}, Mode: "replace"},
			{WaitSeconds: 300, Channels: []string{"im"}, Mode: "replace"},
		},
	}}
	alert := domain.Alert{ID: "alert-w1", Severity: domain.SeverityWarning, Status: domain.StatusFiring, TeamID: "team-a"}

	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	timeline := &fakePolicyTimeline{}
	sched := &fakePolicyScheduler{}
	dispatcher := &fakeDispatcher{}
	clock := &fakeClock{t: time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)}
	e := newTestEngine(alerts, timeline, &fakePolicyRepo{global: &policy}, sched, dispatcher, clock)

	// Start：注册第 0 级定时器，wait_seconds=0，fire_at = now。
	if err := e.Start(context.Background(), nil, alert, time.Time{}); err != nil {
		t.Fatalf("Start() 返回意外错误: %v", err)
	}
	first := sched.lastScheduled()
	if first == nil || first.kind != TimerKindChannelEscalation || first.refID != alert.ID {
		t.Fatalf("Start() 应注册 kind=%s refID=%s 的定时器，实际 %+v", TimerKindChannelEscalation, alert.ID, sched.scheduled)
	}
	if !first.fireAt.Equal(clock.t) {
		t.Fatalf("第 0 级 fire_at = %v, want %v（wait_seconds=0）", first.fireAt, clock.t)
	}

	// 模拟第 0 级到期：Handle 触发，应发 email，并注册第 1 级（5min 后）定时器。
	if err := e.Handle(context.Background(), nil, alert.ID, first.payload); err != nil {
		t.Fatalf("Handle(step0) 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 1 || dispatcher.calls[0].step != 0 {
		t.Fatalf("第 0 级应触发一次 DispatchStep(step=0)，实际 %+v", dispatcher.calls)
	}
	if got := dispatcher.calls[0].channels; len(got) != 1 || got[0] != "email" {
		t.Fatalf("第 0 级渠道 = %v, want [email]", got)
	}
	second := sched.lastScheduled()
	if second == nil || len(sched.scheduled) != 2 {
		t.Fatalf("第 0 级触发后应注册第 1 级定时器，实际 %+v", sched.scheduled)
	}
	wantFireAt := clock.t.Add(5 * time.Minute)
	if !second.fireAt.Equal(wantFireAt) {
		t.Fatalf("第 1 级 fire_at = %v, want %v（5min 后）", second.fireAt, wantFireAt)
	}

	// 模拟第 1 级到期（5min 未 Ack）：Handle 触发，应发 IM，且已到序列末级不再注册定时器。
	clock.t = wantFireAt
	if err := e.Handle(context.Background(), nil, alert.ID, second.payload); err != nil {
		t.Fatalf("Handle(step1) 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 2 || dispatcher.calls[1].step != 1 {
		t.Fatalf("第 1 级应触发一次 DispatchStep(step=1)，实际 %+v", dispatcher.calls)
	}
	if got := dispatcher.calls[1].channels; len(got) != 1 || got[0] != "im" {
		t.Fatalf("第 1 级渠道 = %v, want [im]（replace 模式）", got)
	}
	if len(sched.scheduled) != 2 {
		t.Fatalf("已到序列末级不应再注册新定时器，实际 %+v", sched.scheduled)
	}
}

// ⑩ critical: IM -> (3min 未 Ack) -> 电话；电话渠道用 fake channel 注册满足 R13.6 校验。
func TestEngine_CriticalScenario_IMThenPhone(t *testing.T) {
	registerPolicyTestChannels()

	policy := store.NotificationPolicy{ID: "p-critical", Steps: store.NotificationSteps{
		domain.SeverityCritical: {
			{WaitSeconds: 0, Channels: []string{"im"}, Mode: "replace"},
			{WaitSeconds: 180, Channels: []string{"phone"}, Mode: "replace"},
		},
	}}
	alert := domain.Alert{ID: "alert-c1", Severity: domain.SeverityCritical, Status: domain.StatusFiring, TeamID: "team-a"}

	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	timeline := &fakePolicyTimeline{}
	sched := &fakePolicyScheduler{}
	dispatcher := &fakeDispatcher{}
	clock := &fakeClock{t: time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)}
	e := newTestEngine(alerts, timeline, &fakePolicyRepo{global: &policy}, sched, dispatcher, clock)

	if err := e.Start(context.Background(), nil, alert, time.Time{}); err != nil {
		t.Fatalf("Start() 返回意外错误: %v", err)
	}
	step0 := sched.lastScheduled()
	if err := e.Handle(context.Background(), nil, alert.ID, step0.payload); err != nil {
		t.Fatalf("Handle(step0) 返回意外错误: %v", err)
	}
	if got := dispatcher.calls[0].channels; len(got) != 1 || got[0] != "im" {
		t.Fatalf("第 0 级渠道 = %v, want [im]", got)
	}
	step1 := sched.lastScheduled()
	wantFireAt := clock.t.Add(3 * time.Minute)
	if !step1.fireAt.Equal(wantFireAt) {
		t.Fatalf("第 1 级 fire_at = %v, want %v（3min 后）", step1.fireAt, wantFireAt)
	}

	clock.t = wantFireAt
	if err := e.Handle(context.Background(), nil, alert.ID, step1.payload); err != nil {
		t.Fatalf("Handle(step1) 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 2 {
		t.Fatalf("第 1 级应触发一次 DispatchStep，实际 %+v", dispatcher.calls)
	}
	if got := dispatcher.calls[1].channels; len(got) != 1 || got[0] != "phone" {
		t.Fatalf("第 1 级渠道 = %v, want [phone]", got)
	}
}

// ============================================================================
// 停止条件：Ack/Resolve/静默命中（R13.4/R10.3）
// ============================================================================

// ⑪ 告警已 Ack：advance 不发送、不注册下一级定时器。
func TestEngine_Advance_AckStopsProgression(t *testing.T) {
	registerPolicyTestChannels()
	policy := store.NotificationPolicy{ID: "p-w", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{Channels: []string{"email"}}, {WaitSeconds: 300, Channels: []string{"im"}}},
	}}
	alert := domain.Alert{ID: "a-acked", Severity: domain.SeverityWarning, Status: domain.StatusAcknowledged}
	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	sched := &fakePolicyScheduler{}
	dispatcher := &fakeDispatcher{}
	e := newTestEngine(alerts, &fakePolicyTimeline{}, &fakePolicyRepo{global: &policy}, sched, dispatcher, &fakeClock{t: time.Now()})

	if err := e.advance(context.Background(), nil, alert.ID, 0); err != nil {
		t.Fatalf("advance() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("已 Ack 的告警不应触发 DispatchStep，实际 %+v", dispatcher.calls)
	}
	if len(sched.scheduled) != 0 {
		t.Fatalf("已 Ack 的告警不应注册下一级定时器，实际 %+v", sched.scheduled)
	}
}

// ⑫ 告警已被静默命中：advance 不发送（R10.3），不注册下一级定时器。
func TestEngine_Advance_SilencedAlert_NoSend(t *testing.T) {
	registerPolicyTestChannels()
	policy := store.NotificationPolicy{ID: "p-w", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{Channels: []string{"email"}}},
	}}
	alert := domain.Alert{ID: "a-silenced", Severity: domain.SeverityWarning, Status: domain.StatusFiring, SilencedBy: []string{"silence-1"}}
	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	dispatcher := &fakeDispatcher{}
	e := newTestEngine(alerts, &fakePolicyTimeline{}, &fakePolicyRepo{global: &policy}, &fakePolicyScheduler{}, dispatcher, &fakeClock{t: time.Now()})

	if err := e.advance(context.Background(), nil, alert.ID, 0); err != nil {
		t.Fatalf("advance() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("被静默命中的告警不应触发 DispatchStep，实际 %+v", dispatcher.calls)
	}
}

// ⑬ 告警已 Resolved：advance 不发送、不注册下一级定时器。
func TestEngine_Advance_ResolvedAlert_NoSend(t *testing.T) {
	registerPolicyTestChannels()
	policy := store.NotificationPolicy{ID: "p-w", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{Channels: []string{"email"}}},
	}}
	alert := domain.Alert{ID: "a-resolved", Severity: domain.SeverityWarning, Status: domain.StatusResolved}
	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	dispatcher := &fakeDispatcher{}
	e := newTestEngine(alerts, &fakePolicyTimeline{}, &fakePolicyRepo{global: &policy}, &fakePolicyScheduler{}, dispatcher, &fakeClock{t: time.Now()})

	if err := e.advance(context.Background(), nil, alert.ID, 0); err != nil {
		t.Fatalf("advance() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("已 resolved 的告警不应触发 DispatchStep，实际 %+v", dispatcher.calls)
	}
}

// ⑭ 告警不存在：advance 视为无需重投，不报错。
func TestEngine_Advance_AlertNotFound_NoOp(t *testing.T) {
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{}, &fakePolicyScheduler{}, &fakeDispatcher{}, &fakeClock{t: time.Now()})
	if err := e.advance(context.Background(), nil, "missing", 0); err != nil {
		t.Fatalf("advance() 对不存在的告警返回了意外错误: %v", err)
	}
}

// ⑮ Stop：取消 alert 对应的渠道升级定时器。
func TestEngine_Stop_CancelsTimer(t *testing.T) {
	sched := &fakePolicyScheduler{}
	e := newTestEngine(&fakePolicyAlertRepo{}, &fakePolicyTimeline{}, &fakePolicyRepo{}, sched, &fakeDispatcher{}, &fakeClock{t: time.Now()})

	if err := e.Stop(context.Background(), nil, "alert-1"); err != nil {
		t.Fatalf("Stop() 返回意外错误: %v", err)
	}
	if len(sched.canceled) != 1 || sched.canceled[0] != "alert-1" {
		t.Fatalf("Cancel 调用 = %+v, want [alert-1]", sched.canceled)
	}
	if !reflect.DeepEqual(sched.exclusiveLocks, []string{"alert-1"}) {
		t.Fatalf("Stop 应先取得 timer 链独占屏障，实际 %+v", sched.exclusiveLocks)
	}
}

// ⑮b Stop 已取得独占屏障时，handler 必须直接结束，不能发送或续挂下一层。
func TestEngine_Handle_StopBarrierHeld(t *testing.T) {
	registerPolicyTestChannels()
	alert := domain.Alert{ID: "alert-barrier", Severity: domain.SeverityWarning, Status: domain.StatusFiring}
	policy := store.NotificationPolicy{ID: "p-barrier", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{Channels: []string{"email"}}, {WaitSeconds: 60, Channels: []string{"im"}}},
	}}
	sched := &fakePolicyScheduler{lockUnavailable: true}
	dispatcher := &fakeDispatcher{}
	timeline := &fakePolicyTimeline{}
	e := newTestEngine(
		&fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}},
		timeline,
		&fakePolicyRepo{global: &policy},
		sched,
		dispatcher,
		&fakeClock{t: time.Now()},
	)

	payload, _ := json.Marshal(stepPayload{Step: 0})
	if err := e.Handle(context.Background(), nil, alert.ID, payload); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 0 || len(sched.scheduled) != 0 || len(timeline.entries) != 0 {
		t.Fatalf("独占屏障期间 handler 应无副作用: dispatch=%d schedule=%d timeline=%d",
			len(dispatcher.calls), len(sched.scheduled), len(timeline.entries))
	}
}

// ============================================================================
// 幂等：at-least-once 重投不应重复发送/重复注册定时器
// ============================================================================

// ⑯ 同一级被重复投递（时间线里已有该级记账）：第二次 advance 不再发送、不再注册定时器。
func TestEngine_Advance_Idempotent_RedeliveryDoesNotDoubleDispatch(t *testing.T) {
	registerPolicyTestChannels()
	policy := store.NotificationPolicy{ID: "p-w", Steps: store.NotificationSteps{
		domain.SeverityWarning: {{Channels: []string{"email"}}, {WaitSeconds: 300, Channels: []string{"im"}}},
	}}
	alert := domain.Alert{ID: "a-redelivered", Severity: domain.SeverityWarning, Status: domain.StatusFiring}
	alerts := &fakePolicyAlertRepo{alerts: map[string]domain.Alert{alert.ID: alert}}
	timeline := &fakePolicyTimeline{}
	sched := &fakePolicyScheduler{}
	dispatcher := &fakeDispatcher{}
	e := newTestEngine(alerts, timeline, &fakePolicyRepo{global: &policy}, sched, dispatcher, &fakeClock{t: time.Now()})

	if err := e.advance(context.Background(), nil, alert.ID, 0); err != nil {
		t.Fatalf("第一次 advance() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 1 || len(sched.scheduled) != 1 {
		t.Fatalf("第一次 advance() 应发送一次并注册下一级定时器，实际 dispatch=%+v schedule=%+v", dispatcher.calls, sched.scheduled)
	}

	// 模拟 scheduler 因外层事务回滚而重投同一个 (alertID, step=0)。
	if err := e.advance(context.Background(), nil, alert.ID, 0); err != nil {
		t.Fatalf("第二次（重投）advance() 返回意外错误: %v", err)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("重投同一级不应重复触发 DispatchStep，实际 %+v", dispatcher.calls)
	}
	if len(sched.scheduled) != 1 {
		t.Fatalf("重投同一级不应重复注册下一级定时器，实际 %+v", sched.scheduled)
	}
}

// ============================================================================
// replace/spread 两种模式（R13.2）
// ============================================================================

// ⑰ replace 模式：本级渠道集合完全替换上一级，不叠加。
func TestEffectiveChannels_ReplaceMode(t *testing.T) {
	steps := []store.NotificationStep{
		{Channels: []string{"email"}},
		{Channels: []string{"im"}, Mode: "replace"},
	}
	got := effectiveChannels(steps, 1)
	if len(got) != 1 || got[0] != "im" {
		t.Fatalf("effectiveChannels(replace) = %v, want [im]", got)
	}
}

// ⑱ spread 模式：本级渠道叠加到上一级已生效的集合上，去重保序。
func TestEffectiveChannels_SpreadMode_Accumulates(t *testing.T) {
	steps := []store.NotificationStep{
		{Channels: []string{"email"}},
		{Channels: []string{"im"}, Mode: modeSpread},
		{Channels: []string{"phone", "email"}, Mode: modeSpread}, // email 重复，验证去重
	}
	got := effectiveChannels(steps, 2)
	want := []string{"email", "im", "phone"}
	if len(got) != len(want) {
		t.Fatalf("effectiveChannels(spread) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("effectiveChannels(spread) = %v, want %v（顺序应为首次出现顺序）", got, want)
		}
	}
}

// ============================================================================
// R13.6：配置校验
// ============================================================================

// ⑲ ValidatePolicySteps：引用未注册渠道时报错。
func TestValidatePolicySteps_UnregisteredChannel_Errors(t *testing.T) {
	steps := store.NotificationSteps{
		domain.SeverityCritical: {{Channels: []string{"sms-unregistered"}}},
	}
	err := ValidatePolicySteps(steps)
	if !errors.Is(err, ErrChannelNotRegistered) {
		t.Fatalf("ValidatePolicySteps() 错误 = %v, want 包裹 ErrChannelNotRegistered", err)
	}
}

// ⑳ ValidatePolicySteps：全部渠道已注册时不报错，覆盖多个 severity 分层。
func TestValidatePolicySteps_AllRegistered_OK(t *testing.T) {
	registerPolicyTestChannels()
	steps := store.NotificationSteps{
		domain.SeverityWarning:  {{Channels: []string{"email"}}, {Channels: []string{"im"}}},
		domain.SeverityCritical: {{Channels: []string{"im"}}, {Channels: []string{"phone"}}},
	}
	if err := ValidatePolicySteps(steps); err != nil {
		t.Fatalf("ValidatePolicySteps() 返回意外错误: %v", err)
	}
}
