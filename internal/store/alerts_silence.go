package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpdateSilencedBy 整体覆盖写入 alerts.silenced_by（R10）：静默 stage 每次求值后，
// 用本次命中的静默 ID 全集覆盖旧值，而不是增量 append/remove——过期或被删除的静默
// 只要不再出现在下一次求值结果里，就会随下一个到达的事件自动从 silenced_by 中消失，
// 天然满足「静默过期/删除后恢复通知」（R10.3/R10.5），调用方无需单独处理移除逻辑。
// silencedBy 为 nil 时按空数组写入（列有 NOT NULL 约束）。
func (r *AlertRepo) UpdateSilencedBy(ctx context.Context, tx pgx.Tx, id string, silencedBy []string) error {
	if silencedBy == nil {
		silencedBy = []string{}
	}
	tag, err := tx.Exec(ctx, `UPDATE alerts SET silenced_by = $2::uuid[] WHERE id = $1`, id, silencedBy)
	if err != nil {
		return fmt.Errorf("store: 更新告警静默命中失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}
