//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// mustFault 用最小合法字段插入一个 Fault，供任务 15 的新增方法测试复用。
func mustFault(t *testing.T, ctx context.Context, st *Store, faults *FaultRepo, correlationKey string, status domain.FaultStatus, at time.Time) domain.Fault {
	t.Helper()
	var stored domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		stored, _, err = faults.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: correlationKey,
			Scope:          domain.FaultScopeCapability,
			Status:         domain.FaultStatusCandidate,
			Severity:       domain.SeverityWarning,
			Kind:           "integration_test_fault",
			Title:          correlationKey,
			EntityRef:      correlationKey,
			Confidence:     0.9,
			FirstDetected:  at,
			LastEvaluated:  at,
		})
		return err
	}); err != nil {
		t.Fatalf("创建 Fault 失败: %v", err)
	}
	// 生命周期状态机只允许 candidate->active->monitoring->resolved 逐级流转
	// （domain.ValidateFaultTransition），因此按目标状态依次推进，不能一步跳转。
	chain := []domain.FaultStatus{domain.FaultStatusActive, domain.FaultStatusMonitoring, domain.FaultStatusResolved}
	for _, next := range chain {
		if stored.Status == status {
			break
		}
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			stored, err = faults.UpdateLifecycleStatus(ctx, tx, stored.ID, stored.Status, next, at)
			return err
		}); err != nil {
			t.Fatalf("推进 Fault 状态到 %s 失败: %v", next, err)
		}
	}
	return stored
}

// TestFaultRepoAck 覆盖 R4.2/R4.3：首次 Ack 落库 acked_by/acked_by_name/acked_at
// 且 ok=true；重复 Ack 不覆盖既有认领人，ok=false 且返回当前状态——与
// domain.AckFault 的幂等短路语义一致（不变量 6）。
func TestFaultRepoAck(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := NewFaultRepo()
	user := mustUser(t, ctx, st, "fault-ack-user")
	other := mustUser(t, ctx, st, "fault-ack-other")
	now := time.Now().UTC().Truncate(time.Microsecond)

	fault := mustFault(t, ctx, st, faults, "fault-ack-key", domain.FaultStatusActive, now)

	var first domain.Fault
	var firstOK bool
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, firstOK, err = faults.Ack(ctx, tx, fault.ID, user.ID, "张三", now)
		return err
	}); err != nil {
		t.Fatalf("首次 Ack 失败: %v", err)
	}
	if !firstOK {
		t.Fatal("首次 Ack 应返回 ok=true")
	}
	if first.AckedBy != user.ID || first.AckedByName != "张三" || first.AckedAt.IsZero() {
		t.Fatalf("首次 Ack 结果非预期: %+v", first)
	}

	later := now.Add(time.Minute)
	var second domain.Fault
	var secondOK bool
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		second, secondOK, err = faults.Ack(ctx, tx, fault.ID, other.ID, "李四", later)
		return err
	}); err != nil {
		t.Fatalf("重复 Ack 失败: %v", err)
	}
	if secondOK {
		t.Fatal("重复 Ack 应返回 ok=false（幂等短路）")
	}
	// 认领人不应被第二次调用覆盖。
	if second.AckedBy != user.ID || second.AckedByName != "张三" {
		t.Fatalf("重复 Ack 不应覆盖既有认领人，got %+v", second)
	}
}

// TestFaultRepoAppendTimeline 覆盖 fault_timeline_entries 的写入与字段落地：
// actor_id 可空、actor_name 必填、detail 落 JSONB。
func TestFaultRepoAppendTimeline(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fault := mustFault(t, ctx, st, faults, "fault-timeline-key", domain.FaultStatusActive, now)

	var entryID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		entryID, err = faults.AppendTimeline(ctx, tx, domain.FaultTimelineEntry{
			FaultID: fault.ID,
			Actor:   "system",
			Action:  domain.FaultTimelineActionNotifySuppressed,
			At:      now,
			Detail:  map[string]any{"reason": "upstream_notified", "upstream_fault_id": "upstream-1"},
		})
		return err
	}); err != nil {
		t.Fatalf("写入时间线失败: %v", err)
	}
	if entryID == "" {
		t.Fatal("AppendTimeline 应返回非空 ID")
	}

	var actorID *string
	var actorName, action string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT actor_id::text, actor_name, action FROM fault_timeline_entries WHERE id = $1
		`, entryID).Scan(&actorID, &actorName, &action)
	}); err != nil {
		t.Fatalf("回读时间线失败: %v", err)
	}
	if actorID != nil {
		t.Fatalf("actor_id 应为空（系统动作），got %v", *actorID)
	}
	if actorName != "system" || action != string(domain.FaultTimelineActionNotifySuppressed) {
		t.Fatalf("时间线字段非预期: actor_name=%q action=%q", actorName, action)
	}
}

// TestFaultRepoListRecentlyResolvedByCorrelationKey 覆盖 R5.3 冷却窗口判定：
// since 之前的 resolved 记录不计入，之后的计入；不同 correlation_key 不干扰。
func TestFaultRepoListRecentlyResolvedByCorrelationKey(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)

	_ = mustFault(t, ctx, st, faults, "fault-cooldown-old", domain.FaultStatusResolved, now.Add(-time.Hour))
	recentResolved := mustFault(t, ctx, st, faults, "fault-cooldown-recent", domain.FaultStatusResolved, now.Add(-time.Minute))
	_ = mustFault(t, ctx, st, faults, "fault-cooldown-other-key", domain.FaultStatusResolved, now.Add(-time.Minute))

	var recent []domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		recent, err = faults.ListRecentlyResolvedByCorrelationKey(ctx, tx, "fault-cooldown-recent", now.Add(-30*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("查询冷却窗口失败: %v", err)
	}
	if len(recent) != 1 || recent[0].ID != recentResolved.ID {
		t.Fatalf("冷却窗口查询结果非预期: %+v", recent)
	}

	var old []domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		old, err = faults.ListRecentlyResolvedByCorrelationKey(ctx, tx, "fault-cooldown-old", now.Add(-30*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("查询冷却窗口失败: %v", err)
	}
	if len(old) != 0 {
		t.Fatalf("窗口外的 resolved 记录不应命中，got %+v", old)
	}
}

// TestFaultRepoSetOwnerNotified 覆盖 owner_notified_at 的写入与不存在时报错。
func TestFaultRepoSetOwnerNotified(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fault := mustFault(t, ctx, st, faults, "fault-owner-notified-key", domain.FaultStatusActive, now)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return faults.SetOwnerNotified(ctx, tx, fault.ID, now)
	}); err != nil {
		t.Fatalf("SetOwnerNotified 失败: %v", err)
	}

	var reloaded domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		reloaded, err = faults.GetByID(ctx, tx, fault.ID)
		return err
	}); err != nil {
		t.Fatalf("回读 Fault 失败: %v", err)
	}
	if reloaded.OwnerNotifiedAt.IsZero() {
		t.Fatal("owner_notified_at 应已写入")
	}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return faults.SetOwnerNotified(ctx, tx, "00000000-0000-0000-0000-000000000000", now)
	})
	if err == nil {
		t.Fatal("对不存在的 Fault 调用 SetOwnerNotified 应返回错误")
	}
}
