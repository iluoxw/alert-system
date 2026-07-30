package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/enrich"
	"alert-system/internal/hook"
	"alert-system/internal/store"
)

// fakeClock 让测试精确控制冷却窗口边界，不依赖真实 wall-clock。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeTxRunner 直接调用 fn，不开真实事务——单测里的 fakeRunStore/fakeTimeline 也不会
// 真的用到 tx，传 nil 即可。
type fakeTxRunner struct{}

func (fakeTxRunner) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}

// fakeRunStore 是 runStore 的内存实现：Insert 把记录追加进 inserted 供断言，
// LastTriggered 按 preset（如果非 nil）直接返回，模拟"已有历史记录"或"没有记录"两种场景。
type fakeRunStore struct {
	inserted []store.ActionRun
	preset   *store.ActionRun // 非 nil 时 LastTriggered 返回它；nil 时返回 ok=false
	err      error
}

func (f *fakeRunStore) Insert(ctx context.Context, tx pgx.Tx, a store.ActionRun) (*store.ActionRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inserted = append(f.inserted, a)
	return &a, nil
}

func (f *fakeRunStore) LastTriggered(ctx context.Context, tx pgx.Tx, ruleID, fingerprint string) (store.ActionRun, bool, error) {
	if f.preset == nil {
		return store.ActionRun{}, false, nil
	}
	return *f.preset, true, nil
}

// fakeTimeline 记录 Append 调用，供断言"是否升级人工"。
type fakeTimeline struct {
	entries []domain.TimelineEntry
}

func (f *fakeTimeline) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.entries = append(f.entries, entry)
	return "tl-1", nil
}

func newTestAlert(fingerprint string) domain.Alert {
	return domain.Alert{
		ID:          "alert-1",
		Fingerprint: fingerprint,
		Status:      domain.StatusFiring,
	}
}

// TestDefaultTrigger_CooldownRejectsWithinWindow 覆盖 R12.4：同 fingerprint 在冷却窗口
// 内已有触发记录时，本次触发应被拒绝，落 skipped_cooldown 记录，并追加一条升级人工的
// 时间线记录。
func TestDefaultTrigger_CooldownRejectsWithinWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := &fakeRunStore{preset: &store.ActionRun{TriggeredAt: base}}
	tl := &fakeTimeline{}
	trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)
	clk := &fakeClock{now: base.Add(30 * time.Second)} // 窗口 60s，30s < 60s，应拒绝
	trig.clock = clk

	rule := Rule{ID: "rule-1", CooldownSeconds: 60}
	alert := newTestAlert("fp-1")

	if err := trig.Trigger(context.Background(), rule, alert); err != nil {
		t.Fatalf("Trigger 不应返回 error（冷却拒绝不是失败）: %v", err)
	}

	if len(runs.inserted) != 1 || runs.inserted[0].Status != store.ActionRunStatusSkippedCooldown {
		t.Fatalf("期望落一条 skipped_cooldown 记录，实际: %+v", runs.inserted)
	}
	if len(tl.entries) != 1 {
		t.Fatalf("期望落一条时间线记录，实际 %d 条", len(tl.entries))
	}
	entry := tl.entries[0]
	if entry.Actor != escalateActor || entry.Action != domain.TimelineActionEscalate {
		t.Fatalf("时间线记录 actor/action 不符: %+v", entry)
	}
	if entry.AlertID != alert.ID {
		t.Fatalf("时间线记录 AlertID 不符: %+v", entry)
	}
}

// TestDefaultTrigger_CooldownWindowBoundary 用 fake clock 精确控制窗口边界：
// 窗口内（< cooldown）拒绝，窗口外（>= cooldown）允许。
func TestDefaultTrigger_CooldownWindowBoundary(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rule := Rule{ID: "rule-1", CooldownSeconds: 60}
	alert := newTestAlert("fp-1")

	cases := []struct {
		name       string
		offset     time.Duration
		wantStatus string
	}{
		{"恰好在窗口边界前 1ns：仍在窗口内，拒绝", 60*time.Second - time.Nanosecond, store.ActionRunStatusSkippedCooldown},
		{"恰好等于窗口时长：窗口已过，允许", 60 * time.Second, store.ActionRunStatusTriggered},
		{"超出窗口：允许", 61 * time.Second, store.ActionRunStatusTriggered},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := &fakeRunStore{preset: &store.ActionRun{TriggeredAt: base}}
			tl := &fakeTimeline{}
			trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)
			trig.clock = &fakeClock{now: base.Add(tc.offset)}

			if err := trig.Trigger(context.Background(), rule, alert); err != nil {
				t.Fatalf("Trigger 失败: %v", err)
			}
			if len(runs.inserted) != 1 || runs.inserted[0].Status != tc.wantStatus {
				t.Fatalf("期望 status=%s，实际: %+v", tc.wantStatus, runs.inserted)
			}
		})
	}
}

