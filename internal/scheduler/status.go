package scheduler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Options 限制每次 Run 的执行容量。超时是合作式取消；handler 必须遵守 ctx。
// 240s 执行 + 15s settle < 固定300s租约，为调度与数据库提交留出余量。
type Options struct {
	NormalWorkers    int
	DetachedWorkers  int
	ExecutionTimeout time.Duration
}

func DefaultOptions() Options { return Options{1, 2, 120 * time.Second} }
func (o Options) Validate() error {
	if o.NormalWorkers < 1 || o.NormalWorkers > 32 || o.DetachedWorkers < 1 || o.DetachedWorkers > 32 {
		return fmt.Errorf("scheduler workers must be within [1,32]")
	}
	if o.ExecutionTimeout < time.Second || o.ExecutionTimeout > 240*time.Second {
		return fmt.Errorf("scheduler execution timeout must be within [1s,240s]")
	}
	return nil
}

// Configure 只允许在启动 Run 前调用。零值字段沿用默认值，便于既有 Build 调用方兼容。
func (s *Scheduler) Configure(o Options) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.running.Load() {
		return fmt.Errorf("scheduler Configure requires stopped Run")
	}
	d := DefaultOptions()
	if o.NormalWorkers == 0 {
		o.NormalWorkers = d.NormalWorkers
	}
	if o.DetachedWorkers == 0 {
		o.DetachedWorkers = d.DetachedWorkers
	}
	if o.ExecutionTimeout == 0 {
		o.ExecutionTimeout = d.ExecutionTimeout
	}
	if err := o.Validate(); err != nil {
		return err
	}
	s.options = o
	return nil
}

type workerKey struct{}
type WorkerStatus struct {
	ID                  int       `json:"id"`
	Pool                string    `json:"pool"`
	Running             bool      `json:"running"`
	LastPollAt          time.Time `json:"last_poll_at"`
	LastPollSuccessAt   time.Time `json:"last_poll_success_at"`
	LastCompletedAt     time.Time `json:"last_completed_at"`
	StartedAt           time.Time `json:"started_at"`
	Kind                string    `json:"kind,omitempty"`
	DurationSeconds     float64   `json:"duration_seconds"`
	LastDurationSeconds float64   `json:"last_duration_seconds"`
	Degraded            bool      `json:"degraded"`
}
type workerState = WorkerStatus

type KindStatus struct {
	Kind              string  `json:"kind"`
	Pending           int64   `json:"pending"`
	Overdue           int64   `json:"overdue"`
	Claimed           int64   `json:"claimed"`
	Failed            int64   `json:"failed"`
	Reclaimed         int64   `json:"reclaimed"`
	OldestWaitSeconds float64 `json:"oldest_wait_seconds"`
}
type PoolStatus struct {
	Pool     string `json:"pool"`
	Workers  int    `json:"workers"`
	InFlight int    `json:"in_flight"`
	Degraded int    `json:"degraded"`
}
type Status struct {
	Pools                   []PoolStatus   `json:"pools"`
	Workers                 []WorkerStatus `json:"workers"`
	Kinds                   []KindStatus   `json:"kinds"`
	ExecutionTimeoutSeconds float64        `json:"execution_timeout_seconds"`
	LeaseSeconds            float64        `json:"lease_seconds"`
}

