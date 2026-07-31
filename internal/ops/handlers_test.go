package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

// settableClock 是 Clock 的可控实现，与 oncall 包 settableClock 同一模式，测试用它
// 精确推进"当前时间"而不依赖 wall-clock。
type settableClock struct{ t time.Time }

func (c *settableClock) Now() time.Time { return c.t }

// ---- fake SettingsRepo/TeamRepo/DailyStatsRepo/StaleAlertRepo/OncallResolver ----

type fakeSettingsRepo struct {
	byTeam map[string]Settings
	err    error // 非 nil 时覆盖返回该 err（用于模拟"未找到"以外的失败）
}

func (f *fakeSettingsRepo) GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*Settings, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.byTeam[teamID]
	if !ok {
		return nil, ErrSettingsNotFound
	}
	return &s, nil
}

type fakeTeamRepo struct{ name string }

func (f *fakeTeamRepo) TeamName(ctx context.Context, tx pgx.Tx, teamID string) (string, error) {
	return f.name, nil
}

type dailyStatsCall struct{ dayStart, dayEnd time.Time }
type longOpenCall struct{ olderThan time.Time }

type fakeDailyStatsRepo struct {
	stats       DailyStats
	statsErr    error
	longOpen    []domain.Alert
	longOpenErr error

	statsCalls    []dailyStatsCall
	longOpenCalls []longOpenCall
}

func (f *fakeDailyStatsRepo) DailyStats(ctx context.Context, tx pgx.Tx, teamID string, dayStart, dayEnd time.Time) (DailyStats, error) {
	f.statsCalls = append(f.statsCalls, dailyStatsCall{dayStart, dayEnd})
	return f.stats, f.statsErr
}

func (f *fakeDailyStatsRepo) ListOpenLongerThan(ctx context.Context, tx pgx.Tx, teamID string, olderThan time.Time) ([]domain.Alert, error) {
	f.longOpenCalls = append(f.longOpenCalls, longOpenCall{olderThan})
	return f.longOpen, f.longOpenErr
}

type fakeStaleAlertRepo struct {
	alerts []domain.Alert
	err    error
}

func (f *fakeStaleAlertRepo) ListUnackedCritical(ctx context.Context, tx pgx.Tx, teamID string) ([]domain.Alert, error) {
	return f.alerts, f.err
}

type fakeOncallResolver struct {
	names []string
	err   error
}

func (f *fakeOncallResolver) ResolveOnCallNames(ctx context.Context, tx pgx.Tx, teamID string, at time.Time) ([]string, error) {
	return f.names, f.err
}

// ---- fake OpsNotifier/TimerScheduler ----

type opsSendCall struct {
	teamID, kind string
	round        int64
	ch           ChannelTarget
	msg          notify.Message
}

type fakeOpsNotifier struct {
	calls []opsSendCall
	err   error
}

func (f *fakeOpsNotifier) ScheduleOpsSend(ctx context.Context, tx pgx.Tx, teamID, kind string, round int64, ch ChannelTarget, msg notify.Message) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, opsSendCall{teamID, kind, round, ch, msg})
	return nil
}

type opsScheduleCall struct {
	kind, refID string
	fireAt      time.Time
	payload     []byte
}

type fakeOpsScheduler struct {
	calls []opsScheduleCall
	err   error
}

func (f *fakeOpsScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, opsScheduleCall{kind, refID, fireAt, payload})
	return nil
}

// newTestHandlers 组装一套 fake 依赖的 Handlers，clock 固定为 now，供测试精确控制。
func newTestHandlers(settings *fakeSettingsRepo, teams *fakeTeamRepo, daily *fakeDailyStatsRepo,
	stale *fakeStaleAlertRepo, oncall *fakeOncallResolver, notifier *fakeOpsNotifier, sched *fakeOpsScheduler, now time.Time) *Handlers {
	h := NewHandlers(settings, teams, daily, stale, oncall, notifier, sched)
	h.clock = &settableClock{t: now}
	return h
}

