package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// TimerKindAlertTTL 是告警 TTL 自动恢复定时器在 scheduler 里的 kind，refID 是
// alert.ID（R4.4）。字面量必须与 internal/pipeline/stage_dedup.go 里未导出的
// timerKindTTL 常量保持一致（两包各自独立定义、互不 import，但共享同一个 kind
// 字符串空间）：DedupStage 负责注册/续期这个定时器，HandleTTL 是它到期后的处理函数。
const TimerKindAlertTTL = "alert_ttl"

// TimerKindAlertRepeat 是 repeat_interval 重复提醒定时器在 scheduler 里的 kind，
// refID 是 alert.ID（R6.5）。与已占用的 "alert_ttl"（TTL 自动恢复，本文件）、
// "personnel_escalation"（oncall 包，人员升级链）、"group_wait"（pipeline 包，
// 分组）分属不同命名空间；任务 22 的渠道升级将使用 "channel_escalation"。
const TimerKindAlertRepeat = "alert_repeat"

// lifecycleActorTTL 是 TTL 自动恢复动作的 actor 标识，取 domain.Resolve 文档注释
// 里给系统自动触发场景建议的示例值，与 pipeline.resolveActorSource
// （"source-event"，R4.3 上游 resolved 事件路径）、oncall 包的 escalationSystemActor
// 是同一约定：系统触发的动作要给出一个可读的系统身份，而不是留空。
const lifecycleActorTTL = "ttl-scheduler"

// lifecycleAlertRepo 是 LifecycleHandlers 依赖的告警仓储子集，由 store.AlertRepo 实现。
type lifecycleAlertRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
}

// lifecycleTimelineRepo 是 LifecycleHandlers 依赖的时间线仓储子集，由 store.TimelineRepo 实现。
type lifecycleTimelineRepo interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
}

// lifecycleScheduler 是 LifecycleHandlers 依赖的定时器子集，由 scheduler.Scheduler 实现。
type lifecycleScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
}

// lifecycleEscalationStopper 抽象 oncall.Chain.Stop：告警进入终态时同步停止其人员
// 升级链定时器。由 *oncall.Chain 结构性满足；不直接 import oncall 包，保持 notify
// 与 oncall 之间只有这一条最小的、鸭子类型的依赖边。
type lifecycleEscalationStopper interface {
	Stop(ctx context.Context, tx pgx.Tx, alertID string) error
}

// RepeatNotifier 是 repeat_interval 到期"应重发提醒"这一意图与实际发送之间的解耦
// 接口——与 oncall.Notifier、pipeline.GroupNotifier 同一模式，本包与装配阶段的契约。
//
// 硬约束（对齐 scheduler.Handler 的文档：handler 在 pollOnce 的数据库事务内执行，
// 期间持有 timers 行锁）：NotifyRepeat 的实现绝不能同步做外部 IO（HTTP/短信等）或
// 调用 notify.Sender.Send（同步阻塞 + 指数退避重试，单次可能耗时数秒），否则会拖长
// HandleRepeat 内部事务与行锁的持有时间，阻塞同一批次里排在后面的其他定时器。
// 实现应当只做快速的本地状态写入，真正的重复发送由该实现自己在事务之外异步完成。
// 真实实现留给装配阶段（与任务 22 的发送编排统一）；本包测试用 fake 实现验证调用参数。
type RepeatNotifier interface {
	NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
}

// handlerClock 抽象"当前时间"，与 scheduler.Clock/dedupClock/oncall.Clock 同一模式，
// 供测试注入 fake clock 精确控制 TTL/repeat 的时间判断。
type handlerClock interface {
	Now() time.Time
}

type realHandlerClock struct{}

func (realHandlerClock) Now() time.Time { return time.Now() }

// LifecycleHandlers 把生命周期定时器到期后的动作与通知/升级链的收尾粘合在一起
// （R4.3/R4.4/R6.5）：
//   - HandleTTL 注册给 TimerKindAlertTTL：告警仍未 resolved 时自动 Resolve（reason=ttl）
//     并清理其余生命周期定时器；已 resolved 视为幂等重投，直接跳过；
//   - HandleRepeat 注册给 TimerKindAlertRepeat：告警非 firing（已 Ack/Resolved）或
//     处于静默中则不发不续约；否则记录重发意图并续约下一轮。
//
// 组状态/告警状态的唯一真相仍是 alerts 表 + timers 表，本类型不维护任何内存态。
type LifecycleHandlers struct {
	alerts         lifecycleAlertRepo
	timeline       lifecycleTimelineRepo
	scheduler      lifecycleScheduler
	escalation     lifecycleEscalationStopper
	repeatNotifier RepeatNotifier
	repeatInterval time.Duration
	clock          handlerClock
}

// NewLifecycleHandlers 构造 LifecycleHandlers。repeatInterval 是 R6.5 提醒的重复
// 周期：MVP 阶段尚未接入按路由配置化的 repeat_interval，先由调用方注入一个固定
// 默认值，配置化留给任务 22/27（policy 编排层）扩展。escalation 可为 nil——此时
// TTL 自动恢复时跳过"停止人员升级链"这一步（例如尚未接入 oncall 的部署）。
func NewLifecycleHandlers(alerts lifecycleAlertRepo, timeline lifecycleTimelineRepo, sched lifecycleScheduler, escalation lifecycleEscalationStopper, repeatNotifier RepeatNotifier, repeatInterval time.Duration) *LifecycleHandlers {
	return &LifecycleHandlers{
		alerts:         alerts,
		timeline:       timeline,
		scheduler:      sched,
		escalation:     escalation,
		repeatNotifier: repeatNotifier,
		repeatInterval: repeatInterval,
		clock:          realHandlerClock{},
	}
}

