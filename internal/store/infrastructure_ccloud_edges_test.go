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

// TestImportSnapshotCrossSourceEdgesAndNoRecentTraffic 覆盖任务 8：
// TopologySnapshot 的边可以通过 FromNodeID/ToNodeID 跨源指向已登记的
// canonical 实体（不在本次快照 Nodes 列表内），且 no_recent_traffic 边在
// 跨来源遍历中按 stale 处理；同时验证按 source 隔离与重放幂等。
func TestImportSnapshotCrossSourceEdgesAndNoRecentTraffic(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "ccloud-edge-owner")
	topology := NewTopologyRepo()
	services := NewServiceCatalogRepo()

	// 预先登记两个「已存在于其他来源」的 canonical 实体：
	// - infraEntity 模拟已对齐的 RELY_NODE（entity_registry 来源）
	// - registeredApp 模拟已登记的应用服务（service_registry 来源），并声明
	//   一条 declared 依赖，用于验证 ccloud 快照替换不动其他来源的边。
	var infraEntity domain.ResourceNode
	var registeredApp domain.ServiceRegistration
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		infraEntity, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "infra/redis/prod", Layer: domain.EntityLayerInfrastructure,
			Name: "Shared Redis", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		platform, err := topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/k8s/prod", Layer: domain.EntityLayerPlatform,
			Name: "Production Kubernetes", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		registeredApp, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity:      domain.ResourceNode{EntityKey: "service/cplm/prod", Name: "CPLM", Environment: "prod", OwnerTeamID: team.ID},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier1, Enabled: true,
			Dependencies: []domain.DependencyEdge{{
				ToNodeID: platform.ID, Relation: domain.DependencyRelationDependsOn,
				Criticality: domain.DependencyCriticalityCritical,
			}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("准备已登记实体失败: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := domain.TopologySnapshot{
		SchemaVersion: 1,
		Source:        "ccloud",
		Environment:   "prod",
		ObservedAt:    observedAt,
		Nodes: []domain.ResourceNode{
			{ExternalRef: "ccloud.apisix", Name: "APISIX Gateway", Kind: domain.ResourceNodeKindInfrastructureCapability},
			{ExternalRef: "ccloud.app.cplm", Name: "CPLM App", Kind: domain.ResourceNodeKindBusinessSystem},
		},
		Edges: []domain.DependencyEdge{
			// 普通同源边：既有 external_ref 路径，不受本任务改动影响。
			{FromExternalRef: "ccloud.app.cplm", ToExternalRef: "ccloud.apisix", Relation: domain.DependencyRelationDependsOn},
			// 跨源边（ToNodeID 回落）：App -> 已对齐的 canonical RELY_NODE，
			// ExternalRef 只是平台侧 node.key，不在本次快照 Nodes 里；
			// NoRecentTraffic=true 模拟平台 totalCount=0。
			{FromExternalRef: "ccloud.app.cplm", ToExternalRef: "ccloud.rely.redis-1", ToNodeID: infraEntity.ID,
				Relation: domain.DependencyRelationDependsOn, NoRecentTraffic: true},
			// 跨源边（FromNodeID 回落）：已登记应用服务作为消费方指向本快照内的
			// ccloud 节点，验证 From 侧同样支持回落（主对话裁决，design.md 只写了 To 侧）。
			{FromExternalRef: "ccloud.app.registered", FromNodeID: registeredApp.Entity.ID, ToExternalRef: "ccloud.apisix",
				Relation: domain.DependencyRelationDependsOn},
		},
	}

	var imported TopologyImportResult
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		imported, err = topology.ImportSnapshot(ctx, tx, snapshot)
		return err
	}); err != nil {
		t.Fatalf("导入跨源快照失败: %v", err)
	}
	if imported.EdgeCount != len(snapshot.Edges) {
		t.Fatalf("导入边数=%d want=%d", imported.EdgeCount, len(snapshot.Edges))
	}
	appNodeID := imported.NodeIDs["ccloud.app.cplm"]
	apisixNodeID := imported.NodeIDs["ccloud.apisix"]

	// 重放同一快照：边数不应翻倍（ON CONFLICT 幂等收敛）。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		replayed, err := topology.ImportSnapshot(ctx, tx, snapshot)
		if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dependency_edges WHERE source = 'ccloud'`).Scan(&count); err != nil {
			return err
		}
		if count != len(snapshot.Edges) {
			return fmt.Errorf("重放后 ccloud 边数=%d want=%d", count, len(snapshot.Edges))
		}
		_ = replayed
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// source 隔离：ccloud 快照替换不影响 service_registry 写入的依赖边
	// （registeredApp -> platform 是 Create 时用 service_registry source 写入的）。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		serviceDeps, err := topology.ListDependenciesForSource(ctx, tx, serviceRegistrySource, registeredApp.Entity.ID)
		if err != nil {
			return err
		}
		if len(serviceDeps) != 1 || serviceDeps[0].ToNodeID == "" {
			return fmt.Errorf("service_registry 依赖边被 ccloud 导入影响: %+v", serviceDeps)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 跨来源遍历：经过 no_recent_traffic 边到达的节点必须标 stale，
	// 经过普通 observed 边到达的节点在时间未过期时不应 stale。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		providers, err := topology.ListDependencyProvidersByNodeID(ctx, tx, appNodeID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		byID := make(map[string]domain.BlastRadiusNode, len(providers))
		for _, p := range providers {
			byID[p.Node.ID] = p
		}
		if apisixItem, ok := byID[apisixNodeID]; !ok || apisixItem.Stale {
			return fmt.Errorf("普通边到达的 apisix 不应 stale: %+v", apisixItem)
		}
		if redisItem, ok := byID[infraEntity.ID]; !ok || !redisItem.Stale {
			return fmt.Errorf("no_recent_traffic 边到达的 redis 必须 stale: %+v", redisItem)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// FromNodeID 回落：已登记应用服务作为消费方也能落库并参与遍历。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		providers, err := topology.ListDependencyProvidersByNodeID(ctx, tx, registeredApp.Entity.ID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		for _, p := range providers {
			if p.Node.ID == apisixNodeID {
				return nil
			}
		}
		return fmt.Errorf("FromNodeID 回落边未能落库或未参与遍历: %+v", providers)
	}); err != nil {
		t.Fatal(err)
	}
}
