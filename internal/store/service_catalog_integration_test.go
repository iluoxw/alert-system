//go:build integration

// Service Registration 分类（layer/kind）在真实 PostgreSQL 上的 round-trip 与不可变
// 口径。这里必须用真库：被测行为本身就是"Update 在调用方事务内读取当前分类并保留 /
// 拒绝迁移"，以及现有 CHECK 约束确实接受两个目标 pair（因此 Phase 2 不需要新迁移）。
//
//	go test -tags=integration ./internal/store -run 'ServiceCatalog.*Classification' -count=1
package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func newClassifiedService(entityKey, name, teamID string, layer domain.EntityLayer, kind domain.ResourceNodeKind) domain.ServiceRegistration {
	return domain.ServiceRegistration{
		Entity: domain.ResourceNode{
			EntityKey: entityKey, Layer: layer, Kind: kind, Managed: true,
			Name: name, Environment: "prod", OwnerTeamID: teamID,
		},
		OwnerTeamID: teamID, Tier: domain.ServiceTier1, Enabled: true,
	}
}

// 两类登记都能落库并原样读回。platform/infrastructure_capability 是 E2B 这类共享
// 基础设施能力的登记方式，Phase 1 的固定 application/business_system 写法无法表达它。
func TestServiceCatalogRepoClassificationRoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "classification-owner")
	repo := NewServiceCatalogRepo()

	cases := []struct {
		entityKey string
		name      string
		layer     domain.EntityLayer
		kind      domain.ResourceNodeKind
	}{
		{"service/cch/prod", "CCH", domain.EntityLayerApplication, domain.ResourceNodeKindBusinessSystem},
		{"platform/e2b/prod", "E2B", domain.EntityLayerPlatform, domain.ResourceNodeKindInfrastructureCapability},
	}
	for _, tc := range cases {
		var created domain.ServiceRegistration
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			created, err = repo.Create(ctx, tx, newClassifiedService(tc.entityKey, tc.name, team.ID, tc.layer, tc.kind))
			return err
		}); err != nil {
			t.Fatalf("登记 %s 失败: %v", tc.entityKey, err)
		}
		if created.Entity.Layer != tc.layer || created.Entity.Kind != tc.kind {
			t.Fatalf("%s create 分类=%q/%q want %q/%q", tc.entityKey,
				created.Entity.Layer, created.Entity.Kind, tc.layer, tc.kind)
		}

		var reread domain.ServiceRegistration
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			reread, err = repo.GetByID(ctx, tx, created.ID)
			return err
		}); err != nil {
			t.Fatalf("回读 %s 失败: %v", tc.entityKey, err)
		}
		if reread.Entity.Layer != tc.layer || reread.Entity.Kind != tc.kind {
			t.Fatalf("%s select 分类=%q/%q want %q/%q", tc.entityKey,
				reread.Entity.Layer, reread.Entity.Kind, tc.layer, tc.kind)
		}
	}
}

// 分类缺省的 create 请求必须被 store 拒绝：缺省补齐是 API 边界的旧客户端兼容行为，
// store 不再写死 application/business_system。
func TestServiceCatalogRepoClassificationRequiredOnCreate(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "classification-required")
	repo := NewServiceCatalogRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, createErr := repo.Create(ctx, tx, domain.ServiceRegistration{
			Entity: domain.ResourceNode{
				EntityKey: "service/no-class/prod", Name: "无分类", Environment: "prod", OwnerTeamID: team.ID,
			},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier2, Enabled: true,
		})
		return createErr
	})
	if !errors.Is(err, domain.ErrInvalidServiceClassification) {
		t.Fatalf("create 缺分类应返回 ErrInvalidServiceClassification, got %v", err)
	}
}

