package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"alert-system/internal/serviceobs/model"
)

// worker/queue 的默认值与硬上限。
const (
	// DefaultWorkers 是默认后台刷新并发度。
	DefaultWorkers = 4
	// DefaultQueueSize 是默认 bounded queue 容量。
	DefaultQueueSize = 256
	// HardMaxWorkers 是 worker 数硬上限。
	HardMaxWorkers = 32
	// HardMaxQueueSize 是队列容量硬上限。
	HardMaxQueueSize = 4096
	// DefaultRefreshTimeout 是单次后台刷新的有界预算。
	DefaultRefreshTimeout = 10 * time.Second
)

var (
	// ErrStoreRequired 表示装配缺少 store。
	ErrStoreRequired = errors.New("serviceobs/cache: 必须注入 store")
	// ErrRefreshRequired 表示装配缺少刷新函数。
	ErrRefreshRequired = errors.New("serviceobs/cache: 必须注入 refresh 函数")
	// ErrInvalidWorkers 表示 worker 数越界。
	ErrInvalidWorkers = errors.New("serviceobs/cache: worker 数必须落在硬上限内")
	// ErrInvalidQueueSize 表示队列容量越界。
	ErrInvalidQueueSize = errors.New("serviceobs/cache: 队列容量必须落在硬上限内")
	// ErrShutdownTimeout 表示 shutdown 在给定预算内没能停完全部 worker。
	ErrShutdownTimeout = errors.New("serviceobs/cache: 等待 worker 退出超时")
)

// RefreshFunc 重新计算一个键的摘要。
//
// 它拿到的 ctx 是 Coordinator 拥有的后台 context，**不是**发起查询的请求 context：
// 请求取消不能打断一次已经被 worker 接管的刷新，否则用户刷新页面就会让缓存永远填不上。
type RefreshFunc func(ctx context.Context, key Key) (Summary, error)

// Event 是一次刷新尝试的结果通知，供指标与测试观察。它只带键与错误，不带摘要内容。
type Event struct {
	Key Key
	Err error
	// Dropped 为 true 表示入队被拒：队列已满，或同一个键已经在刷新中。
	// 这两种情况都不会额外起 goroutine。
	Dropped bool
}

// Observer 接收刷新事件。它在 worker goroutine 上被同步调用，实现必须快速返回。
type Observer func(Event)

// Options 是 Coordinator 的构造参数。
type Options struct {
	Store          Store
	Refresh        RefreshFunc
	Workers        int
	QueueSize      int
	RefreshTimeout time.Duration
	Observer       Observer
}

// Stats 是协调器的可观测计数。
type Stats struct {
	Workers int
	// RunningWorkers 是已启动且尚未退出的 worker 数。它在 Start 返回后立即等于
	// Workers，在 Shutdown 完成后回到 0。
	RunningWorkers int
	QueueCapacity  int
	QueueLength    int
	Inflight       int
	// Enqueued 是成功入队的刷新请求数。
	Enqueued int
	// Dropped 是被拒绝入队的刷新请求数（队列满或同键已在飞）。
	Dropped int
	// Started 是真正开始执行的刷新次数。它永远不会超过 Enqueued。
	Started int
	// Succeeded / Failed 是刷新结果。
	Succeeded int
	Failed    int
}

// Result 是一次查找的结果。
type Result struct {
	Summary Summary
	State   State
	// Stale 为 true 表示这是过期快照，页面必须显式标 Stale 并显示 observed_at。
	Stale bool
	// Placeholder 为 true 表示这是 cache miss 的 Unknown/Unconfigured 占位，
	// 不是任何真实观测。
	Placeholder bool
	// Enqueued 为 true 表示已入队等待后台刷新。false 表示不需要刷新（fresh），
	// 或队列满/同键已在飞而被丢弃——此时页面保留 Unknown/Stale，不额外起 goroutine。
	Enqueued bool
}

// Coordinator 实现 bounded stale-while-revalidate。
type Coordinator struct {
	store    Store
	refresh  RefreshFunc
	queue    chan Key
	workers  int
	timeout  time.Duration
	observer Observer

	// baseCtx 由 Coordinator 拥有：worker 用它执行刷新，因此请求取消影响不到已接管的
	// 刷新，而 Shutdown 可以一次性停掉全部在飞刷新。
	baseCtx    context.Context
	cancelBase context.CancelFunc

	mu       sync.Mutex
	inflight map[Key]struct{}
	closed   bool

	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once

	running   atomic.Int64
	enqueued  atomic.Int64
	dropped   atomic.Int64
	started   atomic.Int64
	succeeded atomic.Int64
	failed    atomic.Int64
}

