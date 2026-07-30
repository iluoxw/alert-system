package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// fakeGroupAlertRepo 是 GroupAlertRepo 的可编程测试替身。
type fakeGroupAlertRepo struct {
	updateCalls []routingUpdateCall // 复用 stage_route_test.go 定义的类型
	updateErr   error

	members   map[string][]domain.Alert
	listCalls []string
	listErr   error
}

func (f *fakeGroupAlertRepo) UpdateRouting(ctx context.Context, tx pgx.Tx, id, teamID, groupKey string) error {
	f.updateCalls = append(f.updateCalls, routingUpdateCall{id: id, teamID: teamID, groupKey: groupKey})
	return f.updateErr
}

func (f *fakeGroupAlertRepo) ListByGroupKey(ctx context.Context, tx pgx.Tx, groupKey string) ([]domain.Alert, error) {
	f.listCalls = append(f.listCalls, groupKey)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.members[groupKey], nil
}

// fakeGroupScheduler 精确模拟真实 scheduler 的 (kind,ref_id) pending 唯一性语义：
// pending 中已存在同 key 时 ScheduleIfAbsent 视为 DO NOTHING，不覆盖已记录的
// fire_at；markFired 供测试模拟"该定时器已到期触发"，把 key 从 pending 移到
// fired 历史，为"组内后续更新走 group_interval 节流"场景准备前置状态。
type fakeGroupScheduler struct {
	fired       map[string]bool
	firedErr    error
	pending     map[string]time.Time
	scheduleErr error
	calls       int
}

func groupTimerKey(kind, refID string) string { return kind + "|" + refID }

func (f *fakeGroupScheduler) HasFired(ctx context.Context, tx pgx.Tx, kind, refID string) (bool, error) {
	if f.firedErr != nil {
		return false, f.firedErr
	}
	return f.fired[groupTimerKey(kind, refID)], nil
}

func (f *fakeGroupScheduler) ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error) {
	f.calls++
	if f.scheduleErr != nil {
		return false, f.scheduleErr
	}
	if f.pending == nil {
		f.pending = map[string]time.Time{}
	}
	key := groupTimerKey(kind, refID)
	if _, exists := f.pending[key]; exists {
		return false, nil
	}
	f.pending[key] = fireAt
	return true, nil
}

func (f *fakeGroupScheduler) markFired(kind, refID string) {
	key := groupTimerKey(kind, refID)
	delete(f.pending, key)
	if f.fired == nil {
		f.fired = map[string]bool{}
	}
	f.fired[key] = true
}

// groupNotifyCall 记录 fakeGroupNotifier.NotifyGroup 收到的参数。
type groupNotifyCall struct {
	groupKey string
	alerts   []domain.Alert
}

type fakeGroupNotifier struct {
	calls []groupNotifyCall
	err   error
}

func (f *fakeGroupNotifier) NotifyGroup(ctx context.Context, tx pgx.Tx, groupKey string, alerts []domain.Alert) error {
	f.calls = append(f.calls, groupNotifyCall{groupKey: groupKey, alerts: alerts})
	return f.err
}

// fakeTxRunner 是 txRunner 的测试替身：直接以 nil tx 调用 fn，测试里的 fake 仓储/
// 通知器都不会真的使用 tx。
type fakeTxRunner struct {
	err error
}

func (f *fakeTxRunner) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	if f.err != nil {
		return f.err
	}
	return fn(nil)
}

func newGroupStageForTest(alerts GroupAlertRepo, sched GroupTimerScheduler, notifier GroupNotifier, clock groupClock) *GroupStage {
	s := NewGroupStage(&fakeTxRunner{}, alerts, sched, notifier)
	s.clock = clock
	return s
}

func TestGroupStage_Name(t *testing.T) {
	s := NewGroupStage(&fakeTxRunner{}, &fakeGroupAlertRepo{}, &fakeGroupScheduler{}, &fakeGroupNotifier{})
	if got := s.Name(); got != "group" {
		t.Fatalf("Name() = %q, want %q", got, "group")
	}
}

