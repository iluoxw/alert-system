package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// fakeGroupAlertRepo 是 GroupAlertRepo 的可编程测试替身。
type fakeGroupAlertRepo struct {
	updateCalls []routingUpdateCall // 复用 stage_route_test.go 定义的类型
	updateErr   error

	members   map[string][]domain.Alert
	listCalls []string
	listErr   error

	// count/countErr 供风暴判定测试可控地返回 CountByRouteSince 结果。
	count     int
	countErr  error
	countArgs []string // 记录每次调用的 routeID，供断言
}

func (f *fakeGroupAlertRepo) UpdateRouting(ctx context.Context, tx pgx.Tx, id, teamID, groupKey, routeID string) error {
	f.updateCalls = append(f.updateCalls, routingUpdateCall{id: id, teamID: teamID, groupKey: groupKey, routeID: routeID})
	return f.updateErr
}

func (f *fakeGroupAlertRepo) ListByGroupKey(ctx context.Context, tx pgx.Tx, groupKey string) ([]domain.Alert, error) {
	f.listCalls = append(f.listCalls, groupKey)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.members[groupKey], nil
}

func (f *fakeGroupAlertRepo) CountByRouteSince(ctx context.Context, tx pgx.Tx, routeID string, since time.Time) (int, error) {
	f.countArgs = append(f.countArgs, routeID)
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
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

// fakeGroupRouteRepo 是 GroupRouteRepo 的可编程测试替身。route 为 nil 时返回一个
// StormThresholdPerMin=0 的零值路由——风暴判定天然短路，是未显式配置时的安全默认值。
type fakeGroupRouteRepo struct {
	route *store.Route
	err   error
	calls []string
}

func (f *fakeGroupRouteRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	if f.route != nil {
		return f.route, nil
	}
	return &store.Route{ID: id}, nil
}

// fakeGroupStormRepo 是 GroupStormRepo 的可编程测试替身。state 为 nil 模拟
// StormStateRepo.Get 无历史记录时的 (nil, nil) 约定。
type fakeGroupStormRepo struct {
	state  *store.StormState
	getErr error
	actErr error
	addErr error

	activateCalls []string
	addSuppressed []int
}

func (f *fakeGroupStormRepo) Get(ctx context.Context, tx pgx.Tx, routeID string) (*store.StormState, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.state, nil
}

func (f *fakeGroupStormRepo) Activate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	f.activateCalls = append(f.activateCalls, routeID)
	return f.actErr
}

func (f *fakeGroupStormRepo) AddSuppressed(ctx context.Context, tx pgx.Tx, routeID string, n int) error {
	f.addSuppressed = append(f.addSuppressed, n)
	return f.addErr
}

// newGroupStageForTest 构造用于既有（非风暴场景）测试的 GroupStage：routes/storms
// 用零阈值默认替身填充——StormThresholdPerMin=0 时 Handle 的风暴判定整体短路，对
// 不关心风暴熔断的既有测试零行为影响。需要定制 routes/storms 行为的风暴专项测试
// 请直接调 NewGroupStage 并自行注入 fakeGroupRouteRepo/fakeGroupStormRepo。
func newGroupStageForTest(alerts GroupAlertRepo, sched GroupTimerScheduler, notifier GroupNotifier, clock groupClock) *GroupStage {
	s := NewGroupStage(alerts, sched, notifier, &fakeGroupRouteRepo{}, &fakeGroupStormRepo{})
	s.clock = clock
	return s
}

func TestGroupStage_Name(t *testing.T) {
	s := NewGroupStage(&fakeGroupAlertRepo{}, &fakeGroupScheduler{}, &fakeGroupNotifier{}, &fakeGroupRouteRepo{}, &fakeGroupStormRepo{})
	if got := s.Name(); got != "group" {
		t.Fatalf("Name() = %q, want %q", got, "group")
	}
}

