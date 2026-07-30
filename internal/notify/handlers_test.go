package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// lifecycleMutableClock 是 handlerClock 的可控实现，测试通过直接改写字段推进时间。
type lifecycleMutableClock struct {
	t time.Time
}

func (c *lifecycleMutableClock) Now() time.Time { return c.t }

// lifecycleFakeAlerts 是 lifecycleAlertRepo 的可编程测试替身。
type lifecycleFakeAlerts struct {
	byID map[string]domain.Alert

	getErr    error
	updateErr error

	updateCalls []domain.Alert
}

func (f *lifecycleFakeAlerts) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	if f.getErr != nil {
		return domain.Alert{}, f.getErr
	}
	a, ok := f.byID[id]
	if !ok {
		return domain.Alert{}, store.ErrAlertNotFound
	}
	return a, nil
}

func (f *lifecycleFakeAlerts) UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	f.updateCalls = append(f.updateCalls, alert)
	if f.updateErr != nil {
		return f.updateErr
	}
	f.byID[alert.ID] = alert
	return nil
}

// lifecycleFakeTimeline 是 lifecycleTimelineRepo 的测试替身。
type lifecycleFakeTimeline struct {
	err     error
	entries []domain.TimelineEntry
}

func (f *lifecycleFakeTimeline) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.entries = append(f.entries, entry)
	return "entry-id", nil
}

// lifecycleScheduleCall 记录一次 lifecycleFakeScheduler.Schedule 调用的参数。
type lifecycleScheduleCall struct {
	kind   string
	refID  string
	fireAt time.Time
}

// lifecycleFakeScheduler 是 lifecycleScheduler 的可编程测试替身。
type lifecycleFakeScheduler struct {
	scheduleErr error
	cancelErr   error

	scheduleCalls []lifecycleScheduleCall
	cancelCalls   []string // "kind|refID"
}

func (f *lifecycleFakeScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	f.scheduleCalls = append(f.scheduleCalls, lifecycleScheduleCall{kind: kind, refID: refID, fireAt: fireAt})
	return f.scheduleErr
}

func (f *lifecycleFakeScheduler) Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error {
	f.cancelCalls = append(f.cancelCalls, kind+"|"+refID)
	return f.cancelErr
}

// lifecycleFakeEscalation 是 lifecycleEscalationStopper 的测试替身。
type lifecycleFakeEscalation struct {
	err       error
	stopCalls []string
}

func (f *lifecycleFakeEscalation) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	f.stopCalls = append(f.stopCalls, alertID)
	return f.err
}

// fakeRepeatNotifier 是 RepeatNotifier 的测试替身。
type fakeRepeatNotifier struct {
	err   error
	calls []domain.Alert
}

func (f *fakeRepeatNotifier) NotifyRepeat(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	f.calls = append(f.calls, alert)
	return f.err
}

func newLifecycleHandlersForTest(alerts *lifecycleFakeAlerts, timeline *lifecycleFakeTimeline, sched *lifecycleFakeScheduler, escalation lifecycleEscalationStopper, notifier RepeatNotifier, repeatInterval time.Duration, clock handlerClock) *LifecycleHandlers {
	h := NewLifecycleHandlers(alerts, timeline, sched, escalation, notifier, repeatInterval)
	h.clock = clock
	return h
}

