package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// 本文件是纯单测（无 build tag，无需 Docker）：stormHandler 依赖的四个窄接口
// （stormStateRepo/stormRouteRepo/stormAlertRepo/stormScheduler）全用内存 fake
// 实现，验证 Handle 的分支逻辑与幂等性，不依赖真实 PostgreSQL。

// fakeStormStateRepo 用内存 map 模拟 route_storm_state 表，语义对齐
// store.StormStateRepo：AddSuppressed/MarkSummary/Deactivate 都要求行已存在。
type fakeStormStateRepo struct {
	states map[string]*store.StormState

	getErr           error
	addSuppressedErr error
	markSummaryErr   error
	deactivateErr    error
}

func (f *fakeStormStateRepo) Get(ctx context.Context, tx pgx.Tx, routeID string) (*store.StormState, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.states[routeID]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStormStateRepo) AddSuppressed(ctx context.Context, tx pgx.Tx, routeID string, n int) error {
	if f.addSuppressedErr != nil {
		return f.addSuppressedErr
	}
	s, ok := f.states[routeID]
	if !ok {
		return store.ErrNotFound
	}
	s.SuppressedCount += n
	return nil
}

func (f *fakeStormStateRepo) MarkSummary(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	if f.markSummaryErr != nil {
		return f.markSummaryErr
	}
	s, ok := f.states[routeID]
	if !ok {
		return store.ErrNotFound
	}
	s.LastSummaryAt = &now
	return nil
}

func (f *fakeStormStateRepo) Deactivate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	if f.deactivateErr != nil {
		return f.deactivateErr
	}
	s, ok := f.states[routeID]
	if !ok {
		return store.ErrNotFound
	}
	s.Active = false
	s.DeactivatedAt = &now
	return nil
}

// fakeStormRouteRepo 用内存 map 模拟 RouteRepo.GetByID。
type fakeStormRouteRepo struct {
	routes map[string]store.Route
}

func (f *fakeStormRouteRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error) {
	r, ok := f.routes[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

// fakeStormAlertRepo 用固定返回值模拟 AlertRepo 的 CountByRouteSince/ListActiveByRoute。
type fakeStormAlertRepo struct {
	count  int
	active []domain.Alert

	countErr  error
	activeErr error
}

func (f *fakeStormAlertRepo) CountByRouteSince(ctx context.Context, tx pgx.Tx, routeID string, since time.Time) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func (f *fakeStormAlertRepo) ListActiveByRoute(ctx context.Context, tx pgx.Tx, routeID string) ([]domain.Alert, error) {
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	return f.active, nil
}

// scheduleCall 记录一次 Schedule/ScheduleIfAbsent 调用的入参，供断言。
type scheduleCall struct {
	kind, refID string
	fireAt      time.Time
	payload     []byte
}

// fakeStormScheduler 记录 Schedule/ScheduleIfAbsent 调用，不做真实持久化。
type fakeStormScheduler struct {
	scheduleCalls         []scheduleCall
	scheduleIfAbsentCalls []scheduleCall
	scheduleErr           error
	scheduleIfAbsentErr   error
}

func (f *fakeStormScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	if f.scheduleErr != nil {
		return f.scheduleErr
	}
	f.scheduleCalls = append(f.scheduleCalls, scheduleCall{kind, refID, fireAt, payload})
	return nil
}

func (f *fakeStormScheduler) ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error) {
	if f.scheduleIfAbsentErr != nil {
		return false, f.scheduleIfAbsentErr
	}
	f.scheduleIfAbsentCalls = append(f.scheduleIfAbsentCalls, scheduleCall{kind, refID, fireAt, payload})
	return true, nil
}

// newTestStormHandler 构造带固定时钟的 stormHandler，routeID 对应的 state/route 已就位。
func newTestStormHandler(state *fakeStormStateRepo, routes *fakeStormRouteRepo, alerts *fakeStormAlertRepo, sched *fakeStormScheduler, now time.Time) *stormHandler {
	h := newStormHandler(state, routes, alerts, sched)
	h.now = func() time.Time { return now }
	return h
}

func testRoute(id string, thresholdPerMin, groupIntervalSeconds int) store.Route {
	return store.Route{ID: id, TargetTeamID: "team-x", StormThresholdPerMin: thresholdPerMin, GroupIntervalSeconds: groupIntervalSeconds}
}

// 从 fakeStormScheduler.scheduleCalls 里找出 notify_send 那条并反序列化出 SendRequest，
// 供断言摘要通知的具体参数（target/channel/round 等）。
func decodeSentRequest(t *testing.T, sched *fakeStormScheduler) notify.SendRequest {
	t.Helper()
	for _, c := range sched.scheduleCalls {
		if c.kind == notifySendTimerKind {
			var req notify.SendRequest
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("反序列化 notify_send payload 失败: %v", err)
			}
			return req
		}
	}
	t.Fatal("未找到 notify_send 调度记录")
	return notify.SendRequest{}
}

