//go:build integration

package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/notify"
	"alert-system/internal/store"
)

type schedulerTestChannel struct {
	kind string
	send func(context.Context) error
}

func (c schedulerTestChannel) Kind() string { return c.kind }
func (c schedulerTestChannel) Send(ctx context.Context, _ string, _ notify.Message) error {
	return c.send(ctx)
}

// 真实 Sender + PG，验证发送错误穿过结果审计，取消不会被误当作完成。
func TestScheduler_RealSender_FailureAndCancellation(t *testing.T) {
	st := setupTestStore(t)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	ctx := context.Background()
	clock := newTestClock(time.Now())
	s := New(st, 10*time.Millisecond)
	s.clock = clock
	notify.Register(schedulerTestChannel{"scheduler_test_failure", func(context.Context) error { return errors.New("test channel unavailable") }})
	req := notify.SendRequest{GroupKey: "scheduler-test", ChannelKind: "scheduler_test_failure", Target: "test", Round: 1}
	s.RegisterDetachedHandler(detachedKind, func(ctx context.Context, _ string, _ []byte) error { return sender.Send(ctx, req) })
	scheduleDetached(t, ctx, s, "failed", clock.Now())
	if _, err := s.processOneDue(ctx); err != nil {
		t.Fatal(err)
	}
	if readTimer(t, ctx, st, detachedKind, "failed").status != "failed" {
		t.Fatal("Sender错误被吞掉")
	}
	failed, err := s.QueryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.Kinds) != 1 || failed.Kinds[0].Failed != 1 {
		t.Fatalf("failed visibility: %+v", failed.Kinds)
	}
	var sends atomic.Int32
	started := make(chan struct{})
	notify.Register(schedulerTestChannel{"scheduler_test_cancel", func(ctx context.Context) error {
		if sends.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}})
	req.ChannelKind = "scheduler_test_cancel"
	req.Round = 2
	scheduleDetached(t, ctx, s, "cancel", clock.Now())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("未发送")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消未退出")
	}
	rec := readTimer(t, ctx, st, detachedKind, "cancel")
	if rec.status != "claimed" {
		t.Fatalf("取消不应终态: %+v", rec)
	}
	clock.Set(clock.Now().Add(defaultLeaseTTL + time.Second))
	if _, err := s.processOneDue(ctx); err != nil {
		t.Fatal(err)
	}
	rec = readTimer(t, ctx, st, detachedKind, "cancel")
	if rec.status != "fired" || rec.claimCount != 2 || sends.Load() != 2 {
		t.Fatalf("取消重领失败: %+v sends=%d", rec, sends.Load())
	}
	var status string
	if err := st.Pool.QueryRow(ctx, `SELECT status FROM notification_log WHERE channel='scheduler_test_cancel'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("重领未恢复Sender: %s", status)
	}
}
