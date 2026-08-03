package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// AlertRepo 是 alerts 表（去重后的告警实体）的仓储实现（R3/R4/R9/R10）。
// 与 EventQueue 一致：无状态，所有方法的连接/事务由调用方传入。
type AlertRepo struct{}

// NewAlertRepo 构造 AlertRepo。
func NewAlertRepo() *AlertRepo {
	return &AlertRepo{}
}

// alertColumns 是 SELECT/RETURNING 共用的列顺序，必须与 scanAlert 的 Scan 顺序一一对应。
const alertColumns = `
	id, fingerprint, status, severity, severity_fallback, title, source,
	labels, resource_ref, signal_context, resolve_policy, ttl_seconds,
	COALESCE(source_integration_id::text, ''), dedup_count,
	COALESCE(group_key, ''), COALESCE(team_id::text, ''), COALESCE(route_id::text, ''), silenced_by,
	COALESCE(acked_by::text, ''), acked_at, starts_at, last_seen_at, ends_at,
	flap_count, flap_window_start, flapping,
	created_at, updated_at,
	upstream_fingerprint, source_state, response_state, suppression_state,
	suppression_details, source_updated_at, last_reconciled_at,
	reconcile_generation, reconcile_miss_count, COALESCE(source_resolution_reason, '')
`

// alertRowScanner 是 pgx.Row 与 pgx.Rows 共有的最小接口，让 scanAlertRow 同时服务于
// QueryRow（单行，如 GetByID/UpsertByFingerprint 的 RETURNING）与 Query（多行，List）。
// 独立于其他仓储文件自行定义（不复用包内同名类型），避免跨文件的隐式耦合。
type alertRowScanner interface {
	Scan(dest ...any) error
}

// scanAlertRow 按 alertColumns 的顺序从 rs 中 Scan 出一个 domain.Alert；extra 用于
// 追加 RETURNING 里 alertColumns 之外的列（如 UpsertByFingerprint 的 (xmax = 0) AS created）。
// ttl_seconds/acked_at/ends_at/flap_window_start 在数据库里均可为 NULL，但 domain.Alert
// 对应字段是值类型（非指针），因此先扫到可空的中间变量再转换，避免 "cannot scan NULL
// into *int/*time.Time"。
// labels/resource_ref/signal_context 同样先取原始 JSONB 字节再显式 json.Unmarshal，
// 避免依赖 pgx 对 map/RawMessage 的隐式编解码行为（与 events.go 的 payload 处理方式一致）。
func scanAlertRow(rs alertRowScanner, extra ...any) (domain.Alert, error) {
	var (
		a                                                                   domain.Alert
		labelsRaw, resourceRefRaw, signalContextRaw, suppressionDetailsRaw  []byte
		ttlSeconds                                                          *int
		ackedAt, endsAt, flapWindowStart, sourceUpdatedAt, lastReconciledAt *time.Time
	)

	dest := []any{
		&a.ID, &a.Fingerprint, &a.Status, &a.Severity, &a.SeverityFallback, &a.Title, &a.Source,
		&labelsRaw, &resourceRefRaw, &signalContextRaw, &a.ResolvePolicy, &ttlSeconds,
		&a.SourceIntegrationID, &a.DedupCount,
		&a.GroupKey, &a.TeamID, &a.RouteID, &a.SilencedBy,
		&a.AckedBy, &ackedAt, &a.StartsAt, &a.LastSeenAt, &endsAt,
		&a.FlapCount, &flapWindowStart, &a.Flapping,
		&a.CreatedAt, &a.UpdatedAt,
		&a.UpstreamFingerprint, &a.SourceState, &a.ResponseState, &a.SuppressionState,
		&suppressionDetailsRaw, &sourceUpdatedAt, &lastReconciledAt,
		&a.ReconcileGeneration, &a.ReconcileMissCount, &a.SourceResolutionReason,
	}
	dest = append(dest, extra...)

	if err := rs.Scan(dest...); err != nil {
		return domain.Alert{}, err
	}

	if ttlSeconds != nil {
		a.TTLSeconds = *ttlSeconds
	}
	if ackedAt != nil {
		a.AckedAt = *ackedAt
	}
	if endsAt != nil {
		a.EndsAt = *endsAt
	}
	if flapWindowStart != nil {
		a.FlapWindowStart = *flapWindowStart
	}
	if sourceUpdatedAt != nil {
		a.SourceUpdatedAt = *sourceUpdatedAt
	}
	if lastReconciledAt != nil {
		a.LastReconciledAt = *lastReconciledAt
	}
	if err := json.Unmarshal(labelsRaw, &a.Labels); err != nil {
		return domain.Alert{}, fmt.Errorf("store: 反序列化 labels 失败: %w", err)
	}
	if err := json.Unmarshal(resourceRefRaw, &a.ResourceRef); err != nil {
		return domain.Alert{}, fmt.Errorf("store: 反序列化 resource_ref 失败: %w", err)
	}
	a.SignalContext = json.RawMessage(signalContextRaw)
	if err := json.Unmarshal(suppressionDetailsRaw, &a.SuppressionDetails); err != nil {
		return domain.Alert{}, fmt.Errorf("store: 反序列化 suppression_details 失败: %w", err)
	}

	return a, nil
}

