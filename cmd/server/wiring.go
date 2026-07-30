// Command server 的装配细节（任务 31，架构裁决见团队评审纪要，不在此重新设计）：
// 把四个结构性同形的"通知意图"接口（oncall.Notifier、pipeline.GroupNotifier、
// notify.RepeatNotifier、notify.Dispatcher）与"实际发送"（notify.Sender 的外部 IO
// + 指数退避重试）解耦成一个 outbox——四者的实现都只做一件事：在调用方传入的事务内
// 注册一个 "notify_send" 定时器，携带 JSON 化的 notify.SendRequest；真正的发送延后
// 到该定时器触发时，由 notify_send 的 handler 自己开一个独立事务解码 payload 并调
// notify.Sender.Send。这样四个通知意图接口的实现都不会在 handler 持锁期间做同步外部
// IO（各自接口文档的硬约束：handler 在 pollOnce 的事务内执行，期间持有 timers 行锁），
// 发送慢只阻塞 "notify_send" 这一个 kind 的 worker 槽位，MVP 可接受。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/scheduler"
)

// notifySendTimerKind 是 outbox 定时器在 scheduler 里的 kind：六个注册 kind 里唯一
// 由本文件（而不是各业务包）定义的一个，因为它是纯装配层概念，不属于任何业务包的
// 领域逻辑。
const notifySendTimerKind = "notify_send"

// defaultNotifyChannel 是"组通知""重复提醒""人员升级"三类通知意图使用的固定渠道：
// R6.2 约定飞书是主力渠道。只有渠道升级（notify.Dispatcher）按通知策略配置的
// channels 逐级切换，不用这个默认值。
//
// 已知集成缝隙（已回报 team-lead，不在本任务范围内解决）：四个通知意图接口都不携带
// "具体联系方式"（如飞书 open_id、邮箱地址）——users 表只有 email 字段，team 没有
// 渠道联系方式表。装配层的临时口径：人员升级以 OnCallEntry.UserID 作 target；
// 组通知/重复提醒/渠道升级以 alert.TeamID 作 target。真实接入飞书/邮件发送时需要
// 补一张团队/用户的渠道联系方式表；E2E 用 mock 渠道断言时以这个临时口径为准。
const defaultNotifyChannel = "lark"

// notifyOutboxScheduler 是 notifyOutbox 依赖的定时器子集，由 *scheduler.Scheduler 实现。
type notifyOutboxScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error)
}

// notifyOutboxChainStarter 抽象 oncall.Chain.Start，供 NotifyGroup 的"首次通知编排"
// 职责启动组内每条告警的人员升级链。
type notifyOutboxChainStarter interface {
	Start(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
}

// notifyOutboxEngineStarter 抽象 notify.Engine.Start，供 NotifyGroup 的"首次通知编排"
// 职责按组首时间启动组内每条告警的渠道升级序列。
type notifyOutboxEngineStarter interface {
	Start(ctx context.Context, tx pgx.Tx, alert domain.Alert, baseTime time.Time) error
}

// notifyOutbox 同时实现四个"通知意图"接口。每个方法只做一件事：把意图翻译成一个
// 或多个 scheduleSend 调用；NotifyGroup 额外承担"首次通知编排"职责（组首时间基准
// 的渠道升级 + 首个 repeat 定时器 + 人员升级链启动），见该方法注释。
type notifyOutbox struct {
	scheduler      notifyOutboxScheduler
	engine         notifyOutboxEngineStarter
	chain          notifyOutboxChainStarter
	repeatInterval time.Duration
	now            func() time.Time
}

func newNotifyOutbox(sched notifyOutboxScheduler, engine notifyOutboxEngineStarter, chain notifyOutboxChainStarter, repeatInterval time.Duration) *notifyOutbox {
	return &notifyOutbox{
		scheduler:      sched,
		engine:         engine,
		chain:          chain,
		repeatInterval: repeatInterval,
		now:            time.Now,
	}
}

// scheduleSend 把一个 SendRequest 序列化后注册进 "notify_send" 定时器，fireAt=now，
// 即"立即可发送"——outbox 的 worker 槽位（scheduler.Run 的下一轮轮询）会拾取，不需要
// 真的延后。refID 是唯一意图键，口径对齐 notify.Sender 的幂等键：同一发送目标
// （同一告警/分组 + 同一级 + 同一渠道 + 同一 target）只会有一个 in-flight 的 outbox
// 定时器，重复调用会替换 fire_at/payload（Schedule 本身是替换语义），不会产生重复发送。
func (o *notifyOutbox) scheduleSend(ctx context.Context, tx pgx.Tx, req notify.SendRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("wiring: 序列化通知发送请求失败: %w", err)
	}
	if err := o.scheduler.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(req), o.now(), payload); err != nil {
		return fmt.Errorf("wiring: 注册 notify_send 定时器失败: %w", err)
	}
	return nil
}

