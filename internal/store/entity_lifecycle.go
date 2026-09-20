package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// EntityLifecyclePreview 是下线前置检查的一次查询结果。集合字段由构造函数保证非 nil。
type EntityLifecyclePreview struct {
	ActiveFaults  []domain.Fault
	OpenIncidents []domain.Incident
	FiringAlerts  []domain.Alert
}

func emptyLifecyclePreview() EntityLifecyclePreview {
	return EntityLifecyclePreview{
		ActiveFaults:  []domain.Fault{},
		OpenIncidents: []domain.Incident{},
		FiringAlerts:  []domain.Alert{},
	}
}

// PreviewEntityLifecycle 用三条批量查询拼出 R3.1 清单，避免按 Fault 逐条 N+1。
func PreviewEntityLifecycle(ctx context.Context, tx pgx.Tx, faults *FaultRepo, incidents *IncidentRepo, alerts *AlertRepo, nodeID string) (EntityLifecyclePreview, error) {
	preview := emptyLifecyclePreview()
	open, err := faults.ListOpenRelatedToNode(ctx, tx, nodeID, 100)
	if err != nil {
		return preview, err
	}
	preview.ActiveFaults = open
	ids := make([]string, 0, len(open))
	for _, fault := range open {
		ids = append(ids, fault.ID)
	}
	openIncidents, err := incidents.ListOpenForFaultIDs(ctx, tx, ids)
	if err != nil {
		return preview, err
	}
	preview.OpenIncidents = openIncidents
	firing, err := alerts.ListFiringEvidenceForFaultIDs(ctx, tx, ids)
	if err != nil {
		return preview, err
	}
	preview.FiringAlerts = firing
	return preview, nil
}

// CascadeFaultsForDecommission 把关联 active/monitoring Fault 推到 resolved 并写
// 时间线；Incident 只追加说明，不改状态。已 resolved 的 Fault 不再写时间线。
// 不产生 notify_send：下线收口不是恢复，也不是 Fault 进入 active。
func CascadeFaultsForDecommission(
	ctx context.Context, tx pgx.Tx, faults *FaultRepo, incidents *IncidentRepo,
	node domain.ResourceNode, actorID, actorName, reason string, at time.Time,
) ([]domain.Fault, error) {
	open, err := faults.ListOpenRelatedToNode(ctx, tx, node.ID, 100)
	if err != nil {
		return nil, err
	}
	cascaded := make([]domain.Fault, 0)
	relatedIDs := make([]string, 0, len(open))
	for _, fault := range open {
		relatedIDs = append(relatedIDs, fault.ID)
		resolved, changed, err := faults.ResolveByDecommission(ctx, tx, fault.ID, at)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if _, err := faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
			FaultID: resolved.ID,
			ActorID: actorID,
			Actor:   actorName,
			Action:  domain.FaultTimelineActionEntityDecommissioned,
			At:      at,
			Detail: map[string]any{
				"entity_id":   node.ID,
				"entity_key":  node.EntityKey,
				"reason":      reason,
				"from_status": string(fault.Status),
			},
		}); err != nil {
			return nil, err
		}
		cascaded = append(cascaded, resolved)
	}
	openIncidents, err := incidents.ListOpenForFaultIDs(ctx, tx, relatedIDs)
	if err != nil {
		return nil, err
	}
	for _, incident := range openIncidents {
		if _, err := incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
			IncidentID:   incident.ID,
			ActorUserID:  actorID,
			ActorDisplay: actorName,
			Kind:         domain.IncidentTimelineFact,
			Summary:      "关联实体已下线，Fault 按下线收口（不是恢复）",
			Detail: map[string]any{
				"entity_id":  node.ID,
				"entity_key": node.EntityKey,
				"reason":     reason,
			},
			DedupeKey:  "entity-decommission:" + node.ID,
			OccurredAt: at,
		}); err != nil {
			return nil, err
		}
	}
	return cascaded, nil
}

// NoteEntityRestore 在关联未收口 Fault / 未关闭 Incident 时间线留下恢复记录，
// 不重开 Fault、不改 Incident 状态。DedupeKey 保证同一实体重复恢复不刷屏。
func NoteEntityRestore(
	ctx context.Context, tx pgx.Tx, faults *FaultRepo, incidents *IncidentRepo,
	node domain.ResourceNode, actorID, actorName string, at time.Time,
) error {
	open, err := faults.ListOpenRelatedToNode(ctx, tx, node.ID, 100)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(open))
	for _, fault := range open {
		ids = append(ids, fault.ID)
		if _, err := faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
			FaultID: fault.ID,
			ActorID: actorID,
			Actor:   actorName,
			Action:  domain.FaultTimelineActionEntityRestored,
			At:      at,
			Detail: map[string]any{
				"entity_id":  node.ID,
				"entity_key": node.EntityKey,
			},
		}); err != nil {
			return err
		}
	}
	openIncidents, err := incidents.ListOpenForFaultIDs(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, incident := range openIncidents {
		if _, err := incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
			IncidentID:   incident.ID,
			ActorUserID:  actorID,
			ActorDisplay: actorName,
			Kind:         domain.IncidentTimelineFact,
			Summary:      "关联实体已恢复为 active",
			Detail: map[string]any{
				"entity_id":  node.ID,
				"entity_key": node.EntityKey,
			},
			DedupeKey:  "entity-restore:" + node.ID,
			OccurredAt: at,
		}); err != nil {
			return err
		}
	}
	return nil
}
