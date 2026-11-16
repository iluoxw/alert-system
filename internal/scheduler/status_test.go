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
	s.workerComplete(slow)
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
