package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// 本文件是纯单测（无 build tag，无需 Docker）：notifyOutbox 依赖的调度器/路由仓储/
// 渠道升级/人员升级链四个窄接口全用内存 fake 实现，验证缺陷 1（路由级 repeat_interval
// 生效）与缺陷 2（分组通知 Body 列出组内成员摘要）两处修复的分支逻辑。
// fakeStormScheduler/fakeStormRouteRepo 复用 storm_test.go 里已有的实现——两者的方法
// 集合恰好分别满足 notifyOutboxScheduler 与 notifyOutboxRouteRepo，不需要重新定义。

// fakeEngineStarter 是 notifyOutboxEngineStarter 的测试替身。
type fakeEngineStarter struct {
	err   error
	calls []domain.Alert
}

func (f *fakeEngineStarter) Start(ctx context.Context, tx pgx.Tx, alert domain.Alert, baseTime time.Time) error {
	f.calls = append(f.calls, alert)
	return f.err
}

// fakeChainStarter 是 notifyOutboxChainStarter 的测试替身。
type fakeChainStarter struct {
	err   error
	calls []domain.Alert
}

func (f *fakeChainStarter) Start(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	f.calls = append(f.calls, alert)
	return f.err
}

// newTestOutbox 构造带固定时钟的 notifyOutbox，globalInterval 是 cfg.RepeatInterval
// 对应的全局默认值。
func newTestOutbox(sched *fakeStormScheduler, routes *fakeStormRouteRepo, engine *fakeEngineStarter, chain *fakeChainStarter, globalInterval time.Duration, now time.Time) *notifyOutbox {
	var routesArg notifyOutboxRouteRepo
	if routes != nil {
		routesArg = routes
	}
	return &notifyOutbox{
		scheduler:        sched,
		engine:           engine,
		chain:            chain,
		routes:           routesArg,
		repeatInterval:   globalInterval,
		personnelChannel: "email",
		now:              func() time.Time { return now },
	}
}

// firstRepeatScheduleCall 找出 NotifyGroup 里为 alert_repeat 注册的 ScheduleIfAbsent 调用。
func firstRepeatScheduleCall(t *testing.T, sched *fakeStormScheduler) scheduleCall {
	t.Helper()
	for _, c := range sched.scheduleIfAbsentCalls {
		if c.kind == notify.TimerKindAlertRepeat {
			return c
		}
	}
	t.Fatal("未找到 alert_repeat 的 ScheduleIfAbsent 调用")
	return scheduleCall{}
}

func decodeRepeatPayload(t *testing.T, payload []byte) notify.RepeatPayload {
	t.Helper()
	var rp notify.RepeatPayload
	if err := json.Unmarshal(payload, &rp); err != nil {
		t.Fatalf("反序列化 RepeatPayload 失败: %v", err)
	}
	return rp
}

// 缺陷 1：路由配置了 repeat_interval_seconds 时，首个 alert_repeat 定时器应使用
// 路由值而非全局默认，且把该值写入 payload。
func TestNotifyOutbox_NotifyGroup_UsesRouteRepeatInterval(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	globalInterval := 30 * time.Minute
	routes := &fakeStormRouteRepo{routes: map[string]store.Route{
		"route-1": {ID: "route-1", RepeatIntervalSeconds: 120},
	}}
	sched := &fakeStormScheduler{}
	engine, chain := &fakeEngineStarter{}, &fakeChainStarter{}
	outbox := newTestOutbox(sched, routes, engine, chain, globalInterval, now)

	alert := domain.Alert{ID: "alert-1", TeamID: "team-x", RouteID: "route-1", Title: "t1", StartsAt: now, Severity: domain.SeverityCritical}
	if err := outbox.NotifyGroup(context.Background(), nil, "group-1", []domain.Alert{alert}); err != nil {
		t.Fatalf("NotifyGroup() 返回意外错误: %v", err)
	}

	call := firstRepeatScheduleCall(t, sched)
	wantFireAt := now.Add(120 * time.Second)
	if !call.fireAt.Equal(wantFireAt) {
		t.Fatalf("fireAt = %v, want %v（应使用路由配置的 120s，而非全局默认 %v）", call.fireAt, wantFireAt, globalInterval)
	}
	rp := decodeRepeatPayload(t, call.payload)
	if rp.IntervalSeconds != 120 {
		t.Fatalf("payload.IntervalSeconds = %d, want 120", rp.IntervalSeconds)
	}
}

