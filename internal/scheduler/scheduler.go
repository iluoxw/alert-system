package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// defaultBatchSize 是每一轮轮询最多取出并处理的到期定时器数量，避免单轮事务
// 因为一次性锁住过多行而长时间占用连接。
const defaultBatchSize = 50

// Clock 抽象「当前时间」。到期判断（fire_at <= now）在 Go 侧用 Clock.Now() 取值
// 作为查询参数，而不是依赖数据库的 now()，这样测试可以注入 fake clock 精确控制
// 定时器何时到期，无需真的等待 wall-clock 时间流逝。
type Clock interface {
	Now() time.Time
}

// realClock 是 Clock 的默认实现，直接使用系统时间。
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Handler 处理一个到期定时器。refID 与 payload 就是 Schedule 时写入的原始值，
// payload 未做任何反序列化，具体格式由注册该 kind 的调用方与 handler 之间自行约定。
//
// at-least-once 语义：Run 的每一轮轮询都在单个数据库事务内完成「取到期定时器 ->
// 分发 handler -> 回写 fired/failed」。如果进程在某个 handler 执行成功之后、
// 事务提交之前崩溃，事务会随进程退出而不会提交，该定时器在数据库里仍是 pending，
// 下一轮轮询（无论是本进程重启后还是同一进程的下一次 tick）都会把它重新取出、
// 再次调用一次 handler。因此 Handler 的实现必须是幂等的：同一个
// (kind, refID, payload) 被调用两次的效果应当与调用一次相同（典型做法是在 handler
// 内部先查询 refID 当前的实际状态，只有状态仍然符合触发条件时才真正执行副作用，
// 而不是无条件重复发送通知或推进状态机）。
type Handler func(ctx context.Context, refID string, payload []byte) error

// dueTimer 是一条从数据库取出的到期定时器。
type dueTimer struct {
	id      string
	kind    string
	refID   string
	payload []byte
}

// Scheduler 是统一持久化定时器：Schedule/Cancel 由调用方在自己的事务里直接调用
// （例如去重 stage 需要「写告警 + 注册定时器」在同一事务内原子提交），Run 则
// 独立按 pollInterval 周期性开事务轮询到期定时器并分发给 RegisterHandler 注册的
// 处理函数。可以同时启动多个 Run（同一进程内的多个 goroutine，或多个进程副本）：
// 到期定时器的 FOR UPDATE SKIP LOCKED 查询保证同一条记录只会被其中一个 worker
// 处理一次。
type Scheduler struct {
	store        *store.Store
	clock        Clock
	pollInterval time.Duration
	batchSize    int

	mu       sync.RWMutex
	handlers map[string]Handler
}

// New 构造 Scheduler。st 仅供 Run 内部按 pollInterval 周期性开事务、轮询到期定时器
// 使用；Schedule/Cancel 不经过 st，而是直接在调用方传入的 tx 上执行 SQL。
func New(st *store.Store, pollInterval time.Duration) *Scheduler {
	return &Scheduler{
		store:        st,
		clock:        realClock{},
		pollInterval: pollInterval,
		batchSize:    defaultBatchSize,
		handlers:     make(map[string]Handler),
	}
}

// RegisterHandler 注册 kind 对应的处理函数，重复注册同一个 kind 会覆盖旧的。
// 并发安全，可以在 Run 已经启动之后调用。
func (s *Scheduler) RegisterHandler(kind string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = h
}

func (s *Scheduler) handlerFor(kind string) (Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[kind]
	return h, ok
}

// Schedule 注册（或替换）一个 kind+refID 的待触发定时器，在调用方传入的 tx 内执行。
// 若已存在同 (kind, refID) 的 pending 定时器，本次调用是**替换**语义：更新其
// fire_at 与 payload，而不是新增第二条记录——依赖 migrations 里
// timers_pending_kind_ref_uidx 这个局部唯一索引（(kind, ref_id) WHERE status='pending'）。
// payload 为 nil 时落 '{}'。
func (s *Scheduler) Schedule(ctx context.Context, tx pgx.Tx, kind string, refID string, fireAt time.Time, payload []byte) error {
	if payload == nil {
		payload = []byte("{}")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, ref_id) WHERE status = 'pending'
		DO UPDATE SET fire_at = EXCLUDED.fire_at, payload = EXCLUDED.payload
	`, kind, refID, fireAt, payload)
	if err != nil {
		return fmt.Errorf("scheduler: 注册定时器失败: %w", err)
	}
	return nil
}

// Cancel 把 kind+refID 对应的 pending 定时器标记为 canceled，在调用方传入的 tx 内执行。
// 幂等：定时器不存在（从未注册过、已经触发、已经被取消）时不返回错误，仅 0 行受影响。
func (s *Scheduler) Cancel(ctx context.Context, tx pgx.Tx, kind string, refID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE timers
		SET status = 'canceled'
		WHERE kind = $1 AND ref_id = $2 AND status = 'pending'
	`, kind, refID)
	if err != nil {
		return fmt.Errorf("scheduler: 取消定时器失败: %w", err)
	}
	return nil
}

