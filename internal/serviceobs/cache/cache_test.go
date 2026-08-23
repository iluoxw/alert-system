package cache

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
)

// 所有测试都用 fake clock 与 channel 同步，绝不依赖真实 sleep：容量、LRU、过期回收与
// 刷新时序都必须是确定的。

var cacheBase = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)

// fakeClock 是可精确推进的注入时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// 真实事实时间锚：它比 cacheBase 早，用来证明占位不会用当前时间。
var cacheFactAnchor = cacheBase.Add(-23 * time.Minute)

func testKey(serviceID string) Key {
	return Key{ServiceID: serviceID, Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300}
}

func healthySummary(observedAt time.Time) Summary {
	value := 12.5
	return Summary{
		Health: model.HealthSummary{
			Status: model.HealthHealthy, ReasonCodes: []string{"all_required_signals_healthy"},
			EvaluatedAt: model.NewTimestamp(observedAt),
		},
		Data: model.DataStatus{State: model.DataStateComplete, Sources: []model.SourceStatus{{
			SourceKey: "alert_system_domain", State: model.AvailabilityAvailable,
			Freshness: model.FreshnessFresh, Retryable: true,
		}}},
		GoldenSignals: []model.SignalValue{{
			BindingKey: "golden_latency", Label: "延迟", Family: model.SignalFamilyGolden,
			Semantic: model.SemanticLatency, ScopeKind: model.ScopeKindService, ScopeRef: "svc",
			Value: &value, DisplayValue: "12.5ms", Unit: "ms", Aggregation: model.AggregationP95,
			WindowSeconds: 300, ObservedAt: model.NewTimestamp(observedAt),
			Freshness: model.FreshnessFresh, Availability: model.AvailabilityAvailable,
			Health: model.SignalHealthHealthy, SourceKey: "prometheus_compatible",
		}},
		DomainCounts: model.DomainCounts{ActiveFaults: 0},
		ObservedAt:   observedAt,
	}
}

func newStore(t *testing.T, clock Clock, opts MemoryOptions) *MemoryStore {
	t.Helper()
	opts.Clock = clock
	if opts.Jitter == nil {
		opts.Jitter = NoJitter()
	}
	store, err := NewMemoryStore(opts)
	if err != nil {
		t.Fatalf("构造 store 失败: %v", err)
	}
	return store
}

// ---- key 语义 ----

func TestKeyIsExactFourTuple(t *testing.T) {
	// 契约事实源规定 key 精确为 service_id + environment + profile_version +
	// default_window_seconds。四个维度都必须参与相等性判断。
	keyType := reflect.TypeOf(Key{})
	if keyType.NumField() != 4 {
		t.Fatalf("Key 必须恰好是四元组，实际 %d 个字段", keyType.NumField())
	}
	want := []string{"ServiceID", "Environment", "ProfileVersion", "DefaultWindowSeconds"}
	for i, name := range want {
		if keyType.Field(i).Name != name {
			t.Fatalf("Key 第 %d 个字段应为 %s，实际 %s", i, name, keyType.Field(i).Name)
		}
	}

	base := testKey("svc-1")
	variants := map[string]Key{
		"service 不同":     {ServiceID: "svc-2", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300},
		"environment 不同": {ServiceID: "svc-1", Environment: "staging", ProfileVersion: 1, DefaultWindowSeconds: 300},
		"profile 版本不同":   {ServiceID: "svc-1", Environment: "production", ProfileVersion: 2, DefaultWindowSeconds: 300},
		"默认窗口不同":         {ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 900},
	}
	for name, variant := range variants {
		if variant == base {
			t.Fatalf("%s 必须是不同的 key", name)
		}
	}
}

func TestDifferentDefaultWindowIsIndependentKey(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 16})

	short := Key{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300}
	long := Key{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 900}

	store.Put(short, healthySummary(cacheFactAnchor))

	// 同 service/env/profile 但不同默认窗口：绝不能互相命中，否则一次查询会看到另一个
	// 窗口的数值。
	if _, state := store.Get(long); state != StateMiss {
		t.Fatalf("不同默认窗口必须是独立 key，实际命中 %q", state)
	}
	if _, state := store.Get(short); state != StateFresh {
		t.Fatalf("原 key 应仍然 fresh，实际 %q", state)
	}
	if store.Len() != 1 {
		t.Fatalf("只写入过一个 key，实际 %d 条", store.Len())
	}

	// 两个 key 各自独立存活。
	store.Put(long, healthySummary(cacheFactAnchor.Add(time.Minute)))
	if store.Len() != 2 {
		t.Fatalf("两个窗口应各占一条，实际 %d 条", store.Len())
	}
	shortEntry, _ := store.Get(short)
	longEntry, _ := store.Get(long)
	if shortEntry.Summary.ObservedAt.Equal(longEntry.Summary.ObservedAt) {
		t.Fatal("两个独立 key 的摘要不得被合并")
	}
}

func TestDifferentDefaultWindowDoesNotShareSingleflight(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 16})

	release := make(chan struct{})
	entered := make(chan Key, 4)
	coordinator, err := New(Options{
		Store: store, Workers: 2, QueueSize: 8,
		Refresh: func(_ context.Context, key Key) (Summary, error) {
			entered <- key
			<-release
			return healthySummary(cacheFactAnchor), nil
		},
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() {
		close(release)
		shutdown(t, coordinator)
	})

	short := Key{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300}
	long := Key{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 900}
	placeholder := UnknownPlaceholder(cacheFactAnchor, clock.Now())

	if result := coordinator.Lookup(short, placeholder); !result.Enqueued {
		t.Fatal("短窗口 miss 应入队刷新")
	}
	if result := coordinator.Lookup(long, placeholder); !result.Enqueued {
		t.Fatal("长窗口 miss 必须独立入队，不得被短窗口的 singleflight 合并")
	}

	// 两次刷新必须都真正开始执行，且是两个不同的 key。
	seen := map[Key]struct{}{}
	for i := 0; i < 2; i++ {
		select {
		case key := <-entered:
			seen[key] = struct{}{}
		case <-time.After(5 * time.Second):
			t.Fatal("等待刷新开始超时")
		}
	}
	if len(seen) != 2 {
		t.Fatalf("不同默认窗口应触发两次独立刷新: %v", seen)
	}
}

