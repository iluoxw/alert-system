package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	DefaultAlertSourceReconcileIntervalSeconds = 30
	DefaultAlertSourceStaleAfterSeconds        = 120
	DefaultAlertSourceMissingConfirmations     = 3
)

// AlertmanagerSourceConfig 是 integrations.config 中 Alertmanager 专属的受控部分。
// 凭据只保存环境变量名，API 与数据库都不接触明文 secret。
type AlertmanagerSourceConfig struct {
	APIURL                   string `json:"api_url"`
	BearerTokenEnv           string `json:"bearer_token_env,omitempty"`
	ReconcileIntervalSeconds int    `json:"reconcile_interval_seconds"`
	StaleAfterSeconds        int    `json:"stale_after_seconds"`
	MissingConfirmations     int    `json:"missing_confirmations"`
}

func (c AlertmanagerSourceConfig) WithDefaults() AlertmanagerSourceConfig {
	if c.ReconcileIntervalSeconds <= 0 {
		c.ReconcileIntervalSeconds = DefaultAlertSourceReconcileIntervalSeconds
	}
	if c.StaleAfterSeconds <= 0 {
		c.StaleAfterSeconds = DefaultAlertSourceStaleAfterSeconds
	}
	if c.MissingConfirmations <= 0 {
		c.MissingConfirmations = DefaultAlertSourceMissingConfirmations
	}
	c.APIURL = strings.TrimRight(strings.TrimSpace(c.APIURL), "/")
	c.BearerTokenEnv = strings.TrimSpace(c.BearerTokenEnv)
	return c
}

func ParseAlertmanagerSourceConfig(raw json.RawMessage) (AlertmanagerSourceConfig, error) {
	var cfg AlertmanagerSourceConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return AlertmanagerSourceConfig{}, fmt.Errorf("store: 解析 Alertmanager source config 失败: %w", err)
		}
	}
	return cfg.WithDefaults(), nil
}

type AlertSourceHealth string

const (
	AlertSourceHealthDisabled     AlertSourceHealth = "disabled"
	AlertSourceHealthUnconfigured AlertSourceHealth = "unconfigured"
	AlertSourceHealthUnknown      AlertSourceHealth = "unknown"
	AlertSourceHealthHealthy      AlertSourceHealth = "healthy"
	AlertSourceHealthDegraded     AlertSourceHealth = "degraded"
	AlertSourceHealthStale        AlertSourceHealth = "stale"
)

