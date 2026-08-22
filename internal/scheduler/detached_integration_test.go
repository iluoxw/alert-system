//go:build integration

package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/store"
)

// 本文件聚焦「事务外执行」这一条执行路径（scheduler.DetachedHandler，生产上只有
// notify_send 走它），容器/建库、testClock、waitForStatus 等基础设施复用
// scheduler_integration_test.go。覆盖的场景与不变量一一对应：
//
//	① 正常成功        ClaimIsCommittedAndLockReleasedBeforeHandler / SuccessSettlesToFired
//	② handler error   HandlerError_MarksFailed
//	③ handler panic   HandlerPanic_MarksFailedAndWorkerSurvives
//	④ claim 后崩溃    ExpiredClaim_IsReclaimedAndRetried
//	⑤ 并发 worker     ConcurrentWorkers_ExecuteExactlyOnce
//	⑥ 租约未过期      LiveClaim_IsNotStolen
//	⑦ fencing token   SettleClaim_StaleTokenDoesNotOverwrite
//	⑧ 轮询取消        CancelDuringHandler_LeavesClaimedThenReclaimFires
//	⑨ 成功后取消      SuccessThenCancel_StillSettlesFired

const detachedKind = "notify_send"

// timerRecord 是 timers 表里与 claim/lease 有关的那几列。
type timerRecord struct {
	id         string
	status     string
	claimCount int
	lease      *time.Time
}

func readTimer(t *testing.T, ctx context.Context, st *store.Store, kind, refID string) timerRecord {
	t.Helper()
	var rec timerRecord
	err := st.Pool.QueryRow(ctx, `
		SELECT id, status, claim_count, lease_expires_at
		FROM timers WHERE kind = $1 AND ref_id = $2
	`, kind, refID).Scan(&rec.id, &rec.status, &rec.claimCount, &rec.lease)
	if err != nil {
		t.Fatalf("读取定时器 kind=%s ref_id=%s 失败: %v", kind, refID, err)
	}
	return rec
}

