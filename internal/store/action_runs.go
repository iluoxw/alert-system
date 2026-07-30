package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ActionRun 对应 action_runs 表：自愈规则的触发历史（R12.2/R12.4）。fingerprint
// 与 alerts.fingerprint 冗余存储——R3.4 意味着同一 fingerprint 可能横跨多个 alert_id
// （resolve 后重新 firing 视为新 alert 行），冷却窗口检查（R12.4）必须按 fingerprint
// 聚合，而不是单个 alert_id，所以这里单独落一列，查询不依赖 join。
type ActionRun struct {
	ID          string
	RuleID      string
	AlertID     string
	Fingerprint string
	TriggeredAt time.Time
	Status      string // triggered|succeeded|failed|skipped_cooldown
	Result      json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// action_runs.status 的取值常量（迁移 000004 增补了 skipped_cooldown）。
const (
	ActionRunStatusTriggered       = "triggered"
	ActionRunStatusSucceeded       = "succeeded"
	ActionRunStatusFailed          = "failed"
	ActionRunStatusSkippedCooldown = "skipped_cooldown"
)

const actionRunColumns = `id, rule_id, alert_id, fingerprint, triggered_at, status, result, created_at, updated_at`

func scanActionRun(row configRowScanner) (ActionRun, error) {
	var a ActionRun
	err := row.Scan(&a.ID, &a.RuleID, &a.AlertID, &a.Fingerprint, &a.TriggeredAt, &a.Status, &a.Result, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// ActionRunRepo 是 action_runs 表的仓储实现，约定同 TeamRepo（config_repos.go 头部注释）。
type ActionRunRepo struct{}

func NewActionRunRepo() *ActionRunRepo { return &ActionRunRepo{} }

// Insert 写入一条动作触发记录：触发成功、失败或因冷却窗口被跳过均落一条，status
// 由调用方指定（ActionRunStatus* 常量）。
func (r *ActionRunRepo) Insert(ctx context.Context, tx pgx.Tx, a ActionRun) (*ActionRun, error) {
	result := a.Result
	if result == nil {
		result = json.RawMessage(`{}`)
	}
	status := a.Status
	if status == "" {
		status = ActionRunStatusTriggered
	}
	out, err := scanActionRun(tx.QueryRow(ctx, `
		INSERT INTO action_runs (rule_id, alert_id, fingerprint, status, result)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+actionRunColumns,
		a.RuleID, a.AlertID, a.Fingerprint, status, result))
	if err != nil {
		return nil, fmt.Errorf("store: 写入动作触发记录失败: %w", err)
	}
	return &out, nil
}

// LastTriggered 返回 rule_id + fingerprint 组合最近一次「确实执行过」的记录
// （status 为 triggered/succeeded/failed，不含 skipped_cooldown）；不存在则 ok=false。
//
// 冷却窗口检查（R12.4）故意排除 skipped_cooldown 记录：如果以「最近一条记录」（含
// skipped）为基准，那么每次因冷却被拒绝的调用都会产生新记录，而这条新记录本身又
// 会把冷却窗口的起点往后推——告警持续 firing 时冷却窗口会无限自我延长，永远无法
// 恢复触发，这与 R12.4「窗口内不重复触发、窗口外应当恢复」的语义相悖。
func (r *ActionRunRepo) LastTriggered(ctx context.Context, tx pgx.Tx, ruleID, fingerprint string) (ActionRun, bool, error) {
	out, err := scanActionRun(tx.QueryRow(ctx, `
		SELECT `+actionRunColumns+`
		FROM action_runs
		WHERE rule_id = $1 AND fingerprint = $2 AND status != $3
		ORDER BY triggered_at DESC
		LIMIT 1
	`, ruleID, fingerprint, ActionRunStatusSkippedCooldown))
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionRun{}, false, nil
	}
	if err != nil {
		return ActionRun{}, false, fmt.Errorf("store: 查询最近动作触发记录失败: %w", err)
	}
	return out, true, nil
}