// ① suppressed_count>0 且仍超阈值：发摘要 + 清零 + 记录摘要时刻 + 续期 1 分钟，
// 不解除熔断、不补组 timer。
func TestStormHandler_StillOverThreshold_SendsSummaryAndRenews(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	activatedAt := now.Add(-5 * time.Minute)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: activatedAt, SuppressedCount: 42},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 10, 300)}}
	alerts := &fakeStormAlertRepo{count: 15, active: []domain.Alert{{ID: "a1", Title: "sample title", GroupKey: "gk1"}}}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("Handle 返回意外错误: %v", err)
	}

	if state.states["r1"].SuppressedCount != 0 {
		t.Fatalf("SuppressedCount 应清零为 0，got %d", state.states["r1"].SuppressedCount)
	}
	if state.states["r1"].LastSummaryAt == nil || !state.states["r1"].LastSummaryAt.Equal(now) {
		t.Fatalf("LastSummaryAt 应记录为本次触发时刻 %v，got %v", now, state.states["r1"].LastSummaryAt)
	}
	if state.states["r1"].Active != true || state.states["r1"].DeactivatedAt != nil {
		t.Fatal("仍超阈值时不应解除熔断")
	}

	req := decodeSentRequest(t, sched)
	if req.ChannelKind != teamNotifyChannel || req.Target != "team-x" {
		t.Fatalf("摘要发送目标不符: channel=%q target=%q", req.ChannelKind, req.Target)
	}
	if req.Round != now.UnixNano() {
		t.Fatalf("Round 应为本次触发时刻捕获，got %d want %d", req.Round, now.UnixNano())
	}
	if req.Msg.Body == "" || req.Msg.Title == "" {
		t.Fatal("摘要消息标题/正文不应为空")
	}

	// 续期：应有一条 storm_summary/r1，fireAt = now+1min。
	var found bool
	for _, c := range sched.scheduleCalls {
		if c.kind == TimerKindStormSummary && c.refID == "r1" {
			found = true
			if !c.fireAt.Equal(now.Add(stormSummaryRecheckBackoff)) {
				t.Fatalf("续期 fireAt = %v, want %v", c.fireAt, now.Add(stormSummaryRecheckBackoff))
			}
		}
	}
	if !found {
		t.Fatal("仍超阈值时应续期 storm_summary 定时器")
	}
	if len(sched.scheduleIfAbsentCalls) != 0 {
		t.Fatal("仍超阈值时不应补组 timer")
	}
}