// ---- 占位语义 ----

func TestPlaceholderNeverUsesCurrentTime(t *testing.T) {
	clock := newFakeClock(cacheBase)

	unknown := UnknownPlaceholder(cacheFactAnchor, clock.Now())
	if !unknown.ObservedAt.Equal(cacheFactAnchor) {
		t.Fatalf("占位 observed_at 必须取真实事实时间，实际 %v", unknown.ObservedAt)
	}
	if unknown.ObservedAt.Equal(clock.Now()) {
		t.Fatal("占位 observed_at 绝不能是当前时间")
	}
	if unknown.Health.Status != model.HealthUnknown ||
		unknown.Health.ReasonCodes[0] != ReasonSnapshotNotYetAvailable {
		t.Fatalf("miss 占位必须是带稳定 reason 的 Unknown: %+v", unknown.Health)
	}
	// miss 不是一次失败的查询：占位不得伪造任何 source 状态。
	if len(unknown.Data.Sources) != 0 {
		t.Fatalf("占位不得伪造 source failure: %+v", unknown.Data.Sources)
	}
	if unknown.Data.State != model.DataStatePartial {
		t.Fatalf("有 Profile 的 miss 占位 Data 应为 partial，实际 %q", unknown.Data.State)
	}
	if len(unknown.GoldenSignals) != 0 {
		t.Fatal("占位不得凭空产生 Golden 数值")
	}

	unconfigured := UnconfiguredPlaceholder(cacheFactAnchor, clock.Now())
	if unconfigured.Data.State != model.DataStateUnconfigured {
		t.Fatalf("未配置 Profile 的占位 Data 应为 unconfigured，实际 %q", unconfigured.Data.State)
	}
	if unconfigured.Health.ReasonCodes[0] != serviceobs.ReasonProfileUnconfigured {
		t.Fatalf("未配置占位 reason 不符: %v", unconfigured.Health.ReasonCodes)
	}

	// 零值锚点退回 epoch，绝不退回当前时间。
	zeroAnchor := UnknownPlaceholder(time.Time{}, clock.Now())
	if !zeroAnchor.ObservedAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("没有事实锚时应退回 epoch，实际 %v", zeroAnchor.ObservedAt)
	}
}

func TestLookupSanitizesCarelessPlaceholder(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 4})
	coordinator := newCoordinator(t, store, func(context.Context, Key) (Summary, error) {
		return healthySummary(cacheFactAnchor), nil
	})

	// 调用方忘了用构造函数：observed_at 是零值。它必须被兜成 epoch，而不是当前时间。
	result := coordinator.Lookup(testKey("svc-careless"), Summary{})
	if !result.Placeholder {
		t.Fatal("miss 必须返回占位")
	}
	if result.Summary.ObservedAt.Equal(clock.Now()) {
		t.Fatal("占位 observed_at 绝不能被兜成当前时间")
	}
	if !result.Summary.ObservedAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("零值锚点应兜成 epoch，实际 %v", result.Summary.ObservedAt)
	}
	if result.Summary.Health.Status != model.HealthUnknown {
		t.Fatalf("兜底占位必须是 Unknown，实际 %q", result.Summary.Health.Status)
	}
}

// ---- value 类型安全 ----

func TestSummaryTypeCannotHoldRawEvidence(t *testing.T) {
	// 这是类型层面的断言：Summary 的字段图里不允许出现 any/[]byte/自由 map，
	// 也不允许出现 evidence/log/trace/凭据一类字段名。缓存因此在结构上装不下原始证据。
	forbiddenNames := []string{
		"evidence", "log", "trace", "span", "token", "secret", "credential",
		"password", "authorization", "cookie", "query", "promql", "logql", "sql",
		"payload", "body", "headers", "prompt",
	}
	visited := map[reflect.Type]struct{}{}
	var walk func(t reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if _, seen := visited[typ]; seen {
			return
		}
		visited[typ] = struct{}{}
		switch typ.Kind() {
		case reflect.Interface:
			t.Fatalf("%s 是 interface：缓存 value 不得容纳任意值", path)
		case reflect.Map:
			t.Fatalf("%s 是 map：缓存 value 不得容纳自由键值内容", path)
		case reflect.Slice:
			if typ.Elem().Kind() == reflect.Uint8 {
				t.Fatalf("%s 是 []byte：缓存 value 不得容纳原始字节", path)
			}
			walk(typ.Elem(), path+"[]")
		case reflect.Pointer:
			walk(typ.Elem(), path+"*")
		case reflect.Struct:
			if typ == reflect.TypeOf(time.Time{}) {
				return
			}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				lowered := strings.ToLower(field.Name)
				for _, forbidden := range forbiddenNames {
					if strings.Contains(lowered, forbidden) {
						t.Fatalf("%s.%s 命中禁用字段名 %q", path, field.Name, forbidden)
					}
				}
				walk(field.Type, path+"."+field.Name)
			}
		}
	}
	walk(reflect.TypeOf(Summary{}), "Summary")
}

func TestPutDeepCopiesSummary(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 4})
	key := testKey("svc-copy")
	summary := healthySummary(cacheFactAnchor)
	store.Put(key, summary)

	// 调用方事后修改自己的 slice 不得影响缓存。
	summary.Health.ReasonCodes[0] = "tampered"
	*summary.GoldenSignals[0].Value = 999
	entry, state := store.Get(key)
	if state != StateFresh {
		t.Fatalf("应为 fresh，实际 %q", state)
	}
	if entry.Summary.Health.ReasonCodes[0] == "tampered" {
		t.Fatal("缓存条目必须是深拷贝")
	}
	if *entry.Summary.GoldenSignals[0].Value == 999 {
		t.Fatal("Golden 数值必须是深拷贝")
	}
}

