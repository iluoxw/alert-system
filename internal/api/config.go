// Package api（本文件）：配置管理 API（任务 27，R1.3/R5/R10/R13.5/R13.6）——
// integrations/teams/users/routes/schedules/escalation policies/notification
// policies 的全部 admin 专属 CRUD 入口。静默（silences）的 CRUD 单独放在
// silences.go，两者共用本文件定义的 Router.RegisterConfigRoutes 入口与
// configHandlers/ConfigDeps。
//
// 路由挂载方式（见任务上下文里对 server.go 独占修改权的说明）：本文件不改动
// server.go 的 mount()，而是在 RegisterConfigRoutes 里对同一个 *gin.Engine 再开一个
// "/api/v1/admin" 分组——gin 允许同前缀存在多个 Group，只要注册的具体 path 不重复
// （server.go 的 admin 分组只注册了 /ping，这里注册的路径都是新的，不冲突）。
// main 装配时需要在 NewRouter 之后调用 router.RegisterConfigRoutes(ConfigDeps{...})。
//
// 鉴权：全部端点走 r.sessionAuth() + r.requireAdmin()（与 server.go 的 /admin/ping
// 同一套中间件链，admin 口径="至少一个团队持有 admin 角色"，见 server.go 顶部注释）。
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"alert-system/internal/alertsource"
	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// ---- 依赖接口 ----
//
// 全部按"本文件实际用到的方法"精简定义（与 auth.go 的 UserStore、middleware.go 的
// SessionStore/MembershipStore 同一约定），使 main 装配时可以直接传对应的
// *store.XxxRepo（天然满足接口），测试则注入内存 fake（不需要 Docker/testcontainers）。
// 命名统一加 Config 前缀，避免与 auth.go 的 UserStore 等已有名字冲突。

type ConfigTeamStore interface {
	Create(ctx context.Context, tx pgx.Tx, name string) (*store.Team, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Team, error)
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Team, error)
	Update(ctx context.Context, tx pgx.Tx, id, name string) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigUserStore interface {
	Create(ctx context.Context, tx pgx.Tx, u store.User) (*store.User, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error)
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.User, error)
	Update(ctx context.Context, tx pgx.Tx, id, displayName, email string, enabled bool) error
	UpdateLarkOpenID(ctx context.Context, tx pgx.Tx, id, openID string) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigRouteStore interface {
	Create(ctx context.Context, tx pgx.Tx, route store.Route) (*store.Route, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error)
	ListOrdered(ctx context.Context, tx pgx.Tx, limit int) ([]store.Route, error)
	Update(ctx context.Context, tx pgx.Tx, route store.Route) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigScheduleStore interface {
	Create(ctx context.Context, tx pgx.Tx, s store.Schedule) (*store.Schedule, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Schedule, error)
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.Schedule, error)
	ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.Schedule, error)
	Update(ctx context.Context, tx pgx.Tx, id, name, timezone string) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigEscalationPolicyStore interface {
	Create(ctx context.Context, tx pgx.Tx, p store.EscalationPolicy) (*store.EscalationPolicy, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.EscalationPolicy, error)
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.EscalationPolicy, error)
	ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.EscalationPolicy, error)
	Update(ctx context.Context, tx pgx.Tx, p store.EscalationPolicy) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigNotificationPolicyStore interface {
	Create(ctx context.Context, tx pgx.Tx, p store.NotificationPolicy) (*store.NotificationPolicy, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.NotificationPolicy, error)
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.NotificationPolicy, error)
	ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.NotificationPolicy, error)
	Update(ctx context.Context, tx pgx.Tx, p store.NotificationPolicy) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigIntegrationStore interface {
	Create(ctx context.Context, tx pgx.Tx, in store.Integration) (*store.Integration, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Integration, error)
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Integration, error)
	Update(ctx context.Context, tx pgx.Tx, in store.Integration) error
	RotateHookIDHash(ctx context.Context, tx pgx.Tx, id, hookIDHash string) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

// ConfigSilenceStore/ConfigAlertSilenceStore 供 silences.go 使用，声明放在这里是因为
// ConfigDeps（下面）把全部配置面依赖集中在一处，main 装配只需要构造一个 ConfigDeps。

type ConfigSilenceStore interface {
	Create(ctx context.Context, tx pgx.Tx, s store.Silence) (*store.Silence, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Silence, error)
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Silence, error)
	Update(ctx context.Context, tx pgx.Tx, s store.Silence) error
	Delete(ctx context.Context, tx pgx.Tx, id string) error
}

type ConfigAlertSilenceStore interface {
	// DetachSilence 见 store.AlertRepo.DetachSilence（R10.5：删除静默后原子清理已
	// 静默告警的引用）。
	DetachSilence(ctx context.Context, tx pgx.Tx, silenceID string) error
}

// ConfigCacheInvalidator 由 pipeline 的失效器实现。调用必须发生在配置事务成功提交
// 之后，避免数据库回滚时却提前丢弃仍然有效的缓存。
type ConfigCacheInvalidator interface {
	InvalidateRoutes()
	InvalidateSilences()
}