// update 不带分类时保留当前分类。这是旧客户端最危险的一条路径：它们完全不知道
// entity_layer 字段，如果 store 回落到 application，一次编辑就会把 platform 服务
// 静默降级，并连带改写爆炸半径与影响投影。
func TestServiceCatalogRepoClassificationPreservedOnUpdate(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "classification-preserve")
	repo := NewServiceCatalogRepo()

	var created domain.ServiceRegistration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = repo.Create(ctx, tx, newClassifiedService(
			"platform/e2b/prod", "E2B", team.ID,
			domain.EntityLayerPlatform, domain.ResourceNodeKindInfrastructureCapability))
		return err
	}); err != nil {
		t.Fatalf("登记 platform 服务失败: %v", err)
	}

	legacy := newClassifiedService("platform/e2b/prod", "E2B 更新后", team.ID, "", "")
	legacy.Description = "旧客户端不带分类字段"
	var updated domain.ServiceRegistration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		updated, err = repo.Update(ctx, tx, created.ID, legacy)
		return err
	}); err != nil {
		t.Fatalf("旧客户端 update 失败: %v", err)
	}
	if updated.Entity.Layer != domain.EntityLayerPlatform ||
		updated.Entity.Kind != domain.ResourceNodeKindInfrastructureCapability {
		t.Fatalf("update 后分类=%q/%q，platform 登记被静默改写",
			updated.Entity.Layer, updated.Entity.Kind)
	}
	if updated.Entity.Name != "E2B 更新后" || updated.Description != "旧客户端不带分类字段" {
		t.Fatalf("保留分类不应吞掉其它字段更新: %+v", updated.Entity)
	}
}

// 显式传入与当前相同的合法 pair 可接受（幂等），传入不同 pair 必须拒绝：分类迁移
// 涉及 confirmed Impact 人工裁决与重投影，不属于 Phase 2 的自动路径。
func TestServiceCatalogRepoClassificationImmutableOnUpdate(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "classification-immutable")
	repo := NewServiceCatalogRepo()

	var created domain.ServiceRegistration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = repo.Create(ctx, tx, newClassifiedService(
			"platform/e2b/prod", "E2B", team.ID,
			domain.EntityLayerPlatform, domain.ResourceNodeKindInfrastructureCapability))
		return err
	}); err != nil {
		t.Fatalf("登记 platform 服务失败: %v", err)
	}

	same := newClassifiedService("platform/e2b/prod", "E2B", team.ID,
		domain.EntityLayerPlatform, domain.ResourceNodeKindInfrastructureCapability)
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, updateErr := repo.Update(ctx, tx, created.ID, same)
		return updateErr
	}); err != nil {
		t.Fatalf("显式传入相同分类应可接受: %v", err)
	}

	migrated := newClassifiedService("platform/e2b/prod", "E2B", team.ID,
		domain.EntityLayerApplication, domain.ResourceNodeKindBusinessSystem)
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, updateErr := repo.Update(ctx, tx, created.ID, migrated)
		return updateErr
	})
	if !errors.Is(err, ErrServiceClassificationImmutable) {
		t.Fatalf("跨 pair update 应返回 ErrServiceClassificationImmutable, got %v", err)
	}

	var reread domain.ServiceRegistration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		reread, getErr = repo.GetByID(ctx, tx, created.ID)
		return getErr
	}); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reread.Entity.Layer != domain.EntityLayerPlatform ||
		reread.Entity.Kind != domain.ResourceNodeKindInfrastructureCapability {
		t.Fatalf("被拒绝的迁移不得留下部分写入: %q/%q", reread.Entity.Layer, reread.Entity.Kind)
	}
}

// 非法 pair 在 store 层同样被拒绝，不依赖 API 校验兜底，也不落到 DB CHECK 才报错。
func TestServiceCatalogRepoClassificationRejectsInvalidPair(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "classification-invalid")
	repo := NewServiceCatalogRepo()

	cases := []struct {
		layer domain.EntityLayer
		kind  domain.ResourceNodeKind
	}{
		{domain.EntityLayerInfrastructure, domain.ResourceNodeKindResource},
		{domain.EntityLayerPlatform, domain.ResourceNodeKindBusinessSystem},
		{domain.EntityLayerApplication, domain.ResourceNodeKindInfrastructureCapability},
	}
	for _, tc := range cases {
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			_, createErr := repo.Create(ctx, tx, newClassifiedService(
				"service/invalid/prod", "非法分类", team.ID, tc.layer, tc.kind))
			return createErr
		})
		if !errors.Is(err, domain.ErrInvalidServiceClassification) {
			t.Fatalf("%q/%q 应被 store 拒绝, got %v", tc.layer, tc.kind, err)
		}
	}
}
