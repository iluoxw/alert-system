package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// defaultBatchSize 是 pollOnce 每次调用最多依次处理的到期定时器条数（每条定时器
// 各自独立开一个事务，见 pollOnce 文档），避免某一轮到期定时器堆积过多时
// pollOnce 长时间不返回，导致 Run 迟迟无法检查 ctx 是否已被取消。
const defaultBatchSize = 50

// defaultLeaseTTL 是 DetachedHandler 一次 claim 的租约时长：worker 领走一条定时器后
// 有这么长时间在事务外完成执行并回写终态，超时未回写就视为该 worker 已经死亡，
// 这条定时器可以被任意 worker 重新领取。取值要明显大于一次外部发送的最坏耗时
// （notify.Sender 是 3 次尝试 + 最大 5s 退避 + 各渠道自身的 HTTP 超时），否则会把
// "发送很慢"误判成"worker 已死"而重复投递。
const defaultLeaseTTL = 5 * time.Minute

// settleTimeout 是「事务外执行完成后回写终态」这个短事务的超时。回写用的是脱离
// 调用方 cancellation 的 context（见 runDetached），必须自己带一个上界，避免进程
// 退出时卡在这里不返回。
const settleTimeout = 15 * time.Second

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
// tx 是 scheduler 已经开好、且已经把这条定时器行标记为 fired 的同一个事务——handler
// 内部若需要读写数据库（含调用 Schedule/Cancel 给同一个或其他 (kind, refID) 注册/
// 取消定时器），必须复用这个 tx，不能自己另开事务：既是为了让 handler 的副作用与
// fired 状态回写在同一原子单元内一起提交/回滚，也是因为 pollOnce 已经用
// FOR UPDATE SKIP LOCKED 锁住了这一行，handler 若另开事务访问同一张表可能产生
// 不必要的锁等待甚至死锁。
//
// at-least-once 语义：pollOnce 对每条到期定时器各自开一个独立事务，事务内先
// markFired 再调用 handler。如果进程在 handler 执行成功之后、事务提交之前崩溃，
// 事务会随进程退出而不会提交，该定时器在数据库里仍是 pending，下一轮轮询会把它
// 重新取出、再次调用一次 handler。因此 Handler 的实现必须是幂等的：同一个
// (kind, refID, payload) 被调用两次的效果应当与调用一次相同（典型做法是在 handler
// 内部先查询 refID 当前的实际状态，只有状态仍然符合触发条件时才真正执行副作用，
// 而不是无条件重复发送通知或推进状态机）。
type Handler func(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error

// DetachedHandler 是第二种执行模式：处理一个到期定时器，但**不在** scheduler 的事务
// 里执行，因此签名里没有 tx。给「handler 内部必须做外部 IO」的 kind 用——目前只有
// notify_send（真正发通知的那一步）。Handler 那种模式会在 HTTP/SMTP 发送与指数退避
// 重试的全过程中一直持有 timers 行锁和一个数据库连接，这里把这段 IO 移出事务。
//
// 执行编排（细节见 processOneDue / runDetached）：
//  1. 短事务内取出该行、置为 'claimed' 并续上租约（defaultLeaseTTL），提交——claim
//     先落库，事务与行锁随即释放。
//  2. 在**没有任何 scheduler 事务持有**的状态下同步调用本函数（不起协程，worker
//     槽位被占住，未完成的执行始终对应一个活着的执行者）。
//  3. 另开一个短事务按返回值把该行回写成 fired 或 failed。
//
// 'claimed' 不是终态，这是「崩溃不丢通知」的关键：进程在第 2 步途中崩溃时，该行留在
// claimed，租约到期后被任意 worker 重新领取、再执行一次；反过来，租约未过期时它对
// 并发 worker 的轮询查询不可见，同一条定时器不会被两个 worker 同时消费。
//
// 因此 DetachedHandler 与 Handler 一样是 at-least-once、必须幂等：notify_send 的幂等
// 由 notify.Sender 的 notification_log 幂等键负责（键含随 payload 持久化的 Round，
// 重投时算出的键不变）。这个幂等键只收敛「同一条意图被逻辑性重复调度」这类重复，
// **消除不了**「外部 IO 已经发出、结果还没确认就崩溃」的重发窗口：那种情况下重投看到
// 的记录是 pending 而不是 sent，于是消息会真的再发一次。at-least-once 是既定事实，
// 不是幂等键能兜掉的（对齐 CLAUDE.md 不变量 4 的既有裁决）。
//
// 返回 error 或 panic 的收口与 Handler 一致：记结构化日志 + 标记为 failed、不重试。
// 区别是这里没有事务可以回滚——外部 IO 已经真的发生了，「回滚」本就不存在，
// 幂等/重复的边界由 Sender 侧兜。
type DetachedHandler func(ctx context.Context, refID string, payload []byte) error

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
	leaseTTL     time.Duration

	mu       sync.RWMutex
	handlers map[string]Handler
	detached map[string]DetachedHandler
}

