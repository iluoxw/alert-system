package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/ops"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// groupMessageMaxTitles 是分组通知 Body 摘要里最多列出的成员标题数，超出部分折叠
// 为"…另有 N 条"——避免组内成员很多时消息体过长，同时给值班人足够的影响面线索
// （比缺陷 1 只显示第一条更有用）。
const groupMessageMaxTitles = 5

// notifySendTimerKind 是 outbox 定时器在 scheduler 里的 kind：六个注册 kind 里唯一
// 由本包（而不是各业务包）定义的一个，因为它是纯装配层概念，不属于任何业务包的
// 领域逻辑。
const notifySendTimerKind = "notify_send"

// teamNotifyChannel 是"组通知""重复提醒"两类团队维度通知意图使用的固定渠道字面量
// （R6.2 约定飞书是主力渠道）。这两类意图的 target 是 alert.TeamID/组内共同 TeamID，
// 不是具体的人，因此不走任务 9 新增的 ContactResolver 到人解析（team 还没有渠道
// 联系方式表，是已知的后续缺口，不在本任务范围）；只有人员升级（NotifyEscalation）
// 携带的是具体责任人的 UserID，才走 PersonnelChannel + TargetUserID 解析路径
// （见 notifyOutbox.personnelChannel）。渠道升级（notify.Dispatcher）按通知策略
// 配置的 channels 逐级切换，两者都不用这个字面量。
const teamNotifyChannel = "lark"

// notifyOutboxScheduler 是 notifyOutbox 依赖的定时器子集，由 *scheduler.Scheduler 实现。
// Cancel 是 IncidentOutbox.CancelRecoveryNudge 需要的额外能力（notifyOutbox 本身
// 不用它，与 IncidentOutbox 共享同一个接口只是为了不再声明第二个几乎相同的接口）。
type notifyOutboxScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error)
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
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

// notifyOutboxRouteRepo 是 NotifyGroup 解析路由级 repeat_interval 依赖的仓储子集，
// 由 store.RouteRepo 实现（结构化满足）。缺陷 1 修复：路由的 RepeatIntervalSeconds
// 此前只写入 pipeline.RouteResult 却从未被消费，是一份死配置——这里在"意图时刻"
// （首个 alert_repeat 定时器注册时）按告警的 RouteID 查一次路由，取其配置值。
type notifyOutboxRouteRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error)
}

// notifyOutbox 同时实现四个"通知意图"接口。每个方法只做一件事：把意图翻译成一个
// 或多个 scheduleSend 调用；NotifyGroup 额外承担"首次通知编排"职责（组首时间基准
// 的渠道升级 + 首个 repeat 定时器 + 人员升级链启动），见该方法注释。
type notifyOutbox struct {
	scheduler        notifyOutboxScheduler
	engine           notifyOutboxEngineStarter
	chain            notifyOutboxChainStarter
	routes           notifyOutboxRouteRepo // 路由级 repeat_interval 查询，可为 nil（无路由仓储时全部走全局默认）
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
	parts := []string{resourceKey, step, req.ChannelKind, req.Target}
	if req.MetaAlert {
		// 元告警必须与原意图分属不同 refID：fallback 目标常常就是原通知失败的那个
		// 目标（owner team 无渠道时本来就回落到它），共用 refID 会让两者在 pending
		// 收敛里互相覆盖 payload——后注册的一方被静默丢掉。
		parts = append(parts, "meta")
	}
	return strings.Join(parts, "\x1e")
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

	req := notify.SendRequest{
		GroupKey:    groupKey,
		ChannelKind: teamNotifyChannel,
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
		interval, err := o.resolveRepeatInterval(ctx, tx, alert.RouteID)
		if err != nil {
			return err
		}
		repeatPayload, err := json.Marshal(notify.RepeatPayload{IntervalSeconds: int(interval / time.Second)})
		if err != nil {
			return fmt.Errorf("app: 序列化 repeat payload 失败: %w", err)
		}
		fireAt := o.now().Add(interval)
		if _, err := o.scheduler.ScheduleIfAbsent(ctx, tx, notify.TimerKindAlertRepeat, alert.ID, fireAt, repeatPayload); err != nil {
			return fmt.Errorf("app: 注册首个 repeat 定时器失败: %w", err)
		}
		if err := o.chain.Start(ctx, tx, alert); err != nil {
			return fmt.Errorf("app: 启动人员升级链失败: %w", err)
		}
	}
	return nil
}

