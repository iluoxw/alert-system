package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/hook"
	"alert-system/internal/store"
)

// Rule 是触发一次自愈动作检查所需的规则要素（对应 action_rules 表的关键字段，
// R12.1/R12.4）。action_rules 表本期只建表、不提供仓储与规则匹配逻辑（label/
// severity/资源匹配，见 migrations/000003_aux.up.sql 的注释），本任务范围只到
// 「给定已经匹配好的规则，做冷却判定与落账」，Rule 由调用方（未来的规则匹配器）解析后传入。
type Rule struct {
	ID string
	// CooldownSeconds 是冷却窗口时长（R12.4）；<= 0 表示不做冷却限制（每次都允许触发），
	// 与 action_rules.cooldown_seconds 列的默认值 0 语义一致。
	CooldownSeconds int
}

// Trigger 是自愈动作触发器的扩展点接口（R12.1）：对一条告警按 rule 做一次触发判定——
// 若同 fingerprint 在冷却窗口内已经触发过，拒绝本次触发并升级人工（R12.4）；否则落一条
// action_runs 记录并执行（本期为 no-op 占位）。
type Trigger interface {
	Trigger(ctx context.Context, rule Rule, alert domain.Alert) error
}

// runStore 抽象 store.ActionRunRepo 用到的两个方法，让 DefaultTrigger 的编排逻辑可以在
// 单测里注入内存 fake，不需要真实 Postgres。*store.ActionRunRepo 天然满足这个接口。
type runStore interface {
	Insert(ctx context.Context, tx pgx.Tx, a store.ActionRun) (*store.ActionRun, error)
	LastTriggered(ctx context.Context, tx pgx.Tx, ruleID, fingerprint string) (store.ActionRun, bool, error)
}

// timelineAppender 抽象 store.TimelineRepo.Append，与 notify/sender.go 同一模式。
type timelineAppender interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
}

// txRunner 抽象 store.Store.WithTx。hook.Func 本身不带 tx 参数（管道触发 hook 时所在的
// 事务不能被异步旁路逻辑并发复用，见 internal/hook/registry.go 的 goroutine 隔离设计），
// 所以 Trigger 需要自己开一个独立事务来完成冷却检查与落账。
type txRunner interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// clock 抽象「当前时间」，供测试注入 fake clock 精确控制冷却窗口边界，无需依赖真实
// wall-clock（与 pipeline 包 dedupClock/routeClock 同一模式）。
type clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// escalateActor 是冷却拒绝时落时间线记录的 actor 标识（R12.4「升级人工」，本期口径：
// 告警时间线 Append 一条 WARN 级别的 escalate 记录）。
const escalateActor = "action-trigger"

// DefaultTrigger 是 Trigger 接口的默认实现。
type DefaultTrigger struct {
	tx       txRunner
	runs     runStore
	timeline timelineAppender
	clock    clock
}

// NewDefaultTrigger 构造 DefaultTrigger。tx/runs/timeline 通常直接传 *store.Store、
// *store.ActionRunRepo、*store.TimelineRepo（三者结构上满足对应的小接口）。
func NewDefaultTrigger(tx txRunner, runs runStore, timeline timelineAppender) *DefaultTrigger {
	return &DefaultTrigger{tx: tx, runs: runs, timeline: timeline, clock: realClock{}}
}

// Trigger 实现 Trigger 接口：整个判定+落账在一个独立事务内完成，保证「查冷却窗口」
// 与「写落账记录」不会在并发调用下产生竞态窗口内多次触发的错判。
func (t *DefaultTrigger) Trigger(ctx context.Context, rule Rule, alert domain.Alert) error {
	return t.tx.WithTx(ctx, func(tx pgx.Tx) error {
		now := t.clock.Now()
		if rule.CooldownSeconds > 0 {
			last, ok, err := t.runs.LastTriggered(ctx, tx, rule.ID, alert.Fingerprint)
			if err != nil {
				return fmt.Errorf("action: 查询冷却窗口失败: %w", err)
			}
			if ok && now.Sub(last.TriggeredAt) < time.Duration(rule.CooldownSeconds)*time.Second {
				return t.skipCooldown(ctx, tx, rule, alert, now)
			}
		}
		return t.execute(ctx, tx, rule, alert)
	})
}