// ① 未配置 group_by：直发，不写 group_key、不查/注册任何定时器。
func TestGroupStage_Process_NoGroupBy_DirectDispatch(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-1", TeamID: "team-a"},
		Route: RouteResult{TeamID: "team-a"}, // GroupBy 为空
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 0 {
		t.Fatalf("未配置 group_by 不应调用 UpdateRouting，实际调用 %+v", alerts.updateCalls)
	}
	if sched.calls != 0 {
		t.Fatalf("未配置 group_by 不应查询/注册定时器，实际 ScheduleIfAbsent 调用 %d 次", sched.calls)
	}
	if pctx.Alert.GroupKey != "" {
		t.Fatalf("GroupKey = %q, want 空", pctx.Alert.GroupKey)
	}
}

// ② 未配置 group_by 但告警此前有 group_key（路由重配去掉了 group_by）：应清空。
func TestGroupStage_Process_NoGroupBy_ClearsStaleGroupKey(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-2", TeamID: "team-a", GroupKey: "stale-key"},
		Route: RouteResult{TeamID: "team-a"},
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 1 || alerts.updateCalls[0] != (routingUpdateCall{id: "alert-2", teamID: "team-a", groupKey: ""}) {
		t.Fatalf("UpdateRouting 调用 = %+v, want 清空 group_key", alerts.updateCalls)
	}
	if pctx.Alert.GroupKey != "" {
		t.Fatalf("GroupKey = %q, want 空", pctx.Alert.GroupKey)
	}
}

// ③ 组首告警：写入 group_key，HasFired=false，用 GroupWait 注册定时器。
func TestGroupStage_Process_FirstAlert_SchedulesGroupWait(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: now})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-3", TeamID: "team-a", Labels: map[string]string{"env": "prod"}},
		Route: RouteResult{RouteID: "r1", TeamID: "team-a", GroupBy: []string{"env"}, GroupWait: 30 * time.Second, GroupInterval: 300 * time.Second},
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if pctx.Alert.GroupKey == "" {
		t.Fatal("组首告警应写入非空 group_key")
	}
	if len(alerts.updateCalls) != 1 || alerts.updateCalls[0].groupKey != pctx.Alert.GroupKey {
		t.Fatalf("UpdateRouting 调用 = %+v, want groupKey=%q", alerts.updateCalls, pctx.Alert.GroupKey)
	}
	key := groupTimerKey(groupTimerKind, pctx.Alert.GroupKey)
	fireAt, ok := sched.pending[key]
	if !ok {
		t.Fatal("组首告警应注册一个 pending 定时器")
	}
	if !fireAt.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("fire_at = %v, want %v（组首应使用 GroupWait）", fireAt, now.Add(30*time.Second))
	}
}

// ④ 组内新告警不重置升级基准：组首注册后，即使时钟推进、后续告警加入，已注册
// 定时器的 fire_at 不应被改写。
func TestGroupStage_Process_JoiningAlert_DoesNotResetBaseline(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	clock := &mutableClock{t: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, clock)

	route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupBy: []string{"env"}, GroupWait: 30 * time.Second, GroupInterval: 300 * time.Second}
	labels := map[string]string{"env": "prod"}

	first := &PipelineContext{Alert: domain.Alert{ID: "alert-4a", TeamID: "team-a", Labels: labels}, Route: route}
	if err := stage.Process(context.Background(), first); err != nil {
		t.Fatalf("第一次 Process() 返回意外错误: %v", err)
	}
	key := groupTimerKey(groupTimerKind, first.Alert.GroupKey)
	baseline := sched.pending[key]

	clock.t = clock.t.Add(10 * time.Second) // 组首定时器仍在等待期内（< group_wait 30s）
	second := &PipelineContext{Alert: domain.Alert{ID: "alert-4b", TeamID: "team-a", Labels: labels}, Route: route}
	if err := stage.Process(context.Background(), second); err != nil {
		t.Fatalf("第二次 Process() 返回意外错误: %v", err)
	}
	if second.Alert.GroupKey != first.Alert.GroupKey {
		t.Fatalf("同标签组合应算出同一 group_key，got %q/%q", first.Alert.GroupKey, second.Alert.GroupKey)
	}
	if got := sched.pending[key]; !got.Equal(baseline) {
		t.Fatalf("后续加入者不应重置组首定时器基准: got fire_at=%v, want 保持 %v", got, baseline)
	}
}