// ② suppressed_count>0 且回落到阈值以下：发摘要 + 清零 + 解除熔断 + 对 distinct
// group_key 各补一轮 group_wait 定时器。
func TestStormHandler_FallsBelowThreshold_DeactivatesAndReschedulesGroups(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: now.Add(-5 * time.Minute), SuppressedCount: 7},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 10, 300)}}
	alerts := &fakeStormAlertRepo{
		count: 3, // 低于阈值 10
		active: []domain.Alert{
			{ID: "a1", Title: "t1", GroupKey: "gk1"},
			{ID: "a2", Title: "t2", GroupKey: "gk1"}, // 与 a1 同组，distinct 后只算一次
			{ID: "a3", Title: "t3", GroupKey: "gk2"},
			{ID: "a4", Title: "t4", GroupKey: ""}, // 空 group_key 不应被调度
		},
	}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("Handle 返回意外错误: %v", err)
	}

	if state.states["r1"].Active {
		t.Fatal("回落后应解除熔断（active=false）")
	}
	if state.states["r1"].DeactivatedAt == nil || !state.states["r1"].DeactivatedAt.Equal(now) {
		t.Fatal("DeactivatedAt 应记录为本次触发时刻")
	}
	if state.states["r1"].SuppressedCount != 0 {
		t.Fatal("回落时也应清零 SuppressedCount（因 suppressed_count>0 仍先发一次摘要）")
	}

	decodeSentRequest(t, sched) // 摘要仍应发出（回落前的抑制不能丢）

	wantGroups := map[string]bool{"gk1": true, "gk2": true}
	gotGroups := map[string]bool{}
	wantFireAt := now.Add(300 * time.Second)
	for _, c := range sched.scheduleIfAbsentCalls {
		if c.kind != groupTimerKind {
			t.Fatalf("补组 timer 的 kind 应为 %q, got %q", groupTimerKind, c.kind)
		}
		gotGroups[c.refID] = true
		if !c.fireAt.Equal(wantFireAt) {
			t.Fatalf("补组 timer fireAt = %v, want %v (now+group_interval)", c.fireAt, wantFireAt)
		}
	}
	if len(gotGroups) != len(wantGroups) {
		t.Fatalf("补组 timer 数量/成员不符: got %v, want %v", gotGroups, wantGroups)
	}
	for k := range wantGroups {
		if !gotGroups[k] {
			t.Fatalf("缺少对 group_key=%s 的补组 timer", k)
		}
	}

	// 不应再续期 storm_summary。
	for _, c := range sched.scheduleCalls {
		if c.kind == TimerKindStormSummary {
			t.Fatal("回落后不应续期 storm_summary 定时器")
		}
	}
}

// ③ suppressed_count==0（本轮到期前没有新的抑制发生）但仍超阈值：跳过摘要发送，
// 只做续期，不触碰 AddSuppressed/MarkSummary。
func TestStormHandler_ZeroSuppressed_StillOverThreshold_SkipsSummaryOnlyRenews(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: now.Add(-time.Minute), SuppressedCount: 0},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 10, 300)}}
	alerts := &fakeStormAlertRepo{count: 20}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("Handle 返回意外错误: %v", err)
	}

	for _, c := range sched.scheduleCalls {
		if c.kind == notifySendTimerKind {
			t.Fatal("suppressed_count 为 0 时不应发摘要通知")
		}
	}
	if state.states["r1"].LastSummaryAt != nil {
		t.Fatal("未发摘要时不应记录 LastSummaryAt")
	}
	found := false
	for _, c := range sched.scheduleCalls {
		if c.kind == TimerKindStormSummary && c.refID == "r1" {
			found = true
		}
	}
	if !found {
		t.Fatal("仍超阈值时应续期")
	}
}

// ④ threshold<=0（阈值已被关闭）：视同回落，解除熔断 + 补组 timer，即便当前 route
// 记录的 threshold 已经改成 0（用户改配置关闭了风暴熔断）。
func TestStormHandler_ThresholdDisabled_TreatedAsFallback(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: now.Add(-time.Minute), SuppressedCount: 0},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 0, 60)}}
	alerts := &fakeStormAlertRepo{count: 999, active: []domain.Alert{{ID: "a1", GroupKey: "gk1"}}}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("Handle 返回意外错误: %v", err)
	}
	if state.states["r1"].Active {
		t.Fatal("阈值关闭时应解除熔断")
	}
	if len(sched.scheduleIfAbsentCalls) != 1 {
		t.Fatalf("应补 1 条组 timer，got %d", len(sched.scheduleIfAbsentCalls))
	}
}

