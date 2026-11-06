package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

type FaultRepo struct{}

func NewFaultRepo() *FaultRepo { return &FaultRepo{} }

var (
	ErrFaultNotFound        = errors.New("store: Fault 不存在")
	ErrFaultImpactNotFound  = errors.New("store: Fault Impact 不存在")
	ErrAckActorNameRequired = errors.New("store: ack 的 actorName 不能为空")
)

// faultColumns 覆盖迁移 000026 新增的五个归属/认领列（owner_team_id / acked_by /
// acked_by_name / acked_at / owner_notified_at），所有读 Fault 的查询共用同一列表，
// 避免个别查询漏读导致 API 层拿不到 owner/ack 字段（R4.7）。
const faultColumns = `
	id, correlation_key, scope, status, severity, kind, title, summary,
	COALESCE(resource_node_id::text, ''), entity_ref, confidence, metadata,
	first_detected_at, last_evaluated_at, resolved_at, created_at, updated_at,
	COALESCE(owner_team_id::text, ''), COALESCE(acked_by::text, ''),
	COALESCE(acked_by_name, ''), acked_at, owner_notified_at
`

type faultRowScanner interface {
	Scan(dest ...any) error
}

func scanFault(row faultRowScanner) (domain.Fault, error) {
	var fault domain.Fault
	var metadataRaw []byte
	var resolvedAt, ackedAt, ownerNotifiedAt *time.Time
	if err := row.Scan(
		&fault.ID, &fault.CorrelationKey, &fault.Scope, &fault.Status, &fault.Severity,
		&fault.Kind, &fault.Title, &fault.Summary, &fault.ResourceNodeID, &fault.EntityRef,
		&fault.Confidence, &metadataRaw, &fault.FirstDetected, &fault.LastEvaluated,
		&resolvedAt, &fault.CreatedAt, &fault.UpdatedAt,
		&fault.OwnerTeamID, &fault.AckedBy, &fault.AckedByName, &ackedAt, &ownerNotifiedAt,
	); err != nil {
		return domain.Fault{}, err
	}
	if resolvedAt != nil {
		fault.ResolvedAt = *resolvedAt
	}
	if ackedAt != nil {
		fault.AckedAt = *ackedAt
	}
	if ownerNotifiedAt != nil {
		fault.OwnerNotifiedAt = *ownerNotifiedAt
	}
	if err := json.Unmarshal(metadataRaw, &fault.Metadata); err != nil {
		return domain.Fault{}, fmt.Errorf("store: 反序列化 Fault metadata 失败: %w", err)
	}
	return fault, nil
}

