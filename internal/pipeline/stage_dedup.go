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
	// GetLatestResolvedByFingerprint 与 UpdateFlapState 供 flapping 防抖（R4/C4）使用，
	// 见本文件 applyFlapOnRefire/persistFlapTransition 的调用点与注释。
	GetLatestResolvedByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint string) (domain.Alert, error)
	UpdateFlapState(ctx context.Context, tx pgx.Tx, id string, flapCount int, flapWindowStart time.Time, flapping bool) error
}

type authoritativeAlertRepo interface {
	UpsertAuthoritativeOccurrence(
		ctx context.Context,
		tx pgx.Tx,
		event domain.AlertEvent,
		sourceID string,
		observedAt time.Time,
		generation int64,
	) (domain.Alert, bool, domain.SourceState, error)
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
//     视为迟到事件忽略；
//   - flapWindowSeconds<=0 时 flapping 防抖（R4/C4）整体关闭，行为与现状逐字节
//     一致（不产生任何额外 UPDATE/时间线写入），flapWindowSeconds>0 时在 firing/
//     resolved 两个转移点计翻转、判定置位/惰性解除（见 evaluateFlap）。
type DedupStage struct {
	alerts           AlertRepo
	timeline         TimelineRepo
	scheduler        TimerScheduler
	severityFallback SeverityFallback
	clock            dedupClock

	flapWindowSeconds int
	flapThreshold     int
}

// NewDedupStage 构造 DedupStage。severityFallback 可为 nil（此时缺失/非法 severity
// 一律兜底为 domain.SeverityWarning）。flapWindowSeconds<=0 关闭 flapping 防抖（opt-in，
// 与 config.Config.FlapWindowSeconds 默认值 0 对齐）。
func NewDedupStage(alerts AlertRepo, timeline TimelineRepo, sched TimerScheduler, severityFallback SeverityFallback, flapWindowSeconds, flapThreshold int) *DedupStage {
	return &DedupStage{
		alerts:            alerts,
		timeline:          timeline,
		scheduler:         sched,
		severityFallback:  severityFallback,
		clock:             realDedupClock{},
		flapWindowSeconds: flapWindowSeconds,
		flapThreshold:     flapThreshold,
	}
}

// Name 返回 "dedup"。
func (s *DedupStage) Name() string { return "dedup" }

func (s *DedupStage) RunForAuthoritativeSource() bool { return true }

// Process 实现 Stage 接口。
func (s *DedupStage) Process(ctx context.Context, pctx *PipelineContext) error {
	event := pctx.Event
	if err := s.applySeverityFallback(ctx, pctx.Tx, &event, pctx.IntegrationID); err != nil {
		return fmt.Errorf("dedup: severity 兜底失败: %w", err)
	}
	pctx.Event = event
	if domain.IsAuthoritativeSourceKind(event.Source) && event.UpstreamFingerprint != "" && pctx.IntegrationID != "" {
		return s.processAuthoritative(ctx, pctx, event)
	}

	if event.Status == domain.StatusResolved {
		return s.processResolved(ctx, pctx, event)
	}
	return s.processFiring(ctx, pctx, event)
}

func (s *DedupStage) processAuthoritative(ctx context.Context, pctx *PipelineContext, event domain.AlertEvent) error {
	repo, ok := s.alerts.(authoritativeAlertRepo)
	if !ok {
		return fmt.Errorf("dedup: 告警仓储未实现 Alertmanager 权威镜像接口")
	}
	now := s.clock.Now()
	alert, created, previous, err := repo.UpsertAuthoritativeOccurrence(ctx, pctx.Tx, event, pctx.IntegrationID, now, 0)
	if err != nil {
		return fmt.Errorf("dedup: 镜像 Alertmanager occurrence 失败: %w", err)
	}
	pctx.Alert = alert
	pctx.Created = created
	pctx.AuthoritativeSource = true

	if alert.SourceState == domain.SourceStateResolved && previous != "" && previous != domain.SourceStateResolved {
		entry := domain.TimelineEntry{
			AlertID: alert.ID,
			Actor:   "alertmanager-webhook",
			Action:  domain.TimelineActionResolve,
			Detail:  map[string]any{"reason": string(domain.ResolveReasonSource)},
			At:      now,
		}
		if _, err := s.timeline.Append(ctx, pctx.Tx, entry); err != nil {
			return fmt.Errorf("dedup: 写入 Alertmanager resolved 时间线失败: %w", err)
		}
	}
	if alert.SourceState == domain.SourceStateFiring && previous == domain.SourceStateUnknown {
		entry := domain.TimelineEntry{
			AlertID: alert.ID, Actor: "alertmanager-webhook",
			Action: domain.TimelineActionSourceRecovered, At: now,
		}
		if _, err := s.timeline.Append(ctx, pctx.Tx, entry); err != nil {
			return fmt.Errorf("dedup: 写入 Alertmanager source_recovered 时间线失败: %w", err)
		}
	}
	return nil
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
// 注册/续期一次 TTL 定时器；flap 防抖开启（见 flapEnabled）且本次是新建行
// （created=true）时额外做一次 flap 基线延续判定（见 applyFlapOnRefire）——合并进
// 已有行（created=false）时状态仍是 firing→firing，不构成转移，不碰 flap 三字段。
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

	if s.flapEnabled() && created {
		if err := s.applyFlapOnRefire(ctx, pctx); err != nil {
			return err
		}
	}
	return nil
}

// flapEnabled 是 flap 防抖的总闸门：窗口与阈值都必须为正。阈值<=0 时若放行，
// evaluateFlap 的 count>=threshold 恒真、首个事件就会置位——与本 spec「零值=关闭」
// 的约定相悖，因此显式配 FLAP_THRESHOLD=0 与不配窗口同义（整体关闭）。
func (s *DedupStage) flapEnabled() bool {
	return s.flapWindowSeconds > 0 && s.flapThreshold > 0
}

// applyFlapOnRefire 处理 processFiring 里 created==true 的 flap 延续：R3.4 下
// resolved 是终态，同 fingerprint 再次 firing 必然落在一条全新的行上（flap 三字段
// 回到列默认值），但这次 refire 本身就是一次 resolved→firing 翻转。若不继承上一条
// 历史行的 flap_count/flap_window_start 作为基线，翻转计数每次都会从 1 重新起算，
// 阈值检测对"同一 fingerprint 反复 firing/resolved"这个 R4 本该覆盖的核心场景形同
// 虚设。查询走 alerts_fingerprint_resolved_idx（迁移 000009，与排除 resolved 行的
// alerts_fingerprint_unresolved_uidx 互不冲突）。flapping 布尔值不继承——新行的
// flapping 从 false 起算，由阈值判定在 evaluateFlap 里自然重新推导，让 flapping_start
// 时间线落在这条新行自己身上（有实际存在的行可挂载）。
func (s *DedupStage) applyFlapOnRefire(ctx context.Context, pctx *PipelineContext) error {
	baseline, err := s.alerts.GetLatestResolvedByFingerprint(ctx, pctx.Tx, pctx.Alert.Fingerprint)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			baseline = domain.Alert{} // 该 fingerprint 首次出现，从零值起算
		} else {
			return fmt.Errorf("dedup: 查询历史 flap 基线失败: %w", err)
		}
	}
	baseline.Flapping = false // 不继承 flapping bool，见函数注释

	return s.persistFlapTransition(ctx, pctx, baseline, s.clock.Now())
}

