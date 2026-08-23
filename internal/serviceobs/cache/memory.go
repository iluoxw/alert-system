package cache

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"

	"alert-system/internal/serviceobs/model"
)

// 默认值与硬上限。硬上限的作用是让"配置写错"变成启动错误，而不是运行期无界增长。
const (
	// DefaultTTL 是默认新鲜期。
	DefaultTTL = 45 * time.Second
	// DefaultMaxStale 是过期条目仍可展示的最长时间。超过它旧值必须彻底不可见。
	DefaultMaxStale = 5 * time.Minute
	// DefaultMaxEntries 是默认条目上限。
	DefaultMaxEntries = 4096
	// HardMaxEntries 是 max_entries 的硬上限。
	HardMaxEntries = 65536
	// DefaultJitterRatio 是默认抖动占 TTL 的比例：大量服务不会在同一秒集体过期。
	DefaultJitterRatio = 10
)

var (
	// ErrInvalidTTL 表示 TTL 非正。
	ErrInvalidTTL = errors.New("serviceobs/cache: TTL 必须为正")
	// ErrInvalidMaxStale 表示 max stale 小于 TTL：那样条目会在还新鲜时就被回收。
	ErrInvalidMaxStale = errors.New("serviceobs/cache: max stale 必须不小于 TTL")
	// ErrInvalidMaxEntries 表示 max_entries 越界。
	ErrInvalidMaxEntries = errors.New("serviceobs/cache: max_entries 必须落在硬上限内")
)

// MemoryOptions 是进程内 store 的构造参数。
type MemoryOptions struct {
	TTL        time.Duration
	MaxStale   time.Duration
	MaxEntries int
	// Clock 为 nil 时使用真实时间。测试注入 fake clock，因此不需要任何 sleep。
	Clock Clock
	// Jitter 为 nil 时使用默认的 bounded 抖动。
	Jitter Jitter
}

// MemoryStats 是容量与回收的可观测计数，用来证明上限真的是硬上限。
type MemoryStats struct {
	Entries int
	// Evicted 是因达到 max_entries 而按 LRU 驱逐的条目数。
	Evicted int
	// Reclaimed 是超过 max stale 后被惰性回收的死条目数。
	Reclaimed int
}

// lruItem 是 LRU 链表节点。key 冗余存一份，驱逐时才能从 map 里删掉对应项。
type lruItem struct {
	key   Key
	entry Entry
}

// MemoryStore 是进程内的 LRU + TTL snapshot store。
//
// 它是完全确定的：给定同一串操作与同一个注入时钟，驱逐顺序与过期判定逐次一致。
type MemoryStore struct {
	mu      sync.Mutex
	entries map[Key]*list.Element
	// order 的表头是最近使用项，表尾是 LRU 驱逐候选。
	order *list.List

	ttl        time.Duration
	maxStale   time.Duration
	maxEntries int
	clock      Clock
	jitter     Jitter

	evicted   int
	reclaimed int
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore 构造进程内 store。非法配置直接返回错误：带着一个无界或会立刻回收
// 全部条目的缓存启动，比没有缓存更糟。
func NewMemoryStore(opts MemoryOptions) (*MemoryStore, error) {
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidTTL, ttl)
	}
	maxStale := opts.MaxStale
	if maxStale == 0 {
		maxStale = DefaultMaxStale
	}
	if maxStale < ttl {
		return nil, fmt.Errorf("%w: max stale %s < TTL %s", ErrInvalidMaxStale, maxStale, ttl)
	}
	maxEntries := opts.MaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxEntries <= 0 || maxEntries > HardMaxEntries {
		return nil, fmt.Errorf("%w: %d", ErrInvalidMaxEntries, maxEntries)
	}
	clock := opts.Clock
	if clock == nil {
		clock = ClockFunc(func() time.Time { return time.Now().UTC() })
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = defaultJitter()
	}
	return &MemoryStore{
		entries:    map[Key]*list.Element{},
		order:      list.New(),
		ttl:        ttl,
		maxStale:   maxStale,
		maxEntries: maxEntries,
		clock:      clock,
		jitter:     jitter,
	}, nil
}

// defaultJitter 用键的哈希派生一个确定的偏移。
//
// 它刻意不用随机数：同一个键每次得到同一个偏移，因此重启前后的过期时刻是可预期的，
// 同时不同键之间仍然错开，避免惊群。
func defaultJitter() Jitter {
	return JitterFunc(func(ttl time.Duration) time.Duration {
		return ttl / DefaultJitterRatio
	})
}

