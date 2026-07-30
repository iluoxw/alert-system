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

// Silence 对应 silences 表：label matcher + 时间窗的维护静默（R10）。
type Silence struct {
	ID        string
	Matchers  domain.MatcherSet
	StartsAt  time.Time
	EndsAt    time.Time
	CreatedBy string // 可为空字符串（created_by 列可空）
	Comment   string // 可为空字符串（comment 列可空）
	CreatedAt time.Time
	UpdatedAt time.Time
}

const silenceColumns = `id, matchers, starts_at, ends_at, created_by, comment, created_at, updated_at`

func scanSilence(row configRowScanner) (Silence, error) {
	var s Silence
	var matchersRaw []byte
	var createdBy, comment *string
	err := row.Scan(&s.ID, &matchersRaw, &s.StartsAt, &s.EndsAt, &createdBy, &comment, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return Silence{}, err
	}
	s.CreatedBy = nullableString(createdBy)
	s.Comment = nullableString(comment)
	if err := json.Unmarshal(matchersRaw, &s.Matchers); err != nil {
		return Silence{}, fmt.Errorf("反序列化静默 matchers 失败: %w", err)
	}
	return s, nil
}

// SilenceRepo 是 silences 表的仓储实现，约定同 TeamRepo（events.go 头部注释）。
type SilenceRepo struct{}

func NewSilenceRepo() *SilenceRepo { return &SilenceRepo{} }

func (r *SilenceRepo) Create(ctx context.Context, tx pgx.Tx, s Silence) (*Silence, error) {
	if s.Matchers == nil {
		s.Matchers = domain.MatcherSet{}
	}
	matchersJSON, err := json.Marshal(s.Matchers)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化静默 matchers 失败: %w", err)
	}
	out, err := scanSilence(tx.QueryRow(ctx, `
		INSERT INTO silences (matchers, starts_at, ends_at, created_by, comment)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+silenceColumns,
		matchersJSON, s.StartsAt, s.EndsAt, nullableStringArg(s.CreatedBy), nullableStringArg(s.Comment)))
	if err != nil {
		return nil, fmt.Errorf("store: 创建静默失败: %w", err)
	}
	return &out, nil
}

func (r *SilenceRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Silence, error) {
	out, err := scanSilence(tx.QueryRow(ctx, `SELECT `+silenceColumns+` FROM silences WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询静默失败: %w", err)
	}
	return &out, nil
}

// List 按开始时间倒序返回静默规则，供管理端列表页使用。
func (r *SilenceRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]Silence, error) {
	rows, err := tx.Query(ctx, `SELECT `+silenceColumns+` FROM silences ORDER BY starts_at DESC LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出静默失败: %w", err)
	}
	defer rows.Close()

	var silences []Silence
	for rows.Next() {
		s, err := scanSilence(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描静默失败: %w", err)
		}
		silences = append(silences, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历静默失败: %w", err)
	}
	return silences, nil
}

// ListActive 返回在 at 时刻生效的静默规则：starts_at <= at AND ends_at > at
// （起始含、结束不含——静默创建时通常以 ends_at 为维护窗口的下一个整点，含端会
// 导致该时刻之后 1 纳秒仍被判定为静默中，不含端与「静默到期后立即恢复通知」
// 的 R10.3 语义更吻合）。
func (r *SilenceRepo) ListActive(ctx context.Context, tx pgx.Tx, at time.Time, limit int) ([]Silence, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+silenceColumns+`
		FROM silences
		WHERE starts_at <= $1 AND ends_at > $1
		ORDER BY starts_at
		LIMIT $2
	`, at, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询生效静默失败: %w", err)
	}
	defer rows.Close()

	var silences []Silence
	for rows.Next() {
		s, err := scanSilence(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描生效静默失败: %w", err)
		}
		silences = append(silences, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历生效静默失败: %w", err)
	}
	return silences, nil
}

// Delete 支持静默的手动提前删除（R10.5：删除后匹配告警的通知立即恢复）。
func (r *SilenceRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM silences WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除静默失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangeEvent 对应 change_events 表：归一化后的发布/配置/扩缩容变更事件（R7.4）。
type ChangeEvent struct {
	ID          string
	Kind        string
	ResourceRef map[string]string
	Payload     json.RawMessage // 来源特有载荷，对核心管道不透明，仅用于展示
	OccurredAt  time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const changeEventColumns = `id, kind, resource_ref, payload, occurred_at, created_at, updated_at`

func scanChangeEvent(row configRowScanner) (ChangeEvent, error) {
	var c ChangeEvent
	var resourceRefRaw []byte
	err := row.Scan(&c.ID, &c.Kind, &resourceRefRaw, &c.Payload, &c.OccurredAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return ChangeEvent{}, err
	}
	if err := json.Unmarshal(resourceRefRaw, &c.ResourceRef); err != nil {
		return ChangeEvent{}, fmt.Errorf("反序列化变更事件 resource_ref 失败: %w", err)
	}
	return c, nil
}

// ChangeEventRepo 是 change_events 表的仓储实现，约定同 TeamRepo。
type ChangeEventRepo struct{}

func NewChangeEventRepo() *ChangeEventRepo { return &ChangeEventRepo{} }

func (r *ChangeEventRepo) Insert(ctx context.Context, tx pgx.Tx, c ChangeEvent) (*ChangeEvent, error) {
	resourceRefJSON, err := json.Marshal(c.ResourceRef)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化变更事件 resource_ref 失败: %w", err)
	}
	payload := c.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	out, err := scanChangeEvent(tx.QueryRow(ctx, `
		INSERT INTO change_events (kind, resource_ref, payload, occurred_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+changeEventColumns, c.Kind, resourceRefJSON, payload, c.OccurredAt))
	if err != nil {
		return nil, fmt.Errorf("store: 写入变更事件失败: %w", err)
	}
	return &out, nil
}

