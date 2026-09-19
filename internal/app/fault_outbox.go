package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// faultOutboxFaultRepo 是 FaultOutbox 依赖的 FaultRepo 子集（结构化满足
// *store.FaultRepo），窄接口只声明本文件用到的方法，便于单测用内存 fake 替换。
type faultOutboxFaultRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error)
	ListRecentlyResolvedByCorrelationKey(ctx context.Context, tx pgx.Tx, correlationKey string, since time.Time) ([]domain.Fault, error)
	ListOpenForNodeIDs(ctx context.Context, tx pgx.Tx, nodeIDs []string, limit int) ([]domain.Fault, error)
	SetOwnerNotified(ctx context.Context, tx pgx.Tx, id string, at time.Time) error
	AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error)
}

// faultOutboxOpsRepo 是查询 owner team 通知渠道配置的仓储子集，由 *store.OpsRepo 实现。
type faultOutboxOpsRepo interface {
	GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*store.OpsSettings, error)
}

// faultOutboxProviderLookup 是拓扑抑制判定所需的上游 provider 查询子集，由
// impact.Projector.ProviderNodeIDs 实现——复用其既有 maxDepth=8/staleAfter=24h
// 裁决（internal/impact/projector.go），本包不重复定义遍历参数。
type faultOutboxProviderLookup interface {
	ProviderNodeIDs(ctx context.Context, tx pgx.Tx, consumerNodeID string) ([]string, error)
}

// FaultOutbox 把 Fault 进入 active 的通知意图持久化为 notify_send 定时器
// （R5，不变量 4/22）。以 IncidentOutbox 为模板：调用方必须在 Fault 状态转移的
// 同一数据库事务内调用 QueueFaultActivated，全程不得出现同步外部 IO。
type FaultOutbox struct {
	scheduler notifyOutboxScheduler
	faults    faultOutboxFaultRepo
	ops       faultOutboxOpsRepo
	providers faultOutboxProviderLookup

	cooldown        time.Duration // R5.3 冷却窗口；<=0 表示不启用冷却判定
	fallbackChannel string        // owner team 未配置渠道时的兜底渠道种类
	fallbackTarget  string        // 兜底渠道地址；可能是飞书机器人 hook URL（凭据），禁止落日志
	webBaseURL      string        // 非空时用于拼 Fault 详情链接

	now func() time.Time
}

// NewFaultOutbox 构造 FaultOutbox。providers 允许为 nil（等价于跳过拓扑抑制判定，
// 例如尚未接入拓扑数据的部署）。
func NewFaultOutbox(scheduler notifyOutboxScheduler, faults faultOutboxFaultRepo, ops faultOutboxOpsRepo, providers faultOutboxProviderLookup, cooldown time.Duration, fallbackChannel, fallbackTarget, webBaseURL string) *FaultOutbox {
	return &FaultOutbox{
		scheduler: scheduler, faults: faults, ops: ops, providers: providers,
		cooldown: cooldown, fallbackChannel: fallbackChannel, fallbackTarget: fallbackTarget,
		webBaseURL: webBaseURL, now: time.Now,
	}
}

// faultActivatedGroupKey 拼出 Fault 激活通知的 GroupKey，与 opsGroupKey 同一手法
// （"资源类型:资源ID:动作"）：notify.SendRequest 没有 FaultID 字段，AlertID/
// IncidentID 均为空时 notifySendRefID/idempotencyKey 回退取 GroupKey 作为
// resourceKey，天然把"同一个 Fault 的同一次激活通知"收敛到同一个业务元组。
func faultActivatedGroupKey(faultID string) string {
	return "fault:" + faultID + ":activated"
}

