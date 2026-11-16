package app

import (
	"context"
	"encoding/json"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
	"github.com/jackc/pgx/v5"
)

const FaultNotificationReconcileKind = "fault_notification_reconcile"

// 一分钟仅为 DB 批次扫描频率，不代表通知响应 SLA。每批最多 100 个已登记 Fault。
func (o *FaultOutbox) HandleReconcile(ctx context.Context, tx pgx.Tx, _ string, _ []byte) error {
	ids, e := o.deliveries.Due(ctx, tx, o.now())
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = tx.Exec(ctx, "SAVEPOINT fault_notify_item"); e != nil {
			return e
		}
		f, itemErr := o.faults.GetByID(ctx, tx, id)
		if itemErr == nil {
			itemErr = o.QueueFaultActivated(ctx, tx, f)
		}
		if itemErr != nil {
			if _, e = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT fault_notify_item"); e != nil {
				return e
			}
			if e = o.deliveries.Defer(ctx, tx, id, "reconcile_error", o.now().Add(time.Minute)); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "RELEASE SAVEPOINT fault_notify_item"); e != nil {
			return e
		}
	}
	return o.scheduler.Schedule(ctx, tx, FaultNotificationReconcileKind, "global", o.now().Add(time.Minute), nil)
}
func ReconcileFaultNotificationTimer(ctx context.Context, st *store.Store, sched *scheduler.Scheduler) error {
	return st.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := sched.ScheduleIfAbsent(ctx, tx, FaultNotificationReconcileKind, "global", time.Now(), nil)
		return e
	})
}
func (o *FaultOutbox) BeforeSend(ctx context.Context, tx pgx.Tx, req notify.SendRequest) (bool, error) {
	return o.deliveries.Begin(ctx, tx, req.FaultNotificationID, req.FaultNotificationGeneration)
}
func (o *FaultOutbox) RecordResult(ctx context.Context, tx pgx.Tx, req notify.SendRequest, status string) error {
	id, wrote, e := o.deliveries.Result(ctx, tx, req.FaultNotificationID, req.FaultNotificationGeneration, status)
	if e != nil || !wrote {
		return e
	}
	if status == "sent" {
		if e = o.faults.SetOwnerNotified(ctx, tx, id, o.now()); e != nil {
			return e
		}
	}
	_, e = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: id, Actor: "notify-sender", Action: domain.FaultTimelineAction("notify_" + status), At: o.now(), Detail: map[string]any{"delivery_id": req.FaultNotificationID, "channel": req.ChannelKind, "target": "[redacted]", "status": status, "generation": req.FaultNotificationGeneration}})
	return e
}

type FaultNotificationPreview struct {
	WatchReason string                            `json:"watch_reason,omitempty"`
	NextCheckAt time.Time                         `json:"next_check_at,omitempty"`
	FaultID     string                            `json:"fault_id"`
	Eligible    bool                              `json:"eligible"`
	Reason      string                            `json:"reason"`
	Targets     []FaultNotificationPreviewTarget  `json:"targets"`
	Deliveries  []store.FaultNotificationDelivery `json:"deliveries"`
}
type FaultNotificationPreviewTarget struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
	Action  string `json:"action"`
}

