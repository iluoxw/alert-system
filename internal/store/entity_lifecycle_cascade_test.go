//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func TestResolveByDecommission_WriteReadBackIdempotentAndLeavesIncidentOpen(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topology := NewTopologyRepo()
	faults := NewFaultRepo()
	incidents := NewIncidentRepo()
	alerts := NewAlertRepo()
	team := mustTeam(t, ctx, st, "lifecycle-cascade-owner")
	actor := mustUser(t, ctx, st, "lifecycle-cascade-sre")
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

	var host domain.ResourceNode
	var fault domain.Fault
	var incident domain.Incident
	var alert domain.Alert
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.11", Layer: domain.EntityLayerInfrastructure,
			Name: "p-ai-sandbox-001", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		fault, _, err = faults.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: "test:lifecycle:cascade", Scope: domain.FaultScopeInstance,
			Status: domain.FaultStatusActive, Severity: domain.SeverityCritical,
			Kind: "exporter_down", Title: "节点采集中断", ResourceNodeID: host.ID,
			EntityRef: host.EntityKey, Confidence: 0.9, FirstDetected: now, LastEvaluated: now,
		})
		if err != nil {
			return err
		}
		incident, err = incidents.Create(ctx, tx, testIncident(actor.ID, "采集目标消失", now))
		if err != nil {
			return err
		}
		if _, err = incidents.LinkFault(ctx, tx, incident.ID, fault.ID, domain.IncidentFaultPrimary, now); err != nil {
			return err
		}
		alert, _, err = alerts.UpsertByFingerprint(ctx, tx, domain.AlertEvent{
			Fingerprint: "lifecycle-cascade-alert", Severity: domain.SeverityCritical,
			Title: "E2BNodeExporterDown", Source: "legacy", StartsAt: now,
			ResolvePolicy: domain.ResolvePolicySource,
		}, "")
		if err != nil {
			return err
		}
		return faults.UpsertAutomatedEvidence(ctx, tx, fault.ID, domain.FaultEvidence{
			AlertID: alert.ID, Role: domain.EvidenceRoleDirectImpact,
			Rationale: "采集目标消失", Confidence: 0.8,
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		preview, err := PreviewEntityLifecycle(ctx, tx, faults, incidents, alerts, host.ID)
		if err != nil {
			return err
		}
		if len(preview.ActiveFaults) != 1 || preview.ActiveFaults[0].ID != fault.ID {
			t.Fatalf("preview faults = %+v", preview.ActiveFaults)
		}
		if len(preview.OpenIncidents) != 1 || preview.OpenIncidents[0].ID != incident.ID ||
			preview.OpenIncidents[0].Status != domain.IncidentStatusDeclared {
			t.Fatalf("preview incidents = %+v", preview.OpenIncidents)
		}
		if len(preview.FiringAlerts) != 1 || preview.FiringAlerts[0].ID != alert.ID {
			t.Fatalf("preview alerts = %+v", preview.FiringAlerts)
		}
		hasActive, err := faults.HasActiveRelatedToNode(ctx, tx, host.ID)
		if err != nil {
			return err
		}
		if !hasActive {
			t.Fatal("挂着 active Fault 时应 HasActiveRelatedToNode=true")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		node, changed, err := topology.SetEntityLifecycle(ctx, tx, host.ID, actor.ID, "节点已从 E2B 集群剔除", domain.EntityLifecycleDecommissioned, now)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatal("首次下线 changed 应为 true")
		}
		cascaded, err := CascadeFaultsForDecommission(ctx, tx, faults, incidents, node, actor.ID, actor.Username, "节点已从 E2B 集群剔除", now)
		if err != nil {
			return err
		}
		if len(cascaded) != 1 || cascaded[0].Status != domain.FaultStatusResolved || cascaded[0].ResolvedAt.IsZero() {
			t.Fatalf("cascade 未把 Fault 收口: %+v", cascaded)
		}
		again, changed, err := faults.ResolveByDecommission(ctx, tx, fault.ID, now.Add(time.Hour))
		if err != nil {
			return err
		}
		if changed || !again.ResolvedAt.Equal(cascaded[0].ResolvedAt) {
			t.Fatalf("重复收口应短路: changed=%v got=%+v", changed, again)
		}
		open, err := incidents.GetByID(ctx, tx, incident.ID)
		if err != nil {
			return err
		}
		if open.Status != domain.IncidentStatusDeclared || !open.ResolvedAt.IsZero() {
			t.Fatalf("下线不得关闭 Incident: %+v", open)
		}
		timeline, err := incidents.ListTimeline(ctx, tx, incident.ID)
		if err != nil {
			return err
		}
		if len(timeline) != 1 || timeline[0].DedupeKey != "entity-decommission:"+host.ID {
			t.Fatalf("Incident 时间线应留下线说明: %+v", timeline)
		}
		if _, err := CascadeFaultsForDecommission(ctx, tx, faults, incidents, node, actor.ID, actor.Username, "再写一次", now.Add(time.Minute)); err != nil {
			return err
		}
		timeline, err = incidents.ListTimeline(ctx, tx, incident.ID)
		if err != nil {
			return err
		}
		if len(timeline) != 1 {
			t.Fatalf("重复 cascade 不得再写 Incident 时间线: %+v", timeline)
		}
		empty, err := PreviewEntityLifecycle(ctx, tx, faults, incidents, alerts, host.ID)
		if err != nil {
			return err
		}
		if len(empty.ActiveFaults) != 0 {
			t.Fatalf("收口后 preview faults 应为空: %+v", empty.ActiveFaults)
		}
		if empty.ActiveFaults == nil || empty.OpenIncidents == nil || empty.FiringAlerts == nil {
			t.Fatalf("空集合必须是非 nil 切片: %+v", empty)
		}
		// Fault 已 resolved，预览不再列出它，因此也不再带出 Incident / 仍 firing 的证据。
		// Incident 本身必须仍开着——上面 GetByID 已断言。
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewEntityLifecycle_EmptyCollections(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topology := NewTopologyRepo()
	team := mustTeam(t, ctx, st, "lifecycle-preview-empty")
	var host domain.ResourceNode
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.99", Layer: domain.EntityLayerInfrastructure,
			Name: "idle-node", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		preview, err := PreviewEntityLifecycle(ctx, tx, NewFaultRepo(), NewIncidentRepo(), NewAlertRepo(), host.ID)
		if err != nil {
			return err
		}
		if preview.ActiveFaults == nil || preview.OpenIncidents == nil || preview.FiringAlerts == nil {
			t.Fatalf("空预览集合为 nil: %+v", preview)
		}
		if len(preview.ActiveFaults) != 0 || len(preview.OpenIncidents) != 0 || len(preview.FiringAlerts) != 0 {
			t.Fatalf("空预览应全空: %+v", preview)
		}
		hasActive, err := NewFaultRepo().HasActiveRelatedToNode(ctx, tx, host.ID)
		if err != nil {
			return err
		}
		if hasActive {
			t.Fatal("无 Fault 时不应 409")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 回归：qualifiedFaultColumns 与 scanFault 必须同列。era3 给 scanFault 加了
// owner/ack 五列却漏改 qualifiedFaultColumns，ListForNode 因此自 da95e0d 起一直
// 扫描列数不匹配；v0.3.0 带着这个缺陷发到了 uat，而 ListForNode 只有 projector
// 单测里的 fake，真实 SQL 从未被测过，五轮全量验证全部漏掉。
// 本测试用真实 PG 跑通查询并断言 era3 列能读回，钉住这一类"列清单漂移"。
func TestListForNode_RoundTripsEra3OwnerAndAckColumns(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topology := NewTopologyRepo()
	faults := NewFaultRepo()
	team := mustTeam(t, ctx, st, "listfornode-owner")
	actor := mustUser(t, ctx, st, "listfornode-sre")
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	var host domain.ResourceNode
	var fault domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.21", Layer: domain.EntityLayerInfrastructure,
			Name: "listfornode-host", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		fault, _, err = faults.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: "test:listfornode", Scope: domain.FaultScopeInstance,
			Status: domain.FaultStatusActive, Severity: domain.SeverityWarning,
			Kind: "exporter_down", Title: "列清单回归", ResourceNodeID: host.ID,
			EntityRef: host.EntityKey, Confidence: 0.7, FirstDetected: now, LastEvaluated: now,
			OwnerTeamID: team.ID,
		})
		if err != nil {
			return err
		}
		_, _, err = faults.Ack(ctx, tx, fault.ID, actor.ID, "值班 SRE", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var got []domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = faults.ListForNode(ctx, tx, host.ID, 20)
		return err
	}); err != nil {
		t.Fatalf("ListForNode 失败（列清单与 scanFault 不匹配会在这里报错）: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 条 Fault，实际 %d", len(got))
	}
	if got[0].OwnerTeamID != team.ID {
		t.Fatalf("owner_team_id 未读回: %q", got[0].OwnerTeamID)
	}
	if got[0].AckedBy != actor.ID || got[0].AckedByName != "值班 SRE" {
		t.Fatalf("认领信息未读回: by=%q name=%q", got[0].AckedBy, got[0].AckedByName)
	}
}
