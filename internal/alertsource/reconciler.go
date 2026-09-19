package alertsource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type ReconcileResult struct {
	SourceID        string
	SnapshotVersion int64
	Seen            int
	Unknown         int
	Resolved        int
	Recovered       int
	CompletedAt     time.Time
}

type Reconciler struct {
	store     *store.Store
	sources   *store.AlertSourceRepo
	alerts    *store.AlertRepo
	timeline  *store.TimelineRepo
	client    *Client
	logger    *slog.Logger
	now       func() time.Time
	tick      time.Duration
	projector AlertProjector
	// faultActivation 在 projector.Project 返回后被无条件调用（若已配置），由
	// 调用方（internal/app.Build）决定是否需要触发 Fault 通知（R5.10）。这是
	// 「Alertmanager 曾不可用、恢复后重新出现告警」场景下 Fault 通知的唯一入口——
	// 漏接会导致对账路径产生的 active Fault 永远不通知 owner，等同漏报。
	faultActivation func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error
}

type AlertProjector interface {
	Project(ctx context.Context, tx pgx.Tx, alert domain.Alert) (domain.AlertMappingResult, error)
}

func NewReconciler(st *store.Store, sources *store.AlertSourceRepo, alerts *store.AlertRepo, timeline *store.TimelineRepo, client *Client, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = NewClient(nil)
	}
	return &Reconciler{
		store: st, sources: sources, alerts: alerts, timeline: timeline,
		client: client, logger: logger, now: time.Now, tick: time.Second,
	}
}

// WithProjector makes API reconciliation and webhook ingestion converge through
// the same deterministic Alert Mapping implementation.
func (r *Reconciler) WithProjector(projector AlertProjector) *Reconciler {
	r.projector = projector
	return r
}

// WithFaultActivation 注入 Fault 激活通知的触发钩子（R5.10：周期对账路径，
// 覆盖 unknown->firing 恢复与快照缺失两处 Project 调用）。
func (r *Reconciler) WithFaultActivation(fn func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error) *Reconciler {
	r.faultActivation = fn
	return r
}

// triggerFaultActivation 是两处 Project 调用共用的收尾：faultActivation 未配置
// 时是零成本 no-op（保持 R2.9"ccloud/通知配置缺失不影响既有功能"的降级口径）。
func (r *Reconciler) triggerFaultActivation(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error {
	if r.faultActivation == nil {
		return nil
	}
	return r.faultActivation(ctx, tx, result)
}

func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	for {
		if err := r.reconcileDue(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.logger.Error("alertsource: 周期对账失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) reconcileDue(ctx context.Context) error {
	var sources []store.AlertSource
	if err := r.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		sources, err = r.sources.List(ctx, tx, 500)
		return err
	}); err != nil {
		return err
	}
	now := r.now()
	for _, source := range sources {
		if !source.ReconcileDue(now) {
			continue
		}
		if _, err := r.ReconcileSource(ctx, source.ID); err != nil && !errors.Is(err, context.Canceled) {
			r.logger.Warn("alertsource: source 对账失败", "source_id", source.ID, "source", source.Name, "err", err)
		}
	}
	return nil
}

// ReconcileSource 在专用 advisory lock 下完成一次拉取。外部 HTTP I/O 位于数据库
// 事务之外；只有快照已经完整解析后才开启短事务推进水位并应用状态。
func (r *Reconciler) ReconcileSource(ctx context.Context, sourceID string) (ReconcileResult, error) {
	conn, err := r.store.Pool.Acquire(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("alertsource: 获取 advisory lock 连接失败: %w", err)
	}
	defer conn.Release()
	lockKey := "alert-source-reconcile:" + sourceID
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, lockKey).Scan(&locked); err != nil {
		return ReconcileResult{}, fmt.Errorf("alertsource: 获取对账锁失败: %w", err)
	}
	if !locked {
		return ReconcileResult{}, fmt.Errorf("alertsource: source %s 已有对账任务运行", sourceID)
	}
	defer func() {
		var ignored bool
		_ = conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey).Scan(&ignored)
	}()

	var source store.AlertSource
	if err := r.store.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		source, getErr = r.sources.GetByID(ctx, tx, sourceID)
		return getErr
	}); err != nil {
		return ReconcileResult{}, err
	}
	at := r.now()
	events, fetchErr := r.client.Fetch(ctx, source)
	if fetchErr != nil {
		recordErr := r.store.WithTx(ctx, func(tx pgx.Tx) error {
			return r.sources.RecordFailure(ctx, tx, sourceID, at, fetchErr)
		})
		if recordErr != nil {
			return ReconcileResult{}, errors.Join(fetchErr, recordErr)
		}
		return ReconcileResult{}, fetchErr
	}

	result := ReconcileResult{SourceID: sourceID, Seen: len(events), CompletedAt: at}
	err = r.store.WithTx(ctx, func(tx pgx.Tx) error {
		version, err := r.sources.BeginSuccessfulSnapshot(ctx, tx, sourceID, at)
		if err != nil {
			return err
		}
		result.SnapshotVersion = version
		for _, event := range events {
			alert, _, previous, err := r.alerts.UpsertAuthoritativeOccurrence(ctx, tx, event, sourceID, at, version)
			if err != nil {
				return err
			}
			if previous == domain.SourceStateUnknown && alert.SourceState == domain.SourceStateFiring {
				result.Recovered++
				if _, err := r.timeline.Append(ctx, tx, domain.TimelineEntry{
					AlertID: alert.ID, Actor: "alertmanager-reconciler",
					Action: domain.TimelineActionSourceRecovered, At: at,
				}); err != nil {
					return err
				}
				if r.projector != nil {
					mappingResult, err := r.projector.Project(ctx, tx, alert)
					if err != nil {
						return fmt.Errorf("投影 Alert Mapping 失败: %w", err)
					}
					if err := r.triggerFaultActivation(ctx, tx, mappingResult); err != nil {
						return fmt.Errorf("触发 Fault 通知失败: %w", err)
					}
				}
			}
		}
		missing, err := r.alerts.MarkMissingAfterSnapshot(ctx, tx, sourceID, version, source.Config.MissingConfirmations, at)
		if err != nil {
			return err
		}
		for _, alert := range missing {
			if r.projector != nil {
				mappingResult, err := r.projector.Project(ctx, tx, alert)
				if err != nil {
					return fmt.Errorf("投影缺失 Alert Mapping 失败: %w", err)
				}
				if err := r.triggerFaultActivation(ctx, tx, mappingResult); err != nil {
					return fmt.Errorf("触发 Fault 通知失败: %w", err)
				}
			}
			switch alert.SourceState {
			case domain.SourceStateUnknown:
				if alert.ReconcileMissCount == 1 {
					result.Unknown++
					if _, err := r.timeline.Append(ctx, tx, domain.TimelineEntry{
						AlertID: alert.ID, Actor: "alertmanager-reconciler",
						Action: domain.TimelineActionSourceUnknown,
						Detail: map[string]any{"missing_confirmations_required": source.Config.MissingConfirmations}, At: at,
					}); err != nil {
						return err
					}
				}
			case domain.SourceStateResolved:
				result.Resolved++
				if _, err := r.timeline.Append(ctx, tx, domain.TimelineEntry{
					AlertID: alert.ID, Actor: "alertmanager-reconciler",
					Action: domain.TimelineActionResolve,
					Detail: map[string]any{"reason": string(domain.ResolveReasonSourceReconciled)}, At: at,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("alertsource: 应用快照失败: %w", err)
	}
	return result, nil
}
