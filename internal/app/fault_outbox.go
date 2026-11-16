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

	deliveries *store.FaultNotificationRepo
	now        func() time.Time
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

// QueueFaultActivated 只持久化意图；owner_notified_at 由发送成功回调更新。
// 无配置的等待 SLA 时不等待 pending 上游；只按渠道接受且目标完全相同的记录抑制。
func (o *FaultOutbox) QueueFaultActivated(ctx context.Context, tx pgx.Tx, fault domain.Fault) error {
	return o.queue(ctx, tx, fault, false)
}
func (o *FaultOutbox) queue(ctx context.Context, tx pgx.Tx, fault domain.Fault, retry bool) error {
	if fault.ID == "" || fault.CorrelationKey == "" {
		return fmt.Errorf("app: Fault 通知意图字段非法")
	}
	if fault.Status != domain.FaultStatusActive || fault.IsAcked() {
		return nil
	}
	if o.deliveries != nil {
		if _, err := tx.Exec(ctx, `SELECT id FROM faults WHERE id=$1 FOR UPDATE`, fault.ID); err != nil {
			return err
		}
		current, err := o.faults.GetByID(ctx, tx, fault.ID)
		if err != nil {
			return err
		}
		fault = current
		if fault.Status != domain.FaultStatusActive || fault.IsAcked() {
			return nil
		}
	}
	now := o.now()
	epoch := fault.FirstDetected.UnixNano()
	if o.deliveries != nil {
		var err error
		epoch, err = o.deliveries.Enroll(ctx, tx, fault.ID, epoch)
		if err != nil {
			return err
		}
		orphaned, e := o.deliveries.Orphaned(ctx, tx, fault.ID)
		if e != nil {
			return e
		}
		for _, id := range orphaned {
			if _, e = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineAction("notify_failed"), At: now, Detail: map[string]any{"delivery_id": id, "reason": "orphaned_timer", "status": "failed"}}); e != nil {
				return e
			}
		}

	}
	reasonChanged := true
	deferWatch := func(reason string, at time.Time) error {
		if o.deliveries == nil {
			return nil
		}
		old, _, e := o.deliveries.Watch(ctx, tx, fault.ID)
		if e != nil {
			return e
		}
		reasonChanged = old != reason
		return o.deliveries.Defer(ctx, tx, fault.ID, reason, at)
	}
	decision, err := o.evaluate(ctx, tx, fault)
	if err != nil {
		return err
	}
	if o.cooldown > 0 {
		recent := decision.recent
		if len(recent) > 0 {
			at := recent[0].ResolvedAt.Add(o.cooldown)
			if at.Before(now) {
				at = now.Add(time.Minute)
			}
			if err = deferWatch("cooldown", at); err != nil {
				return err
			}
			if !reasonChanged {
				return nil
			}
			_, err = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySuppressed, At: now, Detail: map[string]any{"reason": "cooldown", "recently_resolved_fault_id": recent[0].ID}})
			return err
		}
	}
	targets := decision.targets
	if len(targets) == 0 {
		if err = deferWatch("no_notify_target", now.Add(time.Minute)); err != nil {
			return err
		}
		if !reasonChanged {
			return nil
		}
		_, err = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySkipped, At: now, Detail: map[string]any{"reason": "no_notify_target"}})
		return err
	}
	upstreamIDs := decision.upstream
	for _, target := range targets {
		req := notify.SendRequest{GroupKey: faultActivatedGroupKey(fault.ID), ChannelKind: target.Kind, Target: target.Target, Msg: buildFaultActivatedMessage(fault, now, o.webBaseURL), Round: epoch}
		if o.deliveries != nil {
			hash := store.NotificationTargetHash(target.Kind, target.Target)
			covered, e := o.deliveries.Covered(ctx, tx, upstreamIDs, target.Kind, hash)
			if e != nil {
				return e
			}
			status := "queued"
			if covered {
				status = "covered"
			}
			d, created, e := o.deliveries.Ensure(ctx, tx, fault.ID, epoch, target.Kind, hash, status)
			if e != nil {
				return e
			}
			if !created && d.Status == "covered" && !covered {
				created, e = o.deliveries.ReleaseCoverage(ctx, tx, d.ID)
				if e != nil {
					return e
				}
				d.Status = "queued"
			}
			if !created {
				if !retry || d.Status != "failed" {
					continue
				}
				ok, e := o.deliveries.Retry(ctx, tx, d.ID)
				if e != nil {
					return e
				}
				if !ok {
					continue
				}
				d.Generation++
			}
			if d.Status == "covered" {
				if created {
					if _, e = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: fault.ID, Actor: "system", Action: domain.FaultTimelineActionNotifySuppressed, At: now, Detail: map[string]any{"reason": "upstream_sent_target_coverage", "upstream_fault_ids": upstreamIDs, "channel": target.Kind, "target": "[redacted]", "delivery_id": d.ID}}); e != nil {
						return e
					}
				}
				continue
			}
			req.FaultNotificationID = d.ID
			req.FaultNotificationGeneration = d.Generation
			req.Round = epoch + int64(d.Generation)
		}
		payload, e := marshalNotifySendRequest(req)
		if e != nil {
			return e
		}
		if e = o.scheduler.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(req), now, payload); e != nil {
			return e
		}
	}
	return deferWatch("reconcile", now.Add(time.Minute))
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
	if o.deliveries != nil && result.Transition == domain.FaultTransitionEnteredActive {
		if err = o.deliveries.Activate(ctx, tx, fault.ID, o.now().UnixNano()); err != nil {
			return err
		}
	}
	return o.QueueFaultActivated(ctx, tx, fault)
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