// New 构造 Scheduler。st 仅供 Run 内部按 pollInterval 周期性开事务、轮询到期定时器
// 使用；Schedule/Cancel 不经过 st，而是直接在调用方传入的 tx 上执行 SQL。
func New(st *store.Store, pollInterval time.Duration) *Scheduler {
	return &Scheduler{
		store:        st,
		clock:        realClock{},
		pollInterval: pollInterval,
		batchSize:    defaultBatchSize,
		leaseTTL:     defaultLeaseTTL,
		handlers:     make(map[string]Handler),
		detached:     make(map[string]DetachedHandler),
	}
}

// RegisterHandler 注册 kind 对应的处理函数，重复注册同一个 kind 会覆盖旧的。
// 并发安全，可以在 Run 已经启动之后调用。
func (s *Scheduler) RegisterHandler(kind string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = h
}

// RegisterDetachedHandler 把 kind 注册成「事务外执行」模式，语义见 DetachedHandler。
// 与 RegisterHandler 同样并发安全、重复注册覆盖旧的。
//
// 一个 kind 只应该注册一种执行模式：两种都注册属于装配错误，分发时以事务外模式为准。
func (s *Scheduler) RegisterDetachedHandler(kind string, h DetachedHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached == nil {
		s.detached = make(map[string]DetachedHandler)
	}
	s.detached[kind] = h
}

func (s *Scheduler) handlerFor(kind string) (Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[kind]
	return h, ok
}

func (s *Scheduler) detachedHandlerFor(kind string) (DetachedHandler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.detached[kind]
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

// TryLockRefShared 与 LockRefExclusive 为“handler 可能续挂下一层 timer”和
// “处置动作取消整条 timer 链”提供事务级读写屏障。handler 已持有 timer 行锁，
// 因此只能 try shared，拿不到时直接结束，不能等待写锁形成锁顺序反转；Stop 使用
// exclusive 等待所有已经开始的 handler 提交后，再取消它们刚续挂的 pending timer。
func (s *Scheduler) TryLockRefShared(ctx context.Context, tx pgx.Tx, refID string) (bool, error) {
	var locked bool
	if err := tx.QueryRow(ctx, `
		SELECT pg_try_advisory_xact_lock_shared(hashtextextended('timer-chain:' || $1, 0))
	`, refID).Scan(&locked); err != nil {
		return false, fmt.Errorf("scheduler: 获取 timer 链共享屏障失败: %w", err)
	}
	return locked, nil
}

func (s *Scheduler) LockRefExclusive(ctx context.Context, tx pgx.Tx, refID string) error {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('timer-chain:' || $1, 0))
	`, refID); err != nil {
		return fmt.Errorf("scheduler: 获取 timer 链独占屏障失败: %w", err)
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

// fetchOneDue 在 tx 内取出一条可处理的定时器并加行锁（FOR UPDATE SKIP LOCKED），跳过
// 已被其他并发事务锁定的行，因此多 worker 并发轮询时同一条定时器只会被其中一个取到。
// found=false 表示当前没有（未被锁定的）可处理定时器，本轮轮询可以结束。
//
// 可处理只有两个分支：
//   - 到期的 pending 行（fire_at <= now），正常路径；
//   - 租约已过期的 claimed 行（lease_expires_at <= now），即某个 DetachedHandler 的
//     执行者在事务外执行途中死亡（进程崩溃 / 回写终态失败），这条定时器要被重新领取。
//
// 租约未过期的 claimed 行不在结果集里，所以正在被别的 worker 执行的定时器不会被抢走。
func (s *Scheduler) fetchOneDue(ctx context.Context, tx pgx.Tx, now time.Time) (t dueTimer, found bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT id, kind, ref_id, payload
		FROM timers
		WHERE (status = 'pending' AND fire_at <= $1)
		   OR (status = 'claimed' AND lease_expires_at <= $1)
		ORDER BY fire_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`, now).Scan(&t.id, &t.kind, &t.refID, &t.payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dueTimer{}, false, nil
		}
		return dueTimer{}, false, fmt.Errorf("scheduler: 取到期定时器失败: %w", err)
	}
	return t, true, nil
}

