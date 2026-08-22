//go:build integration

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"alert-system/internal/store"
)

// 本文件用真实 PostgreSQL 回归 000024 的**回滚**边界。
//
// 起因是一个稳定复现的阻断问题：同一个 (kind, ref_id) 可以合法地同时存在多条
// status='claimed'——timers_pending_kind_ref_uidx 只约束 pending，所以第一轮
// notify_send 还在事务外执行时，同业务元组的新一轮意图会先插入一条新的 pending，
// 随后被另一个 worker 领取，于是出现两条 claimed 且一条 pending 都没有。
// 早先的 down migration 把所有 claimed 一次性改回 pending，在这种形态下直接撞
// 唯一索引：
//
//	ERROR: duplicate key value violates unique constraint "timers_pending_kind_ref_uidx"
//	DETAIL: Key (kind, ref_id)=(notify_send, same-ref) already exists.
//
// 因此回滚必须做到「每个 (kind, ref_id) 至多归还一条 pending，其余 claimed 标 failed」。

const claimLeaseDownMigration = "000024_timer_claim_lease.down.sql"

func applyMigrationFile(t *testing.T, ctx context.Context, st *store.Store, name string) {
	t.Helper()
	sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir, name))
	if err != nil {
		t.Fatalf("读取迁移文件 %s 失败: %v", name, err)
	}
	if _, err := st.Pool.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("执行迁移 %s 失败: %v", name, err)
	}
}

// insertTimerFixture 直接插入一条指定 status 与 created_at 的 timers 行，返回其 id。
// created_at 必须显式给：回滚保留哪一条 claimed 的判定就是按它排序的，靠默认 now()
// 无法在同一个事务里造出不同的时间。
func insertTimerFixture(t *testing.T, ctx context.Context, st *store.Store, refID, status string, createdAt time.Time) string {
	t.Helper()
	// timers_claimed_lease_check 要求 claimed 行必须带租约；其余状态留 NULL。
	var lease any
	if status == "claimed" {
		lease = createdAt.Add(time.Hour)
	}
	var id string
	err := st.Pool.QueryRow(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status, lease_expires_at, created_at)
		VALUES ($1, $2, $3, '{}'::jsonb, $4, $5, $6)
		RETURNING id
	`, detachedKind, refID, createdAt, status, lease, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入 fixture(ref_id=%s status=%s) 失败: %v", refID, status, err)
	}
	return id
}

func statusByID(t *testing.T, ctx context.Context, st *store.Store, id string) string {
	t.Helper()
	var status string
	if err := st.Pool.QueryRow(ctx, `SELECT status FROM timers WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("查询 id=%s 的 status 失败: %v", id, err)
	}
	return status
}

func assertStatus(t *testing.T, ctx context.Context, st *store.Store, label, id, want string) {
	t.Helper()
	if got := statusByID(t, ctx, st, id); got != want {
		t.Errorf("%s 的 status = %q, want %q", label, got, want)
	}
}