// ⑤ 组内后续更新受 group_interval 节流：组首那一轮定时器已 fired 之后，新告警
// 加入同一组应改用 GroupInterval 延迟注册下一轮定时器。
func TestGroupStage_Process_AfterFlush_UsesGroupIntervalThrottle(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	clock := &mutableClock{t: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, clock)

	route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupBy: []string{"env"}, GroupWait: 30 * time.Second, GroupInterval: 300 * time.Second}
	labels := map[string]string{"env": "prod"}

	first := &PipelineContext{Alert: domain.Alert{ID: "alert-5a", TeamID: "team-a", Labels: labels}, Route: route}
	if err := stage.Process(context.Background(), first); err != nil {
		t.Fatalf("第一次 Process() 返回意外错误: %v", err)
	}
	groupKey := first.Alert.GroupKey
	sched.markFired(groupTimerKind, groupKey) // 模拟组首那一轮定时器已到期触发

	clock.t = clock.t.Add(60 * time.Second)
	second := &PipelineContext{Alert: domain.Alert{ID: "alert-5b", TeamID: "team-a", Labels: labels}, Route: route}
	if err := stage.Process(context.Background(), second); err != nil {
		t.Fatalf("第二次 Process() 返回意外错误: %v", err)
	}

	key := groupTimerKey(groupTimerKind, groupKey)
	fireAt, ok := sched.pending[key]
	if !ok {
		t.Fatal("组已 fired 后的新成员应重新注册一个 pending 定时器")
	}
	want := clock.t.Add(300 * time.Second)
	if !fireAt.Equal(want) {
		t.Fatalf("fire_at = %v, want %v（应使用 GroupInterval 而非 GroupWait）", fireAt, want)
	}
}

// ⑥ 零值 Alert：直接跳过，不查/不写任何东西。
func TestGroupStage_Process_ZeroValueAlert_Skipped(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 对零值 Alert 返回了意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 0 || sched.calls != 0 {
		t.Fatal("零值 Alert 不应触发任何仓储/定时器调用")
	}
}

// ⑦ resolved 告警：跳过入组与定时器注册，但不清空已有 group_key。
func TestGroupStage_Process_ResolvedAlert_SkippedButGroupKeyPreserved(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-6", Status: domain.StatusResolved, GroupKey: "existing-key"},
		Route: RouteResult{TeamID: "team-a", GroupBy: []string{"env"}, GroupWait: 30 * time.Second},
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 对 resolved Alert 返回了意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 0 || sched.calls != 0 {
		t.Fatal("resolved 告警不应触发任何仓储/定时器调用")
	}
	if pctx.Alert.GroupKey != "existing-key" {
		t.Fatalf("GroupKey = %q, want 保留原值 existing-key", pctx.Alert.GroupKey)
	}
}

// ⑧ UpdateRouting 失败应向上传播。
func TestGroupStage_Process_UpdateRoutingError_Propagates(t *testing.T) {
	wantErr := errors.New("写入失败")
	alerts := &fakeGroupAlertRepo{updateErr: wantErr}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-7", Labels: map[string]string{"env": "prod"}},
		Route: RouteResult{RouteID: "r1", GroupBy: []string{"env"}, GroupWait: 30 * time.Second},
	}
	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑨ HasFired 查询失败应向上传播。