// ① 未配置 group_by：退化为单例组（route_id+fingerprint），仍写 group_key、仍按
// GroupWait 注册组到期定时器——与 Alertmanager 语义一致，不存在脱离组机制的
// "直发"路径。
func TestGroupStage_Process_NoGroupBy_SingletonGroup_SchedulesGroupWait(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: now})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-1", TeamID: "team-a", Fingerprint: "fp-1"},
		Route: RouteResult{RouteID: "r1", TeamID: "team-a", GroupWait: 30 * time.Second, GroupInterval: 300 * time.Second}, // GroupBy 为空
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	wantKey := singletonGroupKey("r1", "fp-1")
	if pctx.Alert.GroupKey != wantKey {
		t.Fatalf("GroupKey = %q, want 单例组键 %q", pctx.Alert.GroupKey, wantKey)
	}
	if len(alerts.updateCalls) != 1 || alerts.updateCalls[0].groupKey != wantKey {
		t.Fatalf("UpdateRouting 调用 = %+v, want groupKey=%q", alerts.updateCalls, wantKey)
	}
	key := groupTimerKey(groupTimerKind, wantKey)
	fireAt, ok := sched.pending[key]
	if !ok {
		t.Fatal("未配置 group_by 也应注册一个 pending 组到期定时器（单例组）")
	}
	if !fireAt.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("fire_at = %v, want %v（组首应使用 GroupWait）", fireAt, now.Add(30*time.Second))
	}
}

// ② 路由重配从"有 group_by"变为"无 group_by"：应把 group_key 从旧的分组键改写为
// 新的单例键（而不是清空为空字符串），继续参与组到期定时器机制。
func TestGroupStage_Process_NoGroupBy_RewritesStaleGroupKeyToSingleton(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert: domain.Alert{ID: "alert-2", TeamID: "team-a", RouteID: "r1", Fingerprint: "fp-2", GroupKey: "stale-key"},
		Route: RouteResult{RouteID: "r1", TeamID: "team-a", GroupWait: 30 * time.Second},
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	wantKey := singletonGroupKey("r1", "fp-2")
	if len(alerts.updateCalls) != 1 || alerts.updateCalls[0] != (routingUpdateCall{id: "alert-2", teamID: "team-a", groupKey: wantKey, routeID: "r1"}) {
		t.Fatalf("UpdateRouting 调用 = %+v, want 改写为单例键 %q 且透传 routeID=r1", alerts.updateCalls, wantKey)
	}
	if pctx.Alert.GroupKey != wantKey {
		t.Fatalf("GroupKey = %q, want %q", pctx.Alert.GroupKey, wantKey)
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

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
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

	if err := stage.Handle(context.Background(), nil, "gk-empty", nil); err != nil {
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

	err := stage.Handle(context.Background(), nil, "gk-1", nil)
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

	err := stage.Handle(context.Background(), nil, "gk-1", nil)
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

// ⑰ window 模式（R1/C1）：固定组键，不同 fingerprint 的告警也应落入同一组，且
// 组首之后的加入者复用同一个 pending 组到期定时器（不重新注册）。
func TestGroupStage_Process_WindowMode_FixedGroupKey_IgnoresFingerprint(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: now})

	route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupMode: "window", GroupWait: 30 * time.Second, GroupInterval: 300 * time.Second}
	wantKey := windowGroupKey("r1")

	first := &PipelineContext{Alert: domain.Alert{ID: "alert-w1", TeamID: "team-a", Fingerprint: "fp-a"}, Route: route}
	if err := stage.Process(context.Background(), first); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if first.Alert.GroupKey != wantKey {
		t.Fatalf("GroupKey = %q, want 窗口固定键 %q", first.Alert.GroupKey, wantKey)
	}

	second := &PipelineContext{Alert: domain.Alert{ID: "alert-w2", TeamID: "team-a", Fingerprint: "fp-b"}, Route: route}
	if err := stage.Process(context.Background(), second); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if second.Alert.GroupKey != wantKey {
		t.Fatalf("不同 fingerprint 也应落同一窗口组，GroupKey = %q, want %q", second.Alert.GroupKey, wantKey)
	}
	if len(alerts.updateCalls) != 2 {
		t.Fatalf("两条告警都应各自写一次 group_key，实际调用次数 = %d", len(alerts.updateCalls))
	}
	key := groupTimerKey(groupTimerKind, wantKey)
	if _, ok := sched.pending[key]; !ok {
		t.Fatal("窗口组首告警应注册一个 pending 组到期定时器")
	}
	if sched.calls != 2 {
		t.Fatalf("ScheduleIfAbsent 应被调用两次（第二次因 pending 已存在而 DO NOTHING），实际 = %d", sched.calls)
	}
}

