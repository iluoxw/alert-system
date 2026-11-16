package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// FaultNotificationDelivery 不包含目标原文。sent 仅表示渠道接受，与 Fault ACK 独立。
type FaultNotificationDelivery struct {
	ID         string    `json:"id"`
	FaultID    string    `json:"fault_id"`
	Epoch      int64     `json:"epoch"`
	Channel    string    `json:"channel"`
	TargetHash string    `json:"target_hash"`
	Status     string    `json:"status"`
	Generation int       `json:"generation"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type FaultNotificationRepo struct{}

func NewFaultNotificationRepo() *FaultNotificationRepo { return &FaultNotificationRepo{} }
func NotificationTargetHash(channel, target string) string {
	h := sha256.Sum256([]byte(channel + "\x1e" + target))
	return hex.EncodeToString(h[:])
}
func (r *FaultNotificationRepo) Enroll(ctx context.Context, tx pgx.Tx, id string, epoch int64) (int64, error) {
	var actual int64
	err := tx.QueryRow(ctx, `INSERT INTO fault_notification_watches(fault_id,epoch) VALUES($1,$2)
 ON CONFLICT(fault_id) DO UPDATE SET fault_id=EXCLUDED.fault_id RETURNING epoch`, id, epoch).Scan(&actual)
	return actual, err
}
func (r *FaultNotificationRepo) Defer(ctx context.Context, tx pgx.Tx, id, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE fault_notification_watches SET reason=$2,next_check_at=$3 WHERE fault_id=$1`, id, reason, at)
	return err
}
func (r *FaultNotificationRepo) Get(ctx context.Context, tx pgx.Tx, faultID string, epoch int64, channel, hash string) (FaultNotificationDelivery, error) {
	var d FaultNotificationDelivery
	err := tx.QueryRow(ctx, `SELECT id::text,fault_id::text,epoch,channel,target_hash,status,generation,updated_at FROM fault_notification_deliveries WHERE fault_id=$1 AND epoch=$2 AND channel=$3 AND target_hash=$4`, faultID, epoch, channel, hash).Scan(&d.ID, &d.FaultID, &d.Epoch, &d.Channel, &d.TargetHash, &d.Status, &d.Generation, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}
func (r *FaultNotificationRepo) Ensure(ctx context.Context, tx pgx.Tx, faultID string, epoch int64, channel, hash, status string) (FaultNotificationDelivery, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO fault_notification_deliveries(fault_id,epoch,channel,target_hash,status) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING RETURNING id::text`, faultID, epoch, channel, hash, status).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return FaultNotificationDelivery{}, false, err
	}
	d, e := r.Get(ctx, tx, faultID, epoch, channel, hash)
	return d, id != "", e
}
func (r *FaultNotificationRepo) Retry(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	tag, e := tx.Exec(ctx, `UPDATE fault_notification_deliveries SET status='queued',generation=generation+1,updated_at=now() WHERE id=$1 AND status='failed' AND generation<2`, id)
	return tag.RowsAffected() == 1, e
}
func (r *FaultNotificationRepo) List(ctx context.Context, tx pgx.Tx, id string) ([]FaultNotificationDelivery, error) {
	rows, e := tx.Query(ctx, `SELECT id::text,fault_id::text,epoch,channel,target_hash,status,generation,updated_at FROM fault_notification_deliveries WHERE fault_id=$1 ORDER BY updated_at,id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FaultNotificationDelivery{}
	for rows.Next() {
		var d FaultNotificationDelivery
		if e = rows.Scan(&d.ID, &d.FaultID, &d.Epoch, &d.Channel, &d.TargetHash, &d.Status, &d.Generation, &d.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (r *FaultNotificationRepo) Covered(ctx context.Context, tx pgx.Tx, ids []string, channel, hash string) (bool, error) {
	var yes bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fault_notification_deliveries d JOIN faults f ON f.id=d.fault_id JOIN fault_notification_watches w ON w.fault_id=f.id AND w.epoch=d.epoch WHERE f.id=ANY($1::uuid[]) AND f.status='active' AND d.channel=$2 AND d.target_hash=$3 AND d.status='sent')`, ids, channel, hash).Scan(&yes)
	return yes, e
}
func (r *FaultNotificationRepo) Due(ctx context.Context, tx pgx.Tx, at time.Time) ([]string, error) {
	rows, e := tx.Query(ctx, `SELECT w.fault_id::text FROM fault_notification_watches w JOIN faults f ON f.id=w.fault_id WHERE w.next_check_at<=$1 AND f.status='active' AND f.acked_at IS NULL ORDER BY w.next_check_at LIMIT 100`, at)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Begin 与 ACK/状态转移共用 Fault 行锁，避免已认领对象继续开始发送。
func (r *FaultNotificationRepo) Begin(ctx context.Context, tx pgx.Tx, id string, generation int) (bool, error) {
	var faultID string
	if e := tx.QueryRow(ctx, `SELECT fault_id::text FROM fault_notification_deliveries WHERE id=$1`, id).Scan(&faultID); e != nil {
		return false, e
	}
	var eligible bool
	if e := tx.QueryRow(ctx, `SELECT status='active' AND acked_at IS NULL AND EXISTS(SELECT 1 FROM fault_notification_deliveries d JOIN fault_notification_watches w ON w.fault_id=d.fault_id AND w.epoch=d.epoch WHERE d.id=$2) FROM faults WHERE id=$1 FOR UPDATE`, faultID, id).Scan(&eligible); e != nil {
		return false, e
	}
	if !eligible {
		_, e := tx.Exec(ctx, `UPDATE fault_notification_deliveries SET status='cancelled',updated_at=now() WHERE id=$1 AND generation=$2 AND status IN ('queued','sending')`, id, generation)
		return false, e
	}
	tag, e := tx.Exec(ctx, `UPDATE fault_notification_deliveries SET status='sending',updated_at=now() WHERE id=$1 AND generation=$2 AND status IN ('queued','sending')`, id, generation)
	return tag.RowsAffected() == 1, e
}
func (r *FaultNotificationRepo) Result(ctx context.Context, tx pgx.Tx, id string, generation int, status string) (string, bool, error) {
	var faultID string
	if e := tx.QueryRow(ctx, `SELECT fault_id::text FROM fault_notification_deliveries WHERE id=$1`, id).Scan(&faultID); e != nil {
		return "", false, e
	}
	if _, e := tx.Exec(ctx, `SELECT id FROM faults WHERE id=$1 FOR UPDATE`, faultID); e != nil {
		return "", false, e
	}
	e := tx.QueryRow(ctx, `UPDATE fault_notification_deliveries SET status=$3,updated_at=now() WHERE id=$1 AND generation=$2 AND status IN ('queued','sending') AND EXISTS(SELECT 1 FROM fault_notification_watches w WHERE w.fault_id=fault_notification_deliveries.fault_id AND w.epoch=fault_notification_deliveries.epoch) RETURNING fault_id::text`, id, generation, status).Scan(&faultID)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", false, nil
	}
	return faultID, e == nil, e
}

func (r *FaultNotificationRepo) ReleaseCoverage(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	tag, e := tx.Exec(ctx, `UPDATE fault_notification_deliveries SET status='queued',updated_at=now() WHERE id=$1 AND status='covered'`, id)
	return tag.RowsAffected() == 1, e
}

func (r *FaultNotificationRepo) Epoch(ctx context.Context, tx pgx.Tx, id string, fallback int64) (int64, error) {
	var epoch int64
	e := tx.QueryRow(ctx, `SELECT epoch FROM fault_notification_watches WHERE fault_id=$1`, id).Scan(&epoch)
	if errors.Is(e, pgx.ErrNoRows) {
		return fallback, nil
	}
	return epoch, e
}
func (r *FaultNotificationRepo) Activate(ctx context.Context, tx pgx.Tx, id string, epoch int64) error {
	if _, e := tx.Exec(ctx, `UPDATE faults SET owner_notified_at=NULL WHERE id=$1`, id); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `INSERT INTO fault_notification_watches(fault_id,epoch) VALUES($1,$2) ON CONFLICT(fault_id) DO UPDATE SET epoch=EXCLUDED.epoch,reason='queued',next_check_at=now()`, id, epoch)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE fault_notification_deliveries SET status='cancelled',updated_at=now() WHERE fault_id=$1 AND epoch<>$2 AND status IN ('queued','sending')`, id, epoch)
	return e
}

// Orphaned 把失去可领取 timer 的在途意图收敛为 failed，等待显式且有界补投。
// claimed 即使租约过期仍可重领，因此不把它当孤儿。
func (r *FaultNotificationRepo) Orphaned(ctx context.Context, tx pgx.Tx, faultID string) ([]string, error) {
	rows, e := tx.Query(ctx, `UPDATE fault_notification_deliveries d SET status='failed',updated_at=now()
 WHERE fault_id=$1 AND status IN ('queued','sending') AND NOT EXISTS(
 SELECT 1 FROM timers t WHERE t.kind='notify_send' AND t.ref_id='fault-delivery:'||d.id::text||':'||d.generation::text AND t.status IN ('pending','claimed')) RETURNING d.id::text`, faultID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *FaultNotificationRepo) Watch(ctx context.Context, tx pgx.Tx, id string) (string, time.Time, error) {
	var reason string
	var at time.Time
	e := tx.QueryRow(ctx, `SELECT reason,next_check_at FROM fault_notification_watches WHERE fault_id=$1`, id).Scan(&reason, &at)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", at, nil
	}
	return reason, at, e
}

func (r *FaultNotificationRepo) HasLiveTimer(ctx context.Context, tx pgx.Tx, id string, generation int) (bool, error) {
	var yes bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM timers WHERE kind='notify_send' AND ref_id='fault-delivery:'||$1||':'||$2::integer::text AND status IN ('pending','claimed'))`, id, generation).Scan(&yes)
	return yes, e
}
