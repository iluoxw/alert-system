package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

// IncidentRecoveryNudgeTimerKind 是恢复提醒定时器的 kind（第 12 个 timer kind），
// refID 固定用 Incident ID。内部命名与文案统一用"解除"而非"恢复"：证据告警
// resolved 可能只是采集目标下线或服务发现注销导致的假解除，不代表故障真的恢复
// （2026-09 线上 E2BNomadAgentDown 假解除误报教训），命名不能替事故指挥下这个结论。
const IncidentRecoveryNudgeTimerKind = "incident_recovery_nudge"

// maxRecoveryNudgeEvidenceListed 是提醒文案里逐条列出的证据告警上限，超出部分
// 折叠成"…另有 N 条"，避免证据很多时文案失控。
const maxRecoveryNudgeEvidenceListed = 5

// IncidentOutbox 把 Incident 状态通报持久化为 notify_send 定时器，并管理
// incident_recovery_nudge 定时器的调度/取消。调用方必须在创建/推进 Incident 的
// 同一个数据库事务中调用它：这样 Incident 与"需要发出一条通报"的意图要么一起
// 提交，要么一起回滚，不会出现状态已变但通知意图丢失。
type IncidentOutbox struct {
	scheduler     notifyOutboxScheduler
	nudgeInterval time.Duration // incident_recovery_nudge 轮询周期；<=0 表示关闭
	now           func() time.Time
}

// ScheduleRecoveryNudge 实现 incident.Service 的 NotificationOutbox 接口：仅当
// nudgeInterval>0 时才 ScheduleIfAbsent 一条 incident_recovery_nudge 定时器
// （已存在 pending 记录时不重置倒计时——语义与 ops 三个周期任务一致）。
func (o *IncidentOutbox) ScheduleRecoveryNudge(ctx context.Context, tx pgx.Tx, incidentID string) error {
	if o.nudgeInterval <= 0 {
		return nil
	}
	if _, err := o.scheduler.ScheduleIfAbsent(ctx, tx, IncidentRecoveryNudgeTimerKind, incidentID, o.now().Add(o.nudgeInterval), nil); err != nil {
		return fmt.Errorf("app: 注册 Incident 恢复提醒定时器失败: %w", err)
	}
	return nil
}

// CancelRecoveryNudge 实现 incident.Service 的 NotificationOutbox 接口：Incident
// 推进到 resolved 时调用，取消 pending 的恢复提醒定时器。Cancel 本身是幂等的
// （定时器不存在/已关闭时 0 行受影响、不报错），因此关闭时无条件调用，不需要先
// 判断 nudgeInterval 是否曾经启用过。
func (o *IncidentOutbox) CancelRecoveryNudge(ctx context.Context, tx pgx.Tx, incidentID string) error {
	if err := o.scheduler.Cancel(ctx, tx, IncidentRecoveryNudgeTimerKind, incidentID); err != nil {
		return fmt.Errorf("app: 取消 Incident 恢复提醒定时器失败: %w", err)
	}
	return nil
}

// QueueIncidentNotification 实现 incident.Service 的可靠通知接口。实际网络 IO 由
// scheduler 提交事务后异步调用 notify.Sender 完成；这里绝不在业务事务内发请求。
func (o *IncidentOutbox) QueueIncidentNotification(ctx context.Context, tx pgx.Tx, notification domain.IncidentNotification) error {
	incident := notification.Incident
	if incident.ID == "" || incident.CommunicationChannel == "" || incident.CommunicationTargetRef == "" || notification.OccurredAt.IsZero() {
		return fmt.Errorf("app: Incident 通报字段非法")
	}

	statusText := map[domain.IncidentNotificationEvent]string{
		domain.IncidentNotificationDeclared:      "已宣布",
		domain.IncidentNotificationMitigated:     "已缓解，进入观察",
		domain.IncidentNotificationResolved:      "已解决",
		domain.IncidentNotificationRecoveryNudge: "证据告警已全部解除",
	}[notification.Event]
	if statusText == "" {
		return fmt.Errorf("app: 未知 Incident 通报事件 %q", notification.Event)
	}

	body := incident.Summary
	if notification.Event == domain.IncidentNotificationRecoveryNudge {
		if notification.RecoveryNudge == nil {
			return fmt.Errorf("app: Incident 恢复提醒缺少判定详情")
		}
		body = buildRecoveryNudgeBody(incident, *notification.RecoveryNudge, notification.OccurredAt)
	}

	request := notify.SendRequest{
		IncidentID:  incident.ID,
		ChannelKind: incident.CommunicationChannel,
		Target:      incident.CommunicationTargetRef,
		Msg: notify.Message{
			Severity: string(incident.Severity),
			Title:    fmt.Sprintf("[%s] %s：%s", incident.DisplayID(), statusText, incident.Title),
			Body:     body,
			Labels: map[string]string{
				"incident_id":         incident.ID,
				"incident_display_id": incident.DisplayID(),
				"incident_status":     string(incident.Status),
				"incident_event":      string(notification.Event),
			},
			OccurredAt:   notification.OccurredAt,
			ResourceType: "incident", ResourceID: incident.ID,
			ActionRequestPrefix: fmt.Sprintf("%s:%d", incident.ID, notification.OccurredAt.UnixNano()),
		},
		Round: notification.OccurredAt.UnixNano(),
	}
	payload, err := marshalNotifySendRequest(request)
	if err != nil {
		return err
	}
	if err := o.scheduler.Schedule(ctx, tx, notifySendTimerKind, notifySendRefID(request), o.now(), payload); err != nil {
		return fmt.Errorf("app: 注册 Incident notify_send 定时器失败: %w", err)
	}
	return nil
}

