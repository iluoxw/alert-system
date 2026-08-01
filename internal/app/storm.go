package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// TimerKindStormSummary 是风暴摘要的 timer kind（design.md C3，第 11 个 kind）。
// refID 固定用 routeID——同一路由同一时刻至多一条 in-flight 的摘要定时器。
const TimerKindStormSummary = "storm_summary"

// stormSummaryRecheckBackoff 是速率复查后仍超阈值时的续期间隔，与判定窗口
// （近 1 分钟计数，design.md C3）同量级。
const stormSummaryRecheckBackoff = time.Minute

// stormStateRepo 是 stormHandler 依赖的风暴状态仓储子集，由 store.StormStateRepo 实现。
type stormStateRepo interface {
	Get(ctx context.Context, tx pgx.Tx, routeID string) (*store.StormState, error)
	AddSuppressed(ctx context.Context, tx pgx.Tx, routeID string, n int) error
	MarkSummary(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error
	Deactivate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error
}

// stormRouteRepo 是 stormHandler 依赖的路由仓储子集：取目标团队（摘要通知 target）、
// storm_threshold_per_min（速率复查阈值）、group_interval_seconds（回落后补组
// 定时器的延迟基准），由 store.RouteRepo 实现。
type stormRouteRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error)
}

// stormAlertRepo 是 stormHandler 依赖的告警仓储子集：CountByRouteSince 供速率复查
// （任务 5 已落，走 idx_alerts_route_created 索引）；ListActiveByRoute 供摘要"代表
// 样本标题"与回落后补组定时器的 distinct group_key 来源，一次查询两处复用。
type stormAlertRepo interface {
	CountByRouteSince(ctx context.Context, tx pgx.Tx, routeID string, since time.Time) (int, error)
	ListActiveByRoute(ctx context.Context, tx pgx.Tx, routeID string) ([]domain.Alert, error)
}

// stormScheduler 是 stormHandler 依赖的定时器子集，由 scheduler.Scheduler 实现。
type stormScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error)
}

// stormHandler 实现 storm_summary 定时器（design.md C3）：
//  1. suppressed_count>0 时，按与 NotifyGroup 相同的 outbox 手法（Schedule 一条
//     notify_send）发一条摘要通知，随即清零计数、记录本次摘要时刻；
//  2. 无条件做一次速率复查（CountByRouteSince）：仍超阈值则续期 1 分钟后再查；
//     回落则解除熔断状态，并对该路由下活跃告警的 distinct group_key 各补一轮
//     group_wait 定时器（保证被熔断吞掉的组通知最终送达，design.md C3）。
//
// 全程只使用 scheduler 传入的 tx，不自开事务；每一步（AddSuppressed 归零、
// MarkSummary、Deactivate 均为 UPDATE，Schedule/ScheduleIfAbsent 本身幂等）
// 都可安全被 at-least-once 重投。
type stormHandler struct {
	state  stormStateRepo
	routes stormRouteRepo
	alerts stormAlertRepo
	sched  stormScheduler

	// now 默认 time.Now，测试替换为可控时钟。
	now func() time.Time
}

// newStormHandler 构造 stormHandler。
func newStormHandler(state stormStateRepo, routes stormRouteRepo, alerts stormAlertRepo, sched stormScheduler) *stormHandler {
	return &stormHandler{state: state, routes: routes, alerts: alerts, sched: sched, now: time.Now}
}

// Handle 实现 scheduler.Handler，refID 是 routeID。
func (h *stormHandler) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	routeID := refID
	now := h.now()

	state, err := h.state.Get(ctx, tx, routeID)
	if err != nil {
		return fmt.Errorf("app: 查询风暴状态失败: %w", err)
	}
	if state == nil {
		// 定时器到期时状态行已不存在（理论上不该发生：状态行只在 Activate 时创建，
		// 而 storm_summary 定时器也只在那之后才会被注册）——防御性短路，不是错误。
		return nil
	}

	route, err := h.routes.GetByID(ctx, tx, routeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("app: 查询路由失败: %w", err)
	}

	active, err := h.alerts.ListActiveByRoute(ctx, tx, routeID)
	if err != nil {
		return fmt.Errorf("app: 查询路由活跃告警失败: %w", err)
	}

	if route == nil {
		// 路由在 storm_summary pending 期间被管理员删除：摘要无处可发（target 取自
		// route.TargetTeamID）、熔断阈值也不复存在。解除状态并立即补组定时器
		// （fireAt=now——没有 group_interval 配置可取），让熔断期间被吞掉的组通知
		// 按成员各自的 team 尽快送达；返回 error 只会把本定时器焊死成 failed。
		if err := h.state.Deactivate(ctx, tx, routeID, now); err != nil {
			return fmt.Errorf("app: 解除风暴状态失败: %w", err)
		}
		return h.rescheduleGroups(ctx, tx, store.Route{}, active, now)
	}

	if state.SuppressedCount > 0 {
		if err := h.sendSummary(ctx, tx, routeID, *route, *state, active, now); err != nil {
			return err
		}
		if err := h.state.AddSuppressed(ctx, tx, routeID, -state.SuppressedCount); err != nil {
			return fmt.Errorf("app: 清零风暴抑制计数失败: %w", err)
		}
		if err := h.state.MarkSummary(ctx, tx, routeID, now); err != nil {
			return fmt.Errorf("app: 记录风暴摘要时刻失败: %w", err)
		}
	}

	count, err := h.alerts.CountByRouteSince(ctx, tx, routeID, now.Add(-time.Minute))
	if err != nil {
		return fmt.Errorf("app: 风暴速率复查失败: %w", err)
	}
	if route.StormThresholdPerMin > 0 && count >= route.StormThresholdPerMin {
		if err := h.sched.Schedule(ctx, tx, TimerKindStormSummary, routeID, now.Add(stormSummaryRecheckBackoff), nil); err != nil {
			return fmt.Errorf("app: 续期风暴摘要定时器失败: %w", err)
		}
		return nil
	}

	if err := h.state.Deactivate(ctx, tx, routeID, now); err != nil {
		return fmt.Errorf("app: 解除风暴状态失败: %w", err)
	}
	return h.rescheduleGroups(ctx, tx, *route, active, now)
}

