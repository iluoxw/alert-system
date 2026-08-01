package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StormState 镜像 route_storm_state 表（design.md C3），字段与表列一一对应。
// 行按 route_id 保留不删——进入/解除熔断的时刻本身就是可查询的历史记录
// （AC3.2），重复进入时 UPSERT 覆盖而不是插入新行。
type StormState struct {
	RouteID         string
	Active          bool
	ActivatedAt     time.Time
	DeactivatedAt   *time.Time // 未解除过时为 nil
	SuppressedCount int
	LastSummaryAt   *time.Time // 从未发过摘要时为 nil
}

// StormStateRepo 是 route_storm_state 表的仓储实现。与项目其余仓储同样的约定：
// 无状态，所有方法在调用方传入的 pgx.Tx 内执行，事务边界由调用方通过
// Store.WithTx 组织；本仓储只 UPSERT/UPDATE，不提供 DELETE（行永久保留）。
type StormStateRepo struct{}

// NewStormStateRepo 构造 StormStateRepo。
func NewStormStateRepo() *StormStateRepo { return &StormStateRepo{} }

func scanStormState(row configRowScanner) (StormState, error) {
	var s StormState
	err := row.Scan(&s.RouteID, &s.Active, &s.ActivatedAt, &s.DeactivatedAt, &s.SuppressedCount, &s.LastSummaryAt)
	return s, err
}

// Get 按 routeID 查询风暴状态；从未进入过熔断（行不存在）返回 (nil, nil)——
// 这是「尚无风暴记录」的正常情况，不是错误（区别于本包其余 GetByID 遇不存在
// 行返回 ErrNotFound 的惯例：调用方 GroupStage.Handle 每次 flush 都要查一次，
// 大多数路由永远不会触发风暴，用 sentinel error 表达"正常"状态反而是噪音）。
func (r *StormStateRepo) Get(ctx context.Context, tx pgx.Tx, routeID string) (*StormState, error) {
	s, err := scanStormState(tx.QueryRow(ctx, `
		SELECT route_id, active, activated_at, deactivated_at, suppressed_count, last_summary_at
		FROM route_storm_state WHERE route_id = $1
	`, routeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询风暴状态失败: %w", err)
	}
	return &s, nil
}

// Activate 把 routeID 的风暴状态置为进入熔断：active=true、activated_at=now、
// deactivated_at 清空。suppressed_count 只在「本次调用前不是 active」时重置为 0
// （首次进入熔断，或从已解除状态重新进入）；若调用前已经是 active（重复
// Activate，比如同一 flush 窗口内被连续判定超阈值），保留既有计数不动——
// 计数的推进只由 AddSuppressed 负责，Activate 不应把已经在累积的数字清零。
//
// ON CONFLICT DO UPDATE 的 SET 子句里裸表名 route_storm_state.active 引用的是
// 冲突发生前该行的旧值（PG 语义），而不是本次 EXCLUDED 的新值，因此能在同一条
// UPSERT 语句内完成"仅在跨越 false->true 边界时清零"的判断，不需要先 SELECT
// 再决定分支（避免 TOCTOU）。
func (r *StormStateRepo) Activate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO route_storm_state (route_id, active, activated_at, deactivated_at, suppressed_count)
		VALUES ($1, true, $2, NULL, 0)
		ON CONFLICT (route_id) DO UPDATE SET
			active = true,
			activated_at = $2,
			deactivated_at = NULL,
			suppressed_count = CASE WHEN route_storm_state.active THEN route_storm_state.suppressed_count ELSE 0 END
	`, routeID, now)
	if err != nil {
		return fmt.Errorf("store: 激活风暴状态失败: %w", err)
	}
	return nil
}

// AddSuppressed 把 routeID 的 suppressed_count 累加 n。要求行已存在（由 Activate
// 先建立）——未找到行返回 ErrNotFound，遵循本包 Update 类方法的既有惯例
// （见 UserRepo.Update 等）。
func (r *StormStateRepo) AddSuppressed(ctx context.Context, tx pgx.Tx, routeID string, n int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE route_storm_state SET suppressed_count = suppressed_count + $2 WHERE route_id = $1
	`, routeID, n)
	if err != nil {
		return fmt.Errorf("store: 累加风暴抑制计数失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkSummary 记录 routeID 本次摘要通知的发送时刻。要求行已存在，未找到行返回
// ErrNotFound。
func (r *StormStateRepo) MarkSummary(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE route_storm_state SET last_summary_at = $2 WHERE route_id = $1
	`, routeID, now)
	if err != nil {
		return fmt.Errorf("store: 记录风暴摘要时刻失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Deactivate 把 routeID 的风暴状态置为解除：active=false、deactivated_at=now。
// 行保留不删（历史即记录，AC3.2）。要求行已存在，未找到行返回 ErrNotFound。
func (r *StormStateRepo) Deactivate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE route_storm_state SET active = false, deactivated_at = $2 WHERE route_id = $1
	`, routeID, now)
	if err != nil {
		return fmt.Errorf("store: 解除风暴状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