// ConfigDeps 是 RegisterConfigRoutes 所需的全部仓储依赖。main 装配时传入对应的
// *store.XxxRepo 即可（均天然满足上面的精简接口）；测试注入内存 fake。
type ConfigDeps struct {
	Teams                ConfigTeamStore
	Users                ConfigUserStore
	Routes               ConfigRouteStore
	Schedules            ConfigScheduleStore
	EscalationPolicies   ConfigEscalationPolicyStore
	NotificationPolicies ConfigNotificationPolicyStore
	Integrations         ConfigIntegrationStore
	Silences             ConfigSilenceStore
	AlertSilences        ConfigAlertSilenceStore
	CacheInvalidator     ConfigCacheInvalidator
}

// configHandlers 持有配置管理 API 的全部依赖，方法接收者集中在这里（而不是直接挂在
// Router 上），因为 Router 的字段由 server.go 定义、本任务不能修改那个文件。
type configHandlers struct {
	tx     TxRunner
	deps   ConfigDeps
	logger *slog.Logger
}

// RegisterConfigRoutes 在既有 engine 上再开一个 "/api/v1/admin" 分组，注册全部配置管理
// 端点（admin only）。必须在 NewRouter 之后调用。
func (r *Router) RegisterConfigRoutes(deps ConfigDeps) {
	h := &configHandlers{tx: r.tx, deps: deps, logger: r.logger}
	admin := r.Engine().Group("/api/v1/admin", r.sessionAuth(), r.requireAdmin())

	admin.POST("/integrations", h.createIntegration)
	admin.GET("/integrations", h.listIntegrations)
	admin.GET("/integrations/:id", h.getIntegration)
	admin.PATCH("/integrations/:id", h.updateIntegration)
	admin.POST("/integrations/:id/rotate-hook", h.rotateIntegrationHook)
	admin.DELETE("/integrations/:id", h.deleteIntegration)
	admin.POST("/alert-sources/:id/rotate-hook", h.rotateAlertSourceHook)

	admin.POST("/teams", h.createTeam)
	admin.GET("/teams", h.listTeams)
	admin.GET("/teams/:id", h.getTeam)
	admin.PATCH("/teams/:id", h.updateTeam)
	admin.DELETE("/teams/:id", h.deleteTeam)

	admin.POST("/users", h.createUser)
	admin.GET("/users", h.listUsers)
	admin.GET("/users/:id", h.getUser)
	admin.PATCH("/users/:id", h.updateUser)
	admin.DELETE("/users/:id", h.deleteUser)

	admin.POST("/routes", h.createRoute)
	admin.GET("/routes", h.listRoutes)
	admin.GET("/routes/:id", h.getRoute)
	admin.PATCH("/routes/:id", h.updateRoute)
	admin.DELETE("/routes/:id", h.deleteRoute)

	admin.POST("/schedules", h.createSchedule)
	admin.GET("/schedules", h.listSchedules)
	admin.GET("/schedules/:id", h.getSchedule)
	admin.PATCH("/schedules/:id", h.updateSchedule)
	admin.DELETE("/schedules/:id", h.deleteSchedule)

	admin.POST("/escalation-policies", h.createEscalationPolicy)
	admin.GET("/escalation-policies", h.listEscalationPolicies)
	admin.GET("/escalation-policies/:id", h.getEscalationPolicy)
	admin.PATCH("/escalation-policies/:id", h.updateEscalationPolicy)
	admin.DELETE("/escalation-policies/:id", h.deleteEscalationPolicy)

	admin.POST("/notification-policies", h.createNotificationPolicy)
	admin.GET("/notification-policies", h.listNotificationPolicies)
	admin.GET("/notification-policies/:id", h.getNotificationPolicy)
	admin.PATCH("/notification-policies/:id", h.updateNotificationPolicy)
	admin.DELETE("/notification-policies/:id", h.deleteNotificationPolicy)

	h.registerSilenceRoutes(admin)
}

// ---- 通用 helper ----

// bindJSON 解析请求体到 dst；失败时写 400 并返回 false，调用方应立即 return。
func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON: " + err.Error()})
		return false
	}
	return true
}

// queryLimit 解析 ?limit= 查询参数；缺失或非法时返回 0（store 层 clampLimit 会把
// <=0 的值当作"用默认上限"处理）。
func queryLimit(c *gin.Context) int {
	limit, _ := strconv.Atoi(c.Query("limit"))
	return limit
}

