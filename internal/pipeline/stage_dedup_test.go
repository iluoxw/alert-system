package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// fakeAlertRepo 是 AlertRepo 的可编程测试替身：记录调用参数，返回预设结果。
type fakeAlertRepo struct {
	upsertAlert   domain.Alert
	upsertCreated bool
	upsertErr     error
	upsertEvents  []domain.AlertEvent

	getByFPAlert domain.Alert
	getByFPErr   error
	getByFPCalls []string

	updateStatusErr   error
	updateStatusCalls []domain.Alert
}

func (f *fakeAlertRepo) UpsertByFingerprint(ctx context.Context, tx pgx.Tx, event domain.AlertEvent, integrationID string) (domain.Alert, bool, error) {
	f.upsertEvents = append(f.upsertEvents, event)
	if f.upsertErr != nil {
		return domain.Alert{}, false, f.upsertErr
	}
	return f.upsertAlert, f.upsertCreated, nil
}

func (f *fakeAlertRepo) GetByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint string) (domain.Alert, error) {
	f.getByFPCalls = append(f.getByFPCalls, fingerprint)
	if f.getByFPErr != nil {
		return domain.Alert{}, f.getByFPErr
	}
	return f.getByFPAlert, nil
}

func (f *fakeAlertRepo) UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	f.updateStatusCalls = append(f.updateStatusCalls, alert)
	return f.updateStatusErr
}

// fakeTimelineRepo 是 TimelineRepo 的可编程测试替身。
type fakeTimelineRepo struct {
	appendCalls []domain.TimelineEntry
	appendErr   error
}

func (f *fakeTimelineRepo) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.appendCalls = append(f.appendCalls, entry)
	if f.appendErr != nil {
		return "", f.appendErr
	}
	return "timeline-entry-1", nil
}

// scheduleCall/cancelCall 记录 fakeScheduler 收到的调用参数。
type scheduleCall struct {
	kind, refID string
	fireAt      time.Time
}

type cancelCall struct {
	kind, refID string
}

// fakeScheduler 是 TimerScheduler 的可编程测试替身。
type fakeScheduler struct {
	scheduleCalls []scheduleCall
	scheduleErr   error
	cancelCalls   []cancelCall
	cancelErr     error
}

func (f *fakeScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	f.scheduleCalls = append(f.scheduleCalls, scheduleCall{kind: kind, refID: refID, fireAt: fireAt})
	return f.scheduleErr
}

func (f *fakeScheduler) Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error {
	f.cancelCalls = append(f.cancelCalls, cancelCall{kind: kind, refID: refID})
	return f.cancelErr
}

// fixedClock 是 dedupClock 的可控实现，让测试断言精确到期时间无需依赖 wall-clock。
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newDedupStageForTest(alerts AlertRepo, timeline TimelineRepo, sched TimerScheduler, fallback SeverityFallback, now time.Time) *DedupStage {
	s := NewDedupStage(alerts, timeline, sched, fallback)
	s.clock = fixedClock{t: now}
	return s
}

func TestDedupStage_Name(t *testing.T) {
	s := NewDedupStage(&fakeAlertRepo{}, &fakeTimelineRepo{}, &fakeScheduler{}, nil)
	if got := s.Name(); got != "dedup" {
		t.Fatalf("Name() = %q, want %q", got, "dedup")
	}
}