// UpsertByFingerprint 是 R3.3/R3.4 去重的唯一写入点：单条 SQL 以
// `INSERT ... ON CONFLICT (fingerprint) WHERE status != 'resolved' DO UPDATE`
// 落库。conflict_target 的 WHERE 谓词必须与 migrations 里的 partial unique index
// （alerts_fingerprint_unresolved_uidx）一致，否则 Postgres 不会用该索引做冲突judge。
//
//   - 若同 fingerprint 不存在未 resolved 的行：新建一行（dedup_count=1），created=true。
//   - 若存在：dedup_count+1、更新 last_seen_at/severity/title/labels/resource_ref/signal_context
//     为本次事件的最新值（R3.3），created=false；同 fingerprint 若已 resolved，
//     该唯一索引不覆盖 resolved 行，天然会新建一行（R3.4，不复活旧告警）。
//
// created 通过 `xmax = 0` 判断：刚插入的新行 xmax 为 0，ON CONFLICT DO UPDATE
// 产生的新元组版本 xmax 非零（PostgreSQL 对 upsert 判断新建/更新的标准手法）。
func (r *AlertRepo) UpsertByFingerprint(ctx context.Context, tx pgx.Tx, event domain.AlertEvent, integrationID string) (domain.Alert, bool, error) {
	eventLabels := event.Labels
	if eventLabels == nil {
		eventLabels = map[string]string{}
	}
	eventResourceRef := event.ResourceRef
	if eventResourceRef == nil {
		eventResourceRef = map[string]string{}
	}
	labels, err := json.Marshal(eventLabels)
	if err != nil {
		return domain.Alert{}, false, fmt.Errorf("store: 序列化 labels 失败: %w", err)
	}
	resourceRef, err := json.Marshal(eventResourceRef)
	if err != nil {
		return domain.Alert{}, false, fmt.Errorf("store: 序列化 resource_ref 失败: %w", err)
	}
	signalContext := event.SignalContext
	if signalContext == nil {
		signalContext = json.RawMessage("{}")
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO alerts (
			fingerprint, status, severity, severity_fallback, title, source,
			labels, resource_ref, signal_context, resolve_policy, ttl_seconds,
			source_integration_id, dedup_count, starts_at, last_seen_at
		) VALUES (
			$1, 'firing', $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			NULLIF($11, '')::uuid, 1, $12, $12
		)
		ON CONFLICT (fingerprint) WHERE status != 'resolved' AND upstream_fingerprint = '' DO UPDATE SET
			dedup_count = alerts.dedup_count + 1,
			last_seen_at = EXCLUDED.last_seen_at,
			severity = EXCLUDED.severity,
			severity_fallback = EXCLUDED.severity_fallback,
			title = EXCLUDED.title,
			labels = EXCLUDED.labels,
			resource_ref = EXCLUDED.resource_ref,
			signal_context = EXCLUDED.signal_context
		RETURNING`+alertColumns+`, (xmax = 0) AS created
	`,
		event.Fingerprint, event.Severity, event.SeverityFallback, event.Title, event.Source,
		labels, resourceRef, []byte(signalContext), event.ResolvePolicy, nullableTTL(event),
		integrationID, event.StartsAt,
	)

	var created bool
	a, err := scanAlertRow(row, &created)
	if err != nil {
		return domain.Alert{}, false, fmt.Errorf("store: upsert 告警失败: %w", err)
	}

	return a, created, nil
}

// nullableTTL 仅当 ResolvePolicy 为 ttl 时下发 TTLSeconds，否则写 NULL——
// 与 alerts.ttl_seconds 列（可空 INT）语义对齐，避免非 ttl 策略的告警上出现无意义的 0。
func nullableTTL(event domain.AlertEvent) any {
	if event.ResolvePolicy != domain.ResolvePolicyTTL {
		return nil
	}
	return event.TTLSeconds
}

// ErrAlertNotFound 表示按 ID 查询/更新的告警不存在。
var ErrAlertNotFound = errors.New("store: 告警不存在")

// GetByID 按 ID 查询单条告警；不存在返回 ErrAlertNotFound。
func (r *AlertRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	row := tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts WHERE id = $1`, id)
	a, err := scanAlertRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Alert{}, ErrAlertNotFound
		}
		return domain.Alert{}, fmt.Errorf("store: 查询告警失败: %w", err)
	}
	return a, nil
}