// Preview 不登记 watch、不写 timer/timeline。提交总是重新加载当前状态与渠道。
func (o *FaultOutbox) Preview(ctx context.Context, tx pgx.Tx, id string, retry bool) (FaultNotificationPreview, error) {
	p := FaultNotificationPreview{FaultID: id, Targets: []FaultNotificationPreviewTarget{}}
	f, e := o.faults.GetByID(ctx, tx, id)
	if e != nil {
		return p, e
	}
	epoch, e := o.deliveries.Epoch(ctx, tx, id, f.FirstDetected.UnixNano())
	if e != nil {
		return p, e
	}
	p.WatchReason, p.NextCheckAt, e = o.deliveries.Watch(ctx, tx, id)
	if e != nil {
		return p, e
	}
	p.Deliveries, e = o.deliveries.List(ctx, tx, id)
	if e != nil {
		return p, e
	}
	if f.Status != domain.FaultStatusActive {
		p.Reason = "not_active"
		return p, nil
	}
	if f.IsAcked() {
		p.Reason = "acknowledged"
		return p, nil
	}
	decision, e := o.evaluate(ctx, tx, f)
	targets := decision.targets
	if e != nil {
		return p, e
	}
	p.Eligible = true
	p.Reason = "ready"
	if len(targets) == 0 {
		p.Reason = "no_notify_target"
	}
	upstream := decision.upstream
	for _, t := range targets {
		action := "queue"
		hash := store.NotificationTargetHash(t.Kind, t.Target)
		covered, e := o.deliveries.Covered(ctx, tx, upstream, t.Kind, hash)
		if e != nil {
			return p, e
		}
		if covered {
			action = "covered"
		}
		for _, d := range p.Deliveries {
			if d.Epoch == epoch && d.Channel == t.Kind && d.TargetHash == hash {
				action = d.Status
				if action == "queued" || action == "sending" {
					live, e := o.deliveries.HasLiveTimer(ctx, tx, d.ID, d.Generation)
					if e != nil {
						return p, e
					}
					if !live {
						action = "failed"
						p.Reason = "orphaned_timer"
					}
				}
				if action == "covered" && !covered {
					action = "queue"
				}
				if retry && action == "failed" && d.Generation < 2 {
					action = "retry"
				}
			}
		}
		p.Targets = append(p.Targets, FaultNotificationPreviewTarget{Channel: t.Kind, Target: "[redacted]", Action: action})
	}
	if len(decision.recent) > 0 {
		p.Reason = "cooldown"
		for i := range p.Targets {
			if p.Targets[i].Action == "queue" || p.Targets[i].Action == "retry" {
				p.Targets[i].Action = "wait_cooldown"
			}
		}
	}
	return p, nil
}
func (o *FaultOutbox) Submit(ctx context.Context, tx pgx.Tx, id string, retry bool, actorID, actor string) (FaultNotificationPreview, error) {
	// 与 ACK/投影串行；所有读写均沿用调用方事务，禁止在这里自开事务。
	if _, e := tx.Exec(ctx, `SELECT id FROM faults WHERE id=$1 FOR UPDATE`, id); e != nil {
		return FaultNotificationPreview{}, e
	}
	f, e := o.faults.GetByID(ctx, tx, id)
	if e != nil {
		return FaultNotificationPreview{}, e
	}
	if f.Status == domain.FaultStatusActive && !f.IsAcked() {
		beforeReason, _, e := o.deliveries.Watch(ctx, tx, id)
		if e != nil {
			return FaultNotificationPreview{}, e
		}
		before, e := o.deliveries.List(ctx, tx, id)
		if e != nil {
			return FaultNotificationPreview{}, e
		}
		if e = o.queue(ctx, tx, f, retry); e != nil {
			return FaultNotificationPreview{}, e
		}
		after, e := o.deliveries.List(ctx, tx, id)
		if e != nil {
			return FaultNotificationPreview{}, e
		}
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(after)
		afterReason, _, e := o.deliveries.Watch(ctx, tx, id)
		if e != nil {
			return FaultNotificationPreview{}, e
		}
		if string(a) != string(b) || beforeReason != afterReason {
			_, e = o.faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{FaultID: id, ActorID: actorID, Actor: actor, Action: domain.FaultTimelineAction("notify_backfill"), At: o.now(), Detail: map[string]any{"retry_failed": retry}})
			if e != nil {
				return FaultNotificationPreview{}, e
			}
		}
	}
	return o.Preview(ctx, tx, id, retry)
}

func (o *FaultOutbox) upstreamFaultIDs(ctx context.Context, tx pgx.Tx, f domain.Fault) ([]string, error) {
	out := []string{}
	if o.deliveries == nil || o.providers == nil || f.ResourceNodeID == "" {
		return out, nil
	}
	nodes, e := o.providers.ProviderNodeIDs(ctx, tx, f.ResourceNodeID)
	if e != nil {
		return nil, e
	}
	upstream, e := o.faults.ListOpenForNodeIDs(ctx, tx, nodes, 500)
	if e != nil {
		return nil, e
	}
	for _, up := range upstream {
		if up.ID != f.ID && up.Status == domain.FaultStatusActive {
			out = append(out, up.ID)
		}
	}
	return out, nil
}

// evaluate 是预览和提交共用的只读决策，禁止在此登记watch或写审计。
type faultNotificationDecision struct {
	targets  []store.OpsChannelTarget
	recent   []domain.Fault
	upstream []string
}

func (o *FaultOutbox) evaluate(ctx context.Context, tx pgx.Tx, f domain.Fault) (faultNotificationDecision, error) {
	d := faultNotificationDecision{}
	var e error
	d.targets, e = o.resolveTargets(ctx, tx, f)
	if e != nil {
		return d, e
	}
	d.upstream, e = o.upstreamFaultIDs(ctx, tx, f)
	if e != nil {
		return d, e
	}
	if o.cooldown > 0 {
		d.recent, e = o.faults.ListRecentlyResolvedByCorrelationKey(ctx, tx, f.CorrelationKey, o.now().Add(-o.cooldown))
	}
	return d, e
}