// insertClaimedTimer 直接插入一条 claimed 记录，用来构造「上一个执行者已经领走这条
// 定时器，然后进程就死了」的历史状态——真实的进程崩溃无法在同一个测试进程里制造，
// 但崩溃留在数据库里的痕迹恰好就是这一行。
func insertClaimedTimer(t *testing.T, ctx context.Context, st *store.Store, refID string, fireAt, leaseExpiresAt time.Time, claimCount int) {
	t.Helper()
	_, err := st.Pool.Exec(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status, lease_expires_at, claim_count)
		VALUES ($1, $2, $3, '{}'::jsonb, 'claimed', $4, $5)
	`, detachedKind, refID, fireAt, leaseExpiresAt, claimCount)
	if err != nil {
		t.Fatalf("插入 claimed 定时器失败: %v", err)
	}
}

func newDetachedWorker(t *testing.T, st *store.Store, clock Clock, leaseTTL time.Duration, h DetachedHandler) *Scheduler {
	t.Helper()
	s := New(st, 15*time.Millisecond)
	s.clock = clock
	s.leaseTTL = leaseTTL
	s.RegisterDetachedHandler(detachedKind, h)
	return s
}

func scheduleDetached(t *testing.T, ctx context.Context, s *Scheduler, refID string, fireAt time.Time) {
	t.Helper()
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		return s.Schedule(ctx, tx, detachedKind, refID, fireAt, nil)
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
}

// ① 正常成功。核心断言是 handler 执行期间的可观测状态：
//   - claim 已经提交（独立连接能读到 status='claimed'、claim_count=1、租约在未来）；
//   - 这一行没有被任何事务持有行锁（独立连接 FOR UPDATE NOWAIT 能立刻拿到锁）——
//     这正是"外部 IO 不在数据库事务和行锁内"的直接证据；
//   - 状态不是 fired：claim 不是终态，崩了还能被重新领取（不变量：禁止发送前永久标 fired）。
//
// 执行成功后才回写 fired 并释放租约。
func TestScheduler_DetachedHandler_ClaimCommittedAndLockReleasedDuringHandler(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	type observed struct {
		status     string
		claimCount int
		lease      *time.Time
		lockErr    error
	}
	seen := make(chan observed, 4)

	var calls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(hctx context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)

		var o observed
		// 独立连接 + 独立事务：FOR UPDATE NOWAIT 在行锁被别人持有时会立刻报
		// 55P03(lock_not_available) 而不是阻塞，所以它拿到锁就等于"没人持有这一行"。
		o.lockErr = st.WithTx(hctx, func(tx pgx.Tx) error {
			return tx.QueryRow(hctx, `
				SELECT status, claim_count, lease_expires_at
				FROM timers WHERE kind = $1 AND ref_id = $2
				FOR UPDATE NOWAIT
			`, detachedKind, "alert-d1").Scan(&o.status, &o.claimCount, &o.lease)
		})
		seen <- o
		return nil
	})

	scheduleDetached(t, ctx, sched, "alert-d1", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))

	var o observed
	select {
	case o = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("等待事务外 handler 被调用超时")
	}

	if o.lockErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(o.lockErr, &pgErr) && pgErr.Code == "55P03" {
			t.Fatalf("handler 执行期间 timers 行仍被持有行锁（55P03），说明外部 IO 还在 scheduler 事务里: %v", o.lockErr)
		}
		t.Fatalf("handler 执行期间观察 timers 行失败: %v", o.lockErr)
	}
	if o.status != "claimed" {
		t.Fatalf("handler 执行期间 status = %q, want claimed（claim 必须先提交，且不能是终态 fired）", o.status)
	}
	if o.claimCount != 1 {
		t.Fatalf("handler 执行期间 claim_count = %d, want 1", o.claimCount)
	}
	if o.lease == nil || !o.lease.After(clock.Now()) {
		t.Fatalf("handler 执行期间租约 = %v, want 晚于当前时间 %v", o.lease, clock.Now())
	}

	waitForStatus(t, ctx, st, detachedKind, "alert-d1", "fired", 5*time.Second)
	rec := readTimer(t, ctx, st, detachedKind, "alert-d1")
	if rec.lease != nil {
		t.Fatalf("回写终态后 lease_expires_at = %v, want NULL（租约必须随终态释放）", rec.lease)
	}
	if rec.claimCount != 1 {
		t.Fatalf("回写终态后 claim_count = %d, want 1（成功路径不应重复领取）", rec.claimCount)
	}

	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 恰好 1 次", got)
	}
}

// ①的反向对照：同一个探针（FOR UPDATE NOWAIT）在默认的事务内执行模式下必须报
// 55P03——handler 全程被 scheduler 的事务和 timers 行锁包着。没有这个对照，上面那条
// "拿到锁"的断言可能只是因为探针本身写错了而恒真；有了它，①才真正证明了
// notify_send 的外部 IO 已经搬出事务与行锁。
func TestScheduler_TxHandler_StillHoldsRowLockDuringHandler(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	sched := New(st, 15*time.Millisecond)
	sched.clock = clock

	lockErrs := make(chan error, 4)
	sched.RegisterHandler("in_tx", func(hctx context.Context, _ pgx.Tx, _ string, _ []byte) error {
		lockErrs <- st.WithTx(hctx, func(tx pgx.Tx) error {
			var status string
			return tx.QueryRow(hctx, `
				SELECT status FROM timers WHERE kind = 'in_tx' AND ref_id = 'alert-lock'
				FOR UPDATE NOWAIT
			`).Scan(&status)
		})
		return nil
	})

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "in_tx", "alert-lock", clock.Now().Add(10*time.Millisecond), nil)
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))

	var lockErr error
	select {
	case lockErr = <-lockErrs:
	case <-time.After(5 * time.Second):
		t.Fatal("等待事务内 handler 被调用超时")
	}

	var pgErr *pgconn.PgError
	if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("事务内 handler 执行期间探针错误 = %v, want 55P03 lock_not_available（该模式本就持有行锁）", lockErr)
	}
}

// ② handler 返回 error：按既有失败策略收口成 failed、不重试，租约释放，worker 存活。
func TestScheduler_DetachedHandler_HandlerError_MarksFailed(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	var calls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("模拟渠道发送失败")
	})

	scheduleDetached(t, ctx, sched, "alert-d2", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, detachedKind, "alert-d2", "failed", 5*time.Second)

	rec := readTimer(t, ctx, st, detachedKind, "alert-d2")
	if rec.lease != nil {
		t.Fatalf("failed 之后 lease_expires_at = %v, want NULL", rec.lease)
	}

	// failed 是终态，不能被 fetchOneDue 再次取出：多等几轮轮询确认没有重投。
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 1（failed 不重试）", got)
	}

	// worker 必须还活着：再挂一条正常的事务内定时器验证。
	sched.RegisterHandler("ok", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error { return nil })
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ok", "alert-d2-after", clock.Now().Add(10*time.Millisecond), nil)
	}); err != nil {
		t.Fatalf("失败之后再次 Schedule 失败: %v", err)
	}
	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, "ok", "alert-d2-after", "fired", 5*time.Second)
}

// ③ handler panic：与返回 error 同一条收口路径（failed），且 panic 不能杀掉 worker
// ——事务外 handler 跑在轮询 goroutine 上，没有 recover 就是进程级崩溃。
func TestScheduler_DetachedHandler_HandlerPanic_MarksFailedAndWorkerSurvives(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	var okCalls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, refID string, _ []byte) error {
		if refID == "alert-d3" {
			panic("模拟事务外 handler 崩溃")
		}
		atomic.AddInt32(&okCalls, 1)
		return nil
	})

	scheduleDetached(t, ctx, sched, "alert-d3", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, detachedKind, "alert-d3", "failed", 5*time.Second)

	rec := readTimer(t, ctx, st, detachedKind, "alert-d3")
	if rec.lease != nil {
		t.Fatalf("panic 收口后 lease_expires_at = %v, want NULL", rec.lease)
	}

	// panic 之后同一个 worker 继续处理后续的事务外定时器。
	scheduleDetached(t, ctx, sched, "alert-d3-after", clock.Now().Add(10*time.Millisecond))
	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, detachedKind, "alert-d3-after", "fired", 5*time.Second)
	if got := atomic.LoadInt32(&okCalls); got != 1 {
		t.Fatalf("panic 之后正常 handler 被调用 %d 次, want 1（worker 不应因 panic 死掉）", got)
	}
}

// ④ claim 之后崩溃的恢复：数据库里留下一条 claimed + 租约已过期的行，新 worker 必须
// 重新领取（claim_count 递增）、再执行一次、并收口成终态。这就是"禁止发送前永久标
// fired"的兑现——崩溃窗口里的通知不会永久丢失，代价是 at-least-once：如果崩溃发生在
// 「消息已经发出、结果还没确认」之后，这次重投会把它真的再发一次，notify.Sender 的
// 幂等键收敛不了这个窗口（只收敛逻辑性的重复调度）。
func TestScheduler_DetachedHandler_ExpiredClaim_IsReclaimedAndRetried(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	clock := newTestClock(now)

	// 上一个执行者在 5 分钟前领走了它、租约 1 分钟前就到期了，然后没有回写终态。
	insertClaimedTimer(t, ctx, st, "alert-d4", now.Add(-10*time.Minute), now.Add(-time.Minute), 1)

	var calls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	waitForStatus(t, ctx, st, detachedKind, "alert-d4", "fired", 5*time.Second)

	rec := readTimer(t, ctx, st, detachedKind, "alert-d4")
	if rec.claimCount != 2 {
		t.Fatalf("重新领取后 claim_count = %d, want 2（过期 claim 必须被重新领取）", rec.claimCount)
	}
	if rec.lease != nil {
		t.Fatalf("收口后 lease_expires_at = %v, want NULL", rec.lease)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 1", got)
	}
}

// ⑤ 两个 worker 并发轮询同一个库：handler 故意执行得比轮询间隔慢很多，制造出足够宽的
// 窗口——另一个 worker 在这段时间里会轮询很多次，但因为 claim 已提交且租约未过期，
// 它看不到这条定时器，handler 只会被执行一次。
func TestScheduler_DetachedHandler_ConcurrentWorkers_ExecuteExactlyOnce(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	var calls int32
	var concurrent int32
	handler := func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		if n := atomic.AddInt32(&concurrent, 1); n > 1 {
			t.Errorf("同一条定时器被 %d 个 worker 同时执行", n)
		}
		time.Sleep(300 * time.Millisecond) // 远大于 15ms 轮询间隔
		atomic.AddInt32(&concurrent, -1)
		return nil
	}

	w1 := newDetachedWorker(t, st, clock, time.Minute, handler)
	w2 := newDetachedWorker(t, st, clock, time.Minute, handler)

	scheduleDetached(t, ctx, w1, "alert-d5", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w1.Run(runCtx)
	go w2.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, detachedKind, "alert-d5", "fired", 5*time.Second)

	time.Sleep(200 * time.Millisecond) // 再多给几轮轮询，确认没有被重复领取
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 恰好 1 次", got)
	}
	rec := readTimer(t, ctx, st, detachedKind, "alert-d5")
	if rec.claimCount != 1 {
		t.Fatalf("claim_count = %d, want 1（租约未过期不得被另一个 worker 重新领取）", rec.claimCount)
	}
}

// ⑥ 租约仍然有效的 claimed 行（对应"另一个进程正在发送中"）绝不能被抢走：
// 轮询查询必须完全看不到它，否则就会有两个进程同时发同一条通知。
func TestScheduler_DetachedHandler_LiveClaim_IsNotStolen(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	clock := newTestClock(now)

	insertClaimedTimer(t, ctx, st, "alert-d6", now.Add(-time.Minute), now.Add(time.Hour), 1)

	var calls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	time.Sleep(300 * time.Millisecond) // 足够跑很多轮轮询

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("handler 被调用 %d 次, want 0（租约有效的 claim 不能被重新领取）", got)
	}
	rec := readTimer(t, ctx, st, detachedKind, "alert-d6")
	if rec.status != "claimed" || rec.claimCount != 1 {
		t.Fatalf("租约有效期间 status=%q claim_count=%d, want claimed/1", rec.status, rec.claimCount)
	}

	// 租约过期后（把时钟推过租约时刻）立刻可以被领取，证明⑥的"看不见"只源于租约而非别的原因。
	clock.Set(now.Add(2 * time.Hour))
	waitForStatus(t, ctx, st, detachedKind, "alert-d6", "fired", 5*time.Second)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("租约过期后 handler 被调用 %d 次, want 1", got)
	}
}

// ⑦ fencing token：租约过期后被别的 worker 重新领取，上一个执行者迟到的回写必须落空，
// 不能把新一轮的 claim 覆盖成终态（否则新执行者发完通知后无处收口，或者一条正在
// 执行中的定时器被提前判成 fired）。
func TestScheduler_SettleClaim_StaleTokenDoesNotOverwriteReclaim(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	sched := New(st, time.Hour)
	scheduleDetached(t, ctx, sched, "alert-d7", now)
	rec := readTimer(t, ctx, st, detachedKind, "alert-d7")

	var first, second int
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = claimTimer(ctx, tx, rec.id, now.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("首次 claim 失败: %v", err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		second, err = claimTimer(ctx, tx, rec.id, now.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("重新 claim 失败: %v", err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("claim_count 序列 = %d,%d, want 1,2", first, second)
	}

	var settled bool
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		settled, err = settleClaim(ctx, tx, rec.id, "fired", first)
		return err
	}); err != nil {
		t.Fatalf("用过期 token 回写终态时出错: %v", err)
	}
	if settled {
		t.Fatal("用已经失效的 claim token 回写终态必须落空（settled=false）")
	}
	if got := readTimer(t, ctx, st, detachedKind, "alert-d7"); got.status != "claimed" {
		t.Fatalf("过期 token 回写后 status = %q, want claimed（不得被覆盖）", got.status)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		settled, err = settleClaim(ctx, tx, rec.id, "fired", second)
		return err
	}); err != nil {
		t.Fatalf("用当前 token 回写终态时出错: %v", err)
	}
	if !settled {
		t.Fatal("持有当前 claim token 的执行者必须能回写终态")
	}
	final := readTimer(t, ctx, st, detachedKind, "alert-d7")
	if final.status != "fired" || final.lease != nil {
		t.Fatalf("最终 status=%q lease=%v, want fired/NULL", final.status, final.lease)
	}
}

// ⑧ SIGTERM / signal.NotifyContext 取消发生在 detached handler 内部：claim 已经提交，
// 不能把 timers 行写成 failed（那是这条路径相对 in-tx 的永久丢通知窗口）。行必须保持
// claimed，租约过期后由新 worker 重领并收口 fired。
func TestScheduler_DetachedHandler_CancelDuringHandler_LeavesClaimedThenReclaimFires(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	clock := newTestClock(now)

	entered := make(chan struct{})
	var calls int32
	first := newDetachedWorker(t, st, clock, time.Minute, func(hctx context.Context, _ string, _ []byte) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(entered)
			<-hctx.Done()
			return hctx.Err()
		}
		return nil
	})

	scheduleDetached(t, ctx, first, "alert-d8", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	go first.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("等待事务外 handler 进入超时")
	}
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := readTimer(t, ctx, st, detachedKind, "alert-d8")
		if rec.status == "failed" {
			t.Fatalf("取消不得把已 claim 的 notify_send 写成 failed: %+v", rec)
		}
		if rec.status == "claimed" && rec.claimCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消后应保持 claimed: %+v", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(150 * time.Millisecond)
	if rec := readTimer(t, ctx, st, detachedKind, "alert-d8"); rec.status != "claimed" {
		t.Fatalf("短暂等待后 status=%q, want claimed", rec.status)
	}

	clock.Set(now.Add(2 * time.Minute))
	second := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	runCtx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	go second.Run(runCtx2)

	waitForStatus(t, ctx, st, detachedKind, "alert-d8", "fired", 5*time.Second)
	rec := readTimer(t, ctx, st, detachedKind, "alert-d8")
	if rec.claimCount != 2 {
		t.Fatalf("重领后 claim_count = %d, want 2", rec.claimCount)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler 被调用 %d 次, want 2（取消那次 + 重领成功）", got)
	}
}

// ⑨ handler 已经成功返回后，即使轮询 ctx 随即被取消，也必须 WithoutCancel settle
// fired，避免租约过期重领造成重复发送。
func TestScheduler_DetachedHandler_SuccessThenCancel_StillSettlesFired(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	entered := make(chan struct{})
	var calls int32
	sched := newDetachedWorker(t, st, clock, time.Minute, func(_ context.Context, _ string, _ []byte) error {
		atomic.AddInt32(&calls, 1)
		close(entered)
		return nil
	})

	scheduleDetached(t, ctx, sched, "alert-d9", clock.Now().Add(10*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("等待事务外 handler 进入超时")
	}
	cancel()

	waitForStatus(t, ctx, st, detachedKind, "alert-d9", "fired", 5*time.Second)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 1（成功后取消不得重领）", got)
	}
}