// ⑱ window 模式对 GroupMode 的判断是防御性的：空串/"label"/未知值都不落入 window
// 分支，一律走既有 label 语义（label 模式回归现状）。
func TestGroupStage_Process_UnknownGroupMode_FallsBackToLabelPath(t *testing.T) {
	for _, mode := range []string{"", "label", "some-unknown-mode"} {
		alerts := &fakeGroupAlertRepo{}
		sched := &fakeGroupScheduler{}
		stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

		route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupMode: mode, GroupBy: []string{"env"}, GroupWait: 30 * time.Second}
		pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-fb", TeamID: "team-a", Labels: map[string]string{"env": "prod"}}, Route: route}
		if err := stage.Process(context.Background(), pctx); err != nil {
			t.Fatalf("GroupMode=%q: Process() 返回意外错误: %v", mode, err)
		}
		wantKey := computeGroupKey("r1", []string{"env"}, groupByLabels(pctx.Alert))
		if pctx.Alert.GroupKey != wantKey {
			t.Fatalf("GroupMode=%q: GroupKey = %q, want label 模式组键 %q", mode, pctx.Alert.GroupKey, wantKey)
		}
	}
}

// ⑲ __template__（R2/C2）：同模板不同变量应落同组，不同模板应落不同组。
func TestGroupStage_Process_TemplateGroupBy_SameTemplateSameGroup(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupBy: []string{"__template__"}, GroupWait: 30 * time.Second}

	first := &PipelineContext{Alert: domain.Alert{ID: "alert-t1", TeamID: "team-a", Title: "Ping timeout from 10.0.0.1"}, Route: route}
	if err := stage.Process(context.Background(), first); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	second := &PipelineContext{Alert: domain.Alert{ID: "alert-t2", TeamID: "team-a", Title: "Ping timeout from 10.0.0.2"}, Route: route}
	if err := stage.Process(context.Background(), second); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if first.Alert.GroupKey != second.Alert.GroupKey {
		t.Fatalf("同模板不同变量应落同组: %q vs %q", first.Alert.GroupKey, second.Alert.GroupKey)
	}

	third := &PipelineContext{Alert: domain.Alert{ID: "alert-t3", TeamID: "team-a", Title: "Disk full on /data"}, Route: route}
	if err := stage.Process(context.Background(), third); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if third.Alert.GroupKey == first.Alert.GroupKey {
		t.Fatalf("不同模板不应落同组: %q", third.Alert.GroupKey)
	}
}

// ⑳ __template__ 可与普通 label 键混用：env 不同则即使同模板也应落不同组；
// env 相同 + 同模板不同变量应落同组。
func TestGroupStage_Process_TemplateGroupBy_MixedWithLabelKey(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	route := RouteResult{RouteID: "r1", TeamID: "team-a", GroupBy: []string{"env", "__template__"}, GroupWait: 30 * time.Second}

	base := &PipelineContext{Alert: domain.Alert{ID: "alert-m1", TeamID: "team-a", Title: "Ping timeout from 10.0.0.1", Labels: map[string]string{"env": "prod"}}, Route: route}
	if err := stage.Process(context.Background(), base); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	sameEnvDiffVar := &PipelineContext{Alert: domain.Alert{ID: "alert-m2", TeamID: "team-a", Title: "Ping timeout from 10.0.0.2", Labels: map[string]string{"env": "prod"}}, Route: route}
	if err := stage.Process(context.Background(), sameEnvDiffVar); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if base.Alert.GroupKey != sameEnvDiffVar.Alert.GroupKey {
		t.Fatalf("同 env + 同模板不同变量应落同组: %q vs %q", base.Alert.GroupKey, sameEnvDiffVar.Alert.GroupKey)
	}

	diffEnv := &PipelineContext{Alert: domain.Alert{ID: "alert-m3", TeamID: "team-a", Title: "Ping timeout from 10.0.0.1", Labels: map[string]string{"env": "staging"}}, Route: route}
	if err := stage.Process(context.Background(), diffEnv); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if diffEnv.Alert.GroupKey == base.Alert.GroupKey {
		t.Fatalf("不同 env 即使同模板也不应落同组: %q", diffEnv.Alert.GroupKey)
	}
}

// ㉑ groupByLabels：__template__ 是保留键，即使 alert.Labels 里真的存在一个同名
// 业务标签，也应被 TemplateFingerprint(alert.Title) 覆盖，而不是取 label 原值。
func TestGroupByLabels_TemplateReservedKey_OverridesRealLabelWithSameName(t *testing.T) {
	alert := domain.Alert{Title: "Ping timeout from 10.0.0.1", Labels: map[string]string{"__template__": "not-a-real-fingerprint"}}
	labels := groupByLabels(alert)
	want := domain.TemplateFingerprint(alert.Title)
	if labels[templateGroupByKey] != want {
		t.Fatalf("__template__ 取值 = %q, want 覆盖为 TemplateFingerprint = %q", labels[templateGroupByKey], want)
	}
}