// ① 新建告警：created=true，resolve_policy=ttl 且 ttl>0 时注册一次 TTL 定时器。
func TestDedupStage_Process_NewFiringAlert_SchedulesTTL(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	alertRepo := &fakeAlertRepo{
		upsertAlert: domain.Alert{
			ID:            "alert-1",
			ResolvePolicy: domain.ResolvePolicyTTL,
			TTLSeconds:    300,
		},
		upsertCreated: true,
	}
	sched := &fakeScheduler{}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, sched, nil, now)

	pctx := &PipelineContext{
		Event: domain.AlertEvent{
			Fingerprint:   "fp-1",
			Status:        domain.StatusFiring,
			Severity:      domain.SeverityCritical,
			ResolvePolicy: domain.ResolvePolicyTTL,
			TTLSeconds:    300,
		},
	}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if !pctx.Created {
		t.Fatal("Created = false, want true（新建告警）")
	}
	if pctx.Alert.ID != "alert-1" {
		t.Fatalf("pctx.Alert.ID = %q, want alert-1", pctx.Alert.ID)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("Schedule 被调用 %d 次, want 1", len(sched.scheduleCalls))
	}
	call := sched.scheduleCalls[0]
	if call.kind != timerKindTTL || call.refID != "alert-1" {
		t.Fatalf("Schedule 调用参数 = %+v, want kind=%s refID=alert-1", call, timerKindTTL)
	}
	wantFireAt := now.Add(300 * time.Second)
	if !call.fireAt.Equal(wantFireAt) {
		t.Fatalf("Schedule fireAt = %v, want %v", call.fireAt, wantFireAt)
	}
}

// ② 合并告警：created=false 时续期 TTL（同样调 Schedule，替换语义），且不产生任何
// "新建"信号——下游 stage 依据 pctx.Created 判断是否需要发通知。
func TestDedupStage_Process_MergedFiringAlert_RenewsTTLWithoutNewFlag(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	alertRepo := &fakeAlertRepo{
		upsertAlert: domain.Alert{
			ID:            "alert-2",
			ResolvePolicy: domain.ResolvePolicyTTL,
			TTLSeconds:    60,
			DedupCount:    3,
		},
		upsertCreated: false,
	}
	sched := &fakeScheduler{}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, sched, nil, now)

	pctx := &PipelineContext{
		Event: domain.AlertEvent{
			Fingerprint:   "fp-2",
			Status:        domain.StatusFiring,
			Severity:      domain.SeverityWarning,
			ResolvePolicy: domain.ResolvePolicyTTL,
			TTLSeconds:    60,
		},
	}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if pctx.Created {
		t.Fatal("Created = true, want false（合并告警不应标记为新建）")
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("Schedule（续期）被调用 %d 次, want 1", len(sched.scheduleCalls))
	}
	wantFireAt := now.Add(60 * time.Second)
	if !sched.scheduleCalls[0].fireAt.Equal(wantFireAt) {
		t.Fatalf("续期 fireAt = %v, want %v", sched.scheduleCalls[0].fireAt, wantFireAt)
	}
}

// 非 TTL 策略不应注册定时器。
func TestDedupStage_Process_NonTTLPolicy_NoScheduleCall(t *testing.T) {
	alertRepo := &fakeAlertRepo{
		upsertAlert:   domain.Alert{ID: "alert-3", ResolvePolicy: domain.ResolvePolicySource},
		upsertCreated: true,
	}
	sched := &fakeScheduler{}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, sched, nil, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{
		Fingerprint:   "fp-3",
		Status:        domain.StatusFiring,
		Severity:      domain.SeverityInfo,
		ResolvePolicy: domain.ResolvePolicySource,
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("非 ttl 策略不应调用 Schedule，实际调用 %d 次", len(sched.scheduleCalls))
	}
}