// ListRecentByResource 返回与 resourceRef 关联（JSONB 包含匹配 @>）、发生时间不早于
// since 的变更事件，按发生时间倒序（R7.4/R7.5：告警详情页展示「最近变更」）。
func (r *ChangeEventRepo) ListRecentByResource(ctx context.Context, tx pgx.Tx, resourceRef map[string]string, since time.Time, limit int) ([]ChangeEvent, error) {
	resourceRefJSON, err := json.Marshal(resourceRef)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化 resource_ref 查询条件失败: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT `+changeEventColumns+`
		FROM change_events
		WHERE resource_ref @> $1::jsonb AND occurred_at >= $2
		ORDER BY occurred_at DESC
		LIMIT $3
	`, resourceRefJSON, since, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询资源近期变更失败: %w", err)
	}
	defer rows.Close()

	var events []ChangeEvent
	for rows.Next() {
		c, err := scanChangeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描变更事件失败: %w", err)
		}
		events = append(events, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历变更事件失败: %w", err)
	}
	return events, nil
}

// NotificationLogEntry 对应 notification_log 表：每次发送尝试的记录，用于重试计数
// 与幂等去重（R6.3）。AlertID 与 GroupKey 二选一（单告警通知 vs 分组聚合通知，R9）。
type NotificationLogEntry struct {
	ID             string
	AlertID        string // 可为空字符串；与 GroupKey 二选一
	GroupKey       string // 可为空字符串；与 AlertID 二选一
	Channel        string
	Target         string
	Step           *int // 对应升级序列的第几级；无关升级序列的发送为 nil
	IdempotencyKey string
	Status         string // pending|sent|failed
	Attempts       int
	LastError      string // 可为空字符串
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const notificationLogColumns = `id, alert_id, group_key, channel, target, step, idempotency_key, status, attempts, last_error, created_at, updated_at`

func scanNotificationLog(row configRowScanner) (NotificationLogEntry, error) {
	var e NotificationLogEntry
	var alertID, groupKey, lastError *string
	err := row.Scan(&e.ID, &alertID, &groupKey, &e.Channel, &e.Target, &e.Step,
		&e.IdempotencyKey, &e.Status, &e.Attempts, &lastError, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return NotificationLogEntry{}, err
	}
	e.AlertID = nullableString(alertID)
	e.GroupKey = nullableString(groupKey)
	e.LastError = nullableString(lastError)
	return e, nil
}

// ErrNotificationLogInvalid 在 AlertID 与 GroupKey 均为空时返回（违反
// notification_log 的 CHECK 约束：二者必须至少指定一个）。
var ErrNotificationLogInvalid = errors.New("store: notification_log 必须指定 alert_id 或 group_key 之一")

// ErrIdempotencyConflict 在 Insert 撞上已存在的 idempotency_key 时返回：
// at-least-once 的通知 handler 可能对同一次发送重试调用 Insert，调用方应据此
// 判断为「已记录过，跳过」而非报错中断。
var ErrIdempotencyConflict = errors.New("store: idempotency_key 已存在")

// NotificationLogRepo 是 notification_log 表的仓储实现，约定同 TeamRepo。
type NotificationLogRepo struct{}

func NewNotificationLogRepo() *NotificationLogRepo { return &NotificationLogRepo{} }

// Insert 写入一条发送记录。idempotency_key 冲突时不覆盖已有记录，返回
// ErrIdempotencyConflict（ON CONFLICT DO NOTHING，避免并发重试产生的竞态覆盖）。
func (r *NotificationLogRepo) Insert(ctx context.Context, tx pgx.Tx, e NotificationLogEntry) (*NotificationLogEntry, error) {
	if e.AlertID == "" && e.GroupKey == "" {
		return nil, ErrNotificationLogInvalid
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO notification_log (alert_id, group_key, channel, target, step, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+notificationLogColumns,
		nullableStringArg(e.AlertID), nullableStringArg(e.GroupKey), e.Channel, e.Target, e.Step, e.IdempotencyKey)
	out, err := scanNotificationLog(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIdempotencyConflict
	}
	if err != nil {
		return nil, fmt.Errorf("store: 写入通知记录失败: %w", err)
	}
	return &out, nil
}

func (r *NotificationLogRepo) GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, key string) (*NotificationLogEntry, error) {
	out, err := scanNotificationLog(tx.QueryRow(ctx, `
		SELECT `+notificationLogColumns+` FROM notification_log WHERE idempotency_key = $1
	`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 按幂等键查询通知记录失败: %w", err)
	}
	return &out, nil
}

// UpdateStatus 更新发送状态、重试计数与最近一次失败原因（R6.3：退避重试 + 记录失败原因）。
func (r *NotificationLogRepo) UpdateStatus(ctx context.Context, tx pgx.Tx, id, status string, attempts int, lastError string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE notification_log SET status = $2, attempts = $3, last_error = $4 WHERE id = $1
	`, id, status, attempts, nullableStringArg(lastError))
	if err != nil {
		return fmt.Errorf("store: 更新通知记录状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