// 缺陷 1：路由的 repeat_interval_seconds 为 0（未配置）或路由不存在时，都应回落
// 全局默认值。
func TestNotifyOutbox_NotifyGroup_FallsBackToGlobalInterval(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	globalInterval := 30 * time.Minute

	cases := map[string]struct {
		routeID string
		routes  *fakeStormRouteRepo
	}{
		"路由存在但未配置repeat_interval": {
			routeID: "route-zero",
			routes:  &fakeStormRouteRepo{routes: map[string]store.Route{"route-zero": {ID: "route-zero", RepeatIntervalSeconds: 0}}},
		},
		"路由不存在": {
			routeID: "route-missing",
			routes:  &fakeStormRouteRepo{routes: map[string]store.Route{}},
		},
		"告警未路由": {
			routeID: "",
			routes:  &fakeStormRouteRepo{routes: map[string]store.Route{}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sched := &fakeStormScheduler{}
			engine, chain := &fakeEngineStarter{}, &fakeChainStarter{}
			outbox := newTestOutbox(sched, tc.routes, engine, chain, globalInterval, now)

			alert := domain.Alert{ID: "alert-x", TeamID: "team-x", RouteID: tc.routeID, Title: "t", StartsAt: now, Severity: domain.SeverityWarning}
			if err := outbox.NotifyGroup(context.Background(), nil, "group-x", []domain.Alert{alert}); err != nil {
				t.Fatalf("NotifyGroup() 返回意外错误: %v", err)
			}

			call := firstRepeatScheduleCall(t, sched)
			wantFireAt := now.Add(globalInterval)
			if !call.fireAt.Equal(wantFireAt) {
				t.Fatalf("fireAt = %v, want %v（应回落全局默认）", call.fireAt, wantFireAt)
			}
			rp := decodeRepeatPayload(t, call.payload)
			if time.Duration(rp.IntervalSeconds)*time.Second != globalInterval {
				t.Fatalf("payload.IntervalSeconds = %d, want %v", rp.IntervalSeconds, globalInterval)
			}
		})
	}
}

func mkAlert(id, title string, severity domain.Severity, startsAt time.Time) domain.Alert {
	return domain.Alert{ID: id, TeamID: "team-x", Title: title, Severity: severity, StartsAt: startsAt}
}

// 缺陷 2：只有 1 条告警时 Body 保持原样，不添加多余格式。
func TestBuildGroupBody_SingleAlert_KeepsPlainTitle(t *testing.T) {
	now := time.Now()
	alerts := []domain.Alert{mkAlert("a1", "10.61.17.11 CPU 过高", domain.SeverityCritical, now)}
	body := buildGroupBody(alerts)
	if body != "10.61.17.11 CPU 过高" {
		t.Fatalf("body = %q, want 原样标题", body)
	}
}