const testTeamID = "team-ops-1"

func twoChannelSettings() Settings {
	return Settings{
		TeamID:                    testTeamID,
		Timezone:                  "Asia/Shanghai",
		DailyReportEnabled:        true,
		DailyReportTime:           "09:00",
		StaleSweepEnabled:         true,
		StaleSweepIntervalSeconds: 1800,
		OncallBroadcastEnabled:    true,
		OncallBroadcastTime:       "09:30",
		Channels: []ChannelTarget{
			{Kind: "webhook", Target: "https://example.com/a"},
			{Kind: "lark", Target: "ou_xxx"},
		},
	}
}

// ===================== 日报 handler =====================

// TestHandleDailyReport_NormalPath 覆盖正常路径：逐渠道调度发送 + 同事务自续期，
// 且统计查询用的时间窗是 fireAt 所在自然日的"昨天"（团队时区）。
func TestHandleDailyReport_NormalPath(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC) // UTC 01:00 = 上海 09:00
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	teams := &fakeTeamRepo{name: "SRE"}
	daily := &fakeDailyStatsRepo{stats: DailyStats{NewCount: 3, BySeverity: map[string]int{}}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, teams, daily, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleDailyReport() error = %v", err)
	}

	if len(notifier.calls) != 2 {
		t.Fatalf("notifier.calls = %d, want 2（两个渠道各一次）", len(notifier.calls))
	}
	for _, c := range notifier.calls {
		if c.teamID != testTeamID || c.kind != TimerKindDailyReport {
			t.Fatalf("发送调用参数不正确: %+v", c)
		}
	}
	if len(sched.calls) != 1 || sched.calls[0].kind != TimerKindDailyReport || sched.calls[0].refID != testTeamID {
		t.Fatalf("未正确自续期: %+v", sched.calls)
	}

	loc, _ := time.LoadLocation("Asia/Shanghai")
	wantDayEnd := time.Date(2026, 7, 20, 0, 0, 0, 0, loc)
	wantDayStart := wantDayEnd.AddDate(0, 0, -1)
	if len(daily.statsCalls) != 1 || !daily.statsCalls[0].dayStart.Equal(wantDayStart) || !daily.statsCalls[0].dayEnd.Equal(wantDayEnd) {
		t.Fatalf("DailyStats 时间窗不正确: %+v, want [%v, %v)", daily.statsCalls, wantDayStart, wantDayEnd)
	}
}

// TestHandleDailyReport_Disabled_NoRenewal 覆盖禁用：不发送、不续期，链路自然断掉。
func TestHandleDailyReport_Disabled_NoRenewal(t *testing.T) {
	s := twoChannelSettings()
	s.DailyReportEnabled = false
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: s}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, notifier, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleDailyReport() error = %v", err)
	}
	if len(notifier.calls) != 0 || len(sched.calls) != 0 {
		t.Fatalf("禁用后不应发送/续期: notifier=%d sched=%d", len(notifier.calls), len(sched.calls))
	}
}

// TestHandleDailyReport_MissingSettings_NoRenewal 覆盖团队从未保存过配置：同禁用，
// 静默返回不续期。
func TestHandleDailyReport_MissingSettings_NoRenewal(t *testing.T) {
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, notifier, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleDailyReport() error = %v", err)
	}
	if len(notifier.calls) != 0 || len(sched.calls) != 0 {
		t.Fatalf("未配置时不应发送/续期: notifier=%d sched=%d", len(notifier.calls), len(sched.calls))
	}
}

// TestHandleDailyReport_StatsError_PropagatesAndDoesNotRenew 覆盖统计查询异常：
// 整体报错（回滚重投语义），且不应先斩后奏地调度发送/续期。
func TestHandleDailyReport_StatsError_PropagatesAndDoesNotRenew(t *testing.T) {
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	daily := &fakeDailyStatsRepo{statsErr: errors.New("db 挂了")}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, daily, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, notifier, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload)
	if err == nil {
		t.Fatalf("HandleDailyReport() 应报错但未报错")
	}
	if len(notifier.calls) != 0 || len(sched.calls) != 0 {
		t.Fatalf("统计失败后不应发送/续期: notifier=%d sched=%d", len(notifier.calls), len(sched.calls))
	}
}

