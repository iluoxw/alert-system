package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// gin.Context 里存取鉴权结果用的 key（sessionAuth() 写入，业务 handler/requireAdmin() 读取）。
const (
	ctxKeySessionID = "auth.session_id"
	ctxKeyUserID    = "auth.user_id"
	ctxKeyUsername  = "auth.username"
	ctxKeyIsAdmin   = "auth.is_admin"
)

// sessionCookieName 是登录会话 cookie 的名字。
const sessionCookieName = "session_token"

// sessionTokenBytes 是 session token 的随机字节数（R8.3：crypto/rand，至少 32 字节）。
const sessionTokenBytes = 32

// errSessionExpired 是会话查得到但已失效（固定过期或滑动续期窗口任一到期，或账号已
// 被禁用）的内部哨兵错误，sessionAuth() 据此与"会话不存在"一样返回 401，不向客户端
// 区分具体原因。
var errSessionExpired = errors.New("api: 会话已过期")

// SessionStore 抽象 store.SessionRepo 用到的方法，供本包在单测里注入内存 fake。
// *store.SessionRepo 天然满足这个接口。
type SessionStore interface {
	Create(ctx context.Context, tx pgx.Tx, s store.Session) (*store.Session, error)
	GetByTokenHash(ctx context.Context, tx pgx.Tx, tokenHash string) (*store.Session, error)
	Touch(ctx context.Context, tx pgx.Tx, id string, lastSeenAt time.Time) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

// MembershipStore 抽象 store.MembershipRepo 用到的方法。
type MembershipStore interface {
	IsAdminAnyTeam(ctx context.Context, tx pgx.Tx, userID string) (bool, error)
}

// Clock 抽象"当前时间"，供测试注入 fake clock 精确控制会话过期边界，与
// internal/pipeline 的 dedupClock 同一模式。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// generateSessionToken 生成 sessionTokenBytes 字节的随机 token，base64 URL 安全编码
// （不含 padding，cookie value 直接可用，无需额外转义）。
func generateSessionToken() (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashSessionToken 计算 token 的 hex(sha256(token))，与 sessions.token_hash 列的存储
// 编码约定一致——同 internal/ingest/server.go 的 hashToken，二者故意重复而不是共享
// 一个跨包函数：ingest 的 token 是 integration 凭据，session 的 token 是登录凭据，
// 语义上并非同一概念，共享实现只会造成两个不相关的包相互耦合。
func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RBACContext 是 sessionAuth() 校验通过后写入 gin.Context 的鉴权结果，供业务 handler
// 通过 UserFromContext 读取。
type RBACContext struct {
	UserID   string
	Username string
	IsAdmin  bool
}

// UserFromContext 读取当前请求已通过会话校验的用户信息；ok=false 表示当前路由未经过
// sessionAuth() 中间件（不应发生在受保护路由上，业务 handler 可按 ok 做防御性判断）。
func UserFromContext(c *gin.Context) (RBACContext, bool) {
	userID, ok := c.Get(ctxKeyUserID)
	if !ok {
		return RBACContext{}, false
	}
	username, _ := c.Get(ctxKeyUsername)
	isAdmin, _ := c.Get(ctxKeyIsAdmin)
	uid, _ := userID.(string)
	uname, _ := username.(string)
	admin, _ := isAdmin.(bool)
	return RBACContext{UserID: uid, Username: uname, IsAdmin: admin}, true
}

// sessionAuth 返回校验登录会话的中间件（R8.3）：从 cookie 取 token，按固定过期 +
// 滑动续期两条规则判定有效性，通过后把用户信息与是否为管理员写入 gin.Context，并
// 推进滑动续期水位线；任何一步失败均返回 401，不区分"未登录"与"会话过期"。
func (r *Router) sessionAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookieName)
		if err != nil || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
			return
		}
		tokenHash := hashSessionToken(token)
		now := r.clock.Now()
		ctx := c.Request.Context()

		var rbac RBACContext
		var sessionID string
		txErr := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
			sess, getErr := r.sessions.GetByTokenHash(ctx, tx, tokenHash)
			if getErr != nil {
				return getErr
			}
			if now.After(sess.ExpiresAt) || now.After(sess.LastSeenAt.Add(r.sessionIdleTTL)) {
				return errSessionExpired
			}
			user, getUserErr := r.users.GetByID(ctx, tx, sess.UserID)
			if getUserErr != nil {
				return getUserErr
			}
			if !user.Enabled {
				return errSessionExpired
			}
			if err := r.sessions.Touch(ctx, tx, sess.ID, now); err != nil {
				return err
			}
			isAdmin, admErr := r.memberships.IsAdminAnyTeam(ctx, tx, sess.UserID)
			if admErr != nil {
				return admErr
			}
			sessionID = sess.ID
			rbac = RBACContext{UserID: user.ID, Username: user.Username, IsAdmin: isAdmin}
			return nil
		})
		if txErr != nil {
			if errors.Is(txErr, store.ErrNotFound) || errors.Is(txErr, errSessionExpired) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
				return
			}
			r.logger.Error("api: 会话校验失败", "err", txErr)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		c.Set(ctxKeySessionID, sessionID)
		c.Set(ctxKeyUserID, rbac.UserID)
		c.Set(ctxKeyUsername, rbac.Username)
		c.Set(ctxKeyIsAdmin, rbac.IsAdmin)
		c.Next()
	}
}

// requireAdmin 返回 RBAC 中间件（R8.2）：必须接在 sessionAuth() 之后使用。管理员判定
// 采用"在至少一个团队持有 admin 角色"这一简化口径——见 server.go NewRouter 文档里对
// R8.2 与 memberships 表按团队分角色之间张力的说明。非管理员返回 403（区别于
// sessionAuth() 的 401：这里已经确认是合法登录用户，只是权限不够）。
func (r *Router) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		rbac, ok := UserFromContext(c)
		if !ok || !rbac.IsAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}
