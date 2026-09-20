//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func TestSetEntityLifecycle_WriteReadBackDefaultFilterAndIdempotent(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "lifecycle-owner")
	actor := mustUser(t, ctx, st, "lifecycle-sre")
	topology := NewTopologyRepo()
	at := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	var host, sidecar domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.11", Layer: domain.EntityLayerInfrastructure,
			Name: "p-ai-sandbox-001", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		sidecar, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.12", Layer: domain.EntityLayerInfrastructure,
			Name: "still-active", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("创建实体失败: %v", err)
	}
	if host.LifecycleState != domain.EntityLifecycleActive {
		t.Fatalf("新建实体默认 lifecycle=%q, want active", host.LifecycleState)
	}
	if host.DecommissionedAt != nil || host.DecommissionedBy != "" || host.DecommissionReason != "" {
		t.Fatalf("新建实体不得带下线审计列: %+v", host)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		got, changed, err := topology.SetEntityLifecycle(ctx, tx, host.ID, actor.ID, "节点已从 E2B 集群剔除", domain.EntityLifecycleDecommissioned, at)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("首次下线 changed 应为 true")
		}
		if got.LifecycleState != domain.EntityLifecycleDecommissioned {
			return errors.New("下线后状态应为 decommissioned")
		}
		if got.DecommissionedBy != actor.ID || got.DecommissionReason != "节点已从 E2B 集群剔除" {
			return errors.New("审计列未落库")
		}
		if got.DecommissionedAt == nil || !got.DecommissionedAt.Equal(at) {
			return errors.New("decommissioned_at 未按传入时刻落库")
		}

		again, changed, err := topology.SetEntityLifecycle(ctx, tx, host.ID, actor.ID, "另一条原因", domain.EntityLifecycleDecommissioned, at.Add(time.Hour))
		if err != nil {
			return err
		}
		if changed {
			return errors.New("重复下线不得重写")
		}
		if again.DecommissionReason != "节点已从 E2B 集群剔除" || !again.DecommissionedAt.Equal(at) {
			return errors.New("重复下线重写了审计列")
		}

		renamed, err := topology.UpdateManagedEntity(ctx, tx, host.ID, domain.ResourceNode{
			EntityKey: host.EntityKey, Layer: host.Layer, Name: "renamed-but-still-decommissioned",
			Environment: host.Environment, OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		if renamed.LifecycleState != domain.EntityLifecycleDecommissioned || renamed.DecommissionReason != "节点已从 E2B 集群剔除" {
			return errors.New("UpdateManagedEntity 不得改生命周期")
		}

		listed, err := topology.ListNodes(ctx, tx, "", "", 50)
		if err != nil {
			return err
		}
		if !containsNode(listed, sidecar.ID) || containsNode(listed, host.ID) {
			return errors.New("默认列表必须只含 active")
		}
		decom, err := topology.ListNodes(ctx, tx, "", domain.EntityLifecycleDecommissioned, 50)
		if err != nil {
			return err
		}
		if !containsNode(decom, host.ID) || containsNode(decom, sidecar.ID) {
			return errors.New("decommissioned 筛选不正确")
		}
		all, err := topology.ListNodes(ctx, tx, "", domain.EntityLifecycleAll, 50)
		if err != nil {
			return err
		}
		if !containsNode(all, host.ID) || !containsNode(all, sidecar.ID) {
			return errors.New("all 筛选应含两种状态")
		}

		restored, changed, err := topology.SetEntityLifecycle(ctx, tx, host.ID, actor.ID, "", domain.EntityLifecycleActive, at)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("恢复 changed 应为 true")
		}
		if restored.LifecycleState != domain.EntityLifecycleActive || restored.DecommissionedAt != nil ||
			restored.DecommissionedBy != "" || restored.DecommissionReason != "" {
			return errors.New("恢复后审计列应清空")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, _, err := topology.SetEntityLifecycle(ctx, tx, sidecar.ID, actor.ID, "", domain.EntityLifecycleDecommissioned, at)
		if !errors.Is(err, ErrEntityLifecycleReasonRequired) {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("缺原因应拒绝: %v", err)
	}
}

func containsNode(nodes []domain.ResourceNode, id string) bool {
	for _, node := range nodes {
		if node.ID == id {
			return true
		}
	}
	return false
}