// TestHandleDailyReport_Idempotent_RoundStableAcrossRetry 覆盖幂等核心：同一条
// payload（同一 fireAt）重投两次，两次算出的 Round 必须相同——即使中途 clock 已经
// 走到了别的时刻（模拟"发送时崩溃、稍后重投"）。
func TestHandleDailyReport_Idempotent_RoundStableAcrossRetry(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("第一次 HandleDailyReport() error = %v", err)
	}
	firstRound := notifier.calls[0].round

	// 模拟重投：时钟已经推进（比如崩溃后过了几分钟才重投），但 payload 里的 fireAt 不变。
	h.clock = &settableClock{t: fireAt.Add(5 * time.Minute)}
	if err := h.HandleDailyReport(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("第二次 HandleDailyReport() error = %v", err)
	}
	secondRound := notifier.calls[len(notifier.calls)-1].round

	if firstRound != secondRound {
		t.Fatalf("Round 在重投前后不一致: %d != %d", firstRound, secondRound)
	}
}

// ===================== 悬置扫描 handler =====================

// TestHandleStaleSweep_EmptyAlerts_SilentButRenews 覆盖：无未 Ack critical 时静默
// 不发送，但仍自续期。
func TestHandleStaleSweep_EmptyAlerts_SilentButRenews(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{alerts: nil}, &fakeOncallResolver{}, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleStaleSweep(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleStaleSweep() error = %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("无未 Ack critical 时不应发送, got %d", len(notifier.calls))
	}
	if len(sched.calls) != 1 {
		t.Fatalf("仍应自续期, got %d", len(sched.calls))
	}
}

// TestHandleStaleSweep_WithAlerts_SendsToAllChannels 覆盖有未 Ack critical 时逐渠道发送。
func TestHandleStaleSweep_WithAlerts_SendsToAllChannels(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	stale := &fakeStaleAlertRepo{alerts: []domain.Alert{{ID: "a1", Title: "CPU 过高", StartsAt: fireAt.Add(-time.Hour)}}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, &fakeDailyStatsRepo{}, stale, &fakeOncallResolver{}, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleStaleSweep(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleStaleSweep() error = %v", err)
	}
	if len(notifier.calls) != 2 {
		t.Fatalf("notifier.calls = %d, want 2", len(notifier.calls))
	}
	if len(sched.calls) != 1 {
		t.Fatalf("未正确自续期, got %d", len(sched.calls))
	}
}

// TestHandleStaleSweep_Disabled_NoRenewal 覆盖禁用：不查询、不续期。
func TestHandleStaleSweep_Disabled_NoRenewal(t *testing.T) {
	s := twoChannelSettings()
	s.StaleSweepEnabled = false
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: s}}
	stale := &fakeStaleAlertRepo{alerts: []domain.Alert{{ID: "a1"}}}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, stale, &fakeOncallResolver{}, &fakeOpsNotifier{}, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleStaleSweep(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleStaleSweep() error = %v", err)
	}
	if len(sched.calls) != 0 {
		t.Fatalf("禁用后不应续期, got %d", len(sched.calls))
	}
}

// TestHandleStaleSweep_ListError_Propagates 覆盖查询异常：整体报错，不静默吞掉。
func TestHandleStaleSweep_ListError_Propagates(t *testing.T) {
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	stale := &fakeStaleAlertRepo{err: errors.New("查询失败")}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, stale, &fakeOncallResolver{}, &fakeOpsNotifier{}, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleStaleSweep(context.Background(), nil, testTeamID, payload); err == nil {
		t.Fatalf("HandleStaleSweep() 应报错但未报错")
	}
	if len(sched.calls) != 0 {
		t.Fatalf("查询失败后不应续期, got %d", len(sched.calls))
	}
}

// ===================== 值班播报 handler =====================

