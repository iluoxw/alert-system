package oncall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// TimerKindPersonnelEscalation 是人员升级链在 scheduler 里的 timer kind，refID 是
// alert.ID（R5.4）。与渠道升级（R13，任务 22：同一人换渠道）使用不同的 kind，
// 二者都借助 scheduler 的统一定时器但彼此隔离——本包完全不感知渠道升级的 kind
// 是什么，反之亦然；这就是两条升级链"正交"的全部含义。
const TimerKindPersonnelEscalation = "personnel_escalation"

// escalationWarningNobodyAvailable/escalationDetailLevel 是落时间线时 Detail 里的
// 固定键，供人工排障与（未来）前端展示识别"这一级没能通知到人"。
const (
	escalationDetailLevel = "level"
	escalationDetailWarn  = "warning"
	escalationDetailTeams = "target_teams"
	escalationDetailUsers = "resolved_user_ids"
	warnNobodyOnCall      = "nobody_on_call"
	warnChainExhausted    = "chain_exhausted_or_unconfigured"
	escalationSystemActor = "oncall-escalation"
)

// escalationPayload 是升级链定时器 payload 的 JSON 结构：只携带"当前推进到第几级"
// （从 0 开始，对应 EscalationPolicy.Levels 下标），其余状态（告警当前状态、团队、
// 升级策略）都在 handler 触发时重新查询，避免 payload 里的快照与数据库当前状态
// 不一致（例如升级策略在链路进行中被管理端修改）。
type escalationPayload struct {
	Level int `json:"level"`
}

// Notifier 是升级链"通知意图"与"实际发送"之间的解耦接口——本包与任务 22/24 的契约。
//
// 硬约束（对齐 scheduler.Handler 的文档：handler 在 pollOnce 的数据库事务内执行，
// 期间持有 timers 行锁；见任务 11 的落账警告）：NotifyEscalation 的实现绝不能
// 同步做外部 IO（HTTP/短信/电话等）或调用 notify.Sender.Send（那是同步阻塞 + 指数
// 退避重试，单次可能耗时数秒）——这些都会拖长 Chain.Handle 内部事务与行锁的持有
// 时间，阻塞同一批次里排在后面的其他定时器。实现应当只做快速的本地状态写入（如
// 写一条"待发送"的 outbox 记录、或把 targets 塞进一个内存/队列型的分发器），真正的
// 发送由该实现自己在事务之外异步完成。真实实现由任务 22（升级策略编排）/24（发送器
// 接入）提供；本包测试用 fake 实现验证调用参数。
type Notifier interface {
	// NotifyEscalation 记录"升级链推进到 level 级，应通知 targets"这一意图。
	// level 从 0 开始，与 EscalationPolicy.Levels 下标一致；tx 是 Chain.Handle 内部
	// 事务，实现若需要落库应复用这个 tx，与本次升级的时间线记录/下一级定时器注册
	// 原子提交。
	NotifyEscalation(ctx context.Context, tx pgx.Tx, alert domain.Alert, level int, targets []OnCallEntry) error
}

// AlertRepo 是 Chain 依赖的告警仓储子集，由 store.AlertRepo 实现。
type AlertRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error)
}

// TimelineRepo 是 Chain 依赖的时间线仓储子集，由 store.TimelineRepo 实现。
// ListByAlert 除了供外部展示，还被 Chain 自身用作幂等判据（见 alreadyHandledLevel）。
type TimelineRepo interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
	ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error)
}

// EscalationPolicyRepo 是 Chain 依赖的升级策略仓储子集，由 store.EscalationPolicyRepo 实现。
type EscalationPolicyRepo interface {
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.EscalationPolicy, error)
}

// TimerScheduler 是 Chain 依赖的定时器子集，由 scheduler.Scheduler 实现。
type TimerScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
}

// Clock 抽象"当前时间"，与 scheduler.Clock/dedup 同一模式，供测试注入 fake clock。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Chain 是人员升级链的编排层（R5.4）：经 scheduler 逐级推进，每级用 Resolver 求出
// 当班人，通知意图记账后交给注入的 Notifier；未 Ack 前逐级推进，Ack/Resolved/
// 静默后停止；无人可通知（班次空洞或链路耗尽）落时间线警告而不中断。
type Chain struct {
	alerts    AlertRepo
	timeline  TimelineRepo
	policies  EscalationPolicyRepo
	resolver  *Resolver
	scheduler TimerScheduler
	notifier  Notifier
	clock     Clock
}