// resolveRepeatInterval 按告警的 RouteID 解析本条 repeat 链应使用的间隔：路由未
// 命中过任何规则（RouteID 为空）、未注入路由仓储、路由已被删除，或路由的
// RepeatIntervalSeconds<=0（未配置，沿用全局默认，对齐不变量 10"降噪一律
// opt-in"同一口径）时，一律回落 o.repeatInterval。
//
// 只有"未找到"可以回落；其他查询错误必须上抛：同一 pgx.Tx 内一次失败的查询会让
// 整个事务进入 aborted 状态，吞掉错误并不能让后续 ScheduleIfAbsent 成功，只会把
// 真正根因换成"current transaction is aborted"（见不变量 21 同类教训）。
func (o *notifyOutbox) resolveRepeatInterval(ctx context.Context, tx pgx.Tx, routeID string) (time.Duration, error) {
	if o.routes == nil || routeID == "" {
		return o.repeatInterval, nil
	}
	route, err := o.routes.GetByID(ctx, tx, routeID)
	if errors.Is(err, store.ErrNotFound) {
		return o.repeatInterval, nil
	}
	if err != nil {
		return 0, fmt.Errorf("app: 查询路由 repeat 间隔失败: %w", err)
	}
	if route.RepeatIntervalSeconds <= 0 {
		return o.repeatInterval, nil
	}
	return time.Duration(route.RepeatIntervalSeconds) * time.Second, nil
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
// teamNotifyChannel，target 用 alert.TeamID。
//
// 注：接口签名（notify.RepeatNotifier）只携带 domain.Alert，没有具体责任人信息，
// 无法像 NotifyEscalation 那样解析到人——这与 design.md C3「升级链与 repeat 两处
// Target: t.UserID 改 TargetUserID」的表述不完全一致，团队维度提醒本任务范围内维持
// 现状，留待主对话确认是否需要引入"重复提醒也到人"的新语义（需要额外的当前责任人
// 数据来源，属于设计层决策，不在本任务擅自展开）。
func (o *notifyOutbox) NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	req := notify.SendRequest{
		AlertID:     alert.ID,
		ChannelKind: teamNotifyChannel,
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
// Body 缺陷修复（此前恒为 alerts[0].Title，组内多条告警时值班人无法从消息判断
// 影响面）：改为列出组内成员摘要，见 buildGroupBody。
func buildGroupMessage(groupKey string, alerts []domain.Alert) notify.Message {
	return notify.Message{
		AlertIDs:   alertIDs(alerts),
		GroupKey:   groupKey,
		Severity:   string(alerts[0].Severity),
		Title:      fmt.Sprintf("%d 条告警（分组 %s）", len(alerts), groupKey),
		Body:       buildGroupBody(alerts),
		OccurredAt: earliestStartsAt(alerts),
	}
}

// groupSeverityRank 给严重程度定一个用于排序的权重：数值越小越严重。展示顺序
// "严重程度从高到低"依赖这个显式权重，而不是 Severity 字符串本身的字典序
// （"critical" < "info" < "warning" 的字典序与业务语义相反）。
var groupSeverityRank = map[domain.Severity]int{
	domain.SeverityCritical: 0,
	domain.SeverityWarning:  1,
	domain.SeverityInfo:     2,
}

// buildGroupBody 按"严重程度从高到低、同级按 StartsAt 从早到晚"排序后，最多列出
// 前 groupMessageMaxTitles 条告警标题，超出部分折叠为"…另有 N 条"。只有 1 条时
// 保持原样（不加多余格式），与升级前单条分组的展示一致。
//
// 排序在拷贝出的切片上进行（不修改调用方传入的 alerts 底层数组），排序键是
// Severity → StartsAt → Title → ID 的全序：批量触发时同级同 StartsAt 很常见，
// 若缺少 Title/ID 兜底，稳定排序会保留输入顺序，乱序到达就会产出不同 Body。
func buildGroupBody(alerts []domain.Alert) string {
	if len(alerts) == 1 {
		return alerts[0].Title
	}

	sorted := make([]domain.Alert, len(alerts))
	copy(sorted, alerts)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := groupSeverityRank[sorted[i].Severity], groupSeverityRank[sorted[j].Severity]
		if ri != rj {
			return ri < rj
		}
		if !sorted[i].StartsAt.Equal(sorted[j].StartsAt) {
			return sorted[i].StartsAt.Before(sorted[j].StartsAt)
		}
		if sorted[i].Title != sorted[j].Title {
			return sorted[i].Title < sorted[j].Title
		}
		return sorted[i].ID < sorted[j].ID
	})

	shown := sorted
	remaining := 0
	if len(shown) > groupMessageMaxTitles {
		remaining = len(shown) - groupMessageMaxTitles
		shown = shown[:groupMessageMaxTitles]
	}

	titles := make([]string, len(shown))
	for i, a := range shown {
		titles[i] = a.Title
	}
	body := strings.Join(titles, "；")
	if remaining > 0 {
		body += fmt.Sprintf("；…另有 %d 条", remaining)
	}
	return body
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

// scheduleNotifyFailureMetaAlert 在 notify_send 最终失败的同一短事务里登记一条
// 元告警：发给 FAULT_NOTIFY_FALLBACK_* ，payload.meta_alert=true，round 沿用原意图。
// fallback 为空时写时间线说明"无兜底目标"（D5），不静默、不 Schedule。
// 本函数只 Schedule，绝不做 HTTP。
func scheduleNotifyFailureMetaAlert(ctx context.Context, tx pgx.Tx, sched notifyOutboxScheduler, faults metaAlertFaultRepo, channel, target string, now time.Time, original notify.SendRequest) error {
	if original.MetaAlert {
		return nil
	}
	if channel == "" || target == "" {
		return writeNoFallbackAudit(ctx, tx, faults, original, now)
	}
	if sched == nil {
		return writeNoFallbackAudit(ctx, tx, faults, original, now)
	}
	meta := buildMetaAlertRequest(original, channel, target, now)
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("app: 序列化元告警 notify_send 失败: %w", err)
	}
	if err := sched.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(meta), now, payload); err != nil {
		return fmt.Errorf("app: 注册元告警 notify_send 失败: %w", err)
	}
	return nil
}

