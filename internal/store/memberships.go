package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MembershipRepo 是 memberships 表的仓储实现，约定同 TeamRepo（config_repos.go
// 头部注释）。除 RBAC 中间件需要的 IsAdminAnyTeam 外，还提供团队成员管理端点（C1，
// R1）所需的增删改查；「最后一名 admin 护栏」是 API 层职责（同一事务内先调
// CountAdmins 再决定是否放行降级/移除），本仓储只负责数据面操作。
type MembershipRepo struct{}

func NewMembershipRepo() *MembershipRepo { return &MembershipRepo{} }

// Member 是 ListByTeam 的返回行形状：join users 后给管理端展示用，不携带
// memberships.id/created_at 等调用方不需要的列。
type Member struct {
	UserID      string
	Username    string
	DisplayName string
	Role        string
}

// ErrMembershipConflict 在 Add 撞上 memberships.(team_id, user_id) 唯一约束时返回，
// 调用方（管理 API）据此给出 409「该用户已是团队成员」而非笼统的 500。
var ErrMembershipConflict = errors.New("store: 该用户已是团队成员")

// isMembershipConflict 判断 err 是否是 memberships 表 UNIQUE (team_id, user_id) 约束
// 被违反（建表语句内联声明，Postgres 按约定生成约束名 memberships_team_id_user_id_key）。
func isMembershipConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "memberships_team_id_user_id_key"
}

// IsAdminAnyTeam 报告 userID 是否在至少一个团队中持有 admin 角色。
func (r *MembershipRepo) IsAdminAnyTeam(ctx context.Context, tx pgx.Tx, userID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM memberships WHERE user_id = $1 AND role = 'admin')
	`, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: 查询用户 admin 成员资格失败: %w", err)
	}
	return exists, nil
}

// ListByTeam 返回团队全部成员，按 username 升序排列（管理端列表页展示顺序稳定，
// 不随插入顺序或 UUID 变化）。
func (r *MembershipRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string) ([]Member, error) {
	rows, err := tx.Query(ctx, `
		SELECT u.id, u.username, u.display_name, m.role
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1
		ORDER BY u.username
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出团队成员失败: %w", err)
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.Role); err != nil {
			return nil, fmt.Errorf("store: 扫描团队成员失败: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历团队成员失败: %w", err)
	}
	return members, nil
}

// Add 把 userID 加入 teamID，持有 role。重复添加（同一 (team_id, user_id) 已存在）
// 返回 ErrMembershipConflict。
func (r *MembershipRepo) Add(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO memberships (team_id, user_id, role) VALUES ($1, $2, $3)
	`, teamID, userID, role)
	if err != nil {
		if isMembershipConflict(err) {
			return ErrMembershipConflict
		}
		return fmt.Errorf("store: 添加团队成员失败: %w", err)
	}
	return nil
}

// UpdateRole 修改 teamID 下 userID 的角色；成员不存在返回 ErrNotFound。
func (r *MembershipRepo) UpdateRole(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE memberships SET role = $3 WHERE team_id = $1 AND user_id = $2
	`, teamID, userID, role)
	if err != nil {
		return fmt.Errorf("store: 更新团队成员角色失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Remove 把 userID 移出 teamID；成员不存在返回 ErrNotFound。
func (r *MembershipRepo) Remove(ctx context.Context, tx pgx.Tx, teamID, userID string) error {
	tag, err := tx.Exec(ctx, `
		DELETE FROM memberships WHERE team_id = $1 AND user_id = $2
	`, teamID, userID)
	if err != nil {
		return fmt.Errorf("store: 移除团队成员失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAdmins 统计 teamID 内持有 admin 角色的成员数，供「最后一名 admin」护栏
// （移除/降级前由调用方在同一事务内查询）使用。
func (r *MembershipRepo) CountAdmins(ctx context.Context, tx pgx.Tx, teamID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memberships WHERE team_id = $1 AND role = 'admin'
	`, teamID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: 统计团队 admin 数量失败: %w", err)
	}
	return count, nil
}