func marshalNotifySendRequest(request notify.SendRequest) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("app: 序列化 Incident 通报失败: %w", err)
	}
	return payload, nil
}

// buildRecoveryNudgeBody 渲染恢复提醒文案：当前状态与持续时长、证据全部解除的
// 时间点、人工确认提示、证据详情（最多列 5 条）、以及关闭门槛还差什么——让事故
// 指挥看一眼就知道下一步，而不是被动收到一句"已恢复"就直接推进 Incident。
func buildRecoveryNudgeBody(incident domain.Incident, detail domain.IncidentRecoveryNudgeDetail, at time.Time) string {
	lines := []string{
		fmt.Sprintf("当前状态：%s（已持续 %s）", string(incident.Status), formatNudgeDuration(at.Sub(currentStatusSince(incident)))),
		fmt.Sprintf("证据告警已于 %s 全部解除。", detail.LatestClearedAt.Format(time.RFC3339)),
		// 固定提示：证据解除不等于故障恢复（可能是采集目标下线/服务发现注销造成
		// 的假解除），禁止让指挥误以为系统已经判定"已恢复"。
		"告警解除可能源于采集目标下线或服务发现注销，请人工确认实际恢复后再推进 Incident。",
		"证据详情：" + formatClearedEvidence(detail.ClearedEvidence),
		"关闭门槛：" + formatNudgeGateSummary(detail),
	}
	return strings.Join(lines, "\n")
}

// currentStatusSince 返回 Incident 进入当前状态的时刻。文案说的是"当前状态已持续
// 多久"，若一律用 DeclaredAt，monitoring 挂了 4 天的事故会显示宣布以来的总时长，
// 掩盖"卡在观察期无人收尾"这个真正要提醒的信号。时间戳缺失时回落 DeclaredAt。
func currentStatusSince(incident domain.Incident) time.Time {
	var since time.Time
	switch incident.Status {
	case domain.IncidentStatusMonitoring:
		since = incident.MonitoringAt
	case domain.IncidentStatusMitigating:
		since = incident.MitigatedAt
	}
	if since.IsZero() {
		return incident.DeclaredAt
	}
	return since
}

// formatNudgeDuration 把当前状态持续时长取整到分钟展示，提醒文案只需要
// 给指挥一个量级感知（"已经挂了多久没人收尾"），不需要精确到秒。
func formatNudgeDuration(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Round(time.Minute).String()
}

// formatClearedEvidence 逐条列出已解除的证据告警（alertname + 解除时间），最多
// maxRecoveryNudgeEvidenceListed 条，超出以"…另有 N 条"收尾，供指挥判断这批
// "解除"是否可信，而不是无条件当作已恢复的证明。
func formatClearedEvidence(items []domain.IncidentRecoveryNudgeEvidence) string {
	if len(items) == 0 {
		return "无"
	}
	shown := items
	suffix := ""
	if len(items) > maxRecoveryNudgeEvidenceListed {
		shown = items[:maxRecoveryNudgeEvidenceListed]
		suffix = fmt.Sprintf("，另有 %d 条", len(items)-maxRecoveryNudgeEvidenceListed)
	}
	parts := make([]string, 0, len(shown))
	for _, item := range shown {
		parts = append(parts, fmt.Sprintf("%s（解除于 %s）", item.AlertName, item.ClearedAt.Format(time.RFC3339)))
	}
	return strings.Join(parts, "、") + suffix
}

// formatNudgeGateSummary 汇总"距离下一道关闭门槛还差什么"：未收口任务数、未排除
// 的活跃关联 Fault 数、尚未通过的必需判据——三者任一非零都说明还不能推进 Incident。
func formatNudgeGateSummary(detail domain.IncidentRecoveryNudgeDetail) string {
	parts := []string{
		fmt.Sprintf("待收口任务 %d 个", detail.OpenTaskCount),
		fmt.Sprintf("未排除的活跃关联 Fault %d 个", detail.ActiveIncludedFaultCount),
	}
	if len(detail.UnmetRequiredCriteria) > 0 {
		parts = append(parts, "尚未通过的必需判据："+strings.Join(detail.UnmetRequiredCriteria, "、"))
	}
	return strings.Join(parts, "；")
}
