package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// fakeRouteRepo 是 RouteRepo 的可编程测试替身：记录 ListOrdered 被调用的次数，
// 供缓存命中/过期的测试断言。
type fakeRouteRepo struct {
	routes []store.Route
	err    error
	calls  int
}

func (f *fakeRouteRepo) ListOrdered(ctx context.Context, tx pgx.Tx, limit int) ([]store.Route, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.routes, nil
}

// routingUpdateCall 记录 fakeRouteAlertRepo.UpdateRouting 收到的参数。
type routingUpdateCall struct {
	id, teamID, groupKey, routeID string
}

// fakeRouteAlertRepo 是 RouteAlertRepo 的可编程测试替身。
type fakeRouteAlertRepo struct {
	calls []routingUpdateCall
	err   error
}

func (f *fakeRouteAlertRepo) UpdateRouting(ctx context.Context, tx pgx.Tx, id, teamID, groupKey, routeID string) error {
	f.calls = append(f.calls, routingUpdateCall{id: id, teamID: teamID, groupKey: groupKey, routeID: routeID})
	return f.err
}

// mutableClock 是 routeClock 的可变实现：测试可在两次 Process 调用之间推进时间，
// 用来驱动路由规则缓存的过期/命中断言，无需依赖真实 wall-clock。
type mutableClock struct{ t time.Time }

func (c *mutableClock) Now() time.Time { return c.t }

func newRouteStageForTest(routes RouteRepo, alerts RouteAlertRepo, defaultTeamID string, clock routeClock) *RouteStage {
	s := NewRouteStage(routes, alerts, defaultTeamID)
	s.clock = clock
	return s
}

func TestRouteStage_Name(t *testing.T) {
	s := NewRouteStage(&fakeRouteRepo{}, &fakeRouteAlertRepo{}, "team-default")
	if got := s.Name(); got != "route" {
		t.Fatalf("Name() = %q, want %q", got, "route")
	}
}

// ① 优先级顺序：priority 数字小的先匹配，即使后面还有一条也能匹配的路由，first-match-wins。
func TestRouteStage_Process_PriorityOrder_FirstMatchWins(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{
			ID:           "r-low",
			Priority:     1,
			Matchers:     domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "staging"}},
			TargetTeamID: "team-low",
		},
		{
			ID:                    "r-mid",
			Priority:              2,
			Matchers:              domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}},
			TargetTeamID:          "team-mid",
			GroupBy:               []string{"env", "service"},
			GroupWaitSeconds:      30,
			GroupIntervalSeconds:  300,
			RepeatIntervalSeconds: 14400,
		},
		{
			ID:           "r-late",
			Priority:     3,
			Matchers:     domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}},
			TargetTeamID: "team-late",
		},
	}}
	alerts := &fakeRouteAlertRepo{}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: now})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1", Labels: map[string]string{"env": "prod"}}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if pctx.Route.RouteID != "r-mid" {
		t.Fatalf("Route.RouteID = %q, want r-mid（优先级更高的应该赢）", pctx.Route.RouteID)
	}
	if pctx.Route.TeamID != "team-mid" || pctx.Alert.TeamID != "team-mid" {
		t.Fatalf("TeamID = %q/%q, want team-mid", pctx.Route.TeamID, pctx.Alert.TeamID)
	}
	if pctx.Alert.RouteID != "r-mid" {
		t.Fatalf("Alert.RouteID = %q, want r-mid（命中路由后应同步写回 Alert，供分组 stage 透传）", pctx.Alert.RouteID)
	}
	if pctx.Route.GroupWait != 30*time.Second || pctx.Route.GroupInterval != 300*time.Second || pctx.Route.RepeatInterval != 14400*time.Second {
		t.Fatalf("分组参数转换错误: %+v", pctx.Route)
	}
	if len(alerts.calls) != 1 || alerts.calls[0] != (routingUpdateCall{id: "alert-1", teamID: "team-mid", groupKey: "", routeID: "r-mid"}) {
		t.Fatalf("UpdateRouting 调用 = %+v, want [{alert-1 team-mid  r-mid}]", alerts.calls)
	}
}

