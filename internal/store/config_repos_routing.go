package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/domain"
)

// Route 对应 routes 表：有序规则列表，按 labels/resource_ref 把告警路由到目标团队
// （R5.1-5.3），并携带该路由下的分组通知窗口（R9）。
type Route struct {
	ID                    string
	Priority              int
	Matchers              domain.MatcherSet
	TargetTeamID          string
	GroupBy               []string
	GroupWaitSeconds      int
	GroupIntervalSeconds  int
	RepeatIntervalSeconds int
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// ErrRoutePriorityConflict 在 Create/Update 撞上 routes.priority 唯一约束时返回，
// 调用方（管理 API）据此给出「优先级已被占用」而非笼统的 500。
var ErrRoutePriorityConflict = errors.New("store: 路由 priority 已被占用")

const routeColumns = `id, priority, matchers, target_team_id, group_by, group_wait_seconds, group_interval_seconds, repeat_interval_seconds, created_at, updated_at`

func scanRoute(row configRowScanner) (Route, error) {
	var out Route
	var matchersRaw, groupByRaw []byte
	err := row.Scan(&out.ID, &out.Priority, &matchersRaw, &out.TargetTeamID, &groupByRaw,
		&out.GroupWaitSeconds, &out.GroupIntervalSeconds, &out.RepeatIntervalSeconds, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return Route{}, err
	}
	if err := json.Unmarshal(matchersRaw, &out.Matchers); err != nil {
		return Route{}, fmt.Errorf("反序列化路由 matchers 失败: %w", err)
	}
	if err := json.Unmarshal(groupByRaw, &out.GroupBy); err != nil {
		return Route{}, fmt.Errorf("反序列化路由 group_by 失败: %w", err)
	}
	return out, nil
}

// isPriorityConflict 判断 err 是否是 routes.priority 唯一约束被违反。
func isPriorityConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "routes_priority_key"
}

// RouteRepo 是 routes 表的仓储实现，约定同 TeamRepo（events.go 头部注释）。
type RouteRepo struct{}

func NewRouteRepo() *RouteRepo { return &RouteRepo{} }

func (r *RouteRepo) Create(ctx context.Context, tx pgx.Tx, route Route) (*Route, error) {
	if route.Matchers == nil {
		// nil slice 序列化成 JSON null 而非 []，与「无 matcher」的语义不符，
		// 且会让未来对 matchers 列做 JSONB 数组操作（如 @>、array_length）时行为不一致。
		route.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(route.Matchers)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化路由 matchers 失败: %w", err)
	}
	if route.GroupBy == nil {
		route.GroupBy = []string{}
	}
	groupByJSON, err := json.Marshal(route.GroupBy)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化路由 group_by 失败: %w", err)
	}

	out, err := scanRoute(tx.QueryRow(ctx, `
		INSERT INTO routes (priority, matchers, target_team_id, group_by, group_wait_seconds, group_interval_seconds, repeat_interval_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+routeColumns,
		route.Priority, matchersJSON, route.TargetTeamID, groupByJSON,
		route.GroupWaitSeconds, route.GroupIntervalSeconds, route.RepeatIntervalSeconds))
	if isPriorityConflict(err) {
		return nil, ErrRoutePriorityConflict
	}
	if err != nil {
		return nil, fmt.Errorf("store: 创建路由失败: %w", err)
	}
	return &out, nil
}

func (r *RouteRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Route, error) {
	out, err := scanRoute(tx.QueryRow(ctx, `SELECT `+routeColumns+` FROM routes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询路由失败: %w", err)
	}
	return &out, nil
}

