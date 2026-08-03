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

// fakeSilenceListRepo 是 SilenceListRepo 的可编程测试替身：记录 List 被调用的次数，
// 供缓存命中/过期的测试断言。
type fakeSilenceListRepo struct {
	silences []store.Silence
	err      error
	calls    int
}

func (f *fakeSilenceListRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Silence, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.silences, nil
}

// silenceUpdateCall 记录 fakeSilenceAlertRepo.UpdateSilencedBy 收到的参数。
type silenceUpdateCall struct {
	id         string
	silencedBy []string
}

// fakeSilenceAlertRepo 是 SilenceAlertRepo 的可编程测试替身。
type fakeSilenceAlertRepo struct {
	calls []silenceUpdateCall
	err   error
}

func (f *fakeSilenceAlertRepo) UpdateSilencedBy(ctx context.Context, tx pgx.Tx, id string, silencedBy []string) error {
	f.calls = append(f.calls, silenceUpdateCall{id: id, silencedBy: silencedBy})
	return f.err
}

// newSilenceStageForTest 复用 stage_route_test.go 里已经定义的 mutableClock
// （routeClock/silenceClock 都只有同一个 Now() time.Time 方法，结构化满足）。
func newSilenceStageForTest(silences SilenceListRepo, alerts SilenceAlertRepo, clock silenceClock) *SilenceStage {
	s := NewSilenceStage(silences, alerts)
	s.clock = clock
	return s
}

func mkMatcher(name, value string) domain.MatcherSet {
	return domain.MatcherSet{{Name: name, Op: domain.MatchEqual, Value: value}}
}

func TestSilenceStage_Name(t *testing.T) {
	s := NewSilenceStage(&fakeSilenceListRepo{}, &fakeSilenceAlertRepo{})
	if s.Name() != "silence" {
		t.Fatalf("Name() = %q, want %q", s.Name(), "silence")
	}
}

func TestSilenceStage_SkipsZeroValueAlert(t *testing.T) {
	listRepo := &fakeSilenceListRepo{}
	alertRepo := &fakeSilenceAlertRepo{}
	s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{}}
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if listRepo.calls != 0 || len(alertRepo.calls) != 0 {
		t.Fatalf("expected no repo calls for zero-value alert, got list=%d update=%d", listRepo.calls, len(alertRepo.calls))
	}
}

func TestSilenceStage_SkipsResolvedAlert(t *testing.T) {
	listRepo := &fakeSilenceListRepo{}
	alertRepo := &fakeSilenceAlertRepo{}
	s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "a1", Status: domain.StatusResolved, Labels: map[string]string{"env": "prod"}}}
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if listRepo.calls != 0 || len(alertRepo.calls) != 0 {
		t.Fatalf("expected no repo calls for resolved alert, got list=%d update=%d", listRepo.calls, len(alertRepo.calls))
	}
}

// TestSilenceStage_WindowBoundaries 覆盖生效窗口的起含终不含边界，口径需与
// store.SilenceRepo.ListActive 一致（starts_at <= now < ends_at）。
func TestSilenceStage_WindowBoundaries(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	sil := store.Silence{ID: "sil-1", Matchers: mkMatcher("env", "prod"), StartsAt: base, EndsAt: base.Add(10 * time.Second)}

	tests := []struct {
		name    string
		now     time.Time
		wantHit bool
	}{
		{"start_inclusive", base, true},
		{"mid_window", base.Add(5 * time.Second), true},
		{"end_exclusive", base.Add(10 * time.Second), false},
		{"before_window", base.Add(-time.Second), false},
		{"after_window", base.Add(11 * time.Second), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listRepo := &fakeSilenceListRepo{silences: []store.Silence{sil}}
			alertRepo := &fakeSilenceAlertRepo{}
			s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: tt.now})

			pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1", Status: domain.StatusFiring, Labels: map[string]string{"env": "prod"}}}
			if err := s.Process(context.Background(), pctx); err != nil {
				t.Fatalf("Process() error = %v", err)
			}

			gotHit := len(pctx.Alert.SilencedBy) == 1 && pctx.Alert.SilencedBy[0] == "sil-1"
			if gotHit != tt.wantHit {
				t.Fatalf("hit = %v, want %v (SilencedBy=%v)", gotHit, tt.wantHit, pctx.Alert.SilencedBy)
			}
			if len(alertRepo.calls) != 1 {
				t.Fatalf("expected exactly 1 UpdateSilencedBy call, got %d", len(alertRepo.calls))
			}
		})
	}
}

// TestSilenceStage_MultipleSilencesAllRecorded 验证多条静默同时命中时全部记录、
// 按 ID 稳定排序，且不命中的静默不会出现在结果里。
func TestSilenceStage_MultipleSilencesAllRecorded(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	silences := []store.Silence{
		{ID: "sil-b", Matchers: mkMatcher("env", "prod"), StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)},
		{ID: "sil-a", Matchers: mkMatcher("service", "checkout"), StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)},
		{ID: "sil-c", Matchers: mkMatcher("env", "staging"), StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)}, // 不命中
	}
	listRepo := &fakeSilenceListRepo{silences: silences}
	alertRepo := &fakeSilenceAlertRepo{}
	s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: now})

	pctx := &PipelineContext{Alert: domain.Alert{
		ID:     "alert-1",
		Status: domain.StatusFiring,
		Labels: map[string]string{"env": "prod", "service": "checkout"},
	}}
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	want := []string{"sil-a", "sil-b"}
	if !equalStringSlices(pctx.Alert.SilencedBy, want) {
		t.Fatalf("SilencedBy = %v, want %v", pctx.Alert.SilencedBy, want)
	}
	if len(alertRepo.calls) != 1 || !equalStringSlices(alertRepo.calls[0].silencedBy, want) {
		t.Fatalf("UpdateSilencedBy call = %+v, want ids %v", alertRepo.calls, want)
	}
}