// NewChain 构造 Chain。
func NewChain(alerts AlertRepo, timeline TimelineRepo, policies EscalationPolicyRepo, resolver *Resolver, sched TimerScheduler, notifier Notifier) *Chain {
	return &Chain{
		alerts:    alerts,
		timeline:  timeline,
		policies:  policies,
		resolver:  resolver,
		scheduler: sched,
		notifier:  notifier,
		clock:     realClock{},
	}
}

// Start 为 alert 启动升级链：查团队升级策略，按第 0 级的 wait_seconds 注册首个
// 定时器。alert.TeamID 未路由或团队未配置升级策略时不启动（不是错误——告警仍然
// 走常规通知策略，只是没有人员升级）。由路由完成后的调用方（如任务 22 的编排逻辑）
// 在写入 alert 的同一事务内调用。
func (c *Chain) Start(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	if alert.TeamID == "" {
		return nil
	}
	policies, err := c.policies.ListByTeam(ctx, tx, alert.TeamID, 1)
	if err != nil {
		return fmt.Errorf("oncall: 查询团队升级策略失败: %w", err)
	}
	if len(policies) == 0 || len(policies[0].Levels) == 0 {
		return nil
	}

	fireAt := c.clock.Now().Add(time.Duration(policies[0].Levels[0].WaitSeconds) * time.Second)
	payload, err := json.Marshal(escalationPayload{Level: 0})
	if err != nil {
		return fmt.Errorf("oncall: 序列化升级链 payload 失败: %w", err)
	}
	if err := c.scheduler.Schedule(ctx, tx, TimerKindPersonnelEscalation, alert.ID, fireAt, payload); err != nil {
		return fmt.Errorf("oncall: 注册升级链首个定时器失败: %w", err)
	}
	return nil
}

// Stop 主动取消 alert 待触发的升级定时器，供 Ack/Resolve/静默动作在各自的事务内
// 调用，避免等到定时器真正触发才由 Handle 自行检测停止条件——语义上与
// pipeline.DedupStage.processResolved 里 Resolve 时 Cancel 掉 TTL 定时器一致。
// 幂等：不存在待触发定时器时不报错（见 scheduler.Cancel 的文档）。
func (c *Chain) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	if err := c.scheduler.Cancel(ctx, tx, TimerKindPersonnelEscalation, alertID); err != nil {
		return fmt.Errorf("oncall: 取消升级链定时器失败: %w", err)
	}
	return nil
}

// Handle 实现 scheduler.Handler：refID 是 alert.ID，payload 是 escalationPayload 的
// JSON。tx 是 scheduler 传入的事务（同一事务内已经把这条定时器标记为 fired），
// 本级的查状态/落时间线/注册下一级定时器都复用这个 tx，与 scheduler.Handler 的
// 文档约定一致。
func (c *Chain) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	var p escalationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("oncall: 反序列化升级链 payload 失败: %w", err)
	}
	return c.advance(ctx, tx, refID, p.Level)
}

