package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound 由本文件系列（config_repos*.go, silences.go）的 GetByID/GetBy* 方法
// 在未找到匹配行时统一返回，调用方用 errors.Is 判断。
var ErrNotFound = errors.New("store: 未找到记录")

// maxListLimit 是所有 List 查询的硬上限；调用方传入的 limit 超出或 <= 0 时回退到
// 这个默认/上限值，防止管理端传入的分页参数意外拖出全表扫描。
const maxListLimit = 500

func clampLimit(limit int) int {
	if limit <= 0 || limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

// configRowScanner 抽象 pgx.Row（QueryRow）与 pgx.Rows（Query 遍历中的一行）共有的
// Scan 方法，使同一套「行 -> 结构体」反序列化逻辑可以同时服务单行与多行查询。
// 命名带 config 前缀是刻意的：alerts.go 里也有一个形状相同的 rowScanner 接口，为避免
// 跟另一个并行任务的文件产生同名冲突（且不允许修改 alerts.go），这里使用独立的类型名。
type configRowScanner interface {
	Scan(dest ...any) error
}

// nullableString 把可能为 NULL 的文本列（如 users.email、silences.created_by）
// 转成本项目的「空字符串代表未设置」约定（对齐 domain.Alert.SourceIntegrationID
// 的既有惯例），调用方不需要处理指针/sql.Null* 类型。
func nullableString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// nullableStringArg 是 nullableString 的反向操作：把「空字符串代表未设置」的字段
// 转成写库用的 *string（NULL 或有值），供 INSERT/UPDATE 参数使用。
func nullableStringArg(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Team 对应 teams 表：路由目标与 RBAC 作用域（R5、R8）。
type Team struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TeamRepo 是 teams 表的仓储实现。与 EventQueue（events.go）同样的约定：仓储本身
// 无状态，所有方法在调用方传入的 pgx.Tx 内执行，事务边界由调用方通过 Store.WithTx 组织。
type TeamRepo struct{}

func NewTeamRepo() *TeamRepo { return &TeamRepo{} }

func scanTeam(row configRowScanner) (Team, error) {
	var t Team
	err := row.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (r *TeamRepo) Create(ctx context.Context, tx pgx.Tx, name string) (*Team, error) {
	t, err := scanTeam(tx.QueryRow(ctx, `
		INSERT INTO teams (name) VALUES ($1)
		RETURNING id, name, created_at, updated_at
	`, name))
	if err != nil {
		return nil, fmt.Errorf("store: 创建团队失败: %w", err)
	}
	return &t, nil
}

func (r *TeamRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*Team, error) {
	t, err := scanTeam(tx.QueryRow(ctx, `
		SELECT id, name, created_at, updated_at FROM teams WHERE id = $1
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询团队失败: %w", err)
	}
	return &t, nil
}

// List 按名称升序返回团队，供管理端列表页使用。
func (r *TeamRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]Team, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, name, created_at, updated_at FROM teams ORDER BY name LIMIT $1
	`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出团队失败: %w", err)
	}
	defer rows.Close()

	var teams []Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描团队失败: %w", err)
		}
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历团队失败: %w", err)
	}
	return teams, nil
}

func (r *TeamRepo) Update(ctx context.Context, tx pgx.Tx, id, name string) error {
	tag, err := tx.Exec(ctx, `UPDATE teams SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return fmt.Errorf("store: 更新团队失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *TeamRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除团队失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// User 对应 users 表：本地账号，认证层为 SSO/OIDC 预留扩展点（R8.1）。
type User struct {
	ID           string
	Username     string
	PasswordHash string
	DisplayName  string
	Email        string // 可为空字符串（email 列可空）
	LarkOpenID   string // 可为空字符串（lark_open_id 列可空，迁移 000007 加，渠道联系方式解析用）
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserRepo 是 users 表的仓储实现，约定同 TeamRepo。
type UserRepo struct{}

func NewUserRepo() *UserRepo { return &UserRepo{} }

func scanUser(row configRowScanner) (User, error) {
	var u User
	var email, larkOpenID *string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &email, &larkOpenID, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	u.Email = nullableString(email)
	u.LarkOpenID = nullableString(larkOpenID)
	return u, nil
}

const userColumns = `id, username, password_hash, display_name, email, lark_open_id, enabled, created_at, updated_at`

func (r *UserRepo) Create(ctx context.Context, tx pgx.Tx, u User) (*User, error) {
	out, err := scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, display_name, email, enabled)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+userColumns, u.Username, u.PasswordHash, u.DisplayName, nullableStringArg(u.Email), u.Enabled))
	if err != nil {
		return nil, fmt.Errorf("store: 创建用户失败: %w", err)
	}
	return &out, nil
}

func (r *UserRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (*User, error) {
	out, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户失败: %w", err)
	}
	return &out, nil
}

// GetByUsername 供登录鉴权使用（R8.1）。
func (r *UserRepo) GetByUsername(ctx context.Context, tx pgx.Tx, username string) (*User, error) {
	out, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE username = $1`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 按用户名查询用户失败: %w", err)
	}
	return &out, nil
}

// GetByLarkOpenID 按飞书 open_id 映射 Firebreak 用户。只返回启用状态由调用方
// 决定，这样身份绑定查询与后台目录查询可以复用同一套仓储语义。
func (r *UserRepo) GetByLarkOpenID(ctx context.Context, tx pgx.Tx, openID string) (*User, error) {
	out, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lark_open_id = $1`, openID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 按飞书 open_id 查询用户失败: %w", err)
	}
	return &out, nil
}

func (r *UserRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]User, error) {
	rows, err := tx.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY username LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出用户失败: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描用户失败: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历用户失败: %w", err)
	}
	return users, nil
}

// Update 更新用户资料与启用状态；密码修改走独立方法（本期未要求，留待认证任务实现）。
func (r *UserRepo) Update(ctx context.Context, tx pgx.Tx, id, displayName, email string, enabled bool) error {
	tag, err := tx.Exec(ctx, `
		UPDATE users SET display_name = $2, email = $3, enabled = $4 WHERE id = $1
	`, id, displayName, nullableStringArg(email), enabled)
	if err != nil {
		return fmt.Errorf("store: 更新用户失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateLarkOpenID 更新飞书身份映射。openID 为空表示解除绑定；数据库上的唯一
// 索引负责防止一个飞书身份绑定多个 Firebreak 用户。
func (r *UserRepo) UpdateLarkOpenID(ctx context.Context, tx pgx.Tx, id, openID string) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET lark_open_id = $2 WHERE id = $1`, id, nullableStringArg(openID))
	if err != nil {
		return fmt.Errorf("store: 更新飞书身份映射失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *UserRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除用户失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