// New 构造协调器。非法配置直接返回错误，不做静默兜底：一个无界的刷新池会在故障时把
// 数据库打满。
func New(opts Options) (*Coordinator, error) {
	if opts.Store == nil {
		return nil, ErrStoreRequired
	}
	if opts.Refresh == nil {
		return nil, ErrRefreshRequired
	}
	workers := opts.Workers
	if workers == 0 {
		workers = DefaultWorkers
	}
	if workers <= 0 || workers > HardMaxWorkers {
		return nil, fmt.Errorf("%w: %d", ErrInvalidWorkers, workers)
	}
	queueSize := opts.QueueSize
	if queueSize == 0 {
		queueSize = DefaultQueueSize
	}
	if queueSize <= 0 || queueSize > HardMaxQueueSize {
		return nil, fmt.Errorf("%w: %d", ErrInvalidQueueSize, queueSize)
	}
	timeout := opts.RefreshTimeout
	if timeout <= 0 {
		timeout = DefaultRefreshTimeout
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		store:      opts.Store,
		refresh:    opts.Refresh,
		queue:      make(chan Key, queueSize),
		workers:    workers,
		timeout:    timeout,
		observer:   opts.Observer,
		baseCtx:    baseCtx,
		cancelBase: cancel,
		inflight:   map[Key]struct{}{},
	}, nil
}

// Start 起固定数量的 worker。重复调用无副作用。
//
// goroutine 数从此固定为 workers：入队被拒时绝不新起 goroutine，所以队列压力不会变成
// goroutine 爆炸。
func (c *Coordinator) Start() {
	c.startOnce.Do(func() {
		c.wg.Add(c.workers)
		// 计数在这里就置好，而不是等 worker goroutine 被调度：否则 Start 刚返回时
		// Stats 会短暂报 0，"worker 数固定"这件事就变得不可观测也不可断言。
		c.running.Store(int64(c.workers))
		for i := 0; i < c.workers; i++ {
			go c.work()
		}
	})
}

// Lookup 读取一个键的摘要，并按需入队后台刷新。
//
// 它刻意不接收请求 context：这个函数不做任何外部 IO，而后台刷新的生命周期由
// Coordinator 拥有。请求取消因此在结构上就不可能取消一次已被 worker 接管的刷新。
//
// 三条路径：
//   - fresh：直接返回，不触发刷新；
//   - stale：立即返回旧值并标 Stale，同时入 bounded queue；
//   - miss：立即返回调用方给的 Unknown/Unconfigured 占位并入队，绝不在目录请求内
//     对每一行同步扇出所有 source。
func (c *Coordinator) Lookup(key Key, placeholder Summary) Result {
	entry, state := c.store.Get(key)
	switch state {
	case StateFresh:
		return Result{Summary: entry.Summary, State: StateFresh}
	case StateStale:
		return Result{
			Summary: entry.Summary, State: StateStale, Stale: true,
			Enqueued: c.enqueue(key),
		}
	default:
		// 占位的 observed_at 由构造函数保证是真实事实时间或 epoch，绝不是当前时间。
		return Result{
			Summary: sanitizePlaceholder(placeholder), State: StateMiss, Placeholder: true,
			Enqueued: c.enqueue(key),
		}
	}
}

// enqueue 尝试把键放进 bounded queue。
//
// 返回 false 有三种原因，都不会起 goroutine：协调器已关闭、同一个键已经在刷新中
// （per-key singleflight，避免同键并发重复计算），或队列已满（此时保留 Unknown/Stale）。
//
// 整个"检查已关闭 -> 检查 inflight -> 投递"序列必须在同一把锁内完成。之前的版本先解锁
// 再投递，于是 Shutdown 可以在这个窗口里 close(queue)，让投递变成向已关闭 channel 发送
// 而直接 panic。队列是有缓冲的且这里用非阻塞 select，所以持锁投递不会阻塞；worker 也从不
// 在持锁状态下投递，因此不存在锁顺序问题。
//
// inflight 标记只在投递成功后写入，"先标记再回滚"的窗口因此也不存在。
func (c *Coordinator) enqueue(key Key) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.dropped.Add(1)
		c.emit(Event{Key: key, Dropped: true})
		return false
	}
	if _, exists := c.inflight[key]; exists {
		c.mu.Unlock()
		c.dropped.Add(1)
		c.emit(Event{Key: key, Dropped: true})
		return false
	}
	select {
	case c.queue <- key:
		c.inflight[key] = struct{}{}
		c.mu.Unlock()
		c.enqueued.Add(1)
		return true
	default:
		// 队列满：保留 Unknown/Stale，不新起 goroutine。
		c.mu.Unlock()
		c.dropped.Add(1)
		// emit 放在解锁之后：observer 是外部回调，不能在持锁时调用。
		c.emit(Event{Key: key, Dropped: true})
		return false
	}
}

