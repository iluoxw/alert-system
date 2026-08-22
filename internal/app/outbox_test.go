package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/ops"
)

type outboxScheduleCall struct {
	kind, refID string
	fireAt      time.Time
	payload     []byte
}

type fakeOutboxScheduler struct {
	scheduled        []outboxScheduleCall
	scheduledIfEmpty []outboxScheduleCall
	err              error
}

func (f *fakeOutboxScheduler) Schedule(_ context.Context, _ pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	if f.err != nil {
		return f.err
	}
	f.scheduled = append(f.scheduled, outboxScheduleCall{kind: kind, refID: refID, fireAt: fireAt, payload: payload})
	return nil
}

func (f *fakeOutboxScheduler) ScheduleIfAbsent(_ context.Context, _ pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.scheduledIfEmpty = append(f.scheduledIfEmpty, outboxScheduleCall{kind: kind, refID: refID, fireAt: fireAt, payload: payload})
	return true, nil
}

type fakeTeamChannelRepo struct {
	byTeam map[string][]ops.ChannelTarget
	err    error
}

func (f *fakeTeamChannelRepo) TeamChannels(_ context.Context, _ pgx.Tx, teamID string) ([]ops.ChannelTarget, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]ops.ChannelTarget(nil), f.byTeam[teamID]...), nil
}

type fakeTeamTimeline struct {
	entries []domain.TimelineEntry
	err     error
}

func (f *fakeTeamTimeline) Append(_ context.Context, _ pgx.Tx, entry domain.TimelineEntry) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.entries = append(f.entries, entry)
	return "timeline-id", nil
}

type fakeOutboxEngine struct{ alerts []string }

func (f *fakeOutboxEngine) Start(_ context.Context, _ pgx.Tx, alert domain.Alert, _ time.Time) error {
	f.alerts = append(f.alerts, alert.ID)
	return nil
}

type fakeOutboxChain struct{ alerts []string }

func (f *fakeOutboxChain) Start(_ context.Context, _ pgx.Tx, alert domain.Alert) error {
	f.alerts = append(f.alerts, alert.ID)
	return nil
}

func newTestNotifyOutbox(scheduler notifyOutboxScheduler, channels teamNotificationChannelRepo, timeline teamNotificationTimeline, now time.Time) *notifyOutbox {
	return &notifyOutbox{
		scheduler:      scheduler,
		teamChannels:   channels,
		timeline:       timeline,
		repeatInterval: 5 * time.Minute,
		now:            func() time.Time { return now },
	}
}

func sentRequests(t *testing.T, calls []outboxScheduleCall) []notify.SendRequest {
	t.Helper()
	var requests []notify.SendRequest
	for _, call := range calls {
		if call.kind != notifySendTimerKind {
			continue
		}
		var request notify.SendRequest
		if err := json.Unmarshal(call.payload, &request); err != nil {
			t.Fatalf("反序列化 notify_send payload 失败: %v", err)
		}
		requests = append(requests, request)
	}
	return requests
}

func TestNotifyOutbox_NotifyGroup_ResolvesConcreteTeamTargets(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{}
	channels := &fakeTeamChannelRepo{byTeam: map[string][]ops.ChannelTarget{
		"team-1": {
			{Kind: "lark", Target: "chat-a"},
			{Kind: "email", Target: "team@example.com"},
			{Kind: "lark", Target: "chat-a"}, // 精确重复项必须去重
			{Kind: "lark", Target: "chat-b"},
		},
	}}
	engine := &fakeOutboxEngine{}
	chain := &fakeOutboxChain{}
	outbox := newTestNotifyOutbox(scheduler, channels, timeline, now)
	outbox.engine = engine
	outbox.chain = chain

	alerts := []domain.Alert{
		{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityCritical, StartsAt: now.Add(-time.Minute)},
		{ID: "a2", TeamID: "team-1", Title: "two", Severity: domain.SeverityCritical, StartsAt: now},
	}
	if err := outbox.NotifyGroup(context.Background(), nil, "group-1", alerts); err != nil {
		t.Fatalf("NotifyGroup 返回意外错误: %v", err)
	}

	requests := sentRequests(t, scheduler.scheduled)
	if len(requests) != 2 {
		t.Fatalf("notify_send 数量 = %d, want 2", len(requests))
	}
	gotTargets := []string{requests[0].Target, requests[1].Target}
	sort.Strings(gotTargets)
	if gotTargets[0] != "chat-a" || gotTargets[1] != "chat-b" {
		t.Fatalf("targets = %v, want [chat-a chat-b]", gotTargets)
	}
	for _, request := range requests {
		if request.ChannelKind != "lark" || request.Target == "team-1" {
			t.Fatalf("团队 UUID 不得作为通知目标: %+v", request)
		}
	}
	if len(engine.alerts) != 2 || len(chain.alerts) != 2 || len(scheduler.scheduledIfEmpty) != 2 {
		t.Fatalf("缺少后续编排: engine=%v chain=%v repeat=%d", engine.alerts, chain.alerts, len(scheduler.scheduledIfEmpty))
	}
	if len(timeline.entries) != 0 {
		t.Fatalf("渠道存在时不应写跳过审计: %+v", timeline.entries)
	}
}