// TestSilenceStage_NoMatchClearsSilencedBy 验证上一轮遗留的 SilencedBy 在本轮
// 无命中时会被整体覆盖清空（而不是保留旧值），呼应「静默过期/删除后恢复通知」。
func TestSilenceStage_NoMatchClearsSilencedBy(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	listRepo := &fakeSilenceListRepo{silences: []store.Silence{
		{ID: "sil-1", Matchers: mkMatcher("env", "staging"), StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)},
	}}
	alertRepo := &fakeSilenceAlertRepo{}
	s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: now})

	pctx := &PipelineContext{Alert: domain.Alert{
		ID:         "alert-1",
		Status:     domain.StatusFiring,
		Labels:     map[string]string{"env": "prod"},
		SilencedBy: []string{"stale-id"}, // 上一轮遗留，本轮应被覆盖清空
	}}
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(pctx.Alert.SilencedBy) != 0 {
		t.Fatalf("SilencedBy = %v, want empty", pctx.Alert.SilencedBy)
	}
	if len(alertRepo.calls) != 1 || len(alertRepo.calls[0].silencedBy) != 0 {
		t.Fatalf("UpdateSilencedBy call = %+v, want empty silencedBy", alertRepo.calls)
	}
}

// TestSilenceStage_ExpirationTakesEffectImmediatelyEvenWithinCacheTTL 是本 stage
// 最关键的测试：验证 10s 规则缓存不会让「静默过期后恢复通知」被拖延到缓存刷新那一刻——
// 生效窗口判断必须用当前时间现算，而不是缓存住某次求值的结果。
func TestSilenceStage_ExpirationTakesEffectImmediatelyEvenWithinCacheTTL(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	listRepo := &fakeSilenceListRepo{silences: []store.Silence{
		{ID: "sil-1", Matchers: mkMatcher("env", "prod"), StartsAt: base, EndsAt: base.Add(5 * time.Second)},
	}}
	alertRepo := &fakeSilenceAlertRepo{}
	clock := &mutableClock{t: base}
	s := newSilenceStageForTest(listRepo, alertRepo, clock)

	firstPctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1", Status: domain.StatusFiring, Labels: map[string]string{"env": "prod"}}}
	if err := s.Process(context.Background(), firstPctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(firstPctx.Alert.SilencedBy) != 1 {
		t.Fatalf("expected hit before expiry, got %v", firstPctx.Alert.SilencedBy)
	}

	// 静默已过期（5s 窗口 vs 推进 6s），但仍在 10s 的规则缓存 TTL 内。
	clock.t = base.Add(6 * time.Second)
	secondPctx := &PipelineContext{Alert: domain.Alert{ID: "alert-2", Status: domain.StatusFiring, Labels: map[string]string{"env": "prod"}}}
	if err := s.Process(context.Background(), secondPctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(secondPctx.Alert.SilencedBy) != 0 {
		t.Fatalf("expected no hit after expiry (even within cache TTL), got %v", secondPctx.Alert.SilencedBy)
	}
	if listRepo.calls != 1 {
		t.Fatalf("expected cache to avoid a second List() call within TTL, got %d calls", listRepo.calls)
	}
}

func TestSilenceStage_CacheRefreshesAfterTTL(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	listRepo := &fakeSilenceListRepo{silences: []store.Silence{
		{ID: "sil-1", Matchers: mkMatcher("env", "prod"), StartsAt: base.Add(-time.Hour), EndsAt: base.Add(time.Hour)},
	}}
	alertRepo := &fakeSilenceAlertRepo{}
	clock := &mutableClock{t: base}
	s := newSilenceStageForTest(listRepo, alertRepo, clock)

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1", Status: domain.StatusFiring, Labels: map[string]string{"env": "prod"}}}
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	clock.t = base.Add(11 * time.Second) // 超过 10s 缓存 TTL
	if err := s.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if listRepo.calls != 2 {
		t.Fatalf("expected cache refresh to trigger a second List() call, got %d", listRepo.calls)
	}
}

func TestSilenceStage_InvalidateCache_RequeriesImmediately(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	listRepo := &fakeSilenceListRepo{}
	stage := newSilenceStageForTest(listRepo, &fakeSilenceAlertRepo{}, &mutableClock{t: base})
	pctx := &PipelineContext{Alert: domain.Alert{ID: "a1", Status: domain.StatusFiring}}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatal(err)
	}
	stage.InvalidateCache()
	pctx.Alert.ID = "a2"
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatal(err)
	}
	if listRepo.calls != 2 {
		t.Fatalf("主动失效后应立即重查，calls=%d", listRepo.calls)
	}
}

func TestSilenceStage_ListErrorPropagates(t *testing.T) {
	listRepo := &fakeSilenceListRepo{err: errors.New("boom")}
	alertRepo := &fakeSilenceAlertRepo{}
	s := newSilenceStageForTest(listRepo, alertRepo, &mutableClock{t: time.Now()})

	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1", Status: domain.StatusFiring}}
	if err := s.Process(context.Background(), pctx); err == nil {
		t.Fatal("expected error to propagate")
	}
	if len(alertRepo.calls) != 0 {
		t.Fatalf("expected no UpdateSilencedBy call when List fails, got %d", len(alertRepo.calls))
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
