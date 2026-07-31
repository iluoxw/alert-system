// Package api（本文件）：团队成员管理的四个端点（serviceability C1，R1）——团队成员
// 增删改查 + 最后一名 admin 护栏。
//
// 挂载方式与 ops_settings.go 完全同构：独立的 RegisterMemberRoutes 方法在既有
// *gin.Engine 上再开一个 "/api/v1/admin" 分组（config.go 顶部注释已说明 gin 允许
// 同前缀多个 Group，只要具体 path 不重复），不改动 server.go 的 mount()——四个端点
// 都是新 path，与 config.go/ops_settings.go 已注册的 path 不冲突。main/harness 装配
// 只需追加一行 router.RegisterMemberRoutes(teams, users, memberships)。
//
// handler 类型独立声明（不复用 configHandlers），理由同 ops_settings.go 文件头
// 注释：本文件全部端点都是 admin only，虽然权限层级与 configHandlers 一致，但依赖
// 面（store.MembershipRepo 的五个方法）与 ConfigDeps 无关，没有必要把 memberHandlers
// 也塞进 configHandlers/ConfigDeps 结构体，保持每个资源族独立可读。
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// MemberTeamStore 抽象校验 :id 对应团队存在所需的方法，*store.TeamRepo 天然满足。
type MemberTeamStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Team, error)
}

// MemberUserStore 抽象校验 user_id 对应用户存在、以及成员响应体里 username/
// display_name 展示字段所需的方法，*store.UserRepo 天然满足。
type MemberUserStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error)
}

// MemberStore 抽象 *store.MembershipRepo 用到的方法。
type MemberStore interface {
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string) ([]store.Member, error)
	Add(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error
	UpdateRole(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error
	Remove(ctx context.Context, tx pgx.Tx, teamID, userID string) error
	CountAdmins(ctx context.Context, tx pgx.Tx, teamID string) (int, error)
}

// memberHandlers 持有四个端点的依赖，方法接收者独立于 configHandlers/opsSettingsHandlers。
type memberHandlers struct {
	router *Router

	teams   MemberTeamStore
	users   MemberUserStore
	members MemberStore
}

// RegisterMemberRoutes 挂载团队成员管理四个端点，全部 admin only。
func (r *Router) RegisterMemberRoutes(teams MemberTeamStore, users MemberUserStore, members MemberStore) *Router {
	h := &memberHandlers{router: r, teams: teams, users: users, members: members}

	admin := r.engine.Group("/api/v1/admin")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/teams/:id/members", h.list)
	admin.POST("/teams/:id/members", h.add)
	admin.PATCH("/teams/:id/members/:userID", h.updateRole)
	admin.DELETE("/teams/:id/members/:userID", h.remove)

	return r
}

// validMemberRoles 是 memberships.role 列 CHECK 约束允许的取值（迁移
// 000002_org_routing.up.sql），API 层提前拒绝非法值以给出明确 422 而不是等 DB 报
// 约束错误。
var validMemberRoles = map[string]bool{"admin": true, "member": true}

func memberJSON(m store.Member) gin.H {
	return gin.H{
		"user_id":      m.UserID,
		"username":     m.Username,
		"display_name": m.DisplayName,
		"role":         m.Role,
	}
}

// handleStoreErr 把仓储层错误映射成 HTTP 响应，口径与 configHandlers.handleStoreErr
// 一致：不存在→404，唯一约束冲突（重复添加成员）→409，其余→500 并记日志。
func (h *memberHandlers) handleStoreErr(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, store.ErrMembershipConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		h.router.logger.Error("api: "+op+" 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// writeValidationOr 处理 WithTx 闭包返回的 err：validationError（config.go 已定义，
// 同包直接复用）→422，其余非 nil→走 handleStoreErr。返回 true 表示已写响应。
func (h *memberHandlers) writeValidationOr(c *gin.Context, err error, op string) bool {
	if err == nil {
		return false
	}
	var verr *validationError
	if errors.As(err, &verr) {
		unprocessable(c, verr.msg)
		return true
	}
	h.handleStoreErr(c, err, op)
	return true
}

// findMember 在 teamID 下按 userID 找当前成员记录，找不到时 found=false（不是错误——
// store.MembershipRepo 没有单条 Get 方法，护栏逻辑需要先知道"当前角色"才能判断是否
// 触碰最后一名 admin，这里用 ListByTeam 扫描获取，团队成员规模不大，可接受）。
func (h *memberHandlers) findMember(ctx context.Context, tx pgx.Tx, teamID, userID string) (store.Member, bool, error) {
	members, err := h.members.ListByTeam(ctx, tx, teamID)
	if err != nil {
		return store.Member{}, false, err
	}
	for _, m := range members {
		if m.UserID == userID {
			return m, true, nil
		}
	}
	return store.Member{}, false, nil
}

// list 处理 GET /api/v1/admin/teams/:id/members。
func (h *memberHandlers) list(c *gin.Context) {
	teamID := c.Param("id")
	ctx := c.Request.Context()

	var members []store.Member
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.teams.GetByID(ctx, tx, teamID); err != nil {
			return err
		}
		var listErr error
		members, listErr = h.members.ListByTeam(ctx, tx, teamID)
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出团队成员")
		return
	}
	items := make([]gin.H, 0, len(members))
	for _, m := range members {
		items = append(items, memberJSON(m))
	}
	c.JSON(http.StatusOK, gin.H{"members": items})
}

type addMemberRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// add 处理 POST /api/v1/admin/teams/:id/members：team/user 不存在 → 404；重复添加
// （ErrMembershipConflict）→ 409；role 非法 → 422；成功 → 201 返回新成员。
func (h *memberHandlers) add(c *gin.Context) {
	teamID := c.Param("id")
	var req addMemberRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.UserID == "" {
		unprocessable(c, "user_id 不能为空")
		return
	}
	if !validMemberRoles[req.Role] {
		unprocessable(c, "role 非法（只允许 admin/member）: "+req.Role)
		return
	}

	ctx := c.Request.Context()
	var resp gin.H
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.teams.GetByID(ctx, tx, teamID); err != nil {
			return err
		}
		user, err := h.users.GetByID(ctx, tx, req.UserID)
		if err != nil {
			return err
		}
		if err := h.members.Add(ctx, tx, teamID, req.UserID, req.Role); err != nil {
			return err
		}
		resp = memberJSON(store.Member{UserID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: req.Role})
		return nil
	})
	if err != nil {
		h.handleStoreErr(c, err, "添加团队成员")
		return
	}
	c.JSON(http.StatusCreated, resp)
}