// ② 无命中：所有路由都不匹配时兜底到构造时注入的默认团队。
func TestRouteStage_Process_NoMatch_FallsBackToDefaultTeam(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r1", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "staging"}}, TargetTeamID: "team-staging"},
	}}
	alerts := &fakeRouteAlertRepo{}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-2", Labels: map[string]string{"env": "prod"}}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if pctx.Route.RouteID != "" {
		t.Fatalf("Route.RouteID = %q, want 空（未命中任何规则）", pctx.Route.RouteID)
	}
	if pctx.Route.TeamID != "team-default" || pctx.Alert.TeamID != "team-default" {
		t.Fatalf("TeamID = %q/%q, want team-default", pctx.Route.TeamID, pctx.Alert.TeamID)
	}
	if pctx.Alert.RouteID != "" {
		t.Fatalf("Alert.RouteID = %q, want 空（未命中任何规则，落到默认团队兜底）", pctx.Alert.RouteID)
	}
	if len(alerts.calls) != 1 || alerts.calls[0].teamID != "team-default" || alerts.calls[0].routeID != "" {
		t.Fatalf("UpdateRouting 调用 = %+v, want teamID=team-default, routeID=空", alerts.calls)
	}
}

// ③ 缓存命中：TTL 内连续两次 Process 只应查一次库。
func TestRouteStage_Process_CacheHit_WithinTTL_DoesNotRequery(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r1", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}}, TargetTeamID: "team-a"},
	}}
	clock := &mutableClock{t: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
	stage := newRouteStageForTest(routes, &fakeRouteAlertRepo{}, "team-default", clock)

	pctx1 := &PipelineContext{Alert: domain.Alert{ID: "alert-3", Labels: map[string]string{"env": "prod"}}}
	if err := stage.Process(context.Background(), pctx1); err != nil {
		t.Fatalf("第一次 Process() 返回意外错误: %v", err)
	}
	clock.t = clock.t.Add(5 * time.Second) // 未超过 10s TTL

	pctx2 := &PipelineContext{Alert: domain.Alert{ID: "alert-4", Labels: map[string]string{"env": "prod"}}}
	if err := stage.Process(context.Background(), pctx2); err != nil {
		t.Fatalf("第二次 Process() 返回意外错误: %v", err)
	}
	if routes.calls != 1 {
		t.Fatalf("ListOrdered 被调用 %d 次, want 1（缓存应命中）", routes.calls)
	}
}

// ④ 缓存过期：超过 10s TTL 后下一次 Process 应重新查库刷新缓存。
func TestRouteStage_Process_CacheExpiry_RequeriesAfterTTL(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r1", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}}, TargetTeamID: "team-a"},
	}}
	clock := &mutableClock{t: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
	stage := newRouteStageForTest(routes, &fakeRouteAlertRepo{}, "team-default", clock)

	pctx1 := &PipelineContext{Alert: domain.Alert{ID: "alert-5", Labels: map[string]string{"env": "prod"}}}
	if err := stage.Process(context.Background(), pctx1); err != nil {
		t.Fatalf("第一次 Process() 返回意外错误: %v", err)
	}
	clock.t = clock.t.Add(11 * time.Second) // 超过 10s TTL

	pctx2 := &PipelineContext{Alert: domain.Alert{ID: "alert-6", Labels: map[string]string{"env": "prod"}}}
	if err := stage.Process(context.Background(), pctx2); err != nil {
		t.Fatalf("第二次 Process() 返回意外错误: %v", err)
	}
	if routes.calls != 2 {
		t.Fatalf("ListOrdered 被调用 %d 次, want 2（缓存过期应重查）", routes.calls)
	}
}