// TestDefaultTrigger_AllowedOutsideWindow 覆盖「窗口外允许」：无历史触发记录时应直接
// 触发（落 triggered 记录），不应追加任何时间线记录。
func TestDefaultTrigger_AllowedOutsideWindow(t *testing.T) {
	runs := &fakeRunStore{} // preset 为 nil：没有历史记录
	tl := &fakeTimeline{}
	trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)

	rule := Rule{ID: "rule-1", CooldownSeconds: 60}
	alert := newTestAlert("fp-1")

	if err := trig.Trigger(context.Background(), rule, alert); err != nil {
		t.Fatalf("Trigger 失败: %v", err)
	}
	if len(runs.inserted) != 1 || runs.inserted[0].Status != store.ActionRunStatusTriggered {
		t.Fatalf("期望落一条 triggered 记录，实际: %+v", runs.inserted)
	}
	if len(tl.entries) != 0 {
		t.Fatalf("允许触发的场景不应追加时间线记录，实际 %d 条", len(tl.entries))
	}
}

// TestDefaultTrigger_CooldownDisabled 覆盖 CooldownSeconds <= 0 时不做冷却限制，
// 即使存在"刚刚触发过"的历史记录也照样允许。
func TestDefaultTrigger_CooldownDisabled(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := &fakeRunStore{preset: &store.ActionRun{TriggeredAt: base}}
	tl := &fakeTimeline{}
	trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)
	trig.clock = &fakeClock{now: base.Add(time.Second)}

	rule := Rule{ID: "rule-1", CooldownSeconds: 0}
	alert := newTestAlert("fp-1")

	if err := trig.Trigger(context.Background(), rule, alert); err != nil {
		t.Fatalf("Trigger 失败: %v", err)
	}
	if len(runs.inserted) != 1 || runs.inserted[0].Status != store.ActionRunStatusTriggered {
		t.Fatalf("期望落一条 triggered 记录，实际: %+v", runs.inserted)
	}
}

// fakeCountingTrigger 只记录被调用次数，供 RegisterHook 的状态过滤逻辑测试使用，
// 不涉及真实的冷却判定。
type fakeCountingTrigger struct {
	calls int
}

func (f *fakeCountingTrigger) Trigger(ctx context.Context, rule Rule, alert domain.Alert) error {
	f.calls++
	return nil
}

// TestRegisterHook_ResolvedAlertSkipped 覆盖 RegisterHook 的状态过滤：已 resolved 的
// 告警不应触发自愈判定。
func TestRegisterHook_ResolvedAlertSkipped(t *testing.T) {
	registry := hook.New()
	trig := &fakeCountingTrigger{}
	RegisterHook(registry, trig, Rule{ID: "rule-1"})

	alert := newTestAlert("fp-1")
	alert.Status = domain.StatusResolved
	registry.Fire(context.Background(), "dedup", hook.After, alert)

	if trig.calls != 0 {
		t.Fatalf("resolved 告警不应触发 Trigger，实际调用 %d 次", trig.calls)
	}
}

// TestRegisterHook_NoRulesRegistersNothing 覆盖「rules 为空时不注册」。
func TestRegisterHook_NoRulesRegistersNothing(t *testing.T) {
	registry := hook.New()
	trig := &fakeCountingTrigger{}
	RegisterHook(registry, trig)

	registry.Fire(context.Background(), "dedup", hook.After, newTestAlert("fp-1"))
	if trig.calls != 0 {
		t.Fatalf("未传 rule 时不应注册任何 Func，实际调用 %d 次", trig.calls)
	}
}

// TestRegisterHook_FullChain 验证「注册 -> hook.Registry.Fire -> Trigger -> action_runs
// 落账」的完整链路真实走通，不是死代码：用真实 DefaultTrigger + fake 仓储挂到
// hook.Registry 上，Fire 一次 firing 告警后应能在 fake 仓储里看到落账记录。
func TestRegisterHook_FullChain(t *testing.T) {
	runs := &fakeRunStore{}
	tl := &fakeTimeline{}
	trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)

	registry := hook.New()
	rule := Rule{ID: "rule-1", CooldownSeconds: 60}
	RegisterHook(registry, trig, rule)

	registry.Fire(context.Background(), "dedup", hook.After, newTestAlert("fp-1"))

	if len(runs.inserted) != 1 {
		t.Fatalf("期望经由 hook.Registry.Fire 落一条 action_runs 记录，实际: %+v", runs.inserted)
	}
	got := runs.inserted[0]
	if got.RuleID != rule.ID || got.Status != store.ActionRunStatusTriggered {
		t.Fatalf("落账记录不符预期: %+v", got)
	}
}