func TestGroupStage_Process_HasFiredError_Propagates(t *testing.T) {
	wantErr := errors.New("查询失败")
	sched := &fakeGroupScheduler{firedErr: wantErr}
	stage := newGroupStageForTest(&fakeGroupAlertRepo{}, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-8", Labels: map[string]string{"env": "prod"}},
		Route: RouteResult{RouteID: "r1", GroupBy: []string{"env"}, GroupWait: 30 * time.Second},
	}
	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑩ ScheduleIfAbsent 失败应向上传播。
func TestGroupStage_Process_ScheduleError_Propagates(t *testing.T) {
	wantErr := errors.New("注册失败")
	sched := &fakeGroupScheduler{scheduleErr: wantErr}
	stage := newGroupStageForTest(&fakeGroupAlertRepo{}, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-9", Labels: map[string]string{"env": "prod"}},
		Route: RouteResult{RouteID: "r1", GroupBy: []string{"env"}, GroupWait: 30 * time.Second},
	}
	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑪ Handle：取到组内成员后交给 GroupNotifier。
func TestGroupStage_Handle_NotifiesGroupMembers(t *testing.T) {
	members := []domain.Alert{{ID: "a1"}, {ID: "a2"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}}
	notifier := &fakeGroupNotifier{}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, notifier, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].groupKey != "gk-1" || len(notifier.calls[0].alerts) != 2 {
		t.Fatalf("NotifyGroup 调用 = %+v, want groupKey=gk-1 且 2 个成员", notifier.calls)
	}
}

// ⑫ Handle：组内已无活跃成员（全部 resolved/迁出）时不调用 NotifyGroup，也不报错。
func TestGroupStage_Handle_NoMembers_NoNotifyCall(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	notifier := &fakeGroupNotifier{}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, notifier, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), "gk-empty", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("组内无成员时不应调用 NotifyGroup，实际调用 %+v", notifier.calls)
	}
}

// ⑬ Handle：ListByGroupKey 失败应向上传播。
func TestGroupStage_Handle_ListByGroupKeyError_Propagates(t *testing.T) {
	wantErr := errors.New("查询失败")
	alerts := &fakeGroupAlertRepo{listErr: wantErr}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	err := stage.Handle(context.Background(), "gk-1", nil)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Handle() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑭ Handle：NotifyGroup 失败应向上传播。
func TestGroupStage_Handle_NotifyError_Propagates(t *testing.T) {
	wantErr := errors.New("记录通知意图失败")
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": {{ID: "a1"}}}}
	notifier := &fakeGroupNotifier{err: wantErr}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, notifier, &mutableClock{t: time.Now()})

	err := stage.Handle(context.Background(), "gk-1", nil)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Handle() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑮ computeGroupKey：相同 route_id + 相同 group_by 取值应算出相同 group_key。
func TestComputeGroupKey_Deterministic(t *testing.T) {
	labels := map[string]string{"env": "prod", "service": "api"}
	k1 := computeGroupKey("r1", []string{"env", "service"}, labels)
	k2 := computeGroupKey("r1", []string{"env", "service"}, labels)
	if k1 != k2 {
		t.Fatalf("相同输入算出不同 group_key: %q vs %q", k1, k2)
	}
}

// ⑯ computeGroupKey：不同 route_id 即使标签取值相同也应算出不同 group_key
// （route_id 前缀避免跨路由碰撞）。
func TestComputeGroupKey_DifferentRouteID_DifferentKey(t *testing.T) {
	labels := map[string]string{"env": "prod"}
	k1 := computeGroupKey("r1", []string{"env"}, labels)
	k2 := computeGroupKey("r2", []string{"env"}, labels)
	if k1 == k2 {
		t.Fatalf("不同 route_id 算出了相同的 group_key: %q", k1)
	}
}