type updateMemberRoleRequest struct {
	Role string `json:"role"`
}

// updateRole 处理 PATCH /api/v1/admin/teams/:id/members/:userID：成员不存在 → 404；
// role 非法 → 422；把该团队最后一名 admin 降级为 member → 422（护栏，见文件头
// 与 findMember 注释）；成功 → 200 返回更新后的成员。护栏判定与写操作在同一事务
// 内完成，防止并发窗口下两个请求同时通过判定、把最后一名 admin 一起降级。
func (h *memberHandlers) updateRole(c *gin.Context) {
	teamID := c.Param("id")
	userID := c.Param("userID")
	var req updateMemberRoleRequest
	if !bindJSON(c, &req) {
		return
	}
	if !validMemberRoles[req.Role] {
		unprocessable(c, "role 非法（只允许 admin/member）: "+req.Role)
		return
	}

	ctx := c.Request.Context()
	var resp gin.H
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		current, found, err := h.findMember(ctx, tx, teamID, userID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrNotFound
		}
		if current.Role == "admin" && req.Role != "admin" {
			count, err := h.members.CountAdmins(ctx, tx, teamID)
			if err != nil {
				return err
			}
			if count <= 1 {
				return &validationError{msg: "不能将团队最后一名管理员降级为普通成员"}
			}
		}
		if err := h.members.UpdateRole(ctx, tx, teamID, userID, req.Role); err != nil {
			return err
		}
		resp = memberJSON(store.Member{UserID: userID, Username: current.Username, DisplayName: current.DisplayName, Role: req.Role})
		return nil
	})
	if h.writeValidationOr(c, err, "更新团队成员角色") {
		return
	}
	c.JSON(http.StatusOK, resp)
}

// remove 处理 DELETE /api/v1/admin/teams/:id/members/:userID：成员不存在 → 404；
// 移除该团队最后一名 admin → 422（护栏）；成功 → 204。自降级/自移除服务端不特判
// 操作者身份（合法操作，确认提示是前端职责，见 design.md C1）。
func (h *memberHandlers) remove(c *gin.Context) {
	teamID := c.Param("id")
	userID := c.Param("userID")
	ctx := c.Request.Context()

	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		current, found, err := h.findMember(ctx, tx, teamID, userID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrNotFound
		}
		if current.Role == "admin" {
			count, err := h.members.CountAdmins(ctx, tx, teamID)
			if err != nil {
				return err
			}
			if count <= 1 {
				return &validationError{msg: "不能移除团队最后一名管理员"}
			}
		}
		return h.members.Remove(ctx, tx, teamID, userID)
	})
	if h.writeValidationOr(c, err, "移除团队成员") {
		return
	}
	c.Status(http.StatusNoContent)
}
