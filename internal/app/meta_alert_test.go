package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

type fakeMetaFaultRepo struct {
	timeline []domain.FaultTimelineEntry
}

func (f *fakeMetaFaultRepo) AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error) {
	f.timeline = append(f.timeline, entry)
	return "ftl-1", nil
}

func TestScheduleNotifyFailureMetaAlert_SchedulesOnceWithOriginalRound(t *testing.T) {
	sched := &fakeStormScheduler{}
	faults := &fakeMetaFaultRepo{}
	original := notify.SendRequest{
		AlertID:     "alert-1",
		ChannelKind: "webhook",
		Target:      "https://hooks.example/secret-token",
		Round:       99,
		Msg:         notify.Message{Title: "原标题", Body: "含 https://hooks.example/secret-token 不应出现"},
	}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, faults, "lark_webhook", "https://fallback.example/hook", now, original); err != nil {
		t.Fatalf("scheduleNotifyFailureMetaAlert: %v", err)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("应恰好 Schedule 1 次，got %d", len(sched.scheduleCalls))
	}
	call := sched.scheduleCalls[0]
	if call.kind != notifySendTimerKind {
		t.Fatalf("kind = %q, want notify_send", call.kind)
	}
	var meta notify.SendRequest
	if err := json.Unmarshal(call.payload, &meta); err != nil {
		t.Fatalf("反序列化 payload: %v", err)
	}
	if !meta.MetaAlert {
		t.Fatal("payload 必须带 meta_alert=true")
	}
	if meta.Round != 99 {
		t.Fatalf("round = %d, want 99（沿用原意图）", meta.Round)
	}
	if meta.ChannelKind != "lark_webhook" || meta.Target != "https://fallback.example/hook" {
		t.Fatalf("元告警目标不对: %+v", meta)
	}
	if strings.Contains(meta.Msg.Body, "hooks.example") || strings.Contains(meta.Msg.Title, "secret-token") {
		t.Fatalf("元告警正文泄露了原始目标 URL: title=%q body=%q", meta.Msg.Title, meta.Msg.Body)
	}
	if strings.Contains(string(call.payload), "secret-token") && !strings.Contains(string(call.payload), "fallback.example") {
		t.Fatalf("payload 含原始凭据: %s", call.payload)
	}
	if strings.Contains(string(call.payload), "https://hooks.example/secret-token") {
		t.Fatalf("payload 不得包含原始目标 URL: %s", call.payload)
	}
	if call.refID != notifySendRefID(meta) {
		t.Fatalf("refID 应走 notifySendRefID（不含 round）")
	}
	if len(faults.timeline) != 0 {
		t.Fatalf("有兜底目标时不应写「无兜底」时间线，got %d", len(faults.timeline))
	}
}

func TestScheduleNotifyFailureMetaAlert_EmptyFallbackWritesTimeline(t *testing.T) {
	sched := &fakeStormScheduler{}
	faults := &fakeMetaFaultRepo{}
	original := notify.SendRequest{
		GroupKey:    "fault:fault-1:activated",
		ChannelKind: "lark_webhook",
		Target:      "https://hooks.example/secret",
		Round:       1,
		Msg:         notify.Message{ResourceType: "fault", ResourceID: "fault-1"},
	}
	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, faults, "", "", time.Now(), original); err != nil {
		t.Fatalf("scheduleNotifyFailureMetaAlert: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("无兜底目标不得 Schedule，got %d", len(sched.scheduleCalls))
	}
	if len(faults.timeline) != 1 {
		t.Fatalf("无兜底目标必须写时间线，got %d", len(faults.timeline))
	}
	entry := faults.timeline[0]
	if entry.Action != domain.FaultTimelineActionNotifySkipped || entry.Detail["reason"] != "notify_send_failed_no_fallback" {
		t.Fatalf("时间线不对: %+v", entry)
	}
	if entry.FaultID != "fault-1" {
		t.Fatalf("FaultID = %q, want fault-1", entry.FaultID)
	}
}

func TestScheduleNotifyFailureMetaAlert_MetaAlertInputNoOp(t *testing.T) {
	sched := &fakeStormScheduler{}
	original := notify.SendRequest{AlertID: "a1", MetaAlert: true, Round: 1}
	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, nil, "lark_webhook", "https://fallback.example/hook", time.Now(), original); err != nil {
		t.Fatalf("scheduleNotifyFailureMetaAlert: %v", err)
	}
	if len(sched.scheduleCalls) != 0 {
		t.Fatalf("元告警自身不得再 Schedule，got %d", len(sched.scheduleCalls))
	}
}

func TestScheduleNotifyFailureMetaAlert_SameRoundKeepsSameRefID(t *testing.T) {
	sched := &fakeStormScheduler{}
	original := notify.SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x", Round: 7}
	now := time.Now()
	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, nil, "lark_webhook", "https://fallback.example/hook", now, original); err != nil {
		t.Fatal(err)
	}
	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, nil, "lark_webhook", "https://fallback.example/hook", now, original); err != nil {
		t.Fatal(err)
	}
	if sched.scheduleCalls[0].refID != sched.scheduleCalls[1].refID {
		t.Fatalf("同一原始 round 的元告警 refID 必须相同，才能靠 Schedule 替换 + notification_log 幂等去重")
	}
	if strings.Contains(sched.scheduleCalls[0].refID, "7") {
		t.Fatalf("refID 不应掺 round（不变量 4）: %q", sched.scheduleCalls[0].refID)
	}
}

func TestFaultIDFromNotifyGroupKey(t *testing.T) {
	if got := faultIDFromNotifyGroupKey("fault:abc-1:activated"); got != "abc-1" {
		t.Fatalf("got %q", got)
	}
	if got := faultIDFromNotifyGroupKey("ops:team-1:daily"); got != "" {
		t.Fatalf("非 fault GroupKey 应为空，got %q", got)
	}
}

// 回归：fallback 与原目标相同时，元告警的 notify_send refID 必须与原意图不同，
// 否则两者在 pending 收敛里互相覆盖 payload，后注册的一方被静默丢掉。
func TestScheduleNotifyFailureMetaAlert_SameTargetStillDistinctRefID(t *testing.T) {
	const fbChannel, fbTarget = "lark_webhook", "https://fallback.example/hook"
	sched := &fakeStormScheduler{}
	original := notify.SendRequest{AlertID: "alert-1", ChannelKind: fbChannel, Target: fbTarget, Round: 7}

	if err := scheduleNotifyFailureMetaAlert(context.Background(), nil, sched, nil, fbChannel, fbTarget, time.Now(), original); err != nil {
		t.Fatalf("调度元告警失败: %v", err)
	}
	if len(sched.scheduleCalls) != 1 {
		t.Fatalf("期望调度 1 条，实际 %d", len(sched.scheduleCalls))
	}
	if got, orig := sched.scheduleCalls[0].refID, notifySendRefID(original); got == orig {
		t.Fatalf("元告警 refID 与原意图相同（%q），pending 收敛会互相覆盖", got)
	}
}