func TestPutCapsGoldenSignalsAtContractLimit(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 4})
	key := testKey("svc-cap")
	summary := healthySummary(cacheFactAnchor)
	for len(summary.GoldenSignals) < 7 {
		summary.GoldenSignals = append(summary.GoldenSignals, summary.GoldenSignals[0])
	}
	store.Put(key, summary)
	entry, _ := store.Get(key)
	if len(entry.Summary.GoldenSignals) != maxGoldenSignals {
		t.Fatalf("Golden 数量必须被夹到契约上限 %d，实际 %d",
			maxGoldenSignals, len(entry.Summary.GoldenSignals))
	}
}

// ---- TTL / stale / jitter ----

func TestFreshStaleAndMaxStaleTransitions(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: 45 * time.Second, MaxStale: 5 * time.Minute, MaxEntries: 4})
	key := testKey("svc-ttl")
	store.Put(key, healthySummary(cacheFactAnchor))

	if _, state := store.Get(key); state != StateFresh {
		t.Fatalf("写入后应 fresh，实际 %q", state)
	}
	clock.Advance(44 * time.Second)
	if _, state := store.Get(key); state != StateFresh {
		t.Fatalf("TTL 内应仍 fresh，实际 %q", state)
	}
	clock.Advance(2 * time.Second)
	entry, state := store.Get(key)
	if state != StateStale {
		t.Fatalf("超过 TTL 应 stale，实际 %q", state)
	}
	// stale 仍返回旧值供展示，且 observed_at 是真实观测时间。
	if !entry.Summary.ObservedAt.Equal(cacheFactAnchor) {
		t.Fatalf("stale 值的 observed_at 应保持真实观测时间，实际 %v", entry.Summary.ObservedAt)
	}
	if entry.Summary.Health.Status != model.HealthHealthy {
		t.Fatal("max stale 内旧值仍可展示")
	}

	// 超过 max stale：旧的 Healthy 必须彻底不可见，并被惰性回收。
	clock.Advance(5 * time.Minute)
	if _, state := store.Get(key); state != StateMiss {
		t.Fatalf("超过 max stale 必须 miss，实际 %q", state)
	}
	if store.Len() != 0 {
		t.Fatalf("死条目必须在读路径被惰性回收，实际仍有 %d 条", store.Len())
	}
	if store.Stats().Reclaimed != 1 {
		t.Fatalf("回收计数不符: %+v", store.Stats())
	}
}

func TestJitterIsInjectableAndBounded(t *testing.T) {
	ttl := time.Minute

	t.Run("注入的抖动延长过期时刻", func(t *testing.T) {
		clock := newFakeClock(cacheBase)
		store := newStore(t, clock, MemoryOptions{
			TTL: ttl, MaxStale: 10 * time.Minute, MaxEntries: 4,
			Jitter: JitterFunc(func(Key, time.Duration) time.Duration { return 20 * time.Second }),
		})
		key := testKey("svc-jitter")
		store.Put(key, healthySummary(cacheFactAnchor))
		clock.Advance(70 * time.Second)
		if _, state := store.Get(key); state != StateFresh {
			t.Fatalf("TTL+抖动内应仍 fresh，实际 %q", state)
		}
		clock.Advance(11 * time.Second)
		if _, state := store.Get(key); state != StateStale {
			t.Fatalf("超过 TTL+抖动应 stale，实际 %q", state)
		}
	})

	t.Run("越界抖动被夹紧", func(t *testing.T) {
		for name, jitter := range map[string]Jitter{
			"负偏移":  JitterFunc(func(Key, time.Duration) time.Duration { return -time.Hour }),
			"超大偏移": JitterFunc(func(Key, time.Duration) time.Duration { return 10 * time.Hour }),
		} {
			clock := newFakeClock(cacheBase)
			store := newStore(t, clock, MemoryOptions{
				TTL: ttl, MaxStale: time.Hour, MaxEntries: 4, Jitter: jitter,
			})
			key := testKey("svc-clamp")
			store.Put(key, healthySummary(cacheFactAnchor))
			// 夹紧后新鲜期落在 [ttl, 2*ttl]：负偏移不会立刻过期，超大偏移也不会永不过期。
			clock.Advance(ttl - time.Second)
			if _, state := store.Get(key); state != StateFresh {
				t.Fatalf("%s: TTL 内必须 fresh，实际 %q", name, state)
			}
			clock.Advance(ttl + 2*time.Second)
			if _, state := store.Get(key); state != StateStale {
				t.Fatalf("%s: 超过 2*TTL 必须 stale，实际 %q", name, state)
			}
		}
	})

	t.Run("默认抖动有界且对同一 key 确定", func(t *testing.T) {
		key := testKey("svc-default-jitter")
		offset := defaultJitter().Offset(key, ttl)
		if offset < 0 || offset > ttl {
			t.Fatalf("默认抖动必须落在 [0, TTL]，实际 %s", offset)
		}
		if offset != defaultJitter().Offset(key, ttl) {
			t.Fatal("同一个 key 的默认抖动必须确定，不能每次不同")
		}
	})
}

func TestDefaultJitterSpreadsAcrossFullKey(t *testing.T) {
	ttl := time.Minute
	jitter := defaultJitter()
	base := Key{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300}
	baseOffset := jitter.Offset(base, ttl)

	// 四个字段各自单独变化都必须改变偏移：只看 TTL 的实现会让所有键同时过期，
	// 抖动因此完全失效。
	variants := map[string]Key{
		"service 不同":     {ServiceID: "svc-2", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300},
		"environment 不同": {ServiceID: "svc-1", Environment: "staging", ProfileVersion: 1, DefaultWindowSeconds: 300},
		"profile 版本不同":   {ServiceID: "svc-1", Environment: "production", ProfileVersion: 2, DefaultWindowSeconds: 300},
		"默认窗口不同":         {ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 900},
	}
	for name, variant := range variants {
		offset := jitter.Offset(variant, ttl)
		if offset == baseOffset {
			t.Fatalf("%s 必须得到不同的抖动偏移（实际都是 %s）", name, offset)
		}
		if offset < 0 || offset > ttl {
			t.Fatalf("%s 的抖动越界: %s", name, offset)
		}
	}
}