func (s *Scheduler) detachedKinds() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.detached))
	for kind := range s.detached {
		keys = append(keys, kind)
	}
	return keys
}
func (s *Scheduler) newWorker(pool string) *workerState {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w := &workerState{ID: int(s.nextWorker.Add(1)), Pool: pool, Running: true}
	s.workers[w.ID] = w
	return w
}
func (s *Scheduler) workerPoll(w *workerState) {
	if w == nil {
		return
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w.LastPollAt = time.Now()
}
func (s *Scheduler) workerStart(w *workerState, kind string) {
	if w == nil {
		return
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w.LastPollSuccessAt = time.Now()
	w.StartedAt = time.Now()
	w.Kind = kind
}
func (s *Scheduler) workerComplete(w *workerState) {
	if w == nil {
		return
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	if w.Kind != "" {
		w.LastCompletedAt = time.Now()
		w.LastDurationSeconds = time.Since(w.StartedAt).Seconds()
		w.StartedAt = time.Time{}
		w.Kind = ""
	}
}
func (s *Scheduler) workerStopped(w *workerState) {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w.Running = false
}
func (s *Scheduler) workerPollSuccess(w *workerState) {
	if w == nil {
		return
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w.LastPollSuccessAt = time.Now()
}

// WorkerStatuses 返回独立快照，不用共享心跳掩盖其他 worker 停滞。
func (s *Scheduler) WorkerStatuses() []WorkerStatus {
	opts := s.Options()
	s.workersMu.RLock()
	defer s.workersMu.RUnlock()
	out := make([]WorkerStatus, 0, len(s.workers))
	limit := 3 * s.pollInterval
	if limit < time.Second {
		limit = time.Second
	}
	for _, w := range s.workers {
		row := *w
		if !row.StartedAt.IsZero() {
			row.DurationSeconds = time.Since(row.StartedAt).Seconds()
			row.Degraded = time.Since(row.StartedAt) > opts.ExecutionTimeout+settleTimeout
		} else {
			row.Degraded = row.LastPollSuccessAt.IsZero() || time.Since(row.LastPollSuccessAt) > limit
		}
		row.Degraded = row.Degraded || !row.Running
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// QueryStatus 只输出执行状态与队列聚合，不读取 ref_id、payload、目标地址或错误原文。
// 重领/失败为当前留存 timer 历史计数，随 retention 清理，并非永久累计指标。
func (s *Scheduler) QueryStatus(ctx context.Context) (Status, error) {
	result := Status{Workers: s.WorkerStatuses(), Kinds: []KindStatus{}, ExecutionTimeoutSeconds: s.Options().ExecutionTimeout.Seconds(), LeaseSeconds: s.leaseTTL.Seconds()}
	result.Pools = []PoolStatus{{Pool: "normal"}, {Pool: "detached"}}
	for _, w := range result.Workers {
		index := 0
		if w.Pool == "detached" {
			index = 1
		}
		p := &result.Pools[index]
		p.Workers++
		if w.Kind != "" {
			p.InFlight++
		}
		if w.Degraded {
			p.Degraded++
		}
	}
	err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE (status='pending' AND fire_at <= $1) OR (status='claimed' AND lease_expires_at <= $1)),count(*) FILTER (WHERE status='claimed'),count(*) FILTER (WHERE status='failed'),COALESCE(sum(GREATEST(claim_count-1,0)),0),COALESCE(max(EXTRACT(EPOCH FROM ($1::timestamptz - CASE WHEN status='claimed' THEN lease_expires_at ELSE fire_at END))) FILTER (WHERE (status='pending' AND fire_at <= $1) OR (status='claimed' AND lease_expires_at <= $1)),0) FROM timers GROUP BY kind ORDER BY kind`, s.clock.Now())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row KindStatus
			if err := rows.Scan(&row.Kind, &row.Pending, &row.Overdue, &row.Claimed, &row.Failed, &row.Reclaimed, &row.OldestWaitSeconds); err != nil {
				return err
			}
			result.Kinds = append(result.Kinds, row)
		}
		return rows.Err()
	})
	return result, err
}

// NormalWorkersReady 逐个检查普通池。通知池慢、暂停或失败只在管理状态暴露。
func (s *Scheduler) NormalWorkersReady() bool {
	count := 0
	for _, w := range s.WorkerStatuses() {
		if w.Pool == "normal" {
			count++
			if w.Degraded {
				return false
			}
		}
	}
	return count == s.Options().NormalWorkers
}

// Options 返回容量配置快照。
func (s *Scheduler) Options() Options {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.options
}
