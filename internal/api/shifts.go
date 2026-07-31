// Package api（本文件）：班次管理 + 轮换生成的五个端点（serviceability C2，R2）——
// schedule 下属班次 CRUD，以及基于 oncall.GenerateRotation 的批量轮换生成（预览/
// 落库两步）。
//
// 挂载方式与 members.go/ops_settings.go 完全同构：独立的 RegisterShiftRoutes 方法在
// 既有 *gin.Engine 上再开一个 "/api/v1/admin" 分组（config.go 顶部注释已说明 gin 允许
// 同前缀多个 Group，只要具体 path 不重复），不改动 server.go 的 mount()、也不复用
// config.go 的 configHandlers/ConfigDeps（同理由：依赖面独立，没必要塞进那个结构体）。
// main/harness 装配只需追加一行 router.RegisterShiftRoutes(schedules, users, shifts)。
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/oncall"
	"alert-system/internal/store"
)

// ShiftScheduleStore 抽象校验 :id 对应值班表存在所需的方法，*store.ScheduleRepo 天然满足。
type ShiftScheduleStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Schedule, error)
}

// ShiftUserStore 抽象校验 user_id 对应用户存在、以及响应体里 user_display_name 展示
// 字段所需的方法，*store.UserRepo 天然满足。List 用于一次性拉取全部用户建 id->name
// 映射，避免对每条班次单独查询 users 造成 N+1（受 store 现有 500 行上限约束，同
// listUsers 等既有端点的惯例，见 userDisplayNames）。
type ShiftUserStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error)
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.User, error)
}

// ShiftStore 抽象 *store.ShiftRepo 用到的方法。
type ShiftStore interface {
	Create(ctx context.Context, tx pgx.Tx, s store.Shift) (*store.Shift, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Shift, error)
	ListInWindow(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, limit int) ([]store.Shift, error)
	Update(ctx context.Context, tx pgx.Tx, s store.Shift) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
	ListOverlapping(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, excludeID string) ([]store.Shift, error)
}

// shiftHandlers 持有五个端点的依赖，方法接收者独立于 configHandlers/memberHandlers。
type shiftHandlers struct {
	router *Router

	schedules ShiftScheduleStore
	users     ShiftUserStore
	shifts    ShiftStore
}

// RegisterShiftRoutes 挂载班次管理 + 轮换生成五个端点，全部 admin only。
func (r *Router) RegisterShiftRoutes(schedules ShiftScheduleStore, users ShiftUserStore, shifts ShiftStore) *Router {
	h := &shiftHandlers{router: r, schedules: schedules, users: users, shifts: shifts}

	admin := r.engine.Group("/api/v1/admin")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/schedules/:id/shifts", h.list)
	admin.POST("/schedules/:id/shifts", h.create)
	admin.PATCH("/schedules/:id/shifts/:shiftID", h.update)
	admin.DELETE("/schedules/:id/shifts/:shiftID", h.remove)
	admin.POST("/schedules/:id/shifts/generate", h.generate)

	return r
}

// defaultShiftWindow 是 GET 列表未指定 from/to 时的默认查询窗口：R2 描述的排班场景
// 通常需要一眼看到"接下来一段时间"的安排，90 天覆盖季度规划的常见跨度。
const defaultShiftWindow = 90 * 24 * time.Hour

// shiftConflictError 标记「班次时段与已有班次重叠」（422），比 validationError 多带
// 一份冲突区间明细（R2.2「指出冲突区间」），供前端逐条展示每个冲突班次。
type shiftConflictError struct {
	details []gin.H
}

func (e *shiftConflictError) Error() string { return "班次时段与已有班次重叠" }