func TestDifferentKeysGetDifferentFreshUntil(t *testing.T) {
	clock := newFakeClock(cacheBase)
	// 用默认抖动（不是 NoJitter），因为被测行为就是默认抖动的分散性。
	store, err := NewMemoryStore(MemoryOptions{
		TTL: time.Minute, MaxStale: 30 * time.Minute, MaxEntries: 16, Clock: clock,
	})
	if err != nil {
		t.Fatalf("构造 store 失败: %v", err)
	}

	keys := []Key{
		{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300},
		{ServiceID: "svc-1", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 900},
		{ServiceID: "svc-2", Environment: "production", ProfileVersion: 1, DefaultWindowSeconds: 300},
		{ServiceID: "svc-1", Environment: "staging", ProfileVersion: 1, DefaultWindowSeconds: 300},
	}
	freshUntil := map[Key]time.Time{}
	for _, key := range keys {
		store.Put(key, healthySummary(cacheFactAnchor))
		entry, state := store.Get(key)
		if state != StateFresh {
			t.Fatalf("%v 应 fresh，实际 %q", key, state)
		}
		freshUntil[key] = entry.FreshUntil
	}
	distinct := map[time.Time]struct{}{}
	for _, moment := range freshUntil {
		distinct[moment] = struct{}{}
	}
	if len(distinct) != len(keys) {
		t.Fatalf("不同 key 必须错开过期时刻，实际只有 %d 个不同值: %v", len(distinct), freshUntil)
	}
	// 同一个 key 重复写入必须稳定得到同一个偏移。
	store.Put(keys[0], healthySummary(cacheFactAnchor))
	entry, _ := store.Get(keys[0])
	if !entry.FreshUntil.Equal(freshUntil[keys[0]]) {
		t.Fatalf("同一个 key 的过期时刻必须稳定: %v vs %v", entry.FreshUntil, freshUntil[keys[0]])
	}
}

func TestFreshUntilNeverExceedsMaxStaleDeadline(t *testing.T) {
	// 越界抖动 + maxStale 等于 TTL：FreshUntil 绝不能越过 max stale 的可见性期限，
	// 否则一个"已经不该可见"的条目会被报成 fresh。
	cases := map[string]struct {
		ttl      time.Duration
		maxStale time.Duration
	}{
		"maxStale 等于 TTL":  {time.Minute, time.Minute},
		"maxStale 略大于 TTL": {time.Minute, 70 * time.Second},
		"maxStale 远大于 TTL": {time.Minute, 10 * time.Minute},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			clock := newFakeClock(cacheBase)
			store := newStore(t, clock, MemoryOptions{
				TTL: test.ttl, MaxStale: test.maxStale, MaxEntries: 4,
				Jitter: JitterFunc(func(Key, time.Duration) time.Duration { return 10 * time.Hour }),
			})
			key := testKey("svc-deadline")
			store.Put(key, healthySummary(cacheFactAnchor))
			entry, state := store.Get(key)
			if state != StateFresh {
				t.Fatalf("刚写入应 fresh，实际 %q", state)
			}
			deadline := entry.StoredAt.Add(test.maxStale)
			if entry.FreshUntil.After(deadline) {
				t.Fatalf("FreshUntil %v 越过 max stale 期限 %v", entry.FreshUntil, deadline)
			}
			// 走到 max stale 之后必须彻底不可见，绝不会仍报 fresh。
			clock.Advance(test.maxStale + time.Second)
			if _, state := store.Get(key); state != StateMiss {
				t.Fatalf("超过 max stale 必须 miss，实际 %q", state)
			}
		})
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 4})
	key := testKey("svc-get-copy")
	store.Put(key, healthySummary(cacheFactAnchor))

	first, state := store.Get(key)
	if state != StateFresh {
		t.Fatalf("应 fresh，实际 %q", state)
	}
	// 调用方修改自己拿到的结果，绝不能影响缓存里的共享状态。
	first.Summary.Health.Status = model.HealthCritical
	first.Summary.Health.ReasonCodes[0] = "tampered"
	first.Summary.Data.Sources[0].State = model.AvailabilityForbidden
	first.Summary.GoldenSignals[0].DisplayValue = "tampered"
	*first.Summary.GoldenSignals[0].Value = 999

	second, _ := store.Get(key)
	if second.Summary.Health.Status != model.HealthHealthy {
		t.Fatal("第一次 Get 的修改不得影响第二次 Get")
	}
	if second.Summary.Health.ReasonCodes[0] == "tampered" {
		t.Fatal("reason codes 必须是深拷贝")
	}
	if second.Summary.Data.Sources[0].State != model.AvailabilityAvailable {
		t.Fatal("source 状态必须是深拷贝")
	}
	if second.Summary.GoldenSignals[0].DisplayValue == "tampered" {
		t.Fatal("Golden 显示值必须是深拷贝")
	}
	if *second.Summary.GoldenSignals[0].Value == 999 {
		t.Fatal("Golden 数值指针必须是深拷贝")
	}
}

func TestConcurrentGetIsolationIsRaceFree(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Hour, MaxStale: 2 * time.Hour, MaxEntries: 8})
	key := testKey("svc-get-race")
	store.Put(key, healthySummary(cacheFactAnchor))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				entry, state := store.Get(key)
				if state != StateFresh {
					t.Errorf("应始终 fresh，实际 %q", state)
					return
				}
				// 每个 goroutine 都写自己拿到的副本：如果 Get 交出的是内部值，
				// race detector 会立刻报告数据竞争。
				entry.Summary.Health.ReasonCodes[0] = "local"
				*entry.Summary.GoldenSignals[0].Value = float64(j)
				if entry.Summary.Health.Status != model.HealthHealthy {
					t.Error("缓存内容被并发修改污染")
					return
				}
			}
		}()
	}
	wg.Wait()
	final, _ := store.Get(key)
	if final.Summary.Health.ReasonCodes[0] != "all_required_signals_healthy" {
		t.Fatalf("缓存内容被污染: %v", final.Summary.Health.ReasonCodes)
	}
}