// TestHandleOncallBroadcast_NoSchedule_SendsNoOncallMessage 覆盖当天无排班：
// BuildOncallBroadcast 已处理为"今日无值班安排"文案，仍照常发送（不是静默）。
func TestHandleOncallBroadcast_NoSchedule_SendsNoOncallMessage(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 30, 0, 0, time.UTC)
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, &fakeOncallResolver{names: nil}, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleOncallBroadcast(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleOncallBroadcast() error = %v", err)
	}
	if len(notifier.calls) != 2 {
		t.Fatalf("notifier.calls = %d, want 2（无排班也照常发送）", len(notifier.calls))
	}
	if len(sched.calls) != 1 {
		t.Fatalf("未正确自续期, got %d", len(sched.calls))
	}
}

// TestHandleOncallBroadcast_WithNames_ResolvesAndSends 覆盖有排班时正常发送。
func TestHandleOncallBroadcast_WithNames_ResolvesAndSends(t *testing.T) {
	fireAt := time.Date(2026, 7, 20, 1, 30, 0, 0, time.UTC)
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	oncall := &fakeOncallResolver{names: []string{"alice", "bob"}}
	notifier := &fakeOpsNotifier{}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{name: "SRE"}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, oncall, notifier, sched, fireAt)

	payload, _ := EncodePayload(fireAt)
	if err := h.HandleOncallBroadcast(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleOncallBroadcast() error = %v", err)
	}
	if len(notifier.calls) != 2 {
		t.Fatalf("notifier.calls = %d, want 2", len(notifier.calls))
	}
}

// TestHandleOncallBroadcast_Disabled_NoRenewal 覆盖禁用：不解析值班、不续期。
func TestHandleOncallBroadcast_Disabled_NoRenewal(t *testing.T) {
	s := twoChannelSettings()
	s.OncallBroadcastEnabled = false
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: s}}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, &fakeOncallResolver{}, &fakeOpsNotifier{}, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleOncallBroadcast(context.Background(), nil, testTeamID, payload); err != nil {
		t.Fatalf("HandleOncallBroadcast() error = %v", err)
	}
	if len(sched.calls) != 0 {
		t.Fatalf("禁用后不应续期, got %d", len(sched.calls))
	}
}

// TestHandleOncallBroadcast_ResolveError_Propagates 覆盖值班解析异常：整体报错。
func TestHandleOncallBroadcast_ResolveError_Propagates(t *testing.T) {
	settings := &fakeSettingsRepo{byTeam: map[string]Settings{testTeamID: twoChannelSettings()}}
	oncall := &fakeOncallResolver{err: errors.New("解析失败")}
	sched := &fakeOpsScheduler{}
	h := newTestHandlers(settings, &fakeTeamRepo{}, &fakeDailyStatsRepo{}, &fakeStaleAlertRepo{}, oncall, &fakeOpsNotifier{}, sched, time.Now())

	payload, _ := EncodePayload(time.Now())
	if err := h.HandleOncallBroadcast(context.Background(), nil, testTeamID, payload); err == nil {
		t.Fatalf("HandleOncallBroadcast() 应报错但未报错")
	}
	if len(sched.calls) != 0 {
		t.Fatalf("解析失败后不应续期, got %d", len(sched.calls))
	}
}

// ===================== payload 相关 =====================

// TestParseOpsPayload_InvalidJSON 覆盖 payload 反序列化失败的错误路径。
func TestParseOpsPayload_InvalidJSON(t *testing.T) {
	if _, err := parseOpsPayload([]byte("not json")); err == nil {
		t.Fatalf("parseOpsPayload() 应报错但未报错")
	}
}

// TestParseOpsPayload_MissingFireAt 覆盖 payload 缺少 fire_at 字段的情形。
func TestParseOpsPayload_MissingFireAt(t *testing.T) {
	if _, err := parseOpsPayload([]byte(`{}`)); err == nil {
		t.Fatalf("parseOpsPayload() 应报错但未报错")
	}
}