func baseFiringAlert(id string) domain.Alert {
	return domain.Alert{
		ID:       id,
		Status:   domain.StatusFiring,
		StartsAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
}

// lifecycleStringSlicesEqual 按顺序比较两个字符串切片，供断言 lifecycleFakeScheduler
// 记录的 Cancel 调用顺序/内容是否与预期一致。
func lifecycleStringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ① TTL 到期：firing 告警自动 resolve（reason=ttl），写时间线，清理 repeat 与
// 人员升级链定时器（"log 告警超时自愈"场景）。
func TestLifecycleHandlers_HandleTTL_FiringAlert_AutoResolves(t *testing.T) {
	alert := baseFiringAlert("alert-1")
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-1": alert}}
	timeline := &lifecycleFakeTimeline{}
	sched := &lifecycleFakeScheduler{}
	escalation := &lifecycleFakeEscalation{}
	now := time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)
	h := newLifecycleHandlersForTest(alerts, timeline, sched, escalation, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: now})

	if err := h.HandleTTL(context.Background(), nil, "alert-1", nil); err != nil {
		t.Fatalf("HandleTTL() 返回意外错误: %v", err)
	}

	if len(alerts.updateCalls) != 1 {
		t.Fatalf("UpdateStatus 调用次数 = %d, want 1", len(alerts.updateCalls))
	}
	got := alerts.updateCalls[0]
	if got.Status != domain.StatusResolved {
		t.Fatalf("Status = %q, want resolved", got.Status)
	}
	if !got.EndsAt.Equal(now) {
		t.Fatalf("EndsAt = %v, want %v", got.EndsAt, now)
	}

	if len(timeline.entries) != 1 || timeline.entries[0].Action != domain.TimelineActionResolve {
		t.Fatalf("时间线记录 = %+v, want 1 条 resolve", timeline.entries)
	}
	if reason, _ := timeline.entries[0].Detail["reason"].(string); reason != string(domain.ResolveReasonTTL) {
		t.Fatalf("时间线 reason = %q, want %q", reason, domain.ResolveReasonTTL)
	}

	wantCancels := []string{TimerKindAlertRepeat + "|alert-1", TimerKindChannelEscalation + "|alert-1"}
	if !lifecycleStringSlicesEqual(sched.cancelCalls, wantCancels) {
		t.Fatalf("Cancel 调用 = %v, want %v", sched.cancelCalls, wantCancels)
	}
	if len(escalation.stopCalls) != 1 || escalation.stopCalls[0] != "alert-1" {
		t.Fatalf("升级链 Stop 调用 = %v, want [alert-1]", escalation.stopCalls)
	}
}

// ② acknowledged 告警 TTL 到期同样应自动 resolve（acknowledged -> resolved 合法）。
func TestLifecycleHandlers_HandleTTL_AcknowledgedAlert_AutoResolves(t *testing.T) {
	alert := baseFiringAlert("alert-2")
	alert.Status = domain.StatusAcknowledged
	alert.AckedBy = "user-1"
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-2": alert}}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleTTL(context.Background(), nil, "alert-2", nil); err != nil {
		t.Fatalf("HandleTTL() 返回意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 1 || alerts.updateCalls[0].Status != domain.StatusResolved {
		t.Fatalf("UpdateStatus 调用 = %+v, want resolved", alerts.updateCalls)
	}
}

// ③ 已 resolved 的告警：幂等跳过，不产生任何写入（重投场景）。
func TestLifecycleHandlers_HandleTTL_AlreadyResolved_Idempotent(t *testing.T) {
	alert := baseFiringAlert("alert-3")
	alert.Status = domain.StatusResolved
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-3": alert}}
	timeline := &lifecycleFakeTimeline{}
	sched := &lifecycleFakeScheduler{}
	escalation := &lifecycleFakeEscalation{}
	h := newLifecycleHandlersForTest(alerts, timeline, sched, escalation, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleTTL(context.Background(), nil, "alert-3", nil); err != nil {
		t.Fatalf("HandleTTL() 返回意外错误: %v", err)
	}
	if len(alerts.updateCalls) != 0 || len(timeline.entries) != 0 || len(sched.cancelCalls) != 0 || len(escalation.stopCalls) != 0 {
		t.Fatal("已 resolved 的告警应幂等跳过，不应产生任何写入")
	}
}

// ④ 告警不存在（理论上不应发生）：直接返回成功，不报错。
func TestLifecycleHandlers_HandleTTL_AlertNotFound_ReturnsNil(t *testing.T) {
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{}}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleTTL(context.Background(), nil, "missing", nil); err != nil {
		t.Fatalf("HandleTTL() 对不存在的告警返回了意外错误: %v", err)
	}
}