// ㉒ 三种组键格式互斥：window 的 "-window" 后缀、single 的 "-single-" 中缀、
// computeGroupKey 的 "%s-%016x" 十六进制哈希格式，同一个 route_id 下三者不会碰撞。
func TestGroupKeyFormats_MutuallyExclusive(t *testing.T) {
	routeID := "r-collision"
	windowKey := windowGroupKey(routeID)
	singleKey := singletonGroupKey(routeID, "fp-1")
	hashKey := computeGroupKey(routeID, []string{"env"}, map[string]string{"env": "prod"})

	keys := map[string]string{"window": windowKey, "single": singleKey, "hash": hashKey}
	for name, k := range keys {
		for other, k2 := range keys {
			if name != other && k == k2 {
				t.Fatalf("组键格式碰撞: %s=%q 与 %s=%q 相同", name, k, other, k2)
			}
		}
	}
	if !strings.HasSuffix(windowKey, "-window") {
		t.Fatalf("windowGroupKey 应以 -window 结尾: %q", windowKey)
	}
	if strings.Contains(singleKey, "-window") || strings.HasSuffix(hashKey, "-window") {
		t.Fatalf("singletonGroupKey/computeGroupKey 的输出不应意外带有 -window 后缀")
	}
}

// newGroupStageForStormTest 是风暴/flapping 专项测试的构造 helper：与
// newGroupStageForTest 不同，routes/storms 由调用方显式传入以精确控制风暴判定
// 分支，而不是套用零阈值的默认替身。
func newGroupStageForStormTest(alerts GroupAlertRepo, routes GroupRouteRepo, storms GroupStormRepo, clock groupClock) (*GroupStage, *fakeGroupScheduler, *fakeGroupNotifier) {
	sched := &fakeGroupScheduler{}
	notifier := &fakeGroupNotifier{}
	s := NewGroupStage(alerts, sched, notifier, routes, storms)
	s.clock = clock
	return s, sched, notifier
}

// ㉓ flapping 告警（非置位当次）应跳过组定时器注册：group_key 仍照常写入，但不
// 调用 ScheduleIfAbsent（R4/C4）。
func TestGroupStage_Process_FlappingAlert_SkipsScheduleIfAbsent(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{
		Alert:           domain.Alert{ID: "alert-flap-1", TeamID: "team-a", Fingerprint: "fp-1", Flapping: true},
		Route:           RouteResult{RouteID: "r1", TeamID: "team-a", GroupWait: 30 * time.Second},
		FlappingStarted: false,
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	wantKey := singletonGroupKey("r1", "fp-1")
	if pctx.Alert.GroupKey != wantKey {
		t.Fatalf("GroupKey = %q, want %q（flapping 也应照常写入 group_key）", pctx.Alert.GroupKey, wantKey)
	}
	if sched.calls != 0 {
		t.Fatalf("flapping 告警（非置位当次）不应调用 ScheduleIfAbsent，实际调用次数 = %d", sched.calls)
	}
}

// ㉔ FlappingStarted 例外：本轮刚把 flapping 从 false 置为 true 的事件，应照常
// 注册组定时器，让"进入抖动"的这一次组通知能送出去（R4/C4）。
func TestGroupStage_Process_FlappingStarted_StillSchedules(t *testing.T) {
	alerts := &fakeGroupAlertRepo{}
	sched := &fakeGroupScheduler{}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage := newGroupStageForTest(alerts, sched, &fakeGroupNotifier{}, &mutableClock{t: now})

	pctx := &PipelineContext{
		Alert:           domain.Alert{ID: "alert-flap-2", TeamID: "team-a", Fingerprint: "fp-2", Flapping: true},
		Route:           RouteResult{RouteID: "r1", TeamID: "team-a", GroupWait: 30 * time.Second},
		FlappingStarted: true,
	}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	wantKey := singletonGroupKey("r1", "fp-2")
	key := groupTimerKey(groupTimerKind, wantKey)
	fireAt, ok := sched.pending[key]
	if !ok {
		t.Fatal("FlappingStarted 置位当次事件应照常注册组到期定时器")
	}
	if !fireAt.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("fire_at = %v, want %v", fireAt, now.Add(30*time.Second))
	}
}