// conflictDetails 把重叠查询命中的班次转成响应体 details 数组的形状。
func conflictDetails(conflicts []store.Shift, names map[string]string) []gin.H {
	details := make([]gin.H, 0, len(conflicts))
	for _, s := range conflicts {
		details = append(details, gin.H{
			"shift_id":          s.ID,
			"user_display_name": displayNameOr(names, s.UserID),
			"starts_at":         s.StartsAt,
			"ends_at":           s.EndsAt,
		})
	}
	return details
}

// handleStoreErr 把仓储层错误映射成 HTTP 响应，口径与 configHandlers/memberHandlers
// 一致：不存在→404，其余→500 并记日志（shifts 表没有需要单独识别的唯一约束冲突：
// 时段重叠走的是业务层 shiftConflictError，不是 DB 约束报错）。
func (h *shiftHandlers) handleStoreErr(c *gin.Context, err error, op string) {
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	h.router.logger.Error("api: "+op+" 失败", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

// writeErrOr 处理 WithTx 闭包返回的 err：shiftConflictError→422 带 details，
// validationError（config.go 已定义，同包直接复用）→422，其余非 nil→走
// handleStoreErr。返回 true 表示已写响应。
func (h *shiftHandlers) writeErrOr(c *gin.Context, err error, op string) bool {
	if err == nil {
		return false
	}
	var cerr *shiftConflictError
	if errors.As(err, &cerr) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": cerr.Error(), "details": cerr.details})
		return true
	}
	var verr *validationError
	if errors.As(err, &verr) {
		unprocessable(c, verr.msg)
		return true
	}
	h.handleStoreErr(c, err, op)
	return true
}

// userDisplayNames 一次性拉取全部用户建 id -> display_name 映射（见 ShiftUserStore
// 文档对 N+1 的说明）。
func (h *shiftHandlers) userDisplayNames(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	users, err := h.users.List(ctx, tx, 0)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		names[u.ID] = u.DisplayName
	}
	return names, nil
}

// displayNameOr 查不到对应用户时回退成 user_id 本身，保证响应里这个字段永不为空
// （用户被删除等边界情况；与前端 useNameMaps 查不到回退显示 UUID 的兜底思路一致，
// 见 design.md C4）。
func displayNameOr(names map[string]string, userID string) string {
	if name, ok := names[userID]; ok {
		return name
	}
	return userID
}

func shiftJSON(s store.Shift, displayName string) gin.H {
	return gin.H{
		"id":                s.ID,
		"user_id":           s.UserID,
		"user_display_name": displayName,
		"starts_at":         s.StartsAt,
		"ends_at":           s.EndsAt,
	}
}

// list 处理 GET /api/v1/admin/schedules/:id/shifts?from=&to=（RFC3339，默认
// from=now、to=now+90d），复用 ShiftRepo.ListInWindow。
func (h *shiftHandlers) list(c *gin.Context) {
	scheduleID := c.Param("id")
	ctx := c.Request.Context()

	now := time.Now()
	from, to := now, now.Add(defaultShiftWindow)
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "非法 from：必须是 RFC3339 时间"})
			return
		}
		from = t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "非法 to：必须是 RFC3339 时间"})
			return
		}
		to = t
	}

	var shifts []store.Shift
	var names map[string]string
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.schedules.GetByID(ctx, tx, scheduleID); err != nil {
			return err
		}
		var listErr error
		shifts, listErr = h.shifts.ListInWindow(ctx, tx, scheduleID, from, to, 0)
		if listErr != nil {
			return listErr
		}
		var namesErr error
		names, namesErr = h.userDisplayNames(ctx, tx)
		return namesErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出班次")
		return
	}
	items := make([]gin.H, 0, len(shifts))
	for _, s := range shifts {
		items = append(items, shiftJSON(s, displayNameOr(names, s.UserID)))
	}
	c.JSON(http.StatusOK, gin.H{"shifts": items})
}

