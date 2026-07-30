package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DetachSilence 从所有引用 silenceID 的 alerts.silenced_by 数组里原子地移除该 ID
// （R10.5：静默被管理员提前删除时，已经被它标记静默的告警必须立即恢复通知，不能
// 等到下一条事件到达、SilenceStage 用新的静默全集覆盖 silenced_by 才生效——两者
// 是互补的两条自愈路径，这里补的是"已持久化告警"这一段，管道侧对"尚未到达的
// 新事件"的自愈见 stage_silence.go 的 10s 缓存 TTL）。调用方需要与
// SilenceRepo.Delete 在同一个事务内调用，保证"删除静默规则"与"清理已静默告警的
// 引用"要么一起成功要么一起回滚。没有任何告警引用该 ID 时也不是错误（幂等）。
func (r *AlertRepo) DetachSilence(ctx context.Context, tx pgx.Tx, silenceID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE alerts SET silenced_by = array_remove(silenced_by, $1::uuid) WHERE $1 = ANY(silenced_by)
	`, silenceID)
	if err != nil {
		return fmt.Errorf("store: 清理告警静默引用失败: %w", err)
	}
	return nil
}
