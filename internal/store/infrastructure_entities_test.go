//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件是集成测试，setupTestStore/mustTeam 复用 events_test.go / config_repos_test.go
// 已有定义（同包、同 build tag），覆盖受管实体（TopologyRepo.CreateManagedEntity/
// UpdateManagedEntity）携带 aliases/dependencies 的场景（任务：受管实体别名与依赖）。
// 实测运行：
//   go test -tags=integration ./internal/store/... -run 'TestManagedEntity' -v

// TestManagedEntityCreateWithAliasesAndDependencies 验证创建受管实体时可以一并
// 落地 Entity Alias 和依赖边，且依赖边方向、source、origin 符合不变量 15/16。
func TestManagedEntityCreateWithAliasesAndDependencies(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "managed-entity-owner")
	topology := NewTopologyRepo()

	var provider domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		provider, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/nomad/prod", Layer: domain.EntityLayerPlatform,
			Name: "Nomad Cluster", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("创建依赖目标失败: %v", err)
	}

	var host domain.ResourceNode
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.11", Layer: domain.EntityLayerInfrastructure,
			Name: "10.61.17.11", Environment: "prod", OwnerTeamID: team.ID,
		}, []domain.EntityAlias{
			{Namespace: "prometheus.instance", Value: "10.61.17.11:4646", Enabled: true},
			{Namespace: "prometheus.instance", Value: "10.61.17.11:9100", Enabled: true},
		}, []domain.DependencyEdge{
			{ToNodeID: provider.ID, Criticality: domain.DependencyCriticalityHigh},
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建受管实体（带 alias/依赖）失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		aliases, err := topology.ListAliases(ctx, tx, host.ID)
		if err != nil {
			return err
		}
		if len(aliases) != 2 {
			t.Fatalf("alias 数量错误: %+v", aliases)
		}
		deps, err := topology.ListDependenciesForSource(ctx, tx, EntityRegistrySource, host.ID)
		if err != nil {
			return err
		}
		if len(deps) != 1 || deps[0].ToNodeID != provider.ID || deps[0].Criticality != domain.DependencyCriticalityHigh ||
			deps[0].Origin != domain.RelationOriginDeclared || deps[0].Source != EntityRegistrySource {
			t.Fatalf("依赖边错误: %+v", deps)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestManagedEntityUpdateReplacesAliasesAndDependenciesWithoutTouchingOtherSources
// 验证：1) 更新受管实体会整体替换它自己的 alias/依赖；2) 不会误删同一实体上其他
// source（service_registry / 拓扑同步）写入的边——这是不变量 15 的 source 隔离。
func TestManagedEntityUpdateReplacesAliasesAndDependenciesWithoutTouchingOtherSources(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "managed-entity-update-owner")
	topology := NewTopologyRepo()
	services := NewServiceCatalogRepo()

	var providerA, providerB domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		providerA, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/nomad-a/prod", Layer: domain.EntityLayerPlatform,
			Name: "Nomad A", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		providerB, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/nomad-b/prod", Layer: domain.EntityLayerPlatform,
			Name: "Nomad B", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("创建依赖目标失败: %v", err)
	}

	var host domain.ResourceNode
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.12", Layer: domain.EntityLayerInfrastructure,
			Name: "10.61.17.12", Environment: "prod", OwnerTeamID: team.ID,
		}, []domain.EntityAlias{{Namespace: "prometheus.instance", Value: "10.61.17.12:4646", Enabled: true}},
			[]domain.DependencyEdge{{ToNodeID: providerA.ID, Criticality: domain.DependencyCriticalityMedium}})
		return err
	})
	if err != nil {
		t.Fatalf("创建受管实体失败: %v", err)
	}

	// 应用服务依赖该主机（service_registry 来源的边），用来验证更新受管实体不会
	// 误删它。
	var service domain.ServiceRegistration
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		service, err = services.Create(ctx, tx, domain.ServiceRegistration{
			Entity:      domain.ResourceNode{EntityKey: "service/edge/prod", Name: "Edge Service", Environment: "prod", OwnerTeamID: team.ID},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier2, Enabled: true,
			Dependencies: []domain.DependencyEdge{{ToNodeID: host.ID, Relation: domain.DependencyRelationDependsOn, Criticality: domain.DependencyCriticalityHigh}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建依赖该主机的应用服务失败: %v", err)
	}

	// 更新受管实体：alias 换成新的两个值，依赖换成 providerB。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.UpdateManagedEntity(ctx, tx, host.ID, domain.ResourceNode{
			EntityKey: host.EntityKey, Layer: host.Layer,
			Name: "10.61.17.12", Environment: "prod", OwnerTeamID: team.ID,
		}, []domain.EntityAlias{
			{Namespace: "prometheus.instance", Value: "10.61.17.12:4646", Enabled: true},
			{Namespace: "prometheus.instance", Value: "10.61.17.12:9100", Enabled: true},
		}, []domain.DependencyEdge{{ToNodeID: providerB.ID, Criticality: domain.DependencyCriticalityCritical}})
		return err
	})
	if err != nil {
		t.Fatalf("更新受管实体失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		aliases, err := topology.ListAliases(ctx, tx, host.ID)
		if err != nil {
			return err
		}
		if len(aliases) != 2 {
			t.Fatalf("更新后 alias 数量错误: %+v", aliases)
		}
		entityDeps, err := topology.ListDependenciesForSource(ctx, tx, EntityRegistrySource, host.ID)
		if err != nil {
			return err
		}
		if len(entityDeps) != 1 || entityDeps[0].ToNodeID != providerB.ID || entityDeps[0].Criticality != domain.DependencyCriticalityCritical {
			t.Fatalf("受管实体依赖未按预期整体替换: %+v", entityDeps)
		}
		// service_registry 来源的边（应用服务 -> 该主机）必须原样保留。
		serviceDeps, err := services.ListDependencies(ctx, tx, service.Entity.ID)
		if err != nil {
			return err
		}
		if len(serviceDeps) != 1 || serviceDeps[0].ToNodeID != host.ID {
			t.Fatalf("更新受管实体不应影响 service_registry 边: %+v", serviceDeps)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestManagedEntityDependencySelfAndMissingTargetRejected 验证依赖自身与依赖
// 不存在的目标都会被拒绝，且不存在目标映射为 ErrServiceDependencyInvalid（API 层
// 据此返回 422，而不是把"目标本身不存在"和"受管实体本身不存在"混为一谈）。
func TestManagedEntityDependencySelfAndMissingTargetRejected(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "managed-entity-reject-owner")
	topology := NewTopologyRepo()

	var host domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.13", Layer: domain.EntityLayerInfrastructure,
			Name: "10.61.17.13", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("创建受管实体失败: %v", err)
	}

	// 自依赖：更新时把依赖目标指向自己。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := topology.UpdateManagedEntity(ctx, tx, host.ID, domain.ResourceNode{
			EntityKey: host.EntityKey, Layer: host.Layer, Name: host.Name,
			Environment: host.Environment, OwnerTeamID: team.ID,
		}, nil, []domain.DependencyEdge{{ToNodeID: host.ID, Criticality: domain.DependencyCriticalityMedium}})
		return err
	})
	if !errors.Is(err, ErrServiceDependencyInvalid) {
		t.Fatalf("自依赖应返回 ErrServiceDependencyInvalid，实际: %v", err)
	}

	// 依赖目标不存在。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := topology.UpdateManagedEntity(ctx, tx, host.ID, domain.ResourceNode{
			EntityKey: host.EntityKey, Layer: host.Layer, Name: host.Name,
			Environment: host.Environment, OwnerTeamID: team.ID,
		}, nil, []domain.DependencyEdge{{ToNodeID: "00000000-0000-0000-0000-000000000000", Criticality: domain.DependencyCriticalityMedium}})
		return err
	})
	if !errors.Is(err, ErrServiceDependencyInvalid) {
		t.Fatalf("依赖不存在的目标应返回 ErrServiceDependencyInvalid，实际: %v", err)
	}
}