// 缺陷 2：5 条以内全部列出，按严重程度从高到低、同级按 StartsAt 从早到晚排序。
func TestBuildGroupBody_FiveAlerts_ListsAllSortedBySeverityThenStartsAt(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	alerts := []domain.Alert{
		mkAlert("a1", "info-早", domain.SeverityInfo, base),
		mkAlert("a2", "critical-晚", domain.SeverityCritical, base.Add(2*time.Minute)),
		mkAlert("a3", "warning-早", domain.SeverityWarning, base.Add(1*time.Minute)),
		mkAlert("a4", "critical-早", domain.SeverityCritical, base),
		mkAlert("a5", "warning-晚", domain.SeverityWarning, base.Add(3*time.Minute)),
	}
	body := buildGroupBody(alerts)
	want := "critical-早；critical-晚；warning-早；warning-晚；info-早"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// 缺陷 2：超过 5 条时只列前 5 条，末尾折叠为"…另有 N 条"。
func TestBuildGroupBody_SevenAlerts_TruncatesWithRemainingCount(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var alerts []domain.Alert
	for i := 0; i < 7; i++ {
		alerts = append(alerts, mkAlert(
			string(rune('a'+i)),
			"host-"+string(rune('0'+i)),
			domain.SeverityWarning,
			base.Add(time.Duration(i)*time.Minute),
		))
	}
	body := buildGroupBody(alerts)
	want := "host-0；host-1；host-2；host-3；host-4；…另有 2 条"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// 缺陷 2：输入顺序打乱不应影响输出（幂等键依赖确定性排序）。
func TestBuildGroupBody_Deterministic_RegardlessOfInputOrder(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	a := mkAlert("a", "critical-早", domain.SeverityCritical, base)
	b := mkAlert("b", "critical-晚", domain.SeverityCritical, base.Add(time.Minute))
	c := mkAlert("c", "info-中", domain.SeverityInfo, base.Add(30*time.Second))

	order1 := buildGroupBody([]domain.Alert{a, b, c})
	order2 := buildGroupBody([]domain.Alert{c, b, a})
	order3 := buildGroupBody([]domain.Alert{b, a, c})

	if order1 != order2 || order2 != order3 {
		t.Fatalf("body 应与输入顺序无关，got %q / %q / %q", order1, order2, order3)
	}
}

// failingRouteRepo 模拟路由查询遇到非"未找到"的数据库错误。
type failingRouteRepo struct{ err error }

func (f failingRouteRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error) {
	return nil, f.err
}

// 非"未找到"的查询错误必须上抛：同一 pgx.Tx 内失败的查询已让事务进入 aborted 状态，
// 静默回落只会让后续 ScheduleIfAbsent 以"current transaction is aborted"失败并掩盖根因。
func TestNotifyOutbox_NotifyGroup_RouteLookupErrorPropagates(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	dbErr := errors.New("conn reset")
	sched := &fakeStormScheduler{}
	outbox := &notifyOutbox{
		scheduler: sched, engine: &fakeEngineStarter{}, chain: &fakeChainStarter{},
		routes: failingRouteRepo{err: dbErr}, repeatInterval: 30 * time.Minute,
		personnelChannel: "email", now: func() time.Time { return now },
	}
	alert := domain.Alert{ID: "alert-1", TeamID: "team-x", RouteID: "route-1", Title: "t", StartsAt: now, Severity: domain.SeverityWarning}
	err := outbox.NotifyGroup(context.Background(), nil, "group-x", []domain.Alert{alert})
	if !errors.Is(err, dbErr) {
		t.Fatalf("NotifyGroup() err = %v, want 包装 %v", err, dbErr)
	}
	for _, c := range sched.scheduleIfAbsentCalls {
		if c.kind == notify.TimerKindAlertRepeat {
			t.Fatal("路由查询失败时不应注册 alert_repeat 定时器")
		}
	}
}

// 批量触发时同级、同 StartsAt 很常见：排序必须是全序，乱序到达也要产出一致 Body。
func TestBuildGroupBody_Deterministic_WhenSeverityAndStartsAtTie(t *testing.T) {
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	a := mkAlert("id-1", "node-exporter down on 10.61.17.11:9100", domain.SeverityWarning, at)
	b := mkAlert("id-2", "node-exporter down on 10.61.17.12:9100", domain.SeverityWarning, at)
	c := mkAlert("id-3", "node-exporter down on 10.61.17.13:9100", domain.SeverityWarning, at)
	d := mkAlert("id-0", "node-exporter down on 10.61.17.11:9100", domain.SeverityWarning, at)

	want := buildGroupBody([]domain.Alert{a, b, c, d})
	for _, order := range [][]domain.Alert{{d, c, b, a}, {c, a, d, b}, {b, d, a, c}} {
		if got := buildGroupBody(order); got != want {
			t.Fatalf("同级同时刻输入顺序不同导致 body 不同：got %q, want %q", got, want)
		}
	}
}
