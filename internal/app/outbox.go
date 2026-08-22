package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// teamNotifyChannel 是组通知、重复提醒和风暴摘要请求的默认渠道种类。真实 target
// 必须由 team_ops_settings.channels 解析，绝不能再把 TeamID 当作飞书 chat ID。
const teamNotifyChannel = "lark"

const (
	teamNotificationGroup             = "group"
	teamNotificationRepeat            = "repeat"
	teamNotificationChannelEscalation = "channel_escalation"
	teamNotificationStormSummary      = "storm_summary"
)

// notifyOutboxScheduler 是 notifyOutbox 依赖的定时器子集，由 *scheduler.Scheduler 实现。
type notifyOutboxScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error)
}

// teamNotificationChannelRepo 把 team_ops_settings.channels 暴露为最小只读接口。
// store.ErrNotFound 由 app 适配器转换为空切片；其他查询错误必须向上传播。
type teamNotificationChannelRepo interface {
	TeamChannels(ctx context.Context, tx pgx.Tx, teamID string) ([]ops.ChannelTarget, error)
}

// teamNotificationTimeline 是缺少真实团队渠道时的审计写入点。
type teamNotificationTimeline interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
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
	scheduler        notifyOutboxScheduler
	teamChannels     teamNotificationChannelRepo
	timeline         teamNotificationTimeline
	engine           notifyOutboxEngineStarter
	chain            notifyOutboxChainStarter
	repeatInterval   time.Duration
	personnelChannel string // 到人发送的渠道种类（config.PersonnelChannel，默认 "email"）
	now              func() time.Time
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