// Upsert 按未解决 correlation_key 合并自动关联结果。active 不会被下一轮缺少直接
// 证据的 candidate 评估静默降级；进入 monitoring/resolved 必须走后续恢复状态机。
func (r *FaultRepo) Upsert(ctx context.Context, tx pgx.Tx, fault domain.Fault) (domain.Fault, bool, error) {
	if fault.CorrelationKey == "" || fault.Kind == "" || fault.Title == "" ||
		!fault.Scope.IsValid() || !fault.Status.IsValid() || !fault.Severity.IsValid() ||
		fault.Confidence < 0 || fault.Confidence > 1 || fault.FirstDetected.IsZero() || fault.LastEvaluated.IsZero() {
		return domain.Fault{}, false, fmt.Errorf("store: Fault 字段非法")
	}
	metadata, err := marshalJSONObject(fault.Metadata)
	if err != nil {
		return domain.Fault{}, false, fmt.Errorf("store: 序列化 Fault metadata 失败: %w", err)
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO faults (
			correlation_key, scope, status, severity, kind, title, summary,
			resource_node_id, entity_ref, confidence, metadata,
			first_detected_at, last_evaluated_at, resolved_at, owner_team_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			NULLIF($8, '')::uuid, $9, $10, $11,
			$12, $13, $14, NULLIF($15, '')::uuid
		)
		ON CONFLICT (correlation_key) WHERE status <> 'resolved' DO UPDATE SET
			status = CASE
				WHEN faults.status = 'active' AND EXCLUDED.status = 'candidate' THEN faults.status
				ELSE EXCLUDED.status
			END,
			severity = EXCLUDED.severity,
			title = EXCLUDED.title,
			summary = EXCLUDED.summary,
			resource_node_id = COALESCE(EXCLUDED.resource_node_id, faults.resource_node_id),
			entity_ref = EXCLUDED.entity_ref,
			confidence = GREATEST(faults.confidence, EXCLUDED.confidence),
			metadata = EXCLUDED.metadata,
			first_detected_at = LEAST(faults.first_detected_at, EXCLUDED.first_detected_at),
			last_evaluated_at = GREATEST(faults.last_evaluated_at, EXCLUDED.last_evaluated_at),
			-- 本轮解析不到 owner（EXCLUDED.owner_team_id 为 NULL）时保留已有值，
			-- 避免实体登记暂时性缺失把已经生效的归属冲成 NULL（R3.2/不变量 15）。
			owner_team_id = COALESCE(EXCLUDED.owner_team_id, faults.owner_team_id)
		RETURNING `+faultColumns+`, (xmax = 0) AS created
	`, fault.CorrelationKey, fault.Scope, fault.Status, fault.Severity, fault.Kind,
		fault.Title, fault.Summary, fault.ResourceNodeID, fault.EntityRef, fault.Confidence,
		metadata, fault.FirstDetected, fault.LastEvaluated, nullableTime(fault.ResolvedAt), fault.OwnerTeamID)

	var created bool
	stored, err := scanFaultWithExtra(row, &created)
	if err != nil {
		return domain.Fault{}, false, fmt.Errorf("store: upsert Fault 失败: %w", err)
	}
	return stored, created, nil
}

func scanFaultWithExtra(row faultRowScanner, extra ...any) (domain.Fault, error) {
	var fault domain.Fault
	var metadataRaw []byte
	var resolvedAt, ackedAt, ownerNotifiedAt *time.Time
	dest := []any{
		&fault.ID, &fault.CorrelationKey, &fault.Scope, &fault.Status, &fault.Severity,
		&fault.Kind, &fault.Title, &fault.Summary, &fault.ResourceNodeID, &fault.EntityRef,
		&fault.Confidence, &metadataRaw, &fault.FirstDetected, &fault.LastEvaluated,
		&resolvedAt, &fault.CreatedAt, &fault.UpdatedAt,
		&fault.OwnerTeamID, &fault.AckedBy, &fault.AckedByName, &ackedAt, &ownerNotifiedAt,
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return domain.Fault{}, err
	}
	if resolvedAt != nil {
		fault.ResolvedAt = *resolvedAt
	}
	if ackedAt != nil {
		fault.AckedAt = *ackedAt
	}
	if ownerNotifiedAt != nil {
		fault.OwnerNotifiedAt = *ownerNotifiedAt
	}
	if err := json.Unmarshal(metadataRaw, &fault.Metadata); err != nil {
		return domain.Fault{}, fmt.Errorf("store: 反序列化 Fault metadata 失败: %w", err)
	}
	return fault, nil
}

func (r *FaultRepo) GetByCorrelationKey(ctx context.Context, tx pgx.Tx, key string) (domain.Fault, error) {
	row := tx.QueryRow(ctx, `SELECT `+faultColumns+` FROM faults WHERE correlation_key = $1 AND status <> 'resolved'`, key)
	fault, err := scanFault(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fault{}, ErrFaultNotFound
	}
	if err != nil {
		return domain.Fault{}, fmt.Errorf("store: 查询 Fault 失败: %w", err)
	}
	return fault, nil
}

func (r *FaultRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error) {
	row := tx.QueryRow(ctx, `SELECT `+faultColumns+` FROM faults WHERE id = $1`, id)
	fault, err := scanFault(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fault{}, ErrFaultNotFound
	}
	if err != nil {
		return domain.Fault{}, fmt.Errorf("store: 按 ID 查询 Fault 失败: %w", err)
	}
	return fault, nil
}

func (r *FaultRepo) ListOpenForNodeIDs(ctx context.Context, tx pgx.Tx, nodeIDs []string, limit int) ([]domain.Fault, error) {
	if len(nodeIDs) == 0 {
		return []domain.Fault{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := tx.Query(ctx, `
		SELECT `+faultColumns+` FROM faults
		WHERE resource_node_id::text = ANY($1) AND status <> 'resolved'
		ORDER BY last_evaluated_at DESC LIMIT $2
	`, nodeIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询实体 open Fault 失败: %w", err)
	}
	defer rows.Close()
	var result []domain.Fault
	for rows.Next() {
		fault, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, fault)
	}
	return result, rows.Err()
}

// HasActiveDirectEvidenceForNode answers whether an application currently has
// direct user-impact evidence overlapping the provider Fault's time window.
// It supports authoritative occurrences and legacy replay Alerts without
// conflating acknowledgement with source recovery.
func (r *FaultRepo) HasActiveDirectEvidenceForNode(ctx context.Context, tx pgx.Tx, nodeID string, since time.Time) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM faults f
			JOIN fault_alert_links l ON l.fault_id = f.id AND l.role = 'direct_impact'
			JOIN alerts a ON a.id = l.alert_id
			WHERE f.resource_node_id = $1
			  AND f.status = 'active'
			  AND a.last_seen_at >= $2::timestamptz - interval '5 minutes'
			  AND (
				(a.upstream_fingerprint <> '' AND a.source_state = 'firing') OR
				(a.upstream_fingerprint = '' AND a.status <> 'resolved')
			  )
		)
	`, nodeID, since).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: 查询应用 direct evidence 失败: %w", err)
	}
	return exists, nil
}

