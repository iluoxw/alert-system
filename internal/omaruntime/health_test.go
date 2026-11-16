package omaruntime

import (
	"context"
	"testing"
	"time"
)

func TestWorkerReadinessRequiresRealProgressAndActiveRenewal(t *testing.T) {
	w := &Worker{Options: Options{Concurrency: 1, PollInterval: 100 * time.Millisecond, Lease: 6 * time.Second}, progress: make([]slotProgress, 1)}
	ctx := context.Background()
	w.slotRunning(ctx, true)
	if w.Ready() {
		t.Fatal("loop start fabricated progress")
	}
	w.recordSuccess(ctx)
	if !w.Ready() {
		t.Fatal("successful idle poll not ready")
	}
	w.progressMu.Lock()
	w.progress[0].LastSuccess = time.Now().Add(-2 * time.Second)
	w.progressMu.Unlock()
	if w.Ready() {
		t.Fatal("failed/blocked polling kept readiness")
	}
	w.slotActive(ctx, true)
	if !w.Ready() {
		t.Fatal("valid active lease incorrectly stale")
	}
	w.progressMu.Lock()
	w.progress[0].LastSuccess = time.Now().Add(-7 * time.Second)
	w.progressMu.Unlock()
	if w.Ready() {
		t.Fatal("expired lease renewal kept readiness")
	}
	w.recordSuccess(ctx)
	if !w.Ready() {
		t.Fatal("successful execution renewal not ready")
	}
	w.slotActive(ctx, false)
	w.recordSuccess(ctx)
	if !w.Ready() {
		t.Fatal("successful long completion dropped readiness before next poll")
	}
	w.slotRunning(ctx, false)
	if w.Ready() {
		t.Fatal("stopped slot ready")
	}
}

func TestAbnormalModelTerminationOverridesObservedEndTurn(t *testing.T) {
	for _, kind := range []string{"session.error", "session.status_terminated"} {
		events := []Event{{Type: "agent.message", Text: `{"schema":"sre.analysis-result/v1"}`}, {Type: "session.status_idle", StopReason: "end_turn"}, {Type: kind}}
		_, _, e := latestRound(events)
		if e != ErrModelTerminated {
			t.Fatal("abnormal event accepted as complete", kind, e)
		}
	}
}
