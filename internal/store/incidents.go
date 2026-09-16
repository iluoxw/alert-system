package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/domain"
)

type IncidentRepo struct{}

func NewIncidentRepo() *IncidentRepo { return &IncidentRepo{} }

var (
	ErrIncidentNotFound          = errors.New("store: Incident 不存在")
	ErrIncidentForFaultExists    = errors.New("store: Fault 已绑定 Incident")
	ErrIncidentFaultLinkNotFound = errors.New("store: Incident Fault 关系不存在")
	ErrIncidentTaskNotFound      = errors.New("store: Incident 任务不存在")
)

const incidentColumns = `
	id, number, status, severity, title, summary, declared_by::text,
	communication_channel, communication_target_ref, declared_at,
	mitigated_at, monitoring_at, resolved_at, created_at, updated_at
`

type incidentRowScanner interface{ Scan(dest ...any) error }

func scanIncident(row incidentRowScanner) (domain.Incident, error) {
	var incident domain.Incident
	var mitigatedAt, monitoringAt, resolvedAt *time.Time
	if err := row.Scan(
		&incident.ID, &incident.Number, &incident.Status, &incident.Severity,
		&incident.Title, &incident.Summary, &incident.DeclaredBy,
		&incident.CommunicationChannel, &incident.CommunicationTargetRef,
		&incident.DeclaredAt, &mitigatedAt, &monitoringAt, &resolvedAt,
		&incident.CreatedAt, &incident.UpdatedAt,
	); err != nil {
		return domain.Incident{}, err
	}
	if mitigatedAt != nil {
		incident.MitigatedAt = *mitigatedAt
	}
	if monitoringAt != nil {
		incident.MonitoringAt = *monitoringAt
	}
	if resolvedAt != nil {
		incident.ResolvedAt = *resolvedAt
	}
	return incident, nil
}

func (r *IncidentRepo) Create(ctx context.Context, tx pgx.Tx, incident domain.Incident) (domain.Incident, error) {
	if incident.Status != domain.IncidentStatusDeclared || !incident.Severity.IsValid() ||
		incident.Title == "" || incident.DeclaredBy == "" || incident.DeclaredAt.IsZero() ||
		incident.CommunicationChannel == "" || incident.CommunicationTargetRef == "" {
		return domain.Incident{}, fmt.Errorf("store: Incident 字段非法")
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO incidents (
			status, severity, title, summary, declared_by,
			communication_channel, communication_target_ref, declared_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+incidentColumns,
		incident.Status, incident.Severity, incident.Title, incident.Summary,
		incident.DeclaredBy, incident.CommunicationChannel, incident.CommunicationTargetRef,
		incident.DeclaredAt)
	created, err := scanIncident(row)
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: 创建 Incident 失败: %w", err)
	}
	return created, nil
}

func (r *IncidentRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Incident, error) {
	incident, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Incident{}, ErrIncidentNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: 查询 Incident 失败: %w", err)
	}
	return incident, nil
}

func (r *IncidentRepo) GetByIDForUpdate(ctx context.Context, tx pgx.Tx, id string) (domain.Incident, error) {
	incident, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Incident{}, ErrIncidentNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: 锁定 Incident 失败: %w", err)
	}
	return incident, nil
}