func TestReclaimRemovesDeadEntryAtMRUAndKeepsLiveAtLRU(t *testing.T) {
	// LRU 顺序按最近**访问**排，与写入时间无关。一个先写入、后来被读过一次的条目会待在
	// 表头；等它超过 max stale 时，只扫表尾的回收永远碰不到它，反而会驱逐位于表尾、
	// 仍然有效的条目。这个测试精确构造那个布局。
	const ttl, maxStale = 60 * time.Second, 100 * time.Second
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: ttl, MaxStale: maxStale, MaxEntries: 2})

	dead, live := testKey("dead-at-mru"), testKey("live-at-lru")

	store.Put(dead, healthySummary(cacheFactAnchor)) // t=0
	clock.Advance(10 * time.Second)
	store.Put(live, healthySummary(cacheFactAnchor)) // t=10：live 成为表头
	clock.Advance(10 * time.Second)
	// t=20：dead 仍在 max stale 内（age 20），读它一次把它挪到表头。
	if _, state := store.Get(dead); state == StateMiss {
		t.Fatal("此时 dead 还不该被回收")
	}
	// 现在链是 dead(表头) -> live(表尾)。
	clock.Advance(85 * time.Second)
	// t=105：dead age=105 > 100 已死；live age=95 <= 100 仍有效。

	// 容量为 2，写第三个键触发容量压力：必须回收表头的死键，而不是驱逐表尾的活键。
	store.Put(testKey("newcomer"), healthySummary(cacheFactAnchor))

	if _, state := store.Get(live); state != StateFresh && state != StateStale {
		t.Fatalf("仍然有效的条目不得被驱逐，实际 %q", state)
	}
	if _, state := store.Get(dead); state != StateMiss {
		t.Fatalf("超过 max stale 的死键必须被回收，实际 %q", state)
	}
	stats := store.Stats()
	if stats.Evicted != 0 {
		t.Fatalf("有死键可回收时不应驱逐活键: %+v", stats)
	}
	if stats.Reclaimed != 1 {
		t.Fatalf("应恰好回收 1 个死键: %+v", stats)
	}
}

func TestReclaimSweepsEveryDeadEntryInChain(t *testing.T) {
	// 多个死键散布在链上不同位置时，一次容量压力必须把它们全部回收，不能只清到第一个
	// 活条目为止。
	const ttl, maxStale = 30 * time.Second, 60 * time.Second
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: ttl, MaxStale: maxStale, MaxEntries: 4})

	deadA, deadB := testKey("dead-a"), testKey("dead-b")
	store.Put(deadA, healthySummary(cacheFactAnchor)) // t=0
	clock.Advance(5 * time.Second)
	store.Put(deadB, healthySummary(cacheFactAnchor)) // t=5
	clock.Advance(5 * time.Second)
	// t=10：交错读一次，让两个未来的死键分别落在链的不同位置。
	store.Get(deadA)
	clock.Advance(55 * time.Second)
	// t=65：deadA age=65 > 60、deadB age=60 <= 60。再推 1 秒让两者都死。
	clock.Advance(time.Second)
	// t=66：deadA age=66、deadB age=61，都已超过 max stale。
	// 写路径的全量清扫必须一次把两个死键都清掉，不能只清到第一个活条目为止。
	live := testKey("live")
	store.Put(live, healthySummary(cacheFactAnchor)) // t=66，仍然新鲜
	if store.Len() != 1 {
		t.Fatalf("两个死键都应被一次清扫掉，实际还剩 %d 条", store.Len())
	}

	for _, key := range []Key{deadA, deadB} {
		if _, state := store.Get(key); state != StateMiss {
			t.Fatalf("%v 必须被回收，实际 %q", key, state)
		}
	}
	if _, state := store.Get(live); state != StateFresh {
		t.Fatalf("活键必须留下，实际 %q", state)
	}
	if stats := store.Stats(); stats.Reclaimed != 2 || stats.Evicted != 0 {
		t.Fatalf("应回收两个死键且不驱逐活键: %+v", stats)
	}
}

func TestConcurrentLookupAndShutdownIsSafe(t *testing.T) {
	// 压测 enqueue 与 Shutdown 的竞态：之前 enqueue 先解锁再投递，Shutdown 可以在窗口里
	// close(queue)，导致向已关闭 channel 发送而 panic。
	for attempt := 0; attempt < 20; attempt++ {
		clock := newFakeClock(cacheBase)
		store := newStore(t, clock, MemoryOptions{TTL: time.Nanosecond, MaxStale: time.Hour, MaxEntries: 256})
		coordinator, err := New(Options{
			Store: store, Workers: 4, QueueSize: 8,
			Refresh: func(context.Context, Key) (Summary, error) {
				return healthySummary(cacheFactAnchor), nil
			},
		})
		if err != nil {
			t.Fatalf("构造协调器失败: %v", err)
		}
		coordinator.Start()

		placeholder := UnknownPlaceholder(cacheFactAnchor, clock.Now())
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					// 任何 panic 都会让测试进程崩溃，因此"没有 panic"本身就是断言。
					coordinator.Lookup(testKey("svc-stress-"+strconv.Itoa((index*50+j)%16)), placeholder)
				}
			}(i)
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- coordinator.Shutdown(shutdownCtx) }()

		wg.Wait()
		if err := <-shutdownErr; err != nil {
			cancel()
			t.Fatalf("shutdown 必须在预算内完成: %v", err)
		}
		cancel()

		stats := coordinator.Stats()
		if stats.RunningWorkers != 0 {
			t.Fatalf("shutdown 后不应有 worker 存活，实际 %d", stats.RunningWorkers)
		}
		// 每个入队的键最终都被处理并清掉 inflight 标记。
		if stats.Inflight != 0 {
			t.Fatalf("inflight 必须归零，实际 %d", stats.Inflight)
		}
		if stats.Started > stats.Enqueued {
			t.Fatalf("执行次数不得超过入队次数: %+v", stats)
		}

		// 关闭之后再查找一律不入队。
		before := coordinator.Stats().Enqueued
		for i := 0; i < 10; i++ {
			if coordinator.Lookup(testKey("svc-after"), placeholder).Enqueued {
				t.Fatal("关闭后不得再入队")
			}
		}
		if coordinator.Stats().Enqueued != before {
			t.Fatal("关闭后入队计数不得增长")
		}
	}
}

