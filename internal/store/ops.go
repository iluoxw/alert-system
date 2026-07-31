package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// OpsRepo 是 team_ops_settings 表的仓储实现 + 运营统计 SQL（R2），约定同其余
// 仓储：无状态，事务由调用方通过 Store.WithTx 传入。
//
// 本文件刻意不引用 internal/ops：internal/ops（report.go）依赖 internal/notify，
// 而 internal/notify（handlers.go/policy.go/sender.go）依赖 internal/store——
// 若这里直接以 OpsSettings/OpsDailyStats 为出入参会形成
// store -> ops -> notify -> store 的导入环。OpsSettings/OpsChannelTarget/
// OpsDailyStats 是与 ops 包同名类型字段一一对应的本地镜像，调用方（internal/app
// 装配层）负责两套结构间的字段拷贝。
type OpsRepo struct{}

// NewOpsRepo 构造 OpsRepo。
func NewOpsRepo() *OpsRepo { return &OpsRepo{} }

// OpsChannelTarget 镜像 ops.ChannelTarget，对应 channels JSONB 数组元素。
type OpsChannelTarget struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// OpsSettings 镜像 ops.Settings，字段与 team_ops_settings 表列一一对应。
type OpsSettings struct {
	TeamID   string
	Timezone string

	DailyReportEnabled bool
	DailyReportTime    string

	StaleSweepEnabled         bool
	StaleSweepIntervalSeconds int

	OncallBroadcastEnabled bool
	OncallBroadcastTime    string

	Channels []OpsChannelTarget
}

// OpsDailyStats 镜像 ops.DailyStats，是 DailyStats 的统计结果。
type OpsDailyStats struct {
	NewCount      int
	ResolvedCount int
	BySeverity    map[string]int

	MTTAAvg         time.Duration
	MTTAAvgValid    bool
	MTTAMedian      time.Duration
	MTTAMedianValid bool

	Prev7DayAvgNew float64
}

// opsSettingsColumns 是 SELECT 列顺序，必须与 scanOpsSettings 的 Scan 顺序一一对应。
// team_id 显式 ::text（team_ops_settings.team_id 非空且唯一，不需要 COALESCE）。
const opsSettingsColumns = `
	team_id::text, timezone, daily_report_enabled, daily_report_time,
	stale_sweep_enabled, stale_sweep_interval_seconds,
	oncall_broadcast_enabled, oncall_broadcast_time, channels
`

// scanOpsSettings 按 opsSettingsColumns 的顺序扫出一行 OpsSettings；channels
// 先取原始 JSONB 字节再显式 json.Unmarshal，与 alerts.go labels 的处理方式一致。
func scanOpsSettings(row configRowScanner) (OpsSettings, error) {
	var s OpsSettings
	var channelsRaw []byte
	err := row.Scan(
		&s.TeamID, &s.Timezone, &s.DailyReportEnabled, &s.DailyReportTime,
		&s.StaleSweepEnabled, &s.StaleSweepIntervalSeconds,
		&s.OncallBroadcastEnabled, &s.OncallBroadcastTime, &channelsRaw,
	)
	if err != nil {
		return OpsSettings{}, err
	}
	if err := json.Unmarshal(channelsRaw, &s.Channels); err != nil {
		return OpsSettings{}, fmt.Errorf("store: 反序列化 ops channels 失败: %w", err)
	}
	return s, nil
}

