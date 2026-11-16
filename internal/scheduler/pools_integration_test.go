//go:build integration

package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 用开始屏障确保通知已占住执行槽，再检查普通任务；旧单 Run 必然超时。
func TestScheduler_Run_IsolatesBlockedNotifications(t *testing.T) {
	st := setupTestStore(t)
	s := New(st, 10*time.Millisecond)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	s.RegisterDetachedHandler("notify_send", func(ctx context.Context, _ string, _ []byte) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	normal := make(chan struct{}, 1)
	s.RegisterHandler("future_normal_kind", func(context.Context, pgx.Tx, string, []byte) error { normal <- struct{}{}; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		close(release)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run 未退出")
		}
	}()
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		for i := 0; i < 3; i++ {
			if err := s.Schedule(ctx, tx, "notify_send", fmt.Sprint(i), time.Now().Add(-time.Minute), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { s.Run(ctx); close(done) }()
	// 至少第一个执行已开始，旧实现也能通过这个屏障。
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("通知未开始")
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error { return s.Schedule(ctx, tx, "future_normal_kind", "normal", time.Now(), nil) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-normal:
	case <-time.After(time.Second):
		t.Fatal("慢通知阻塞了普通 timer")
	}
	// 所有通知槽占满；第三条必须留在 PG pending，不允许超领后排内存队列。
	for i := 1; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("通知池未达到预期并发")
		}
	}
	status, err := s.QueryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var notifyKind KindStatus
	for _, kind := range status.Kinds {
		if kind.Kind == "notify_send" {
			notifyKind = kind
		}
	}
	if notifyKind.Pending != 1 || notifyKind.Overdue != 1 || notifyKind.Claimed != 2 || notifyKind.OldestWaitSeconds < 60 {
		t.Fatalf("queue visibility: %+v", notifyKind)
	}
	if len(status.Pools) != 2 || status.Pools[1].InFlight != 2 {
		t.Fatalf("pool visibility: %+v", status.Pools)
	}
	if !s.NormalWorkersReady() {
		t.Fatal("blocked notifications affected normal pool readiness")
	}
	var claimed, pending int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='claimed'),count(*) FILTER (WHERE status='pending') FROM timers WHERE kind='notify_send'`).Scan(&claimed, &pending); err != nil {
		t.Fatal(err)
	}
	if claimed != 2 || pending != 1 {
		t.Fatalf("claimed=%d pending=%d，预期2/1", claimed, pending)
	}
}

// 真实 Run 的执行 deadline 不得把取消中的发送改成终态；重启后过期 claim 可重领。
func TestScheduler_Run_DeadlineLeavesClaimRecoverable(t *testing.T) {
	st := setupTestStore(t)
	s := New(st, 10*time.Millisecond)
	if err := s.Configure(Options{1, 1, time.Second}); err != nil {
		t.Fatal(err)
	}
	clock := newTestClock(time.Now())
	s.clock = clock
	canceled := make(chan struct{})
	s.RegisterDetachedHandler(detachedKind, func(ctx context.Context, _ string, _ []byte) error { <-ctx.Done(); close(canceled); return ctx.Err() })
	scheduleDetached(t, context.Background(), s, "deadline", clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("deadline 未取消 handler")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run未退出")
	}
	rec := readTimer(t, context.Background(), st, detachedKind, "deadline")
	if rec.status != "claimed" || rec.claimCount != 1 {
		t.Fatalf("deadline丢失可恢复claim: %+v", rec)
	}
	clock.Set(clock.Now().Add(defaultLeaseTTL + time.Second))
	overdue, err := s.QueryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(overdue.Kinds) != 1 || overdue.Kinds[0].Overdue != 1 || overdue.Kinds[0].OldestWaitSeconds < 1 {
		t.Fatalf("expired claim visibility: %+v", overdue.Kinds)
	}
	s.RegisterDetachedHandler(detachedKind, func(context.Context, string, []byte) error { return nil })
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitForStatus(t, context.Background(), st, detachedKind, "deadline", "fired", 2*time.Second)
	cancel()
	<-done
	rec = readTimer(t, context.Background(), st, detachedKind, "deadline")
	if rec.claimCount != 2 {
		t.Fatalf("未重领: %+v", rec)
	}
	if len(s.WorkerStatuses()) != 2 {
		t.Fatal("Run重启保留了历史worker")
	}
	status, err := s.QueryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Kinds) != 1 || status.Kinds[0].Reclaimed != 1 {
		t.Fatalf("reclaim聚合: %+v", status.Kinds)
	}
}

func TestScheduler_CompletionProgressRequiresDatabaseSuccess(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	for _, failCommit := range []bool{false, true} {
		s := New(st, 10*time.Millisecond)
		w := s.newWorker("normal")
		s.RegisterHandler("completion", func(ctx context.Context, tx pgx.Tx, _ string, _ []byte) error {
			// 不睡眠，确定性构造合法长任务刚完成的时序。
			s.workersMu.Lock()
			w.StartedAt = time.Now().Add(-2 * time.Second)
			w.LastPollSuccessAt = w.StartedAt
			s.workersMu.Unlock()
			if failCommit {
				_, _ = tx.Exec(ctx, "SELECT 1/0")
			}
			return nil
		})
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			return s.Schedule(ctx, tx, "completion", fmt.Sprint(failCommit), time.Now(), nil)
		}); err != nil {
			t.Fatal(err)
		}
		workerCtx := context.WithValue(ctx, workerKey{}, w)
		if _, err := s.processOneDue(workerCtx); err != nil {
			t.Fatal(err)
		}
		row := s.WorkerStatuses()[0]
		if (failCommit && !row.LastProgressAt.IsZero()) || (!failCommit && row.LastProgressAt.IsZero()) {
			t.Fatalf("commit failure=%v progress=%s", failCommit, row.LastProgressAt)
		}
		if s.NormalWorkersReady() == failCommit {
			t.Fatalf("commit failure=%v ready=%v", failCommit, s.NormalWorkersReady())
		}
		if failCommit {
			// failed补偿事务成功也不把原业务事务的完成失败冒充成功进度。
			var status string
			if err := st.Pool.QueryRow(ctx, `SELECT status FROM timers WHERE kind='completion' AND ref_id=$1`, fmt.Sprint(failCommit)).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "failed" {
				t.Fatal("fixture未触发事务完成失败")
			}
		}
	}
	s := New(st, 10*time.Millisecond)
	w := s.newWorker("normal")
	s.workersMu.Lock()
	w.LastPollSuccessAt = time.Now().Add(-2 * time.Second)
	s.workersMu.Unlock()
	before := s.WorkerStatuses()[0].LastPollSuccessAt
	if _, err := st.Pool.Exec(ctx, "ALTER TABLE timers RENAME TO timers_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.processOneDue(context.WithValue(ctx, workerKey{}, w)); err == nil {
		t.Fatal("fixture未触发查询失败")
	}
	row := s.WorkerStatuses()[0]
	if !row.LastPollSuccessAt.Equal(before) || !row.LastProgressAt.IsZero() || s.NormalWorkersReady() {
		t.Fatal("失败查询伪造成功进度")
	}
}
