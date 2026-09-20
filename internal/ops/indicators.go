package ops

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// recoveryNudgeTimerKind 与 internal/app.IncidentRecoveryNudgeTimerKind 同字面量。
// 任务 37 口径（主对话 2026-09-20 裁决）：恢复提醒「拦截数」落地为已发出的
// nudge 次数——count timers where kind=本常量 AND status='fired'。不变量 11
// 规定 nudge 永不推进状态，因此不存在「拦下一次自动关闭」这种事件。
const recoveryNudgeTimerKind = "incident_recovery_nudge"

// Indicators 是 GET /api/v1/ops/indicators 与日报先行指标小节的共享读模型。
// 指针字段为 nil 表示「无数据」（前端/日报显示「暂无数据」），禁止用 0 或 NaN 冒充。
// 计数字段的 0 是真实零（例如当前没有无 owner 的 active Fault）。
type Indicators struct {
	// FiringMappingRate 是 source_state='firing' 的 Alert 中已关联 Fault
	// （alert_mapping_results.status='mapped' 且 fault_id 非空）的比率，范围 [0,1]。
	// 分母为 0 时为 nil。
	FiringMappingRate *float64

	ActiveFaultsWithoutOwner int

	// FaultAckMedianSeconds 是 acked_at − first_detected_at 的中位数（秒）。
	// 无已认领样本时为 nil。
	FaultAckMedianSeconds *float64

	// RecoveryNudgeFiredCount 是 incident_recovery_nudge 已 fired 的定时器条数。
	RecoveryNudgeFiredCount int

	// UnresolvedIdentityCount 只统计当前仍未对齐的身份：last_seen >= 最近一次
	// 成功 ccloud 同步的 observed_at。没有任何成功同步记录时为 nil（uat 未开
	// ccloud 是常态），不能返回 0。
	UnresolvedIdentityCount *int
}

// QueryIndicators 在调用方事务内用一条 SQL 算出五个先行指标。不建表、不建物化视图。
func QueryIndicators(ctx context.Context, tx pgx.Tx) (Indicators, error) {
	var (
		firingTotal, firingMapped int
		ownerless                 int
		ackMedian                 *float64
		nudgeCount                int
		hasSync                   bool
		unresolvedN               int
	)
	err := tx.QueryRow(ctx, `
		WITH firing AS (
			SELECT
				count(*) FILTER (WHERE a.source_state = 'firing') AS total,
				count(*) FILTER (
					WHERE a.source_state = 'firing'
					  AND r.status = 'mapped'
					  AND r.fault_id IS NOT NULL
				) AS mapped
			FROM alerts a
			LEFT JOIN alert_mapping_results r ON r.alert_id = a.id
		),
		ownerless AS (
			SELECT count(*) AS n
			FROM faults
			WHERE status = 'active' AND owner_team_id IS NULL
		),
		ack_median AS (
			SELECT percentile_cont(0.5) WITHIN GROUP (
				ORDER BY extract(epoch FROM (acked_at - first_detected_at))
			) AS seconds
			FROM faults
			WHERE acked_at IS NOT NULL
		),
		nudges AS (
			SELECT count(*) AS n
			FROM timers
			WHERE kind = $1 AND status = 'fired'
		),
		last_sync AS (
			SELECT observed_at
			FROM topology_sync_runs
			WHERE source = 'ccloud' AND status = 'completed'
			ORDER BY observed_at DESC
			LIMIT 1
		),
		unresolved AS (
			SELECT
				EXISTS (SELECT 1 FROM last_sync) AS has_sync,
				COALESCE((
					SELECT count(*)::int
					FROM unresolved_identities u
					JOIN last_sync s ON u.last_seen >= s.observed_at
				), 0) AS n
		)
		SELECT
			firing.total, firing.mapped,
			ownerless.n,
			ack_median.seconds,
			nudges.n,
			unresolved.has_sync, unresolved.n
		FROM firing, ownerless, ack_median, nudges, unresolved
	`, recoveryNudgeTimerKind).Scan(
		&firingTotal, &firingMapped,
		&ownerless,
		&ackMedian,
		&nudgeCount,
		&hasSync, &unresolvedN,
	)
	if err != nil {
		return Indicators{}, fmt.Errorf("ops: 查询先行指标失败: %w", err)
	}

	out := Indicators{
		ActiveFaultsWithoutOwner: ownerless,
		RecoveryNudgeFiredCount:  nudgeCount,
		FaultAckMedianSeconds:    ackMedian,
	}
	if firingTotal > 0 {
		rate := float64(firingMapped) / float64(firingTotal)
		out.FiringMappingRate = &rate
	}
	if hasSync {
		n := unresolvedN
		out.UnresolvedIdentityCount = &n
	}
	return out, nil
}