// ⑤ 路由规则查询失败应向上传播为 stage 失败（"求值错误的传播"：domain.MatcherSet.Matches
// 本身不返回 error——非法正则被 compileAnchored 当作不命中处理，不会 panic/报错——
// 因此这里覆盖的是 RouteStage 唯一真实的错误来源：加载路由规则失败）。
func TestRouteStage_Process_ListOrderedError_Propagates(t *testing.T) {
	wantErr := errors.New("数据库连接失败")
	routes := &fakeRouteRepo{err: wantErr}
	alerts := &fakeRouteAlertRepo{}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-7", Labels: map[string]string{"env": "prod"}}}

	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
	if len(alerts.calls) != 0 {
		t.Fatal("加载路由规则失败后不应继续调用 UpdateRouting")
	}
}

// ⑥ 零值 Alert（迟到 resolved 事件被 dedup stage 忽略）：直接跳过路由，不报错、不查库。
func TestRouteStage_Process_ZeroValueAlert_Skipped(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{{ID: "r1", Priority: 1, TargetTeamID: "team-a"}}}
	alerts := &fakeRouteAlertRepo{}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{} // pctx.Alert 是零值，ID == ""

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 对零值 Alert 返回了意外错误: %v", err)
	}
	if routes.calls != 0 {
		t.Fatalf("零值 Alert 不应查询路由规则，实际调用 %d 次", routes.calls)
	}
	if len(alerts.calls) != 0 {
		t.Fatalf("零值 Alert 不应调用 UpdateRouting，实际调用 %d 次", len(alerts.calls))
	}
}

// ⑦ 合并场景下 Alert 已有 group_key：路由 stage 不应把它覆盖为空字符串。
func TestRouteStage_Process_PreservesExistingGroupKeyOnMerge(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r1", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}}, TargetTeamID: "team-a"},
	}}
	alerts := &fakeRouteAlertRepo{}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{
		ID:       "alert-8",
		Labels:   map[string]string{"env": "prod"},
		GroupKey: "existing-group-key",
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if len(alerts.calls) != 1 || alerts.calls[0].groupKey != "existing-group-key" {
		t.Fatalf("UpdateRouting 收到的 groupKey = %+v, want existing-group-key", alerts.calls)
	}
}

// ⑧ 持久化路由结果失败应向上传播。
func TestRouteStage_Process_UpdateRoutingError_Propagates(t *testing.T) {
	wantErr := errors.New("写入失败")
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r1", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}}, TargetTeamID: "team-a"},
	}}
	alerts := &fakeRouteAlertRepo{err: wantErr}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-9", Labels: map[string]string{"env": "prod"}}}

	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑨ 路由重配从"命中 r1"变为"命中 r2"：pctx.Alert.RouteID 应改写为新命中的路由 ID，
// 而不是保留上一轮遗留的旧值——route_id 是路由 stage 自己负责的字段，理应无条件
// 覆盖为本轮求出的最新结果。
func TestRouteStage_Process_RouteIDUpdatesOnReroute(t *testing.T) {
	routes := &fakeRouteRepo{routes: []store.Route{
		{ID: "r2", Priority: 1, Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "prod"}}, TargetTeamID: "team-b"},
	}}
	alerts := &fakeRouteAlertRepo{}
	stage := newRouteStageForTest(routes, alerts, "team-default", &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{
		ID:      "alert-10",
		Labels:  map[string]string{"env": "prod"},
		RouteID: "r1", // 上一轮命中的旧路由 ID
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if pctx.Alert.RouteID != "r2" {
		t.Fatalf("Alert.RouteID = %q, want 改写为本轮命中的 r2", pctx.Alert.RouteID)
	}
	if len(alerts.calls) != 1 || alerts.calls[0].routeID != "r2" {
		t.Fatalf("UpdateRouting 收到的 routeID = %+v, want r2", alerts.calls)
	}
}

func TestInhibitStage_Name(t *testing.T) {
	s := NewInhibitStage()
	if got := s.Name(); got != "inhibit" {
		t.Fatalf("Name() = %q, want %q", got, "inhibit")
	}
}

// 抑制 stage 本期恒返回 nil，即便面对零值 pctx 也不应出错——它是无条件通过的占位实现。
func TestInhibitStage_Process_AlwaysReturnsNil(t *testing.T) {
	s := NewInhibitStage()
	if err := s.Process(context.Background(), &PipelineContext{}); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
}
