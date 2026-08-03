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

var (
	ErrRunbookNotFound       = errors.New("store: Runbook 不存在")
	ErrRunbookRunNotFound    = errors.New("store: Runbook Run 不存在")
	ErrRunbookApplyInFlight  = errors.New("store: 该计划已有执行中或成功的 apply")
	ErrRunbookApprovalAbsent = errors.New("store: Runbook 审批不存在")
)

type RunbookRepo struct{}

func NewRunbookRepo() *RunbookRepo { return &RunbookRepo{} }

func (r *RunbookRepo) ListDefinitions(ctx context.Context, tx pgx.Tx) ([]domain.RunbookDefinition, error) {
	rows, err := tx.Query(ctx, `
		SELECT action_key, name, description, risk_level, enabled, uat_only
		FROM runbooks ORDER BY
			CASE risk_level WHEN 'coordination' THEN 1 WHEN 'readonly' THEN 2 ELSE 3 END,
			action_key
	`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Runbook 列表失败: %w", err)
	}
	defer rows.Close()
	result := make([]domain.RunbookDefinition, 0)
	for rows.Next() {
		var item domain.RunbookDefinition
		if err := rows.Scan(&item.ActionKey, &item.Name, &item.Description, &item.RiskLevel, &item.Enabled, &item.UATOnly); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *RunbookRepo) GetDefinition(ctx context.Context, tx pgx.Tx, actionKey string) (domain.RunbookDefinition, error) {
	var item domain.RunbookDefinition
	err := tx.QueryRow(ctx, `
		SELECT action_key, name, description, risk_level, enabled, uat_only
		FROM runbooks WHERE action_key = $1
	`, actionKey).Scan(&item.ActionKey, &item.Name, &item.Description, &item.RiskLevel, &item.Enabled, &item.UATOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrRunbookNotFound
	}
	if err != nil {
		return item, fmt.Errorf("store: 查询 Runbook 失败: %w", err)
	}
	return item, nil
}

const runbookRunColumns = `
	id::text, incident_id::text, action_key, mode, status, parameters, plan,
	plan_sha256, plan_expires_at, COALESCE(source_plan_run_id::text, ''),
	requested_by::text, COALESCE(executed_by::text, ''), requested_at,
	started_at, completed_at, result, error, created_at, updated_at
`

type runbookRowScanner interface{ Scan(dest ...any) error }

func scanRunbookRun(row runbookRowScanner) (domain.RunbookRun, error) {
	var run domain.RunbookRun
	var parametersRaw, planRaw, resultRaw []byte
	var planExpiresAt, startedAt, completedAt *time.Time
	if err := row.Scan(
		&run.ID, &run.IncidentID, &run.ActionKey, &run.Mode, &run.Status,
		&parametersRaw, &planRaw, &run.PlanSHA256, &planExpiresAt, &run.SourcePlanRunID,
		&run.RequestedBy, &run.ExecutedBy, &run.RequestedAt, &startedAt, &completedAt,
		&resultRaw, &run.Error, &run.CreatedAt, &run.UpdatedAt,
	); err != nil {
		return run, err
	}
	for _, pair := range []struct {
		raw    []byte
		target *map[string]any
	}{
		{parametersRaw, &run.Parameters},
		{planRaw, &run.Plan},
		{resultRaw, &run.Result},
	} {
		if err := json.Unmarshal(pair.raw, pair.target); err != nil {
			return run, fmt.Errorf("store: 解析 Runbook JSON 失败: %w", err)
		}
	}
	if planExpiresAt != nil {
		run.PlanExpiresAt = *planExpiresAt
	}
	if startedAt != nil {
		run.StartedAt = *startedAt
	}
	if completedAt != nil {
		run.CompletedAt = *completedAt
	}
	return run, nil
}

func (r *RunbookRepo) CreateRun(ctx context.Context, tx pgx.Tx, run domain.RunbookRun) (domain.RunbookRun, error) {
	if run.IncidentID == "" || run.ActionKey == "" || run.RequestedBy == "" || run.RequestedAt.IsZero() ||
		run.Mode == "" || run.Status == "" {
		return domain.RunbookRun{}, fmt.Errorf("store: Runbook Run 字段非法")
	}
	parameters, err := marshalJSONObject(run.Parameters)
	if err != nil {
		return domain.RunbookRun{}, err
	}
	plan, err := marshalJSONObject(run.Plan)
	if err != nil {
		return domain.RunbookRun{}, err
	}
	result, err := marshalJSONObject(run.Result)
	if err != nil {
		return domain.RunbookRun{}, err
	}
	created, err := scanRunbookRun(tx.QueryRow(ctx, `
		INSERT INTO runbook_runs (
			incident_id, action_key, mode, status, parameters, plan,
			plan_sha256, plan_expires_at, source_plan_run_id, requested_by,
			executed_by, requested_at, started_at, completed_at, result, error
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, NULLIF($9, '')::uuid, $10,
			NULLIF($11, '')::uuid, $12, $13, $14, $15, $16
		)
		RETURNING `+runbookRunColumns,
		run.IncidentID, run.ActionKey, run.Mode, run.Status, parameters, plan,
		run.PlanSHA256, nullableTime(run.PlanExpiresAt), run.SourcePlanRunID,
		run.RequestedBy, run.ExecutedBy, run.RequestedAt, nullableTime(run.StartedAt),
		nullableTime(run.CompletedAt), result, run.Error))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "runbook_runs_active_apply_uidx" {
			return domain.RunbookRun{}, ErrRunbookApplyInFlight
		}
		return domain.RunbookRun{}, fmt.Errorf("store: 创建 Runbook Run 失败: %w", err)
	}
	return created, nil
}