// handleStoreErr 把仓储层错误映射成 HTTP 响应：不存在→404，唯一约束冲突→409，
// 其余视为基础设施故障→500（并记日志；500 之外的分支不记日志，属预期内的客户端错误）。
func (h *configHandlers) handleStoreErr(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, store.ErrRoutePriorityConflict),
		errors.Is(err, store.ErrIntegrationNameConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		h.logger.Error("api: "+op+" 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// unprocessable 写 422，供三类语义校验（渠道未注册/priority 冲突的等价语义校验点/
// 时间窗与引用合法性）统一使用。
func unprocessable(c *gin.Context, msg string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": msg})
}

// teamExists 校验 teamID 对应的团队存在；调用方在语义校验阶段（422，而非 404）使用，
// 因为"引用的团队不存在"是当前请求本身不合法，不是"你查询的资源不存在"。
func (h *configHandlers) teamExists(ctx context.Context, tx pgx.Tx, teamID string) (bool, error) {
	_, err := h.deps.Teams.GetByID(ctx, tx, teamID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// validationError 标记"事务内语义校验失败"（422），与仓储层返回的基础设施错误区分
// 开——handler 把校验逻辑放进 WithTx 闭包里（校验要读团队/渠道等数据），闭包只能靠
// 返回值区分"业务上不合法"与"数据库故障"，写成两种 error 类型即可用 errors.As 判别。
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// writeValidationOr 统一处理 WithTx 闭包返回的 err：validationError→422，其余非 nil
// →走 handleStoreErr（404/409/500）。返回 true 表示已经写了响应，调用方应立即 return；
// 返回 false 表示 err 为 nil，调用方继续走成功路径。
func (h *configHandlers) writeValidationOr(c *gin.Context, err error, op string) bool {
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

// isUniqueViolation 判断 err 是否是任意唯一约束冲突（不区分具体约束名）——用于
// teams.name/users.username 这类既有仓储方法（TeamRepo.Create/UserRepo.Update 等）
// 没有专门 sentinel error 的场景，不需要改动 store 层即可把冲突映射成 409。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- integrations ----
//
// R1.3 安全约束：明文 hook UUID 只在 Create/Rotate 的响应里出现一次；GetByID/List/
// Update 的响应体不含凭据（即便是 hash 也不返回）。UUID 是接入 URL 的 opaque bearer
// credential，不是 integration 主键；轮换它不会改变来源身份和历史关联。

// generateIntegrationHookID 使用 crypto/rand 生成 RFC 4122 UUID v4。122 位随机熵足以
// 作为 webhook URL 的 bearer credential；数据库只保存其 SHA-256。
func generateIntegrationHookID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

func hashIntegrationHookID(hookID string) string {
	sum := sha256.Sum256([]byte(hookID))
	return hex.EncodeToString(sum[:])
}

func hookCredentialJSON(in store.Integration, hookID string) gin.H {
	return gin.H{
		"id": in.ID, "name": in.Name,
		"hook_id": hookID, "webhook_path": "/ingest/v1/hook/" + hookID,
	}
}

type createIntegrationRequest struct {
	Kind            string          `json:"kind"`
	Name            string          `json:"name"`
	DefaultSeverity string          `json:"default_severity"`
	Enabled         *bool           `json:"enabled"`
	Config          json.RawMessage `json:"config"`
}

func integrationJSON(in store.Integration) gin.H {
	config := in.Config
	if config == nil {
		config = json.RawMessage(`{}`)
	}
	return gin.H{
		"id":               in.ID,
		"kind":             in.Kind,
		"name":             in.Name,
		"default_severity": in.DefaultSeverity,
		"enabled":          in.Enabled,
		"config":           config,
		"created_at":       in.CreatedAt,
		"updated_at":       in.UpdatedAt,
	}
}

func (h *configHandlers) createIntegration(c *gin.Context) {
	var req createIntegrationRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Kind == "" {
		unprocessable(c, "kind 不能为空")
		return
	}
	if req.Name == "" {
		unprocessable(c, "name 不能为空")
		return
	}
	if req.DefaultSeverity == "" {
		req.DefaultSeverity = string(domain.SeverityWarning)
	}
	if !domain.Severity(req.DefaultSeverity).IsValid() {
		unprocessable(c, "非法 default_severity: "+req.DefaultSeverity)
		return
	}
	if domain.IsAuthoritativeSourceKind(req.Kind) {
		cfg, err := store.ParseAlertmanagerSourceConfig(req.Config)
		if err != nil {
			unprocessable(c, err.Error())
			return
		}
		if cfg.APIURL != "" {
			if err := alertsource.ValidateConfig(req.Kind, cfg); err != nil {
				unprocessable(c, err.Error())
				return
			}
		}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	hookID, err := generateIntegrationHookID()
	if err != nil {
		h.logger.Error("api: 生成 integration hook UUID 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	ctx := c.Request.Context()
	var out *store.Integration
	err = h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		out, createErr = h.deps.Integrations.Create(ctx, tx, store.Integration{
			Kind:            req.Kind,
			Name:            req.Name,
			HookIDHash:      hashIntegrationHookID(hookID),
			DefaultSeverity: req.DefaultSeverity,
			Enabled:         enabled,
			Config:          req.Config,
		})
		return createErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "创建 integration")
		return
	}

	resp := integrationJSON(*out)
	for key, value := range hookCredentialJSON(*out, hookID) {
		resp[key] = value
	}
	c.JSON(http.StatusCreated, resp)
}

func (h *configHandlers) listIntegrations(c *gin.Context) {
	ctx := c.Request.Context()
	var list []store.Integration
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		list, listErr = h.deps.Integrations.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出 integration")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, in := range list {
		items = append(items, integrationJSON(in))
	}
	c.JSON(http.StatusOK, gin.H{"integrations": items})
}

func (h *configHandlers) getIntegration(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.Integration
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Integrations.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询 integration")
		return
	}
	c.JSON(http.StatusOK, integrationJSON(*out))
}

type updateIntegrationRequest struct {
	Kind            string          `json:"kind"`
	Name            string          `json:"name"`
	DefaultSeverity string          `json:"default_severity"`
	Enabled         bool            `json:"enabled"`
	Config          json.RawMessage `json:"config"`
}

func (h *configHandlers) updateIntegration(c *gin.Context) {
	var req updateIntegrationRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Kind == "" {
		unprocessable(c, "kind 不能为空")
		return
	}
	if req.Name == "" {
		unprocessable(c, "name 不能为空")
		return
	}
	if !domain.Severity(req.DefaultSeverity).IsValid() {
		unprocessable(c, "非法 default_severity: "+req.DefaultSeverity)
		return
	}
	if domain.IsAuthoritativeSourceKind(req.Kind) {
		cfg, err := store.ParseAlertmanagerSourceConfig(req.Config)
		if err != nil {
			unprocessable(c, err.Error())
			return
		}
		if cfg.APIURL != "" {
			if err := alertsource.ValidateConfig(req.Kind, cfg); err != nil {
				unprocessable(c, err.Error())
				return
			}
		}
	}

	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Integrations.Update(ctx, tx, store.Integration{
			ID:              id,
			Kind:            req.Kind,
			Name:            req.Name,
			DefaultSeverity: req.DefaultSeverity,
			Enabled:         req.Enabled,
			Config:          req.Config,
		})
	})
	if err != nil {
		h.handleStoreErr(c, err, "更新 integration")
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) rotateIntegrationHook(c *gin.Context) {
	h.rotateHook(c, false)
}

func (h *configHandlers) rotateAlertSourceHook(c *gin.Context) {
	h.rotateHook(c, true)
}

// rotateHook 先生成新 hook，再在同一事务内确认来源类型并替换哈希。事务提交后旧 URL
// 立即失效；失败时不返回新凭据，也不会影响原 URL。
func (h *configHandlers) rotateHook(c *gin.Context, alertmanagerOnly bool) {
	hookID, err := generateIntegrationHookID()
	if err != nil {
		h.logger.Error("api: 生成 integration hook UUID 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	ctx := c.Request.Context()
	id := c.Param("id")
	var in *store.Integration
	err = h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		in, getErr = h.deps.Integrations.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}
		if alertmanagerOnly && !domain.IsAuthoritativeSourceKind(in.Kind) {
			return &validationError{msg: "该 integration 不是权威告警数据源"}
		}
		return h.deps.Integrations.RotateHookIDHash(ctx, tx, id, hashIntegrationHookID(hookID))
	})
	if h.writeValidationOr(c, err, "轮换 integration hook") {
		return
	}
	c.JSON(http.StatusOK, hookCredentialJSON(*in, hookID))
}

func (h *configHandlers) deleteIntegration(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Integrations.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除 integration")
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- teams ----

func teamJSON(t store.Team) gin.H {
	return gin.H{"id": t.ID, "name": t.Name, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt}
}

type teamRequest struct {
	Name string `json:"name"`
}

func (h *configHandlers) createTeam(c *gin.Context) {
	var req teamRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == "" {
		unprocessable(c, "name 不能为空")
		return
	}
	ctx := c.Request.Context()
	var out *store.Team
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		out, createErr = h.deps.Teams.Create(ctx, tx, req.Name)
		return createErr
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "团队名已被占用"})
			return
		}
		h.handleStoreErr(c, err, "创建团队")
		return
	}
	c.JSON(http.StatusCreated, teamJSON(*out))
}