// flapTransition 是 evaluateFlap 的计算结果：count/windowStart/flapping 是待持久化
// 的三个 flap 字段目标值；started/ended 标记本次转移是否触发了 flapping 置位/惰性
// 解除，供调用方决定要不要写时间线、要不要置 pctx.FlappingStarted。
type flapTransition struct {
	count       int
	windowStart time.Time
	flapping    bool
	started     bool
	ended       bool
}

// evaluateFlap 按 design.md C4 计算一次 firing↔resolved 转移对 flap 三字段的影响。
// baseline 是转移前的 flap 状态（同一行的既有值，或 refire 时从历史行延续来的基线，
// 来源由调用方决定，本函数不关心）：
//   - 窗口滑出（baseline.FlapWindowStart 为零值，或 now 距其超过 windowSeconds）：
//     count 重置为 1、windowStart=now；若 baseline 此前处于 flapping，本次判定为
//     惰性解除（ended=true，flapping 置回 false）。
//   - 未滑出：count = baseline.FlapCount + 1，windowStart 不变；count >= threshold
//     且 baseline 此前未 flapping 时判定为置位（started=true，flapping 置 true）；
//     否则 flapping 维持 baseline 原值不变（已在 flapping 中的后续翻转不重复触发）。
//
// 调用方负责保证 windowSeconds>0（总闸门在 processFiring/processResolved 判断）。
func evaluateFlap(baseline domain.Alert, now time.Time, windowSeconds, threshold int) flapTransition {
	window := time.Duration(windowSeconds) * time.Second
	slidOut := baseline.FlapWindowStart.IsZero() || now.Sub(baseline.FlapWindowStart) > window

	if slidOut {
		return flapTransition{
			count:       1,
			windowStart: now,
			flapping:    false,
			ended:       baseline.Flapping,
		}
	}

	count := baseline.FlapCount + 1
	flapping := baseline.Flapping
	started := false
	if count >= threshold && !baseline.Flapping {
		flapping = true
		started = true
	}
	return flapTransition{
		count:       count,
		windowStart: baseline.FlapWindowStart,
		flapping:    flapping,
		started:     started,
	}
}