// ---- 容量与 LRU ----

func TestMaxEntriesIsHardLimitWithDeterministicLRU(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 10 * time.Minute, MaxEntries: 3})

	keys := []Key{testKey("a"), testKey("b"), testKey("c")}
	for _, key := range keys {
		store.Put(key, healthySummary(cacheFactAnchor))
	}
	if store.Len() != 3 {
		t.Fatalf("应有 3 条，实际 %d", store.Len())
	}
	// 访问 a 与 c，让 b 成为 LRU 尾部。
	if _, state := store.Get(keys[0]); state != StateFresh {
		t.Fatal("a 应 fresh")
	}
	if _, state := store.Get(keys[2]); state != StateFresh {
		t.Fatal("c 应 fresh")
	}

	store.Put(testKey("d"), healthySummary(cacheFactAnchor))
	if store.Len() != 3 {
		t.Fatalf("max_entries 是硬上限，实际 %d 条", store.Len())
	}
	// 确定性驱逐：被淘汰的一定是最久未使用的 b。
	if _, state := store.Get(keys[1]); state != StateMiss {
		t.Fatalf("LRU 尾部的 b 应被驱逐，实际 %q", state)
	}
	// 容量压力只导致 miss/Unknown，绝不保留被驱逐的旧绿值。
	for _, key := range []Key{keys[0], keys[2], testKey("d")} {
		if _, state := store.Get(key); state != StateFresh {
			t.Fatalf("%v 应仍 fresh，实际 %q", key, state)
		}
	}
	if store.Stats().Evicted != 1 {
		t.Fatalf("驱逐计数不符: %+v", store.Stats())
	}
}

func TestCapacityPressurePrefersReclaimingDeadKeys(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 2 * time.Minute, MaxEntries: 2})

	dead := testKey("dead-profile-version")
	store.Put(dead, healthySummary(cacheFactAnchor))
	// 让 dead 超过 max stale，再写入一个活键把容量填满。
	clock.Advance(3 * time.Minute)
	live := testKey("live")
	store.Put(live, healthySummary(cacheFactAnchor.Add(3*time.Minute)))

	// 写路径的惰性回收应该已经清掉死键，所以现在还有余量。
	if store.Len() != 1 {
		t.Fatalf("死键应在写路径被惰性回收，实际 %d 条", store.Len())
	}
	if store.Stats().Reclaimed != 1 {
		t.Fatalf("回收计数不符: %+v", store.Stats())
	}
	// 再写一个新键不应驱逐仍然有效的 live。
	store.Put(testKey("another"), healthySummary(cacheFactAnchor.Add(3*time.Minute)))
	if _, state := store.Get(live); state != StateFresh {
		t.Fatalf("容量压力不该先牺牲仍有效的摘要，实际 %q", state)
	}
	if store.Stats().Evicted != 0 {
		t.Fatalf("有死键可回收时不应驱逐活键: %+v", store.Stats())
	}
}

func TestNewMemoryStoreRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]MemoryOptions{
		"负 TTL":            {TTL: -time.Second},
		"max stale 小于 TTL": {TTL: time.Minute, MaxStale: time.Second},
		"max_entries 为负":   {TTL: time.Minute, MaxStale: time.Minute, MaxEntries: -1},
		"max_entries 超硬上限": {TTL: time.Minute, MaxStale: time.Minute, MaxEntries: HardMaxEntries + 1},
	}
	for name, opts := range cases {
		if _, err := NewMemoryStore(opts); err == nil {
			t.Fatalf("%s 必须构造失败", name)
		}
	}
}

// ---- SWR 协调 ----

func newCoordinator(t *testing.T, store Store, refresh RefreshFunc) *Coordinator {
	t.Helper()
	coordinator, err := New(Options{Store: store, Refresh: refresh, Workers: 2, QueueSize: 8})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() { shutdown(t, coordinator) })
	return coordinator
}

func shutdown(t *testing.T, coordinator *Coordinator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := coordinator.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown 失败: %v", err)
	}
}

// awaitEvents 用 channel 等待 n 次刷新事件，不使用真实 sleep。
func awaitEvents(t *testing.T, events <-chan Event, n int) []Event {
	t.Helper()
	collected := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		select {
		case event := <-events:
			collected = append(collected, event)
		case <-time.After(5 * time.Second):
			t.Fatalf("等待第 %d 个刷新事件超时", i+1)
		}
	}
	return collected
}

func TestFreshHitDoesNotTriggerRefresh(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})
	key := testKey("svc-fresh")
	store.Put(key, healthySummary(cacheFactAnchor))

	coordinator := newCoordinator(t, store, func(context.Context, Key) (Summary, error) {
		t.Fatal("fresh 命中绝不能触发刷新")
		return Summary{}, nil
	})
	result := coordinator.Lookup(key, UnknownPlaceholder(cacheFactAnchor, clock.Now()))
	if result.State != StateFresh || result.Stale || result.Placeholder || result.Enqueued {
		t.Fatalf("fresh 应直接返回且不入队: %+v", result)
	}
	if stats := coordinator.Stats(); stats.Enqueued != 0 || stats.Started != 0 {
		t.Fatalf("fresh 不得产生任何刷新: %+v", stats)
	}
}