func TestNotifyOutbox_NotifyRepeat_NoTeamChannelAuditsWithoutSend(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 1, 0, 0, time.UTC)
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{}
	outbox := newTestNotifyOutbox(scheduler, &fakeTeamChannelRepo{byTeam: map[string][]ops.ChannelTarget{}}, timeline, now)
	alert := domain.Alert{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityWarning, StartsAt: now}

	if err := outbox.NotifyRepeat(context.Background(), nil, alert); err != nil {
		t.Fatalf("NotifyRepeat 返回意外错误: %v", err)
	}
	if got := len(sentRequests(t, scheduler.scheduled)); got != 0 {
		t.Fatalf("缺渠道时 notify_send 数量 = %d, want 0", got)
	}
	if len(timeline.entries) != 1 {
		t.Fatalf("跳过审计数量 = %d, want 1", len(timeline.entries))
	}
	entry := timeline.entries[0]
	if entry.AlertID != "a1" || entry.Action != domain.TimelineActionNotifySkipped || entry.Detail["reason"] != "no_team_channel" || entry.Detail["attempts"] != 0 {
		t.Fatalf("跳过审计内容不符: %+v", entry)
	}
}

func TestNotifyOutbox_NotifyGroup_NoTeamChannelAuditsAllAndContinuesOrchestration(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 1, 30, 0, time.UTC)
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{}
	engine := &fakeOutboxEngine{}
	chain := &fakeOutboxChain{}
	outbox := newTestNotifyOutbox(scheduler, &fakeTeamChannelRepo{byTeam: map[string][]ops.ChannelTarget{}}, timeline, now)
	outbox.engine = engine
	outbox.chain = chain
	alerts := []domain.Alert{
		{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityCritical, StartsAt: now.Add(-time.Minute)},
		{ID: "a2", TeamID: "team-1", Title: "two", Severity: domain.SeverityCritical, StartsAt: now},
	}

	if err := outbox.NotifyGroup(context.Background(), nil, "group-1", alerts); err != nil {
		t.Fatalf("缺渠道不应阻塞组通知后续编排: %v", err)
	}
	if got := len(sentRequests(t, scheduler.scheduled)); got != 0 {
		t.Fatalf("缺渠道时 notify_send 数量 = %d, want 0", got)
	}
	if len(timeline.entries) != 2 || timeline.entries[0].Action != domain.TimelineActionNotifySkipped || timeline.entries[1].Action != domain.TimelineActionNotifySkipped {
		t.Fatalf("组内告警应各写一条跳过审计: %+v", timeline.entries)
	}
	if len(engine.alerts) != 2 || len(chain.alerts) != 2 || len(scheduler.scheduledIfEmpty) != 2 {
		t.Fatalf("缺渠道仍应继续渠道升级、人员升级与 repeat 编排: engine=%v chain=%v repeat=%d", engine.alerts, chain.alerts, len(scheduler.scheduledIfEmpty))
	}
}

func TestNotifyOutbox_DispatchStep_ResolvesAvailableKindAndAuditsMissingKind(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 2, 0, 0, time.UTC)
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{}
	channels := &fakeTeamChannelRepo{byTeam: map[string][]ops.ChannelTarget{
		"team-1": {{Kind: "lark", Target: "chat-a"}},
	}}
	outbox := newTestNotifyOutbox(scheduler, channels, timeline, now)
	alert := domain.Alert{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityWarning, StartsAt: now}

	if err := outbox.DispatchStep(context.Background(), nil, alert, 2, []string{"email", "lark", "lark"}); err != nil {
		t.Fatalf("DispatchStep 返回意外错误: %v", err)
	}
	requests := sentRequests(t, scheduler.scheduled)
	if len(requests) != 1 || requests[0].ChannelKind != "lark" || requests[0].Target != "chat-a" || requests[0].Step == nil || *requests[0].Step != 2 {
		t.Fatalf("已解析发送意图不符: %+v", requests)
	}
	if len(timeline.entries) != 1 || timeline.entries[0].Detail["channel"] != "email" || timeline.entries[0].Detail["notification_kind"] != teamNotificationChannelEscalation {
		t.Fatalf("缺失 email 的审计不符: %+v", timeline.entries)
	}
}

func TestNotifyOutbox_TeamChannelRepositoryErrorStopsScheduling(t *testing.T) {
	wantErr := errors.New("database unavailable")
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{}
	outbox := newTestNotifyOutbox(scheduler, &fakeTeamChannelRepo{err: wantErr}, timeline, time.Now())
	alert := domain.Alert{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityWarning, StartsAt: time.Now()}

	err := outbox.NotifyRepeat(context.Background(), nil, alert)
	if !errors.Is(err, wantErr) {
		t.Fatalf("NotifyRepeat error = %v, want 包含 %v", err, wantErr)
	}
	if len(scheduler.scheduled) != 0 || len(timeline.entries) != 0 {
		t.Fatalf("查询错误后不得产生发送或跳过审计: scheduled=%d timeline=%d", len(scheduler.scheduled), len(timeline.entries))
	}
}

func TestNotifyOutbox_SkipAuditErrorPropagates(t *testing.T) {
	wantErr := errors.New("timeline unavailable")
	scheduler := &fakeOutboxScheduler{}
	timeline := &fakeTeamTimeline{err: wantErr}
	outbox := newTestNotifyOutbox(scheduler, &fakeTeamChannelRepo{byTeam: map[string][]ops.ChannelTarget{}}, timeline, time.Now())
	alert := domain.Alert{ID: "a1", TeamID: "team-1", Title: "one", Severity: domain.SeverityWarning, StartsAt: time.Now()}

	err := outbox.NotifyRepeat(context.Background(), nil, alert)
	if !errors.Is(err, wantErr) {
		t.Fatalf("NotifyRepeat error = %v, want 包含 %v", err, wantErr)
	}
	if len(scheduler.scheduled) != 0 {
		t.Fatalf("跳过审计失败时不得注册 notify_send: %d", len(scheduler.scheduled))
	}
}

var _ oncall.Notifier = (*notifyOutbox)(nil)
