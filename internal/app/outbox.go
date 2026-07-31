package app

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
	"alert-system/internal/ops"
	"alert-system/internal/scheduler"
)

// notifySendTimerKind 是 outbox 定时器在 scheduler 里的 kind：六个注册 kind 里唯一
// 由本包（而不是各业务包）定义的一个，因为它是纯装配层概念，不属于任何业务包的
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

// scheduleSend 把一个 SendRequest 序列化后注册进 "notify_send" 定时器，fireAt=now，
// 即"立即可发送"——outbox 的 worker 槽位（scheduler.Run 的下一轮轮询）会拾取，不需要
// 真的延后。refID 是唯一意图键，口径对齐 notify.Sender 的幂等键：同一发送目标
// （同一告警/分组 + 同一级 + 同一渠道 + 同一 target）只会有一个 in-flight 的 outbox
// 定时器，重复调用会替换 fire_at/payload（Schedule 本身是替换语义），不会产生重复发送。
func (o *notifyOutbox) scheduleSend(ctx context.Context, tx pgx.Tx, req notify.SendRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("app: 序列化通知发送请求失败: %w", err)
	}
	if err := o.scheduler.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(req), o.now(), payload); err != nil {
		return fmt.Errorf("app: 注册 notify_send 定时器失败: %w", err)
	}
	return nil
}