func (r *RunbookRepo) GetRun(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (domain.RunbookRun, error) {
	query := `SELECT ` + runbookRunColumns + ` FROM runbook_runs WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	run, err := scanRunbookRun(tx.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrRunbookRunNotFound
	}
	if err != nil {
		return run, fmt.Errorf("store: 查询 Runbook Run 失败: %w", err)
	}
	return run, nil
}

func (r *RunbookRepo) CompleteRun(ctx context.Context, tx pgx.Tx, id string, status domain.RunbookRunStatus, executedBy string, plan map[string]any, planSHA string, planExpiresAt time.Time, result map[string]any, runError string, at time.Time) (domain.RunbookRun, error) {
	planJSON, err := marshalJSONObject(plan)
	if err != nil {
		return domain.RunbookRun{}, err
	}
	resultJSON, err := marshalJSONObject(result)
	if err != nil {
		return domain.RunbookRun{}, err
	}
	run, err := scanRunbookRun(tx.QueryRow(ctx, `
		UPDATE runbook_runs SET
			status = $2, executed_by = NULLIF($3, '')::uuid,
			plan = $4, plan_sha256 = $5, plan_expires_at = $6::timestamptz,
			result = $7, error = $8, completed_at = $9::timestamptz
		WHERE id = $1 AND status = 'running'
		RETURNING `+runbookRunColumns,
		id, status, executedBy, planJSON, planSHA, nullableTime(planExpiresAt),
		resultJSON, runError, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrRunbookRunNotFound
	}
	if err != nil {
		return run, fmt.Errorf("store: 完成 Runbook Run 失败: %w", err)
	}
	return run, nil
}

func (r *RunbookRepo) ListRuns(ctx context.Context, tx pgx.Tx, incidentID string) ([]domain.RunbookRun, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+runbookRunColumns+` FROM runbook_runs
		WHERE incident_id = $1 ORDER BY requested_at, created_at
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.RunbookRun, 0)
	for rows.Next() {
		item, err := scanRunbookRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *RunbookRepo) UpsertApproval(ctx context.Context, tx pgx.Tx, approval domain.RunbookApproval) (domain.RunbookApproval, error) {
	if approval.PlanRunID == "" || approval.PlanSHA256 == "" || approval.ApprovedBy == "" || approval.ApprovedAt.IsZero() {
		return domain.RunbookApproval{}, fmt.Errorf("store: Runbook 审批字段非法")
	}
	var stored domain.RunbookApproval
	err := tx.QueryRow(ctx, `
		INSERT INTO runbook_approvals (
			plan_run_id, plan_sha256, decision, approved_by, comment, approved_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (plan_run_id, approved_by) DO UPDATE SET
			plan_sha256 = EXCLUDED.plan_sha256,
			decision = EXCLUDED.decision,
			comment = EXCLUDED.comment,
			approved_at = EXCLUDED.approved_at
		RETURNING id::text, plan_run_id::text, plan_sha256, decision,
		          approved_by::text, comment, approved_at
	`, approval.PlanRunID, approval.PlanSHA256, approval.Decision, approval.ApprovedBy,
		approval.Comment, approval.ApprovedAt).Scan(
		&stored.ID, &stored.PlanRunID, &stored.PlanSHA256, &stored.Decision,
		&stored.ApprovedBy, &stored.Comment, &stored.ApprovedAt)
	if err != nil {
		return stored, fmt.Errorf("store: 保存 Runbook 审批失败: %w", err)
	}
	return stored, nil
}

func (r *RunbookRepo) LatestApproved(ctx context.Context, tx pgx.Tx, planRunID, planSHA string) (domain.RunbookApproval, error) {
	var approval domain.RunbookApproval
	err := tx.QueryRow(ctx, `
		SELECT id::text, plan_run_id::text, plan_sha256, decision,
		       approved_by::text, comment, approved_at
		FROM runbook_approvals
		WHERE plan_run_id = $1 AND plan_sha256 = $2 AND decision = 'approved'
		ORDER BY approved_at DESC LIMIT 1
	`, planRunID, planSHA).Scan(
		&approval.ID, &approval.PlanRunID, &approval.PlanSHA256, &approval.Decision,
		&approval.ApprovedBy, &approval.Comment, &approval.ApprovedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return approval, ErrRunbookApprovalAbsent
	}
	if err != nil {
		return approval, fmt.Errorf("store: 查询 Runbook 审批失败: %w", err)
	}
	return approval, nil
}