// ⑤ GetByID 非 ErrAlertNotFound 的错误应向上传播。
func TestLifecycleHandlers_HandleTTL_GetByIDError_Propagates(t *testing.T) {
	wantErr := errors.New("查询失败")
	alerts := &lifecycleFakeAlerts{getErr: wantErr}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	err := h.HandleTTL(context.Background(), nil, "alert-x", nil)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("HandleTTL() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
}

// ⑥ UpdateStatus/Append/Cancel/escalation.Stop 各自失败都应向上传播。
func TestLifecycleHandlers_HandleTTL_DownstreamErrors_Propagate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(alerts *lifecycleFakeAlerts, timeline *lifecycleFakeTimeline, sched *lifecycleFakeScheduler, escalation *lifecycleFakeEscalation, wantErr error)
	}{
		{"UpdateStatus", func(alerts *lifecycleFakeAlerts, _ *lifecycleFakeTimeline, _ *lifecycleFakeScheduler, _ *lifecycleFakeEscalation, wantErr error) {
			alerts.updateErr = wantErr
		}},
		{"Append", func(_ *lifecycleFakeAlerts, timeline *lifecycleFakeTimeline, _ *lifecycleFakeScheduler, _ *lifecycleFakeEscalation, wantErr error) {
			timeline.err = wantErr
		}},
		{"Cancel", func(_ *lifecycleFakeAlerts, _ *lifecycleFakeTimeline, sched *lifecycleFakeScheduler, _ *lifecycleFakeEscalation, wantErr error) {
			sched.cancelErr = wantErr
		}},
		{"EscalationStop", func(_ *lifecycleFakeAlerts, _ *lifecycleFakeTimeline, _ *lifecycleFakeScheduler, escalation *lifecycleFakeEscalation, wantErr error) {
			escalation.err = wantErr
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr := errors.New(tc.name + " 失败")
			alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-y": baseFiringAlert("alert-y")}}
			timeline := &lifecycleFakeTimeline{}
			sched := &lifecycleFakeScheduler{}
			escalation := &lifecycleFakeEscalation{}
			tc.mutate(alerts, timeline, sched, escalation, wantErr)
			h := newLifecycleHandlersForTest(alerts, timeline, sched, escalation, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

			err := h.HandleTTL(context.Background(), nil, "alert-y", nil)
			if err == nil || !errors.Is(err, wantErr) {
				t.Fatalf("HandleTTL() 错误 = %v, 期望包裹 %v", err, wantErr)
			}
		})
	}
}

// ⑦ escalation 为 nil 时（未接入 oncall 的部署）：HandleTTL 不应 panic，只跳过
// 停止升级链这一步，repeat 定时器仍应被取消。
func TestLifecycleHandlers_HandleTTL_NilEscalation_SkipsStopWithoutPanic(t *testing.T) {
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-z": baseFiringAlert("alert-z")}}
	sched := &lifecycleFakeScheduler{}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, nil, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleTTL(context.Background(), nil, "alert-z", nil); err != nil {
		t.Fatalf("HandleTTL() 返回意外错误: %v", err)
	}
	if len(sched.cancelCalls) != 2 {
		t.Fatalf("Cancel 调用次数 = %d, want 2（escalation 为 nil 不影响 repeat/渠道升级定时器清理）", len(sched.cancelCalls))
	}
}

// ⑧ repeat 到期：firing 且未静默，记录重发意图并续约下一轮（fireAt = now + repeatInterval）。
func TestLifecycleHandlers_HandleRepeat_FiringUnsilenced_NotifiesAndReschedules(t *testing.T) {
	alert := baseFiringAlert("alert-4")
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-4": alert}}
	sched := &lifecycleFakeScheduler{}
	notifier := &fakeRepeatNotifier{}
	now := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	repeatInterval := 5 * time.Minute
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, notifier, repeatInterval, &lifecycleMutableClock{t: now})

	if err := h.HandleRepeat(context.Background(), nil, "alert-4", nil); err != nil {
		t.Fatalf("HandleRepeat() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].ID != "alert-4" {
		t.Fatalf("NotifyRepeat 调用 = %+v, want 针对 alert-4 调用一次", notifier.calls)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("Schedule 调用次数 = %d, want 1", len(sched.scheduleCalls))
	}
	call := sched.scheduleCalls[0]
	if call.kind != TimerKindAlertRepeat || call.refID != "alert-4" {
		t.Fatalf("Schedule 调用 = %+v, want kind=%s refID=alert-4", call, TimerKindAlertRepeat)
	}
	if !call.fireAt.Equal(now.Add(repeatInterval)) {
		t.Fatalf("fireAt = %v, want %v", call.fireAt, now.Add(repeatInterval))
	}
}

