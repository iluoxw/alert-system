package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// TimelineRepo 是 timeline_entries 表（告警时间线）的仓储实现（R4.6）。
// 时间线是 append-only 的审计记录：本文件只提供 Append/ListByAlert，
// 不提供、也绝不应该提供任何 UPDATE/DELETE 方法。
type TimelineRepo struct{}

// NewTimelineRepo 构造 TimelineRepo。
func NewTimelineRepo() *TimelineRepo {
	return &TimelineRepo{}
}

// Append 把 domain 层状态机产出的一条 TimelineEntry 落库，返回生成的 ID。
// entry.At 作为 created_at 写入（而非依赖列默认值 now()），以尊重调用方
// 通过 domain.Ack/Resolve/Handoff 传入的可测试时间参数。
func (r *TimelineRepo) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	detail, err := json.Marshal(entry.Detail)
	if err != nil {
		return "", fmt.Errorf("store: 序列化时间线详情失败: %w", err)
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO timeline_entries (alert_id, actor, action, detail, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, entry.AlertID, entry.Actor, entry.Action, detail, entry.At).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: 写入时间线失败: %w", err)
	}
	return id, nil
}

// ListByAlert 按时间正序（从早到晚）返回一个告警的完整时间线。
func (r *TimelineRepo) ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT alert_id, actor, action, detail, created_at
		FROM timeline_entries
		WHERE alert_id = $1
		ORDER BY created_at ASC
	`, alertID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询时间线失败: %w", err)
	}
	defer rows.Close()

	var entries []domain.TimelineEntry
	for rows.Next() {
		var (
			entry     domain.TimelineEntry
			detailRaw []byte
		)
		if err := rows.Scan(&entry.AlertID, &entry.Actor, &entry.Action, &detailRaw, &entry.At); err != nil {
			return nil, fmt.Errorf("store: 扫描时间线条目失败: %w", err)
		}
		if err := json.Unmarshal(detailRaw, &entry.Detail); err != nil {
			return nil, fmt.Errorf("store: 反序列化时间线详情失败: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历时间线失败: %w", err)
	}
	return entries, nil
}
