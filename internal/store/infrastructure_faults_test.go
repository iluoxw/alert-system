//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/e2b"
)

func TestTopologyRepoImportAndBlastRadius(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewTopologyRepo()
	snapshot, err := e2b.LoadTopologyFixture()
	if err != nil {
		t.Fatal(err)
	}

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
		radius, err := repo.ListBlastRadius(ctx, tx, snapshot.Source, "e2b.orchestrator.pool", 8)
		if err != nil {
			return err
		}
		wantDepth := map[string]int{
			"e2b.orchestrator.pool":    0,
			"e2b.api":                  1,
			"e2b.client-proxy":         2,
			"e2b.sandbox-runtime":      3,
			"insforge.preview-runtime": 4,
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
		})
		if err != nil {
			return err
		}
		platform, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/kubernetes/prod", Layer: domain.EntityLayerPlatform,
			Name: "Production Kubernetes", Environment: "prod", OwnerTeamID: team.ID,
		})
		if err != nil {
			return err
		}
		service, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity: domain.ResourceNode{EntityKey: "service/cch/prod", Name: "CCH", Environment: "prod",
				OwnerTeamID: team.ID, Layer: domain.EntityLayerApplication,
				Kind: domain.ResourceNodeKindBusinessSystem},
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

func TestFaultRepoPreservesHumanEvidenceAndMonotonicActivation(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	topologyRepo := NewTopologyRepo()
	faultRepo := NewFaultRepo()
	alertRepo := NewAlertRepo()
	snapshot, err := e2b.LoadTopologyFixture()
	if err != nil {
		t.Fatal(err)
	}

	var nodeID, alertID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, err := topologyRepo.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		nodeID = result.NodeIDs["e2b.sandbox-runtime"]
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
			CorrelationKey: "test:e2b:capability", Scope: domain.FaultScopeCapability,
			Status: domain.FaultStatusCandidate, Severity: domain.SeverityWarning,
			Kind: "sandbox_placement_degradation", Title: "Placement 容量退化",
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
	snapshot, err := e2b.LoadTopologyFixture()
	if err != nil {
		t.Fatal(err)
	}

	var fault domain.Fault
	var impactedNodeID string
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, err := topologyRepo.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		impactedNodeID = result.NodeIDs["insforge.preview-runtime"]
		fault, _, err = faultRepo.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: "test:impact:decision", Scope: domain.FaultScopeCapability,
			Status: domain.FaultStatusActive, Severity: domain.SeverityCritical,
			Kind: "provider_unavailable", Title: "共享能力不可用",
			ResourceNodeID: result.NodeIDs["e2b.orchestrator.pool"], Confidence: 0.9,
			FirstDetected: now, LastEvaluated: now,
		})
		if err != nil {
			return err
		}
		return faultRepo.ReplaceAutomatedImpacts(ctx, tx, fault.ID, []domain.FaultImpact{{
			NodeID: impactedNodeID, State: domain.ImpactStatePotential,
			Path:      []string{result.NodeIDs["e2b.orchestrator.pool"], impactedNodeID},
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