// Get 返回条目与状态。
//
// 超过 max stale 的条目在读路径被惰性回收并报 StateMiss：这既清掉了 Profile 版本变更或
// 服务禁用留下的死键，也保证"旧的 Healthy 在 max stale 之后彻底不可见"。
func (s *MemoryStore) Get(key Key) (Entry, State) {
	s.mu.Lock()
	defer s.mu.Unlock()

	element, ok := s.entries[key]
	if !ok {
		return Entry{}, StateMiss
	}
	item := element.Value.(*lruItem)
	now := s.clock.Now()
	if now.Sub(item.entry.StoredAt) > s.maxStale {
		s.removeLocked(element)
		s.reclaimed++
		return Entry{}, StateMiss
	}
	s.order.MoveToFront(element)
	if now.Before(item.entry.FreshUntil) {
		return item.entry, StateFresh
	}
	return item.entry, StateStale
}

// Put 原子替换整个条目。
//
// "原子"在这里是字面意思：整个 Entry 在同一把锁下一次性换掉，读侧不可能看到一半新值
// 一半旧值的摘要。刷新失败时调用方根本不会调用 Put，最后一次成功值因此自然保留。
func (s *MemoryStore) Put(key Key, summary Summary) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	entry := Entry{
		Summary:    cloneSummary(summary),
		StoredAt:   now,
		FreshUntil: now.Add(s.freshWindowLocked()),
	}
	if element, ok := s.entries[key]; ok {
		element.Value.(*lruItem).entry = entry
		s.order.MoveToFront(element)
		return
	}
	// 先惰性回收死键，再考虑驱逐活键：容量压力不该先牺牲仍然有效的摘要。
	s.reclaimDeadLocked(now)
	for s.order.Len() >= s.maxEntries {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.removeLocked(oldest)
		s.evicted++
	}
	s.entries[key] = s.order.PushFront(&lruItem{key: key, entry: entry})
}

// Len 返回当前条目数。
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// Stats 返回容量与回收计数。
func (s *MemoryStore) Stats() MemoryStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MemoryStats{Entries: s.order.Len(), Evicted: s.evicted, Reclaimed: s.reclaimed}
}

// freshWindowLocked 返回 TTL 加上夹紧后的抖动。
//
// 夹紧是必须的：一个坏的 Jitter 实现最多让条目多活一个 TTL，绝不可能变成永不过期，
// 也不会因为负偏移而立刻过期。
func (s *MemoryStore) freshWindowLocked() time.Duration {
	offset := s.jitter.Offset(s.ttl)
	if offset < 0 {
		offset = 0
	}
	if offset > s.ttl {
		offset = s.ttl
	}
	return s.ttl + offset
}

// reclaimDeadLocked 从 LRU 表尾开始清掉已超过 max stale 的死条目。
//
// 只扫尾部即可：LRU 顺序下越靠尾部越久未访问，一旦遇到仍在 max stale 内的条目就停止，
// 因此单次回收的代价与死键数量成正比，而不是与总条目数成正比。
func (s *MemoryStore) reclaimDeadLocked(now time.Time) {
	for {
		oldest := s.order.Back()
		if oldest == nil {
			return
		}
		if now.Sub(oldest.Value.(*lruItem).entry.StoredAt) <= s.maxStale {
			return
		}
		s.removeLocked(oldest)
		s.reclaimed++
	}
}

func (s *MemoryStore) removeLocked(element *list.Element) {
	item := element.Value.(*lruItem)
	delete(s.entries, item.key)
	s.order.Remove(element)
}

// maxGoldenSignals 与契约 golden_signals 的 maxItems 一致。缓存层也执行这个上界，
// 避免一个坏的刷新结果把超限数组带进 Page View。
const maxGoldenSignals = 4

// cloneSummary 深拷贝摘要。
//
// 调用方拿到的是副本，因此无法通过修改返回值改到缓存里的共享状态；反过来写入后调用方
// 继续修改自己的 slice 也不会影响缓存。
func cloneSummary(summary Summary) Summary {
	clone := summary
	clone.Health.ReasonCodes = cloneStrings(summary.Health.ReasonCodes)
	clone.Data.Sources = cloneSources(summary.Data.Sources)
	signals := summary.GoldenSignals
	if len(signals) > maxGoldenSignals {
		signals = signals[:maxGoldenSignals]
	}
	clone.GoldenSignals = make([]model.SignalValue, len(signals))
	for i, signal := range signals {
		copied := signal
		if signal.Value != nil {
			value := *signal.Value
			copied.Value = &value
		}
		if signal.BaselineDeltaPercent != nil {
			delta := *signal.BaselineDeltaPercent
			copied.BaselineDeltaPercent = &delta
		}
		if signal.Sparkline != nil {
			copied.Sparkline = make([]float64, len(signal.Sparkline))
			copy(copied.Sparkline, signal.Sparkline)
		}
		clone.GoldenSignals[i] = copied
	}
	clone.ObservedAt = summary.ObservedAt.UTC()
	return clone
}

func cloneStrings(items []string) []string {
	if items == nil {
		return []string{}
	}
	out := make([]string, len(items))
	copy(out, items)
	return out
}

func cloneSources(sources []model.SourceStatus) []model.SourceStatus {
	if sources == nil {
		return []model.SourceStatus{}
	}
	out := make([]model.SourceStatus, len(sources))
	copy(out, sources)
	return out
}