func (h *configHandlers) listTeams(c *gin.Context) {
	ctx := c.Request.Context()
	var list []store.Team
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		list, listErr = h.deps.Teams.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出团队")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, t := range list {
		items = append(items, teamJSON(t))
	}
	c.JSON(http.StatusOK, gin.H{"teams": items})
}

func (h *configHandlers) getTeam(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.Team
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Teams.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询团队")
		return
	}
	c.JSON(http.StatusOK, teamJSON(*out))
}

func (h *configHandlers) updateTeam(c *gin.Context) {
	var req teamRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == "" {
		unprocessable(c, "name 不能为空")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Teams.Update(ctx, tx, id, req.Name)
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "团队名已被占用"})
			return
		}
		h.handleStoreErr(c, err, "更新团队")
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteTeam(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Teams.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除团队")
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- users ----
//
// 密码：Create 接受明文 password，服务端 bcrypt 哈希后落库；响应体从不包含
// password/password_hash 字段。Update 不支持改密码/用户名（store.UserRepo.Update
// 本身的签名只覆盖 display_name/email/enabled，见 config_repos.go）——重置密码是
// 单独的操作，本期不做（YAGNI，见任务回报）。

func userJSON(u store.User) gin.H {
	return gin.H{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"email":        u.Email,
		"lark_open_id": u.LarkOpenID,
		"enabled":      u.Enabled,
		"created_at":   u.CreatedAt,
		"updated_at":   u.UpdatedAt,
	}
}

type createUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Enabled     *bool  `json:"enabled"`
}

func (h *configHandlers) createUser(c *gin.Context) {
	var req createUserRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" {
		unprocessable(c, "username 不能为空")
		return
	}
	if req.Password == "" {
		unprocessable(c, "password 不能为空")
		return
	}
	if req.DisplayName == "" {
		unprocessable(c, "display_name 不能为空")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.logger.Error("api: 生成密码哈希失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	ctx := c.Request.Context()
	var out *store.User
	err = h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		out, createErr = h.deps.Users.Create(ctx, tx, store.User{
			Username:     req.Username,
			PasswordHash: string(hash),
			DisplayName:  req.DisplayName,
			Email:        req.Email,
			Enabled:      enabled,
		})
		return createErr
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "username 已被占用"})
			return
		}
		h.handleStoreErr(c, err, "创建用户")
		return
	}
	c.JSON(http.StatusCreated, userJSON(*out))
}

