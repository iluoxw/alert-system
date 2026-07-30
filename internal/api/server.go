package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/config"
	"alert-system/internal/store"
)

// Router 是管理类 HTTP API 的路由骨架（R8：登录、会话、RBAC）。命名避让本包已有的
// Server 类型（lark_callback.go，任务 21 的飞书卡片回调，逻辑不受本任务改动）：
// main 装配时二者是同一个 *gin.Engine 上的两组端点，Router 负责 /api/v1 与登录态，
// Server 的回调端点通过 gin.WrapH 挂进同一个 engine（见 NewRouter，会话中间件不覆盖
// 这条路径——回调有自己的签名校验，R8.3 要求 /ingest、/callbacks/lark/card 都豁免
// session 校验）。
//
// RBAC 口径说明（R8.2 与 memberships 表之间的张力，需要显式记录）：requirements.md
// R8.2 把 admin/member 写成看起来是全局的两级能力（"member 可查看与处置告警，admin
// 可管理团队/路由/值班/集成配置"），但 memberships 表（迁移 000002）把 role 存成
// UNIQUE(team_id, user_id) 的按团队维度——同一用户理论上可以在 A 团队是 admin、在
// B 团队无角色。本任务是"API 底座"骨架，目前还没有任何按团队细分权限的具体管理端点
// 需要保护，而 R8.2 描述的"管理团队/路由/值班/集成配置"这些资源本身是平台级的，不是
// 团队私有数据，按团队再细分 admin 语义上也说不通。因此这里选择一个明确的简化口径：
// requireAdmin() 判定"是否在至少一个团队持有 admin 角色"（MembershipStore.
// IsAdminAnyTeam），把它当作全局管理员能力使用。如果后续任务需要真正的团队级细分
// 权限（例如"只能管理自己团队的路由"），需要在那个任务里重新设计，不能想当然复用
// 这里的口径。
type Router struct {
	engine *gin.Engine

	tx          TxRunner
	users       UserStore
	sessions    SessionStore
	memberships MembershipStore
	auth        Authenticator
	clock       Clock

	sessionTTL     time.Duration
	sessionIdleTTL time.Duration
	cookieSecure   bool

	logger *slog.Logger
}

// NewRouter 构造 Router 并完成路由装配。larkHandler 是已经构造好的
// (*api.Server).Handler()（任务 21），原样通过 gin.WrapH 挂载，不改动其内部逻辑。
// logger 为 nil 时使用 slog.Default()。
//
// 配置校验入口（落账警告，任务 21）：这里顺带校验 cfg.Lark.VerificationToken 是否
// 非空——为空只 WARN 不报错，因为空 token 是 SDK 自身认可的"跳过 challenge token
// 校验、仅保留签名校验"配置（与 lark_callback.go NewServer 的文档一致），不是本任务
// 引入的新语义，只是一个容易被忽略的部署疏漏，值得在装配时提醒一次。
func NewRouter(cfg config.Config, users UserStore, sessions SessionStore, memberships MembershipStore, tx TxRunner, auth Authenticator, larkHandler http.Handler, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Lark.VerificationToken == "" {
		logger.Warn("api: lark verification_token 未配置，飞书回调 url_verification 握手将跳过 token 校验（仅保留签名校验）")
	}

	r := &Router{
		tx:             tx,
		users:          users,
		sessions:       sessions,
		memberships:    memberships,
		auth:           auth,
		clock:          realClock{},
		sessionTTL:     cfg.SessionTTL,
		sessionIdleTTL: cfg.SessionIdleTTL,
		cookieSecure:   cfg.SessionCookieSecure,
		logger:         logger,
	}
	r.mount(larkHandler)
	return r
}