// notifySendRefID 与 notify.idempotencyKey（sender.go，未导出）使用完全相同的拼接
// 口径：resourceKey（AlertID 或 GroupKey）/ step（无则用 "-"）/ channel / target，
// 用 "\x1e" 连接。
func notifySendRefID(req notify.SendRequest) string {
	resourceKey := req.AlertID
	if resourceKey == "" {
		resourceKey = req.GroupKey
	}
	step := "-"
	if req.Step != nil {
		step = strconv.Itoa(*req.Step)
	}
	return strings.Join([]string{resourceKey, step, req.ChannelKind, req.Target}, "\x1e")
}

// NotifyEscalation 实现 oncall.Notifier：人员升级链推进到 level 级，给每个目标责任人
// 各自注册一个发送意图（同一渠道、不同 target），target 用 OnCallEntry.UserID。
func (o *notifyOutbox) NotifyEscalation(ctx context.Context, tx pgx.Tx, alert domain.Alert, level int, targets []oncall.OnCallEntry) error {
	msg := buildAlertMessage(alert)
	for _, t := range targets {
		req := notify.SendRequest{
			AlertID:     alert.ID,
			Step:        &level,
			ChannelKind: defaultNotifyChannel,
			Target:      t.UserID,
			Msg:         msg,
		}
		if err := o.scheduleSend(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// NotifyGroup 实现 pipeline.GroupNotifier：组 flush 时（1）发一条固定走
// defaultNotifyChannel 的"组消息"发送意图，target 用组内告警共同的 TeamID（路由阶段
// 保证组内告警 TeamID 一致，取第一条即可）；（2）承担"首次通知编排"职责——对组内
// 每条告警：以组首时间（组内最早 StartsAt）为基准启动渠道升级序列
// （notify.Engine.Start 的 baseTime 参数正是为这个用途设计的）、用 ScheduleIfAbsent
// 注册首个 alert_repeat 定时器（避免组内后续到达的告警重置已经在跑的 repeat 节奏）、
// 启动人员升级链（oncall.Chain.Start）。
func (o *notifyOutbox) NotifyGroup(ctx context.Context, tx pgx.Tx, groupKey string, alerts []domain.Alert) error {
	if len(alerts) == 0 {
		return nil
	}

	req := notify.SendRequest{
		GroupKey:    groupKey,
		ChannelKind: defaultNotifyChannel,
		Target:      alerts[0].TeamID,
		Msg:         buildGroupMessage(groupKey, alerts),
	}
	if err := o.scheduleSend(ctx, tx, req); err != nil {
		return err
	}

	baseTime := earliestStartsAt(alerts)
	for _, alert := range alerts {
		if err := o.engine.Start(ctx, tx, alert, baseTime); err != nil {
			return fmt.Errorf("wiring: 启动渠道升级序列失败: %w", err)
		}
		fireAt := o.now().Add(o.repeatInterval)
		if _, err := o.scheduler.ScheduleIfAbsent(ctx, tx, notify.TimerKindAlertRepeat, alert.ID, fireAt, nil); err != nil {
			return fmt.Errorf("wiring: 注册首个 repeat 定时器失败: %w", err)
		}
		if err := o.chain.Start(ctx, tx, alert); err != nil {
			return fmt.Errorf("wiring: 启动人员升级链失败: %w", err)
		}
	}
	return nil
}

// NotifyRepeat 实现 notify.RepeatNotifier：repeat_interval 到期的重复提醒，固定走
// defaultNotifyChannel，target 用 alert.TeamID。
func (o *notifyOutbox) NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	req := notify.SendRequest{
		AlertID:     alert.ID,
		ChannelKind: defaultNotifyChannel,
		Target:      alert.TeamID,
		Msg:         buildAlertMessage(alert),
	}
	return o.scheduleSend(ctx, tx, req)
}

// DispatchStep 实现 notify.Dispatcher：渠道升级推进到 step 级，对 channels 里的每个
// 渠道各自注册一个发送意图，target 用 alert.TeamID（渠道升级是团队维度的触达强度
// 提升，不针对具体个人——针对个人的升级是 oncall 包的人员升级链，二者正交）。
func (o *notifyOutbox) DispatchStep(ctx context.Context, tx pgx.Tx, alert domain.Alert, step int, channels []string) error {
	msg := buildAlertMessage(alert)
	for _, ch := range channels {
		req := notify.SendRequest{
			AlertID:     alert.ID,
			Step:        &step,
			ChannelKind: ch,
			Target:      alert.TeamID,
			Msg:         msg,
		}
		if err := o.scheduleSend(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// buildAlertMessage 把单个告警组装成渠道无关的 Message（Sender/Channel 的载荷）。
func buildAlertMessage(alert domain.Alert) notify.Message {
	return notify.Message{
		AlertIDs:   []string{alert.ID},
		Severity:   string(alert.Severity),
		Title:      alert.Title,
		Body:       alert.Title,
		Labels:     alert.Labels,
		OccurredAt: alert.StartsAt,
	}
}

// buildGroupMessage 把一组告警组装成分组摘要 Message，OccurredAt 取组首时间。
func buildGroupMessage(groupKey string, alerts []domain.Alert) notify.Message {
	return notify.Message{
		AlertIDs:   alertIDs(alerts),
		GroupKey:   groupKey,
		Severity:   string(alerts[0].Severity),
		Title:      fmt.Sprintf("%d 条告警（分组 %s）", len(alerts), groupKey),
		Body:       alerts[0].Title,
		OccurredAt: earliestStartsAt(alerts),
	}
}

func alertIDs(alerts []domain.Alert) []string {
	ids := make([]string, len(alerts))
	for i, a := range alerts {
		ids[i] = a.ID
	}
	return ids
}

// earliestStartsAt 返回一组告警里最早的 StartsAt，即"组首时间"，供 NotifyGroup 传给
// notify.Engine.Start 的 baseTime 参数（渠道升级基准取组维度，而不是 flush 发生的
// 当前时间）。
func earliestStartsAt(alerts []domain.Alert) time.Time {
	earliest := alerts[0].StartsAt
	for _, a := range alerts[1:] {
		if a.StartsAt.Before(earliest) {
			earliest = a.StartsAt
		}
	}
	return earliest
}

// notifySendPayloadHandler 构造 "notify_send" 的 scheduler.Handler：解码 payload 为
// notify.SendRequest，自开事务调 notify.Sender.Send（Sender 自带
// notification_log.idempotency_key 幂等，重投安全）。发送慢只阻塞该 worker 槽位，
// MVP 可接受（见文件头注释）。
func notifySendPayloadHandler(sender *notify.Sender) scheduler.Handler {
	return func(ctx context.Context, refID string, payload []byte) error {
		var req notify.SendRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("wiring: 反序列化 notify_send payload 失败: %w", err)
		}
		return sender.Send(ctx, req)
	}
}

// compositeStopper 同时满足 api.EscalationStopper 与 internal/api/lark_callback.go
// 未来可能的同类注入点：把 oncall.Chain.Stop 与 notify.Engine.Stop 组合成一次调用——
// Ack/Resolve 只停人员升级链会留下渠道升级的 pending 定时器（虽然 advance 内部的
// shouldContinueEscalation 会让它到期时幂等 no-op，不会误发，但应当主动停干净，
// 不依赖"到期后自愈"）。两者的 Stop 方法签名完全一致，都是幂等的。
type compositeStopper struct {
	chain  *oncall.Chain
	engine *notify.Engine
}

func newCompositeStopper(chain *oncall.Chain, engine *notify.Engine) compositeStopper {
	return compositeStopper{chain: chain, engine: engine}
}

func (s compositeStopper) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	if err := s.chain.Stop(ctx, tx, alertID); err != nil {
		return err
	}
	return s.engine.Stop(ctx, tx, alertID)
}