// GetByFingerprint 按 fingerprint 查询未 resolved 的告警；不存在返回 ErrAlertNotFound。
// 唯一性由 alerts_fingerprint_unresolved_uidx（(fingerprint) WHERE status != 'resolved'
// 的 partial unique index）保证：该谓词下至多一行，因此天然无需 LIMIT/排序。
func (r *AlertRepo) GetByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint string) (domain.Alert, error) {
	row := tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts WHERE fingerprint = $1 AND status != 'resolved'`, fingerprint)
	a, err := scanAlertRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Alert{}, ErrAlertNotFound
		}
		return domain.Alert{}, fmt.Errorf("store: 按 fingerprint 查询告警失败: %w", err)
	}
	return a, nil
}

// GetLatestResolvedByFingerprint 按 fingerprint 查询最近一条已 resolved 的历史行
// （按 created_at 倒序取第一条），供 dedup stage 在 refire（resolved 后同 fingerprint
// 再次 firing）时延续 flap_count/flap_window_start 基线——alerts_fingerprint_unresolved_uidx
// 是排除 resolved 行的 partial unique index，resolved 是终态，故同 fingerprint 可能存在
// 多条历史行，取最近一条即上一轮生命周期的收尾状态。不存在时返回 ErrAlertNotFound
// （该 fingerprint 首次出现）。走 alerts_fingerprint_resolved_idx（迁移 000009），
// 避免在 dedup 热路径里对 resolved 历史行做全表顺序扫描。
func (r *AlertRepo) GetLatestResolvedByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint string) (domain.Alert, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+alertColumns+` FROM alerts WHERE fingerprint = $1 AND status = 'resolved' ORDER BY created_at DESC LIMIT 1
	`, fingerprint)
	a, err := scanAlertRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Alert{}, ErrAlertNotFound
		}
		return domain.Alert{}, fmt.Errorf("store: 查询历史 resolved 告警失败: %w", err)
	}
	return a, nil
}

// UpdateStatus 把状态机产出的新状态持久化：status 必写；acked_by/acked_at 仅在
// Ack 后有值；ends_at（对应 resolved_at 语义，表里无独立同名列）仅在 Resolve 后
// 有值。调用方（状态机编排层）负责先用 domain.Ack/Resolve/Handoff 算出目标
// Alert，本方法只做一次整体列更新，不重复做转移合法性校验（该校验属于 domain
// 层职责）。domain 层以零值 time.Time 表示「未设置」，这里转换为 SQL NULL。
func (r *AlertRepo) UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	responseState := alert.ResponseState
	if !responseState.IsValid() {
		if alert.Status == domain.StatusAcknowledged {
			responseState = domain.ResponseStateAcknowledged
		} else {
			responseState = domain.ResponseStateUnacknowledged
		}
	}
	sourceState := alert.SourceState
	if !sourceState.IsValid() {
		if alert.Status == domain.StatusResolved {
			sourceState = domain.SourceStateResolved
		} else {
			sourceState = domain.SourceStateFiring
		}
	}
	if !alert.IsAuthoritativeSource() && alert.Status == domain.StatusResolved {
		sourceState = domain.SourceStateResolved
	}
	tag, err := tx.Exec(ctx, `
		UPDATE alerts SET
			status = $2,
			acked_by = NULLIF($3, '')::uuid,
			acked_at = $4,
			ends_at = $5,
			response_state = $6,
			source_state = $7
		WHERE id = $1
	`, alert.ID, alert.Status, alert.AckedBy, nullableTime(alert.AckedAt), nullableTime(alert.EndsAt), responseState, sourceState)
	if err != nil {
		return fmt.Errorf("store: 更新告警状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// nullableTime 把 domain 层「零值 time.Time 表示未设置」的约定翻译成 SQL NULL，
// 供 pgx 按 *time.Time 编码。
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// UpdateRouting 把路由 stage（R5.1-5.3）求出的 team_id/route_id 与分组 stage（R9）
// 求出的 group_key 一并写回 alerts 表；三个字段分属不同 stage 计算，但同落在这一次
// UPDATE 里，调用方各自传入自己负责字段的目标值 + 另外两个字段的既有值（透传），
// 互不覆盖对方负责的列。route_id 为空字符串表示未命中任何路由规则、落到默认团队
// 兜底，此时写 NULL（而不是尝试把空串 cast 成 uuid）。
func (r *AlertRepo) UpdateRouting(ctx context.Context, tx pgx.Tx, id, teamID, groupKey, routeID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE alerts SET team_id = NULLIF($2, '')::uuid, group_key = NULLIF($3, ''), route_id = NULLIF($4, '')::uuid WHERE id = $1
	`, id, teamID, groupKey, routeID)
	if err != nil {
		return fmt.Errorf("store: 更新告警路由信息失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// UpdateFlapState 把 dedup stage 在 firing↔resolved 转移点算出的 flap 三字段
// （aiops-foundation R4/C4）落库：flapWindowStart 为零值时写 NULL（与列可空语义
// 对齐，语义为"该行尚无翻转窗口"，理论上不会发生——evaluateFlap 每次都会给出
// 非零 windowStart——保留 nullableTime 转换只是与本文件其余方法的口径一致）。
func (r *AlertRepo) UpdateFlapState(ctx context.Context, tx pgx.Tx, id string, flapCount int, flapWindowStart time.Time, flapping bool) error {
	tag, err := tx.Exec(ctx, `
		UPDATE alerts SET flap_count = $2, flap_window_start = $3, flapping = $4 WHERE id = $1
	`, id, flapCount, nullableTime(flapWindowStart), flapping)
	if err != nil {
		return fmt.Errorf("store: 更新告警 flap 状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// CountByRouteSince 统计 routeID 路由下 since 之后创建的告警数，供风暴熔断判定
// （design.md C3）读取——走 idx_alerts_route_created (route_id, created_at) 索引，
// 避免全表扫描或 group_key LIKE 前缀匹配。
func (r *AlertRepo) CountByRouteSince(ctx context.Context, tx pgx.Tx, routeID string, since time.Time) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM alerts WHERE route_id = $1::uuid AND created_at >= $2
	`, routeID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: 统计路由近窗口告警数失败: %w", err)
	}
	return count, nil
}

// ListByGroupKey 按 group_key 返回组内全部活跃（非 resolved）告警，按 starts_at
// 升序（组首在前）——供分组 stage 到期 handler 取出组内成员交给聚合通知。已
// resolved 的成员不参与聚合发送，但它们的 group_key 不会被清空（分组 stage 的
// 口径），因此这里显式排除，避免通知里出现早已恢复的告警。
func (r *AlertRepo) ListByGroupKey(ctx context.Context, tx pgx.Tx, groupKey string) ([]domain.Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+alertColumns+` FROM alerts WHERE group_key = $1 AND status != 'resolved' ORDER BY starts_at ASC
	`, groupKey)
	if err != nil {
		return nil, fmt.Errorf("store: 按 group_key 查询组内告警失败: %w", err)
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
		return nil, fmt.Errorf("store: 遍历组内告警失败: %w", err)
	}
	return alerts, nil
}

// ListActiveByRoute 返回 routeID 下当前活跃（非 resolved）的告警，按 last_seen_at
// 倒序（最新在前）——供 storm_summary handler（design.md C3）在同一次查询里同时取
// "代表样本标题"（取最新一条）与"distinct group_key"（熔断解除后补一轮组到期定时器，
// 保证被熔断吞掉的组通知最终送达）两处用途复用，不必分别查两次。
func (r *AlertRepo) ListActiveByRoute(ctx context.Context, tx pgx.Tx, routeID string) ([]domain.Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+alertColumns+` FROM alerts WHERE route_id = $1::uuid AND status != 'resolved' ORDER BY last_seen_at DESC
	`, routeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询路由活跃告警失败: %w", err)
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
		return nil, fmt.Errorf("store: 遍历路由活跃告警失败: %w", err)
	}
	return alerts, nil
}

// AlertFilter 是 List 支持的组合过滤条件；字段为零值（""/nil/0）表示不按该维度过滤。
type AlertFilter struct {
	Severity         domain.Severity
	Status           domain.Status
	SourceState      domain.SourceState
	ResponseState    domain.ResponseState
	SuppressionState domain.SuppressionState
	SourceID         string
	TeamID           string
	// Labels 为空 map 或 nil 表示不按标签过滤；非空时用 JSONB `@>` 包含匹配，
	// 即要求 alerts.labels 包含这里给出的全部 key/value（AND 语义）。
	Labels map[string]string

	Limit  int // <= 0 时使用默认值 50
	Offset int
}

// AlertPage 是 List 的分页结果：Alerts 为当前页数据，Total 为满足过滤条件的总行数
// （不受 Limit/Offset 影响，用于前端渲染分页控件）。
type AlertPage struct {
	Alerts []domain.Alert
	Total  int
}

// DefaultAlertPageLimit 是 List 在 filter.Limit <= 0 时使用的默认分页大小；导出
// 供 internal/api 的列表端点在响应里回显"实际生效的 limit"时复用同一常量，避免
// 两处各自维护一份可能漂移的默认值。
const DefaultAlertPageLimit = 50

// List 按 severity/status/team/labels 组合过滤（AND 语义）+ 分页，按 last_seen_at 倒序
// 返回（NFR-Usability：告警列表支持按 severity/status/team/label 过滤）。
func (r *AlertRepo) List(ctx context.Context, tx pgx.Tx, filter AlertFilter) (AlertPage, error) {
	var (
		clauses []string
		args    []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.Severity != "" {
		clauses = append(clauses, "severity = "+arg(filter.Severity))
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = "+arg(filter.Status))
	}
	if filter.SourceState != "" {
		clauses = append(clauses, "source_state = "+arg(filter.SourceState))
	}
	if filter.ResponseState != "" {
		clauses = append(clauses, "response_state = "+arg(filter.ResponseState))
	}
	if filter.SuppressionState != "" {
		clauses = append(clauses, "suppression_state = "+arg(filter.SuppressionState))
	}
	if filter.SourceID != "" {
		clauses = append(clauses, "source_integration_id = "+arg(filter.SourceID)+"::uuid")
	}
	if filter.TeamID != "" {
		clauses = append(clauses, "team_id = "+arg(filter.TeamID)+"::uuid")
	}
	if len(filter.Labels) > 0 {
		labelsJSON, err := json.Marshal(filter.Labels)
		if err != nil {
			return AlertPage{}, fmt.Errorf("store: 序列化 label 过滤条件失败: %w", err)
		}
		clauses = append(clauses, "labels @> "+arg(labelsJSON)+"::jsonb")
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultAlertPageLimit
	}

	var total int
	countSQL := "SELECT count(*) FROM alerts " + where
	if err := tx.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return AlertPage{}, fmt.Errorf("store: 统计告警总数失败: %w", err)
	}

	limitArg := arg(limit)
	offsetArg := arg(filter.Offset)
	listSQL := "SELECT " + alertColumns + " FROM alerts " + where +
		" ORDER BY last_seen_at DESC LIMIT " + limitArg + " OFFSET " + offsetArg

	rows, err := tx.Query(ctx, listSQL, args...)
	if err != nil {
		return AlertPage{}, fmt.Errorf("store: 查询告警列表失败: %w", err)
	}
	defer rows.Close()

	var alerts []domain.Alert
	for rows.Next() {
		a, err := scanAlertRow(rows)
		if err != nil {
			return AlertPage{}, fmt.Errorf("store: 扫描告警失败: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return AlertPage{}, fmt.Errorf("store: 遍历告警列表失败: %w", err)
	}

	return AlertPage{Alerts: alerts, Total: total}, nil
}

// FingerprintStats 统计同 fingerprint 自 since 起的历史发生次数（含全部状态）与
// 已解决样本的平均持续时长——供告警详情页「噪声画像」（ui-enrichment）读取。
// avgSeconds 在无已解决样本时返回 nil（而非 0），避免前端把"没有样本"误读成
// "平均耗时 0 秒"。走 alerts_fingerprint_starts_idx（迁移 000010），
// 覆盖全部状态历史行的扫描。
func (r *AlertRepo) FingerprintStats(ctx context.Context, tx pgx.Tx, fingerprint string, since time.Time) (int, *float64, error) {
	var (
		count      int
		avgSeconds *float64
	)
	err := tx.QueryRow(ctx, `
		SELECT
			count(*),
			avg(EXTRACT(EPOCH FROM (ends_at - starts_at))) FILTER (WHERE status = 'resolved' AND ends_at IS NOT NULL)
		FROM alerts
		WHERE fingerprint = $1 AND starts_at >= $2
	`, fingerprint, since).Scan(&count, &avgSeconds)
	if err != nil {
		return 0, nil, fmt.Errorf("store: 统计 fingerprint 历史发生次数失败: %w", err)
	}
	return count, avgSeconds, nil
}

// ResolvedRef 是 ListRecentResolvedByFingerprint 的查询投影（非 domain.Alert 的
// 全量列），只承载「噪声画像」最近已解决记录卡片所需的字段。
type ResolvedRef struct {
	AlertID  string
	StartsAt time.Time
	EndsAt   time.Time
	Note     string
	Reason   string
}

// ListRecentResolvedByFingerprint 按 ends_at 倒序返回同 fingerprint 最近 limit 条
// 已解决历史（排除 excludeID 自身，即当前查看的这一条），并各自带上处置时写入
// timeline_entries（action='resolve'）的 note/reason（domain.Resolve 的落库口径，
// 见 internal/domain/state.go）。LATERAL 子查询按 created_at DESC LIMIT 1 取最新
// 一条 resolve 记录是防御性写法——幂等短路（R6.6）保证正常情况下每条告警只有一条
// resolve 时间线条目，不依赖这个前提也能拿到确定结果。
func (r *AlertRepo) ListRecentResolvedByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint, excludeID string, limit int) ([]ResolvedRef, error) {
	// excludeID 传空串时以 NULL 绑定：id 列是 uuid，若直接绑定 ""，Postgres 会把
	// $2 参数类型推断为 uuid（因 a.id != $2 那处比较），导致 "" 触发非法 uuid 报错
	// （同 ShiftRepo.ListOverlapping 的既有解法）。
	var excludeArg any
	if excludeID != "" {
		excludeArg = excludeID
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.starts_at, a.ends_at,
			COALESCE(t.detail->>'note', ''), COALESCE(t.detail->>'reason', '')
		FROM alerts a
		LEFT JOIN LATERAL (
			SELECT detail FROM timeline_entries
			WHERE alert_id = a.id AND action = 'resolve'
			ORDER BY created_at DESC LIMIT 1
		) t ON true
		WHERE a.fingerprint = $1 AND a.status = 'resolved' AND a.ends_at IS NOT NULL
			AND ($2::uuid IS NULL OR a.id != $2)
		ORDER BY a.ends_at DESC
		LIMIT $3
	`, fingerprint, excludeArg, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询最近已解决历史失败: %w", err)
	}
	defer rows.Close()

	var refs []ResolvedRef
	for rows.Next() {
		var ref ResolvedRef
		if err := rows.Scan(&ref.AlertID, &ref.StartsAt, &ref.EndsAt, &ref.Note, &ref.Reason); err != nil {
			return nil, fmt.Errorf("store: 扫描最近已解决历史失败: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历最近已解决历史失败: %w", err)
	}
	return refs, nil
}