// ㉕ 风暴阈值未配置（StormThresholdPerMin==0）：即使 CountByRouteSince 会返回一个
// 很大的数，风暴判定也应整体短路——不查风暴状态、不跳过通知（R3/C3 opt-in）。
func TestGroupStage_Handle_StormThresholdZero_NoOp(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: "r1"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}, count: 999}
	routes := &fakeGroupRouteRepo{route: &store.Route{ID: "r1", StormThresholdPerMin: 0}}
	storms := &fakeGroupStormRepo{}
	stage, _, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("阈值未配置时应照常调用 NotifyGroup，实际调用次数 = %d", len(notifier.calls))
	}
	if len(alerts.countArgs) != 0 {
		t.Fatalf("阈值未配置时不应调用 CountByRouteSince，实际调用 = %v", alerts.countArgs)
	}
	if len(storms.activateCalls) != 0 {
		t.Fatalf("阈值未配置时不应调用 Activate，实际调用 = %v", storms.activateCalls)
	}
}

// ㉖ 首次超阈值：CountByRouteSince 达标 -> 跳过 NotifyGroup，改为 Activate +
// AddSuppressed(过滤后非抖动成员数) + ScheduleIfAbsent(storm_summary, routeID, ...)。
func TestGroupStage_Handle_StormThresholdExceeded_ActivatesAndSuppresses(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: "r1"}, {ID: "a2", RouteID: "r1"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}, count: 10}
	routes := &fakeGroupRouteRepo{route: &store.Route{ID: "r1", StormThresholdPerMin: 5}}
	storms := &fakeGroupStormRepo{} // Get 返回 (nil, nil)：无历史熔断记录
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage, sched, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: now})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("超阈值应跳过 NotifyGroup，实际调用 = %+v", notifier.calls)
	}
	if len(storms.activateCalls) != 1 || storms.activateCalls[0] != "r1" {
		t.Fatalf("应调用 Activate(routeID=r1)，实际调用 = %v", storms.activateCalls)
	}
	if len(storms.addSuppressed) != 1 || storms.addSuppressed[0] != len(members) {
		t.Fatalf("AddSuppressed 应传入过滤后成员数 %d，实际 = %v", len(members), storms.addSuppressed)
	}
	key := groupTimerKey(stormSummaryTimerKind, "r1")
	fireAt, ok := sched.pending[key]
	if !ok {
		t.Fatal("超阈值应注册 storm_summary 定时器")
	}
	if !fireAt.Equal(now.Add(stormSummaryDelay)) {
		t.Fatalf("storm_summary fire_at = %v, want %v", fireAt, now.Add(stormSummaryDelay))
	}
}

// ㉗ 已熔断累计：即使本次 CountByRouteSince 回落到阈值以下，只要风暴状态已是
// active，也应继续跳过 NotifyGroup 并累计抑制计数（熔断的解除只由 storm_summary
// handler 的速率复查决定，GroupStage.Handle 不自行解除）。
func TestGroupStage_Handle_StormAlreadyActive_SkipsNotifyRegardlessOfCount(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: "r1"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}, count: 1} // 低于阈值
	routes := &fakeGroupRouteRepo{route: &store.Route{ID: "r1", StormThresholdPerMin: 5}}
	storms := &fakeGroupStormRepo{state: &store.StormState{RouteID: "r1", Active: true, SuppressedCount: 20}}
	stage, sched, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("已熔断 active 状态应跳过 NotifyGroup，实际调用 = %+v", notifier.calls)
	}
	if len(storms.activateCalls) != 1 {
		t.Fatalf("已 active 也应（幂等地）调用一次 Activate，实际调用 = %v", storms.activateCalls)
	}
	if len(storms.addSuppressed) != 1 || storms.addSuppressed[0] != 1 {
		t.Fatalf("AddSuppressed 应传入 1，实际 = %v", storms.addSuppressed)
	}
	key := groupTimerKey(stormSummaryTimerKind, "r1")
	if _, ok := sched.pending[key]; !ok {
		t.Fatal("已熔断累计场景也应（续期式地）注册 storm_summary 定时器")
	}
}

// ㉘ 组内成员全部处于抖动中：过滤后为空，整组跳过通知，不报错、也不触发风暴判定
// （风暴判定发生在过滤为空的短路之后，本场景应完全不查路由/风暴状态）。
func TestGroupStage_Handle_AllMembersFlapping_SkipsNotify(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: "r1", Flapping: true}, {ID: "a2", RouteID: "r1", Flapping: true}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}}
	routes := &fakeGroupRouteRepo{route: &store.Route{ID: "r1", StormThresholdPerMin: 1}}
	storms := &fakeGroupStormRepo{}
	stage, _, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("全部成员抖动中应跳过 NotifyGroup，实际调用 = %+v", notifier.calls)
	}
	if len(routes.calls) != 0 {
		t.Fatalf("过滤为空应在风暴判定之前短路，不应查询路由，实际调用 = %v", routes.calls)
	}
}