// notifySendRefID 是 outbox 定时器的 refID，只用业务元组（resourceKey/step/channel/
// target）拼接，故意不掺 Round：同一个业务元组只应该有一个 in-flight 的 notify_send
// 定时器，本轮通知意图还没发出去时再次触发同一元组（如分组的 group_interval 抖动、
// GroupStage 重复 flush）会被 Schedule 的替换语义收敛成一次，而不是让 Round 差异
// 制造出并行的多个定时器——这个"防抖折叠"的粒度必须停留在业务元组层面。Round 只
// 影响 notify.Sender 的幂等键（sender.go），不影响这里的定时器去重粒度。
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
	// round 在本次调用（本次人员升级推进）发生的时刻一次性捕获，随每个 SendRequest
	// 序列化进 payload 持久化——同一次推进里给多个 target 各发一条，共享同一个 round；
	// 绝不能在 notify.Sender.Send 内部现算，否则崩溃重投时幂等键会跟着变，见
	// SendRequest.Round 的文档。
	round := o.now().UnixNano()
	for _, t := range targets {
		req := notify.SendRequest{
			AlertID:     alert.ID,
			Step:        &level,
			ChannelKind: defaultNotifyChannel,
			Target:      t.UserID,
			Msg:         msg,
			Round:       round,
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
//
// 只对 activeAlerts（未被任何静默命中）执行上述三件事：组的 group_wait/group_interval
// 定时器不感知静默——只要组内任意成员在静默生效期间收到重复事件（去重合并/迟到更新），
// GroupStage.Process 就会重新挂上组到期定时器，到期后无条件调用本方法。若不在这里过滤，
// 静默生效期间只要有重复事件抵达就会重新发出组消息、重启 repeat/人员升级/渠道升级
// 三条链，与"静默生效零通知"的要求相悖。repeat/两条升级链各自的 advance/HandleRepeat
// 在自己的定时器触发时还有一道被动的 SilencedBy 检查（阻止继续续期），但那是"链路已经
// 在跑"之后的收尾；本方法是"要不要重新驱动"的入口，必须在入口处过滤。
// 组内全部成员都被静默命中时 activeAlerts 为空，直接跳过（不发消息、不启动任何链）。
func (o *notifyOutbox) NotifyGroup(ctx context.Context, tx pgx.Tx, groupKey string, alerts []domain.Alert) error {
	active := activeAlerts(alerts)
	if len(active) == 0 {
		return nil
	}

	req := notify.SendRequest{
		GroupKey:    groupKey,
		ChannelKind: defaultNotifyChannel,
		Target:      active[0].TeamID,
		Msg:         buildGroupMessage(groupKey, active),
		// round 在本次组 flush 的时刻捕获（Bug C 修复）：静默过期后重复事件重新驱动
		// 组 flush 会再次调用本方法，届时 o.now() 已经推进，round 随之不同，
		// notify.Sender 才不会把"恢复后的组通知"误判为上一次组通知的崩溃重投。
		Round: o.now().UnixNano(),
	}
	if err := o.scheduleSend(ctx, tx, req); err != nil {
		return err
	}

	baseTime := earliestStartsAt(active)
	for _, alert := range active {
		if err := o.engine.Start(ctx, tx, alert, baseTime); err != nil {
			return fmt.Errorf("app: 启动渠道升级序列失败: %w", err)
		}
		fireAt := o.now().Add(o.repeatInterval)
		if _, err := o.scheduler.ScheduleIfAbsent(ctx, tx, notify.TimerKindAlertRepeat, alert.ID, fireAt, nil); err != nil {
			return fmt.Errorf("app: 注册首个 repeat 定时器失败: %w", err)
		}
		if err := o.chain.Start(ctx, tx, alert); err != nil {
			return fmt.Errorf("app: 启动人员升级链失败: %w", err)
		}
	}
	return nil
}

// activeAlerts 过滤出 alerts 里当前未被任何静默命中的子集，顺序与输入一致。
func activeAlerts(alerts []domain.Alert) []domain.Alert {
	active := make([]domain.Alert, 0, len(alerts))
	for _, a := range alerts {
		if len(a.SilencedBy) == 0 {
			active = append(active, a)
		}
	}
	return active
}

// NotifyRepeat 实现 notify.RepeatNotifier：repeat_interval 到期的重复提醒，固定走
// defaultNotifyChannel，target 用 alert.TeamID。
func (o *notifyOutbox) NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	req := notify.SendRequest{
		AlertID:     alert.ID,
		ChannelKind: defaultNotifyChannel,
		Target:      alert.TeamID,
		Msg:         buildAlertMessage(alert),
		// round 在本次 repeat 定时器触发的时刻捕获：下一轮 repeat 触发时 o.now() 已经
		// 推进 repeatInterval，round 随之不同，是 R6.5"重复提醒需要真正重发"的关键。
		Round: o.now().UnixNano(),
	}
	return o.scheduleSend(ctx, tx, req)
}

// DispatchStep 实现 notify.Dispatcher：渠道升级推进到 step 级，对 channels 里的每个
// 渠道各自注册一个发送意图，target 用 alert.TeamID（渠道升级是团队维度的触达强度
// 提升，不针对具体个人——针对个人的升级是 oncall 包的人员升级链，二者正交）。
func (o *notifyOutbox) DispatchStep(ctx context.Context, tx pgx.Tx, alert domain.Alert, step int, channels []string) error {
	msg := buildAlertMessage(alert)
	// round 在本次渠道升级推进的时刻一次性捕获，同一次推进里给多个渠道各发一条，
	// 共享同一个 round（理由同 NotifyEscalation）。
	round := o.now().UnixNano()
	for _, ch := range channels {
		req := notify.SendRequest{
			AlertID:     alert.ID,
			Step:        &step,
			ChannelKind: ch,
			Target:      alert.TeamID,
			Msg:         msg,
			Round:       round,
		}
		if err := o.scheduleSend(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// ScheduleOpsSend 实现 ops.OpsNotifier：把 ops 周期任务（日报/悬置扫描/值班播报）
// 的一次发送意图注册进既有 notify_send 定时器，完全复用 scheduleSend/
// notifySendRefID——不新写一套调度逻辑。ops 消息没有 AlertID，只需按 design.md R2
// 把 GroupKey 拼成 "ops:{teamID}:{kind}"：notifySendRefID 的 resourceKey 在
// AlertID 为空时回退取 GroupKey，算出的 refID 天然是
// "ops:{teamID}:{kind}\x1e-\x1e{channel}\x1e{target}"（step 恒为 "-"，因为 ops
// 没有渠道升级的"级"概念）——不含 round，满足不变量 4"refID 不掺 round、round 只
// 随 payload 持久化供 Sender 算幂等键"的要求。
func (o *notifyOutbox) ScheduleOpsSend(ctx context.Context, tx pgx.Tx, teamID, kind string, round int64, ch ops.ChannelTarget, msg notify.Message) error {
	req := notify.SendRequest{
		GroupKey:    opsGroupKey(teamID, kind),
		ChannelKind: ch.Kind,
		Target:      ch.Target,
		Msg:         msg,
		Round:       round,
	}
	return o.scheduleSend(ctx, tx, req)
}

// opsGroupKey 拼出 ops 消息的 GroupKey，与 ops.OpsNotifier 接口注释里"由实现自己
// 拼、调用方不重复维护这个格式常量"的约定一致。
func opsGroupKey(teamID, kind string) string {
	return "ops:" + teamID + ":" + kind
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
// notify.SendRequest，交给 notify.Sender.Send。tx 是 processOneDue 传入的、已经把这条
// notify_send 定时器标记为 fired 的事务，本 handler 不使用它——Sender.Send 内部按
// R6.1/R6.3 的既有设计自己开独立事务做占位写入/状态回写（notify.Sender 的 txRunner，
// 与 scheduler 的 tx 完全无关），期间夹着同步的外部 IO（HTTP/短信）与指数退避重试。
// 这意味着外层 processOneDue 事务（持有该 notify_send 定时器行锁）在 Sender.Send
// 整个执行期间都保持打开——已由 team-lead 确认接受这一 MVP 权衡（发送慢只阻塞
// "notify_send" 这一个 kind 的 worker 槽位，不阻塞其余五个 kind），留待后续任务视
// 吞吐需求把发送异步化。
func notifySendPayloadHandler(sender *notify.Sender) scheduler.Handler {
	return func(ctx context.Context, _ pgx.Tx, refID string, payload []byte) error {
		var req notify.SendRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("app: 反序列化 notify_send payload 失败: %w", err)
		}
		return sender.Send(ctx, req)
	}
}