func TestStaleHitReturnsOldValueAndEnqueues(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})
	key := testKey("svc-stale")
	store.Put(key, healthySummary(cacheFactAnchor))
	clock.Advance(90 * time.Second)

	events := make(chan Event, 4)
	refreshed := healthySummary(cacheFactAnchor.Add(time.Hour))
	coordinator, err := New(Options{
		Store: store, Workers: 1, QueueSize: 4,
		Refresh:  func(context.Context, Key) (Summary, error) { return refreshed, nil },
		Observer: func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() { shutdown(t, coordinator) })

	result := coordinator.Lookup(key, UnknownPlaceholder(cacheFactAnchor, clock.Now()))
	if result.State != StateStale || !result.Stale {
		t.Fatalf("过期命中必须显式标 Stale: %+v", result)
	}
	if !result.Summary.ObservedAt.Equal(cacheFactAnchor) {
		t.Fatalf("stale 应立即返回旧值: %v", result.Summary.ObservedAt)
	}
	if !result.Enqueued {
		t.Fatal("stale 必须入队后台刷新")
	}
	if event := awaitEvents(t, events, 1)[0]; event.Err != nil {
		t.Fatalf("刷新应成功: %v", event.Err)
	}
	// 刷新成功后原子替换：下一次查找看到新值且重新 fresh。
	next, state := store.Get(key)
	if state != StateFresh {
		t.Fatalf("刷新后应重新 fresh，实际 %q", state)
	}
	if !next.Summary.ObservedAt.Equal(refreshed.ObservedAt) {
		t.Fatalf("刷新应原子替换整个条目: %v", next.Summary.ObservedAt)
	}
}

func TestMissReturnsPlaceholderAndEnqueues(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})
	events := make(chan Event, 4)
	coordinator, err := New(Options{
		Store: store, Workers: 1, QueueSize: 4,
		Refresh: func(context.Context, Key) (Summary, error) {
			return healthySummary(cacheFactAnchor), nil
		},
		Observer: func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() { shutdown(t, coordinator) })

	key := testKey("svc-miss")
	result := coordinator.Lookup(key, UnknownPlaceholder(cacheFactAnchor, clock.Now()))
	if result.State != StateMiss || !result.Placeholder {
		t.Fatalf("miss 必须立即返回占位: %+v", result)
	}
	if result.Summary.Health.Status != model.HealthUnknown {
		t.Fatalf("占位必须是 Unknown，实际 %q", result.Summary.Health.Status)
	}
	if !result.Enqueued {
		t.Fatal("miss 必须入队后台刷新")
	}
	awaitEvents(t, events, 1)
	if _, state := store.Get(key); state != StateFresh {
		t.Fatalf("后台刷新后应可用，实际 %q", state)
	}
}

func TestConcurrentLookupsOnSameKeyRefreshOnce(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})

	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	events := make(chan Event, 32)
	coordinator, err := New(Options{
		Store: store, Workers: 4, QueueSize: 16,
		Refresh: func(context.Context, Key) (Summary, error) {
			entered <- struct{}{}
			<-release
			return healthySummary(cacheFactAnchor), nil
		},
		Observer: func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()

	key := testKey("svc-singleflight")
	placeholder := UnknownPlaceholder(cacheFactAnchor, clock.Now())

	// 先让一次刷新进入执行并卡住，确保 inflight 标记已生效。
	if !coordinator.Lookup(key, placeholder).Enqueued {
		t.Fatal("首次 miss 应入队")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("等待首次刷新开始超时")
	}

	// 同键并发查找必须全部被合并，一次都不新增刷新。
	var wg sync.WaitGroup
	enqueuedCount := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enqueuedCount <- coordinator.Lookup(key, placeholder).Enqueued
		}()
	}
	wg.Wait()
	close(enqueuedCount)
	for wasEnqueued := range enqueuedCount {
		if wasEnqueued {
			t.Fatal("同键并发查找必须被 singleflight 合并")
		}
	}

	close(release)
	awaitEvents(t, events, 1)
	shutdown(t, coordinator)
	if stats := coordinator.Stats(); stats.Started != 1 {
		t.Fatalf("同键只应执行一次刷新，实际 %d 次", stats.Started)
	}
}

func TestFullQueueKeepsStaleAndSpawnsNoGoroutine(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 10 * time.Minute, MaxEntries: 128})

	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	const workers, queueSize = 2, 3
	coordinator, err := New(Options{
		Store: store, Workers: workers, QueueSize: queueSize,
		Refresh: func(context.Context, Key) (Summary, error) {
			entered <- struct{}{}
			<-release
			return healthySummary(cacheFactAnchor), nil
		},
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()

	// 先把 worker 全部占满。
	for i := 0; i < workers; i++ {
		coordinator.Lookup(testKey("busy-"+string(rune('a'+i))), UnknownPlaceholder(cacheFactAnchor, clock.Now()))
	}
	for i := 0; i < workers; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("等待 worker 占满超时")
		}
	}

	// 再填满队列，然后继续压入：多余请求必须被丢弃。
	accepted, rejected := 0, 0
	for i := 0; i < queueSize+5; i++ {
		key := testKey("overflow-" + string(rune('a'+i)))
		if coordinator.Lookup(key, UnknownPlaceholder(cacheFactAnchor, clock.Now())).Enqueued {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted != queueSize {
		t.Fatalf("队列容量是硬上限：应只接受 %d 个，实际 %d", queueSize, accepted)
	}
	if rejected != 5 {
		t.Fatalf("队列满后必须拒绝入队，实际拒绝 %d", rejected)
	}

	stats := coordinator.Stats()
	// goroutine 数固定为 worker 数：队列压力绝不变成 goroutine 爆炸。
	if stats.RunningWorkers != workers {
		t.Fatalf("worker 数必须固定为 %d，实际 %d", workers, stats.RunningWorkers)
	}
	if stats.Started > workers {
		t.Fatalf("同时执行的刷新不得超过 worker 数，实际已开始 %d", stats.Started)
	}
	if stats.Dropped != 5 {
		t.Fatalf("丢弃计数不符: %+v", stats)
	}

	close(release)
	shutdown(t, coordinator)
}

func TestRefreshFailureKeepsLastSuccessUntilMaxStale(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})
	key := testKey("svc-failing")
	store.Put(key, healthySummary(cacheFactAnchor))
	clock.Advance(90 * time.Second)

	events := make(chan Event, 8)
	refreshErr := errors.New("upstream unavailable")
	coordinator, err := New(Options{
		Store: store, Workers: 1, QueueSize: 4,
		Refresh:  func(context.Context, Key) (Summary, error) { return Summary{}, refreshErr },
		Observer: func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() { shutdown(t, coordinator) })

	placeholder := UnknownPlaceholder(cacheFactAnchor, clock.Now())
	result := coordinator.Lookup(key, placeholder)
	if !result.Stale {
		t.Fatal("过期值应先返回 Stale")
	}
	event := awaitEvents(t, events, 1)[0]
	if !errors.Is(event.Err, refreshErr) {
		t.Fatalf("刷新失败必须被上报: %v", event.Err)
	}

	// 失败不覆盖最后一次成功快照：max stale 内旧值仍可展示。
	again := coordinator.Lookup(key, placeholder)
	if again.State != StateStale || again.Summary.Health.Status != model.HealthHealthy {
		t.Fatalf("刷新失败不得覆盖最后成功值: %+v", again)
	}

	// 超过 max stale 后旧的 Healthy 必须彻底不可见，退化成 Unknown 占位。
	clock.Advance(6 * time.Minute)
	expired := coordinator.Lookup(key, placeholder)
	if expired.State != StateMiss || !expired.Placeholder {
		t.Fatalf("超过 max stale 必须退化成占位: %+v", expired)
	}
	if expired.Summary.Health.Status != model.HealthUnknown {
		t.Fatalf("超过 max stale 后不得再显示 Healthy，实际 %q", expired.Summary.Health.Status)
	}
	if expired.Summary.ObservedAt.Equal(clock.Now()) {
		t.Fatal("退化后的占位 observed_at 绝不能是当前时间")
	}
}

