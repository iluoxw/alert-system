package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// EventQueue 是 alert_events 表（原始事件队列）的仓储实现，供 ingest（写入）与
// pipeline worker（消费）复用。
//
// 事务边界统一由调用方通过 pgx.Tx 显式组织（例如 Store.WithTx）：
//   - Enqueue 在调用方事务内执行，使 ingest 端点满足「落库成功才 202」（R1.6 at-least-once）。
//   - Dequeue 依赖 `FOR UPDATE SKIP LOCKED` 取得的行锁，该锁只在事务存续期间生效；
//     调用方必须在同一个事务里紧接着调用 MarkProcessed/MarkFailed 并提交，否则：
//     (a) 若调用方直接用连接池（非显式事务）调用，SELECT 会作为隐式单语句事务立即提交，
//     行锁随之释放，其他 worker 可能在本次处理完成前就拿到同一批事件；
//     (b) 若迟迟不提交，会一直占用这些行，阻塞其他 worker 的 SKIP LOCKED 轮询。
//     因此 Dequeue/MarkProcessed/MarkFailed 的入参类型直接是 pgx.Tx 而非更宽泛的连接池
//     接口，把「必须在同一事务里」这条约束落实到编译期，调用方无法误传裸连接池。
type EventQueue struct{}

// NewEventQueue 构造 EventQueue。EventQueue 本身无状态，所有方法的执行上下文
// （连接/事务）均由调用方传入。
func NewEventQueue() *EventQueue {
	return &EventQueue{}
}

// DequeuedEvent 是 Dequeue 返回的一条待处理事件：ID 用于后续 MarkProcessed/MarkFailed，
// Event 是反序列化后的统一告警信封，IntegrationID 是产生该事件的 integration（可为空
// 字符串，对应 alert_events.integration_id 为 NULL），供 pipeline 层去重/severity 兜底使用。
type DequeuedEvent struct {
	ID            string
	IntegrationID string
	Event         domain.AlertEvent
}

// Enqueue 将一个统一告警事件序列化后落入 alert_events（初始 status='pending'），
// 返回生成的事件 ID。integrationID 为空字符串时落 NULL（如未来支持匿名/测试来源）。
//
// domain.AlertEvent 本身不携带 integration ID（只有面向展示的 Source 字符串），
// 该 ID 由 ingest 端点从请求路径 `/ingest/{integration_id}` 解析后传入，
// 因此签名比任务描述里的 (ctx, tx, event) 多了 integrationID 一个参数。
func (q *EventQueue) Enqueue(ctx context.Context, tx pgx.Tx, integrationID string, event domain.AlertEvent) (string, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("store: 序列化事件失败: %w", err)
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO alert_events (integration_id, payload)
		VALUES (NULLIF($1, '')::uuid, $2)
		RETURNING id
	`, integrationID, payload).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: 写入事件队列失败: %w", err)
	}
	return id, nil
}

// Dequeue 在 tx 内取出最多 batchSize 条 pending 事件并加行锁（FOR UPDATE SKIP LOCKED），
// 跳过已被其他并发事务锁定的行，实现多 worker 并发消费不重不漏。
// 调用方必须在同一个 tx 内对返回的事件调用 MarkProcessed/MarkFailed 后再提交。
func (q *EventQueue) Dequeue(ctx context.Context, tx pgx.Tx, batchSize int) ([]DequeuedEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, COALESCE(integration_id::text, ''), payload
		FROM alert_events
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, batchSize)
	if err != nil {
		return nil, fmt.Errorf("store: 取事件批次失败: %w", err)
	}
	defer rows.Close()

	var batch []DequeuedEvent
	for rows.Next() {
		var id, integrationID string
		var payload []byte
		if err := rows.Scan(&id, &integrationID, &payload); err != nil {
			return nil, fmt.Errorf("store: 扫描事件失败: %w", err)
		}
		var event domain.AlertEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("store: 反序列化事件 %s 失败: %w", id, err)
		}
		batch = append(batch, DequeuedEvent{ID: id, IntegrationID: integrationID, Event: event})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历事件批次失败: %w", err)
	}
	return batch, nil
}

// MarkProcessed 将给定 ID 的事件标记为 processed。必须与产生这些 ID 的 Dequeue
// 处于同一事务。ids 为空时是空操作。
func (q *EventQueue) MarkProcessed(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE alert_events
		SET status = 'processed', processed_at = now()
		WHERE id = ANY($1::uuid[])
	`, ids)
	if err != nil {
		return fmt.Errorf("store: 标记事件已处理失败: %w", err)
	}
	return nil
}

// MarkFailed 将单条事件标记为 failed 并记录失败原因。failed 状态的事件不再被
// Dequeue 取出（不会被重复消费），需要人工介入或走独立的重试/死信路径。
func (q *EventQueue) MarkFailed(ctx context.Context, tx pgx.Tx, id string, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE alert_events
		SET status = 'failed', error = $2, processed_at = now()
		WHERE id = $1
	`, id, reason)
	if err != nil {
		return fmt.Errorf("store: 标记事件失败原因失败: %w", err)
	}
	return nil
}
