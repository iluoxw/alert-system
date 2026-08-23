// Package cache 是 `/services` 目录摘要的进程内 snapshot cache 与 bounded
// stale-while-revalidate 协调器。
//
// 它存在的唯一目的是让目录不必为每一行同步扇出全部数据源：目录有几百行，逐行现场查询
// 会同时变慢并放大故障。
//
// 三条硬约束贯穿本包：
//
//  1. value 类型只容纳目录摘要（Health、Data、四大 Golden compact 值、domain counts、
//     observed_at）。它在类型层面就装不下 raw evidence、日志正文、trace 或凭据——
//     Summary 的字段图里没有 any、[]byte 或自由 map。
//  2. 占位与过期语义绝不假绿：cache miss 返回 Unknown/Unconfigured 占位，且占位的
//     observed_at 取 registration/domain 里最新的真实时间，绝不用当前时间伪装成观测；
//     超过 max stale 的条目不再可见，旧的 Healthy 一律退化成 Unknown。
//  3. 一切都有界：固定 worker 数、bounded queue、per-key singleflight、max_entries 与
//     可注入的 bounded TTL jitter。队列满时保留 Unknown/Stale，不额外起 goroutine。
//
// 接口刻意保持可替换：Phase 3 可以用 PostgreSQL snapshot store 换掉 MemoryStore，
// Coordinator 不变。
package cache

import (
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
)

// Key 是 snapshot 的缓存键。
//
// 它精确固定为四元组 service_id + environment + profile_version +
// default_window_seconds。四个维度都必须参与相等性判断、LRU、singleflight 与失效：
// 同一个服务在同一环境、同一 Profile 版本下，用不同默认窗口算出来的摘要是不同的观测，
// 把它们合并成一个键会让一次查询看到另一个窗口的数值。窗口维度不能靠"profile_version
// 隐含窗口"来省略。
type Key struct {
	ServiceID      string
	Environment    string
	ProfileVersion int
	// DefaultWindowSeconds 是这份摘要使用的默认聚合窗口。
	DefaultWindowSeconds int
}

// Summary 是目录摘要，也是 cache 的 value 类型。
//
// 字段集合是刻意封闭的：这里只能放已经投影好的显示事实。任何 raw evidence、日志正文、
// trace span 或凭据都没有可以落脚的字段，因此"cache 不保存原始证据"是类型层面的保证，
// 而不是靠 code review 维持的约定。
type Summary struct {
	Health model.HealthSummary
	Data   model.DataStatus
	// GoldenSignals 最多四项，与契约 golden_signals 的 maxItems 一致。
	GoldenSignals []model.SignalValue
	DomainCounts  model.DomainCounts
	// ObservedAt 必须是真实观测/事实时间，绝不是写入缓存的时刻。
	ObservedAt time.Time
}

// ReasonSnapshotNotYetAvailable 是 cache miss 占位的稳定 Health reason code。
//
// 它与任何 source failure 明确区分：miss 表示"这个服务的摘要还没算出来"，不是"某个数据源
// 查询失败了"。因此占位的 Data 不带任何 source 状态条目——把 miss 写成 source
// unavailable 等于伪造一次失败的查询。
const ReasonSnapshotNotYetAvailable = "snapshot_not_yet_available"

// State 是一次缓存查找的结果分类。
type State string

const (
	// StateFresh 表示条目仍在 TTL（含 jitter）内：直接返回，不触发刷新。
	StateFresh State = "fresh"
	// StateStale 表示条目已过 TTL 但仍在 max stale 内：立即返回旧值并显式标 Stale。
	StateStale State = "stale"
	// StateMiss 表示没有可用条目：从未写入、已被驱逐，或已超过 max stale 被惰性回收。
	StateMiss State = "miss"
)

