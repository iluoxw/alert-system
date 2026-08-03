package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

// IncidentOutbox 把 Incident 状态通报持久化为 notify_send 定时器。调用方必须在
// 创建/推进 Incident 的同一个数据库事务中调用它：这样 Incident 与“需要发出一条
// 通报”的意图要么一起提交，要么一起回滚，不会出现状态已变但通知意图丢失。
type IncidentOutbox struct {
	scheduler notifyOutboxScheduler
	now       func() time.Time
}

// QueueIncidentNotification 实现 incident.Service 的可靠通知接口。实际网络 IO 由
// scheduler 提交事务后异步调用 notify.Sender 完成；这里绝不在业务事务内发请求。
func (o *IncidentOutbox) QueueIncidentNotification(ctx context.Context, tx pgx.Tx, notification domain.IncidentNotification) error {
	incident := notification.Incident
	if incident.ID == "" || incident.CommunicationChannel == "" || incident.CommunicationTargetRef == "" || notification.OccurredAt.IsZero() {
		return fmt.Errorf("app: Incident 通报字段非法")
	}

	statusText := map[domain.IncidentNotificationEvent]string{
		domain.IncidentNotificationDeclared:  "已宣布",
		domain.IncidentNotificationMitigated: "已缓解，进入观察",
		domain.IncidentNotificationResolved:  "已解决",
	}[notification.Event]
	if statusText == "" {
		return fmt.Errorf("app: 未知 Incident 通报事件 %q", notification.Event)
	}

	request := notify.SendRequest{
		IncidentID:  incident.ID,
		ChannelKind: incident.CommunicationChannel,
		Target:      incident.CommunicationTargetRef,
		Msg: notify.Message{
			Severity: string(incident.Severity),
			Title:    fmt.Sprintf("[%s] %s：%s", incident.DisplayID(), statusText, incident.Title),
			Body:     incident.Summary,
			Labels: map[string]string{
				"incident_id":         incident.ID,
				"incident_display_id": incident.DisplayID(),
				"incident_status":     string(incident.Status),
				"incident_event":      string(notification.Event),
			},
			OccurredAt: notification.OccurredAt,
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