// TestResolveAliasAcrossMultipleInstancesOfSameManagedHost 验证通过受管实体 API
// 登记的主机 + 两个 instance 别名，能让两条不同 instance 的告警标签都解析到同一
// canonical 实体——这是 AlertMappingPolicy 通过 alias 定位实体（ResolveAlias）的
// 底层机制，Prometheus 场景下一台主机常见暴露多个采集端口/instance。
func TestResolveAliasAcrossMultipleInstancesOfSameManagedHost(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "resolve-alias-owner")
	topology := NewTopologyRepo()
	catalog := NewServiceCatalogRepo()

	var host domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.11", Layer: domain.EntityLayerInfrastructure,
			Name: "10.61.17.11", Environment: "prod", OwnerTeamID: team.ID,
		}, []domain.EntityAlias{
			{Namespace: "prometheus.instance", Value: "10.61.17.11:4646", Enabled: true},
			{Namespace: "prometheus.instance", Value: "10.61.17.11:9100", Enabled: true},
		}, nil)
		return err
	})
	if err != nil {
		t.Fatalf("创建受管主机失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, instance := range []string{"10.61.17.11:4646", "10.61.17.11:9100"} {
			nodes, err := catalog.ResolveAlias(ctx, tx, "prometheus.instance", instance)
			if err != nil {
				return err
			}
			if len(nodes) != 1 || nodes[0].ID != host.ID {
				t.Fatalf("instance=%s 未解析到同一主机实体: %+v", instance, nodes)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 更新时 aliases/dependencies 为 nil 表示调用方未提供，必须保持现状；空切片才清空。
// 回归场景：旧前端或脚本只改名称，不应抹掉实体的别名与依赖边，否则告警映射静默失效。
func TestManagedEntityUpdateNilCollectionsKeepExistingAliasesAndDependencies(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "managed-entity-nil-keep-owner")
	topology := NewTopologyRepo()

	var provider, host domain.ResourceNode
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		provider, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/nomad-keep/prod", Layer: domain.EntityLayerPlatform,
			Name: "Nomad Keep", Environment: "prod", OwnerTeamID: team.ID,
		}, nil, nil)
		if err != nil {
			return err
		}
		host, err = topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "host:10.61.17.13", Layer: domain.EntityLayerInfrastructure,
			Name: "10.61.17.13", Environment: "prod", OwnerTeamID: team.ID,
		}, []domain.EntityAlias{{Namespace: "prometheus.instance", Value: "10.61.17.13:9100", Enabled: true}},
			[]domain.DependencyEdge{{ToNodeID: provider.ID, Criticality: domain.DependencyCriticalityHigh}})
		return err
	})
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	counts := func() (int, int) {
		var aliases, deps int
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			a, err := topology.ListAliases(ctx, tx, host.ID)
			if err != nil {
				return err
			}
			d, err := topology.ListDependenciesForSource(ctx, tx, EntityRegistrySource, host.ID)
			aliases, deps = len(a), len(d)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return aliases, deps
	}

	rename := func(aliases []domain.EntityAlias, deps []domain.DependencyEdge, name string) {
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := topology.UpdateManagedEntity(ctx, tx, host.ID, domain.ResourceNode{
				EntityKey: host.EntityKey, Layer: host.Layer, Name: name, Environment: "prod", OwnerTeamID: team.ID,
			}, aliases, deps)
			return err
		}); err != nil {
			t.Fatalf("更新失败: %v", err)
		}
	}

	rename(nil, nil, "p-ai-sandbox-003")
	if a, d := counts(); a != 1 || d != 1 {
		t.Fatalf("nil 集合应保持现状，got aliases=%d deps=%d", a, d)
	}

	rename([]domain.EntityAlias{}, []domain.DependencyEdge{}, "p-ai-sandbox-003")
	if a, d := counts(); a != 0 || d != 0 {
		t.Fatalf("空切片应清空，got aliases=%d deps=%d", a, d)
	}
}