// scheduleTeamSends 把团队维度的渠道 kind 解析成一个或多个真实 target，再逐一落
// notify_send。解析发生在意图创建事务中，因此 payload、timer ref ID 与 Sender 幂等
// 键都固化同一个真实 target；后续配置变更不会改写已经在途的意图。
func (o *notifyOutbox) scheduleTeamSends(ctx context.Context, tx pgx.Tx, teamID string, requestedKinds []string, req notify.SendRequest, notificationKind string) error {
	if o.teamChannels == nil {
		return fmt.Errorf("app: 未配置团队通知渠道仓储")
	}
	channels, err := o.teamChannels.TeamChannels(ctx, tx, teamID)
	if err != nil {
		return fmt.Errorf("app: 查询团队通知渠道失败(team_id=%s): %w", teamID, err)
	}

	type channelPair struct {
		kind   string
		target string
	}
	byKind := make(map[string][]string)
	configured := make(map[channelPair]struct{}, len(channels))
	for _, channel := range channels {
		kind := strings.TrimSpace(channel.Kind)
		target := strings.TrimSpace(channel.Target)
		if kind == "" || target == "" {
			continue
		}
		pair := channelPair{kind: kind, target: target}
		if _, exists := configured[pair]; exists {
			continue
		}
		configured[pair] = struct{}{}
		byKind[kind] = append(byKind[kind], target)
	}

	requested := make(map[string]struct{}, len(requestedKinds))
	for _, rawKind := range requestedKinds {
		kind := strings.TrimSpace(rawKind)
		if kind == "" {
			continue
		}
		if _, exists := requested[kind]; exists {
			continue
		}
		requested[kind] = struct{}{}

		targets := byKind[kind]
		if len(targets) == 0 {
			if err := o.appendTeamNotificationSkipped(ctx, tx, teamID, kind, notificationKind, req); err != nil {
				return err
			}
			continue
		}
		for _, target := range targets {
			resolved := req
			resolved.ChannelKind = kind
			resolved.Target = target
			if err := o.scheduleSend(ctx, tx, resolved); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendTeamNotificationSkipped 明确记录“没有真实团队渠道，所以未尝试发送”。它不
// 使用 notify_failed：外部渠道一次都没有调用，attempts 必须保持 0。Group/Storm
// 按 Message.AlertIDs 扇出；没有任何可关联告警的合成摘要只能落结构化日志。
func (o *notifyOutbox) appendTeamNotificationSkipped(ctx context.Context, tx pgx.Tx, teamID, channelKind, notificationKind string, req notify.SendRequest) error {
	alertIDs := req.Msg.AlertIDs
	if req.AlertID != "" {
		alertIDs = []string{req.AlertID}
	}
	if len(alertIDs) == 0 {
		log.Printf("app: 团队通知已跳过 reason=no_team_channel team_id=%s channel=%s notification_kind=%s group_key=%s", teamID, channelKind, notificationKind, req.GroupKey)
		return nil
	}
	if o.timeline == nil {
		return fmt.Errorf("app: 未配置团队通知跳过审计仓储")
	}

	var step any
	if req.Step != nil {
		step = *req.Step
	}
	detail := map[string]any{
		"channel":           channelKind,
		"team_id":           teamID,
		"step":              step,
		"attempts":          0,
		"status":            "skipped",
		"reason":            "no_team_channel",
		"notification_kind": notificationKind,
	}
	if req.GroupKey != "" {
		detail["group_key"] = req.GroupKey
	}

	now := o.now()
	seen := make(map[string]struct{}, len(alertIDs))
	for _, alertID := range alertIDs {
		if alertID == "" {
			continue
		}
		if _, exists := seen[alertID]; exists {
			continue
		}
		seen[alertID] = struct{}{}
		if _, err := o.timeline.Append(ctx, tx, domain.TimelineEntry{
			AlertID: alertID,
			Actor:   "notify-outbox",
			Action:  domain.TimelineActionNotifySkipped,
			Detail:  detail,
			At:      now,
		}); err != nil {
			return fmt.Errorf("app: 记录团队通知跳过失败(alert_id=%s): %w", alertID, err)
		}
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
		resourceKey = req.IncidentID
	}
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
// 各自注册一个发送意图（同一渠道、不同 target），到人发送——ChannelKind 用
// personnelChannel（config.PersonnelChannel），TargetUserID 触发 Sender.Send 第 0 段
// 解析出实际渠道地址（任务 9）。Target 仍填 t.UserID（而不是留空或填解析后地址）：
// notifySendRefID 用它拼定时器 refID，必须是 UserID 这个稳定标识，解析后的地址会
// 随联系方式变更而变，破坏"同一意图只有一个 in-flight 定时器"的收敛语义。
func (o *notifyOutbox) NotifyEscalation(ctx context.Context, tx pgx.Tx, alert domain.Alert, level int, targets []oncall.OnCallEntry) error {
	msg := buildAlertMessage(alert)
	// round 在本次调用（本次人员升级推进）发生的时刻一次性捕获，随每个 SendRequest
	// 序列化进 payload 持久化——同一次推进里给多个 target 各发一条，共享同一个 round；
	// 绝不能在 notify.Sender.Send 内部现算，否则崩溃重投时幂等键会跟着变，见
	// SendRequest.Round 的文档。
	round := o.now().UnixNano()
	for _, t := range targets {
		req := notify.SendRequest{
			AlertID:      alert.ID,
			Step:         &level,
			ChannelKind:  o.personnelChannel,
			Target:       t.UserID,
			TargetUserID: t.UserID,
			Msg:          msg,
			Round:        round,
		}
		if err := o.scheduleSend(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// NotifyGroup 实现 pipeline.GroupNotifier：组 flush 时（1）发一条固定走
// teamNotifyChannel 的"组消息"发送意图，target 用组内告警共同的 TeamID（路由阶段
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

	round := o.now().UnixNano()
	req := notify.SendRequest{
		GroupKey: groupKey,
		Msg:      buildGroupMessage(groupKey, active),
		// round 在本次组 flush 的时刻捕获（Bug C 修复）：静默过期后重复事件重新驱动
		// 组 flush 会再次调用本方法，届时 o.now() 已经推进，round 随之不同，
		// notify.Sender 才不会把"恢复后的组通知"误判为上一次组通知的崩溃重投。
		Round: round,
	}
	if err := o.scheduleTeamSends(ctx, tx, active[0].TeamID, []string{teamNotifyChannel}, req, teamNotificationGroup); err != nil {
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

// NotifyRepeat 实现 notify.RepeatNotifier：repeat_interval 到期的重复提醒，固定请求
// teamNotifyChannel，再由 team_ops_settings.channels 解析一个或多个真实 target。
//
// 注：接口签名（notify.RepeatNotifier）只携带 domain.Alert，没有具体责任人信息，
// 无法像 NotifyEscalation 那样解析到人——这与 design.md C3「升级链与 repeat 两处
// Target: t.UserID 改 TargetUserID」的表述不完全一致，团队维度提醒本任务范围内维持
// 现状，留待主对话确认是否需要引入"重复提醒也到人"的新语义（需要额外的当前责任人
// 数据来源，属于设计层决策，不在本任务擅自展开）。
func (o *notifyOutbox) NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	req := notify.SendRequest{
		AlertID: alert.ID,
		Msg:     buildAlertMessage(alert),
		// round 在本次 repeat 定时器触发的时刻捕获：下一轮 repeat 触发时 o.now() 已经
		// 推进 repeatInterval，round 随之不同，是 R6.5"重复提醒需要真正重发"的关键。
		Round: o.now().UnixNano(),
	}
	return o.scheduleTeamSends(ctx, tx, alert.TeamID, []string{teamNotifyChannel}, req, teamNotificationRepeat)
}

// DispatchStep 实现 notify.Dispatcher：渠道升级推进到 step 级，把策略请求的 channel
// kinds 与 team_ops_settings.channels 求交集并按真实 target 注册发送意图。渠道升级
// 是团队维度的触达强度提升，不针对具体个人；针对个人的升级是 oncall 包的人员升级
// 链，二者正交。
func (o *notifyOutbox) DispatchStep(ctx context.Context, tx pgx.Tx, alert domain.Alert, step int, channels []string) error {
	msg := buildAlertMessage(alert)
	// round 在本次渠道升级推进的时刻一次性捕获，同一次推进里给多个渠道各发一条，
	// 共享同一个 round（理由同 NotifyEscalation）。
	round := o.now().UnixNano()
	req := notify.SendRequest{AlertID: alert.ID, Step: &step, Msg: msg, Round: round}
	return o.scheduleTeamSends(ctx, tx, alert.TeamID, channels, req, teamNotificationChannelEscalation)
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
	prefix := alert.ID
	if !alert.UpdatedAt.IsZero() {
		prefix += ":" + fmt.Sprint(alert.UpdatedAt.UnixNano())
	}
	return notify.Message{
		AlertIDs:     []string{alert.ID},
		Severity:     string(alert.Severity),
		Title:        alert.Title,
		Body:         alert.Title,
		Labels:       alert.Labels,
		OccurredAt:   alert.StartsAt,
		ResourceType: "alert", ResourceID: alert.ID, ActionRequestPrefix: prefix,
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

// notifySendPayloadHandler 构造 "notify_send" 的 scheduler.DetachedHandler：解码
// payload 为 notify.SendRequest，交给 notify.Sender.Send。
//
// 这是唯一一个注册成「事务外执行」模式的 kind（scheduler.RegisterDetachedHandler），
// 因为它是唯一一个 handler 内部必然发生外部 IO 的 kind：Sender.Send 自己开独立小事务
// 做占位写入/状态回写（notify.Sender 的 txRunner），中间夹着同步的 HTTP/SMTP 发送与
// 指数退避重试。如果按默认模式在 scheduler 的 per-timer 事务内调用，那个事务会在整段
// 外部 IO 期间保持打开、一直持有这条 notify_send 的 timers 行锁和一个连接池连接——
// 慢渠道或超时渠道会把事务和行锁的持有时间拉到分钟级。
//
// 因此签名里没有 tx，也不需要：scheduler 先在短事务里把这条定时器 claim 下来（带租约）
// 并提交、释放行锁，之后才调用本函数；执行结果由 scheduler 另开短事务回写。崩溃安全性
// 由租约负责（claimed 不是终态，租约过期后重新领取）。
//
// 口径与改造前完全一致地保持 at-least-once：Sender 的 notification_log 幂等键（含随
// payload 持久化的 Round）只收敛逻辑性的重复调度，**消除不了**「HTTP/SMTP 已经发出、
// 结果还没确认就崩溃」的重发窗口——重投时看到的记录是 pending 而不是 sent，消息会真的
// 再发一次。这是外部 IO 无法参与数据库事务的固有代价（CLAUDE.md 不变量 4 已裁决接受），
// 租约机制换掉的只是「崩溃后永远不再重投、通知永久丢失」这个更坏的行为。
func notifySendPayloadHandler(sender *notify.Sender) scheduler.DetachedHandler {
	return func(ctx context.Context, _ string, payload []byte) error {
		var req notify.SendRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("app: 反序列化 notify_send payload 失败: %w", err)
		}
		return sender.Send(ctx, req)
	}
}
