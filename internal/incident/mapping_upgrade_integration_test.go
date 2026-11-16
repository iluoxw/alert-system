//go:build integration

package incident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/mapping"
	"alert-system/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestMappingUpgradeMissingDimensionPreservesBoundLifecycle(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	policies := store.NewAlertMappingRepo()
	faults := store.NewFaultRepo()
	topology := store.NewTopologyRepo()
	projector := mapping.NewProjector(policies, store.NewServiceCatalogRepo(), topology, faults, nil)
	var alert domain.Alert
	var fault domain.Fault
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := topology.ImportSnapshot(ctx, tx, domain.TopologySnapshot{SchemaVersion: 1, Source: "review", ObservedAt: now, Nodes: []domain.ResourceNode{{ExternalRef: "gateway", Name: "Gateway", Kind: domain.ResourceNodeKindInfrastructureCapability}}})
		if err != nil {
			return err
		}
		node, err := topology.GetByExternalRef(ctx, tx, "review", "gateway")
		if err != nil {
			return err
		}
		alert, _, err = store.NewAlertRepo().UpsertByFingerprint(ctx, tx, domain.AlertEvent{Fingerprint: "review-old-missing", ResolvePolicy: domain.ResolvePolicySource, RuleID: "test", Severity: domain.SeverityCritical, Status: domain.StatusFiring, Title: "test", Source: "generic", StartsAt: now, Labels: map[string]string{"sre.entity.key": "review:gateway", "sre.fault.kind": "unavailable", "sre.evidence.role": "direct_impact", "sre.correlation.labels": "cluster"}}, "")
		if err != nil {
			return err
		}
		// 精确模拟旧 Project 的空维度 key 与 active evidence。
		sum := sha256.Sum256([]byte("review:gateway\x00unavailable\x00cluster="))
		key := "fault:review:gateway:unavailable:" + hex.EncodeToString(sum[:6])
		fault, _, err = faults.Upsert(ctx, tx, domain.Fault{CorrelationKey: key, Scope: domain.FaultScopeCapability, Status: domain.FaultStatusActive, Severity: domain.SeverityCritical, Kind: "unavailable", Title: "old", ResourceNodeID: node.ID, EntityRef: node.EntityKey, Confidence: 1, FirstDetected: now, LastEvaluated: now})
		if err != nil {
			return err
		}
		return faults.UpsertAutomatedEvidence(ctx, tx, fault.ID, domain.FaultEvidence{AlertID: alert.ID, Role: domain.EvidenceRoleDirectImpact, Rationale: "pre-upgrade mapping", Confidence: 1, DecisionOrigin: domain.DecisionOriginAutomation})
	})
	if err != nil {
		t.Fatal(err)
	}

	alert.Source = "alertmanager"
	alert.SourceIntegrationID = "source"
	alert.UpstreamFingerprint = "fp"
	alert.SourceState = domain.SourceStateFiring
	check := func(want domain.FaultStatus) {
		t.Helper()
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			actual, err := faults.GetByID(ctx, tx, fault.ID)
			if err != nil {
				return err
			}
			var links, total int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM fault_alert_links WHERE fault_id=$1),(SELECT count(*) FROM faults)`, fault.ID).Scan(&links, &total); err != nil {
				return err
			}
			if actual.Status != want || links != 1 || total != 1 {
				t.Fatalf("状态/历史证据/故障数不符: status=%s links=%d total=%d", actual.Status, links, total)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	project := func() {
		t.Helper()
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			preview, err := projector.Evaluate(ctx, tx, alert)
			if err != nil {
				return err
			}
			actual, err := projector.Project(ctx, tx, alert)
			if err != nil {
				return err
			}
			if actual.ReasonCode != "correlation_label_missing" || actual.FaultID != fault.ID || actual.CorrelationKey != fault.CorrelationKey || preview.WouldCreateFault {
				t.Fatalf("应保留绑定并治理，不得新建: %+v / %+v", actual, preview)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 升级后的重复 firing 不再新建、合并、删证据，也不产生 active 通知转移。
	project()
	check(domain.FaultStatusActive)
	// 另一个 occurrence 的 firing 证据阻止本次解除关闭共享 Fault。
	var other domain.Alert
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		other, _, err = store.NewAlertRepo().UpsertByFingerprint(ctx, tx, domain.AlertEvent{Fingerprint: "other-occurrence", ResolvePolicy: domain.ResolvePolicySource, RuleID: "test", Severity: domain.SeverityWarning, Status: domain.StatusFiring, Title: "other", Source: "generic", StartsAt: time.Now()}, "")
		if err != nil {
			return err
		}
		return faults.UpsertAutomatedEvidence(ctx, tx, fault.ID, domain.FaultEvidence{AlertID: other.ID, Role: domain.EvidenceRoleDirectImpact, Rationale: "other firing", Confidence: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	alert.SourceState = domain.SourceStateResolved
	project()
	if got := faultStatus(t, ctx, st, fault.ID); got != domain.FaultStatusActive {
		t.Fatalf("另一条firing不得降级: %s", got)
	}
	// 另一条证据为 human，同样保护它，解除后只允许 monitoring。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fault_alert_links SET decision_origin='human' WHERE alert_id=$1`, other.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE alerts SET status='resolved',source_state='resolved' WHERE id=$1`, other.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	project()
	if got := faultStatus(t, ctx, st, fault.ID); got != domain.FaultStatusMonitoring {
		t.Fatalf("最后解除必须进入monitoring: %s", got)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var automated, human int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE decision_origin='automation'),count(*) FILTER(WHERE decision_origin='human') FROM fault_alert_links WHERE fault_id=$1`, fault.ID).Scan(&automated, &human); err != nil {
			return err
		}
		if automated != 1 || human != 1 {
			t.Fatalf("不得删除历史自动/人工证据: %d/%d", automated, human)
		}
		_, err := tx.Exec(ctx, `UPDATE faults SET status='resolved',resolved_at=now() WHERE id=$1`, fault.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// 人工裁决证据的角色与裁决保留，但该 occurrence 的解除仍可收口既有 Fault。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fault_alert_links SET decision_origin='human' WHERE alert_id=$1`, alert.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE faults SET status='active',resolved_at=NULL WHERE id=$1`, fault.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	project()
	if got := faultStatus(t, ctx, st, fault.ID); got != domain.FaultStatusMonitoring {
		t.Fatalf("人工证据也须沿已有绑定解除: %s", got)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE faults SET status='resolved',resolved_at=now() WHERE id=$1`, fault.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	project()
	if got := faultStatus(t, ctx, st, fault.ID); got != domain.FaultStatusResolved {
		t.Fatalf("历史resolved重放不得重开: %s", got)
	}
	alert.SourceState = domain.SourceStateFiring
	project()
	if got := faultStatus(t, ctx, st, fault.ID); got != domain.FaultStatusResolved {
		t.Fatalf("缺维度重复firing不得重开: %s", got)
	}
}
