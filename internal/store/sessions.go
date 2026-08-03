package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session 对应 sessions 表：自产登录会话（R8.1/R8.3，不引入第三方 session 库）。
// TokenHash 是 hex(sha256(明文会话 token))；明文只经 Set-Cookie 下发给客户端，
// DB 里只存哈希。它与 Alertmanager hook 凭据是两套独立机制。ExpiresAt 是登录时
// 确定、此后不再变化的固定过期时间；
// LastSeenAt 是滑动续期水位线，每次带有效 session 的请求都会推进（"固定过期 +
// 滑动续期"：会话在两者都未过期时才有效，任一到期即失效）。
type Session struct {
	ID         string
	UserID     string
	TokenHash  string
	ExpiresAt  time.Time
	LastSeenAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const sessionColumns = `id, user_id, token_hash, expires_at, last_seen_at, created_at, updated_at`

func scanSession(row configRowScanner) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.ExpiresAt, &s.LastSeenAt, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// SessionRepo 是 sessions 表的仓储实现，约定同 TeamRepo（config_repos.go 头部注释）。
type SessionRepo struct{}

func NewSessionRepo() *SessionRepo { return &SessionRepo{} }

// Create 写入一条新会话：登录成功时调用，ExpiresAt 由调用方按配置的固定会话时长
// 算好传入；LastSeenAt 由 DB 默认值置为 now()。
func (r *SessionRepo) Create(ctx context.Context, tx pgx.Tx, s Session) (*Session, error) {
	out, err := scanSession(tx.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING `+sessionColumns, s.UserID, s.TokenHash, s.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("store: 创建会话失败: %w", err)
	}
	return &out, nil
}

// GetByTokenHash 供每次请求的会话中间件查询；不存在返回 ErrNotFound。
func (r *SessionRepo) GetByTokenHash(ctx context.Context, tx pgx.Tx, tokenHash string) (*Session, error) {
	out, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE token_hash = $1`, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 按 token_hash 查询会话失败: %w", err)
	}
	return &out, nil
}

// Touch 推进滑动续期水位线，供每次请求校验通过后调用；不改变 ExpiresAt（固定过期
// 不受滑动续期影响）。
func (r *SessionRepo) Touch(ctx context.Context, tx pgx.Tx, id string, lastSeenAt time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, id, lastSeenAt)
	if err != nil {
		return fmt.Errorf("store: 续期会话失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 使会话立即失效（登出，R8.3：服务端使 session 失效，不是只清客户端 cookie）。
// 幂等：会话已不存在时不报错。
func (r *SessionRepo) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: 删除会话失败: %w", err)
	}
	return nil
}
