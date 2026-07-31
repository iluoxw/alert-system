package app

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/config"
)

// TimerKindRetentionSweep 是数据保留清理任务的 timer kind（C5，第十个 kind）。
// 与其余按告警/团队区分实例的 kind 不同，这是进程级全局单例任务，固定使用
// retentionSweepRefID，永远只存在一条 pending 记录。
const TimerKindRetentionSweep = "retention_sweep"

// retentionSweepRefID 是 retention_sweep 定时器固定使用的 refID。
const retentionSweepRefID = "global"

// retentionCatchUpBackoff 是"本轮任一表删满一批"时的续期间隔：用比 SweepInterval
// 短得多的固定值快速消化积压（如首次上线、面对多年历史数据），不必等一整个
// SweepInterval 才继续清下一批。
const retentionCatchUpBackoff = time.Minute

// retentionScheduler 是 handler 自续期依赖的窄接口，由 scheduler.Scheduler 实现。
type retentionScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
}

// retentionHandler 实现 retention_sweep 定时器（C5）：同一事务内分批删除 timers
// 表里已进入终态（fired/failed/canceled）且超过保留期的行、notification_log 表里
// 超过保留期的行，随即自续期。
//
// 分批删除（DELETE ... WHERE id IN (SELECT id ... LIMIT n)）而不是一条不限行数的
// DELETE，是为了避免长事务锁表（NFR：批量上限防长事务锁表）；两条 DELETE 与续期
// INSERT 同属 scheduler 传入的这一个事务，符合 handler 不自开事务的不变量。
type retentionHandler struct {
	sched retentionScheduler
	cfg   config.RetentionConfig

	// now 默认 time.Now，测试替换为可控时钟以精确断言"删了哪些行、下次何时续期"。
	now func() time.Time
}

// newRetentionHandler 构造 retentionHandler。
func newRetentionHandler(sched retentionScheduler, cfg config.RetentionConfig) *retentionHandler {
	return &retentionHandler{sched: sched, cfg: cfg, now: time.Now}
}

// Handle 实现 scheduler.Handler。DELETE 天然幂等（重复执行只会删到 0 行），续期用
// 既有 Schedule 语义（同 kind+refID 替换 fire_at），因此本 handler 对 at-least-once
// 重投安全，无需额外去重。
func (h *retentionHandler) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	now := h.now()

	timersDeleted, err := deleteExpiredTimers(ctx, tx, now.AddDate(0, 0, -h.cfg.TimersDays), h.cfg.BatchSize)
	if err != nil {
		return err
	}
	logsDeleted, err := deleteExpiredNotificationLog(ctx, tx, now.AddDate(0, 0, -h.cfg.NotificationLogDays), h.cfg.BatchSize)
	if err != nil {
		return err
	}
	// 只记数量级别，不落任何行内容——timers/notification_log 都可能携带通知
	// target 等敏感字段（安全约束：通知渠道内容对敏感字段脱敏，日志同样不能例外）。
	log.Printf("app: retention_sweep 清理完成 timers=%d notification_log=%d 行", timersDeleted, logsDeleted)

	next := h.cfg.SweepInterval
	if timersDeleted == h.cfg.BatchSize || logsDeleted == h.cfg.BatchSize {
		next = retentionCatchUpBackoff
	}
	return h.sched.Schedule(ctx, tx, TimerKindRetentionSweep, retentionSweepRefID, now.Add(next), nil)
}

// deleteExpiredTimers 删除一批已进入终态且 fire_at 早于 cutoff 的 timers 行，
// 返回实际删除行数（用于判断本轮是否删满一批、需要 1 分钟后快速续期）。
func deleteExpiredTimers(ctx context.Context, tx pgx.Tx, cutoff time.Time, batchSize int) (int, error) {
	tag, err := tx.Exec(ctx, `
		DELETE FROM timers WHERE id IN (
			SELECT id FROM timers
			WHERE status IN ('fired', 'failed', 'canceled') AND fire_at < $1
			LIMIT $2
		)
	`, cutoff, batchSize)
	if err != nil {
		return 0, fmt.Errorf("app: 清理过期 timers 失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// deleteExpiredNotificationLog 删除一批 created_at 早于 cutoff 的 notification_log 行。
func deleteExpiredNotificationLog(ctx context.Context, tx pgx.Tx, cutoff time.Time, batchSize int) (int, error) {
	tag, err := tx.Exec(ctx, `
		DELETE FROM notification_log WHERE id IN (
			SELECT id FROM notification_log
			WHERE created_at < $1
			LIMIT $2
		)
	`, cutoff, batchSize)
	if err != nil {
		return 0, fmt.Errorf("app: 清理过期 notification_log 失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