func markFired(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE timers SET status = 'fired' WHERE id = $1`, id); err != nil {
		return fmt.Errorf("scheduler: 标记定时器已触发失败: %w", err)
	}
	return nil
}

// markFailed 只把 status 置为 failed。timers 表没有像 alert_events.error 那样的
// 失败原因字段，失败原因（handler 返回的 error 或 recover 到的 panic 信息）只通过
// pollOnce 里的结构化日志记录，不落库；failed 状态的定时器不会被 fetchOneDue 再次
// 取出，需要人工介入排查或走独立的重试路径。
//
// 顺手清空 lease_expires_at：这条路径也会收口「重新领取一条过期 claim 但 claim 本身
// 写失败」的行，那种行带着上一轮的租约值，留着会让 timers 里出现「终态却还挂着租约」
// 的脏数据。pending 行的该列本来就是 NULL，置 NULL 是无副作用的。
func markFailed(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE timers SET status = 'failed', lease_expires_at = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("scheduler: 标记定时器失败状态失败: %w", err)
	}
	return nil
}

// claimTimer 把一条定时器置为 'claimed' 并续上到 leaseExpiresAt 的租约，返回本次领取
// 之后的 claim_count。这个计数就是 fencing token：settleClaim 回写终态时要求它没变，
// 从而让「租约过期后已被别的 worker 重新领取」的情况下，上一个执行者迟到的回写落空
// 而不是覆盖新一轮的结果。
//
// 调用方必须在 fetchOneDue 拿到行锁的同一个事务里调用，并在提交后才开始事务外执行：
// claim 先持久化，才能保证并发 worker 看不到这条定时器、崩溃后又能靠租约过期恢复。
func claimTimer(ctx context.Context, tx pgx.Tx, id string, leaseExpiresAt time.Time) (int, error) {
	var claimCount int
	err := tx.QueryRow(ctx, `
		UPDATE timers
		SET status = 'claimed', lease_expires_at = $2, claim_count = claim_count + 1
		WHERE id = $1
		RETURNING claim_count
	`, id, leaseExpiresAt).Scan(&claimCount)
	if err != nil {
		return 0, fmt.Errorf("scheduler: 领取定时器失败: %w", err)
	}
	return claimCount, nil
}

// settleClaim 给一条 claimed 定时器写终态（fired 或 failed）并释放租约。
// settled=false 表示这一行已经不是「我领取的那次 claim」（租约过期后被重新领取，
// claim_count 已经推进），本次回写被有意丢弃，交给新的执行者收口。
func settleClaim(ctx context.Context, tx pgx.Tx, id, status string, claimCount int) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE timers
		SET status = $2, lease_expires_at = NULL
		WHERE id = $1 AND status = 'claimed' AND claim_count = $3
	`, id, status, claimCount)
	if err != nil {
		return false, fmt.Errorf("scheduler: 回写定时器终态失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// invoke 把一条到期定时器分发给 kind 对应的 handler（复用调用方传入的 tx）并
// recover 住其 panic。返回 nil 表示成功；非 nil 时同时覆盖了三种失败情形：kind
// 未注册 handler、handler 返回 error、handler panic——三者在上层都被统一当作
// 「failed」处理，都不会重试（未注册 kind 不会无限重投）。
func (s *Scheduler) invoke(ctx context.Context, tx pgx.Tx, t dueTimer) (failErr error) {
	h, ok := s.handlerFor(t.kind)
	if !ok {
		return fmt.Errorf("未注册 kind=%q 的 handler", t.kind)
	}
	defer func() {
		if p := recover(); p != nil {
			failErr = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, tx, t.refID, t.payload)
}

// invokeDetached 是 invoke 的事务外对应物：调用 DetachedHandler 并 recover 住其 panic，
// 让 panic 收敛成一个普通的失败结果，而不是杀死轮询 worker（与 invoke 完全一致的口径）。
func invokeDetached(ctx context.Context, h DetachedHandler, t dueTimer) (failErr error) {
	defer func() {
		if p := recover(); p != nil {
			failErr = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, t.refID, t.payload)
}

// pollOnce 是一轮轮询：依次处理最多 batchSize 条到期定时器，每条各自独立开一个
// 事务，直到某次处理发现已经没有到期定时器为止。
func (s *Scheduler) pollOnce(ctx context.Context) error {
	for i := 0; i < s.batchSize; i++ {
		more, err := s.processOneDue(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

// processOneDue 取出（若存在）一条到期定时器并处理。默认（Handler，事务内执行）模式下
// 一切都在单个事务内完成，事务内顺序：SELECT ... FOR UPDATE SKIP LOCKED 取出该行 ->
// **先** markFired -> **再**调用 handler（同一事务，把 tx 传给 handler）。这个顺序是关键：handler 内部经常需要
// 用 Schedule 给同一个 (kind, refID) 重新注册下一次触发（例如升级链推进到下一步、
// 重复通知安排下一次）；Schedule 的 UPSERT 目标是"同 (kind, refID) 的 pending
// 记录"——如果不先把当前这条改成 fired，handler 里的 Schedule 会去更新*当前这条
// 正被本事务持有行锁、尚未提交*的记录，等价于同一事务里自己等自己的锁，死锁；
// 先 markFired 之后，当前行不再是 pending，handler 的 Schedule 会插入一条全新的
// pending 记录，不会与当前行冲突。
//
// handler 返回 error 或 panic：整个事务（含 markFired）一并回滚，定时器保持
// pending，下一轮轮询会把它重新取出重试；随后另开一个独立的小事务把该定时器标记
// 为 failed，避免它被无限重试（这个小事务只有这一条 UPDATE，不含任何业务副作用，
// 可以安全提交，即使刚才那次业务事务已经回滚）。
//
// 注册成 DetachedHandler 的 kind（目前只有 notify_send）走另一条路：本事务内不调
// handler，只把该行 claim 下来（markFired 换成 claimTimer）然后**提交并释放行锁**，
// 真正的执行交给事务外的 runDetached。语义见 DetachedHandler 的文档。
//
// 返回值 more=true 表示这次确实取到了一条定时器（无论处理成功与否），调用方
// pollOnce 应当继续下一轮循环；more=false 表示当前已经没有到期定时器，本轮
// pollOnce 可以结束。
func (s *Scheduler) processOneDue(ctx context.Context) (more bool, err error) {
	var t dueTimer
	var found bool
	var detached DetachedHandler
	var claimCount int

	txErr := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		var ferr error
		t, found, ferr = s.fetchOneDue(ctx, tx, s.clock.Now())
		if ferr != nil {
			return ferr
		}
		if !found {
			return nil
		}

		if h, ok := s.detachedHandlerFor(t.kind); ok {
			detached = h
			claimCount, ferr = claimTimer(ctx, tx, t.id, s.clock.Now().Add(s.leaseTTL))
			return ferr
		}

		if err := markFired(ctx, tx, t.id); err != nil {
			return err
		}
		return s.invoke(ctx, tx, t)
	})

	if !found {
		return false, txErr
	}
	if txErr != nil {
		log.Printf("scheduler: 定时器 id=%s kind=%s ref_id=%s 执行失败: %v", t.id, t.kind, t.refID, txErr)
		if merr := s.store.WithTx(ctx, func(tx pgx.Tx) error {
			return markFailed(ctx, tx, t.id)
		}); merr != nil {
			return true, fmt.Errorf("scheduler: 标记定时器失败状态时出错: %w", merr)
		}
		return true, nil
	}

	// 到这里 claim 已经提交、行锁已经释放，才开始执行外部 IO。
	if detached != nil {
		s.runDetached(ctx, t, claimCount, detached)
	}
	return true, nil
}

// runDetached 在**没有任何 scheduler 事务持有**的状态下执行一条已 claim 的定时器，
// 然后另开一个短事务用 fencing token 回写终态。
//
// 同步执行、不起 goroutine 是刻意的：worker 槽位必须被占住，让"尚未完成的执行"始终
// 对应一个活着的执行者。fire-and-forget 协程既会让轮询无节制地继续领取新任务把下游
// 渠道打爆，也让"发送到哪一步了"完全不可观测。慢发送阻塞的只是本 worker 的这一轮
// 轮询，代价与改造前一致，区别是不再占着数据库事务和 timers 行锁。
//
// 失败收口对齐 processOneDue 的既有策略：结构化日志 + 标记 failed、不自动重试。
func (s *Scheduler) runDetached(ctx context.Context, t dueTimer, claimCount int, h DetachedHandler) {
	status := "fired"
	if handlerErr := invokeDetached(ctx, h, t); handlerErr != nil {
		status = "failed"
		log.Printf("scheduler: 定时器 id=%s kind=%s ref_id=%s 事务外执行失败: %v", t.id, t.kind, t.refID, handlerErr)
	}

	// 回写用脱离调用方 cancellation 的有界 context：外部 IO 已经真的发生了，若因为进程
	// 正在优雅退出就放弃回写，这条 claim 只能等租约过期被重新领取，把同一条消息再发一次。
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()

	var settled bool
	if err := s.store.WithTx(settleCtx, func(tx pgx.Tx) error {
		var serr error
		settled, serr = settleClaim(settleCtx, tx, t.id, status, claimCount)
		return serr
	}); err != nil {
		// 回写失败不做补偿：这条定时器仍是 claimed，租约到期后会被重新领取重跑一次，
		// 与「claim 之后进程崩溃」是同一条恢复路径（at-least-once）。
		log.Printf("scheduler: 定时器 id=%s kind=%s ref_id=%s 回写终态 %s 失败，租约到期后将被重新领取: %v",
			t.id, t.kind, t.refID, status, err)
		return
	}
	if !settled {
		log.Printf("scheduler: 定时器 id=%s kind=%s ref_id=%s 的租约已在执行期间被其他 worker 重新领取，本次 %s 结果不回写",
			t.id, t.kind, t.refID, status)
	}
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
