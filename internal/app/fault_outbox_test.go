package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// 本文件是纯单测（无 build tag，无需 Docker）：FaultOutbox 依赖的四个窄接口
// （scheduler/faults/ops/providers）全用内存 fake 实现，覆盖 QueueFaultActivated
// 的五步判定分支。fakeStormScheduler 复用 storm_test.go 里已有的实现。

// fakeFaultOutboxFaultRepo 内存模拟 FaultRepo 的四个方法子集。
type fakeFaultOutboxFaultRepo struct {
	byID             map[string]domain.Fault   // id -> fault，供 TriggerFromMappingResult 的 GetByID 使用
	recentlyResolved map[string][]domain.Fault // correlationKey -> resolved faults
	openForNode      map[string][]domain.Fault // nodeID -> open faults（简化：不做多 nodeID 聚合，测试按需单 key 命中）
	timeline         []domain.FaultTimelineEntry
	notifiedAt       map[string]time.Time

	setOwnerNotifiedErr error
	getByIDErr          error
}

func newFakeFaultOutboxFaultRepo() *fakeFaultOutboxFaultRepo {
	return &fakeFaultOutboxFaultRepo{
		byID:             map[string]domain.Fault{},
		recentlyResolved: map[string][]domain.Fault{},
		openForNode:      map[string][]domain.Fault{},
		notifiedAt:       map[string]time.Time{},
	}
}

func (f *fakeFaultOutboxFaultRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error) {
	if f.getByIDErr != nil {
		return domain.Fault{}, f.getByIDErr
	}
	fault, ok := f.byID[id]
	if !ok {
		return domain.Fault{}, store.ErrFaultNotFound
	}
	return fault, nil
}

func (f *fakeFaultOutboxFaultRepo) ListRecentlyResolvedByCorrelationKey(ctx context.Context, tx pgx.Tx, correlationKey string, since time.Time) ([]domain.Fault, error) {
	return f.recentlyResolved[correlationKey], nil
}

func (f *fakeFaultOutboxFaultRepo) ListOpenForNodeIDs(ctx context.Context, tx pgx.Tx, nodeIDs []string, limit int) ([]domain.Fault, error) {
	var result []domain.Fault
	for _, id := range nodeIDs {
		result = append(result, f.openForNode[id]...)
	}
	return result, nil
}

func (f *fakeFaultOutboxFaultRepo) SetOwnerNotified(ctx context.Context, tx pgx.Tx, id string, at time.Time) error {
	if f.setOwnerNotifiedErr != nil {
		return f.setOwnerNotifiedErr
	}
	f.notifiedAt[id] = at
	return nil
}

func (f *fakeFaultOutboxFaultRepo) AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error) {
	f.timeline = append(f.timeline, entry)
	return "timeline-entry", nil
}

// fakeFaultOutboxOpsRepo 内存模拟 OpsRepo.GetSettings。
type fakeFaultOutboxOpsRepo struct {
	settings map[string]*store.OpsSettings
}