// ③ resolved 事件命中现存未 resolved 告警：Resolve + UpdateStatus + 时间线 + Cancel 定时器。
func TestDedupStage_Process_ResolvedEvent_ExistingAlert_ResolvesAndCancelsTimer(t *testing.T) {
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	starts := now.Add(-10 * time.Minute)
	alertRepo := &fakeAlertRepo{
		getByFPAlert: domain.Alert{
			ID:          "alert-4",
			Fingerprint: "fp-4",
			Status:      domain.StatusFiring,
			StartsAt:    starts,
		},
	}
	timeline := &fakeTimelineRepo{}
	sched := &fakeScheduler{}
	stage := newDedupStageForTest(alertRepo, timeline, sched, nil, now)

	pctx := &PipelineContext{Event: domain.AlertEvent{
		Fingerprint: "fp-4",
		Status:      domain.StatusResolved,
		Severity:    domain.SeverityCritical,
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}

	if len(alertRepo.getByFPCalls) != 1 || alertRepo.getByFPCalls[0] != "fp-4" {
		t.Fatalf("GetByFingerprint 调用 = %v, want [fp-4]", alertRepo.getByFPCalls)
	}
	if len(alertRepo.updateStatusCalls) != 1 {
		t.Fatalf("UpdateStatus 被调用 %d 次, want 1", len(alertRepo.updateStatusCalls))
	}
	if got := alertRepo.updateStatusCalls[0].Status; got != domain.StatusResolved {
		t.Fatalf("UpdateStatus 收到的 Status = %q, want resolved", got)
	}
	if len(timeline.appendCalls) != 1 {
		t.Fatalf("时间线 Append 被调用 %d 次, want 1", len(timeline.appendCalls))
	}
	if got := timeline.appendCalls[0].Action; got != domain.TimelineActionResolve {
		t.Fatalf("时间线 Action = %q, want resolve", got)
	}
	if len(sched.cancelCalls) != 1 || sched.cancelCalls[0] != (cancelCall{kind: timerKindTTL, refID: "alert-4"}) {
		t.Fatalf("Cancel 调用 = %v, want [{%s alert-4}]", sched.cancelCalls, timerKindTTL)
	}
	if pctx.Alert.Status != domain.StatusResolved {
		t.Fatalf("pctx.Alert.Status = %q, want resolved", pctx.Alert.Status)
	}
	if pctx.Created {
		t.Fatal("resolved 路径不应把 Created 置为 true")
	}
}

// ④ resolved 事件找不到现存未 resolved 告警：视为迟到事件，忽略、不报错、不产生任何写操作。
func TestDedupStage_Process_ResolvedEvent_NoExistingAlert_Ignored(t *testing.T) {
	alertRepo := &fakeAlertRepo{getByFPErr: store.ErrAlertNotFound}
	timeline := &fakeTimelineRepo{}
	sched := &fakeScheduler{}
	stage := newDedupStageForTest(alertRepo, timeline, sched, nil, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{
		Fingerprint: "fp-5",
		Status:      domain.StatusResolved,
		Severity:    domain.SeverityWarning,
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 对迟到的 resolved 事件返回了意外错误: %v", err)
	}
	if len(alertRepo.updateStatusCalls) != 0 {
		t.Fatalf("不应调用 UpdateStatus，实际调用 %d 次", len(alertRepo.updateStatusCalls))
	}
	if len(timeline.appendCalls) != 0 {
		t.Fatalf("不应写时间线，实际调用 %d 次", len(timeline.appendCalls))
	}
	if len(sched.cancelCalls) != 0 {
		t.Fatalf("不应调用 Cancel，实际调用 %d 次", len(sched.cancelCalls))
	}
}

// resolved 事件查询出现非"未找到"的真实错误时，应向上传播为 stage 失败。
func TestDedupStage_Process_ResolvedEvent_QueryError_Propagates(t *testing.T) {
	wantErr := errors.New("数据库连接失败")
	alertRepo := &fakeAlertRepo{getByFPErr: wantErr}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, &fakeScheduler{}, nil, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{Fingerprint: "fp-6", Status: domain.StatusResolved, Severity: domain.SeverityInfo}}

	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑤ severity 缺失/非法时调用注入的兜底函数，并置 SeverityFallback 标记。
func TestDedupStage_Process_SeverityFallback_UsesInjectedFunc(t *testing.T) {
	alertRepo := &fakeAlertRepo{upsertAlert: domain.Alert{ID: "alert-7"}, upsertCreated: true}
	var gotIntegrationID string
	fallback := func(ctx context.Context, tx pgx.Tx, integrationID string) (domain.Severity, error) {
		gotIntegrationID = integrationID
		return domain.SeverityCritical, nil
	}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, &fakeScheduler{}, fallback, time.Now())

	pctx := &PipelineContext{
		IntegrationID: "integration-9",
		Event: domain.AlertEvent{
			Fingerprint: "fp-7",
			Status:      domain.StatusFiring,
			Severity:    "", // 缺失
		},
	}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if gotIntegrationID != "integration-9" {
		t.Fatalf("兜底函数收到的 integrationID = %q, want integration-9", gotIntegrationID)
	}
	if len(alertRepo.upsertEvents) != 1 {
		t.Fatalf("UpsertByFingerprint 被调用 %d 次, want 1", len(alertRepo.upsertEvents))
	}
	got := alertRepo.upsertEvents[0]
	if got.Severity != domain.SeverityCritical {
		t.Fatalf("upsert 收到的 Severity = %q, want critical", got.Severity)
	}
	if !got.SeverityFallback {
		t.Fatal("upsert 收到的 SeverityFallback = false, want true")
	}
}

// severity 兜底函数为 nil 时，缺失/非法 severity 退回 domain.SeverityWarning。
func TestDedupStage_Process_SeverityFallback_NilFunc_DefaultsToWarning(t *testing.T) {
	alertRepo := &fakeAlertRepo{upsertAlert: domain.Alert{ID: "alert-8"}, upsertCreated: true}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, &fakeScheduler{}, nil, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{
		Fingerprint: "fp-8",
		Status:      domain.StatusFiring,
		Severity:    "not-a-real-severity",
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	got := alertRepo.upsertEvents[0]
	if got.Severity != domain.SeverityWarning {
		t.Fatalf("upsert 收到的 Severity = %q, want warning", got.Severity)
	}
	if !got.SeverityFallback {
		t.Fatal("upsert 收到的 SeverityFallback = false, want true")
	}
}

// 合法 severity 不应触发兜底函数，也不应置 SeverityFallback。
func TestDedupStage_Process_ValidSeverity_NoFallbackTriggered(t *testing.T) {
	alertRepo := &fakeAlertRepo{upsertAlert: domain.Alert{ID: "alert-9"}, upsertCreated: true}
	called := false
	fallback := func(ctx context.Context, tx pgx.Tx, integrationID string) (domain.Severity, error) {
		called = true
		return domain.SeverityCritical, nil
	}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, &fakeScheduler{}, fallback, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{
		Fingerprint: "fp-9",
		Status:      domain.StatusFiring,
		Severity:    domain.SeverityInfo,
	}}

	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() 返回意外错误: %v", err)
	}
	if called {
		t.Fatal("合法 severity 不应调用兜底函数")
	}
	got := alertRepo.upsertEvents[0]
	if got.Severity != domain.SeverityInfo || got.SeverityFallback {
		t.Fatalf("upsert 收到的 event = %+v, severity/fallback 不应被改动", got)
	}
}

// severity 兜底函数报错时，Process 应整体失败（不应静默吞掉错误继续 upsert）。
func TestDedupStage_Process_SeverityFallback_FuncError_Propagates(t *testing.T) {
	wantErr := errors.New("查询 integration 默认 severity 失败")
	fallback := func(ctx context.Context, tx pgx.Tx, integrationID string) (domain.Severity, error) {
		return "", wantErr
	}
	alertRepo := &fakeAlertRepo{}
	stage := newDedupStageForTest(alertRepo, &fakeTimelineRepo{}, &fakeScheduler{}, fallback, time.Now())

	pctx := &PipelineContext{Event: domain.AlertEvent{Fingerprint: "fp-10", Status: domain.StatusFiring, Severity: ""}}

	err := stage.Process(context.Background(), pctx)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
	if len(alertRepo.upsertEvents) != 0 {
		t.Fatal("severity 兜底失败后不应继续调用 UpsertByFingerprint")
	}
}
