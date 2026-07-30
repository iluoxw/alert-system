package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Integration 对应 integrations 表：入站/出站 adapter 注册（R1、R2.5）。TokenHash 是
// hex(sha256(明文 token))，与 internal/ingest/server.go hashToken 同一编码约定；本类型
// 与仓储方法都不携带明文 token——由调用方（管理 API，任务 27）在创建时生成明文、算好
// hash 后写入这里，明文只经由那次创建的 HTTP 响应下发一次，DB 与本仓储的任何查询路径
// 都只经手 hash。
type Integration struct {
	ID              string
	Kind            string
	Name            string
	TokenHash       string
	DefaultSeverity string
	Enabled         bool
	Config          json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ErrIntegrationNameConflict 在 Create/Update 撞上 integrations.name 唯一约束时返回，
// 调用方（管理 API）据此给出「名称已被占用」而非笼统的 500。
var ErrIntegrationNameConflict = errors.New("store: integration name 已被占用")

const integrationColumns = `id, kind, name, token_hash, default_severity, enabled, config, created_at, updated_at`

func scanIntegration(row configRowScanner) (Integration, error) {
	var in Integration
	var configRaw []byte
	err := row.Scan(&in.ID, &in.Kind, &in.Name, &in.TokenHash, &in.DefaultSeverity, &in.Enabled, &configRaw, &in.CreatedAt, &in.UpdatedAt)
	if err != nil {
		return Integration{}, err
	}
	in.Config = configRaw
	return in, nil
}

// isIntegrationNameConflict 判断 err 是否是 integrations.name 唯一约束被违反
// （建表语句里 UNIQUE (name) 内联声明，Postgres 按约定生成约束名 integrations_name_key）。
func isIntegrationNameConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "integrations_name_key"
}

// IntegrationRepo 是 integrations 表的仓储实现，约定同 TeamRepo（config_repos.go 头部注释）。
type IntegrationRepo struct{}

func NewIntegrationRepo() *IntegrationRepo { return &IntegrationRepo{} }

// Create 写入一条新 integration。in.TokenHash 必须由调用方预先算好（本仓储不生成/不
// 感知明文 token）；in.Config 为 nil 时按空对象写入（列有 NOT NULL 约束）。
func (r *IntegrationRepo) Create(ctx context.Context, tx pgx.Tx, in Integration) (*Integration, error) {
	config := in.Config
	if config == nil {
		config = json.RawMessage(`{}`)
	}
	out, err := scanIntegration(tx.QueryRow(ctx, `
		INSERT INTO integrations (kind, name, token_hash, default_severity, enabled, config)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+integrationColumns,
		in.Kind, in.Name, in.TokenHash, in.DefaultSeverity, in.Enabled, config))
	if isIntegrationNameConflict(err) {
		return nil, ErrIntegrationNameConflict
	}
	if err != nil {
		return nil, fmt.Errorf("store: 创建 integration 失败: %w", err)
	}
	return &out, nil
}

func (r *IntegrationRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Integration, error) {
	out, err := scanIntegration(tx.QueryRow(ctx, `SELECT `+integrationColumns+` FROM integrations WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询 integration 失败: %w", err)
	}
	return &out, nil
}

// List 按名称升序返回全部 integration，供管理端列表页使用。
func (r *IntegrationRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]Integration, error) {
	rows, err := tx.Query(ctx, `SELECT `+integrationColumns+` FROM integrations ORDER BY name LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出 integration 失败: %w", err)
	}
	defer rows.Close()

	var integrations []Integration
	for rows.Next() {
		in, err := scanIntegration(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 integration 失败: %w", err)
		}
		integrations = append(integrations, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 integration 失败: %w", err)
	}
	return integrations, nil
}

// Update 更新 kind/name/default_severity/enabled/config；不改动 token_hash——轮换凭据
// 不在本期范围内（YAGNY，见任务回报），需要时应作为单独的「重置 token」操作设计。
func (r *IntegrationRepo) Update(ctx context.Context, tx pgx.Tx, in Integration) error {
	config := in.Config
	if config == nil {
		config = json.RawMessage(`{}`)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE integrations SET kind = $2, name = $3, default_severity = $4, enabled = $5, config = $6 WHERE id = $1
	`, in.ID, in.Kind, in.Name, in.DefaultSeverity, in.Enabled, config)
	if isIntegrationNameConflict(err) {
		return ErrIntegrationNameConflict
	}
	if err != nil {
		return fmt.Errorf("store: 更新 integration 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *IntegrationRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM integrations WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除 integration 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
