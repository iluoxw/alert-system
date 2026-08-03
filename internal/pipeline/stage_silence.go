package pipeline

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// silenceCacheTTL 是静默规则缓存的存活时间，与 routeCacheTTL 同一取值：配置变更
// （新建/删除静默）后至多 10s 内对新事件生效。生效窗口本身不受这个 TTL 影响——见
// currentSilences 与 isSilenceActive 的说明，窗口判断永远用求值时刻的当前时间现算，
// 不会因为缓存而让「静默过期后恢复通知」出现额外延迟。
const silenceCacheTTL = 10 * time.Second

// SilenceListRepo 是 SilenceStage 依赖的静默仓储子集，由 store.SilenceRepo 实现。
// 这里刻意缓存全量规则列表（List）而不是某一时刻的生效子集（ListActive）：后者会把
// 「生效窗口」这个随时间变化的判断结果也缓存住，导致静默过期后要等到缓存刷新才恢复
// 通知，与 R10.3 的「立即恢复」冲突。
type SilenceListRepo interface {
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Silence, error)
}

// SilenceAlertRepo 是 SilenceStage 依赖的告警仓储子集，用于把命中结果持久化回
// alerts 表，由 store.AlertRepo（alerts_silence.go 的纯增量方法）实现。
type SilenceAlertRepo interface {
	UpdateSilencedBy(ctx context.Context, tx pgx.Tx, id string, silencedBy []string) error
}

// silenceClock 抽象"当前时间"，供测试注入 fake clock 精确控制生效窗口边界，
// 与 routeClock/dedupClock/groupClock 同一模式。
type silenceClock interface {
	Now() time.Time
}

type realSilenceClock struct{}

func (realSilenceClock) Now() time.Time { return time.Now() }

// silenceCache 是进程内缓存的一份静默规则快照及其过期时间。
type silenceCache struct {
	silences  []store.Silence
	expiresAt time.Time
}

// SilenceStage 是管道的静默 stage（R10）：用 MatcherSet 对生效窗口内的静默规则求值，
// 命中则把全部命中的静默 ID 写入 alerts.silenced_by（告警本身照常落库与展示，只是
// 抑制通知——是否抑制由 notify 侧读取 SilencedBy 决定，本 stage 不做任何发送/抑制
// 判断）。规则列表在进程内缓存 ≤10s，生效窗口判断始终用当前时间现算（见
// silenceCacheTTL 的说明）。
type SilenceStage struct {
	silences SilenceListRepo
	alerts   SilenceAlertRepo
	clock    silenceClock

	mu    sync.RWMutex
	cache *silenceCache
}

// NewSilenceStage 构造 SilenceStage。
func NewSilenceStage(silences SilenceListRepo, alerts SilenceAlertRepo) *SilenceStage {
	return &SilenceStage{
		silences: silences,
		alerts:   alerts,
		clock:    realSilenceClock{},
	}
}

// Name 返回 "silence"。
func (s *SilenceStage) Name() string { return "silence" }

// InvalidateCache 让新建、更新或删除的静默在下一条事件上立即重新求值。
func (s *SilenceStage) InvalidateCache() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// Process 实现 Stage 接口。
func (s *SilenceStage) Process(ctx context.Context, pctx *PipelineContext) error {
	// 迟到的 resolved 事件在 dedup stage 里找不到现存告警时，pctx.Alert 仍是零值
	// （ID==""）：没有告警可求值，直接跳过。
	if pctx.Alert.ID == "" {
		return nil
	}
	// 已 resolved 的告警不再有待发送/待抑制的通知，求值静默没有意义，直接跳过——
	// 不清空、也不更新它 resolved 前最后一次求值出的 silenced_by（保留历史命中记录
	// 供事后查看），与 R10 的目的（抑制"进行中"告警的通知）一致。
	if pctx.Alert.Status == domain.StatusResolved {
		return nil
	}

	silences, err := s.currentSilences(ctx, pctx.Tx)
	if err != nil {
		return fmt.Errorf("silence: 加载静默规则失败: %w", err)
	}

	now := s.clock.Now()
	labels := mergedMatchLabels(pctx.Alert)

	var hitIDs []string
	for _, sil := range silences {
		if !isSilenceActive(sil, now) {
			continue
		}
		if sil.Matchers.Matches(labels) {
			hitIDs = append(hitIDs, sil.ID)
		}
	}
	// 稳定排序：List() 里的规则顺序（按 starts_at 倒序）与命中顺序无关，排序后
	// 保证同一组命中结果每次求值都产出相同的 SilencedBy 切片，便于测试断言与展示。
	sort.Strings(hitIDs)

	pctx.Alert.SilencedBy = hitIDs
	if err := s.alerts.UpdateSilencedBy(ctx, pctx.Tx, pctx.Alert.ID, hitIDs); err != nil {
		return fmt.Errorf("silence: 持久化静默命中失败: %w", err)
	}
	return nil
}

// isSilenceActive 判断 sil 在 now 时刻是否处于生效窗口内：起含终不含
// （starts_at <= now < ends_at），与 store.SilenceRepo.ListActive 的口径完全一致。
func isSilenceActive(sil store.Silence, now time.Time) bool {
	return !sil.StartsAt.After(now) && sil.EndsAt.After(now)
}

// currentSilences 返回当前缓存的静默规则全集，过期（或从未加载）则用 tx 重查并
// 刷新缓存；与 RouteStage.currentRoutes 同一双重检查锁定模式。
func (s *SilenceStage) currentSilences(ctx context.Context, tx pgx.Tx) ([]store.Silence, error) {
	now := s.clock.Now()

	s.mu.RLock()
	if s.cache != nil && now.Before(s.cache.expiresAt) {
		silences := s.cache.silences
		s.mu.RUnlock()
		return silences, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil && now.Before(s.cache.expiresAt) {
		return s.cache.silences, nil
	}

	silences, err := s.silences.List(ctx, tx, 0)
	if err != nil {
		return nil, err
	}
	s.cache = &silenceCache{silences: silences, expiresAt: now.Add(silenceCacheTTL)}
	return silences, nil
}