// ⑨ 静默期间：不发不续约（"静默期间 repeat 不发"场景）。
func TestLifecycleHandlers_HandleRepeat_Silenced_DoesNotNotifyOrReschedule(t *testing.T) {
	alert := baseFiringAlert("alert-5")
	alert.SilencedBy = []string{"silence-1"}
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-5": alert}}
	sched := &lifecycleFakeScheduler{}
	notifier := &fakeRepeatNotifier{}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, notifier, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleRepeat(context.Background(), nil, "alert-5", nil); err != nil {
		t.Fatalf("HandleRepeat() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 || len(sched.scheduleCalls) != 0 {
		t.Fatal("静默期间不应调用 NotifyRepeat 或续约定时器")
	}
}

// ⑩ 解静默后恢复：同一告警清空 SilencedBy 后再次到期应正常发送并续约
// （"解静默后恢复"场景——本 handler 层面即"静默解除后下一次到期正常处理"）。
func TestLifecycleHandlers_HandleRepeat_UnsilencedAfterward_ResumesNotifying(t *testing.T) {
	alert := baseFiringAlert("alert-6")
	alert.SilencedBy = []string{"silence-1"}
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-6": alert}}
	sched := &lifecycleFakeScheduler{}
	notifier := &fakeRepeatNotifier{}
	clock := &lifecycleMutableClock{t: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC)}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, notifier, time.Minute, clock)

	if err := h.HandleRepeat(context.Background(), nil, "alert-6", nil); err != nil {
		t.Fatalf("静默期间 HandleRepeat() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatal("静默期间不应通知")
	}

	// 模拟静默解除，时钟推进到下一次到期。
	unsilenced := alert
	unsilenced.SilencedBy = nil
	alerts.byID["alert-6"] = unsilenced
	clock.t = clock.t.Add(time.Minute)

	if err := h.HandleRepeat(context.Background(), nil, "alert-6", nil); err != nil {
		t.Fatalf("解静默后 HandleRepeat() 返回意外错误: %v", err)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].ID != "alert-6" {
		t.Fatalf("解静默后应恢复通知，NotifyRepeat 调用 = %+v", notifier.calls)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("解静默后应续约定时器，Schedule 调用次数 = %d, want 1", len(sched.scheduleCalls))
	}
}

// ⑪ acknowledged/resolved 告警：不发不续约。
func TestLifecycleHandlers_HandleRepeat_NotFiring_DoesNotNotifyOrReschedule(t *testing.T) {
	for _, status := range []domain.Status{domain.StatusAcknowledged, domain.StatusResolved} {
		t.Run(string(status), func(t *testing.T) {
			alert := baseFiringAlert("alert-7")
			alert.Status = status
			alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-7": alert}}
			sched := &lifecycleFakeScheduler{}
			notifier := &fakeRepeatNotifier{}
			h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, notifier, time.Minute, &lifecycleMutableClock{t: time.Now()})

			if err := h.HandleRepeat(context.Background(), nil, "alert-7", nil); err != nil {
				t.Fatalf("HandleRepeat() 返回意外错误: %v", err)
			}
			if len(notifier.calls) != 0 || len(sched.scheduleCalls) != 0 {
				t.Fatalf("状态 %s 不应通知或续约", status)
			}
		})
	}
}

