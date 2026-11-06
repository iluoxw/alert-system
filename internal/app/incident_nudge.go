package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// incidentNudgeScheduler 是 incidentNudgeHandler 自续期依赖的窄接口，由
// *scheduler.Scheduler 实现——与 retentionScheduler 同样的收拢手法。
type incidentNudgeScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
}

// incidentNudgeHandler 实现 incident_recovery_nudge 定时器（第 12 个 timer kind）：
// 判断一个仍 open 的 Incident 是否所有 included Fault 的有效证据告警都已解除，
// 是则入队一条恢复提醒；无论发不发都在同一事务内续挂下一轮。
//
// 只提醒、绝不自动推进 Incident 状态——不变量 11 要求 Incident 必须人工宣布/
// 推进，这里的"证据全部解除"只是提示信号，不是自动化判据。
type incidentNudgeHandler struct {
	incidents *store.IncidentRepo
	faults    *store.FaultRepo
	alerts    *store.AlertRepo
	topology  *store.TopologyRepo
	outbox    *IncidentOutbox
	sched     incidentNudgeScheduler
	interval  time.Duration

	// now 默认 time.Now，测试注入可控时钟精确断言"何时判定解除、下次何时续期"。
	now func() time.Time
}

func newIncidentNudgeHandler(incidents *store.IncidentRepo, faults *store.FaultRepo, alerts *store.AlertRepo, topology *store.TopologyRepo, outbox *IncidentOutbox, sched incidentNudgeScheduler, interval time.Duration) *incidentNudgeHandler {
	return &incidentNudgeHandler{
		incidents: incidents, faults: faults, alerts: alerts, topology: topology,
		outbox: outbox, sched: sched, interval: interval, now: time.Now,
	}
}

// Handle 实现 scheduler.Handler：整个判定 + 入队通知 + 续期都在 scheduler 传入的
// 同一个事务内完成（不变量 3：handler 绝不能自开事务），因此对 at-least-once
// 重投天然安全——某次重投要么完整生效，要么随事务整体回滚重来，不会出现"发了
// 一半"的中间态。
func (h *incidentNudgeHandler) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	if h.interval <= 0 {
		// 配置关闭：不续挂。若运行中途关闭，pending 的这一条会在触发时静默消失，
		// 与"未启用时从不调度"的语义一致，不需要额外的取消路径。
		return nil
	}

	incident, err := h.incidents.GetByID(ctx, tx, refID)
	if errors.Is(err, store.ErrIncidentNotFound) {
		return nil // 历史脏定时器（Incident 已被物理删除，当前系统无此路径但仍兜底）
	}
	if err != nil {
		return err
	}
	if !incident.Status.IsOpen() {
		// 正常应该已经被 CancelRecoveryNudge 取消；这里是兜底（例如 Cancel 与本次
		// 触发发生竞争的极端时序），已关闭的 Incident 不再续挂、不再提醒。
		return nil
	}

	now := h.now()
	cleared, haveEvidence, err := h.collectClearedEvidence(ctx, tx, refID)
	if err != nil {
		return err
	}
	if haveEvidence && len(cleared.items) == len(cleared.total) {
		detail, err := h.buildNudgeDetail(ctx, tx, incident, cleared)
		if err != nil {
			return err
		}
		if err := h.outbox.QueueIncidentNotification(ctx, tx, domain.IncidentNotification{
			Incident: incident, Event: domain.IncidentNotificationRecoveryNudge,
			OccurredAt: now, RecoveryNudge: &detail,
		}); err != nil {
			return err
		}
	}

	return h.sched.Schedule(ctx, tx, IncidentRecoveryNudgeTimerKind, refID, now.Add(h.interval), nil)
}

// clearedEvidenceResult 区分"全部有效证据"（total，用于判断 haveEvidence 与
// all-cleared）与"已解除的那部分"（items，用于渲染文案），两者长度相等才代表
// 全部解除——排除人工判为 excluded 的证据后一条都不剩时不算"有证据"，不触发提醒
// （没有证据支撑就不该暗示"可以收尾了"）。
type clearedEvidenceResult struct {
	total []domain.FaultEvidence
	items []domain.IncidentRecoveryNudgeEvidence
}

// collectClearedEvidence 汇总所有 disposition=included 的关联 Fault 的有效证据
// 告警（排除人工判为 excluded 的证据角色），并区分出其中已解除的那部分。
// Alertmanager 权威来源看 source_state=resolved，其他来源看 status=resolved
// （不变量 14：两套生命周期轴含义不同，不能混用同一个字段判断）。
func (h *incidentNudgeHandler) collectClearedEvidence(ctx context.Context, tx pgx.Tx, incidentID string) (clearedEvidenceResult, bool, error) {
	links, err := h.incidents.ListFaultLinks(ctx, tx, incidentID)
	if err != nil {
		return clearedEvidenceResult{}, false, err
	}
	var result clearedEvidenceResult
	for _, link := range links {
		if link.Disposition != domain.IncidentFaultIncluded {
			continue
		}
		evidence, err := h.faults.ListEvidence(ctx, tx, link.FaultID)
		if err != nil {
			return clearedEvidenceResult{}, false, err
		}
		for _, ev := range evidence {
			if ev.Role == domain.EvidenceRoleExcluded {
				continue
			}
			result.total = append(result.total, ev)
			alert, err := h.alerts.GetByID(ctx, tx, ev.AlertID)
			if err != nil {
				return clearedEvidenceResult{}, false, err
			}
			if !alertCleared(alert) {
				continue
			}
			result.items = append(result.items, domain.IncidentRecoveryNudgeEvidence{
				AlertName: alertDisplayName(alert), ClearedAt: alertClearedAt(alert),
			})
		}
	}
	return result, len(result.total) > 0, nil
}