func (h *configHandlers) listUsers(c *gin.Context) {
	ctx := c.Request.Context()
	var list []store.User
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		list, listErr = h.deps.Users.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出用户")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, u := range list {
		items = append(items, userJSON(u))
	}
	c.JSON(http.StatusOK, gin.H{"users": items})
}

func (h *configHandlers) getUser(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.User
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Users.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询用户")
		return
	}
	c.JSON(http.StatusOK, userJSON(*out))
}

type updateUserRequest struct {
	DisplayName string  `json:"display_name"`
	Email       string  `json:"email"`
	Enabled     bool    `json:"enabled"`
	LarkOpenID  *string `json:"lark_open_id"`
}

func (h *configHandlers) updateUser(c *gin.Context) {
	var req updateUserRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.DisplayName == "" {
		unprocessable(c, "display_name 不能为空")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if err := h.deps.Users.Update(ctx, tx, id, req.DisplayName, req.Email, req.Enabled); err != nil {
			return err
		}
		if req.LarkOpenID == nil {
			return nil
		}
		return h.deps.Users.UpdateLarkOpenID(ctx, tx, id, *req.LarkOpenID)
	})
	if err != nil {
		h.handleStoreErr(c, err, "更新用户")
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteUser(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Users.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除用户")
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- routes ----
//
// priority 冲突口径（任务上下文点 7 提出的待裁决问题，这里给出并记录裁决）：
// routes 表的 priority 列是 `INTEGER NOT NULL UNIQUE`（单列唯一约束，migrations/
// 000002_org_routing.up.sql），RouteRepo.ListOrdered 也没有按 team 过滤，是一份
// 全局有序列表——据此裁定 priority 冲突是"全局唯一"，不是"同 team 内唯一"，与
// store.ErrRoutePriorityConflict 已有的实现完全对齐，这里只需要把它映射成 409。

func routeJSON(r store.Route) gin.H {
	return gin.H{
		"id":                      r.ID,
		"priority":                r.Priority,
		"matchers":                r.Matchers,
		"target_team_id":          r.TargetTeamID,
		"group_by":                r.GroupBy,
		"group_wait_seconds":      r.GroupWaitSeconds,
		"group_interval_seconds":  r.GroupIntervalSeconds,
		"repeat_interval_seconds": r.RepeatIntervalSeconds,
		"group_mode":              r.GroupMode,
		"storm_threshold_per_min": r.StormThresholdPerMin,
		"created_at":              r.CreatedAt,
		"updated_at":              r.UpdatedAt,
	}
}

type routeRequest struct {
	Priority              int               `json:"priority"`
	Matchers              domain.MatcherSet `json:"matchers"`
	TargetTeamID          string            `json:"target_team_id"`
	GroupBy               []string          `json:"group_by"`
	GroupWaitSeconds      int               `json:"group_wait_seconds"`
	GroupIntervalSeconds  int               `json:"group_interval_seconds"`
	RepeatIntervalSeconds int               `json:"repeat_interval_seconds"`
	GroupMode             string            `json:"group_mode"`
	StormThresholdPerMin  int               `json:"storm_threshold_per_min"`
}

// normalizedGroupMode 把请求里缺省的 group_mode（""）落成 "label"，与 store 层
// （config_repos_routing.go 的 Create/Update 对空 GroupMode 的兜底）双保险对齐——
// 保证不带该字段的既有路由请求落库结果与引入 group_mode 之前逐字节一致（design.md
// C1：缺省值语义=关闭/现状）。
func normalizedGroupMode(m string) string {
	if m == "" {
		return "label"
	}
	return m
}

// validateRoute 校验 matcher 语法（正则合法性等，见 domain.Matcher.Validate）与
// target_team_id 是否存在；不要求 matchers 非空——留白 matchers 在当前领域模型下
// 恒不匹配（domain.MatcherSet.Matches），是否应该在 API 层拒绝这种"实质上的死配置"
// 不在本任务裁决范围内，按 brief 要求只做了列出的三类校验，这里不新增。
// 另外校验 group_mode（R1/C1）、storm_threshold_per_min（R3/C3）与
// repeat_interval_seconds：group_mode 提供时只能是 label/window；group_mode=window
// 与 group_by 语义互斥（window 是固定组键，不使用 group_by 计算分组），二者同时
// 非空即 422；storm_threshold_per_min、repeat_interval_seconds 均不能为负——0 是
// 合法值（表示不覆盖路由级间隔，续用 outbox 注入的全局默认，缺陷 1 修复的一部分）。
// group_by 本身（含保留键 __template__，R2/C2）在这里不做格式校验——现有实现从未
// 校验过 group_by 的元素格式（只有 matchers/target_team_id 两类语义校验），因此
// __template__ 无需任何放行改动即可直接使用。
// 返回值第三项非 nil 表示查团队存在性时发生了基础设施故障（而非校验不通过），
// 调用方应把它当普通 store 错误处理（500），不能当成校验失败（422）。
func (h *configHandlers) validateRoute(ctx context.Context, tx pgx.Tx, req routeRequest) (string, bool, error) {
	if err := req.Matchers.Validate(); err != nil {
		return "matchers 不合法: " + err.Error(), false, nil
	}
	if req.TargetTeamID == "" {
		return "target_team_id 不能为空", false, nil
	}
	exists, err := h.teamExists(ctx, tx, req.TargetTeamID)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return "target_team_id 不存在", false, nil
	}
	if req.GroupMode != "" && req.GroupMode != "label" && req.GroupMode != "window" {
		return "group_mode 只能是 label 或 window", false, nil
	}
	if req.GroupMode == "window" && len(req.GroupBy) > 0 {
		return "group_mode=window 与 group_by 互斥：窗口全归并模式不使用 group_by 分组键", false, nil
	}
	if req.StormThresholdPerMin < 0 {
		return "storm_threshold_per_min 不能为负数", false, nil
	}
	if req.RepeatIntervalSeconds < 0 {
		return "repeat_interval_seconds 不能为负数", false, nil
	}
	return "", true, nil
}

func (h *configHandlers) createRoute(c *gin.Context) {
	var req routeRequest
	if !bindJSON(c, &req) {
		return
	}

	ctx := c.Request.Context()
	var out *store.Route
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateRoute(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		var createErr error
		out, createErr = h.deps.Routes.Create(ctx, tx, store.Route{
			Priority:              req.Priority,
			Matchers:              req.Matchers,
			TargetTeamID:          req.TargetTeamID,
			GroupBy:               req.GroupBy,
			GroupWaitSeconds:      req.GroupWaitSeconds,
			GroupIntervalSeconds:  req.GroupIntervalSeconds,
			RepeatIntervalSeconds: req.RepeatIntervalSeconds,
			GroupMode:             normalizedGroupMode(req.GroupMode),
			StormThresholdPerMin:  req.StormThresholdPerMin,
		})
		return createErr
	})
	if h.writeValidationOr(c, err, "创建路由") {
		return
	}
	h.invalidateRoutes()
	c.JSON(http.StatusCreated, routeJSON(*out))
}