// ⑤ state 不存在（防御性分支）：直接返回 nil，不调用任何仓储/调度方法。
func TestStormHandler_StateNil_NoOp(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 10, 300)}}
	alerts := &fakeStormAlertRepo{count: 100}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("Handle 返回意外错误: %v", err)
	}
	if len(sched.scheduleCalls) != 0 || len(sched.scheduleIfAbsentCalls) != 0 {
		t.Fatal("state 不存在时不应发起任何调度")
	}
}

// ⑥ 重投幂等：同一状态被 Handle 两次（模拟 at-least-once 重投），第二次不应重复
// 清零已经是 0 的计数、也不应重复发摘要——用真实的 fake 状态在两次调用间被
// 第一次调用实际修改（而不是每次重置 fixture），才能验证"第二次读到的是第一次
// 落库后的新状态"这一真实场景。
func TestStormHandler_Redelivery_SecondCallIsNoOpOnSummary(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: now.Add(-5 * time.Minute), SuppressedCount: 5},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{"r1": testRoute("r1", 10, 300)}}
	alerts := &fakeStormAlertRepo{count: 3} // 低于阈值，两次调用都会走"回落"分支
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("第一次 Handle 失败: %v", err)
	}
	sendCount := func() int {
		n := 0
		for _, c := range sched.scheduleCalls {
			if c.kind == notifySendTimerKind {
				n++
			}
		}
		return n
	}
	if sendCount() != 1 {
		t.Fatalf("第一次调用后应恰好发过 1 次摘要，got %d", sendCount())
	}

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("第二次（重投）Handle 失败: %v", err)
	}
	if sendCount() != 1 {
		t.Fatalf("重投场景下 suppressed_count 已被第一次清零，第二次不应再发摘要，累计发送次数 got %d want 1", sendCount())
	}
}

// ⑦ 路由在 storm_summary pending 期间被删除（GetByID 返回 ErrNotFound）：不能报错
// 焊死定时器，应解除熔断状态并对活跃告警的 distinct group_key 立即（fireAt=now，
// 没有 group_interval 配置可取）补组定时器；摘要无处可发（target 取自路由），跳过。
func TestStormHandler_RouteDeleted_DeactivatesAndReschedulesImmediately(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	state := &fakeStormStateRepo{states: map[string]*store.StormState{
		"r1": {RouteID: "r1", Active: true, ActivatedAt: now.Add(-5 * time.Minute), SuppressedCount: 4},
	}}
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{}} // r1 已被删除
	alerts := &fakeStormAlertRepo{active: []domain.Alert{
		{ID: "a1", Title: "t1", GroupKey: "gk1"},
		{ID: "a2", Title: "t2", GroupKey: "gk2"},
	}}
	sched := &fakeStormScheduler{}
	h := newTestStormHandler(state, routes, alerts, sched, now)

	if err := h.Handle(context.Background(), nil, "r1", nil); err != nil {
		t.Fatalf("路由已删除时 Handle 不应报错（报错会焊死定时器），实际 = %v", err)
	}
	if state.states["r1"].Active {
		t.Fatal("路由已删除应解除熔断（active=false）")
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("路由已删除时摘要无处可发，不应有任何 Schedule 调用，实际 = %v", sched.scheduleCalls)
	}
	gotGroups := map[string]bool{}
	for _, c := range sched.scheduleIfAbsentCalls {
		if c.kind != groupTimerKind {
			t.Fatalf("补组 timer 的 kind 应为 %q, got %q", groupTimerKind, c.kind)
		}
		if !c.fireAt.Equal(now) {
			t.Fatalf("路由已删除时补组 timer 应立即触发（fireAt=now），got %v", c.fireAt)
		}
		gotGroups[c.refID] = true
	}
	if !gotGroups["gk1"] || !gotGroups["gk2"] || len(gotGroups) != 2 {
		t.Fatalf("应对 gk1/gk2 各补一条组定时器，实际 = %v", gotGroups)
	}
}