// skipCooldown 落一条 skipped_cooldown 记录，并在告警时间线追加一条升级人工的
// escalate 记录（R12.4）。
func (t *DefaultTrigger) skipCooldown(ctx context.Context, tx pgx.Tx, rule Rule, alert domain.Alert, now time.Time) error {
	if _, err := t.runs.Insert(ctx, tx, store.ActionRun{
		RuleID:      rule.ID,
		AlertID:     alert.ID,
		Fingerprint: alert.Fingerprint,
		Status:      store.ActionRunStatusSkippedCooldown,
	}); err != nil {
		return fmt.Errorf("action: 落账冷却跳过记录失败: %w", err)
	}
	if _, err := t.timeline.Append(ctx, tx, domain.TimelineEntry{
		AlertID: alert.ID,
		Actor:   escalateActor,
		Action:  domain.TimelineActionEscalate,
		At:      now,
		Detail: map[string]any{
			"rule_id":          rule.ID,
			"cooldown_seconds": rule.CooldownSeconds,
			"reason":           "cooldown_window_active",
		},
	}); err != nil {
		return fmt.Errorf("action: 写入冷却升级时间线失败: %w", err)
	}
	return nil
}

// execute 落一条 triggered 记录。本期执行器为 no-op webhook 占位：真正对接外部编排
// 引擎是二期工作（R12.1）；外部编排引擎不可用时的降级（R12.6）本期直接体现为「不
// 做任何真实外呼」，核心告警链路完全不受影响。
func (t *DefaultTrigger) execute(ctx context.Context, tx pgx.Tx, rule Rule, alert domain.Alert) error {
	if _, err := t.runs.Insert(ctx, tx, store.ActionRun{
		RuleID:      rule.ID,
		AlertID:     alert.ID,
		Fingerprint: alert.Fingerprint,
		Status:      store.ActionRunStatusTriggered,
	}); err != nil {
		return fmt.Errorf("action: 落账触发记录失败: %w", err)
	}
	return nil
}

// RegisterHook 把 trigger 挂接到 hook.Registry 的 (stage, phase) 扩展点上（R12.1/R12.6）。
//
// 挂载点选择："dedup" stage 的 After 阶段。理由：
//  1. pctx.Alert 只有在 dedup stage upsert 之后才第一次被填充（ID/Fingerprint/
//     Severity/Labels/ResourceRef 均已落库）——在 dedup 之前的任何挂钩点拿到的都是
//     零值 Alert，规则匹配与冷却判定都无从谈起（见 internal/pipeline/pipeline.go
//     process() 里 pctx.Alert 的赋值时机）。
//  2. 自愈本质上是独立于通知链路的旁路系统（R12 与 R11 同属 D6 可插拔扩展点），不应该
//     等分组/静默/抑制/路由都跑完才触发——挂在管道里最早能拿到有效 Alert 的点，既能让
//     「磁盘满/进程宕」这类场景尽快自愈，也不受下游是否静默/抑制该告警通知的影响
//     （自愈与是否发出通知是两个正交的决定）。
//
// rules 为空时不注册任何 Func（等价于未启用自愈）。已 resolved 的告警不触发自愈——
// 没有正在发生的问题需要处理。多个 rule 之间某一个触发失败不影响其余 rule 的判定，
// 所有错误用 errors.Join 汇总后交给 hook.Registry.Fire 统一记一条 WARN（R11.6/R12.6：
// 旁路失败不传播、不阻塞主管道）。
func RegisterHook(registry *hook.Registry, trigger Trigger, rules ...Rule) {
	if len(rules) == 0 {
		return
	}
	registry.Register("dedup", hook.After, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		if alert.Status != domain.StatusFiring {
			return nil
		}
		var errs []error
		for _, rule := range rules {
			if err := trigger.Trigger(ctx, rule, alert); err != nil {
				errs = append(errs, fmt.Errorf("rule %s: %w", rule.ID, err))
			}
		}
		return errors.Join(errs...)
	})
}