func (r *FaultRepo) HasActiveDirectEvidenceForFault(ctx context.Context, tx pgx.Tx, faultID string, since time.Time) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM faults f
			JOIN fault_alert_links l ON l.fault_id = f.id AND l.role = 'direct_impact'
			JOIN alerts a ON a.id = l.alert_id
			WHERE f.id = $1 AND f.status = 'active'
			  AND a.last_seen_at >= $2::timestamptz - interval '5 minutes'
			  AND (
				(a.upstream_fingerprint <> '' AND a.source_state = 'firing') OR
				(a.upstream_fingerprint = '' AND a.status <> 'resolved')
			  )
		)
	`, faultID, since).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: 查询 Fault direct evidence 失败: %w", err)
	}
	return exists, nil
}

func (r *FaultRepo) List(ctx context.Context, tx pgx.Tx, status domain.FaultStatus, limit int) ([]domain.Fault, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT ` + faultColumns + ` FROM faults`
	args := []any{}
	if status != "" {
		query += ` WHERE status = $1`
		args = append(args, status)
	}
	query += fmt.Sprintf(` ORDER BY CASE status WHEN 'active' THEN 1 WHEN 'candidate' THEN 2 WHEN 'monitoring' THEN 3 ELSE 4 END, severity, last_evaluated_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Fault 列表失败: %w", err)
	}
	defer rows.Close()
	var result []domain.Fault
	for rows.Next() {
		item, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *FaultRepo) ListForNode(ctx context.Context, tx pgx.Tx, nodeID string, limit int) ([]domain.Fault, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT `+qualifiedFaultColumns("f")+`
		FROM faults f
		LEFT JOIN fault_impacts i ON i.fault_id = f.id
		WHERE f.resource_node_id = $1 OR i.node_id = $1
		ORDER BY f.last_evaluated_at DESC
		LIMIT $2
	`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Fault
	for rows.Next() {
		item, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func qualifiedFaultColumns(alias string) string {
	return alias + `.id, ` + alias + `.correlation_key, ` + alias + `.scope, ` + alias + `.status, ` +
		alias + `.severity, ` + alias + `.kind, ` + alias + `.title, ` + alias + `.summary, ` +
		`COALESCE(` + alias + `.resource_node_id::text, ''), ` + alias + `.entity_ref, ` +
		alias + `.confidence, ` + alias + `.metadata, ` + alias + `.first_detected_at, ` +
		alias + `.last_evaluated_at, ` + alias + `.resolved_at, ` + alias + `.created_at, ` + alias + `.updated_at, ` +
		`COALESCE(` + alias + `.owner_team_id::text, ''), COALESCE(` + alias + `.acked_by::text, ''), ` +
		`COALESCE(` + alias + `.acked_by_name, ''), ` + alias + `.acked_at, ` + alias + `.owner_notified_at`
}

// ListOpenRelatedToNode 一次取回挂在该实体上的 active/monitoring Fault：
// resource_node_id 命中，或 fault_impacts.node_id 命中。resolved / candidate 不进预览。
func (r *FaultRepo) ListOpenRelatedToNode(ctx context.Context, tx pgx.Tx, nodeID string, limit int) ([]domain.Fault, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT `+qualifiedFaultColumns("f")+`
		FROM faults f
		LEFT JOIN fault_impacts i ON i.fault_id = f.id
		WHERE (f.resource_node_id = $1 OR i.node_id = $1)
		  AND f.status IN ('active', 'monitoring')
		ORDER BY f.last_evaluated_at DESC
		LIMIT $2
	`, nodeID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询实体关联未收口 Fault 失败: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Fault, 0)
	for rows.Next() {
		item, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// HasActiveRelatedToNode 只看 status='active'。409 用这条，不能拿预览里的
// monitoring 一并挡下线（R3.2 只约束 active）。
func (r *FaultRepo) HasActiveRelatedToNode(ctx context.Context, tx pgx.Tx, nodeID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM faults f
			LEFT JOIN fault_impacts i ON i.fault_id = f.id
			WHERE (f.resource_node_id = $1 OR i.node_id = $1)
			  AND f.status = 'active'
		)
	`, nodeID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: 查询实体 active Fault 失败: %w", err)
	}
	return exists, nil
}

// ResolveByDecommission 是下线收口专用路径：不走 ValidateFaultTransition
// （active→resolved 被通用状态机禁止）。已 resolved 则 changed=false，不重写。
func (r *FaultRepo) ResolveByDecommission(ctx context.Context, tx pgx.Tx, id string, at time.Time) (domain.Fault, bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	row := tx.QueryRow(ctx, `
		UPDATE faults
		SET status = 'resolved',
		    last_evaluated_at = GREATEST(last_evaluated_at, $2),
		    resolved_at = $2
		WHERE id = $1 AND status <> 'resolved'
		RETURNING `+faultColumns, id, at)
	fault, err := scanFault(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := r.GetByID(ctx, tx, id)
		if getErr != nil {
			return domain.Fault{}, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return domain.Fault{}, false, fmt.Errorf("store: 按下线收口 Fault 失败: %w", err)
	}
	return fault, true, nil
}

func (r *FaultRepo) HasConfirmedBusinessImpact(ctx context.Context, tx pgx.Tx, faultID string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM fault_impacts i
			JOIN resource_nodes n ON n.id = i.node_id
			WHERE i.fault_id = $1
			  AND i.state = 'confirmed'
			  AND n.kind = 'business_system'
		)
	`, faultID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: 查询 Fault 已确认业务影响失败: %w", err)
	}
	return exists, nil
}

func (r *FaultRepo) UpdateLifecycleStatus(ctx context.Context, tx pgx.Tx, id string, from, to domain.FaultStatus, at time.Time) (domain.Fault, error) {
	if err := domain.ValidateFaultTransition(from, to); err != nil {
		return domain.Fault{}, err
	}
	row := tx.QueryRow(ctx, `
		UPDATE faults
		SET status = $3,
		    last_evaluated_at = GREATEST(last_evaluated_at, $4),
		    resolved_at = CASE WHEN $3 = 'resolved' THEN $4 ELSE resolved_at END
		WHERE id = $1 AND status = $2
		RETURNING `+faultColumns,
		id, from, to, at)
	fault, err := scanFault(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fault{}, fmt.Errorf("%w: 并发状态已变化", domain.ErrInvalidFaultTransition)
	}
	if err != nil {
		return domain.Fault{}, fmt.Errorf("store: 更新 Fault 生命周期失败: %w", err)
	}
	return fault, nil
}

// ReplaceAutomatedEvidence 只替换自动判断；人工裁决行不会被删除，也不会被自动结果
// 覆盖，从存储层保证 human-over-automation 的优先级。
func (r *FaultRepo) ReplaceAutomatedEvidence(ctx context.Context, tx pgx.Tx, faultID string, evidence []domain.FaultEvidence) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fault_alert_links WHERE fault_id = $1 AND decision_origin = 'automation'`, faultID); err != nil {
		return fmt.Errorf("store: 清理自动 Fault 证据失败: %w", err)
	}
	for _, item := range evidence {
		if item.AlertID == "" || !item.Role.IsValid() || item.Confidence < 0 || item.Confidence > 1 {
			return fmt.Errorf("store: Fault 证据字段非法")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fault_alert_links (
				fault_id, alert_id, role, rationale, confidence, decision_origin
			) VALUES ($1, $2, $3, $4, $5, 'automation')
			ON CONFLICT (fault_id, alert_id) DO UPDATE SET
				role = EXCLUDED.role,
				rationale = EXCLUDED.rationale,
				confidence = EXCLUDED.confidence,
				decision_origin = EXCLUDED.decision_origin
			WHERE fault_alert_links.decision_origin <> 'human'
		`, faultID, item.AlertID, item.Role, item.Rationale, item.Confidence); err != nil {
			return fmt.Errorf("store: 写入 Fault 证据失败: %w", err)
		}
	}
	return nil
}

// UpsertAutomatedEvidence 供逐条 Alert Mapping 投影使用；它不会删除同一 Fault 的
// 其他证据，且绝不覆盖人工裁决。
func (r *FaultRepo) UpsertAutomatedEvidence(ctx context.Context, tx pgx.Tx, faultID string, evidence domain.FaultEvidence) error {
	if evidence.AlertID == "" || !evidence.Role.IsValid() || evidence.Confidence < 0 || evidence.Confidence > 1 {
		return fmt.Errorf("store: Fault 证据字段非法")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fault_alert_links (
			fault_id, alert_id, role, rationale, confidence, decision_origin
		) VALUES ($1, $2, $3, $4, $5, 'automation')
		ON CONFLICT (fault_id, alert_id) DO UPDATE SET
			role = EXCLUDED.role, rationale = EXCLUDED.rationale,
			confidence = EXCLUDED.confidence, decision_origin = 'automation'
		WHERE fault_alert_links.decision_origin <> 'human'
	`, faultID, evidence.AlertID, evidence.Role, evidence.Rationale, evidence.Confidence)
	if err != nil {
		return fmt.Errorf("store: 写入单条 Fault 证据失败: %w", err)
	}
	return nil
}

// RemoveAutomatedEvidenceForAlertExcept 清除策略重映射后遗留的旧自动证据；人工裁决
// 始终保留，exceptFaultID 为空时清除该 Alert 的全部自动关联。
func (r *FaultRepo) RemoveAutomatedEvidenceForAlertExcept(ctx context.Context, tx pgx.Tx, alertID, exceptFaultID string) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM fault_alert_links
		WHERE alert_id = $1 AND decision_origin = 'automation'
		  AND ($2 = '' OR fault_id <> $2::uuid)
	`, alertID, exceptFaultID)
	return err
}

// Ack 认领 Fault：仅在尚未认领（acked_at IS NULL）时写入 acked_by/acked_by_name/
// acked_at；已认领时不覆盖既有认领人，返回当前状态且 ok=false——与
// domain.AckFault 的幂等短路语义一致（不变量 6），调用方据此判断是否需要追加
// 时间线，而不是把"重复认领"当错误处理。
func (r *FaultRepo) Ack(ctx context.Context, tx pgx.Tx, id, actorID, actorName string, at time.Time) (domain.Fault, bool, error) {
	if strings.TrimSpace(actorName) == "" {
		return domain.Fault{}, false, ErrAckActorNameRequired
	}
	row := tx.QueryRow(ctx, `
		UPDATE faults
		SET acked_by = NULLIF($2, '')::uuid, acked_by_name = $3, acked_at = $4
		WHERE id = $1 AND acked_at IS NULL
		RETURNING `+faultColumns, id, actorID, actorName, at)
	fault, err := scanFault(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// 0 行受影响：要么已认领，要么 Fault 不存在，两种情况都需要回查当前状态
		// 才能区分（GetByID 在不存在时返回 ErrFaultNotFound）。
		existing, getErr := r.GetByID(ctx, tx, id)
		if getErr != nil {
			return domain.Fault{}, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return domain.Fault{}, false, fmt.Errorf("store: 认领 Fault 失败: %w", err)
	}
	return fault, true, nil
}

// SetOwnerNotified 记录 Fault owner 团队通知意图已产生的时刻（R5.6）。语义取舍见
// app.FaultOutbox.QueueFaultActivated 的文档：这里是"意图已调度"而非"确认发送
// 成功"，因为拓扑抑制需要在意图时刻就能看到"已通知"。
func (r *FaultRepo) SetOwnerNotified(ctx context.Context, tx pgx.Tx, id string, at time.Time) error {
	ct, err := tx.Exec(ctx, `UPDATE faults SET owner_notified_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("store: 记录 Fault owner 通知时间失败: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrFaultNotFound
	}
	return nil
}