func (f *fakeFaultOutboxOpsRepo) GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*store.OpsSettings, error) {
	s, ok := f.settings[teamID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

// fakeFaultOutboxProviders 内存模拟 impact.Projector.ProviderNodeIDs。
type fakeFaultOutboxProviders struct {
	providers map[string][]string
}

func (f *fakeFaultOutboxProviders) ProviderNodeIDs(ctx context.Context, tx pgx.Tx, consumerNodeID string) ([]string, error) {
	return f.providers[consumerNodeID], nil
}

func newTestFaultOutbox(sched *fakeStormScheduler, faults *fakeFaultOutboxFaultRepo, ops *fakeFaultOutboxOpsRepo, providers *fakeFaultOutboxProviders, now time.Time) *FaultOutbox {
	o := NewFaultOutbox(sched, faults, ops, providers, 30*time.Minute, "lark_webhook", "https://fallback.example/hook", "")
	o.now = func() time.Time { return now }
	return o
}

func testFault(id, correlationKey, nodeID, ownerTeamID string) domain.Fault {
	return domain.Fault{
		ID: id, CorrelationKey: correlationKey, ResourceNodeID: nodeID, OwnerTeamID: ownerTeamID,
		Status: domain.FaultStatusActive, Severity: domain.SeverityCritical, Kind: "test_kind",
		Title: "测试 Fault", EntityRef: "entity-1", FirstDetected: time.Now(),
	}
}

// scheduleCallsOfKind 从 fakeStormScheduler.scheduleCalls 里过滤出 notify_send 调用。
func scheduleCallsOfKind(sched *fakeStormScheduler, kind string) []scheduleCall {
	var result []scheduleCall
	for _, c := range sched.scheduleCalls {
		if c.kind == kind {
			result = append(result, c)
		}
	}
	return result
}

// TestFaultOutboxQueueFaultActivated_Cooldown 覆盖 R5.3：冷却窗口内刚 resolved
// 时跳过通知，不产生任何 Schedule 调用，也不写时间线（冷却是降噪结果，不是需要
// 人回溯的事件）。
func TestFaultOutboxQueueFaultActivated_Cooldown(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	faults.recentlyResolved["corr-1"] = []domain.Fault{{ID: "old-fault", CorrelationKey: "corr-1"}}
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}}
	outbox := newTestFaultOutbox(sched, faults, ops, nil, now)

	if err := outbox.QueueFaultActivated(context.Background(), nil, testFault("f1", "corr-1", "", "")); err != nil {
		t.Fatalf("QueueFaultActivated 失败: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("冷却窗口内不应产生 Schedule 调用，got %d", len(sched.scheduleCalls))
	}
	// 冷却跳过同样要可追溯：active Fault 没有通知任何人时，时间线必须写明原因。
	if len(faults.timeline) != 1 {
		t.Fatalf("冷却跳过应写一条时间线，got %d", len(faults.timeline))
	}
	entry := faults.timeline[0]
	if entry.Action != domain.FaultTimelineActionNotifySuppressed || entry.Detail["reason"] != "cooldown" ||
		entry.Detail["recently_resolved_fault_id"] != "old-fault" {
		t.Fatalf("冷却时间线内容不符: %+v", entry)
	}
}

// TestFaultOutboxQueueFaultActivated_UpstreamSuppressed 覆盖 R5.4：上游 provider
// 已有 active 且已通知的 Fault 时，不单独通知下游，写时间线说明抑制原因。
func TestFaultOutboxQueueFaultActivated_UpstreamSuppressed(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	upstream := domain.Fault{ID: "upstream-fault", Status: domain.FaultStatusActive, OwnerNotifiedAt: now.Add(-time.Minute)}
	faults.openForNode["provider-node"] = []domain.Fault{upstream}
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}}
	providers := &fakeFaultOutboxProviders{providers: map[string][]string{"consumer-node": {"provider-node"}}}
	outbox := newTestFaultOutbox(sched, faults, ops, providers, now)

	if err := outbox.QueueFaultActivated(context.Background(), nil, testFault("f2", "corr-2", "consumer-node", "")); err != nil {
		t.Fatalf("QueueFaultActivated 失败: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("拓扑抑制时不应产生 Schedule 调用，got %d", len(sched.scheduleCalls))
	}
	if len(faults.timeline) != 1 {
		t.Fatalf("拓扑抑制应写一条时间线，got %d", len(faults.timeline))
	}
	entry := faults.timeline[0]
	if entry.Action != domain.FaultTimelineActionNotifySuppressed {
		t.Fatalf("时间线动作 = %q, want %q", entry.Action, domain.FaultTimelineActionNotifySuppressed)
	}
	if entry.Detail["upstream_fault_id"] != "upstream-fault" {
		t.Fatalf("时间线 detail 未记录上游 Fault ID: %+v", entry.Detail)
	}
}

// TestFaultOutboxQueueFaultActivated_OwnerChannelFallback 覆盖 R5.2：owner team
// 未配置渠道时回落全局兜底渠道，正常调度一次。
func TestFaultOutboxQueueFaultActivated_OwnerChannelFallback(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}} // owner team 无配置 -> store.ErrNotFound
	outbox := newTestFaultOutbox(sched, faults, ops, nil, now)

	if err := outbox.QueueFaultActivated(context.Background(), nil, testFault("f3", "corr-3", "", "team-without-channels")); err != nil {
		t.Fatalf("QueueFaultActivated 失败: %v", err)
	}
	calls := scheduleCallsOfKind(sched, notifySendTimerKind)
	if len(calls) != 1 {
		t.Fatalf("应恰好调度一次 notify_send，got %d", len(calls))
	}
	var req notify.SendRequest
	if err := json.Unmarshal(calls[0].payload, &req); err != nil {
		t.Fatalf("反序列化 SendRequest 失败: %v", err)
	}
	if req.ChannelKind != "lark_webhook" || req.Target != "https://fallback.example/hook" {
		t.Fatalf("应回落到全局兜底渠道，got %+v", req)
	}
	if faults.notifiedAt["f3"].IsZero() {
		t.Fatal("应回写 owner_notified_at")
	}
}

// TestFaultOutboxQueueFaultActivated_NoTargetAtAll 覆盖：owner 与全局兜底均未配置
// 时不调度，但必须写时间线，不能静默失败。
func TestFaultOutboxQueueFaultActivated_NoTargetAtAll(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}}
	outbox := NewFaultOutbox(sched, faults, ops, nil, 30*time.Minute, "", "", "")
	outbox.now = func() time.Time { return now }

	if err := outbox.QueueFaultActivated(context.Background(), nil, testFault("f4", "corr-4", "", "")); err != nil {
		t.Fatalf("QueueFaultActivated 失败: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("无可用通知目标时不应调度，got %d", len(sched.scheduleCalls))
	}
	if len(faults.timeline) != 1 || faults.timeline[0].Action != domain.FaultTimelineActionNotifySkipped {
		t.Fatalf("无可用通知目标应写一条 notify_skipped 时间线，got %+v", faults.timeline)
	}
}