// AlertSource 把 integration 登记、同步水位和当前镜像统计组合成管理面读模型。
type AlertSource struct {
	ID              string
	Name            string
	Enabled         bool
	DefaultSeverity string
	Config          AlertmanagerSourceConfig
	RawConfig       json.RawMessage

	LastWebhookAt       time.Time
	LastReconcileAt     time.Time
	LastSuccessAt       time.Time
	LastErrorAt         time.Time
	LastError           string
	ConsecutiveFailures int
	SnapshotVersion     int64

	FiringCount     int
	UnknownCount    int
	SuppressedCount int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (s AlertSource) Health(now time.Time) AlertSourceHealth {
	if !s.Enabled {
		return AlertSourceHealthDisabled
	}
	if s.Config.APIURL == "" {
		return AlertSourceHealthUnconfigured
	}
	if s.LastSuccessAt.IsZero() {
		if s.ConsecutiveFailures > 0 {
			return AlertSourceHealthStale
		}
		return AlertSourceHealthUnknown
	}
	if now.Sub(s.LastSuccessAt) > time.Duration(s.Config.StaleAfterSeconds)*time.Second {
		return AlertSourceHealthStale
	}
	if s.ConsecutiveFailures > 0 {
		return AlertSourceHealthDegraded
	}
	return AlertSourceHealthHealthy
}

func (s AlertSource) ReconcileDue(now time.Time) bool {
	if !s.Enabled || s.Config.APIURL == "" {
		return false
	}
	if s.LastReconcileAt.IsZero() {
		return true
	}
	return !now.Before(s.LastReconcileAt.Add(time.Duration(s.Config.ReconcileIntervalSeconds) * time.Second))
}

type AlertSourceRepo struct{}

func NewAlertSourceRepo() *AlertSourceRepo { return &AlertSourceRepo{} }

const alertSourceSelect = `
	i.id::text, i.name, i.enabled, i.default_severity, i.config,
	s.last_webhook_at, s.last_reconcile_at, s.last_success_at, s.last_error_at,
	COALESCE(s.last_error, ''), COALESCE(s.consecutive_failures, 0), COALESCE(s.snapshot_version, 0),
	count(a.id) FILTER (WHERE a.source_state = 'firing'),
	count(a.id) FILTER (WHERE a.source_state = 'unknown'),
	count(a.id) FILTER (WHERE a.source_state != 'resolved' AND a.suppression_state != 'none'),
	i.created_at, i.updated_at
`

func scanAlertSource(row alertRowScanner) (AlertSource, error) {
	var s AlertSource
	var raw []byte
	var lastWebhook, lastReconcile, lastSuccess, lastError *time.Time
	if err := row.Scan(
		&s.ID, &s.Name, &s.Enabled, &s.DefaultSeverity, &raw,
		&lastWebhook, &lastReconcile, &lastSuccess, &lastError,
		&s.LastError, &s.ConsecutiveFailures, &s.SnapshotVersion,
		&s.FiringCount, &s.UnknownCount, &s.SuppressedCount,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return AlertSource{}, err
	}
	s.RawConfig = json.RawMessage(raw)
	cfg, err := ParseAlertmanagerSourceConfig(s.RawConfig)
	if err != nil {
		return AlertSource{}, err
	}
	s.Config = cfg
	if lastWebhook != nil {
		s.LastWebhookAt = *lastWebhook
	}
	if lastReconcile != nil {
		s.LastReconcileAt = *lastReconcile
	}
	if lastSuccess != nil {
		s.LastSuccessAt = *lastSuccess
	}
	if lastError != nil {
		s.LastErrorAt = *lastError
	}
	return s, nil
}

func (r *AlertSourceRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]AlertSource, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+alertSourceSelect+`
		FROM integrations i
		LEFT JOIN alert_source_status s ON s.integration_id = i.id
		LEFT JOIN alerts a ON a.source_integration_id = i.id AND a.upstream_fingerprint != ''
		WHERE i.kind = 'alertmanager'
		GROUP BY i.id, s.integration_id
		ORDER BY i.name
		LIMIT $1
	`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Alertmanager sources 失败: %w", err)
	}
	defer rows.Close()
	var result []AlertSource
	for rows.Next() {
		s, err := scanAlertSource(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 Alertmanager source 失败: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (r *AlertSourceRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (AlertSource, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+alertSourceSelect+`
		FROM integrations i
		LEFT JOIN alert_source_status s ON s.integration_id = i.id
		LEFT JOIN alerts a ON a.source_integration_id = i.id AND a.upstream_fingerprint != ''
		WHERE i.id = $1::uuid AND i.kind = 'alertmanager'
		GROUP BY i.id, s.integration_id
	`, id)
	s, err := scanAlertSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertSource{}, ErrNotFound
	}
	if err != nil {
		return AlertSource{}, fmt.Errorf("store: 查询 Alertmanager source 失败: %w", err)
	}
	return s, nil
}

func (r *AlertSourceRepo) RecordFailure(ctx context.Context, tx pgx.Tx, id string, at time.Time, cause error) error {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO alert_source_status (
			integration_id, last_reconcile_at, last_error_at, last_error, consecutive_failures
		) VALUES ($1::uuid, $2, $2, $3, 1)
		ON CONFLICT (integration_id) DO UPDATE SET
			last_reconcile_at = EXCLUDED.last_reconcile_at,
			last_error_at = EXCLUDED.last_error_at,
			last_error = EXCLUDED.last_error,
			consecutive_failures = alert_source_status.consecutive_failures + 1
	`, id, at, msg)
	if err != nil {
		return fmt.Errorf("store: 记录 Alertmanager source 失败状态失败: %w", err)
	}
	return nil
}

// BeginSuccessfulSnapshot 与镜像写入处于同一个短事务；若后续应用快照失败，水位也
// 随事务回滚，不会出现“健康但数据没写完”的假象。
func (r *AlertSourceRepo) BeginSuccessfulSnapshot(ctx context.Context, tx pgx.Tx, id string, at time.Time) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `
		INSERT INTO alert_source_status (
			integration_id, last_reconcile_at, last_success_at, consecutive_failures, snapshot_version
		) VALUES ($1::uuid, $2, $2, 0, 1)
		ON CONFLICT (integration_id) DO UPDATE SET
			last_reconcile_at = EXCLUDED.last_reconcile_at,
			last_success_at = EXCLUDED.last_success_at,
			last_error = NULL,
			consecutive_failures = 0,
			snapshot_version = alert_source_status.snapshot_version + 1
		RETURNING snapshot_version
	`, id, at).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("store: 推进 Alertmanager snapshot 版本失败: %w", err)
	}
	return version, nil
}