type shiftRequest struct {
	UserID   string    `json:"user_id"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// create 处理 POST /api/v1/admin/schedules/:id/shifts：schedule/user 不存在→404；
// ends_at<=starts_at→422（DB CHECK 兜底，这里提前给出中文文案）；与既有班次时段
// 重叠→422 带 details；成功→201。
func (h *shiftHandlers) create(c *gin.Context) {
	scheduleID := c.Param("id")
	var req shiftRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.UserID == "" {
		unprocessable(c, "user_id 不能为空")
		return
	}
	if !req.EndsAt.After(req.StartsAt) {
		unprocessable(c, "ends_at 必须晚于 starts_at")
		return
	}

	ctx := c.Request.Context()
	var out *store.Shift
	var displayName string
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.schedules.GetByID(ctx, tx, scheduleID); err != nil {
			return err
		}
		user, err := h.users.GetByID(ctx, tx, req.UserID)
		if err != nil {
			return err
		}
		displayName = user.DisplayName
		conflicts, err := h.shifts.ListOverlapping(ctx, tx, scheduleID, req.StartsAt, req.EndsAt, "")
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			names, namesErr := h.userDisplayNames(ctx, tx)
			if namesErr != nil {
				return namesErr
			}
			return &shiftConflictError{details: conflictDetails(conflicts, names)}
		}
		var createErr error
		out, createErr = h.shifts.Create(ctx, tx, store.Shift{ScheduleID: scheduleID, UserID: req.UserID, StartsAt: req.StartsAt, EndsAt: req.EndsAt})
		return createErr
	})
	if h.writeErrOr(c, err, "创建班次") {
		return
	}
	c.JSON(http.StatusCreated, shiftJSON(*out, displayName))
}

// update 处理 PATCH /api/v1/admin/schedules/:id/shifts/:shiftID：三字段（user_id/
// starts_at/ends_at）整体替换，语义同 updateSchedule/updateRoute 等既有 PATCH 端点
// （不是局部 JSON merge patch）。shiftID 不属于路径里的 schedule → 404（避免跨
// schedule 误改班次）；重叠校验排除自身（excludeID=shiftID）。
func (h *shiftHandlers) update(c *gin.Context) {
	scheduleID := c.Param("id")
	shiftID := c.Param("shiftID")
	var req shiftRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.UserID == "" {
		unprocessable(c, "user_id 不能为空")
		return
	}
	if !req.EndsAt.After(req.StartsAt) {
		unprocessable(c, "ends_at 必须晚于 starts_at")
		return
	}

	ctx := c.Request.Context()
	var out *store.Shift
	var displayName string
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		existing, err := h.shifts.GetByID(ctx, tx, shiftID)
		if err != nil {
			return err
		}
		if existing.ScheduleID != scheduleID {
			return store.ErrNotFound
		}
		user, err := h.users.GetByID(ctx, tx, req.UserID)
		if err != nil {
			return err
		}
		displayName = user.DisplayName
		conflicts, err := h.shifts.ListOverlapping(ctx, tx, scheduleID, req.StartsAt, req.EndsAt, shiftID)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			names, namesErr := h.userDisplayNames(ctx, tx)
			if namesErr != nil {
				return namesErr
			}
			return &shiftConflictError{details: conflictDetails(conflicts, names)}
		}
		updated := store.Shift{ID: shiftID, ScheduleID: scheduleID, UserID: req.UserID, StartsAt: req.StartsAt, EndsAt: req.EndsAt}
		if updateErr := h.shifts.Update(ctx, tx, updated); updateErr != nil {
			return updateErr
		}
		out = &updated
		return nil
	})
	if h.writeErrOr(c, err, "更新班次") {
		return
	}
	c.JSON(http.StatusOK, shiftJSON(*out, displayName))
}

// remove 处理 DELETE /api/v1/admin/schedules/:id/shifts/:shiftID：shiftID 不属于
// 路径里的 schedule → 404；成功 → 204。
func (h *shiftHandlers) remove(c *gin.Context) {
	scheduleID := c.Param("id")
	shiftID := c.Param("shiftID")
	ctx := c.Request.Context()

	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		existing, err := h.shifts.GetByID(ctx, tx, shiftID)
		if err != nil {
			return err
		}
		if existing.ScheduleID != scheduleID {
			return store.ErrNotFound
		}
		return h.shifts.Delete(ctx, tx, shiftID)
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除班次")
		return
	}
	c.Status(http.StatusNoContent)
}

type generateShiftsRequest struct {
	UserIDs              []string  `json:"user_ids"`
	ShiftDurationSeconds int       `json:"shift_duration_seconds"`
	StartsAt             time.Time `json:"starts_at"`
	Rounds               int       `json:"rounds"`
	DryRun               bool      `json:"dry_run"`
}

// generate 处理 POST /api/v1/admin/schedules/:id/shifts/generate：调用纯函数
// oncall.GenerateRotation 展开轮换（禁止在这里重新实现轮换逻辑），dry_run=true 只
// 返回预览（不落库、响应结构与 GET 列表一致但没有 id）；dry_run=false 在校验通过
// 后单事务批量 INSERT。校验与生成响应统一返回 200（而不是区分 200/201）：generate
// 是"预览或落库"的单一动作端点，前端已经靠请求里的 dry_run 字段区分两种语义，不需要
// 再靠状态码区分。
//
// 重叠校验：生成集内部由 GenerateRotation 保证连续、互不重叠，所以只需要用生成集的
// 整体区间 [首班 starts_at, 末班 ends_at) 查一次 ListOverlapping 就等价于逐条校验；
// 任一冲突则整体 422 不落库（不做"部分落库"）。
func (h *shiftHandlers) generate(c *gin.Context) {
	scheduleID := c.Param("id")
	var req generateShiftsRequest
	if !bindJSON(c, &req) {
		return
	}

	rotation, err := oncall.GenerateRotation(req.UserIDs, req.StartsAt, time.Duration(req.ShiftDurationSeconds)*time.Second, req.Rounds)
	if err != nil {
		unprocessable(c, err.Error())
		return
	}

	ctx := c.Request.Context()
	var created []store.Shift
	var names map[string]string
	err = h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.schedules.GetByID(ctx, tx, scheduleID); err != nil {
			return err
		}
		for _, userID := range req.UserIDs {
			if _, err := h.users.GetByID(ctx, tx, userID); err != nil {
				return err
			}
		}

		var namesErr error
		names, namesErr = h.userDisplayNames(ctx, tx)
		if namesErr != nil {
			return namesErr
		}

		if len(rotation) == 0 {
			return nil
		}
		from, to := rotation[0].StartsAt, rotation[len(rotation)-1].EndsAt
		conflicts, err := h.shifts.ListOverlapping(ctx, tx, scheduleID, from, to, "")
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			return &shiftConflictError{details: conflictDetails(conflicts, names)}
		}
		if req.DryRun {
			return nil
		}
		for _, rs := range rotation {
			s, createErr := h.shifts.Create(ctx, tx, store.Shift{ScheduleID: scheduleID, UserID: rs.UserID, StartsAt: rs.StartsAt, EndsAt: rs.EndsAt})
			if createErr != nil {
				return createErr
			}
			created = append(created, *s)
		}
		return nil
	})
	if h.writeErrOr(c, err, "生成轮换班次") {
		return
	}

	items := make([]gin.H, 0, len(rotation))
	if req.DryRun {
		for _, rs := range rotation {
			items = append(items, gin.H{
				"user_id":           rs.UserID,
				"user_display_name": displayNameOr(names, rs.UserID),
				"starts_at":         rs.StartsAt,
				"ends_at":           rs.EndsAt,
			})
		}
	} else {
		for _, s := range created {
			items = append(items, shiftJSON(s, displayNameOr(names, s.UserID)))
		}
	}
	c.JSON(http.StatusOK, gin.H{"shifts": items})
}