// AppendTimeline 是 fault_timeline_entries 的唯一写入方法（append-only）。
// entry.ActorID 可空（系统动作无用户），entry.Actor 必填。
func (r *FaultRepo) AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error) {
	if entry.FaultID == "" || strings.TrimSpace(entry.Actor) == "" || entry.Action == "" || entry.At.IsZero() {
		return "", fmt.Errorf("store: Fault 时间线字段非法")
	}
	detail, err := marshalJSONObject(entry.Detail)
	if err != nil {
		return "", fmt.Errorf("store: 序列化 Fault 时间线 detail 失败: %w", err)
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO fault_timeline_entries (fault_id, actor_id, actor_name, action, detail, at)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6)
		RETURNING id::text
	`, entry.FaultID, entry.ActorID, entry.Actor, entry.Action, detail, entry.At).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: 写入 Fault 时间线失败: %w", err)
	}
	return id, nil
}

// ListRecentlyResolvedByCorrelationKey 查询同一 correlation_key 在 since 之后
// resolved 的 Fault（冷却窗口判定用，R5.3）：非空即表示冷却窗口内刚恢复，本轮
// 不应重新通知。correlation_key 的部分唯一索引只约束未 resolved 的行，因此同一
// key 的历史 resolved 行可能有多条，这里按 resolved_at 倒序返回全部匹配。
func (r *FaultRepo) ListRecentlyResolvedByCorrelationKey(ctx context.Context, tx pgx.Tx, correlationKey string, since time.Time) ([]domain.Fault, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+faultColumns+` FROM faults
		WHERE correlation_key = $1 AND status = 'resolved' AND resolved_at >= $2
		ORDER BY resolved_at DESC
	`, correlationKey, since)
	if err != nil {
		return nil, fmt.Errorf("store: 查询近期 resolved Fault 失败: %w", err)
	}
	defer rows.Close()
	var result []domain.Fault
	for rows.Next() {
		item, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *FaultRepo) ListEvidence(ctx context.Context, tx pgx.Tx, faultID string) ([]domain.FaultEvidence, error) {
	rows, err := tx.Query(ctx, `
		SELECT fault_id::text, alert_id::text, role, rationale, confidence,
		       decision_origin, COALESCE(decided_by::text, ''), decided_at,
		       created_at, updated_at
		FROM fault_alert_links WHERE fault_id = $1
		ORDER BY CASE role
			WHEN 'direct_impact' THEN 1 WHEN 'cause_candidate' THEN 2
			WHEN 'trigger' THEN 3 WHEN 'amplifier' THEN 4
			WHEN 'symptom' THEN 5 WHEN 'supporting' THEN 6 ELSE 7 END,
			created_at
	`, faultID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Fault 证据失败: %w", err)
	}
	defer rows.Close()
	var result []domain.FaultEvidence
	for rows.Next() {
		var item domain.FaultEvidence
		var decidedAt *time.Time
		if err := rows.Scan(&item.FaultID, &item.AlertID, &item.Role, &item.Rationale,
			&item.Confidence, &item.DecisionOrigin, &item.DecidedBy, &decidedAt,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 Fault 证据失败: %w", err)
		}
		if decidedAt != nil {
			item.DecidedAt = *decidedAt
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// UpsertHumanEvidence 让人工裁决覆盖自动角色，并记录操作者。后续自动重算通过
// ReplaceAutomatedEvidence 的 WHERE decision_origin <> 'human' 保护，不能覆盖此行。
func (r *FaultRepo) UpsertHumanEvidence(ctx context.Context, tx pgx.Tx, evidence domain.FaultEvidence) (domain.FaultEvidence, error) {
	if evidence.FaultID == "" || evidence.AlertID == "" || !evidence.Role.IsValid() ||
		strings.TrimSpace(evidence.Rationale) == "" || evidence.Confidence < 0 || evidence.Confidence > 1 ||
		evidence.DecidedBy == "" || evidence.DecidedAt.IsZero() {
		return domain.FaultEvidence{}, fmt.Errorf("store: 人工 Fault 证据裁决字段非法")
	}
	var stored domain.FaultEvidence
	var decidedAt *time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO fault_alert_links (
			fault_id, alert_id, role, rationale, confidence, decision_origin,
			decided_by, decided_at
		) VALUES ($1, $2, $3, $4, $5, 'human', $6, $7)
		ON CONFLICT (fault_id, alert_id) DO UPDATE SET
			role = EXCLUDED.role,
			rationale = EXCLUDED.rationale,
			confidence = EXCLUDED.confidence,
			decision_origin = 'human',
			decided_by = EXCLUDED.decided_by,
			decided_at = EXCLUDED.decided_at
		RETURNING fault_id::text, alert_id::text, role, rationale, confidence,
		          decision_origin, decided_by::text, decided_at, created_at, updated_at
	`, evidence.FaultID, evidence.AlertID, evidence.Role, evidence.Rationale,
		evidence.Confidence, evidence.DecidedBy, evidence.DecidedAt).Scan(
		&stored.FaultID, &stored.AlertID, &stored.Role, &stored.Rationale,
		&stored.Confidence, &stored.DecisionOrigin, &stored.DecidedBy, &decidedAt,
		&stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return stored, ErrFaultNotFound
	}
	if err != nil {
		return stored, fmt.Errorf("store: 保存人工 Fault 证据裁决失败: %w", err)
	}
	if decidedAt != nil {
		stored.DecidedAt = *decidedAt
	}
	return stored, nil
}