// ListOrdered 按 priority 升序返回全部路由，供路由 stage 按优先级有序匹配（R5.1）。
func (r *RouteRepo) ListOrdered(ctx context.Context, tx pgx.Tx, limit int) ([]Route, error) {
	rows, err := tx.Query(ctx, `SELECT `+routeColumns+` FROM routes ORDER BY priority LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出路由失败: %w", err)
	}
	defer rows.Close()

	var routes []Route
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描路由失败: %w", err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历路由失败: %w", err)
	}
	return routes, nil
}

func (r *RouteRepo) Update(ctx context.Context, tx pgx.Tx, route Route) error {
	if route.Matchers == nil {
		route.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(route.Matchers)
	if err != nil {
		return fmt.Errorf("store: 序列化路由 matchers 失败: %w", err)
	}
	if route.GroupBy == nil {
		route.GroupBy = []string{}
	}
	groupByJSON, err := json.Marshal(route.GroupBy)
	if err != nil {
		return fmt.Errorf("store: 序列化路由 group_by 失败: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE routes SET priority = $2, matchers = $3, target_team_id = $4, group_by = $5,
			group_wait_seconds = $6, group_interval_seconds = $7, repeat_interval_seconds = $8
		WHERE id = $1
	`, route.ID, route.Priority, matchersJSON, route.TargetTeamID, groupByJSON,
		route.GroupWaitSeconds, route.GroupIntervalSeconds, route.RepeatIntervalSeconds)
	if isPriorityConflict(err) {
		return ErrRoutePriorityConflict
	}
	if err != nil {
		return fmt.Errorf("store: 更新路由失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *RouteRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM routes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除路由失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Schedule 对应 schedules 表：每团队一张值班表，在固定时区下求值（R5.5）。
type Schedule struct {
	ID        string
	TeamID    string
	Name      string
	Timezone  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const scheduleColumns = `id, team_id, name, timezone, created_at, updated_at`

func scanSchedule(row configRowScanner) (Schedule, error) {
	var s Schedule
	err := row.Scan(&s.ID, &s.TeamID, &s.Name, &s.Timezone, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// ScheduleRepo 是 schedules 表的仓储实现，约定同 TeamRepo。
type ScheduleRepo struct{}

func NewScheduleRepo() *ScheduleRepo { return &ScheduleRepo{} }

func (r *ScheduleRepo) Create(ctx context.Context, tx pgx.Tx, s Schedule) (*Schedule, error) {
	out, err := scanSchedule(tx.QueryRow(ctx, `
		INSERT INTO schedules (team_id, name, timezone) VALUES ($1, $2, $3)
		RETURNING `+scheduleColumns, s.TeamID, s.Name, s.Timezone))
	if err != nil {
		return nil, fmt.Errorf("store: 创建值班表失败: %w", err)
	}
	return &out, nil
}

func (r *ScheduleRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Schedule, error) {
	out, err := scanSchedule(tx.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询值班表失败: %w", err)
	}
	return &out, nil
}

// ListByTeam 返回团队下全部值班表。
func (r *ScheduleRepo) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]Schedule, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+scheduleColumns+` FROM schedules WHERE team_id = $1 ORDER BY name LIMIT $2
	`, teamID, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出团队值班表失败: %w", err)
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描值班表失败: %w", err)
		}
		schedules = append(schedules, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历值班表失败: %w", err)
	}
	return schedules, nil
}

func (r *ScheduleRepo) Update(ctx context.Context, tx pgx.Tx, id, name, timezone string) error {
	tag, err := tx.Exec(ctx, `UPDATE schedules SET name = $2, timezone = $3 WHERE id = $1`, id, name, timezone)
	if err != nil {
		return fmt.Errorf("store: 更新值班表失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *ScheduleRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除值班表失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Shift 对应 shifts 表：物化班次（起止时间戳，非轮换规则），使「某时刻谁在值班」
// 是一次普通的区间查询（R5.5）。
type Shift struct {
	ID         string
	ScheduleID string
	UserID     string
	StartsAt   time.Time
	EndsAt     time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const shiftColumns = `id, schedule_id, user_id, starts_at, ends_at, created_at, updated_at`

func scanShift(row configRowScanner) (Shift, error) {
	var s Shift
	err := row.Scan(&s.ID, &s.ScheduleID, &s.UserID, &s.StartsAt, &s.EndsAt, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// ShiftRepo 是 shifts 表的仓储实现，约定同 TeamRepo。
type ShiftRepo struct{}

func NewShiftRepo() *ShiftRepo { return &ShiftRepo{} }

func (r *ShiftRepo) Create(ctx context.Context, tx pgx.Tx, s Shift) (*Shift, error) {
	out, err := scanShift(tx.QueryRow(ctx, `
		INSERT INTO shifts (schedule_id, user_id, starts_at, ends_at) VALUES ($1, $2, $3, $4)
		RETURNING `+shiftColumns, s.ScheduleID, s.UserID, s.StartsAt, s.EndsAt))
	if err != nil {
		return nil, fmt.Errorf("store: 创建班次失败: %w", err)
	}
	return &out, nil
}

func (r *ShiftRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Shift, error) {
	out, err := scanShift(tx.QueryRow(ctx, `SELECT `+shiftColumns+` FROM shifts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询班次失败: %w", err)
	}
	return &out, nil
}

func (r *ShiftRepo) Update(ctx context.Context, tx pgx.Tx, s Shift) error {
	tag, err := tx.Exec(ctx, `
		UPDATE shifts SET user_id = $2, starts_at = $3, ends_at = $4 WHERE id = $1
	`, s.ID, s.UserID, s.StartsAt, s.EndsAt)
	if err != nil {
		return fmt.Errorf("store: 更新班次失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOverlapping 返回同一 schedule 下与 [from, to) 有重叠的班次，供编辑/新建班次前
// 校验值班表内班次互不重叠（半开区间语义：starts_at < to AND ends_at > from——
// 严格相邻，即一条的 ends_at 恰好等于另一条的 starts_at，不算重叠，允许无缝衔接排班）。
// excludeID 非空时排除该 id，供编辑现有班次时排除自身。
func (r *ShiftRepo) ListOverlapping(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, excludeID string) ([]Shift, error) {
	// excludeID 传空串时以 NULL 绑定：id 列是 uuid，若直接绑定 ""，Postgres 会把
	// $4 参数类型推断为 uuid（因 id != $4 那处比较），导致 "" 触发非法 uuid 报错。
	var excludeArg any
	if excludeID != "" {
		excludeArg = excludeID
	}
	rows, err := tx.Query(ctx, `
		SELECT `+shiftColumns+`
		FROM shifts
		WHERE schedule_id = $1 AND starts_at < $3 AND ends_at > $2 AND ($4::uuid IS NULL OR id != $4)
		ORDER BY starts_at
	`, scheduleID, from, to, excludeArg)
	if err != nil {
		return nil, fmt.Errorf("store: 查询重叠班次失败: %w", err)
	}
	defer rows.Close()

	var shifts []Shift
	for rows.Next() {
		s, err := scanShift(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描班次失败: %w", err)
		}
		shifts = append(shifts, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历重叠班次失败: %w", err)
	}
	return shifts, nil
}

// ListInWindow 返回与 [from, to) 有重叠的班次（起止区间求交），按开始时间升序，
// 供值班求值（Resolver.OnCallAt）在应用层挑出覆盖具体时刻的那一条。
func (r *ShiftRepo) ListInWindow(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, limit int) ([]Shift, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+shiftColumns+`
		FROM shifts
		WHERE schedule_id = $1 AND starts_at < $3 AND ends_at > $2
		ORDER BY starts_at
		LIMIT $4
	`, scheduleID, from, to, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询班次失败: %w", err)
	}
	defer rows.Close()

	var shifts []Shift
	for rows.Next() {
		s, err := scanShift(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描班次失败: %w", err)
		}
		shifts = append(shifts, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历班次失败: %w", err)
	}
	return shifts, nil
}

func (r *ShiftRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM shifts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除班次失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