func TestCallerCancellationDoesNotCancelWorkerRefresh(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})

	observed := make(chan error, 4)
	events := make(chan Event, 4)
	coordinator, err := New(Options{
		Store: store, Workers: 1, QueueSize: 4,
		Refresh: func(ctx context.Context, _ Key) (Summary, error) {
			// worker 拿到的必须是 Coordinator 自己的 context，而不是请求 context。
			observed <- ctx.Err()
			return healthySummary(cacheFactAnchor), nil
		},
		Observer: func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	t.Cleanup(func() { shutdown(t, coordinator) })

	// 请求 context 先取消，再触发查找：刷新必须照常完成。
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if requestCtx.Err() == nil {
		t.Fatal("请求 context 应已取消")
	}

	key := testKey("svc-cancel")
	if !coordinator.Lookup(key, UnknownPlaceholder(cacheFactAnchor, clock.Now())).Enqueued {
		t.Fatal("miss 应入队刷新")
	}
	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("worker 的 context 不应因请求取消而失效: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待刷新开始超时")
	}
	awaitEvents(t, events, 1)
	if _, state := store.Get(key); state != StateFresh {
		t.Fatalf("请求取消不得影响已接管的刷新，实际 %q", state)
	}
}

func TestShutdownStopsAllWorkersWithinBudget(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: 5 * time.Minute, MaxEntries: 8})

	observed := make(chan context.Context, 4)
	coordinator, err := New(Options{
		Store: store, Workers: 3, QueueSize: 4,
		Refresh: func(ctx context.Context, _ Key) (Summary, error) {
			observed <- ctx
			// 等 shutdown 取消后台 context：证明 shutdown 能停掉在飞刷新。
			<-ctx.Done()
			return Summary{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	coordinator.Start()
	if stats := coordinator.Stats(); stats.RunningWorkers != 3 {
		t.Fatalf("应起 3 个 worker，实际 %d", stats.RunningWorkers)
	}

	coordinator.Lookup(testKey("svc-shutdown"), UnknownPlaceholder(cacheFactAnchor, clock.Now()))
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("等待刷新开始超时")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := coordinator.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown 必须在预算内停完全部 worker: %v", err)
	}
	if stats := coordinator.Stats(); stats.RunningWorkers != 0 {
		t.Fatalf("shutdown 后不应有 worker 存活，实际 %d", stats.RunningWorkers)
	}
	// 重复 shutdown 必须幂等。
	if err := coordinator.Shutdown(ctx); err != nil {
		t.Fatalf("重复 shutdown 应幂等: %v", err)
	}
	// 关闭后查找不再入队，也不 panic。
	if coordinator.Lookup(testKey("svc-after-shutdown"), UnknownPlaceholder(cacheFactAnchor, clock.Now())).Enqueued {
		t.Fatal("关闭后不得再入队刷新")
	}
}

func TestNewRejectsInvalidCoordinatorConfiguration(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Minute, MaxStale: time.Minute, MaxEntries: 4})
	refresh := func(context.Context, Key) (Summary, error) { return Summary{}, nil }

	cases := map[string]Options{
		"缺 store":     {Refresh: refresh},
		"缺 refresh":   {Store: store},
		"worker 为负":   {Store: store, Refresh: refresh, Workers: -1},
		"worker 超硬上限": {Store: store, Refresh: refresh, Workers: HardMaxWorkers + 1},
		"队列为负":        {Store: store, Refresh: refresh, QueueSize: -1},
		"队列超硬上限":      {Store: store, Refresh: refresh, QueueSize: HardMaxQueueSize + 1},
	}
	for name, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Fatalf("%s 必须构造失败", name)
		}
	}
}

func TestConcurrentLookupAndRefreshIsRaceFree(t *testing.T) {
	clock := newFakeClock(cacheBase)
	store := newStore(t, clock, MemoryOptions{TTL: time.Millisecond, MaxStale: time.Hour, MaxEntries: 32})
	coordinator := newCoordinator(t, store, func(context.Context, Key) (Summary, error) {
		return healthySummary(cacheFactAnchor), nil
	})

	placeholder := UnknownPlaceholder(cacheFactAnchor, clock.Now())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := testKey("svc-race-" + string(rune('a'+index%4)))
			for j := 0; j < 50; j++ {
				result := coordinator.Lookup(key, placeholder)
				// 任何时刻返回的摘要都必须自洽：available 的 Golden 必须有数值。
				for _, signal := range result.Summary.GoldenSignals {
					if signal.Availability == model.AvailabilityAvailable && signal.Value == nil {
						t.Errorf("available 的 signal 必须有数值: %+v", signal)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}