// slowTrigger 的 Trigger 方法故意阻塞超过 hook.Registry 的内置超时（2s），用于验证
// hook.Fire 的旁路隔离——它不需要真的执行完，只需要不阻塞 Fire 的返回。
type slowTrigger struct{ called chan struct{} }

func (s *slowTrigger) Trigger(ctx context.Context, rule Rule, alert domain.Alert) error {
	close(s.called)
	time.Sleep(3 * time.Second)
	return nil
}

// TestRegisterHook_SlowTriggerDoesNotBlockFire 覆盖「超时旁路不阻塞管道」：即使
// Trigger 实现阻塞 3s，registry.Fire 也必须在 hook 包内置的 2s 超时附近返回，不等
// Trigger 跑完（R12.6）。
func TestRegisterHook_SlowTriggerDoesNotBlockFire(t *testing.T) {
	registry := hook.New()
	slow := &slowTrigger{called: make(chan struct{})}
	RegisterHook(registry, slow, Rule{ID: "rule-1"})

	start := time.Now()
	registry.Fire(context.Background(), "dedup", hook.After, newTestAlert("fp-1"))
	elapsed := time.Since(start)

	select {
	case <-slow.called:
	default:
		t.Fatal("slowTrigger.Trigger 应该被真实调用过")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("Fire 不应等待慢 Trigger 跑完，实际耗时 %s", elapsed)
	}
}

// slowLookup 的 Lookup 方法故意阻塞超过 hook.Registry 的内置超时，验证 enrich 侧同样
// 享有旁路隔离——这依赖 hook.Registry.Fire 自身的超时机制，enrich.Lookup 不需要重复实现。
type slowLookup struct{ called chan struct{} }

func (s *slowLookup) Lookup(ctx context.Context, resourceRef map[string]string) (enrich.Enrichment, error) {
	close(s.called)
	time.Sleep(3 * time.Second)
	return enrich.Enrichment{}, nil
}

func TestEnrichRegisterHook_SlowLookupDoesNotBlockFire(t *testing.T) {
	registry := hook.New()
	slow := &slowLookup{called: make(chan struct{})}
	enrich.RegisterHook(registry, slow)

	start := time.Now()
	registry.Fire(context.Background(), "dedup", hook.After, newTestAlert("fp-1"))
	elapsed := time.Since(start)

	select {
	case <-slow.called:
	default:
		t.Fatal("slowLookup.Lookup 应该被真实调用过")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("Fire 不应等待慢 Lookup 跑完，实际耗时 %s", elapsed)
	}
}

// TestEnrichRegisterHook_CallsLookupWithResourceRef 验证 enrich.RegisterHook 注册的
// Func 真的会用告警的 ResourceRef 调用 Lookup，不是死代码。
func TestEnrichRegisterHook_CallsLookupWithResourceRef(t *testing.T) {
	registry := hook.New()
	rec := &recordingLookup{}
	enrich.RegisterHook(registry, rec)

	alert := newTestAlert("fp-1")
	alert.ResourceRef = map[string]string{"service.name": "checkout"}
	registry.Fire(context.Background(), "dedup", hook.After, alert)

	if rec.calls != 1 {
		t.Fatalf("期望 Lookup 被调用 1 次，实际 %d 次", rec.calls)
	}
	if rec.lastRef["service.name"] != "checkout" {
		t.Fatalf("Lookup 收到的 resourceRef 不符: %+v", rec.lastRef)
	}
}

type recordingLookup struct {
	calls   int
	lastRef map[string]string
}

func (r *recordingLookup) Lookup(ctx context.Context, resourceRef map[string]string) (enrich.Enrichment, error) {
	r.calls++
	r.lastRef = resourceRef
	return enrich.Enrichment{}, nil
}

// errBoom 用于验证 DefaultTrigger 在仓储层报错时把错误原样透传（而不是吞掉）。
var errBoom = errors.New("boom")

func TestDefaultTrigger_InsertErrorPropagates(t *testing.T) {
	runs := &fakeRunStore{err: errBoom}
	tl := &fakeTimeline{}
	trig := NewDefaultTrigger(fakeTxRunner{}, runs, tl)

	err := trig.Trigger(context.Background(), Rule{ID: "rule-1"}, newTestAlert("fp-1"))
	if !errors.Is(err, errBoom) {
		t.Fatalf("期望错误透传，实际: %v", err)
	}
}
