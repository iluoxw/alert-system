package omaruntime

import (
	"context"
	"time"
)

type slotKey struct{}
type slotProgress struct {
	Running, Active bool
	LastSuccess     time.Time
}

func (w *Worker) slot(ctx context.Context) int {
	n, _ := ctx.Value(slotKey{}).(int)
	if n < 0 || n >= len(w.progress) {
		return 0
	}
	return n
}
func (w *Worker) recordSuccess(ctx context.Context) {
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	w.progress[w.slot(ctx)].LastSuccess = time.Now()
}
func (w *Worker) slotRunning(ctx context.Context, yes bool) {
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	w.progress[w.slot(ctx)].Running = yes
}
func (w *Worker) slotActive(ctx context.Context, yes bool) {
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	w.progress[w.slot(ctx)].Active = yes
}

// Ready requires real successful storage/queue progress for EVERY slot. Loop entry and failures never refresh it.
// Idle completion uses a short poll window; active work stays ready only while its execution lease renews.
func (w *Worker) Ready() bool {
	w.progressMu.RLock()
	defer w.progressMu.RUnlock()
	if len(w.progress) != w.Options.Concurrency {
		return false
	}
	idle := 3 * w.Options.PollInterval
	if idle < time.Second {
		idle = time.Second
	}
	for _, s := range w.progress {
		limit := idle
		if s.Active {
			limit = w.Options.Lease
		}
		if !s.Running || s.LastSuccess.IsZero() || time.Since(s.LastSuccess) > limit {
			return false
		}
	}
	return true
}