// GetSettings 按 team_id 查询运营配置；不存在返回 ErrNotFound（团队尚未保存过
// 配置时，列默认值由调用方按需自行回退展示，本方法不臆造"零值即默认"的行为）。
func (r *OpsRepo) GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*OpsSettings, error) {
	s, err := scanOpsSettings(tx.QueryRow(ctx, `
		SELECT `+opsSettingsColumns+` FROM team_ops_settings WHERE team_id = $1
	`, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询团队运营配置失败: %w", err)
	}
	return &s, nil
}

// ListEnabledSettings 返回三个周期任务中任一开关为 true 的行，供启动对账
// （internal/app 启动时逐团队 ScheduleIfAbsent）扫描全量待续期任务。
func (r *OpsRepo) ListEnabledSettings(ctx context.Context, tx pgx.Tx) ([]OpsSettings, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+opsSettingsColumns+` FROM team_ops_settings
		WHERE daily_report_enabled OR stale_sweep_enabled OR oncall_broadcast_enabled
		ORDER BY team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出已启用运营配置失败: %w", err)
	}
	defer rows.Close()

	var settings []OpsSettings
	for rows.Next() {
		s, err := scanOpsSettings(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描运营配置失败: %w", err)
		}
		settings = append(settings, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历运营配置失败: %w", err)
	}
	return settings, nil
}

// UpsertSettings 以 team_id 为冲突键整行覆盖写入（每团队一行，ON CONFLICT DO
// UPDATE），供设置页保存接口调用；幂等——同一 team_id 多次调用只留一行。
func (r *OpsRepo) UpsertSettings(ctx context.Context, tx pgx.Tx, s OpsSettings) error {
	channels := s.Channels
	if channels == nil {
		channels = []OpsChannelTarget{} // nil 序列化成 JSON null，与列默认值 '[]' 语义不符
	}
	channelsJSON, err := json.Marshal(channels)
	if err != nil {
		return fmt.Errorf("store: 序列化 ops channels 失败: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO team_ops_settings (
			team_id, timezone, daily_report_enabled, daily_report_time,
			stale_sweep_enabled, stale_sweep_interval_seconds,
			oncall_broadcast_enabled, oncall_broadcast_time, channels
		) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (team_id) DO UPDATE SET
			timezone                     = EXCLUDED.timezone,
			daily_report_enabled         = EXCLUDED.daily_report_enabled,
			daily_report_time            = EXCLUDED.daily_report_time,
			stale_sweep_enabled          = EXCLUDED.stale_sweep_enabled,
			stale_sweep_interval_seconds = EXCLUDED.stale_sweep_interval_seconds,
			oncall_broadcast_enabled     = EXCLUDED.oncall_broadcast_enabled,
			oncall_broadcast_time        = EXCLUDED.oncall_broadcast_time,
			channels                     = EXCLUDED.channels
	`,
		s.TeamID, s.Timezone, s.DailyReportEnabled, s.DailyReportTime,
		s.StaleSweepEnabled, s.StaleSweepIntervalSeconds,
		s.OncallBroadcastEnabled, s.OncallBroadcastTime, channelsJSON,
	)
	if err != nil {
		return fmt.Errorf("store: upsert 团队运营配置失败: %w", err)
	}
	return nil
}