// sendSummary 把一条风暴摘要发送意图注册进既有 "notify_send" 定时器，做法与
// notifyOutbox.scheduleSend 完全一致（同包内直接复用 notifySendTimerKind/
// notifySendRefID，不另起一套调度逻辑）：GroupKey 用 stormGroupKey(routeID) 前缀
// 区分于普通组通知，target 走团队固定渠道（与 NotifyGroup/NotifyRepeat 同一约定，
// 摘要本身是团队维度提示，不针对具体个人）。
func (h *stormHandler) sendSummary(ctx context.Context, tx pgx.Tx, routeID string, route store.Route, state store.StormState, active []domain.Alert, now time.Time) error {
	sampleTitle := ""
	if len(active) > 0 {
		sampleTitle = active[0].Title
	}

	req := notify.SendRequest{
		GroupKey:    stormGroupKey(routeID),
		ChannelKind: teamNotifyChannel,
		Target:      route.TargetTeamID,
		Msg:         buildStormSummaryMessage(routeID, state, sampleTitle, now),
		// round 在本次摘要发送意图产生的时刻（本次 storm_summary 定时器触发）一次性
		// 捕获，随 payload 持久化——下一次摘要（下一个熔断周期）now 已推进，round
		// 随之不同，notify.Sender 才不会把两次不同周期的摘要误判为同一次的重投。
		Round: now.UnixNano(),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("app: 序列化风暴摘要发送请求失败: %w", err)
	}
	if err := h.sched.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(req), now, payload); err != nil {
		return fmt.Errorf("app: 注册风暴摘要 notify_send 定时器失败: %w", err)
	}
	return nil
}

// rescheduleGroups 对 active 里出现过的每个非空 distinct group_key 各补一轮
// group_wait 定时器（design.md C3：熔断解除后，被吞掉的组通知必须最终送达）。
// fireAt 统一取 route.GroupInterval 而非 GroupWait——这些组此前多半已经触发过至少
// 一轮（否则也不会被风暴判定拦截），语义上更接近"节流窗口后的下一次通知"而非
// 组首等待；即使某个组恰好从未触发过，ScheduleIfAbsent 的幂等语义也保证这里不会
// 打断它已有的 pending 定时器，仅补建缺失的。
func (h *stormHandler) rescheduleGroups(ctx context.Context, tx pgx.Tx, route store.Route, active []domain.Alert, now time.Time) error {
	fireAt := now.Add(time.Duration(route.GroupIntervalSeconds) * time.Second)
	seen := make(map[string]bool, len(active))
	for _, a := range active {
		if a.GroupKey == "" || seen[a.GroupKey] {
			continue
		}
		seen[a.GroupKey] = true
		if _, err := h.sched.ScheduleIfAbsent(ctx, tx, groupTimerKind, a.GroupKey, fireAt, nil); err != nil {
			return fmt.Errorf("app: 补组到期定时器失败(group_key=%s): %w", a.GroupKey, err)
		}
	}
	return nil
}

// stormGroupKey 拼出风暴摘要通知的 GroupKey，"storm:" 前缀与 opsGroupKey 的 "ops:"
// 前缀同一手法：既复用 notifySendRefID 的 resourceKey 回退规则（AlertID 为空取
// GroupKey），又保证摘要意图不会与该路由下任何真实组通知的 group_key 撞出同一个
// notify_send refID/幂等键。
func stormGroupKey(routeID string) string {
	return "storm:" + routeID
}

// buildStormSummaryMessage 组装摘要通知正文：累计抑制条数、抑制窗口时间范围（上次
// 摘要时刻到本次；从未发过摘要则取熔断进入时刻）、代表样本标题（active 为空——比如
// 熔断期间告警已全部 resolve——时留空，摘要仍然发出，只是没有样本可展示）。
// DetailURL 留空：全仓目前没有告警列表页 URL 配置（同 ops.Handlers.detailURL 的
// 现状，见 internal/app/app.go Build 注释），待后续引入该配置后再统一补上。
func buildStormSummaryMessage(routeID string, state store.StormState, sampleTitle string, now time.Time) notify.Message {
	windowStart := state.ActivatedAt
	if state.LastSummaryAt != nil {
		windowStart = *state.LastSummaryAt
	}
	body := fmt.Sprintf("路由 %s 熔断期间（%s ~ %s）累计抑制 %d 条告警",
		routeID, windowStart.Format(time.RFC3339), now.Format(time.RFC3339), state.SuppressedCount)
	if sampleTitle != "" {
		body += "；代表样本：" + sampleTitle
	}
	// Message.GroupKey 故意留空：飞书卡片渲染（channel_lark.go）对 GroupKey 非空的
	// 消息会额外拼一行 "聚合组：{key}（共 len(AlertIDs) 条告警）"，风暴摘要不是真实
	// 的组通知（没有单一 group_key，AlertIDs 也未填充），累计条数已经在 Body 里，
	// 留空避免渲染出误导性的"共 0 条告警"。
	return notify.Message{
		Title:      fmt.Sprintf("风暴摘要：%d 条告警被抑制", state.SuppressedCount),
		Body:       body,
		OccurredAt: now,
	}
}
