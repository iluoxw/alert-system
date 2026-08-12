package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LarkCardAction 是一次经过飞书验签的卡片动作占位，request_id 是客户端生成的
// 幂等键。它只记录动作事实，不替代 Alert/Fault/Incident 的领域时间线。
type LarkCardAction struct {
	RequestID      string
	ResourceType   string
	ResourceID     string
	Action         string
	OperatorLarkID string
	OperatorUserID string
	Payload        json.RawMessage
	Result         json.RawMessage
}

type LarkCardActionRepo struct{}

func NewLarkCardActionRepo() *LarkCardActionRepo { return &LarkCardActionRepo{} }

// Insert 占位写入动作。返回 created=false 表示同一个 request_id 已经成功处理过，
// 调用方应直接向飞书返回成功，避免重复点击造成二次状态转移。
func (r *LarkCardActionRepo) Insert(ctx context.Context, tx pgx.Tx, action LarkCardAction) (created bool, err error) {
	payload := action.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	result := action.Result
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	var userID any
	if action.OperatorUserID != "" {
		userID = action.OperatorUserID
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO lark_card_actions
			(request_id, resource_type, resource_id, action, operator_lark_id, operator_user_id, payload, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (request_id) DO NOTHING
	`, action.RequestID, action.ResourceType, action.ResourceID, action.Action, action.OperatorLarkID, userID, payload, result)
	if err != nil {
		return false, fmt.Errorf("store: 写入飞书卡片动作失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateResult 在领域状态机成功后补写结果快照。它与 Insert 位于同一事务，
// 因此失败动作不会留下“看起来已成功”的审计记录。
func (r *LarkCardActionRepo) UpdateResult(ctx context.Context, tx pgx.Tx, requestID string, result json.RawMessage) error {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	if _, err := tx.Exec(ctx, `UPDATE lark_card_actions SET result = $2 WHERE request_id = $1`, requestID, result); err != nil {
		return fmt.Errorf("store: 更新飞书卡片动作结果失败: %w", err)
	}
	return nil
}