// HandleTTL 实现 scheduler.Handler，注册给 TimerKindAlertTTL。refID 是 alert.ID。
// tx 是 scheduler 传入的事务（同一事务内已经把这条定时器标记为 fired），本次
// Resolve/落时间线/清理其余定时器都复用这个 tx。幂等：告警不存在（理论上不应
// 发生）或已经 resolved（重投，或已被其他路径 resolve）时直接返回成功，不重复处理。
func (h *LifecycleHandlers) HandleTTL(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	alert, err := h.alerts.GetByID(ctx, tx, refID)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			return nil
		}
		return fmt.Errorf("notify: 查询告警失败: %w", err)
	}
	if alert.Status == domain.StatusResolved {
		return nil
	}

	now := h.clock.Now()
	resolved, entry, err := domain.Resolve(alert, lifecycleActorTTL, domain.ResolveReasonTTL, now)
	if err != nil {
		return fmt.Errorf("notify: TTL 自动恢复状态转移失败: %w", err)
	}
	if err := h.alerts.UpdateStatus(ctx, tx, resolved); err != nil {
		return fmt.Errorf("notify: 更新告警状态失败: %w", err)
	}
	if _, err := h.timeline.Append(ctx, tx, entry); err != nil {
		return fmt.Errorf("notify: 写入时间线失败: %w", err)
	}
	return h.CancelLifecycleTimers(ctx, tx, resolved.ID)
}

// HandleRepeat 实现 scheduler.Handler，注册给 TimerKindAlertRepeat。refID 是
// alert.ID。tx 是 scheduler 传入的事务，本次记发送意图/续约定时器都复用这个 tx。
// R6.5：告警非 firing（已 Ack/Resolved）或处于静默中
// （len(alert.SilencedBy) > 0）时不发不续约——静默只是暂停当前这一轮 repeat 链，
// 不清空任何字段；解除静默后由管道下一次 Process 重新走一遍分组/静默判断，若
// 该告警仍 firing，路由/通知编排会重新驱动通知，无需本 handler 关心"恢复"。
// firing 且未静默：记录重发意图，同一事务内注册下一轮定时器（续约）。
//
// 首个 repeat 定时器的注册不归本 handler：由任务 22 的首次通知编排在发出首次
// 通知时负责注册，本 handler 只处理"已经到期一次之后"的后续续约。
func (h *LifecycleHandlers) HandleRepeat(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	alert, err := h.alerts.GetByID(ctx, tx, refID)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			return nil
		}
		return fmt.Errorf("notify: 查询告警失败: %w", err)
	}
	if alert.Status != domain.StatusFiring || len(alert.SilencedBy) > 0 {
		return nil
	}
	if err := h.repeatNotifier.NotifyRepeat(ctx, tx, alert); err != nil {
		return fmt.Errorf("notify: 记录 repeat 通知意图失败: %w", err)
	}
	fireAt := h.clock.Now().Add(h.repeatInterval)
	if err := h.scheduler.Schedule(ctx, tx, TimerKindAlertRepeat, alert.ID, fireAt, nil); err != nil {
		return fmt.Errorf("notify: 续约 repeat 定时器失败: %w", err)
	}
	return nil
}

// CancelLifecycleTimers 取消 alertID 名下全部"生命周期收尾"类定时器：
// TimerKindAlertRepeat（repeat_interval 重复提醒）、TimerKindChannelEscalation
// （渠道升级引擎，任务 22 的 policy.go）与人员升级链（若注入了 escalation）。任何把
// 告警推进到终态（resolved）的路径都应该调用它，避免告警已经恢复后残留的定时器
// 继续触发"重发提醒"或"升级"——目前只有 pipeline.DedupStage.processResolved
// （R4.3，上游 resolved 事件路径）与本文件的 HandleTTL（R4.4）会把告警推进到终态；
// 前者写下时这三个 kind 都还不存在，因此这里单独提供一个可复用的清理点，供装配层
// 与后续任务（如手动 Resolve 路径）统一调用，而不是让每条 resolve 路径各自重复
// 一遍 Cancel 序列。scheduler.Cancel 本身是幂等的（定时器不存在时不报错），本方法
// 同样幂等。
func (h *LifecycleHandlers) CancelLifecycleTimers(ctx context.Context, tx pgx.Tx, alertID string) error {
	if err := h.scheduler.Cancel(ctx, tx, TimerKindAlertRepeat, alertID); err != nil {
		return fmt.Errorf("notify: 取消 repeat 定时器失败: %w", err)
	}
	if err := h.scheduler.Cancel(ctx, tx, TimerKindChannelEscalation, alertID); err != nil {
		return fmt.Errorf("notify: 取消渠道升级定时器失败: %w", err)
	}
	if h.escalation != nil {
		if err := h.escalation.Stop(ctx, tx, alertID); err != nil {
			return fmt.Errorf("notify: 停止人员升级链失败: %w", err)
		}
	}
	return nil
}
