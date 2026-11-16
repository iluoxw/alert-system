package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestOptionsBounds(t *testing.T) {
	for _, o := range []Options{{0, 1, time.Second}, {33, 1, time.Second}, {1, 0, time.Second}, {1, 33, time.Second}, {1, 1, 0}, {1, 1, 241 * time.Second}} {
		if o.Validate() == nil {
			t.Fatalf("invalid options accepted: %+v", o)
		}
	}
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	if 240*time.Second+settleTimeout >= defaultLeaseTTL {
		t.Fatal("execution and settlement exceed lease")
	}
}

func TestWorkerStatuses_IndependentProgressAndSlowExecution(t *testing.T) {
	s := New(nil, time.Second)
	slow := s.newWorker("detached")
	idle := s.newWorker("normal")
	s.workerPoll(slow)
	s.workerStart(slow, "notify_send")
	s.workerPoll(idle)
	s.workerPollSuccess(idle)
	s.workersMu.Lock()
	slow.StartedAt = time.Now().Add(-10 * time.Second)
	slow.LastPollSuccessAt = time.Now().Add(-10 * time.Second)
	s.workersMu.Unlock()
	rows := s.WorkerStatuses()
	if len(rows) != 2 || rows[0].Degraded || rows[1].Degraded || rows[0].DurationSeconds < 10 {
		t.Fatalf("slow/idle state: %+v", rows)
	}
	s.workersMu.Lock()
	slow.StartedAt = time.Now().Add(-300 * time.Second)
	s.workersMu.Unlock()
	rows = s.WorkerStatuses()
	if !rows[0].Degraded || rows[1].Degraded {
		t.Fatalf("one heartbeat masked stuck worker: %+v", rows)
	}
	s.workerComplete(slow, false)
	s.workerStopped(slow)
	if !s.WorkerStatuses()[0].Degraded {
		t.Fatal("stopped worker hidden")
	}
}

func TestRun_CanceledDoesNotPoll(t *testing.T) {
	s := New(nil, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	for _, w := range s.WorkerStatuses() {
		if w.Running {
			t.Fatal("worker did not stop")
		}
	}
}

func TestWorkerCompletionReadinessBoundary(t *testing.T) {
	for _, success := range []bool{true, false} {
		s := New(nil, 10*time.Millisecond)
		w := s.newWorker("normal")
		s.workerStart(w, "ttl")
		s.workersMu.Lock()
		w.StartedAt = time.Now().Add(-2 * time.Second)
		w.LastPollSuccessAt = w.StartedAt
		s.workersMu.Unlock()
		if !s.NormalWorkersReady() {
			t.Fatal("合法长任务执行时不应降级")
		}
		s.workerComplete(w, success)
		if s.NormalWorkersReady() != success {
			t.Fatalf("完成成功=%v，ready=%v", success, s.NormalWorkersReady())
		}
		before := s.WorkerStatuses()[0].LastProgressAt
		// 下一轮查询尚未完成（或失败），不能刷新成功进度；不依赖 sleep。
		s.workerPoll(w)
		if !s.WorkerStatuses()[0].LastProgressAt.Equal(before) {
			t.Fatal("开始查询伪造成功进度")
		}
		s.workersMu.Lock()
		w.LastProgressAt = time.Now().Add(-2 * time.Second)
		w.LastPollSuccessAt = w.LastProgressAt
		s.workersMu.Unlock()
		if s.NormalWorkersReady() {
			t.Fatal("持续失败查询不能永远沿用旧完成进度")
		}
	}
}