// ㉙ 组内部分成员抖动中：过滤后非空则照常通知，且传给 NotifyGroup 的是完整的原始
// members（含抖动中的成员），而不是过滤后的子集——这样"进入抖动"当次的组通知
// 才能把该告警带到消息里，供下游 Message 构造处按 alert.Flapping 标注"抖动中"。
func TestGroupStage_Handle_SomeMembersFlapping_NotifiesFullUnfilteredList(t *testing.T) {
	members := []domain.Alert{{ID: "a1"}, {ID: "a2", Flapping: true}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}}
	notifier := &fakeGroupNotifier{}
	stage := newGroupStageForTest(alerts, &fakeGroupScheduler{}, notifier, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 || len(notifier.calls[0].alerts) != 2 {
		t.Fatalf("NotifyGroup 应收到完整的 2 个成员（含抖动中的），实际 = %+v", notifier.calls)
	}
}

// ㉚ 风暴计数查询失败：fail-open 放行本次 NotifyGroup，且必须返回 nil——handler 返回
// error 会让整个事务回滚（NotifyGroup 意图被撤销）且定时器被标 failed 不重试
// （scheduler 语义），通知就永久丢了。错误只记日志。
func TestGroupStage_Handle_StormCountError_FailsOpenAndReturnsNil(t *testing.T) {
	countErr := errors.New("统计查询失败")
	members := []domain.Alert{{ID: "a1", RouteID: "r1"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}, countErr: countErr}
	routes := &fakeGroupRouteRepo{route: &store.Route{ID: "r1", StormThresholdPerMin: 5}}
	storms := &fakeGroupStormRepo{}
	stage, _, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("计数查询失败必须吞掉错误返回 nil（返回 error 会回滚通知意图并焊死定时器），实际 = %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("计数查询失败应 fail-open 放行本次 NotifyGroup，实际调用次数 = %d", len(notifier.calls))
	}
	if len(storms.activateCalls) != 0 {
		t.Fatalf("fail-open 分支不应触碰风暴状态，实际 Activate 调用 = %v", storms.activateCalls)
	}
}

// ㉛ 默认团队兜底路径（members[0].RouteID 为空串）：风暴判定必须短路跳过，不查
// 路由也不查风暴状态，直接照常通知。
func TestGroupStage_Handle_EmptyRouteID_SkipsStormJudgment(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: ""}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}}
	routes := &fakeGroupRouteRepo{}
	storms := &fakeGroupStormRepo{}
	stage, _, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("Handle() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("默认团队兜底路径应照常调用 NotifyGroup，实际调用次数 = %d", len(notifier.calls))
	}
	if len(routes.calls) != 0 {
		t.Fatalf("RouteID 为空串时不应查询路由，实际调用 = %v", routes.calls)
	}
}

// ㉜ 路由在组定时器 pending 期间被删除（GetByID 返回 store.ErrNotFound）：视同未配置
// storm 阈值，跳过风暴判定照常通知——返回 error 会把定时器焊死成 failed（scheduler
// 不重试），组通知永久丢失。
func TestGroupStage_Handle_RouteDeleted_SkipsStormAndNotifies(t *testing.T) {
	members := []domain.Alert{{ID: "a1", RouteID: "r-deleted"}}
	alerts := &fakeGroupAlertRepo{members: map[string][]domain.Alert{"gk-1": members}}
	routes := &fakeGroupRouteRepo{err: store.ErrNotFound}
	storms := &fakeGroupStormRepo{}
	stage, _, notifier := newGroupStageForStormTest(alerts, routes, storms, &mutableClock{t: time.Now()})

	if err := stage.Handle(context.Background(), nil, "gk-1", nil); err != nil {
		t.Fatalf("路由已删除时 Handle 不应报错（报错会焊死定时器丢通知），实际 = %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("路由已删除应照常调用 NotifyGroup，实际调用次数 = %d", len(notifier.calls))
	}
	if len(storms.activateCalls) != 0 {
		t.Fatalf("路由已删除不应触碰风暴状态，实际 Activate 调用 = %v", storms.activateCalls)
	}
}