// persistFlapTransition 依据 baseline 算出新的 flap 三字段并落到 pctx.Alert 这一行：
// 置位/解除时补写时间线（flapping_start/flapping_end）并在置位当次设置
// pctx.FlappingStarted（供 group stage 判断"抖动提示当次仍要发出"，见
// PipelineContext 注释）。同步更新 pctx.Alert 的内存态，保持与落库结果一致，
// 供后续 stage（route/group/silence/inhibit）读取到最新 flap 状态。
func (s *DedupStage) persistFlapTransition(ctx context.Context, pctx *PipelineContext, baseline domain.Alert, now time.Time) error {
	t := evaluateFlap(baseline, now, s.flapWindowSeconds, s.flapThreshold)

	if err := s.alerts.UpdateFlapState(ctx, pctx.Tx, pctx.Alert.ID, t.count, t.windowStart, t.flapping); err != nil {
		return fmt.Errorf("dedup: 更新 flap 状态失败: %w", err)
	}
	pctx.Alert.FlapCount = t.count
	pctx.Alert.FlapWindowStart = t.windowStart
	pctx.Alert.Flapping = t.flapping

	var action domain.TimelineAction
	switch {
	case t.started:
		pctx.FlappingStarted = true
		action = domain.TimelineActionFlappingStart
	case t.ended:
		action = domain.TimelineActionFlappingEnd
	default:
		return nil
	}

	entry := domain.TimelineEntry{
		AlertID: pctx.Alert.ID,
		Actor:   resolveActorSource,
		Action:  action,
		At:      now,
	}
	if _, err := s.timeline.Append(ctx, pctx.Tx, entry); err != nil {
		return fmt.Errorf("dedup: 写入 %s 时间线失败: %w", action, err)
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
	resolved, entry, err := domain.Resolve(existing, resolveActorSource, domain.ResolveReasonSource, "", now)
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

	// firing→resolved 本身就是一次翻转，基线就是这一行自己转移前的 flap 状态
	// （existing，尚未被上面的 resolve 覆盖）。
	if s.flapEnabled() {
		if err := s.persistFlapTransition(ctx, pctx, existing, now); err != nil {
			return err
		}
	}
	return nil
}