// QueueFaultActivated 实现 R5 的五步判定（design.md「扩展点：internal/app/fault_outbox.go」）：
//  1. 冷却：同 correlation_key 在冷却窗口内刚 resolved，跳过并写时间线——active 的
//     Fault 没有通知任何人时，必须能从时间线查到原因，与"无通知目标"同一口径。
//  2. 拓扑抑制：Fault 的上游 provider 存在「active 且已通知」的 Fault，则不单独
//     通知，在本 Fault 时间线写明抑制原因与上游 Fault ID（不变量 22）。
//  3. 目标解析：owner team 的 team_ops_settings.channels；为空回落全局兜底渠道；
//     两者都缺失时不调度，但必须写时间线，不能静默失败（不变量 13 的"审计必须
//     可追溯"精神延伸到 Fault 通知场景）。
//  4. Schedule 一条（或每个渠道各一条）notify_send；round 在本次调用时刻一次性
//     捕获并随 payload 持久化，refID 不掺 round（不变量 4）——重复调用同一 Fault
//     只会替换同一条 pending 定时器，不会产生第二条。
//  5. 回写 owner_notified_at。
//
// 取舍说明（R5.6 原文是"通知成功发出后记录"）：这里落地为"通知意图已调度即记录"，
// 而不是等 notify_send 事务外执行确认发送成功后再回写——因为第 2 步的拓扑抑制
// 判定需要在"意图时刻"就能看到"上游已通知"，如果等真正发送成功才记录，多个下游
// Fault 在同一批次里几乎同时触发时会互相看不到对方已经产生的通知意图，导致抑制
// 失效、重复轰炸。发送最终失败由任务 40 的 notify_send 失败元告警兜底，不在这里
// 重试或回滚 owner_notified_at。
//
// 全程只在事务内做纯 DB 读写与一次 Schedule 调用，不出现任何同步外部 IO。
func (o *FaultOutbox) QueueFaultActivated(ctx context.Context, tx pgx.Tx, fault domain.Fault) error {
	if fault.ID == "" || fault.CorrelationKey == "" {
		return fmt.Errorf("app: Fault 通知意图字段非法")
	}
	now := o.now()

	// 1. 冷却窗口：同 correlation_key 刚 resolved，跳过本轮通知。
	if o.cooldown > 0 {
		recent, err := o.faults.ListRecentlyResolvedByCorrelationKey(ctx, tx, fault.CorrelationKey, now.Add(-o.cooldown))
		if err != nil {
			return fmt.Errorf("app: 查询 Fault 通知冷却窗口失败: %w", err)
		}
		if len(recent) > 0 {
			if _, err := o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
				FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySuppressed, At: now,
				Detail: map[string]any{"reason": "cooldown", "recently_resolved_fault_id": recent[0].ID},
			}); err != nil {
				return fmt.Errorf("app: 写入冷却抑制时间线失败: %w", err)
			}
			return nil
		}
	}

	// 2. 拓扑抑制：上游 provider 已有已通知的 active Fault。
	if fault.ResourceNodeID != "" && o.providers != nil {
		suppressed, upstreamID, err := o.suppressedByUpstream(ctx, tx, fault.ResourceNodeID)
		if err != nil {
			return err
		}
		if suppressed {
			if _, err := o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
				FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySuppressed, At: now,
				Detail: map[string]any{"reason": "upstream_already_notified", "upstream_fault_id": upstreamID},
			}); err != nil {
				return fmt.Errorf("app: 写入拓扑抑制时间线失败: %w", err)
			}
			return nil
		}
	}

	// 3. 目标解析：owner team 渠道；为空回落全局兜底；都缺失则写时间线、不调度。
	targets, err := o.resolveTargets(ctx, tx, fault)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		if _, err := o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
			FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySkipped, At: now,
			Detail: map[string]any{"reason": "no_notify_target"},
		}); err != nil {
			return fmt.Errorf("app: 写入无通知目标时间线失败: %w", err)
		}
		return nil
	}

	// 4. Schedule：round 在意图时刻一次性捕获，多渠道共享同一 round（同一次"进入
	// active"事件只应算一次逻辑发送意图，理由同 notifyOutbox.DispatchStep）。
	round := now.UnixNano()
	msg := buildFaultActivatedMessage(fault, now, o.webBaseURL)
	groupKey := faultActivatedGroupKey(fault.ID)
	for _, target := range targets {
		req := notify.SendRequest{
			GroupKey:    groupKey,
			ChannelKind: target.Kind,
			Target:      target.Target,
			Msg:         msg,
			Round:       round,
		}
		payload, err := marshalNotifySendRequest(req)
		if err != nil {
			return err
		}
		if err := o.scheduler.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(req), now, payload); err != nil {
			return fmt.Errorf("app: 注册 Fault notify_send 定时器失败: %w", err)
		}
	}

	// 5. 回写 owner_notified_at（语义取舍见函数顶部注释）。
	if err := o.faults.SetOwnerNotified(ctx, tx, fault.ID, now); err != nil {
		return fmt.Errorf("app: 记录 Fault owner 通知时间失败: %w", err)
	}
	return nil
}

