//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// TestFaultRepoUpsertPersistsOwnerTeamID 覆盖任务 14 的核心缺陷：owner_team_id
// 必须真正落库并能读回（R3.2），而不仅仅是 projector 侧组装的内存字段。同时验证
// 无 owner 时为空、以及二次 Upsert 传空 owner 不会把已有 owner 冲成 NULL——否则
// 一次拓扑短暂缺失就会把已经生效的归属抹掉（不变量 15）。
func TestFaultRepoUpsertPersistsOwnerTeamID(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := NewFaultRepo()
	team := mustTeam(t, ctx, st, "fault-owner-team")
	now := time.Now().UTC().Truncate(time.Microsecond)

	input := func(key string, ownerTeamID string) domain.Fault {
		return domain.Fault{
			CorrelationKey: key,
			Scope:          domain.FaultScopeCapability,
			Status:         domain.FaultStatusCandidate,
			Severity:       domain.SeverityWarning,
			Kind:           "integration_test_fault",
			Title:          key,
			EntityRef:      key,
			OwnerTeamID:    ownerTeamID,
			Confidence:     0.9,
			FirstDetected:  now,
			LastEvaluated:  now,
		}
	}

	t.Run("有owner时落库并可读回", func(t *testing.T) {
		var stored domain.Fault
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			stored, _, err = faults.Upsert(ctx, tx, input("fault-owner-has-owner", team.ID))
			return err
		}); err != nil {
			t.Fatalf("Upsert 失败: %v", err)
		}
		if stored.OwnerTeamID != team.ID {
			t.Fatalf("Upsert 返回 OwnerTeamID = %q, want %q", stored.OwnerTeamID, team.ID)
		}

		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			reloaded, err := faults.GetByID(ctx, tx, stored.ID)
			if err != nil {
				return err
			}
			if reloaded.OwnerTeamID != team.ID {
				t.Fatalf("GetByID 读回 OwnerTeamID = %q, want %q", reloaded.OwnerTeamID, team.ID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("无owner时为空", func(t *testing.T) {
		var stored domain.Fault
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			stored, _, err = faults.Upsert(ctx, tx, input("fault-owner-no-owner", ""))
			return err
		}); err != nil {
			t.Fatalf("Upsert 失败: %v", err)
		}
		if stored.OwnerTeamID != "" {
			t.Fatalf("OwnerTeamID = %q, want 空字符串", stored.OwnerTeamID)
		}
	})

	t.Run("二次Upsert传空owner不清空已有owner", func(t *testing.T) {
		key := "fault-owner-keep-existing"
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			_, _, err := faults.Upsert(ctx, tx, input(key, team.ID))
			return err
		}); err != nil {
			t.Fatalf("首次 Upsert 失败: %v", err)
		}

		var second domain.Fault
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			// 模拟解析不到 owner 的后续评估轮次（例如实体登记暂时缺失）。
			second, _, err = faults.Upsert(ctx, tx, input(key, ""))
			return err
		}); err != nil {
			t.Fatalf("二次 Upsert 失败: %v", err)
		}
		if second.OwnerTeamID != team.ID {
			t.Fatalf("二次 Upsert 传空 owner 后 OwnerTeamID = %q, want 保留原值 %q", second.OwnerTeamID, team.ID)
		}
	})
}
