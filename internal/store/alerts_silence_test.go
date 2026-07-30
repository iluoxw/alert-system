//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestAlertRepo_UpdateSilencedBy_RoundTrip 验证 silenced_by（UUID[] 列）能正确写入
// 与读回，且再次调用会用新的命中集合整体覆盖旧值（而不是追加/合并），呼应
// UpdateSilencedBy 的覆盖写语义（静默过期后下一次求值即可自动清除旧命中）。
func TestAlertRepo_UpdateSilencedBy_RoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	var alertID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		a, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent("fp-silence-1"), "")
		if err != nil {
			return err
		}
		alertID = a.ID
		return nil
	}); err != nil {
		t.Fatalf("准备测试告警失败: %v", err)
	}

	silA, silB := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpdateSilencedBy(ctx, tx, alertID, []string{silA, silB})
	}); err != nil {
		t.Fatalf("UpdateSilencedBy 写入失败: %v", err)
	}

	var got []string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		a, err := repo.GetByID(ctx, tx, alertID)
		if err != nil {
			return err
		}
		got = a.SilencedBy
		return nil
	}); err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if len(got) != 2 || got[0] != silA || got[1] != silB {
		t.Fatalf("silenced_by = %v, want [%s %s]", got, silA, silB)
	}

	// 再次调用：用空集合整体覆盖，验证不是追加。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpdateSilencedBy(ctx, tx, alertID, nil)
	}); err != nil {
		t.Fatalf("UpdateSilencedBy 清空失败: %v", err)
	}
	var got2 []string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		a, err := repo.GetByID(ctx, tx, alertID)
		if err != nil {
			return err
		}
		got2 = a.SilencedBy
		return nil
	}); err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("清空后 silenced_by = %v, want empty", got2)
	}
}

// TestAlertRepo_UpdateSilencedBy_NotFound 验证告警不存在时返回 ErrAlertNotFound，
// 与 UpdateStatus/UpdateRouting 的既有约定一致。
func TestAlertRepo_UpdateSilencedBy_NotFound(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpdateSilencedBy(ctx, tx, "00000000-0000-0000-0000-000000000000", []string{})
	})
	if err == nil {
		t.Fatal("expected ErrAlertNotFound")
	}
}