// 四个必需场景 + 两个边界（created_at 相同的确定性兜底、已有终态行不受影响）刻意放在
// **同一个库里一次 down 收口**：真实回滚面对的就是这些形态同时存在，分开跑反而测不到
// 「一条语句要同时处理多种元组」这个关键点。
func TestMigration000024Down_ReturnsAtMostOnePendingPerRefKey(t *testing.T) {
	st := setupTestStore(t) // 已按顺序跑完全部 up 迁移（含 000024）
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).UTC()

	// A 单条 claimed、无 pending：应当归还 pending 继续被轮询。
	aClaimed := insertTimerFixture(t, ctx, st, "single-claimed", "claimed", base)

	// B 一条 pending + 一条 claimed：pending 名额已被占，claimed 只能标 failed。
	bPending := insertTimerFixture(t, ctx, st, "claimed-with-pending", "pending", base)
	bClaimed := insertTimerFixture(t, ctx, st, "claimed-with-pending", "claimed", base.Add(time.Minute))

	// C 三条 claimed、无 pending：**这就是线上撞唯一索引的形态**。只有最新那条归还
	// pending，另两条标 failed。
	cOldest := insertTimerFixture(t, ctx, st, "multi-claimed-no-pending", "claimed", base)
	cMiddle := insertTimerFixture(t, ctx, st, "multi-claimed-no-pending", "claimed", base.Add(time.Minute))
	cNewest := insertTimerFixture(t, ctx, st, "multi-claimed-no-pending", "claimed", base.Add(2*time.Minute))

	// D 一条 pending + 两条 claimed：pending 名额已被占，两条 claimed 都标 failed。
	dPending := insertTimerFixture(t, ctx, st, "multi-claimed-with-pending", "pending", base)
	dClaimedOld := insertTimerFixture(t, ctx, st, "multi-claimed-with-pending", "claimed", base.Add(time.Minute))
	dClaimedNew := insertTimerFixture(t, ctx, st, "multi-claimed-with-pending", "claimed", base.Add(2*time.Minute))

	// E 两条 claimed 且 created_at 完全相同：保留哪条必须确定，不能依赖物理行序。
	insertTimerFixture(t, ctx, st, "tie-created-at", "claimed", base)
	insertTimerFixture(t, ctx, st, "tie-created-at", "claimed", base)
	// 期望保留 id 最大的那条（down migration 的 tie-break 是 id DESC）。这里让
	// PostgreSQL 自己按 uuid 排序算出期望值，避免在 Go 侧假设 uuid 的文本序等于字节序。
	var eExpectedKeep string
	if err := st.Pool.QueryRow(ctx, `
		SELECT id FROM timers WHERE kind = $1 AND ref_id = 'tie-created-at'
		ORDER BY id DESC LIMIT 1
	`, detachedKind).Scan(&eExpectedKeep); err != nil {
		t.Fatalf("计算 tie-break 期望保留行失败: %v", err)
	}
	var eExpectedFail string
	if err := st.Pool.QueryRow(ctx, `
		SELECT id FROM timers WHERE kind = $1 AND ref_id = 'tie-created-at'
		ORDER BY id ASC LIMIT 1
	`, detachedKind).Scan(&eExpectedFail); err != nil {
		t.Fatalf("计算 tie-break 期望标失败行失败: %v", err)
	}

	// F 已经是终态的行不属于本次回滚的收口范围，必须原样不动。
	fFired := insertTimerFixture(t, ctx, st, "already-terminal", "fired", base)
	fCanceled := insertTimerFixture(t, ctx, st, "already-terminal-canceled", "canceled", base)

	// 回滚本身不报错就是对第 3 条复现结论的直接回归。
	applyMigrationFile(t, ctx, st, claimLeaseDownMigration)

	t.Run("A_单条claimed归还pending", func(t *testing.T) {
		assertStatus(t, ctx, st, "A claimed", aClaimed, "pending")
	})

	t.Run("B_claimed加pending只保留原pending", func(t *testing.T) {
		assertStatus(t, ctx, st, "B pending", bPending, "pending")
		assertStatus(t, ctx, st, "B claimed", bClaimed, "failed")
	})

	t.Run("C_多条claimed无pending只归还最新一条", func(t *testing.T) {
		assertStatus(t, ctx, st, "C 最新 claimed", cNewest, "pending")
		assertStatus(t, ctx, st, "C 次新 claimed", cMiddle, "failed")
		assertStatus(t, ctx, st, "C 最旧 claimed", cOldest, "failed")
	})

	t.Run("D_多条claimed加pending全部标failed", func(t *testing.T) {
		assertStatus(t, ctx, st, "D pending", dPending, "pending")
		assertStatus(t, ctx, st, "D 较新 claimed", dClaimedNew, "failed")
		assertStatus(t, ctx, st, "D 较旧 claimed", dClaimedOld, "failed")
	})

	t.Run("E_created_at相同时按id确定性保留", func(t *testing.T) {
		assertStatus(t, ctx, st, "E id 最大的 claimed", eExpectedKeep, "pending")
		assertStatus(t, ctx, st, "E id 较小的 claimed", eExpectedFail, "failed")
	})

	t.Run("F_已有终态行不受影响", func(t *testing.T) {
		assertStatus(t, ctx, st, "F fired", fFired, "fired")
		assertStatus(t, ctx, st, "F canceled", fCanceled, "canceled")
	})

	t.Run("回滚后不残留claimed且每个元组至多一条pending", func(t *testing.T) {
		var claimed int
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM timers WHERE status = 'claimed'`).Scan(&claimed); err != nil {
			t.Fatalf("统计残留 claimed 失败: %v", err)
		}
		if claimed != 0 {
			t.Errorf("回滚后仍有 %d 条 claimed，旧代码不认识这个状态", claimed)
		}

		var dupGroups int
		if err := st.Pool.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT kind, ref_id FROM timers WHERE status = 'pending'
				GROUP BY kind, ref_id HAVING count(*) > 1
			) dup
		`).Scan(&dupGroups); err != nil {
			t.Fatalf("统计重复 pending 元组失败: %v", err)
		}
		if dupGroups != 0 {
			t.Errorf("有 %d 个 (kind, ref_id) 出现多条 pending，违反 timers_pending_kind_ref_uidx", dupGroups)
		}
	})

	t.Run("回滚后列结构与status_CHECK恢复改动前形态", func(t *testing.T) {
		for _, col := range []string{"lease_expires_at", "claim_count"} {
			var exists bool
			if err := st.Pool.QueryRow(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM information_schema.columns
					WHERE table_name = 'timers' AND column_name = $1
				)
			`, col).Scan(&exists); err != nil {
				t.Fatalf("查询列 %s 是否存在失败: %v", col, err)
			}
			if exists {
				t.Errorf("回滚后列 %s 仍然存在", col)
			}
		}

		var claimedLeaseCheck bool
		if err := st.Pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'timers'::regclass AND conname = 'timers_claimed_lease_check'
			)
		`).Scan(&claimedLeaseCheck); err != nil {
			t.Fatalf("查询 timers_claimed_lease_check 失败: %v", err)
		}
		if claimedLeaseCheck {
			t.Error("回滚后 timers_claimed_lease_check 仍然存在")
		}

		// 收紧后的 CHECK 用功能性断言验证：直接插 'claimed' 必须被拒绝。
		// 这比比对 pg_get_constraintdef 的文本更能证明约束真的生效了。
		_, err := st.Pool.Exec(ctx, `
			INSERT INTO timers (kind, ref_id, fire_at, status)
			VALUES ($1, 'post-rollback', now(), 'claimed')
		`, detachedKind)
		if err == nil {
			t.Error("回滚后仍能插入 status='claimed'，说明旧 status CHECK 没有恢复")
		}

		// 反向确认：旧的四个合法状态仍然可用，CHECK 没有被收得过紧。
		for _, status := range []string{"pending", "fired", "canceled", "failed"} {
			if _, err := st.Pool.Exec(ctx, `
				INSERT INTO timers (kind, ref_id, fire_at, status)
				VALUES ($1, $2, now(), $3)
			`, detachedKind, "post-rollback-"+status, status); err != nil {
				t.Errorf("回滚后插入合法状态 %q 失败: %v", status, err)
			}
		}
	})
}