func (r *IncidentRepo) GetByFaultID(ctx context.Context, tx pgx.Tx, faultID string) (domain.Incident, error) {
	incident, err := scanIncident(tx.QueryRow(ctx, `
		SELECT `+qualifiedIncidentColumns("i")+`
		FROM incidents i
		JOIN incident_fault_links l ON l.incident_id = i.id
		WHERE l.fault_id = $1
	`, faultID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Incident{}, ErrIncidentNotFound
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: 按 Fault 查询 Incident 失败: %w", err)
	}
	return incident, nil
}

func qualifiedIncidentColumns(alias string) string {
	return alias + `.id, ` + alias + `.number, ` + alias + `.status, ` + alias + `.severity, ` +
		alias + `.title, ` + alias + `.summary, ` + alias + `.declared_by::text, ` +
		alias + `.communication_channel, ` + alias + `.communication_target_ref, ` +
		alias + `.declared_at, ` + alias + `.mitigated_at, ` + alias + `.monitoring_at, ` +
		alias + `.resolved_at, ` + alias + `.created_at, ` + alias + `.updated_at`
}

func (r *IncidentRepo) List(ctx context.Context, tx pgx.Tx, status domain.IncidentStatus, limit int) ([]domain.Incident, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT ` + incidentColumns + ` FROM incidents`
	args := []any{}
	if status != "" {
		query += ` WHERE status = $1`
		args = append(args, status)
	}
	query += fmt.Sprintf(` ORDER BY declared_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Incident 列表失败: %w", err)
	}
	defer rows.Close()
	incidents := make([]domain.Incident, 0)
	for rows.Next() {
		incident, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 Incident 失败: %w", err)
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}

func (r *IncidentRepo) LinkFault(ctx context.Context, tx pgx.Tx, incidentID, faultID string, relation domain.IncidentFaultRelation, at time.Time) (domain.IncidentFaultLink, error) {
	if !relation.IsValid() || incidentID == "" || faultID == "" || at.IsZero() {
		return domain.IncidentFaultLink{}, fmt.Errorf("store: Incident Fault 关系字段非法")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO incident_fault_links (incident_id, fault_id, relation, linked_at)
		VALUES ($1, $2, $3, $4)
	`, incidentID, faultID, relation, at)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.IncidentFaultLink{}, ErrIncidentForFaultExists
		}
		return domain.IncidentFaultLink{}, fmt.Errorf("store: 绑定 Incident Fault 失败: %w", err)
	}
	return r.GetFaultLink(ctx, tx, incidentID, faultID)
}

func scanIncidentFaultLink(row incidentRowScanner) (domain.IncidentFaultLink, error) {
	var link domain.IncidentFaultLink
	var decidedAt *time.Time
	if err := row.Scan(
		&link.IncidentID, &link.FaultID, &link.Relation, &link.Disposition,
		&link.DispositionRationale, &link.DecidedBy, &decidedAt, &link.LinkedAt,
	); err != nil {
		return domain.IncidentFaultLink{}, err
	}
	if decidedAt != nil {
		link.DecidedAt = *decidedAt
	}
	return link, nil
}

const incidentFaultLinkColumns = `
	incident_id::text, fault_id::text, relation, disposition,
	disposition_rationale, COALESCE(decided_by::text, ''), decided_at, linked_at
`

func (r *IncidentRepo) GetFaultLink(ctx context.Context, tx pgx.Tx, incidentID, faultID string) (domain.IncidentFaultLink, error) {
	link, err := scanIncidentFaultLink(tx.QueryRow(ctx, `
		SELECT `+incidentFaultLinkColumns+`
		FROM incident_fault_links WHERE incident_id = $1 AND fault_id = $2
	`, incidentID, faultID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IncidentFaultLink{}, ErrIncidentFaultLinkNotFound
	}
	if err != nil {
		return domain.IncidentFaultLink{}, fmt.Errorf("store: 查询 Incident Fault 关系失败: %w", err)
	}
	return link, nil
}

func (r *IncidentRepo) ListFaultLinks(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.IncidentFaultLink, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+incidentFaultLinkColumns+`
		FROM incident_fault_links WHERE incident_id = $1
		ORDER BY CASE relation WHEN 'primary' THEN 1 ELSE 2 END, linked_at
	`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Incident Fault 关系失败: %w", err)
	}
	defer rows.Close()
	result := make([]domain.IncidentFaultLink, 0)
	for rows.Next() {
		link, err := scanIncidentFaultLink(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, link)
	}
	return result, rows.Err()
}

// SetPrimaryFault 要求调用方先锁定 Incident 行。先降级旧 primary、再提升目标，
// 与部分唯一索引共同保证任意时刻每个 Incident 至多一个 primary。
func (r *IncidentRepo) SetPrimaryFault(ctx context.Context, tx pgx.Tx, incidentID, faultID string) (domain.IncidentFaultLink, string, error) {
	target, err := r.GetFaultLink(ctx, tx, incidentID, faultID)
	if err != nil {
		return domain.IncidentFaultLink{}, "", err
	}
	var previous string
	err = tx.QueryRow(ctx, `
		SELECT fault_id::text FROM incident_fault_links
		WHERE incident_id = $1 AND relation = 'primary'
	`, incidentID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		previous = ""
	} else if err != nil {
		return domain.IncidentFaultLink{}, "", fmt.Errorf("store: 查询 primary Fault 失败: %w", err)
	}
	if previous == faultID {
		return target, previous, nil
	}
	if previous != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE incident_fault_links SET relation = 'supporting'
			WHERE incident_id = $1 AND fault_id = $2 AND relation = 'primary'
		`, incidentID, previous); err != nil {
			return domain.IncidentFaultLink{}, "", fmt.Errorf("store: 降级旧 primary Fault 失败: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE incident_fault_links SET relation = 'primary'
		WHERE incident_id = $1 AND fault_id = $2
	`, incidentID, faultID); err != nil {
		return domain.IncidentFaultLink{}, "", fmt.Errorf("store: 设置 primary Fault 失败: %w", err)
	}
	updated, err := r.GetFaultLink(ctx, tx, incidentID, faultID)
	return updated, previous, err
}

func (r *IncidentRepo) UpdateFaultDisposition(ctx context.Context, tx pgx.Tx, incidentID, faultID string, disposition domain.IncidentFaultDisposition, rationale, actorID string, at time.Time) (domain.IncidentFaultLink, error) {
	if !disposition.IsValid() || rationale == "" || actorID == "" || at.IsZero() {
		return domain.IncidentFaultLink{}, fmt.Errorf("store: Incident Fault 裁决字段非法")
	}
	link, err := scanIncidentFaultLink(tx.QueryRow(ctx, `
		UPDATE incident_fault_links SET
			disposition = $3,
			disposition_rationale = $4,
			decided_by = $5,
			decided_at = $6
		WHERE incident_id = $1 AND fault_id = $2
		RETURNING `+incidentFaultLinkColumns,
		incidentID, faultID, disposition, rationale, actorID, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IncidentFaultLink{}, ErrIncidentFaultLinkNotFound
	}
	if err != nil {
		return domain.IncidentFaultLink{}, fmt.Errorf("store: 更新 Incident Fault 裁决失败: %w", err)
	}
	return link, nil
}

func (r *IncidentRepo) UpsertRole(ctx context.Context, tx pgx.Tx, assignment domain.IncidentRoleAssignment) (domain.IncidentRoleAssignment, error) {
	if !assignment.Role.IsValid() || assignment.UserID == "" || assignment.AssignedBy == "" || assignment.AssignedAt.IsZero() {
		return domain.IncidentRoleAssignment{}, fmt.Errorf("store: Incident 角色字段非法")
	}
	var result domain.IncidentRoleAssignment
	err := tx.QueryRow(ctx, `
		INSERT INTO incident_roles (incident_id, role, user_id, assigned_by, assigned_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (incident_id, role) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			assigned_by = EXCLUDED.assigned_by,
			assigned_at = EXCLUDED.assigned_at
		RETURNING incident_id::text, role, user_id::text, assigned_by::text, assigned_at
	`, assignment.IncidentID, assignment.Role, assignment.UserID, assignment.AssignedBy, assignment.AssignedAt).Scan(
		&result.IncidentID, &result.Role, &result.UserID, &result.AssignedBy, &result.AssignedAt)
	if err != nil {
		return domain.IncidentRoleAssignment{}, fmt.Errorf("store: 分配 Incident 角色失败: %w", err)
	}
	return result, nil
}

func (r *IncidentRepo) ListRoles(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.IncidentRoleAssignment, error) {
	rows, err := tx.Query(ctx, `
		SELECT incident_id::text, role, user_id::text, assigned_by::text, assigned_at
		FROM incident_roles WHERE incident_id = $1 ORDER BY role
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.IncidentRoleAssignment, 0)
	for rows.Next() {
		var item domain.IncidentRoleAssignment
		if err := rows.Scan(&item.IncidentID, &item.Role, &item.UserID, &item.AssignedBy, &item.AssignedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *IncidentRepo) CreateTask(ctx context.Context, tx pgx.Tx, task domain.IncidentTask) (domain.IncidentTask, error) {
	if task.IncidentID == "" || task.Title == "" || task.CreatedBy == "" {
		return domain.IncidentTask{}, fmt.Errorf("store: Incident 任务字段非法")
	}
	if task.Status == "" {
		task.Status = domain.IncidentTaskOpen
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO incident_tasks (
			incident_id, title, description, status, owner_user_id, created_by, due_at
		) VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7)
		RETURNING id::text, incident_id::text, title, description, status,
		          COALESCE(owner_user_id::text, ''), created_by::text, due_at,
		          completed_at, created_at, updated_at
	`, task.IncidentID, task.Title, task.Description, task.Status, task.OwnerUserID,
		task.CreatedBy, nullableTime(task.DueAt))
	created, err := scanIncidentTask(row)
	if err != nil {
		return domain.IncidentTask{}, fmt.Errorf("store: 创建 Incident 任务失败: %w", err)
	}
	return created, nil
}

func scanIncidentTask(row incidentRowScanner) (domain.IncidentTask, error) {
	var task domain.IncidentTask
	var dueAt, completedAt *time.Time
	if err := row.Scan(&task.ID, &task.IncidentID, &task.Title, &task.Description,
		&task.Status, &task.OwnerUserID, &task.CreatedBy, &dueAt, &completedAt,
		&task.CreatedAt, &task.UpdatedAt); err != nil {
		return domain.IncidentTask{}, err
	}
	if dueAt != nil {
		task.DueAt = *dueAt
	}
	if completedAt != nil {
		task.CompletedAt = *completedAt
	}
	return task, nil
}

func (r *IncidentRepo) UpdateTask(ctx context.Context, tx pgx.Tx, incidentID, taskID string, status domain.IncidentTaskStatus, ownerUserID string, at time.Time) (domain.IncidentTask, error) {
	if !status.IsValid() {
		return domain.IncidentTask{}, fmt.Errorf("store: Incident 任务状态非法")
	}
	row := tx.QueryRow(ctx, `
		UPDATE incident_tasks SET
			status = $3,
			owner_user_id = CASE WHEN $4 = '' THEN owner_user_id ELSE $4::uuid END,
			completed_at = CASE WHEN $3 = 'done' THEN $5::timestamptz ELSE NULL END
		WHERE id = $2 AND incident_id = $1
		RETURNING id::text, incident_id::text, title, description, status,
		          COALESCE(owner_user_id::text, ''), created_by::text, due_at,
		          completed_at, created_at, updated_at
	`, incidentID, taskID, status, ownerUserID, at)
	updated, err := scanIncidentTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IncidentTask{}, ErrIncidentTaskNotFound
	}
	if err != nil {
		return domain.IncidentTask{}, fmt.Errorf("store: 更新 Incident 任务失败: %w", err)
	}
	return updated, nil
}

func (r *IncidentRepo) ListTasks(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.IncidentTask, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, incident_id::text, title, description, status,
		       COALESCE(owner_user_id::text, ''), created_by::text, due_at,
		       completed_at, created_at, updated_at
		FROM incident_tasks WHERE incident_id = $1 ORDER BY created_at
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.IncidentTask, 0)
	for rows.Next() {
		item, err := scanIncidentTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// AppendIncidentTimeline 是 append-only 写入点，同时满足 notify.Sender 的 Incident
// 通信结果审计接口。DedupeKey 非空时重复追加返回既有行 ID。
func (r *IncidentRepo) AppendIncidentTimeline(ctx context.Context, tx pgx.Tx, entry domain.IncidentTimelineEntry) (string, error) {
	if entry.IncidentID == "" || entry.ActorDisplay == "" || !entry.Kind.IsValid() || entry.Summary == "" || entry.OccurredAt.IsZero() {
		return "", fmt.Errorf("store: Incident 时间线字段非法")
	}
	detail, err := marshalJSONObject(entry.Detail)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO incident_timeline_entries (
			incident_id, actor_user_id, actor_display, kind, summary,
			detail, dedupe_key, occurred_at
		) VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, NULLIF($7, ''), $8)
		ON CONFLICT (incident_id, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING id::text
	`, entry.IncidentID, entry.ActorUserID, entry.ActorDisplay, entry.Kind,
		entry.Summary, detail, entry.DedupeKey, entry.OccurredAt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) && entry.DedupeKey != "" {
		if err := tx.QueryRow(ctx, `
			SELECT id::text FROM incident_timeline_entries
			WHERE incident_id = $1 AND dedupe_key = $2
		`, entry.IncidentID, entry.DedupeKey).Scan(&id); err != nil {
			return "", fmt.Errorf("store: 查询重复 Incident 时间线失败: %w", err)
		}
		return id, nil
	}
	if err != nil {
		return "", fmt.Errorf("store: 写入 Incident 时间线失败: %w", err)
	}
	return id, nil
}

func (r *IncidentRepo) ListTimeline(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.IncidentTimelineEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, incident_id::text, COALESCE(actor_user_id::text, ''),
		       actor_display, kind, summary, detail, COALESCE(dedupe_key, ''), occurred_at
		FROM incident_timeline_entries WHERE incident_id = $1
		ORDER BY occurred_at, created_at
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.IncidentTimelineEntry, 0)
	for rows.Next() {
		var item domain.IncidentTimelineEntry
		var detailRaw []byte
		if err := rows.Scan(&item.ID, &item.IncidentID, &item.ActorUserID, &item.ActorDisplay,
			&item.Kind, &item.Summary, &detailRaw, &item.DedupeKey, &item.OccurredAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detailRaw, &item.Detail); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *IncidentRepo) CreateRecoveryCriterion(ctx context.Context, tx pgx.Tx, criterion domain.RecoveryCriterion) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO recovery_criteria (
			incident_id, key, description, gate, required, status, evidence
		) VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb)
	`, criterion.IncidentID, criterion.Key, criterion.Description, criterion.Gate,
		criterion.Required, criterion.Status)
	if err != nil {
		return fmt.Errorf("store: 创建恢复判据失败: %w", err)
	}
	return nil
}

func (r *IncidentRepo) UpdateRecoveryCriterion(ctx context.Context, tx pgx.Tx, incidentID, key string, status domain.RecoveryCriterionStatus, evidence map[string]any, evaluatedBy string, observedAt time.Time) (domain.RecoveryCriterion, error) {
	if !status.IsValid() || (status == domain.RecoveryCriterionPassed && (len(evidence) == 0 || observedAt.IsZero())) {
		return domain.RecoveryCriterion{}, fmt.Errorf("store: 恢复判据证据不完整")
	}
	evidenceJSON, err := marshalJSONObject(evidence)
	if err != nil {
		return domain.RecoveryCriterion{}, err
	}
	row := tx.QueryRow(ctx, `
		UPDATE recovery_criteria SET
			status = $3, evidence = $4, evaluated_by = NULLIF($5, '')::uuid, observed_at = $6
		WHERE incident_id = $1 AND key = $2
		RETURNING id::text, incident_id::text, key, description, gate, required,
		          status, evidence, COALESCE(evaluated_by::text, ''), observed_at,
		          created_at, updated_at
	`, incidentID, key, status, evidenceJSON, evaluatedBy, nullableTime(observedAt))
	criterion, err := scanRecoveryCriterion(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RecoveryCriterion{}, ErrNotFound
	}
	if err != nil {
		return domain.RecoveryCriterion{}, fmt.Errorf("store: 更新恢复判据失败: %w", err)
	}
	return criterion, nil
}

func scanRecoveryCriterion(row incidentRowScanner) (domain.RecoveryCriterion, error) {
	var item domain.RecoveryCriterion
	var evidenceRaw []byte
	var observedAt *time.Time
	if err := row.Scan(&item.ID, &item.IncidentID, &item.Key, &item.Description,
		&item.Gate, &item.Required, &item.Status, &evidenceRaw, &item.EvaluatedBy,
		&observedAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return domain.RecoveryCriterion{}, err
	}
	if observedAt != nil {
		item.ObservedAt = *observedAt
	}
	if err := json.Unmarshal(evidenceRaw, &item.Evidence); err != nil {
		return domain.RecoveryCriterion{}, err
	}
	return item, nil
}

func (r *IncidentRepo) ListRecoveryCriteria(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.RecoveryCriterion, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, incident_id::text, key, description, gate, required,
		       status, evidence, COALESCE(evaluated_by::text, ''), observed_at,
		       created_at, updated_at
		FROM recovery_criteria WHERE incident_id = $1
		ORDER BY CASE gate WHEN 'mitigation' THEN 1 ELSE 2 END, created_at
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.RecoveryCriterion, 0)
	for rows.Next() {
		item, err := scanRecoveryCriterion(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *IncidentRepo) UpdateStatus(ctx context.Context, tx pgx.Tx, id string, from, to domain.IncidentStatus, at time.Time) (domain.Incident, error) {
	incident, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE incidents SET
			status = $3,
			mitigated_at = CASE WHEN $3 IN ('mitigating', 'monitoring') AND mitigated_at IS NULL THEN $4 ELSE mitigated_at END,
			monitoring_at = CASE WHEN $3 = 'monitoring' THEN $4 ELSE monitoring_at END,
			resolved_at = CASE WHEN $3 = 'resolved' THEN $4 ELSE resolved_at END
		WHERE id = $1 AND status = $2
		RETURNING `+incidentColumns,
		id, from, to, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Incident{}, fmt.Errorf("%w: 并发状态已变化", domain.ErrInvalidIncidentTransition)
	}
	if err != nil {
		return domain.Incident{}, fmt.Errorf("store: 更新 Incident 状态失败: %w", err)
	}
	return incident, nil
}

// ListOpenIDs 返回所有仍处于 open（非 resolved）状态的 Incident ID，供
// incident_recovery_nudge 定时器的启动对账使用。不分页——Incident 数量级远小于
// 告警，与 ops.ListEnabledSettings 同样不做分页。
func (r *IncidentRepo) ListOpenIDs(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM incidents WHERE status <> 'resolved'`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 open Incident 列表失败: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: 扫描 open Incident ID 失败: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *IncidentRepo) Detail(ctx context.Context, tx pgx.Tx, id string) (domain.IncidentDetail, error) {
	incident, err := r.GetByID(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	faults, err := r.ListFaultLinks(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	roles, err := r.ListRoles(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	tasks, err := r.ListTasks(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	timeline, err := r.ListTimeline(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	recovery, err := r.ListRecoveryCriteria(ctx, tx, id)
	if err != nil {
		return domain.IncidentDetail{}, err
	}
	return domain.IncidentDetail{
		Incident: incident, Faults: faults, Roles: roles, Tasks: tasks,
		Timeline: timeline, Recovery: recovery,
	}, nil
}