type metaAlertFaultRepo interface {
	AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error)
}

func buildMetaAlertRequest(original notify.SendRequest, channel, target string, now time.Time) notify.SendRequest {
	return notify.SendRequest{
		AlertID:     original.AlertID,
		GroupKey:    original.GroupKey,
		IncidentID:  original.IncidentID,
		Step:        original.Step,
		ChannelKind: channel,
		Target:      target,
		Round:       original.Round,
		MetaAlert:   true,
		Msg: notify.Message{
			AlertIDs:     original.Msg.AlertIDs,
			GroupKey:     original.Msg.GroupKey,
			Severity:     original.Msg.Severity,
			Title:        "通知发送失败",
			Body:         metaAlertBody(original),
			DetailURL:    original.Msg.DetailURL,
			ResourceType: original.Msg.ResourceType,
			ResourceID:   original.Msg.ResourceID,
			OccurredAt:   now,
		},
	}
}

// metaAlertBody 只描述资源身份，禁止带上原始通知目标 URL（不变量 13）。
func metaAlertBody(original notify.SendRequest) string {
	switch {
	case original.AlertID != "":
		return fmt.Sprintf("告警 %s 的通知在重试耗尽后仍未送达。", original.AlertID)
	case original.IncidentID != "":
		return fmt.Sprintf("Incident %s 的通报在重试耗尽后仍未送达。", original.IncidentID)
	case original.GroupKey != "":
		return fmt.Sprintf("分组 %s 的通知在重试耗尽后仍未送达。", original.GroupKey)
	default:
		return "一条通知在重试耗尽后仍未送达。"
	}
}

func writeNoFallbackAudit(ctx context.Context, tx pgx.Tx, faults metaAlertFaultRepo, original notify.SendRequest, now time.Time) error {
	faultID := ""
	if original.Msg.ResourceType == "fault" {
		faultID = original.Msg.ResourceID
	}
	if faultID == "" {
		faultID = faultIDFromNotifyGroupKey(original.GroupKey)
	}
	if faultID != "" && faults != nil {
		if _, err := faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
			FaultID: faultID, Actor: "system", Action: domain.FaultTimelineActionNotifySkipped, At: now,
			Detail: map[string]any{"reason": "notify_send_failed_no_fallback"},
		}); err != nil {
			return fmt.Errorf("app: 写入无兜底目标时间线失败: %w", err)
		}
		return nil
	}
	log.Printf("notify: 发送最终失败且无兜底目标，跳过元告警 alert=%s group=%s", original.AlertID, original.GroupKey)
	return nil
}

func faultIDFromNotifyGroupKey(groupKey string) string {
	const prefix = "fault:"
	if !strings.HasPrefix(groupKey, prefix) {
		return ""
	}
	rest := groupKey[len(prefix):]
	if i := strings.LastIndex(rest, ":"); i > 0 {
		return rest[:i]
	}
	return rest
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