// buildNudgeDetail 补齐提醒文案需要的门槛快照：最晚解除时间、未收口任务数、
// 尚未通过的必需判据、未排除的活跃关联 Fault 数。
func (h *incidentNudgeHandler) buildNudgeDetail(ctx context.Context, tx pgx.Tx, incident domain.Incident, cleared clearedEvidenceResult) (domain.IncidentRecoveryNudgeDetail, error) {
	detail := domain.IncidentRecoveryNudgeDetail{ClearedEvidence: cleared.items}
	for _, item := range cleared.items {
		if item.ClearedAt.After(detail.LatestClearedAt) {
			detail.LatestClearedAt = item.ClearedAt
		}
	}

	tasks, err := h.incidents.ListTasks(ctx, tx, incident.ID)
	if err != nil {
		return domain.IncidentRecoveryNudgeDetail{}, err
	}
	for _, task := range tasks {
		if task.Status != domain.IncidentTaskDone && task.Status != domain.IncidentTaskCanceled {
			detail.OpenTaskCount++
		}
	}

	criteria, err := h.incidents.ListRecoveryCriteria(ctx, tx, incident.ID)
	if err != nil {
		return domain.IncidentRecoveryNudgeDetail{}, err
	}
	detail.UnmetRequiredCriteria = domain.UnmetRequiredCriteria(incident.Status, criteria)

	links, err := h.incidents.ListFaultLinks(ctx, tx, incident.ID)
	if err != nil {
		return domain.IncidentRecoveryNudgeDetail{}, err
	}
	nodeIDs := make([]string, 0)
	seenNode := map[string]struct{}{}
	for _, link := range links {
		if link.Disposition != domain.IncidentFaultIncluded {
			continue
		}
		fault, err := h.faults.GetByID(ctx, tx, link.FaultID)
		if err != nil {
			return domain.IncidentRecoveryNudgeDetail{}, err
		}
		if fault.Status == domain.FaultStatusActive {
			detail.ActiveIncludedFaultCount++
		}
		if fault.ResourceNodeID == "" {
			continue
		}
		if _, exists := seenNode[fault.ResourceNodeID]; exists {
			continue
		}
		seenNode[fault.ResourceNodeID] = struct{}{}
		nodeIDs = append(nodeIDs, fault.ResourceNodeID)
	}
	if len(nodeIDs) > 0 && h.topology != nil {
		allDecommissioned := true
		for _, nodeID := range nodeIDs {
			node, err := h.topology.GetByID(ctx, tx, nodeID)
			if err != nil {
				return domain.IncidentRecoveryNudgeDetail{}, err
			}
			if node.LifecycleState != domain.EntityLifecycleDecommissioned {
				allDecommissioned = false
				break
			}
		}
		detail.AllEvidenceEntitiesDecommissioned = allDecommissioned
	}
	return detail, nil
}

// alertCleared 判断一条证据告警是否已解除：权威来源（Alertmanager 镜像）看
// SourceState，其余来源看本地 Status（不变量 14）。命名故意用"解除"而非
// "恢复"——见文件头注释。
func alertCleared(alert domain.Alert) bool {
	if alert.IsAuthoritativeSource() {
		return alert.SourceState == domain.SourceStateResolved
	}
	return alert.Status == domain.StatusResolved
}

// alertClearedAt 取告警解除时间：优先用上游携带的 EndsAt，缺失时退化为 UpdatedAt
// （兼容来源没有精确 EndsAt 的情况）。
func alertClearedAt(alert domain.Alert) time.Time {
	if !alert.EndsAt.IsZero() {
		return alert.EndsAt
	}
	return alert.UpdatedAt
}

// alertDisplayName 优先取 alertname 标签（Alertmanager/Prometheus 惯例），缺失时
// 退化为 Title，保证提醒文案里每条证据都有可读名称。
func alertDisplayName(alert domain.Alert) string {
	if name, ok := alert.Labels["alertname"]; ok && name != "" {
		return name
	}
	return alert.Title
}

// ReconcileIncidentNudgeTimers 是启动对账入口，与 ReconcileOpsTimers 同一模式：
// 用 ScheduleIfAbsent 补建所有 open Incident 缺失的 pending 定时器，不打断正在
// 倒计时的既有记录。interval<=0（关闭）时直接跳过，不需要查表。
func ReconcileIncidentNudgeTimers(ctx context.Context, st *store.Store, incidents *store.IncidentRepo, sched *scheduler.Scheduler, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	return st.WithTx(ctx, func(tx pgx.Tx) error {
		ids, err := incidents.ListOpenIDs(ctx, tx)
		if err != nil {
			return fmt.Errorf("app: 查询 open Incident 列表失败: %w", err)
		}
		now := time.Now()
		for _, id := range ids {
			if _, err := sched.ScheduleIfAbsent(ctx, tx, IncidentRecoveryNudgeTimerKind, id, now.Add(interval), nil); err != nil {
				return fmt.Errorf("app: 对账 incident_recovery_nudge 定时器失败(incident=%s): %w", id, err)
			}
		}
		return nil
	})
}
