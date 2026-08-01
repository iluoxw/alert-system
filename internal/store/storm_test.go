//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件依赖迁移 000008（route_storm_state 表、C3）——若该迁移尚未合入
// migrations/ 目录，本文件测试会因表不存在而失败，属于任务 4 与任务 1（迁移）
// 并行时序下的正常现象，迁移合入后复跑即可。setupTestStore/applyMigrations
// 复用 events_test.go 的既有实现（同包，无需重复定义）。

// seedStormRoute 准备一条测试路由：route_storm_state.route_id 外键指向 routes，
// 而 routes.target_team_id 又非空外键指向 teams，因此需要先建团队再建路由。
// 每个测试各自起一个全新容器（setupTestStore 每次调用都新建），不会有 priority
// 唯一约束冲突，固定写 1 即可。
func seedStormRoute(t *testing.T, ctx context.Context, st *Store) string {
	t.Helper()
	var teamID string
	if err := st.Pool.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('storm-test-team') RETURNING id`).Scan(&teamID); err != nil {
		t.Fatalf("准备测试团队失败: %v", err)
	}
	var routeID string
	err := st.Pool.QueryRow(ctx, `
		INSERT INTO routes (priority, matchers, target_team_id, group_by)
		VALUES (1, '[]'::jsonb, $1, '[]'::jsonb)
		RETURNING id
	`, teamID).Scan(&routeID)
	if err != nil {
		t.Fatalf("准备测试路由失败: %v", err)
	}
	return routeID
}

// TestStormStateRepo_Get_NoRow_ReturnsNilNil 验证「从未进入过熔断」是 (nil, nil)
// 而不是 ErrNotFound——这是本仓储与包内其余 GetByID 惯例的刻意差异。
func TestStormStateRepo_Get_NoRow_ReturnsNilNil(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			t.Fatalf("Get 不应返回 error，got %v", err)
		}
		if state != nil {
			t.Fatalf("从未进入过熔断应返回 nil，got %+v", state)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_Activate_UpsertRoundTrip 验证 Activate 的 UPSERT 往返：
// 首次调用插入新行，字段与传入值一致；deactivated_at 为 nil。
func TestStormStateRepo_Activate_UpsertRoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state == nil {
			t.Fatal("Activate 后 Get 应返回非 nil")
		}
		if !state.Active {
			t.Error("Activate 后 Active 应为 true")
		}
		if !state.ActivatedAt.Equal(now) {
			t.Errorf("ActivatedAt = %v, want %v", state.ActivatedAt, now)
		}
		if state.DeactivatedAt != nil {
			t.Errorf("首次 Activate 后 DeactivatedAt 应为 nil，got %v", *state.DeactivatedAt)
		}
		if state.SuppressedCount != 0 {
			t.Errorf("首次 Activate 后 SuppressedCount 应为 0，got %d", state.SuppressedCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_Activate_RepeatedDoesNotResetCount 验证「已 active 时重复
// Activate 不清零 suppressed_count」——只有跨越 false->true 边界才重置。
func TestStormStateRepo_Activate_RepeatedDoesNotResetCount(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		if err := repo.AddSuppressed(ctx, tx, routeID, 7); err != nil {
			return err
		}
		// 同一 flush 窗口内再次判定超阈值，重复 Activate（activated_at 前移）。
		if err := repo.Activate(ctx, tx, routeID, now.Add(time.Second)); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state.SuppressedCount != 7 {
			t.Errorf("已 active 时重复 Activate 不应清零计数，got %d, want 7", state.SuppressedCount)
		}
		if !state.ActivatedAt.Equal(now.Add(time.Second)) {
			t.Errorf("重复 Activate 仍应更新 activated_at，got %v", state.ActivatedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_Activate_AfterDeactivate_ResetsCount 验证「从非 active
// 转入 active」（此处是 Deactivate 后重新进入）会把计数重置为 0。
func TestStormStateRepo_Activate_AfterDeactivate_ResetsCount(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		if err := repo.AddSuppressed(ctx, tx, routeID, 5); err != nil {
			return err
		}
		if err := repo.Deactivate(ctx, tx, routeID, now.Add(time.Minute)); err != nil {
			return err
		}
		// 风暴再次抬头，重新进入熔断。
		if err := repo.Activate(ctx, tx, routeID, now.Add(2*time.Minute)); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state.SuppressedCount != 0 {
			t.Errorf("从 Deactivate 重新 Activate 应重置计数为 0，got %d", state.SuppressedCount)
		}
		if state.DeactivatedAt != nil {
			t.Errorf("重新 Activate 后 DeactivatedAt 应被清空，got %v", *state.DeactivatedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_AddSuppressed_Accumulates 验证 AddSuppressed 多次调用累加。
func TestStormStateRepo_AddSuppressed_Accumulates(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		if err := repo.AddSuppressed(ctx, tx, routeID, 3); err != nil {
			return err
		}
		if err := repo.AddSuppressed(ctx, tx, routeID, 4); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state.SuppressedCount != 7 {
			t.Errorf("累加后 SuppressedCount = %d, want 7", state.SuppressedCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_AddSuppressed_NoRow_ReturnsErrNotFound 验证行不存在时
// AddSuppressed 返回 ErrNotFound（要求调用方先 Activate）。
func TestStormStateRepo_AddSuppressed_NoRow_ReturnsErrNotFound(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.AddSuppressed(ctx, tx, routeID, 1)
	})
	if err == nil {
		t.Fatal("行不存在时 AddSuppressed 应返回错误")
	}
}

// TestStormStateRepo_MarkSummary_SetsTimestamp 验证 MarkSummary 写入
// last_summary_at。
func TestStormStateRepo_MarkSummary_SetsTimestamp(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state.LastSummaryAt != nil {
			t.Fatalf("Activate 后 LastSummaryAt 应为 nil，got %v", *state.LastSummaryAt)
		}
		summaryAt := now.Add(time.Minute)
		if err := repo.MarkSummary(ctx, tx, routeID, summaryAt); err != nil {
			return err
		}
		state, err = repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state.LastSummaryAt == nil || !state.LastSummaryAt.Equal(summaryAt) {
			t.Errorf("LastSummaryAt = %v, want %v", state.LastSummaryAt, summaryAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_Deactivate_RowPersistsWithCorrectFields 验证 Deactivate
// 后行保留不删，字段正确（AC3.2：进入/解除时刻即可查询记录）。
func TestStormStateRepo_Deactivate_RowPersistsWithCorrectFields(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	deactivatedAt := now.Add(5 * time.Minute)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.Activate(ctx, tx, routeID, now); err != nil {
			return err
		}
		if err := repo.AddSuppressed(ctx, tx, routeID, 2); err != nil {
			return err
		}
		if err := repo.Deactivate(ctx, tx, routeID, deactivatedAt); err != nil {
			return err
		}
		state, err := repo.Get(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if state == nil {
			t.Fatal("Deactivate 后行应保留，Get 不应返回 nil")
		}
		if state.Active {
			t.Error("Deactivate 后 Active 应为 false")
		}
		if state.DeactivatedAt == nil || !state.DeactivatedAt.Equal(deactivatedAt) {
			t.Errorf("DeactivatedAt = %v, want %v", state.DeactivatedAt, deactivatedAt)
		}
		if state.SuppressedCount != 2 {
			t.Errorf("Deactivate 不应改动 SuppressedCount，got %d, want 2", state.SuppressedCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestStormStateRepo_Deactivate_NoRow_ReturnsErrNotFound 验证行不存在时
// Deactivate 返回 ErrNotFound。
func TestStormStateRepo_Deactivate_NoRow_ReturnsErrNotFound(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewStormStateRepo()
	routeID := seedStormRoute(t, ctx, st)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.Deactivate(ctx, tx, routeID, now)
	})
	if err == nil {
		t.Fatal("行不存在时 Deactivate 应返回错误")
	}
}