// DailyStats 统计 [dayStart, dayEnd) 窗口（起点含、终点不含）内团队的日报数据。
// 恢复数用 ends_at 列判定——alerts 表没有独立的 resolved_at 列，ends_at 就是
// resolved 语义的落地时刻（见 alerts.go UpdateStatus 注释）。四段查询都先用
// team_id 等值条件命中 alerts_team_id_idx 缩小到team 范围，避免全表扫描；
// 恢复数与未 Ack 严重告警额外命中 alerts_status_idx/alerts_severity_idx。
func (r *OpsRepo) DailyStats(ctx context.Context, tx pgx.Tx, teamID string, dayStart, dayEnd time.Time) (OpsDailyStats, error) {
	stats := OpsDailyStats{BySeverity: map[string]int{}}

	// 新增数 + 按 severity 分布：starts_at 落在窗口内即计入"新增"，与 status 无关。
	rows, err := tx.Query(ctx, `
		SELECT severity, count(*) FROM alerts
		WHERE team_id = $1::uuid AND starts_at >= $2 AND starts_at < $3
		GROUP BY severity
	`, teamID, dayStart, dayEnd)
	if err != nil {
		return OpsDailyStats{}, fmt.Errorf("store: 统计新增告警失败: %w", err)
	}
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			rows.Close()
			return OpsDailyStats{}, fmt.Errorf("store: 扫描新增告警分布失败: %w", err)
		}
		stats.BySeverity[sev] = n
		stats.NewCount += n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return OpsDailyStats{}, fmt.Errorf("store: 遍历新增告警分布失败: %w", err)
	}
	rows.Close()

	// 恢复数：resolved 且 ends_at 落窗——与"新增"是两个独立维度，一条告警可以
	// 在昨天新增、今天才恢复（此时只计入恢复数所在的那一天）。
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM alerts
		WHERE team_id = $1::uuid AND status = 'resolved' AND ends_at >= $2 AND ends_at < $3
	`, teamID, dayStart, dayEnd).Scan(&stats.ResolvedCount); err != nil {
		return OpsDailyStats{}, fmt.Errorf("store: 统计恢复告警失败: %w", err)
	}

	// MTTA 均值/中位：只统计窗内新增且已 Ack（acked_at 非空）的行。avg/
	// percentile_cont 在空集上返回 SQL NULL，用 *float64 承接以区分"无 Ack
	// 数据"（Valid=false）与"均值恰好为 0 秒"。
	var avgSeconds, medianSeconds *float64
	if err := tx.QueryRow(ctx, `
		SELECT
			avg(extract(epoch FROM (acked_at - starts_at))),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (acked_at - starts_at)))
		FROM alerts
		WHERE team_id = $1::uuid AND starts_at >= $2 AND starts_at < $3 AND acked_at IS NOT NULL
	`, teamID, dayStart, dayEnd).Scan(&avgSeconds, &medianSeconds); err != nil {
		return OpsDailyStats{}, fmt.Errorf("store: 统计 MTTA 失败: %w", err)
	}
	if avgSeconds != nil {
		stats.MTTAAvg = time.Duration(*avgSeconds * float64(time.Second))
		stats.MTTAAvgValid = true
	}
	if medianSeconds != nil {
		stats.MTTAMedian = time.Duration(*medianSeconds * float64(time.Second))
		stats.MTTAMedianValid = true
	}

	// 环比：窗口前 7 天（[dayStart-7d, dayStart)）的日均新增数，分母固定为 7
	// （不按团队实际上线天数折算），调用方（BuildDailyReport）把 0 当作
	// "无历史数据"的哨兵值处理。
	var prevCount int
	prevStart := dayStart.AddDate(0, 0, -7)
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM alerts WHERE team_id = $1::uuid AND starts_at >= $2 AND starts_at < $3
	`, teamID, prevStart, dayStart).Scan(&prevCount); err != nil {
		return OpsDailyStats{}, fmt.Errorf("store: 统计环比新增数失败: %w", err)
	}
	stats.Prev7DayAvgNew = float64(prevCount) / 7

	return stats, nil
}

// ListUnackedCritical 返回团队下未 Ack 的 firing critical 告警，供悬置扫描
// handler 使用。status = 'firing' 已蕴含未 Ack（domain.Ack 把状态机迁到
// acknowledged 时才写 acked_at，二者总是同步变化，见 domain/state.go），
// 这里不再重复加 acked_at IS NULL 条件。
func (r *OpsRepo) ListUnackedCritical(ctx context.Context, tx pgx.Tx, teamID string) ([]domain.Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+alertColumns+` FROM alerts
		WHERE team_id = $1::uuid AND status = 'firing' AND severity = 'critical'
		ORDER BY starts_at ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询未 Ack 严重告警失败: %w", err)
	}
	defer rows.Close()

	var alerts []domain.Alert
	for rows.Next() {
		a, err := scanAlertRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描告警失败: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未 Ack 严重告警失败: %w", err)
	}
	return alerts, nil
}

// ListOpenLongerThan 返回团队下未关闭（status != resolved）且 starts_at 早于
// olderThan 的告警，供日报"超时未关闭清单"使用；调用方传入 now.Add(-24h) 之类
// 的绝对时刻，而非时长——避免同一个"多久算超时"的常量在 store/ops 两处重复。
func (r *OpsRepo) ListOpenLongerThan(ctx context.Context, tx pgx.Tx, teamID string, olderThan time.Time) ([]domain.Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+alertColumns+` FROM alerts
		WHERE team_id = $1::uuid AND status != 'resolved' AND starts_at < $2
		ORDER BY starts_at ASC
	`, teamID, olderThan)
	if err != nil {
		return nil, fmt.Errorf("store: 查询超时未关闭告警失败: %w", err)
	}
	defer rows.Close()

	var alerts []domain.Alert
	for rows.Next() {
		a, err := scanAlertRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描告警失败: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历超时未关闭告警失败: %w", err)
	}
	return alerts, nil
}