// ScheduleIfAbsent 仅当 kind+refID 当前不存在 pending 定时器时才插入一条新记录，
// 与 Schedule 的"替换"语义相反：已存在 pending 记录时 DO NOTHING，不修改其
// fire_at/payload。用于"组首告警注册定时器、后续加入者不得重置已注册的基准时间"
// 这类场景（分组 stage，R9）。created=true 表示本次真的插入了新记录（本次调用
// 是"组首"）；created=false 表示已存在待触发的同 kind+refID 定时器，未做任何修改。
func (s *Scheduler) ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind string, refID string, fireAt time.Time, payload []byte) (bool, error) {
	if payload == nil {
		payload = []byte("{}")
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, ref_id) WHERE status = 'pending' DO NOTHING
	`, kind, refID, fireAt, payload)
	if err != nil {
		return false, fmt.Errorf("scheduler: 注册定时器（若不存在）失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// HasFired 报告 kind+refID 是否存在至少一条状态为 fired 的历史定时器记录。供分组
// stage 判断某个 group_key 此前是否已经完成过至少一轮到期通知，从而决定下一次
// 注册定时器该用 group_wait（组首，从未触发过）还是 group_interval（组内后续
// 更新的节流窗口，此前已触发过至少一次）。
func (s *Scheduler) HasFired(ctx context.Context, tx pgx.Tx, kind string, refID string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM timers WHERE kind = $1 AND ref_id = $2 AND status = 'fired')
	`, kind, refID).Scan(&exists); err != nil {
		return false, fmt.Errorf("scheduler: 查询定时器历史触发状态失败: %w", err)
	}
	return exists, nil
}

// fetchDue 在 tx 内取出最多 batchSize 条到期（fire_at <= now）的 pending 定时器并加行锁
// （FOR UPDATE SKIP LOCKED），跳过已被其他并发事务锁定的行，实现多 worker 并发轮询
// 时同一条定时器只会被其中一个取到。
func (s *Scheduler) fetchDue(ctx context.Context, tx pgx.Tx, now time.Time) ([]dueTimer, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, kind, ref_id, payload
		FROM timers
		WHERE status = 'pending' AND fire_at <= $1
		ORDER BY fire_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, now, s.batchSize)
	if err != nil {
		return nil, fmt.Errorf("scheduler: 取到期定时器失败: %w", err)
	}
	defer rows.Close()

	var due []dueTimer
	for rows.Next() {
		var t dueTimer
		if err := rows.Scan(&t.id, &t.kind, &t.refID, &t.payload); err != nil {
			return nil, fmt.Errorf("scheduler: 扫描定时器失败: %w", err)
		}
		due = append(due, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduler: 遍历到期定时器失败: %w", err)
	}
	return due, nil
}

func markFired(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE timers SET status = 'fired' WHERE id = $1`, id); err != nil {
		return fmt.Errorf("scheduler: 标记定时器已触发失败: %w", err)
	}
	return nil
}

// markFailed 只把 status 置为 failed。timers 表没有像 alert_events.error 那样的
// 失败原因字段，失败原因（handler 返回的 error 或 recover 到的 panic 信息）只通过
// pollOnce 里的结构化日志记录，不落库；failed 状态的定时器不会被 fetchDue 再次取出，
// 需要人工介入排查或走独立的重试路径。
func markFailed(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE timers SET status = 'failed' WHERE id = $1`, id); err != nil {
		return fmt.Errorf("scheduler: 标记定时器失败状态失败: %w", err)
	}
	return nil
}

// invoke 分发一条到期定时器给 kind 对应的 handler 并 recover 住其 panic。
// 返回 nil 表示成功；非 nil 时同时覆盖了三种失败情形：kind 未注册 handler、
// handler 返回 error、handler panic——三者在上层都被统一当作「failed」处理，
// 都不会重试（未注册 kind 不会无限重投）。
func (s *Scheduler) invoke(ctx context.Context, t dueTimer) (failErr error) {
	h, ok := s.handlerFor(t.kind)
	if !ok {
		return fmt.Errorf("未注册 kind=%q 的 handler", t.kind)
	}
	defer func() {
		if p := recover(); p != nil {
			failErr = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, t.refID, t.payload)
}

// pollOnce 是一轮轮询：在单个事务内取出到期定时器、逐条分发、回写结果。
func (s *Scheduler) pollOnce(ctx context.Context) error {
	return s.store.WithTx(ctx, func(tx pgx.Tx) error {
		due, err := s.fetchDue(ctx, tx, s.clock.Now())
		if err != nil {
			return err
		}
		for _, t := range due {
			if err := s.invoke(ctx, t); err != nil {
				log.Printf("scheduler: 定时器 id=%s kind=%s ref_id=%s 执行失败: %v", t.id, t.kind, t.refID, err)
				if merr := markFailed(ctx, tx, t.id); merr != nil {
					return merr
				}
				continue
			}
			if merr := markFired(ctx, tx, t.id); merr != nil {
				return merr
			}
		}
		return nil
	})
}

// Run 启动轮询 worker：按 pollInterval 周期性地把到期的 pending 定时器分发给已注册
// 的 handler，直到 ctx 被取消为止（阻塞调用，通常配合 `go sched.Run(ctx)` 使用）。
//
// at-least-once 与 handler 幂等的约定见 Handler 类型的文档；可以同时运行多个 Run
// （同进程多 goroutine，或多个进程副本），FOR UPDATE SKIP LOCKED 保证同一条定时器
// 只会被其中一个 worker 处理一次。
func (s *Scheduler) Run(ctx context.Context) {
	if err := s.pollOnce(ctx); err != nil {
		log.Printf("scheduler: 轮询失败: %v", err)
	}

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.pollOnce(ctx); err != nil {
				log.Printf("scheduler: 轮询失败: %v", err)
			}
		}
	}
}