// Entry 是一个缓存条目。
type Entry struct {
	Summary Summary
	// StoredAt 是写入时刻，用来判断是否超过 max stale。
	StoredAt time.Time
	// FreshUntil 是含 bounded jitter 的新鲜期终点。jitter 让大量服务不会在同一秒
	// 集体过期形成惊群。
	FreshUntil time.Time
}

// Clock 让时间可注入。测试用 fake clock 精确推进，不依赖真实 sleep。
type Clock interface {
	Now() time.Time
}

// ClockFunc 把普通函数适配成 Clock。
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

// Jitter 返回附加到 TTL 上的偏移。
//
// 它必须按 key 分散：抖动的全部意义是让大量服务不在同一秒集体过期形成惊群，而一个只看
// TTL 的实现会给每个键同一个偏移，等于没有抖动。因此签名带上完整的四元组 Key。
//
// 实现不必自己控制边界：MemoryStore 会把结果夹到 [0, ttl]，并且进一步保证
// FreshUntil 不越过 max stale 的可见性期限。
type Jitter interface {
	Offset(key Key, ttl time.Duration) time.Duration
}

// JitterFunc 把普通函数适配成 Jitter。
type JitterFunc func(key Key, ttl time.Duration) time.Duration

func (f JitterFunc) Offset(key Key, ttl time.Duration) time.Duration { return f(key, ttl) }

// NoJitter 关闭抖动。测试用它得到完全确定的过期时刻。
func NoJitter() Jitter {
	return JitterFunc(func(Key, time.Duration) time.Duration { return 0 })
}

// Store 是 snapshot 存储抽象。Phase 3 可以换成 PostgreSQL 实现。
type Store interface {
	// Get 返回条目与其状态。超过 max stale 的死条目在这里被惰性回收并报 StateMiss。
	Get(key Key) (Entry, State)
	// Put 原子替换整个条目。刷新失败时调用方不调用它，从而保留最后一次成功值。
	Put(key Key, summary Summary)
	// Len 返回当前条目数，用于证明 max_entries 是硬上限。
	Len() int
}

// UnknownPlaceholder 构造 cache miss 时立即返回的 Unknown 占位摘要。
//
// observedAt 必须是 registration/domain 里最新的真实时间。传零值会被替换成 Unix epoch
// （明确表示"没有任何观测"），**绝不**替换成当前时间——那会把"还没算出来"伪装成
// "刚刚观测过"。
//
// 占位的 Data 是 partial 且不带任何 source 条目：覆盖确实不完整，但没有任何数据源失败。
func UnknownPlaceholder(observedAt, evaluatedAt time.Time) Summary {
	return Summary{
		Health: model.HealthSummary{
			Status:      model.HealthUnknown,
			ReasonCodes: []string{ReasonSnapshotNotYetAvailable},
			EvaluatedAt: model.NewTimestamp(evaluatedAt),
		},
		Data:          model.DataStatus{State: model.DataStatePartial, Sources: []model.SourceStatus{}},
		GoldenSignals: []model.SignalValue{},
		ObservedAt:    realObservedAt(observedAt),
	}
}

// UnconfiguredPlaceholder 构造未配置 Profile 的服务在 cache miss 时的占位摘要。
//
// 它与 UnknownPlaceholder 的区别只在 Data：未配置 Profile 是配置状态，不是覆盖缺口。
func UnconfiguredPlaceholder(observedAt, evaluatedAt time.Time) Summary {
	return Summary{
		Health: model.HealthSummary{
			Status:      model.HealthUnknown,
			ReasonCodes: []string{serviceobs.ReasonProfileUnconfigured},
			EvaluatedAt: model.NewTimestamp(evaluatedAt),
		},
		Data:          model.DataStatus{State: model.DataStateUnconfigured, Sources: []model.SourceStatus{}},
		GoldenSignals: []model.SignalValue{},
		ObservedAt:    realObservedAt(observedAt),
	}
}

// realObservedAt 保证占位的 observed_at 永远不是当前时间。
func realObservedAt(observedAt time.Time) time.Time {
	if observedAt.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return observedAt.UTC()
}