// ⑫ 告警不存在：直接返回成功。
func TestLifecycleHandlers_HandleRepeat_AlertNotFound_ReturnsNil(t *testing.T) {
	alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{}}
	h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.HandleRepeat(context.Background(), nil, "missing", nil); err != nil {
		t.Fatalf("HandleRepeat() 对不存在的告警返回了意外错误: %v", err)
	}
}

// ⑬ NotifyRepeat/Schedule 失败应向上传播。
func TestLifecycleHandlers_HandleRepeat_DownstreamErrors_Propagate(t *testing.T) {
	t.Run("NotifyRepeat", func(t *testing.T) {
		wantErr := errors.New("记录重发意图失败")
		alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-8": baseFiringAlert("alert-8")}}
		h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{err: wantErr}, time.Minute, &lifecycleMutableClock{t: time.Now()})

		err := h.HandleRepeat(context.Background(), nil, "alert-8", nil)
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("HandleRepeat() 错误 = %v, 期望包裹 %v", err, wantErr)
		}
	})
	t.Run("Schedule", func(t *testing.T) {
		wantErr := errors.New("续约失败")
		alerts := &lifecycleFakeAlerts{byID: map[string]domain.Alert{"alert-9": baseFiringAlert("alert-9")}}
		sched := &lifecycleFakeScheduler{scheduleErr: wantErr}
		h := newLifecycleHandlersForTest(alerts, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

		err := h.HandleRepeat(context.Background(), nil, "alert-9", nil)
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("HandleRepeat() 错误 = %v, 期望包裹 %v", err, wantErr)
		}
	})
}

// ⑭ CancelLifecycleTimers：无 escalation 注入时只取消 repeat 定时器，不 panic。
func TestLifecycleHandlers_CancelLifecycleTimers_NilEscalation(t *testing.T) {
	sched := &lifecycleFakeScheduler{}
	h := newLifecycleHandlersForTest(&lifecycleFakeAlerts{byID: map[string]domain.Alert{}}, &lifecycleFakeTimeline{}, sched, nil, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

	if err := h.CancelLifecycleTimers(context.Background(), nil, "alert-10"); err != nil {
		t.Fatalf("CancelLifecycleTimers() 返回意外错误: %v", err)
	}
	wantCancels := []string{TimerKindAlertRepeat + "|alert-10", TimerKindChannelEscalation + "|alert-10"}
	if !lifecycleStringSlicesEqual(sched.cancelCalls, wantCancels) {
		t.Fatalf("Cancel 调用 = %v, want %v", sched.cancelCalls, wantCancels)
	}
}

// ⑮ CancelLifecycleTimers：Cancel/Stop 各自失败应向上传播。
func TestLifecycleHandlers_CancelLifecycleTimers_DownstreamErrors_Propagate(t *testing.T) {
	t.Run("Cancel", func(t *testing.T) {
		wantErr := errors.New("取消失败")
		sched := &lifecycleFakeScheduler{cancelErr: wantErr}
		h := newLifecycleHandlersForTest(&lifecycleFakeAlerts{byID: map[string]domain.Alert{}}, &lifecycleFakeTimeline{}, sched, &lifecycleFakeEscalation{}, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

		err := h.CancelLifecycleTimers(context.Background(), nil, "alert-11")
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("CancelLifecycleTimers() 错误 = %v, 期望包裹 %v", err, wantErr)
		}
	})
	t.Run("EscalationStop", func(t *testing.T) {
		wantErr := errors.New("停止升级链失败")
		escalation := &lifecycleFakeEscalation{err: wantErr}
		h := newLifecycleHandlersForTest(&lifecycleFakeAlerts{byID: map[string]domain.Alert{}}, &lifecycleFakeTimeline{}, &lifecycleFakeScheduler{}, escalation, &fakeRepeatNotifier{}, time.Minute, &lifecycleMutableClock{t: time.Now()})

		err := h.CancelLifecycleTimers(context.Background(), nil, "alert-12")
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("CancelLifecycleTimers() 错误 = %v, 期望包裹 %v", err, wantErr)
		}
	})
}