// work 是固定 worker 的主循环。
func (c *Coordinator) work() {
	defer c.wg.Done()
	defer c.running.Add(-1)

	for key := range c.queue {
		c.runRefresh(key)
	}
}

// runRefresh 执行一次有界刷新。
//
// 成功时原子替换整个条目；失败时**不调用 Put**，因此最后一次成功的摘要被保留，
// 直到它超过 max stale 后由读路径惰性回收（届时旧的 Healthy 不再可见，退化为 Unknown）。
func (c *Coordinator) runRefresh(key Key) {
	// inflight 标记必须在写入之后才清除：否则刷新期间的查找会重复排一次同键刷新。
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
	}()

	c.started.Add(1)
	ctx, cancel := context.WithTimeout(c.baseCtx, c.timeout)
	defer cancel()

	summary, err := c.refresh(ctx, key)
	if err != nil {
		c.failed.Add(1)
		c.emit(Event{Key: key, Err: err})
		return
	}
	c.store.Put(key, summary)
	c.succeeded.Add(1)
	c.emit(Event{Key: key})
}

func (c *Coordinator) emit(event Event) {
	if c.observer != nil {
		c.observer(event)
	}
}

// Shutdown 有界地停止全部 worker。
//
// 它先停止接受新工作并取消后台 context（在飞刷新因此立刻收到取消），再在 ctx 的预算内
// 等待 worker 退出。等待超时返回 ErrShutdownTimeout，让 app lifecycle 能明确报出
// "没停干净"而不是无限期挂住。
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.stopOnce.Do(func() {
		// closed 与 close(queue) 必须在同一把锁内完成，enqueue 才不可能在两者之间
		// 挤进一次向已关闭 channel 的发送。
		c.mu.Lock()
		c.closed = true
		close(c.queue)
		c.mu.Unlock()
		c.cancelBase()
	})

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrShutdownTimeout, ctx.Err())
	}
}

// Stats 返回当前计数。
func (c *Coordinator) Stats() Stats {
	c.mu.Lock()
	inflight := len(c.inflight)
	c.mu.Unlock()
	return Stats{
		Workers:        c.workers,
		RunningWorkers: int(c.running.Load()),
		QueueCapacity:  cap(c.queue),
		QueueLength:    len(c.queue),
		Inflight:       inflight,
		Enqueued:       int(c.enqueued.Load()),
		Dropped:        int(c.dropped.Load()),
		Started:        int(c.started.Load()),
		Succeeded:      int(c.succeeded.Load()),
		Failed:         int(c.failed.Load()),
	}
}

// sanitizePlaceholder 兜住调用方忘记用 UnknownPlaceholder/UnconfiguredPlaceholder 构造
// 占位的情况：observed_at 绝不允许是零值，更不允许是当前时间。
func sanitizePlaceholder(placeholder Summary) Summary {
	placeholder.ObservedAt = realObservedAt(placeholder.ObservedAt)
	if placeholder.GoldenSignals == nil {
		placeholder.GoldenSignals = []model.SignalValue{}
	}
	if placeholder.Data.Sources == nil {
		placeholder.Data.Sources = []model.SourceStatus{}
	}
	if placeholder.Health.ReasonCodes == nil {
		placeholder.Health.ReasonCodes = []string{ReasonSnapshotNotYetAvailable}
	}
	if placeholder.Health.Status == "" {
		placeholder.Health.Status = model.HealthUnknown
	}
	if placeholder.Data.State == "" {
		placeholder.Data.State = model.DataStatePartial
	}
	return placeholder
}
