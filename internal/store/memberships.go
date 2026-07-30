package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MembershipRepo 是 memberships 表的仓储实现，约定同 TeamRepo（config_repos.go
// 头部注释）。本期只提供 RBAC 中间件需要的最小查询面：判断用户是否在任意团队持有
// admin 角色（见 internal/api/middleware.go 对 R8.2 的落地说明）。
type MembershipRepo struct{}

func NewMembershipRepo() *MembershipRepo { return &MembershipRepo{} }

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
