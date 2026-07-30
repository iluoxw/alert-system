package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// timerKindTTL 是告警 TTL 自动恢复定时器在 scheduler 里的 kind，refID 是 alert.ID（R4.4）。
const timerKindTTL = "alert_ttl"

// resolveActorSource 是上游 resolved 事件驱动的 Resolve 动作的 actor 标识（R4.3），
// 与 scheduler 里"ttl-scheduler"这类系统标识是同一约定：非人工操作时由触发方给出
// 一个可读的系统身份，而不是留空（domain.Resolve 要求 actor 非空）。
const resolveActorSource = "source-event"

// AlertRepo 是 DedupStage 依赖的告警仓储子集，由 store.AlertRepo 实现（结构化满足，
// 不要求显式声明）。GetByFingerprint 用于 resolved 事件路径查找待 resolve 的现存告警。
type AlertRepo interface {
	UpsertByFingerprint(ctx context.Context, tx pgx.Tx, event domain.AlertEvent, integrationID string) (domain.Alert, bool, error)
	GetByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint string) (domain.Alert, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
}

// TimelineRepo 是 DedupStage 依赖的时间线仓储子集，由 store.TimelineRepo 实现。
type TimelineRepo interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
}

// TimerScheduler 是 DedupStage 依赖的定时器子集，由 scheduler.Scheduler 实现。
type TimerScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
}

// SeverityFallback 在 Event.Severity 缺失或非法时返回 integrationID 对应的兜底等级
// （R2.5）。具体来源（如 integrations.default_severity 列）由调用方决定，DedupStage
// 本身不关心；为 nil 或返回非法值时，DedupStage 退回 domain.SeverityWarning
// （与 integrations 表 default_severity 列的 DB 默认值一致）。
type SeverityFallback func(ctx context.Context, tx pgx.Tx, integrationID string) (domain.Severity, error)

// dedupClock 抽象「当前时间」，供测试注入 fake clock 精确控制 TTL 到期时间与
// resolve 时间戳，无需依赖真实 wall-clock（与 scheduler.Clock 同一模式）。
type dedupClock interface {
	Now() time.Time
}

type realDedupClock struct{}

func (realDedupClock) Now() time.Time { return time.Now() }

// DedupStage 是管道的去重 stage（R2.5/R3/R4.4）：
//   - severity 缺失/非法时兜底（R2.5）；
//   - firing 事件按 fingerprint upsert（新建或合并），合并不触发新通知（R3.4，
//     由 PipelineContext.Created 标志供后续 stage 判断）；
//   - resolve_policy=ttl 的告警在每次 upsert（新建或合并）后都会 Schedule 一次
//     TTL 定时器，新建即注册、合并即续期，都是同一次 Schedule 调用（scheduler
//     内部按 (kind, refID) 的替换语义天然处理了"续期"）；
//   - resolved 事件按 fingerprint 查找现存未 resolved 告警并 Resolve，找不到则
//     视为迟到事件忽略。
type DedupStage struct {
	alerts           AlertRepo
	timeline         TimelineRepo
	scheduler        TimerScheduler
	severityFallback SeverityFallback
	clock            dedupClock
}

// NewDedupStage 构造 DedupStage。severityFallback 可为 nil（此时缺失/非法 severity
// 一律兜底为 domain.SeverityWarning）。
func NewDedupStage(alerts AlertRepo, timeline TimelineRepo, sched TimerScheduler, severityFallback SeverityFallback) *DedupStage {
	return &DedupStage{
		alerts:           alerts,
		timeline:         timeline,
		scheduler:        sched,
		severityFallback: severityFallback,
		clock:            realDedupClock{},
	}
}

// Name 返回 "dedup"。
func (s *DedupStage) Name() string { return "dedup" }

// Process 实现 Stage 接口。
func (s *DedupStage) Process(ctx context.Context, pctx *PipelineContext) error {
	event := pctx.Event
	if err := s.applySeverityFallback(ctx, pctx.Tx, &event, pctx.IntegrationID); err != nil {
		return fmt.Errorf("dedup: severity 兜底失败: %w", err)
	}
	pctx.Event = event

	if event.Status == domain.StatusResolved {
		return s.processResolved(ctx, pctx, event)
	}
	return s.processFiring(ctx, pctx, event)
}

// applySeverityFallback 就地修正 event.Severity：合法值原样保留；缺失/非法时调用
// severityFallback 取兜底值，仍不合法（或 severityFallback 为 nil）时退回
// domain.SeverityWarning，并统一置 SeverityFallback = true（R2.5）。
func (s *DedupStage) applySeverityFallback(ctx context.Context, tx pgx.Tx, event *domain.AlertEvent, integrationID string) error {
	if event.Severity.IsValid() {
		return nil
	}

	fallback := domain.SeverityWarning
	if s.severityFallback != nil {
		v, err := s.severityFallback(ctx, tx, integrationID)
		if err != nil {
			return err
		}
		if v.IsValid() {
			fallback = v
		}
	}

	event.Severity = fallback
	event.SeverityFallback = true
	return nil
}

// processFiring 处理 firing 事件：upsert 告警，若 resolve_policy=ttl 则
// 注册/续期一次 TTL 定时器。
func (s *DedupStage) processFiring(ctx context.Context, pctx *PipelineContext, event domain.AlertEvent) error {
	alert, created, err := s.alerts.UpsertByFingerprint(ctx, pctx.Tx, event, pctx.IntegrationID)
	if err != nil {
		return fmt.Errorf("dedup: upsert 告警失败: %w", err)
	}
	pctx.Alert = alert
	pctx.Created = created

	if alert.ResolvePolicy == domain.ResolvePolicyTTL && alert.TTLSeconds > 0 {
		fireAt := s.clock.Now().Add(time.Duration(alert.TTLSeconds) * time.Second)
		if err := s.scheduler.Schedule(ctx, pctx.Tx, timerKindTTL, alert.ID, fireAt, nil); err != nil {
			return fmt.Errorf("dedup: 注册/续期 TTL 定时器失败: %w", err)
		}
	}
	return nil
}

// processResolved 处理 resolved 事件：按 fingerprint 查找现存未 resolved 告警。
// 找不到（已经 resolved 过，或从未创建）视为迟到事件，忽略、不报错。
func (s *DedupStage) processResolved(ctx context.Context, pctx *PipelineContext, event domain.AlertEvent) error {
	existing, err := s.alerts.GetByFingerprint(ctx, pctx.Tx, event.Fingerprint)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			return nil
		}
		return fmt.Errorf("dedup: 查询待 resolve 告警失败: %w", err)
	}

	now := s.clock.Now()
	resolved, entry, err := domain.Resolve(existing, resolveActorSource, domain.ResolveReasonSource, now)
	if err != nil {
		return fmt.Errorf("dedup: resolve 状态转移失败: %w", err)
	}
	if err := s.alerts.UpdateStatus(ctx, pctx.Tx, resolved); err != nil {
		return fmt.Errorf("dedup: 更新告警状态失败: %w", err)
	}
	if _, err := s.timeline.Append(ctx, pctx.Tx, entry); err != nil {
		return fmt.Errorf("dedup: 写入时间线失败: %w", err)
	}
	if err := s.scheduler.Cancel(ctx, pctx.Tx, timerKindTTL, resolved.ID); err != nil {
		return fmt.Errorf("dedup: 取消 TTL 定时器失败: %w", err)
	}

	pctx.Alert = resolved
	pctx.Created = false
	return nil
}