// mount 装配全部路由。
func (r *Router) mount(larkHandler http.Handler) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	v1 := engine.Group("/api/v1")
	v1.POST("/auth/login", r.handleLogin)

	authed := v1.Group("")
	authed.Use(r.sessionAuth())
	authed.POST("/auth/logout", r.handleLogout)
	authed.GET("/me", r.handleMe)

	// /admin 下是占位端点，只用来验证 requireAdmin() 中间件链路本身工作正常；真正的
	// 团队/路由/值班/集成管理端点由后续任务实现，接入时复用这个分组或 r.requireAdmin()。
	admin := authed.Group("/admin")
	admin.Use(r.requireAdmin())
	admin.GET("/ping", r.handleAdminPing)

	// R8.3：/callbacks/lark/card 有自己的签名校验，豁免 session 中间件，直接挂在
	// engine 根上（不进 v1/authed 分组）。
	engine.POST("/callbacks/lark/card", gin.WrapH(larkHandler))

	r.engine = engine
}

// Engine 返回底层 *gin.Engine，主要供测试与（如需要）main 装配时的少量定制使用。
func (r *Router) Engine() *gin.Engine { return r.engine }

// ServeHTTP 实现 http.Handler，main 装配可以直接 http.ListenAndServe(addr, router)。
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.engine.ServeHTTP(w, req)
}

// MountUnauthenticated 在 engine 根上挂载一个豁免 session 中间件的路由，供 main 装配
// 时接入 /ingest 端点用（internal/ingest/server.go，任务 17，stdlib net/http.Handler，
// 有自己的 token 校验）。本任务不知道 main 最终选择的路径/方法，所以只留出这个挂载点，
// 不在这里预先注册 /ingest 本身。
func (r *Router) MountUnauthenticated(method, path string, h http.Handler) {
	r.engine.Handle(method, path, gin.WrapH(h))
}

// handleLogin 处理 POST /api/v1/auth/login：校验用户名密码，成功则签发会话 cookie。
func (r *Router) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	ctx := c.Request.Context()
	var user *store.User
	err := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
		u, authErr := r.auth.Authenticate(ctx, tx, req.Username, req.Password)
		if authErr != nil {
			return authErr
		}
		user = u
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
			return
		}
		r.logger.Error("api: 登录处理失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	token, err := generateSessionToken()
	if err != nil {
		r.logger.Error("api: 生成会话 token 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	now := r.clock.Now()
	err = r.tx.WithTx(ctx, func(tx pgx.Tx) error {
		_, createErr := r.sessions.Create(ctx, tx, store.Session{
			UserID:    user.ID,
			TokenHash: hashSessionToken(token),
			ExpiresAt: now.Add(r.sessionTTL),
		})
		return createErr
	})
	if err != nil {
		r.logger.Error("api: 创建会话失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, token, int(r.sessionTTL.Seconds()), "/", "", r.cookieSecure, true)
	c.JSON(http.StatusOK, gin.H{"username": user.Username})
}

// handleLogout 处理 POST /api/v1/auth/logout（session 保护）：在 DB 里删除会话使其
// 立即失效（R8.3：服务端失效，不是只清客户端 cookie），再清空 cookie。
func (r *Router) handleLogout(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID, _ := c.Get(ctxKeySessionID)
	if id, ok := sessionID.(string); ok && id != "" {
		if err := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
			return r.sessions.Delete(ctx, tx, id)
		}); err != nil {
			r.logger.Error("api: 登出删除会话失败", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", r.cookieSecure, true)
	c.Status(http.StatusNoContent)
}

// handleMe 处理 GET /api/v1/me（session 保护）：返回当前登录用户信息，主要用于验证
// 会话中间件链路，也是前端判断登录态/角色的落脚点。
func (r *Router) handleMe(c *gin.Context) {
	rbac, _ := UserFromContext(c)
	c.JSON(http.StatusOK, gin.H{
		"user_id":  rbac.UserID,
		"username": rbac.Username,
		"is_admin": rbac.IsAdmin,
	})
}

// handleAdminPing 处理 GET /api/v1/admin/ping（session + admin 保护）：占位端点，见
// mount() 里的注释。
func (r *Router) handleAdminPing(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
