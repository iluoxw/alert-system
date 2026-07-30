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

// EscalationLevel 是升级链中的一级：等待时长与该级的责任人/团队目标集合（R5.4）。
type EscalationLevel struct {
	WaitSeconds int      `json:"wait_seconds"`
	Targets     []string `json:"targets"`
}

// EscalationPolicy 对应 escalation_policies 表：团队的人员升级链（R5.4）。
type EscalationPolicy struct {
	ID        string
	TeamID    string
	Levels    []EscalationLevel
	CreatedAt time.Time
	UpdatedAt time.Time
}

const escalationPolicyColumns = `id, team_id, levels, created_at, updated_at`

func scanEscalationPolicy(row configRowScanner) (EscalationPolicy, error) {
	var p EscalationPolicy
	var levelsRaw []byte
	if err := row.Scan(&p.ID, &p.TeamID, &levelsRaw, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return EscalationPolicy{}, err
	}
	if err := json.Unmarshal(levelsRaw, &p.Levels); err != nil {
		return EscalationPolicy{}, fmt.Errorf("反序列化升级链 levels 失败: %w", err)
	}
	return p, nil
}

// EscalationPolicyRepo 是 escalation_policies 表的仓储实现，约定同 TeamRepo。
type EscalationPolicyRepo struct{}

func NewEscalationPolicyRepo() *EscalationPolicyRepo { return &EscalationPolicyRepo{} }

func (r *EscalationPolicyRepo) Create(ctx context.Context, tx pgx.Tx, p EscalationPolicy) (*EscalationPolicy, error) {
	levelsJSON, err := json.Marshal(p.Levels)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化升级链 levels 失败: %w", err)
	}
	out, err := scanEscalationPolicy(tx.QueryRow(ctx, `
		INSERT INTO escalation_policies (team_id, levels) VALUES ($1, $2)
		RETURNING `+escalationPolicyColumns, p.TeamID, levelsJSON))
	if err != nil {
		return nil, fmt.Errorf("store: 创建升级策略失败: %w", err)
	}
	return &out, nil
}