// TestFaultOutboxQueueFaultActivated_OwnerChannel 覆盖正常路径：owner team 配置
// 了渠道时优先使用它（不回落兜底）。
func TestFaultOutboxQueueFaultActivated_OwnerChannel(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{
		"team-1": {TeamID: "team-1", Channels: []store.OpsChannelTarget{{Kind: "lark_webhook", Target: "https://team-1.example/hook"}}},
	}}
	outbox := newTestFaultOutbox(sched, faults, ops, nil, now)

	if err := outbox.QueueFaultActivated(context.Background(), nil, testFault("f5", "corr-5", "", "team-1")); err != nil {
		t.Fatalf("QueueFaultActivated 失败: %v", err)
	}
	calls := scheduleCallsOfKind(sched, notifySendTimerKind)
	if len(calls) != 1 {
		t.Fatalf("应恰好调度一次，got %d", len(calls))
	}
	var req notify.SendRequest
	if err := json.Unmarshal(calls[0].payload, &req); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if req.Target != "https://team-1.example/hook" {
		t.Fatalf("应使用 owner team 配置的渠道，got %+v", req)
	}
}

// TestFaultOutboxTriggerFromMappingResult 覆盖 R5.10/R5.11 的判定逻辑：只有
// Transition 为 CreatedActive/EnteredActive 且 FaultID 非空时才真正调用
// QueueFaultActivated，其余情况（含 Transition=None）无副作用返回。
func TestFaultOutboxTriggerFromMappingResult(t *testing.T) {
	now := time.Now()
	fault := testFault("fault-trigger", "corr-trigger", "", "")

	cases := []struct {
		name       string
		result     domain.AlertMappingResult
		wantQueued bool
	}{
		{"CreatedActive触发", domain.AlertMappingResult{FaultID: fault.ID, Transition: domain.FaultTransitionCreatedActive}, true},
		{"EnteredActive触发", domain.AlertMappingResult{FaultID: fault.ID, Transition: domain.FaultTransitionEnteredActive}, true},
		{"None不触发", domain.AlertMappingResult{FaultID: fault.ID, Transition: domain.FaultTransitionNone}, false},
		{"空FaultID不触发", domain.AlertMappingResult{FaultID: "", Transition: domain.FaultTransitionCreatedActive}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := &fakeStormScheduler{}
			faults := newFakeFaultOutboxFaultRepo()
			faults.byID[fault.ID] = fault
			ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}}
			outbox := newTestFaultOutbox(sched, faults, ops, nil, now)

			if err := outbox.TriggerFromMappingResult(context.Background(), nil, tc.result); err != nil {
				t.Fatalf("TriggerFromMappingResult 失败: %v", err)
			}
			calls := scheduleCallsOfKind(sched, notifySendTimerKind)
			queued := len(calls) > 0
			if queued != tc.wantQueued {
				t.Fatalf("queued = %v, want %v（Schedule 调用数 %d）", queued, tc.wantQueued, len(calls))
			}
		})
	}
}

// TestFaultOutboxQueueFaultActivated_DedupOnRepeat 覆盖 R5.5/R5.11：同一 Fault
// 被两次调用时只留一条 pending notify_send 定时器——refID 不含 round，Schedule
// 对同 (kind, refID) 是替换语义，天然收敛。
func TestFaultOutboxQueueFaultActivated_DedupOnRepeat(t *testing.T) {
	now := time.Now()
	sched := &fakeStormScheduler{}
	faults := newFakeFaultOutboxFaultRepo()
	ops := &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{}}
	outbox := newTestFaultOutbox(sched, faults, ops, nil, now)

	fault := testFault("f6", "corr-6", "", "")
	if err := outbox.QueueFaultActivated(context.Background(), nil, fault); err != nil {
		t.Fatalf("第一次调用失败: %v", err)
	}
	// 第二次调用发生在稍后时刻（不同 round），但 refID 只由业务元组决定。
	outbox.now = func() time.Time { return now.Add(time.Second) }
	if err := outbox.QueueFaultActivated(context.Background(), nil, fault); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}

	calls := scheduleCallsOfKind(sched, notifySendTimerKind)
	if len(calls) != 2 {
		t.Fatalf("Schedule 应被调用两次（各自替换同一条 pending 记录），got %d", len(calls))
	}
	if calls[0].refID != calls[1].refID {
		t.Fatalf("两次调用的 refID 应相同（不含 round），got %q 和 %q", calls[0].refID, calls[1].refID)
	}
}