func (h *configHandlers) listRoutes(c *gin.Context) {
	ctx := c.Request.Context()
	var list []store.Route
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		list, listErr = h.deps.Routes.ListOrdered(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出路由")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, r := range list {
		items = append(items, routeJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"routes": items})
}

func (h *configHandlers) getRoute(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.Route
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Routes.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询路由")
		return
	}
	c.JSON(http.StatusOK, routeJSON(*out))
}

func (h *configHandlers) updateRoute(c *gin.Context) {
	var req routeRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateRoute(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		return h.deps.Routes.Update(ctx, tx, store.Route{
			ID:                    id,
			Priority:              req.Priority,
			Matchers:              req.Matchers,
			TargetTeamID:          req.TargetTeamID,
			GroupBy:               req.GroupBy,
			GroupWaitSeconds:      req.GroupWaitSeconds,
			GroupIntervalSeconds:  req.GroupIntervalSeconds,
			RepeatIntervalSeconds: req.RepeatIntervalSeconds,
			GroupMode:             normalizedGroupMode(req.GroupMode),
			StormThresholdPerMin:  req.StormThresholdPerMin,
		})
	})
	if h.writeValidationOr(c, err, "更新路由") {
		return
	}
	h.invalidateRoutes()
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteRoute(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Routes.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除路由")
		return
	}
	h.invalidateRoutes()
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) invalidateRoutes() {
	if h.deps.CacheInvalidator != nil {
		h.deps.CacheInvalidator.InvalidateRoutes()
	}
}

func (h *configHandlers) invalidateSilences() {
	if h.deps.CacheInvalidator != nil {
		h.deps.CacheInvalidator.InvalidateSilences()
	}
}

// ---- schedules ----

func scheduleJSON(s store.Schedule) gin.H {
	return gin.H{
		"id":         s.ID,
		"team_id":    s.TeamID,
		"name":       s.Name,
		"timezone":   s.Timezone,
		"created_at": s.CreatedAt,
		"updated_at": s.UpdatedAt,
	}
}

