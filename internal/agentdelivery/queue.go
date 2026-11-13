package agentdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"alert-system/internal/store"
)

// Queue 的所有状态变化和对应审计事件一起提交。
type Queue struct{ st *store.Store }

func NewQueue(st *store.Store) *Queue { return &Queue{st: st} }

// Enqueue 由投影器在处理批次的事务中调用。新消息和原先 pending 的消息可更新快照，
// 已领取消息只累加计数，始终保留这次领取时的内容与最初的过期时间。
func (q *Queue) Enqueue(ctx context.Context, tx pgx.Tx, env Envelope, p Policy) error {
	if p.TTL <= 0 || p.MaxAttempts < 1 || p.Role == "" {
		return errors.New("agent delivery: invalid dispatch policy")
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	// 活跃索引中可能还有 TTL 已到、尚未由 Reaper 落地的行。ON CONFLICT 先锁行，
	// 若命中此类冲突则在当前事务中将旧行标记过期并重试一次，避免吞掉新到达的组。
	const upsert = `WITH timing AS MATERIALIZED (
            SELECT gen_random_uuid() AS id, clock_timestamp() AS projected_at, coalesce($9::timestamptz,clock_timestamp()) AS enqueued_at
        ), candidate AS (
            SELECT *, enqueued_at + make_interval(secs => $8) AS expires_at FROM timing
        ) INSERT INTO agent_messages (id,role,dedup_key,envelope,snapshot_at,route_id,config_version,priority,max_attempts,created_at,expires_at,status,settled_at)
        SELECT id,$1,NULLIF($2,''), $3::jsonb || jsonb_build_object('id',id),enqueued_at,$4,$5,$6,$7,enqueued_at,expires_at,
            CASE WHEN expires_at<=projected_at THEN 'expired' ELSE 'pending' END,
            CASE WHEN expires_at<=projected_at THEN projected_at END
        FROM candidate
        ON CONFLICT (role,dedup_key) WHERE status IN ('pending','leased') AND dedup_key IS NOT NULL
        DO UPDATE SET dup_count = agent_messages.dup_count + 1,
            envelope = CASE WHEN agent_messages.status='pending' AND excluded.snapshot_at>=agent_messages.snapshot_at
                THEN excluded.envelope || jsonb_build_object('id',agent_messages.id) ELSE agent_messages.envelope END,
            snapshot_at = CASE WHEN agent_messages.status='pending' THEN greatest(agent_messages.snapshot_at,excluded.snapshot_at) ELSE agent_messages.snapshot_at END
        WHERE agent_messages.expires_at > clock_timestamp()
        RETURNING id::text, status, dup_count`
	var enqueuedAt any
	if !p.EnqueuedAt.IsZero() {
		enqueuedAt = p.EnqueuedAt
	}
	var id, status string
	var count int64
	for {
		err = tx.QueryRow(ctx, upsert, p.Role, env.DedupKey, payload, p.RouteID, p.ConfigVersion, p.Priority, p.MaxAttempts, p.TTL.Seconds(), enqueuedAt).Scan(&id, &status, &count)
		if !errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if _, err = tx.Exec(ctx, `WITH old AS (
            SELECT id,status FROM agent_messages WHERE role=$1 AND dedup_key=$2 AND status IN ('pending','leased') AND expires_at<=clock_timestamp() FOR UPDATE
        ), expired AS (
            UPDATE agent_messages m SET status='expired',settled_at=clock_timestamp(),lease_id=NULL,lease_owner=NULL,lease_expires_at=NULL
            FROM old WHERE m.id=old.id RETURNING m.id,old.status
        ) INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor)
          SELECT id,'ttl',status,'expired','projector' FROM expired`, p.Role, env.DedupKey); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	kind := "enqueue"
	var previous any
	if status == "expired" {
		kind = "ttl_before_projection"
	}
	if count > 1 {
		kind = "duplicate"
		previous = status
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
        VALUES ($1,$2,$3,$4,'projector',jsonb_build_object('trace_id',$5::text,'route_id',$6::text,'config_version',$7::text))`, id, kind, previous, status, env.TraceID, p.RouteID, p.ConfigVersion)
	if err != nil || count > 1 || status == "expired" {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('agent_delivery_ready',$1)`, p.Role)
	return err
}

// Claim 在一个短事务中完成心跳、竞争领取和审计。每条消息生成独立租约，
// attempts 是领取代次。数据库游标和连接都在返回 HTTP 层之前释放。
func (q *Queue) Claim(ctx context.Context, who Principal, version string, max int) ([]Delivery, error) {
	if max < 1 || who.ClientID == "" || who.Role == "" {
		return nil, errors.New("agent delivery: invalid claim")
	}
	var batch []Delivery
	err := q.st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO agent_consumers(role,client_id,version,last_seen_at)
            SELECT $1,$2,$3,clock_timestamp() WHERE NOT EXISTS (
                SELECT 1 FROM agent_consumers WHERE role=$1 AND client_id=$2 AND version=$3 AND last_seen_at>now()-interval '10 seconds')
            ON CONFLICT (role,client_id) DO UPDATE SET version=excluded.version,last_seen_at=excluded.last_seen_at`, who.Role, who.ClientID, version)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH next AS (
            SELECT id FROM agent_messages WHERE role=$1 AND status='pending' AND visible_at<=now() AND expires_at>clock_timestamp()
            ORDER BY priority DESC,created_at,id LIMIT $3 FOR UPDATE SKIP LOCKED
        ), claimed AS (
            UPDATE agent_messages m SET status='leased',lease_id=gen_random_uuid(),lease_owner=$2,
                lease_expires_at=least(m.expires_at,clock_timestamp()+interval '30 seconds'),attempts=m.attempts+1
            FROM next WHERE m.id=next.id AND m.expires_at>clock_timestamp()
            RETURNING m.id,m.lease_id,m.attempts,m.lease_expires_at,m.ack_mode,m.envelope,m.priority,m.created_at
        ), audit AS (
            INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
            SELECT id,'claim','pending','leased',$2,jsonb_build_object('attempt',attempts) FROM claimed
        ) SELECT id::text,lease_id::text,attempts,lease_expires_at,ack_mode,envelope FROM claimed ORDER BY priority DESC,created_at,id`, who.Role, who.ClientID, max)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Delivery
			var raw []byte
			if err := rows.Scan(&d.MessageID, &d.LeaseID, &d.Attempt, &d.LeaseExpiresAt, &d.AckMode, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &d.Envelope); err != nil {
				return fmt.Errorf("agent delivery: decode envelope: %w", err)
			}
			batch = append(batch, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// 按凭据身份、消息和领取代次取行锁。截止时间在获得锁后的下一条 SQL 中
// 使用 clock_timestamp() 检查，从而包含等待并发操作释放锁所用的时间。
func lockReceipt(ctx context.Context, tx pgx.Tx, who Principal, r Receipt) (string, error) {
	var msg, lease pgtype.UUID
	if r.Attempt < 1 || msg.Scan(r.MessageID) != nil || lease.Scan(r.LeaseID) != nil {
		return "", ErrLeaseConflict
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM agent_messages WHERE id=$1 AND lease_id=$2 AND attempts=$3 AND role=$4 AND lease_owner=$5 FOR UPDATE`, msg, lease, r.Attempt, who.Role, who.ClientID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLeaseConflict
	}
	return status, err
}

func (q *Queue) Ack(ctx context.Context, who Principal, r Receipt) error {
	return q.st.WithTx(ctx, func(tx pgx.Tx) error {
		status, err := lockReceipt(ctx, tx, who, r)
		if err != nil {
			return err
		}
		if status == "acked" {
			return nil
		}
		tag, err := tx.Exec(ctx, `WITH ack AS (
            UPDATE agent_messages SET status='acked',settled_at=clock_timestamp(),lease_expires_at=NULL
            WHERE id=$1 AND status='leased' AND expires_at>clock_timestamp() AND lease_expires_at>clock_timestamp() RETURNING id
        ) INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
          SELECT id,'ack','leased','acked',$2,jsonb_build_object('attempt',$3::int) FROM ack`, r.MessageID, who.ClientID, r.Attempt)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrLeaseConflict
		}
		return err
	})
}

const backoffSQL = `least(60, 1 << least(attempts-1, 6))`

func (q *Queue) Nack(ctx context.Context, who Principal, r NegativeReceipt) error {
	return q.st.WithTx(ctx, func(tx pgx.Tx) error {
		status, err := lockReceipt(ctx, tx, who, r.Receipt)
		if err != nil {
			return err
		}
		if status != "leased" {
			return ErrLeaseConflict
		}
		delay := min(max(r.RetryAfter, 0), 3600)
		tag, err := tx.Exec(ctx, `WITH nack AS (
            UPDATE agent_messages SET status=CASE WHEN $2 AND attempts<max_attempts THEN 'pending' ELSE 'dead' END,
                visible_at=clock_timestamp() + make_interval(secs => coalesce(nullif($3::int,0),`+backoffSQL+`)),
                settled_at=CASE WHEN $2 AND attempts<max_attempts THEN NULL ELSE clock_timestamp() END,
                lease_id=NULL,lease_owner=NULL,lease_expires_at=NULL
            WHERE id=$1 AND status='leased' AND expires_at>clock_timestamp() AND lease_expires_at>clock_timestamp() RETURNING id,status
        ) INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
          SELECT id,'nack','leased',status,$4,jsonb_build_object('reason',left($5::text,512),'attempt',$6::int) FROM nack`, r.MessageID, r.Retryable, delay, who.ClientID, r.Reason, r.Attempt)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrLeaseConflict
		}
		return err
	})
}

// Reap 每次回收有限数量的任务，按 TTL、租约到期、终态保留期处理。
// 回退可见时间的任务由 poll 的有界重查发现，无须为每次重试创建定时器。
func (q *Queue) Reap(ctx context.Context) error {
	return q.st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH due AS (
            SELECT id,status,CASE WHEN expires_at<=now() THEN 'expired' WHEN attempts>=max_attempts THEN 'dead' ELSE 'pending' END AS target
            FROM agent_messages WHERE status IN ('leased','pending') AND (expires_at<=now() OR lease_expires_at<=now())
            ORDER BY least(expires_at,lease_expires_at),id LIMIT 128 FOR UPDATE SKIP LOCKED
        ), updated AS (
            UPDATE agent_messages m SET status=due.target,lease_id=NULL,lease_owner=NULL,lease_expires_at=NULL,
                visible_at=CASE WHEN due.target='pending' THEN now()+make_interval(secs => `+backoffSQL+`) ELSE m.visible_at END,
                settled_at=CASE WHEN due.target='pending' THEN NULL ELSE now() END
            FROM due WHERE m.id=due.id RETURNING m.id,due.status AS previous,m.status,m.attempts
        ) INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
          SELECT id,CASE WHEN status='expired' THEN 'ttl' ELSE 'lease_timeout' END,previous,status,'reaper',jsonb_build_object('attempt',attempts) FROM updated`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM agent_messages WHERE id IN (SELECT id FROM agent_messages WHERE settled_at <= now() - interval '7 days' ORDER BY settled_at,id LIMIT 128 FOR UPDATE SKIP LOCKED)`)
		return err
	})
}