func (r *FaultRepo) UpsertRelation(ctx context.Context, tx pgx.Tx, relation domain.FaultRelation) error {
	if relation.FromFaultID == "" || relation.ToFaultID == "" || relation.FromFaultID == relation.ToFaultID ||
		relation.Type != domain.FaultRelationContributesTo || relation.Confidence < 0 || relation.Confidence > 1 {
		return fmt.Errorf("store: Fault 关系字段非法")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fault_relations (from_fault_id, to_fault_id, relation, rationale, confidence)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (from_fault_id, to_fault_id, relation) DO UPDATE SET
			rationale = EXCLUDED.rationale,
			confidence = EXCLUDED.confidence
	`, relation.FromFaultID, relation.ToFaultID, relation.Type, relation.Rationale, relation.Confidence)
	if err != nil {
		return fmt.Errorf("store: upsert Fault 关系失败: %w", err)
	}
	return nil
}

func (r *FaultRepo) ReplaceAutomatedImpacts(ctx context.Context, tx pgx.Tx, faultID string, impacts []domain.FaultImpact) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fault_impacts WHERE fault_id = $1 AND decision_origin = 'automation'`, faultID); err != nil {
		return fmt.Errorf("store: 清理自动爆炸半径失败: %w", err)
	}
	for _, impact := range impacts {
		if impact.NodeID == "" || !impact.State.IsValid() ||
			impact.Confidence < 0 || impact.Confidence > 1 {
			return fmt.Errorf("store: Fault impact 字段非法")
		}
		path, err := json.Marshal(impact.Path)
		if err != nil {
			return fmt.Errorf("store: 序列化 impact path 失败: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fault_impacts (
				fault_id, node_id, state, path, rationale, confidence, decision_origin
			) VALUES ($1, $2, $3, $4, $5, $6, 'automation')
			ON CONFLICT (fault_id, node_id) DO UPDATE SET
				state = EXCLUDED.state,
				path = EXCLUDED.path,
				rationale = EXCLUDED.rationale,
				confidence = EXCLUDED.confidence,
				decision_origin = EXCLUDED.decision_origin
			WHERE fault_impacts.decision_origin <> 'human'
		`, faultID, impact.NodeID, impact.State, path, impact.Rationale, impact.Confidence); err != nil {
			return fmt.Errorf("store: 写入 Fault impact 失败: %w", err)
		}
	}
	return nil
}

// UpsertAutomatedImpact 写入一个明确的应用影响，不替换同 Fault 的其他爆炸半径，
// 也不覆盖人工 confirmed/ruled_out 裁决。
func (r *FaultRepo) UpsertAutomatedImpact(ctx context.Context, tx pgx.Tx, faultID string, impact domain.FaultImpact) error {
	if impact.NodeID == "" || !impact.State.IsValid() || impact.Confidence < 0 || impact.Confidence > 1 {
		return fmt.Errorf("store: Fault impact 字段非法")
	}
	path, err := json.Marshal(impact.Path)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fault_impacts (
			fault_id, node_id, state, path, rationale, confidence, decision_origin
		) VALUES ($1, $2, $3, $4, $5, $6, 'automation')
		ON CONFLICT (fault_id, node_id) DO UPDATE SET
			state = EXCLUDED.state, path = EXCLUDED.path, rationale = EXCLUDED.rationale,
			confidence = EXCLUDED.confidence, decision_origin = 'automation'
		WHERE fault_impacts.decision_origin <> 'human'
	`, faultID, impact.NodeID, impact.State, path, impact.Rationale, impact.Confidence)
	return err
}

func (r *FaultRepo) UpsertHumanImpact(ctx context.Context, tx pgx.Tx, impact domain.FaultImpact) (domain.FaultImpact, error) {
	if impact.FaultID == "" || impact.NodeID == "" || !impact.State.IsValid() ||
		impact.Rationale == "" || impact.Confidence < 0 || impact.Confidence > 1 ||
		impact.DecisionOrigin != domain.DecisionOriginHuman || impact.DecidedBy == "" || impact.DecidedAt.IsZero() {
		return domain.FaultImpact{}, fmt.Errorf("store: 人工 Fault impact 字段非法")
	}
	var stored domain.FaultImpact
	var pathRaw []byte
	var decidedAt *time.Time
	err := tx.QueryRow(ctx, `
		UPDATE fault_impacts SET
			state = $3,
			rationale = $4,
			confidence = $5,
			decision_origin = 'human',
			decided_by = $6,
			decided_at = $7
		WHERE fault_id = $1 AND node_id = $2
		RETURNING fault_id::text, node_id::text, state, path, rationale, confidence,
		          decision_origin, COALESCE(decided_by::text, ''), decided_at,
		          created_at, updated_at
	`, impact.FaultID, impact.NodeID, impact.State, impact.Rationale,
		impact.Confidence, impact.DecidedBy, impact.DecidedAt).Scan(
		&stored.FaultID, &stored.NodeID, &stored.State, &pathRaw, &stored.Rationale,
		&stored.Confidence, &stored.DecisionOrigin, &stored.DecidedBy, &decidedAt,
		&stored.CreatedAt, &stored.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FaultImpact{}, ErrFaultImpactNotFound
	}
	if err != nil {
		return domain.FaultImpact{}, fmt.Errorf("store: 保存人工 Fault impact 裁决失败: %w", err)
	}
	if err := json.Unmarshal(pathRaw, &stored.Path); err != nil {
		return domain.FaultImpact{}, fmt.Errorf("store: 反序列化人工 impact path 失败: %w", err)
	}
	if decidedAt != nil {
		stored.DecidedAt = *decidedAt
	}
	return stored, nil
}

func (r *FaultRepo) ListImpacts(ctx context.Context, tx pgx.Tx, faultID string) ([]domain.FaultImpact, error) {
	rows, err := tx.Query(ctx, `
		SELECT fault_id::text, node_id::text, state, path, rationale, confidence,
		       decision_origin, COALESCE(decided_by::text, ''), decided_at,
		       created_at, updated_at
		FROM fault_impacts WHERE fault_id = $1
		ORDER BY CASE state WHEN 'confirmed' THEN 1 WHEN 'potential' THEN 2 ELSE 3 END, created_at
	`, faultID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Fault impact 失败: %w", err)
	}
	defer rows.Close()
	var result []domain.FaultImpact
	for rows.Next() {
		var item domain.FaultImpact
		var pathRaw []byte
		var decidedAt *time.Time
		if err := rows.Scan(&item.FaultID, &item.NodeID, &item.State, &pathRaw,
			&item.Rationale, &item.Confidence, &item.DecisionOrigin, &item.DecidedBy,
			&decidedAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 Fault impact 失败: %w", err)
		}
		if err := json.Unmarshal(pathRaw, &item.Path); err != nil {
			return nil, fmt.Errorf("store: 反序列化 impact path 失败: %w", err)
		}
		if decidedAt != nil {
			item.DecidedAt = *decidedAt
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
