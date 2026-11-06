package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// UpsertAuthoritativeOccurrence 是 Alertmanager webhook 与 API 对账共用的唯一镜像
// 写入口。advisory xact lock 把同 occurrence 的重复/并发写串行化，配合
// source_updated_at 防止迟到 firing 覆盖较新的 resolved。
func (r *AlertRepo) UpsertAuthoritativeOccurrence(
	ctx context.Context,
	tx pgx.Tx,
	event domain.AlertEvent,
	sourceID string,
	observedAt time.Time,
	generation int64,
) (domain.Alert, bool, domain.SourceState, error) {
	if sourceID == "" || event.UpstreamFingerprint == "" || event.StartsAt.IsZero() {
		return domain.Alert{}, false, "", fmt.Errorf("store: Alertmanager occurrence 身份不完整")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if event.SourceUpdatedAt.IsZero() {
		event.SourceUpdatedAt = event.StartsAt
	}
	sourceState := domain.SourceStateFiring
	if event.Status == domain.StatusResolved {
		sourceState = domain.SourceStateResolved
	}
	suppressionState := event.SuppressionState
	if !suppressionState.IsValid() {
		suppressionState = domain.SuppressionStateNone
	}

	lockKey := sourceID + "\x1e" + event.UpstreamFingerprint + "\x1e" + event.StartsAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return domain.Alert{}, false, "", fmt.Errorf("store: 锁定 Alertmanager occurrence 失败: %w", err)
	}

	previous := domain.SourceState("")
	var previousID string
	err := tx.QueryRow(ctx, `
		SELECT id::text, source_state FROM alerts
		WHERE source_integration_id = $1::uuid AND upstream_fingerprint = $2 AND starts_at = $3
	`, sourceID, event.UpstreamFingerprint, event.StartsAt).Scan(&previousID, &previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Alert{}, false, "", fmt.Errorf("store: 查询 Alertmanager occurrence 旧状态失败: %w", err)
	}

	labels, resourceRef, signalContext, suppressionDetails, err := marshalAuthoritativeEvent(event)
	if err != nil {
		return domain.Alert{}, false, "", err
	}
	legacyStatus := domain.ProjectLegacyStatus(sourceState, domain.ResponseStateUnacknowledged)
	endsAt := nullableTime(event.EndsAt)
	if sourceState == domain.SourceStateResolved && endsAt == nil {
		endsAt = &observedAt
	}
	lastReconciledAt := (*time.Time)(nil)
	if generation > 0 {
		lastReconciledAt = &observedAt
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO alerts (
			fingerprint, upstream_fingerprint, status, source_state, response_state,
			suppression_state, suppression_details,
			severity, severity_fallback, title, source, labels, resource_ref, signal_context,
			resolve_policy, source_integration_id, starts_at, last_seen_at, ends_at,
			source_updated_at, last_reconciled_at, reconcile_generation, reconcile_miss_count,
			source_resolution_reason
		) VALUES (
			$1, $2, $3, $4, 'unacknowledged', $5, $6,
			$7, $8, $9, $21, $10, $11, $12,
			'source', $13::uuid, $14, $15, $16,
			$17, $18, $19, 0, $20
		)
		ON CONFLICT (source_integration_id, upstream_fingerprint, starts_at)
			WHERE source_integration_id IS NOT NULL AND upstream_fingerprint != ''
		DO UPDATE SET
			dedup_count = alerts.dedup_count + 1,
			last_seen_at = GREATEST(alerts.last_seen_at, EXCLUDED.last_seen_at),
			last_reconciled_at = COALESCE(EXCLUDED.last_reconciled_at, alerts.last_reconciled_at),
			reconcile_generation = GREATEST(alerts.reconcile_generation, EXCLUDED.reconcile_generation),
			reconcile_miss_count = CASE WHEN EXCLUDED.reconcile_generation > 0 THEN 0 ELSE alerts.reconcile_miss_count END,
			severity = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.severity ELSE alerts.severity END,
			severity_fallback = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.severity_fallback ELSE alerts.severity_fallback END,
			title = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.title ELSE alerts.title END,
			labels = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.labels ELSE alerts.labels END,
			resource_ref = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.resource_ref ELSE alerts.resource_ref END,
			signal_context = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.signal_context ELSE alerts.signal_context END,
			source_state = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.source_state ELSE alerts.source_state END,
			suppression_state = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.suppression_state ELSE alerts.suppression_state END,
			suppression_details = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.suppression_details ELSE alerts.suppression_details END,
			source_updated_at = GREATEST(alerts.source_updated_at, EXCLUDED.source_updated_at),
			status = CASE
				WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at
				THEN CASE WHEN EXCLUDED.source_state = 'resolved' THEN 'resolved' WHEN alerts.response_state = 'acknowledged' THEN 'acknowledged' ELSE 'firing' END
				ELSE alerts.status END,
			ends_at = CASE WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at THEN EXCLUDED.ends_at ELSE alerts.ends_at END,
			source_resolution_reason = CASE
				WHEN alerts.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= alerts.source_updated_at
				THEN EXCLUDED.source_resolution_reason ELSE alerts.source_resolution_reason END
		RETURNING `+alertColumns+`, (xmax = 0) AS created
	`,
		event.UpstreamFingerprint, event.UpstreamFingerprint, legacyStatus, sourceState,
		suppressionState, suppressionDetails,
		event.Severity, event.SeverityFallback, event.Title, labels, resourceRef, signalContext,
		sourceID, event.StartsAt, observedAt, endsAt, event.SourceUpdatedAt,
		lastReconciledAt, generation, resolutionReason(sourceState, generation),
		event.Source,
	)
	var created bool
	alert, err := scanAlertRow(row, &created)
	if err != nil {
		return domain.Alert{}, false, previous, fmt.Errorf("store: upsert Alertmanager occurrence 失败: %w", err)
	}
	return alert, created, previous, nil
}

func marshalAuthoritativeEvent(event domain.AlertEvent) ([]byte, []byte, []byte, []byte, error) {
	labels := event.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	resourceRef := event.ResourceRef
	if resourceRef == nil {
		resourceRef = map[string]string{}
	}
	signalContext := event.SignalContext
	if signalContext == nil {
		signalContext = json.RawMessage(`{}`)
	}
	suppressionDetails := event.SuppressionDetails
	if suppressionDetails == nil {
		suppressionDetails = map[string][]string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: 序列化 Alertmanager labels 失败: %w", err)
	}
	resourceJSON, err := json.Marshal(resourceRef)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: 序列化 Alertmanager resource_ref 失败: %w", err)
	}
	suppressionJSON, err := json.Marshal(suppressionDetails)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: 序列化 Alertmanager suppression 失败: %w", err)
	}
	return labelsJSON, resourceJSON, []byte(signalContext), suppressionJSON, nil
}

func resolutionReason(state domain.SourceState, generation int64) any {
	if state != domain.SourceStateResolved {
		return nil
	}
	if generation > 0 {
		return "source_snapshot"
	}
	return "source_webhook"
}

// MarkMissingAfterSnapshot 只在一次完整、成功的 API 快照事务中调用。首次缺失先转
// unknown；达到确认次数才 resolved。API 请求失败不会进入本方法。
func (r *AlertRepo) MarkMissingAfterSnapshot(
	ctx context.Context,
	tx pgx.Tx,
	sourceID string,
	generation int64,
	missingConfirmations int,
	at time.Time,
) ([]domain.Alert, error) {
	if missingConfirmations < 2 {
		missingConfirmations = 2
	}
	rows, err := tx.Query(ctx, `
		SELECT `+alertColumns+`
		FROM alerts
		WHERE source_integration_id = $1::uuid
		  AND upstream_fingerprint != ''
		  AND source_state != 'resolved'
		  AND reconcile_generation < $2
		FOR UPDATE
	`, sourceID, generation)
	if err != nil {
		return nil, fmt.Errorf("store: 查询快照缺失 Alert 失败: %w", err)
	}
	defer rows.Close()
	var missing []domain.Alert
	for rows.Next() {
		a, err := scanAlertRow(rows)
		if err != nil {
			return nil, err
		}
		missing = append(missing, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	changed := make([]domain.Alert, 0, len(missing))
	for _, a := range missing {
		nextMiss := a.ReconcileMissCount + 1
		nextSource := domain.SourceStateUnknown
		reason := any(nil)
		endsAt := nullableTime(a.EndsAt)
		if nextMiss >= missingConfirmations {
			nextSource = domain.SourceStateResolved
			reason = "source_reconciled_absence"
			endsAt = &at
		}
		nextStatus := domain.ProjectLegacyStatus(nextSource, a.ResponseState)
		_, err := tx.Exec(ctx, `
			UPDATE alerts SET
				source_state = $2,
				status = $3,
				reconcile_miss_count = $4,
				last_reconciled_at = $5,
				ends_at = $6,
				source_resolution_reason = $7
			WHERE id = $1
		`, a.ID, nextSource, nextStatus, nextMiss, at, endsAt, reason)
		if err != nil {
			return nil, fmt.Errorf("store: 更新快照缺失 Alert 失败: %w", err)
		}
		a.SourceState = nextSource
		a.Status = nextStatus
		a.ReconcileMissCount = nextMiss
		a.LastReconciledAt = at
		if nextSource == domain.SourceStateResolved {
			a.EndsAt = at
			a.SourceResolutionReason = "source_reconciled_absence"
		}
		changed = append(changed, a)
	}
	return changed, nil
}
