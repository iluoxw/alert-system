package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// Update 是 SilenceRepo 既有 CRUD 面（silences.go：Create/GetByID/List/ListActive/
// Delete）的纯增量补充——管理 API（任务 27）的 silences CRUD 需要能修改一条静默的
// matchers/时间窗/comment，既有文件里没有这个方法，这里新增，不改动 silences.go。
func (r *SilenceRepo) Update(ctx context.Context, tx pgx.Tx, s Silence) error {
	if s.Matchers == nil {
		s.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(s.Matchers)
	if err != nil {
		return fmt.Errorf("store: 序列化静默 matchers 失败: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE silences SET matchers = $2, starts_at = $3, ends_at = $4, comment = $5 WHERE id = $1
	`, s.ID, matchersJSON, s.StartsAt, s.EndsAt, nullableStringArg(s.Comment))
	if err != nil {
		return fmt.Errorf("store: 更新静默失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
