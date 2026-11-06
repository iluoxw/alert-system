// Package api（本文件）：轻量目录端点（serviceability C4，R4）+ 健康检查（C5，R5
// 里除 retention_sweep 外的部分，那部分是 internal/app 的定时器任务，不在本文件）。
//
// 目录端点走登录级鉴权（sessionAuth，无 requireAdmin）——这是与 config.go 的
// /admin/teams、/admin/users 的关键区别：R4 需要普通 member 也能把告警列表/详情里
// 出现的 team_id、acked_by 等 UUID 解析成人类可读的名字，不能锁进 admin 分组。响应
// 体是硬字段白名单（只出 id + name/display_name），序列化时逐字段手写，不直接
// json.Marshal(store.Team{...})，避免未来 store.Team/store.User 加字段时不小心
// 泄露 email/username/enabled/password_hash 等敏感或管理语境字段给全体登录用户。
//
// 健康检查两端点豁免鉴权，挂在 engine 根上，与 /callbacks/lark/card（server.go）
// 同层：/healthz 是进程存活探针恒 200，不查任何依赖；/readyz 调用注入的 ready
// 函数（main 传 ComposeReady(DatabaseReady(Ping), WorkerReady(scheduler),
// WorkerReady(pipeline))）。只看进程内状态：ccloud / Alertmanager 不可用不得导致
// 503。错误文案只给概括句，不带 DSN/连接串/错误原文。
//
// 挂载方式与 members.go/ops_settings.go 同构：独立 Register 方法各自开分组/直接在
// engine 根注册，不碰 server.go 的 mount()。
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// ErrReadyDatabase / ErrReadyWorkers 是 /readyz 的哨兵错误。handler 只把它们映射
// 成概括文案，绝不把 err.Error() 写进响应体（避免 DSN/密码泄漏）。
var (
	ErrReadyDatabase = errors.New("数据库不可达")
	ErrReadyWorkers  = errors.New("后台任务停摆")
)

// Heartbeat 是 scheduler / pipeline 暴露给 /readyz 的进程内心跳。实现不得做外部 IO。
type Heartbeat interface {
	LastPollSuccessAt() time.Time
	PollInterval() time.Duration
}

// DirectoryTeamStore 抽象目录端点用到的团队列表方法，*store.TeamRepo 天然满足。
type DirectoryTeamStore interface {
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Team, error)
}

// DirectoryUserStore 抽象目录端点用到的用户列表方法，*store.UserRepo 天然满足。
type DirectoryUserStore interface {
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.User, error)
}

// directoryHandlers 持有两个目录端点的依赖，方法接收者独立于 configHandlers。
type directoryHandlers struct {
	router *Router
	teams  DirectoryTeamStore
	users  DirectoryUserStore
}

// RegisterDirectoryRoutes 挂载 GET /api/v1/teams、GET /api/v1/users：登录即可
// （不要求 admin），响应体只含 id+name / id+display_name 两个字段。
func (r *Router) RegisterDirectoryRoutes(teams DirectoryTeamStore, users DirectoryUserStore) *Router {
	h := &directoryHandlers{router: r, teams: teams, users: users}

	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/teams", h.listTeams)
	authed.GET("/users", h.listUsers)

	return r
}

// listTeams 处理 GET /api/v1/teams。
func (h *directoryHandlers) listTeams(c *gin.Context) {
	ctx := c.Request.Context()
	var teams []store.Team
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		teams, listErr = h.teams.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.router.logger.Error("api: 列出团队目录失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	items := make([]gin.H, 0, len(teams))
	for _, t := range teams {
		items = append(items, gin.H{"id": t.ID, "name": t.Name})
	}
	c.JSON(http.StatusOK, gin.H{"teams": items})
}

// listUsers 处理 GET /api/v1/users。
func (h *directoryHandlers) listUsers(c *gin.Context) {
	ctx := c.Request.Context()
	var users []store.User
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		users, listErr = h.users.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.router.logger.Error("api: 列出用户目录失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	items := make([]gin.H, 0, len(users))
	for _, u := range users {
		items = append(items, gin.H{"id": u.ID, "display_name": u.DisplayName})
	}
	c.JSON(http.StatusOK, gin.H{"users": items})
}

// ---- 健康检查 ----

// RegisterHealthRoutes 挂载 GET /healthz（恒 200，进程存活探针）、GET /readyz
// （调 ready 判断依赖是否可用），二者豁免鉴权，直接挂在 engine 根上。ready 为 nil
// 时 /readyz 视为恒 ready（不阻塞未接入依赖检查的装配场景）。
//
// 响应体永不包含 ready 返回的错误原文。已知哨兵映射为固定中文；其余一律「服务未就绪」。
func (r *Router) RegisterHealthRoutes(ready func(ctx context.Context) error) *Router {
	r.engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.engine.GET("/readyz", func(c *gin.Context) {
		if ready != nil {
			if err := ready(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": readyErrorMessage(err)})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}

func readyErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrReadyDatabase):
		return "数据库不可达"
	case errors.Is(err, ErrReadyWorkers):
		return "后台任务停摆"
	default:
		return "服务未就绪"
	}
}

// DatabaseReady 把连接池 Ping 包成 ready 检查；失败只返回 ErrReadyDatabase，
// 不把 DSN 或驱动原文带出去。
func DatabaseReady(ping func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if ping == nil {
			return nil
		}
		if err := ping(ctx); err != nil {
			return ErrReadyDatabase
		}
		return nil
	}
}

// WorkerReady 检查一个进程内循环的最近成功轮询是否仍在 3× 轮询间隔内。
// last 为零值（从未成功轮询）或超时，返回 ErrReadyWorkers。h 为 nil 时跳过。
func WorkerReady(name string, h Heartbeat) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if h == nil {
			return nil
		}
		last := h.LastPollSuccessAt()
		limit := 3 * h.PollInterval()
		if limit <= 0 {
			limit = 3 * time.Second
		}
		if last.IsZero() || time.Since(last) > limit {
			return fmt.Errorf("%s: %w", name, ErrReadyWorkers)
		}
		return nil
	}
}

// ComposeReady 按顺序执行检查，任一失败立即返回。nil 检查被跳过。
// 装配约定：只放 DatabaseReady 与 WorkerReady，不要加入 ccloud / Alertmanager
// 等外部依赖——那些不可用不得导致 503。
func ComposeReady(checks ...func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		for _, check := range checks {
			if check == nil {
				continue
			}
			if err := check(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}