// TriggerFromMappingResult 是 pipeline.MappingStage 与 alertsource.Reconciler 两个
// AlertProjector.Project 调用方共用的触发入口（R5.10/R5.11）：仅当 result.Transition
// 为 CreatedActive/EnteredActive 时才重新加载 Fault 并调用 QueueFaultActivated；
// 其余情况（已经是 active 的重复评估、未命中 Fault、或调用方——如 reprocess
// 端点——干脆不接入本方法）一律无副作用返回 nil。判定逻辑集中在这一处，两个
// 调用方所在的包都不需要各自实现一遍，也不需要感知 Fault 通知的任何细节。
func (o *FaultOutbox) TriggerFromMappingResult(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error {
	if result.FaultID == "" {
		return nil
	}
	if result.Transition != domain.FaultTransitionCreatedActive && result.Transition != domain.FaultTransitionEnteredActive {
		return nil
	}
	fault, err := o.faults.GetByID(ctx, tx, result.FaultID)
	if err != nil {
		return fmt.Errorf("app: 加载待通知 Fault 失败: %w", err)
	}
	return o.QueueFaultActivated(ctx, tx, fault)
}

// suppressedByUpstream 判断 consumerNodeID 的上游 provider 中是否存在「active 且
// owner_notified_at 非空」的 Fault；存在则返回该上游 Fault 的 ID 供写时间线引用。
func (o *FaultOutbox) suppressedByUpstream(ctx context.Context, tx pgx.Tx, consumerNodeID string) (bool, string, error) {
	providerIDs, err := o.providers.ProviderNodeIDs(ctx, tx, consumerNodeID)
	if err != nil {
		return false, "", fmt.Errorf("app: 查询上游 provider 失败: %w", err)
	}
	if len(providerIDs) == 0 {
		return false, "", nil
	}
	upstream, err := o.faults.ListOpenForNodeIDs(ctx, tx, providerIDs, 500)
	if err != nil {
		return false, "", fmt.Errorf("app: 查询上游 Fault 失败: %w", err)
	}
	for _, up := range upstream {
		if up.Status == domain.FaultStatusActive && !up.OwnerNotifiedAt.IsZero() {
			return true, up.ID, nil
		}
	}
	return false, "", nil
}

// resolveTargets 解析本次通知要发往的渠道列表：优先取 owner team 的
// team_ops_settings.channels（可能不止一个）；owner 未登记渠道（含团队从未保存过
// 运营配置，即 store.ErrNotFound）时回落全局兜底渠道；兜底渠道未配置时返回空列表，
// 调用方据此写"无可用通知目标"时间线而不是当作错误处理。
func (o *FaultOutbox) resolveTargets(ctx context.Context, tx pgx.Tx, fault domain.Fault) ([]store.OpsChannelTarget, error) {
	if fault.OwnerTeamID != "" {
		settings, err := o.ops.GetSettings(ctx, tx, fault.OwnerTeamID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("app: 查询 owner team 通知配置失败: %w", err)
		}
		if settings != nil && len(settings.Channels) > 0 {
			return settings.Channels, nil
		}
	}
	if o.fallbackChannel != "" && o.fallbackTarget != "" {
		return []store.OpsChannelTarget{{Kind: o.fallbackChannel, Target: o.fallbackTarget}}, nil
	}
	return nil, nil
}

// buildFaultActivatedMessage 组装 Fault 进入 active 的通知正文：标题含标题与严重度，
// 正文含实体、fault_kind、首次发现时间；措辞禁止出现"已恢复"一类表述（这是激活
// 通知，不是恢复通知，恢复外播是 R9 的 QueueImpactResolved，不在本任务范围）。
// webBaseURL 非空时附 Fault 详情链接，由三条通道（lark_webhook/lark/email）各自
// 渲染 Message.DetailURL。
func buildFaultActivatedMessage(fault domain.Fault, at time.Time, webBaseURL string) notify.Message {
	title := fmt.Sprintf("[%s] Fault 进入 active：%s", fault.Severity, fault.Title)
	body := fmt.Sprintf("实体: %s\n类型: %s\n首次发现: %s", fault.EntityRef, fault.Kind, fault.FirstDetected.Format(time.RFC3339))
	msg := notify.Message{
		Severity:            string(fault.Severity),
		Title:               title,
		Body:                body,
		OccurredAt:          at,
		ResourceType:        "fault",
		ResourceID:          fault.ID,
		ActionRequestPrefix: fmt.Sprintf("%s:%d", fault.ID, at.UnixNano()),
	}
	if webBaseURL != "" {
		msg.DetailURL = strings.TrimRight(webBaseURL, "/") + "/faults/" + fault.ID
	}
	return msg
}
