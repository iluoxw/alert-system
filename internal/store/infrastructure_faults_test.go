//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func testBlastRadiusTopologySnapshot() domain.TopologySnapshot {
	observedAt := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	return domain.TopologySnapshot{
		SchemaVersion: 1,
		Source:        "test-topology-sync",
		Environment:   "test",
		ObservedAt:    observedAt,
		Nodes: []domain.ResourceNode{
			{ExternalRef: "shared.orchestrator.pool", Name: "Shared Orchestrator Pool", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.orchestrator.node-a", Name: "Shared Orchestrator Node A", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.orchestrator.node-b", Name: "Shared Orchestrator Node B", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.api", Name: "Shared API", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.client-proxy", Name: "Shared Client Proxy", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.redis", Name: "Shared Redis Metadata", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.object-storage", Name: "Shared Object Storage", Kind: domain.ResourceNodeKindResource},
			{ExternalRef: "shared.runtime", Name: "Shared Runtime", Kind: domain.ResourceNodeKindInfrastructureCapability},
			{ExternalRef: "preview.app", Name: "Preview Application", Kind: domain.ResourceNodeKindBusinessSystem},
		},
		Edges: []domain.DependencyEdge{
			{FromExternalRef: "shared.orchestrator.node-a", ToExternalRef: "shared.orchestrator.pool", Relation: domain.DependencyRelationPartOf, Criticality: domain.DependencyCriticalityHigh},
			{FromExternalRef: "shared.orchestrator.node-b", ToExternalRef: "shared.orchestrator.pool", Relation: domain.DependencyRelationPartOf, Criticality: domain.DependencyCriticalityHigh},
			{FromExternalRef: "shared.api", ToExternalRef: "shared.orchestrator.pool", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
			{FromExternalRef: "shared.client-proxy", ToExternalRef: "shared.api", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
			{FromExternalRef: "shared.runtime", ToExternalRef: "shared.client-proxy", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
			{FromExternalRef: "shared.runtime", ToExternalRef: "shared.redis", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
			{FromExternalRef: "shared.runtime", ToExternalRef: "shared.object-storage", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
			{FromExternalRef: "preview.app", ToExternalRef: "shared.runtime", Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityCritical},
		},
	}
}

func TestTopologyRepoImportAndBlastRadius(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewTopologyRepo()
	snapshot := testBlastRadiusTopologySnapshot()

	var imported TopologyImportResult
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var importErr error
		imported, importErr = repo.ImportSnapshot(ctx, tx, snapshot)
		return importErr
	}); err != nil {
		t.Fatalf("导入拓扑失败: %v", err)
	}
	if imported.NodeCount != len(snapshot.Nodes) || imported.EdgeCount != len(snapshot.Edges) || imported.SnapshotHash == "" {
		t.Fatalf("导入结果错误: %+v", imported)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		radius, err := repo.ListBlastRadius(ctx, tx, snapshot.Source, "shared.orchestrator.pool", 8)
		if err != nil {
			return err
		}
		wantDepth := map[string]int{
			"shared.orchestrator.pool": 0,
			"shared.api":               1,
			"shared.client-proxy":      2,
			"shared.runtime":           3,
			"preview.app":              4,
		}
		if len(radius) != len(wantDepth) {
			return fmt.Errorf("爆炸半径节点=%d want=%d: %+v", len(radius), len(wantDepth), radius)
		}
		for _, item := range radius {
			depth, ok := wantDepth[item.Node.ExternalRef]
			if !ok || depth != item.Depth || len(item.Path) != item.Depth+1 {
				return fmt.Errorf("爆炸半径节点错误: %+v", item)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedServiceRelationCrossSourceTraversalAndStale(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "relation-owner")
	topology := NewTopologyRepo()
	services := NewServiceCatalogRepo()

	var infrastructure, platform domain.ResourceNode
	var service domain.ServiceRegistration
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		infrastructure, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "infra/shared/prod", Layer: domain.EntityLayerInfrastructure,
			Name: "Shared Compute", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		platform, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/kubernetes/prod", Layer: domain.EntityLayerPlatform,
			Name: "Production Kubernetes", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		service, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity:      domain.ResourceNode{EntityKey: "service/cch/prod", Name: "CCH", Environment: "prod", OwnerTeamID: team.ID},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier1, Enabled: true,
			Dependencies: []domain.DependencyEdge{{
				ToNodeID: platform.ID, Relation: domain.DependencyRelationDependsOn,
				Criticality: domain.DependencyCriticalityCritical,
			}},
		})
		if err != nil {
			return err
		}
		var runID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash, finished_at)
			VALUES ('kubernetes', 'completed', $1, 'stale-fixture', now()) RETURNING id::text
		`, time.Now().Add(-48*time.Hour)).Scan(&runID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO dependency_edges (
				source, from_node_id, to_node_id, relation, criticality, evidence,
				observed_at, sync_run_id, origin
			) VALUES ('kubernetes', $1, $2, 'depends_on', 'high', '{}', $3, $4, 'observed')
		`, platform.ID, infrastructure.ID, time.Now().Add(-48*time.Hour), runID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		radius, err := topology.ListBlastRadiusByNodeID(ctx, tx, infrastructure.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		want := map[string]struct {
			depth int
			stale bool
		}{
			infrastructure.ID: {0, false},
			platform.ID:       {1, true},
			service.Entity.ID: {2, true},
		}
		if len(radius) != len(want) {
			return fmt.Errorf("跨来源爆炸半径=%d want=%d: %+v", len(radius), len(want), radius)
		}
		for _, item := range radius {
			expected, ok := want[item.Node.ID]
			if !ok || item.Depth != expected.depth || item.Stale != expected.stale || len(item.Path) != item.Depth+1 {
				return fmt.Errorf("跨来源路径错误: %+v", item)
			}
		}
		providers, err := topology.ListDependencyProvidersByNodeID(ctx, tx, service.Entity.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		if len(providers) != 3 || providers[2].Node.ID != infrastructure.ID || !providers[2].Stale {
			return fmt.Errorf("正向 provider closure 错误: %+v", providers)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalTraversalSkipsDecommissionedAndDownstream(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "decom-traversal-owner")
	actor := mustUser(t, ctx, st, "decom-traversal-sre")
	topology := NewTopologyRepo()
	services := NewServiceCatalogRepo()

	var root, mid, bypass domain.ResourceNode
	var blocked, live domain.ServiceRegistration
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		root, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "infra/e2b-node/prod", Layer: domain.EntityLayerInfrastructure,
			Name: "e2b-root", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		mid, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/e2b-agent/prod", Layer: domain.EntityLayerPlatform,
			Name: "e2b-mid", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, []domain.DependencyEdge{{
			ToNodeID: root.ID, Relation: domain.DependencyRelationDependsOn,
			Criticality: domain.DependencyCriticalityHigh,
		}})
		if err != nil {
			return err
		}
		bypass, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/e2b-live/prod", Layer: domain.EntityLayerPlatform,
			Name: "e2b-bypass", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, []domain.DependencyEdge{{
			ToNodeID: root.ID, Relation: domain.DependencyRelationDependsOn,
			Criticality: domain.DependencyCriticalityHigh,
		}})
		if err != nil {
			return err
		}
		blocked, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity:      domain.ResourceNode{EntityKey: "service/blocked/prod", Name: "Blocked App", Environment: "prod", OwnerTeamID: team.ID},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier1, Enabled: true,
			Dependencies: []domain.DependencyEdge{{
				ToNodeID: mid.ID, Relation: domain.DependencyRelationDependsOn,
				Criticality: domain.DependencyCriticalityCritical,
			}},
		})
		if err != nil {
			return err
		}
		live, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity:      domain.ResourceNode{EntityKey: "service/live/prod", Name: "Live App", Environment: "prod", OwnerTeamID: team.ID},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier1, Enabled: true,
			Dependencies: []domain.DependencyEdge{{
				ToNodeID: bypass.ID, Relation: domain.DependencyRelationDependsOn,
				Criticality: domain.DependencyCriticalityCritical,
			}},
		})
		if err != nil {
			return err
		}
		_, _, err = topology.SetEntityLifecycle(ctx, tx, mid.ID, actor.ID, "节点已剔除", domain.EntityLifecycleDecommissioned, time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("构造图失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		radius, err := topology.ListBlastRadiusByNodeID(ctx, tx, root.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		got := map[string]int{}
		for _, item := range radius {
			got[item.Node.ID] = item.Depth
		}
		if _, ok := got[mid.ID]; ok {
			return fmt.Errorf("下线节点自己不应出现在下游遍历里: %+v", radius)
		}
		if _, ok := got[blocked.Entity.ID]; ok {
			return fmt.Errorf("下线节点的下游仍被遍历出来（D1：必须在 SQL 递归里断链）: %+v", radius)
		}
		if got[root.ID] != 0 || got[bypass.ID] != 1 || got[live.Entity.ID] != 2 {
			return fmt.Errorf("active 路径被破坏: %+v", radius)
		}
		if len(got) != 3 {
			return fmt.Errorf("爆炸半径=%d want=3: %+v", len(got), radius)
		}

		fromBlocked, err := topology.ListDependencyProvidersByNodeID(ctx, tx, blocked.Entity.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		if len(fromBlocked) != 1 || fromBlocked[0].Node.ID != blocked.Entity.ID {
			return fmt.Errorf("从下游反查不应穿过下线节点: %+v", fromBlocked)
		}

		edges, err := topology.ListEdgesForNode(ctx, tx, mid.ID)
		if err != nil {
			return err
		}
		if len(edges) == 0 {
			return fmt.Errorf("下线不得删除依赖边")
		}

		self, err := topology.ListBlastRadiusByNodeID(ctx, tx, mid.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		if len(self) != 1 || self[0].Node.ID != mid.ID {
			return fmt.Errorf("从下线节点出发应只含自身、不含下游: %+v", self)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListBlastRadius_SourceScopedAlsoStopsAtDecommissioned(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := mustUser(t, ctx, st, "blast-decom-sre")
	repo := NewTopologyRepo()
	snapshot := testBlastRadiusTopologySnapshot()

	var imported TopologyImportResult
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		imported, err = repo.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		_, _, err = repo.SetEntityLifecycle(ctx, tx, imported.NodeIDs["shared.client-proxy"], actor.ID, "采集目标已消失", domain.EntityLifecycleDecommissioned, time.Now())
		return err
	}); err != nil {
		t.Fatalf("导入或下线失败: %v", err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		radius, err := repo.ListBlastRadius(ctx, tx, snapshot.Source, "shared.orchestrator.pool", 8)
		if err != nil {
			return err
		}
		got := map[string]int{}
		for _, item := range radius {
			got[item.Node.ExternalRef] = item.Depth
		}
		if _, ok := got["shared.client-proxy"]; ok {
			return fmt.Errorf("同源遍历仍含下线节点: %+v", radius)
		}
		if _, ok := got["shared.runtime"]; ok {
			return fmt.Errorf("同源遍历仍穿过下线节点到 runtime: %+v", radius)
		}
		if _, ok := got["preview.app"]; ok {
			return fmt.Errorf("同源遍历仍穿过下线节点到 preview.app: %+v", radius)
		}
		if got["shared.orchestrator.pool"] != 0 || got["shared.api"] != 1 {
			return fmt.Errorf("同源 active 前缀被破坏: %+v", radius)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFaultRepoPreservesHumanEvidenceAndMonotonicActivation(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topologyRepo := NewTopologyRepo()
	faultRepo := NewFaultRepo()
	alertRepo := NewAlertRepo()
	snapshot := testBlastRadiusTopologySnapshot()

	var nodeID, alertID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, err := topologyRepo.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		nodeID = result.NodeIDs["shared.runtime"]
		alert, _, err := alertRepo.UpsertByFingerprint(ctx, tx, testAlertEvent("fault-evidence-alert"), "")
		if err != nil {
			return err
		}
		alertID = alert.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	var fault domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		fault, _, err = faultRepo.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: "test:shared:capability", Scope: domain.FaultScopeCapability,
			Status: domain.FaultStatusCandidate, Severity: domain.SeverityWarning,
			Kind: "provider_capacity_degradation", Title: "Placement 容量退化",
			ResourceNodeID: nodeID, Confidence: 0.7, FirstDetected: now, LastEvaluated: now,
		})
		if err != nil {
			return err
		}
		return faultRepo.ReplaceAutomatedEvidence(ctx, tx, fault.ID, []domain.FaultEvidence{{
			AlertID: alertID, Role: domain.EvidenceRoleCauseCandidate,
			Rationale: "自动根因候选", Confidence: 0.8,
		}})
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE fault_alert_links
			SET role = 'excluded', rationale = '人工确认属于独立故障链', decision_origin = 'human'
			WHERE fault_id = $1 AND alert_id = $2
		`, fault.ID, alertID); err != nil {
			return err
		}
		if err := faultRepo.ReplaceAutomatedEvidence(ctx, tx, fault.ID, []domain.FaultEvidence{{
			AlertID: alertID, Role: domain.EvidenceRoleDirectImpact,
			Rationale: "下一轮自动结果", Confidence: 1,
		}}); err != nil {
			return err
		}
		evidence, err := faultRepo.ListEvidence(ctx, tx, fault.ID)
		if err != nil {
			return err
		}
		if len(evidence) != 1 || evidence[0].Role != domain.EvidenceRoleExcluded || evidence[0].DecisionOrigin != domain.DecisionOriginHuman {
			return fmt.Errorf("人工证据被自动关联覆盖: %+v", evidence)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		active, _, err := faultRepo.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: fault.CorrelationKey, Scope: fault.Scope, Status: domain.FaultStatusActive,
			Severity: domain.SeverityCritical, Kind: fault.Kind, Title: fault.Title,
			ResourceNodeID: nodeID, Confidence: 0.99, FirstDetected: now, LastEvaluated: now.Add(time.Minute),
		})
		if err != nil {
			return err
		}
		candidate, _, err := faultRepo.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: fault.CorrelationKey, Scope: fault.Scope, Status: domain.FaultStatusCandidate,
			Severity: domain.SeverityWarning, Kind: fault.Kind, Title: fault.Title,
			ResourceNodeID: nodeID, Confidence: 0.6, FirstDetected: now, LastEvaluated: now.Add(2 * time.Minute),
		})
		if err != nil {
			return err
		}
		if candidate.ID != active.ID || candidate.Status != domain.FaultStatusActive || candidate.Confidence != 0.99 {
			return fmt.Errorf("active Fault 被静默降级: active=%+v after=%+v", active, candidate)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFaultImpactHumanDecisionWins(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topologyRepo := NewTopologyRepo()
	faultRepo := NewFaultRepo()
	actor := mustUser(t, ctx, st, "impact-reviewer")
	snapshot := testBlastRadiusTopologySnapshot()

	var fault domain.Fault
	var impactedNodeID string
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, err := topologyRepo.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		impactedNodeID = result.NodeIDs["preview.app"]
		fault, _, err = faultRepo.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: "test:impact:decision", Scope: domain.FaultScopeCapability,
			Status: domain.FaultStatusActive, Severity: domain.SeverityCritical,
			Kind: "provider_unavailable", Title: "共享能力不可用",
			ResourceNodeID: result.NodeIDs["shared.orchestrator.pool"], Confidence: 0.9,
			FirstDetected: now, LastEvaluated: now,
		})
		if err != nil {
			return err
		}
		return faultRepo.ReplaceAutomatedImpacts(ctx, tx, fault.ID, []domain.FaultImpact{{
			NodeID: impactedNodeID, State: domain.ImpactStatePotential,
			Path:      []string{result.NodeIDs["shared.orchestrator.pool"], impactedNodeID},
			Rationale: "拓扑反向遍历可达", Confidence: 0.6,
		}})
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		stored, err := faultRepo.UpsertHumanImpact(ctx, tx, domain.FaultImpact{
			FaultID: fault.ID, NodeID: impactedNodeID, State: domain.ImpactStateRuledOut,
			Path: []string{impactedNodeID}, Rationale: "应用 SLO 与合成探针均健康",
			Confidence: 1, DecisionOrigin: domain.DecisionOriginHuman,
			DecidedBy: actor.ID, DecidedAt: now,
		})
		if err != nil {
			return err
		}
		if stored.State != domain.ImpactStateRuledOut || stored.DecidedBy != actor.ID || stored.DecidedAt.IsZero() {
			return fmt.Errorf("人工 Impact 裁决未完整保存: %+v", stored)
		}
		return faultRepo.ReplaceAutomatedImpacts(ctx, tx, fault.ID, []domain.FaultImpact{{
			NodeID: impactedNodeID, State: domain.ImpactStateConfirmed,
			Path: []string{impactedNodeID}, Rationale: "下一轮自动结果", Confidence: 1,
		}})
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		impacts, err := faultRepo.ListImpacts(ctx, tx, fault.ID)
		if err != nil {
			return err
		}
		if len(impacts) != 1 || impacts[0].State != domain.ImpactStateRuledOut ||
			impacts[0].DecisionOrigin != domain.DecisionOriginHuman || impacts[0].DecidedBy != actor.ID {
			return fmt.Errorf("人工 Impact 被自动结果覆盖: %+v", impacts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