func (r *EscalationPolicyRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*EscalationPolicy, error) {
	out, err := scanEscalationPolicy(tx.QueryRow(ctx, `SELECT `+escalationPolicyColumns+` FROM escalation_policies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询升级策略失败: %w", err)
	}
	return &out, nil
}

// ListByTeam 返回团队下全部升级策略。
func (r *EscalationPolicyRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]EscalationPolicy, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+escalationPolicyColumns+` FROM escalation_policies WHERE team_id = $1 ORDER BY created_at LIMIT $2
	`, teamID, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出团队升级策略失败: %w", err)
	}
	defer rows.Close()

	var policies []EscalationPolicy
	for rows.Next() {
		p, err := scanEscalationPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描升级策略失败: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历升级策略失败: %w", err)
	}
	return policies, nil
}

func (r *EscalationPolicyRepo) Update(ctx context.Context, tx pgx.Tx, p EscalationPolicy) error {
	levelsJSON, err := json.Marshal(p.Levels)
	if err != nil {
		return fmt.Errorf("store: 序列化升级链 levels 失败: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE escalation_policies SET team_id = $2, levels = $3 WHERE id = $1`, p.ID, p.TeamID, levelsJSON)
	if err != nil {
		return fmt.Errorf("store: 更新升级策略失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *EscalationPolicyRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM escalation_policies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除升级策略失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NotificationStep 是渠道升级序列中的一级：等待时长、本级渠道集合与叠加模式
// （replace 替换更强渠道 / spread 叠加扩散，R13.2）。
type NotificationStep struct {
	WaitSeconds int      `json:"wait_seconds"`
	Channels    []string `json:"channels"`
	Mode        string   `json:"mode"` // replace|spread
}

// NotificationSteps 按 severity 分层的渠道升级序列（R13.1），JSON 形如
// {"warning": [...], "critical": [...]}，对应 notification_policies.steps 列。
type NotificationSteps map[domain.Severity][]NotificationStep

// NotificationPolicy 对应 notification_policies 表：按业务/service/component 绑定
// 的通知策略（R13.5）。TeamID 为空表示团队默认；TeamID 与 Matchers 都为空表示
// 全局默认（由 GetGlobalDefault 专门查询）。
type NotificationPolicy struct {
	ID        string
	Name      string
	Matchers  domain.MatcherSet
	TeamID    string // 空字符串 = 全局（team_id IS NULL）
	Steps     NotificationSteps
	CreatedAt time.Time
	UpdatedAt time.Time
}

const notificationPolicyColumns = `id, name, matchers, team_id, steps, created_at, updated_at`

func scanNotificationPolicy(row configRowScanner) (NotificationPolicy, error) {
	var p NotificationPolicy
	var matchersRaw, stepsRaw []byte
	var teamID *string
	err := row.Scan(&p.ID, &p.Name, &matchersRaw, &teamID, &stepsRaw, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return NotificationPolicy{}, err
	}
	p.TeamID = nullableString(teamID)
	if err := json.Unmarshal(matchersRaw, &p.Matchers); err != nil {
		return NotificationPolicy{}, fmt.Errorf("反序列化策略 matchers 失败: %w", err)
	}
	if err := json.Unmarshal(stepsRaw, &p.Steps); err != nil {
		return NotificationPolicy{}, fmt.Errorf("反序列化策略 steps 失败: %w", err)
	}
	return p, nil
}

// NotificationPolicyRepo 是 notification_policies 表的仓储实现，约定同 TeamRepo。
type NotificationPolicyRepo struct{}

func NewNotificationPolicyRepo() *NotificationPolicyRepo { return &NotificationPolicyRepo{} }

func (r *NotificationPolicyRepo) Create(ctx context.Context, tx pgx.Tx, p NotificationPolicy) (*NotificationPolicy, error) {
	if p.Matchers == nil {
		// nil 与 domain.MatcherSet{} 语义相同（都是"无 matcher"），但 encoding/json
		// 把 nil slice 序列化成 null 而非 []。GetGlobalDefault 用 matchers = '[]'::jsonb
		// 精确匹配"全局默认"策略，这里必须落地成 [] 才能被正确识别为全局默认。
		p.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(p.Matchers)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化策略 matchers 失败: %w", err)
	}
	stepsJSON, err := json.Marshal(p.Steps)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化策略 steps 失败: %w", err)
	}
	out, err := scanNotificationPolicy(tx.QueryRow(ctx, `
		INSERT INTO notification_policies (name, matchers, team_id, steps) VALUES ($1, $2, $3, $4)
		RETURNING `+notificationPolicyColumns, p.Name, matchersJSON, nullableStringArg(p.TeamID), stepsJSON))
	if err != nil {
		return nil, fmt.Errorf("store: 创建通知策略失败: %w", err)
	}
	return &out, nil
}

func (r *NotificationPolicyRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*NotificationPolicy, error) {
	out, err := scanNotificationPolicy(tx.QueryRow(ctx, `SELECT `+notificationPolicyColumns+` FROM notification_policies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询通知策略失败: %w", err)
	}
	return &out, nil
}

// ListByTeam 返回绑定到该团队的全部通知策略（含 matchers 非空的业务绑定策略与
// matchers 为空的团队默认策略，二者由调用方按 R13.5 的求值顺序自行区分优先级）。
func (r *NotificationPolicyRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]NotificationPolicy, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+notificationPolicyColumns+` FROM notification_policies WHERE team_id = $1 ORDER BY created_at LIMIT $2
	`, teamID, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出团队通知策略失败: %w", err)
	}
	defer rows.Close()

	var policies []NotificationPolicy
	for rows.Next() {
		p, err := scanNotificationPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描通知策略失败: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历通知策略失败: %w", err)
	}
	return policies, nil
}

// GetGlobalDefault 返回全局默认策略：team_id IS NULL 且 matchers 为空数组。
// 这个 matchers 过滤是刻意加的：team_id IS NULL 还可能是「全局范围的业务绑定策略」
// （R13.5 允许绑定不限团队），只有 team_id 和 matchers 都为空的那一条才是兜底的
// 全局默认——不加这条过滤，多条 team_id IS NULL 记录并存时会拿到不确定的一条。
func (r *NotificationPolicyRepo) GetGlobalDefault(ctx context.Context, tx pgx.Tx) (*NotificationPolicy, error) {
	out, err := scanNotificationPolicy(tx.QueryRow(ctx, `
		SELECT `+notificationPolicyColumns+`
		FROM notification_policies
		WHERE team_id IS NULL AND matchers = '[]'::jsonb
		ORDER BY created_at
		LIMIT 1
	`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询全局默认通知策略失败: %w", err)
	}
	return &out, nil
}

func (r *NotificationPolicyRepo) Update(ctx context.Context, tx pgx.Tx, p NotificationPolicy) error {
	if p.Matchers == nil {
		p.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(p.Matchers)
	if err != nil {
		return fmt.Errorf("store: 序列化策略 matchers 失败: %w", err)
	}
	stepsJSON, err := json.Marshal(p.Steps)
	if err != nil {
		return fmt.Errorf("store: 序列化策略 steps 失败: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE notification_policies SET name = $2, matchers = $3, team_id = $4, steps = $5 WHERE id = $1
	`, p.ID, p.Name, matchersJSON, nullableStringArg(p.TeamID), stepsJSON)
	if err != nil {
		return fmt.Errorf("store: 更新通知策略失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *NotificationPolicyRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM notification_policies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除通知策略失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