// advance 是 Handle 的核心逻辑，在 Handle 传入的事务内执行。
func (c *Chain) advance(ctx context.Context, tx pgx.Tx, alertID string, level int) error {
	alert, err := c.alerts.GetByID(ctx, tx, alertID)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			return nil // 告警已不存在（理论上不应发生）：无需重投，直接结束
		}
		return fmt.Errorf("oncall: 查询告警失败: %w", err)
	}
	if !shouldContinueEscalation(alert) {
		return nil // 已 Ack/Resolved/静默：停止，不再注册下一级定时器
	}

	entries, err := c.timeline.ListByAlert(ctx, tx, alertID)
	if err != nil {
		return fmt.Errorf("oncall: 查询时间线失败: %w", err)
	}
	if alreadyHandledLevel(entries, level) {
		return nil // 幂等：本级已经处理过（重投），不重复通知、不重复注册定时器
	}

	policies, err := c.policies.ListByTeam(ctx, tx, alert.TeamID, 1)
	if err != nil {
		return fmt.Errorf("oncall: 查询团队升级策略失败: %w", err)
	}
	if len(policies) == 0 || level >= len(policies[0].Levels) {
		return c.recordWarning(ctx, tx, alert, level, warnChainExhausted, nil)
	}
	policy := policies[0]
	lvl := policy.Levels[level]

	now := c.clock.Now()
	var targets []OnCallEntry
	for _, targetTeamID := range lvl.Targets {
		resolved, err := c.resolver.OnCallAt(ctx, tx, targetTeamID, now)
		if err != nil {
			return fmt.Errorf("oncall: 求值第 %d 级值班人失败: %w", level, err)
		}
		targets = append(targets, resolved...)
	}

	if len(targets) == 0 {
		if err := c.recordWarning(ctx, tx, alert, level, warnNobodyOnCall, lvl.Targets); err != nil {
			return err
		}
	} else {
		userIDs := make([]string, len(targets))
		for i, t := range targets {
			userIDs[i] = t.UserID
		}
		if _, err := c.timeline.Append(ctx, tx, domain.TimelineEntry{
			AlertID: alert.ID,
			Actor:   escalationSystemActor,
			Action:  domain.TimelineActionEscalate,
			At:      now,
			Detail: map[string]any{
				escalationDetailLevel: level,
				escalationDetailUsers: userIDs,
			},
		}); err != nil {
			return fmt.Errorf("oncall: 写入升级时间线失败: %w", err)
		}
		if err := c.notifier.NotifyEscalation(ctx, tx, alert, level, targets); err != nil {
			return fmt.Errorf("oncall: 记录升级通知意图失败: %w", err)
		}
	}

	next := level + 1
	if next >= len(policy.Levels) {
		return nil // 已到链路末级，正常结束
	}
	fireAt := now.Add(time.Duration(policy.Levels[next].WaitSeconds) * time.Second)
	nextPayload, err := json.Marshal(escalationPayload{Level: next})
	if err != nil {
		return fmt.Errorf("oncall: 序列化升级链 payload 失败: %w", err)
	}
	if err := c.scheduler.Schedule(ctx, tx, TimerKindPersonnelEscalation, alert.ID, fireAt, nextPayload); err != nil {
		return fmt.Errorf("oncall: 注册下一级升级定时器失败: %w", err)
	}
	return nil
}

// recordWarning 落一条"本级无人可通知"的时间线警告（班次空洞或升级链已耗尽/未配置），
// 不返回 error——按团队-lead 的约束，这种情况不能中断 handler、更不能让 scheduler
// 把定时器标记为 failed。
func (c *Chain) recordWarning(ctx context.Context, tx pgx.Tx, alert domain.Alert, level int, reason string, targetTeams []string) error {
	detail := map[string]any{
		escalationDetailLevel: level,
		escalationDetailWarn:  reason,
	}
	if len(targetTeams) > 0 {
		detail[escalationDetailTeams] = targetTeams
	}
	if _, err := c.timeline.Append(ctx, tx, domain.TimelineEntry{
		AlertID: alert.ID,
		Actor:   escalationSystemActor,
		Action:  domain.TimelineActionEscalate,
		At:      c.clock.Now(),
		Detail:  detail,
	}); err != nil {
		return fmt.Errorf("oncall: 写入无人可通知警告失败: %w", err)
	}
	return nil
}

// shouldContinueEscalation 报告 alert 是否仍处于需要人员升级的状态：firing 且未被
// 任何静默规则命中。Acknowledged/Resolved/静默命中三者任一即停止（R5.4 的停止条件）。
func shouldContinueEscalation(alert domain.Alert) bool {
	return alert.Status == domain.StatusFiring && len(alert.SilencedBy) == 0
}

// alreadyHandledLevel 检查时间线里是否已经存在"本级已处理"的记账（无论结果是成功
// 通知还是无人可通知的警告），作为 Handle 的幂等判据——与 notify.Sender 用
// notification_log.idempotency_key 判重是同一思路：用已经落库的记录本身当幂等键，
// 而不是额外引入一张状态表。JSON 往返后数字统一是 float64，这里做一次兼容处理。
func alreadyHandledLevel(entries []domain.TimelineEntry, level int) bool {
	for _, e := range entries {
		if e.Action != domain.TimelineActionEscalate {
			continue
		}
		switch v := e.Detail[escalationDetailLevel].(type) {
		case int:
			if v == level {
				return true
			}
		case float64:
			if int(v) == level {
				return true
			}
		}
	}
	return false
}