type scheduleRequest struct {
	TeamID   string `json:"team_id"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

func (h *configHandlers) validateSchedule(ctx context.Context, tx pgx.Tx, req scheduleRequest, requireTeam bool) (string, bool, error) {
	if req.Name == "" {
		return "name 不能为空", false, nil
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "非法 timezone: " + tz, false, nil
	}
	if requireTeam {
		if req.TeamID == "" {
			return "team_id 不能为空", false, nil
		}
		exists, err := h.teamExists(ctx, tx, req.TeamID)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return "team_id 不存在", false, nil
		}
	}
	return "", true, nil
}

func (h *configHandlers) createSchedule(c *gin.Context) {
	var req scheduleRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}

	ctx := c.Request.Context()
	var out *store.Schedule
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateSchedule(ctx, tx, req, true)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		var createErr error
		out, createErr = h.deps.Schedules.Create(ctx, tx, store.Schedule{TeamID: req.TeamID, Name: req.Name, Timezone: req.Timezone})
		return createErr
	})
	if h.writeValidationOr(c, err, "创建值班表") {
		return
	}
	c.JSON(http.StatusCreated, scheduleJSON(*out))
}

func (h *configHandlers) listSchedules(c *gin.Context) {
	ctx := c.Request.Context()
	teamID := c.Query("team_id")
	var list []store.Schedule
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		if teamID != "" {
			list, listErr = h.deps.Schedules.ListByTeam(ctx, tx, teamID, queryLimit(c))
		} else {
			list, listErr = h.deps.Schedules.ListAll(ctx, tx, queryLimit(c))
		}
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出值班表")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, s := range list {
		items = append(items, scheduleJSON(s))
	}
	c.JSON(http.StatusOK, gin.H{"schedules": items})
}

func (h *configHandlers) getSchedule(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.Schedule
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Schedules.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询值班表")
		return
	}
	c.JSON(http.StatusOK, scheduleJSON(*out))
}

func (h *configHandlers) updateSchedule(c *gin.Context) {
	var req scheduleRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateSchedule(ctx, tx, req, false)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		tz := req.Timezone
		if tz == "" {
			tz = "UTC"
		}
		return h.deps.Schedules.Update(ctx, tx, id, req.Name, tz)
	})
	if h.writeValidationOr(c, err, "更新值班表") {
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteSchedule(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Schedules.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除值班表")
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- escalation policies ----
//
// Targets 契约（任务上下文点 7："escalation policy 的 Targets 是团队 ID 列表，任务
// 23 已裁决的契约，校验团队存在"）：这里对每一级的每个 target 都校验对应团队存在，
// 校验失败给出具体是哪个 target 不合法，而不是笼统报错。

func escalationPolicyJSON(p store.EscalationPolicy) gin.H {
	return gin.H{
		"id":              p.ID,
		"team_id":         p.TeamID,
		"levels":          p.Levels,
		"severity_config": p.SeverityConfig,
		"created_at":      p.CreatedAt,
		"updated_at":      p.UpdatedAt,
	}
}

type escalationPolicyRequest struct {
	TeamID         string                  `json:"team_id"`
	Levels         []store.EscalationLevel `json:"levels"`
	SeverityConfig store.SeverityConfig    `json:"severity_config"`
}

func (h *configHandlers) validateEscalationPolicy(ctx context.Context, tx pgx.Tx, req escalationPolicyRequest) (string, bool, error) {
	if req.TeamID == "" {
		return "team_id 不能为空", false, nil
	}
	exists, err := h.teamExists(ctx, tx, req.TeamID)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return "team_id 不存在", false, nil
	}
	if len(req.Levels) == 0 {
		return "levels 不能为空", false, nil
	}
	for i, lvl := range req.Levels {
		if lvl.WaitSeconds < 0 {
			return fmt.Sprintf("levels[%d].wait_seconds 不能为负数", i), false, nil
		}
		if len(lvl.Targets) == 0 {
			return fmt.Sprintf("levels[%d].targets 不能为空", i), false, nil
		}
		for _, targetTeamID := range lvl.Targets {
			targetExists, err := h.teamExists(ctx, tx, targetTeamID)
			if err != nil {
				return "", false, err
			}
			if !targetExists {
				return fmt.Sprintf("levels[%d].targets 引用的团队不存在: %s", i, targetTeamID), false, nil
			}
		}
	}
	if msg, ok := validateSeverityConfig(req.SeverityConfig, len(req.Levels)); !ok {
		return msg, false, nil
	}
	return "", true, nil
}

// validateSeverityConfig 校验升级策略的 severity_config（R1 覆写口径）：
// - overrides 的键与 skip 的元素都必须是已定义的 severity；
// - overrides 的等待秒数不能为负；
// - overrides 数组长度不能超过 levels 数组长度（缺位回退默认，超出则无级可对应）；
// skip 与 overrides 同时命中同一 severity 不算冲突（skip 优先），无需互斥校验。
func validateSeverityConfig(cfg store.SeverityConfig, levelCount int) (string, bool) {
	for severity, waits := range cfg.Overrides {
		if !domain.Severity(severity).IsValid() {
			return fmt.Sprintf("severity_config.overrides 包含未知 severity: %s", severity), false
		}
		if len(waits) > levelCount {
			return fmt.Sprintf("severity_config.overrides[%s] 长度不能超过 levels 数组长度", severity), false
		}
		for i, wait := range waits {
			if wait < 0 {
				return fmt.Sprintf("severity_config.overrides[%s][%d] 不能为负数", severity, i), false
			}
		}
	}
	for _, severity := range cfg.Skip {
		if !domain.Severity(severity).IsValid() {
			return fmt.Sprintf("severity_config.skip 包含未知 severity: %s", severity), false
		}
	}
	return "", true
}

func (h *configHandlers) createEscalationPolicy(c *gin.Context) {
	var req escalationPolicyRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	var out *store.EscalationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateEscalationPolicy(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		var createErr error
		out, createErr = h.deps.EscalationPolicies.Create(ctx, tx, store.EscalationPolicy{TeamID: req.TeamID, Levels: req.Levels, SeverityConfig: req.SeverityConfig})
		return createErr
	})
	if h.writeValidationOr(c, err, "创建升级策略") {
		return
	}
	c.JSON(http.StatusCreated, escalationPolicyJSON(*out))
}

func (h *configHandlers) listEscalationPolicies(c *gin.Context) {
	ctx := c.Request.Context()
	teamID := c.Query("team_id")
	var list []store.EscalationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		if teamID != "" {
			list, listErr = h.deps.EscalationPolicies.ListByTeam(ctx, tx, teamID, queryLimit(c))
		} else {
			list, listErr = h.deps.EscalationPolicies.ListAll(ctx, tx, queryLimit(c))
		}
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出升级策略")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, p := range list {
		items = append(items, escalationPolicyJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"escalation_policies": items})
}

func (h *configHandlers) getEscalationPolicy(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.EscalationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.EscalationPolicies.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询升级策略")
		return
	}
	c.JSON(http.StatusOK, escalationPolicyJSON(*out))
}

func (h *configHandlers) updateEscalationPolicy(c *gin.Context) {
	var req escalationPolicyRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateEscalationPolicy(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		return h.deps.EscalationPolicies.Update(ctx, tx, store.EscalationPolicy{ID: id, TeamID: req.TeamID, Levels: req.Levels, SeverityConfig: req.SeverityConfig})
	})
	if h.writeValidationOr(c, err, "更新升级策略") {
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteEscalationPolicy(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.EscalationPolicies.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除升级策略")
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- notification policies ----
//
// R13.6：每个 step 引用的 channel kind 必须已在 internal/notify 的渠道注册表里注册，
// 复用已有的只读函数 notify.Validate（内部就是遍历 kinds 查 registry，未注册返回
// notify.ErrChannelNotRegistered）——已经是"判存在"的合适函数，不需要在 notify 包
// 新增文件（任务上下文点 5 允许的兜底方案用不上）。

func notificationPolicyJSON(p store.NotificationPolicy) gin.H {
	return gin.H{
		"id":         p.ID,
		"name":       p.Name,
		"matchers":   p.Matchers,
		"team_id":    p.TeamID,
		"steps":      p.Steps,
		"created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt,
	}
}

type notificationPolicyRequest struct {
	Name     string                  `json:"name"`
	Matchers domain.MatcherSet       `json:"matchers"`
	TeamID   string                  `json:"team_id"`
	Steps    store.NotificationSteps `json:"steps"`
}

var validNotificationModes = map[string]bool{"replace": true, "spread": true}

func (h *configHandlers) validateNotificationPolicy(ctx context.Context, tx pgx.Tx, req notificationPolicyRequest) (string, bool, error) {
	if req.Name == "" {
		return "name 不能为空", false, nil
	}
	if err := req.Matchers.Validate(); err != nil {
		return "matchers 不合法: " + err.Error(), false, nil
	}
	if req.TeamID != "" {
		exists, err := h.teamExists(ctx, tx, req.TeamID)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return "team_id 不存在", false, nil
		}
	}
	if len(req.Steps) == 0 {
		return "steps 不能为空", false, nil
	}
	for severity, steps := range req.Steps {
		if !severity.IsValid() {
			return "steps 包含非法 severity: " + string(severity), false, nil
		}
		if len(steps) == 0 {
			return fmt.Sprintf("steps[%s] 不能为空", severity), false, nil
		}
		for i, step := range steps {
			if step.WaitSeconds < 0 {
				return fmt.Sprintf("steps[%s][%d].wait_seconds 不能为负数", severity, i), false, nil
			}
			if !validNotificationModes[step.Mode] {
				return fmt.Sprintf("steps[%s][%d].mode 非法: %q（只允许 replace/spread）", severity, i, step.Mode), false, nil
			}
			if len(step.Channels) == 0 {
				return fmt.Sprintf("steps[%s][%d].channels 不能为空", severity, i), false, nil
			}
			if err := notify.Validate(step.Channels); err != nil {
				return fmt.Sprintf("steps[%s][%d].channels 引用了未注册的渠道: %v", severity, i, err), false, nil
			}
		}
	}
	return "", true, nil
}

func (h *configHandlers) createNotificationPolicy(c *gin.Context) {
	var req notificationPolicyRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	var out *store.NotificationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateNotificationPolicy(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		var createErr error
		out, createErr = h.deps.NotificationPolicies.Create(ctx, tx, store.NotificationPolicy{
			Name: req.Name, Matchers: req.Matchers, TeamID: req.TeamID, Steps: req.Steps,
		})
		return createErr
	})
	if h.writeValidationOr(c, err, "创建通知策略") {
		return
	}
	c.JSON(http.StatusCreated, notificationPolicyJSON(*out))
}

func (h *configHandlers) listNotificationPolicies(c *gin.Context) {
	ctx := c.Request.Context()
	teamID := c.Query("team_id")
	var list []store.NotificationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		if teamID != "" {
			list, listErr = h.deps.NotificationPolicies.ListByTeam(ctx, tx, teamID, queryLimit(c))
		} else {
			list, listErr = h.deps.NotificationPolicies.ListAll(ctx, tx, queryLimit(c))
		}
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出通知策略")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, p := range list {
		items = append(items, notificationPolicyJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"notification_policies": items})
}

func (h *configHandlers) getNotificationPolicy(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.NotificationPolicy
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.NotificationPolicies.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询通知策略")
		return
	}
	c.JSON(http.StatusOK, notificationPolicyJSON(*out))
}

func (h *configHandlers) updateNotificationPolicy(c *gin.Context) {
	var req notificationPolicyRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		msg, ok, verr := h.validateNotificationPolicy(ctx, tx, req)
		if verr != nil {
			return verr
		}
		if !ok {
			return &validationError{msg: msg}
		}
		return h.deps.NotificationPolicies.Update(ctx, tx, store.NotificationPolicy{
			ID: id, Name: req.Name, Matchers: req.Matchers, TeamID: req.TeamID, Steps: req.Steps,
		})
	})
	if h.writeValidationOr(c, err, "更新通知策略") {
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *configHandlers) deleteNotificationPolicy(c *gin.Context) {
	ctx := c.Request.Context()
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.NotificationPolicies.Delete(ctx, tx, c.Param("id"))
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除通知策略")
		return
	}
	c.Status(http.StatusNoContent)
}
