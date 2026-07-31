// Package api（本文件）：团队运营周期任务配置的两个端点（任务 10，R2 配置入口）——
// GET /api/v1/teams/:id/ops-settings（登录即可，member 也需要看到当前配置才能理解
// 值班播报/日报是否生效）、PUT /api/v1/admin/teams/:id/ops-settings（admin only，
// 保存即按新配置重排三个周期任务的定时器）。
//
// 与 config.go 共用 validationError/unprocessable/bindJSON 等既有 helper（同包，
// 直接复用，不重复定义）；handler 类型独立声明（不复用 configHandlers），理由同
// alerts.go 的 alertsHandler——GET 端点鉴权层级（登录即可）与 configHandlers 全部
// admin-only 的既有假设不符，混用会让权限模型难以从类型上一眼看出。
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/notify"
	"alert-system/internal/ops"
	"alert-system/internal/store"
)

// OpsSettingsTeamStore 抽象校验 :id 对应团队是否存在所需的方法，*store.TeamRepo 天然满足。
type OpsSettingsTeamStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Team, error)
}

// OpsSettingsStore 抽象 *store.OpsRepo 用到的方法。
type OpsSettingsStore interface {
	GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*store.OpsSettings, error)
	UpsertSettings(ctx context.Context, tx pgx.Tx, s store.OpsSettings) error
}

// OpsSettingsScheduler 抽象保存端点需要的定时器编排方法，*scheduler.Scheduler 天然满足。
type OpsSettingsScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
}

// opsSettingsHandlers 持有两个端点的依赖，方法接收者独立于 configHandlers/alertsHandler。
type opsSettingsHandlers struct {
	router *Router

	teams OpsSettingsTeamStore
	ops   OpsSettingsStore
	sched OpsSettingsScheduler
}

// RegisterOpsSettingsRoutes 挂载 ops-settings 两个端点：GET 挂到登录即可的 /api/v1
// 分组（复用 alerts.go 的鉴权层级约定），PUT 挂到 admin-only 的 /api/v1/admin 分组
// （复用 config.go 的鉴权层级约定）——两个分组各自独立 Group 调用，与既有分组不冲突
// （gin 允许同前缀多个 Group，只要具体 path 不重复，见 config.go 顶部注释）。
func (r *Router) RegisterOpsSettingsRoutes(teams OpsSettingsTeamStore, opsRepo OpsSettingsStore, sched OpsSettingsScheduler) *Router {
	h := &opsSettingsHandlers{router: r, teams: teams, ops: opsRepo, sched: sched}

	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/teams/:id/ops-settings", h.get)

	admin := r.engine.Group("/api/v1/admin")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.PUT("/teams/:id/ops-settings", h.update)

	return r
}

// hhmmPattern 校验 "HH:MM" 严格零填充格式（00-23:00-59）。internal/ops 的
// parseHHMM（未导出）额外接受非零填充的写法（如 "9:00"），这里刻意更严格——是它
// 接受集合的子集，凡是通过本校验的值 ops.NextFire 触发时一定能解析成功，不会出现
// "API 校验通过、定时器触发时才发现格式错误"的裂缝。
var hhmmPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// opsSettingsRequest 是 PUT 请求体，字段与 team_ops_settings 表列一一对应（snake_case
// 对齐迁移列名）。Channels 直接复用 store.OpsChannelTarget（已带 kind/target 的 json
// tag），避免为同一个 {kind,target} 形状再定义一遍。
type opsSettingsRequest struct {
	Timezone                  string                   `json:"timezone"`
	DailyReportEnabled        bool                     `json:"daily_report_enabled"`
	DailyReportTime           string                   `json:"daily_report_time"`
	StaleSweepEnabled         bool                     `json:"stale_sweep_enabled"`
	StaleSweepIntervalSeconds int                      `json:"stale_sweep_interval_seconds"`
	OncallBroadcastEnabled    bool                     `json:"oncall_broadcast_enabled"`
	OncallBroadcastTime       string                   `json:"oncall_broadcast_time"`
	Channels                  []store.OpsChannelTarget `json:"channels"`
}

// defaultOpsSettings 是团队从未保存过配置时的展示默认值，与迁移 000006
// team_ops_settings 各列的 DEFAULT 一一对应（GET 未配置团队返回 200+本默认值，而
// 非 404——前端需要"未配置也能渲染表单"）。
func defaultOpsSettings(teamID string) store.OpsSettings {
	return store.OpsSettings{
		TeamID:                    teamID,
		Timezone:                  "Asia/Shanghai",
		DailyReportTime:           "09:00",
		StaleSweepIntervalSeconds: 1800,
		OncallBroadcastTime:       "09:30",
		Channels:                  []store.OpsChannelTarget{},
	}
}

// opsSettingsJSON 是响应体的 JSON 形状，GET（含默认值场景）与 PUT（保存后回显）共用。
func opsSettingsJSON(s store.OpsSettings) gin.H {
	channels := s.Channels
	if channels == nil {
		channels = []store.OpsChannelTarget{}
	}
	return gin.H{
		"team_id":                      s.TeamID,
		"timezone":                     s.Timezone,
		"daily_report_enabled":         s.DailyReportEnabled,
		"daily_report_time":            s.DailyReportTime,
		"stale_sweep_enabled":          s.StaleSweepEnabled,
		"stale_sweep_interval_seconds": s.StaleSweepIntervalSeconds,
		"oncall_broadcast_enabled":     s.OncallBroadcastEnabled,
		"oncall_broadcast_time":        s.OncallBroadcastTime,
		"channels":                     channels,
	}
}

// toOpsSettings 把 store.OpsSettings 转换成 ops.Settings 供 ops.NextFire 使用。与
// internal/app/app.go 同名函数逻辑一致但各自独立维护——api 包不 import internal/app
// （避免装配层与 HTTP 层反向依赖），store.OpsSettings 是刻意的镜像类型（见
// internal/store/ops.go 文件头注释：store 不能直接依赖 ops，否则 store -> ops ->
// notify -> store 成环），两处转换分别是 store -> ops 类型转换的两个独立调用点。
func toOpsSettings(s store.OpsSettings) ops.Settings {
	channels := make([]ops.ChannelTarget, len(s.Channels))
	for i, c := range s.Channels {
		channels[i] = ops.ChannelTarget{Kind: c.Kind, Target: c.Target}
	}
	return ops.Settings{
		TeamID:                    s.TeamID,
		Timezone:                  s.Timezone,
		DailyReportEnabled:        s.DailyReportEnabled,
		DailyReportTime:           s.DailyReportTime,
		StaleSweepEnabled:         s.StaleSweepEnabled,
		StaleSweepIntervalSeconds: s.StaleSweepIntervalSeconds,
		OncallBroadcastEnabled:    s.OncallBroadcastEnabled,
		OncallBroadcastTime:       s.OncallBroadcastTime,
		Channels:                  channels,
	}
}

// teamExists 校验 teamID 对应的团队存在，逻辑与 configHandlers.teamExists 完全一致
// （独立声明理由见文件头注释）。
func (h *opsSettingsHandlers) teamExists(ctx context.Context, tx pgx.Tx, teamID string) (bool, error) {
	_, err := h.teams.GetByID(ctx, tx, teamID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// handleStoreErr 把 err 映射成 HTTP 响应：团队不存在 -> 404（:id 是资源自身标识，
// 语义等价于 getTeam 对不存在团队的既有处理）；其余一律 500 并记日志。ops-settings
// 没有唯一约束冲突场景（UpsertSettings 是 upsert，不会因重复保存而 409），不需要
// config.go handleStoreErr 的 409 分支。
func (h *opsSettingsHandlers) handleStoreErr(c *gin.Context, err error, op string) {
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "团队不存在"})
		return
	}
	h.router.logger.Error("api: "+op+" 失败", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

// get 处理 GET /api/v1/teams/:id/ops-settings。
func (h *opsSettingsHandlers) get(c *gin.Context) {
	teamID := c.Param("id")
	ctx := c.Request.Context()

	var resp gin.H
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		exists, err := h.teamExists(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}

		s, err := h.ops.GetSettings(ctx, tx, teamID)
		if errors.Is(err, store.ErrNotFound) {
			resp = opsSettingsJSON(defaultOpsSettings(teamID))
			return nil
		}
		if err != nil {
			return err
		}
		resp = opsSettingsJSON(*s)
		return nil
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询运营配置")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// validate 校验 PUT 请求体。teamID 不存在时直接返回 store.ErrNotFound（:id 是资源自身
// 标识，404 而非 422，理由见 handleStoreErr 注释）；其余是纯格式类语义校验（HH:MM/
// interval/timezone/channels），一律用 validationError 包装（config.go 已定义，同包
// 直接复用），由调用方映射 422。
func (h *opsSettingsHandlers) validate(ctx context.Context, tx pgx.Tx, teamID string, req opsSettingsRequest) error {
	exists, err := h.teamExists(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if !exists {
		return store.ErrNotFound
	}

	if _, err := time.LoadLocation(req.Timezone); err != nil {
		return &validationError{msg: "非法 timezone: " + req.Timezone}
	}
	if !hhmmPattern.MatchString(req.DailyReportTime) {
		return &validationError{msg: "daily_report_time 格式非法（期望 HH:MM）: " + req.DailyReportTime}
	}
	if !hhmmPattern.MatchString(req.OncallBroadcastTime) {
		return &validationError{msg: "oncall_broadcast_time 格式非法（期望 HH:MM）: " + req.OncallBroadcastTime}
	}
	if req.StaleSweepIntervalSeconds < 60 {
		return &validationError{msg: fmt.Sprintf("stale_sweep_interval_seconds 不能小于 60（当前 %d）", req.StaleSweepIntervalSeconds)}
	}
	kinds := make([]string, len(req.Channels))
	for i, ch := range req.Channels {
		kinds[i] = ch.Kind
	}
	if err := notify.Validate(kinds); err != nil {
		return &validationError{msg: "channels 引用了未注册的渠道: " + err.Error()}
	}
	return nil
}

// update 处理 PUT /api/v1/admin/teams/:id/ops-settings（admin only）。校验全部通过后
// 才在同一事务内先 UpsertSettings 再逐 kind Schedule/Cancel——校验失败时事务根本没有
// 开始，天然不会部分落库；Schedule/Cancel 失败时整个事务回滚，不会出现"配置已保存但
// 定时器没排上"的不一致状态。
func (h *opsSettingsHandlers) update(c *gin.Context) {
	teamID := c.Param("id")
	var req opsSettingsRequest
	if !bindJSON(c, &req) {
		return
	}

	ctx := c.Request.Context()
	now := h.router.clock.Now()
	var result store.OpsSettings
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if err := h.validate(ctx, tx, teamID, req); err != nil {
			return err
		}

		settings := store.OpsSettings{
			TeamID:                    teamID,
			Timezone:                  req.Timezone,
			DailyReportEnabled:        req.DailyReportEnabled,
			DailyReportTime:           req.DailyReportTime,
			StaleSweepEnabled:         req.StaleSweepEnabled,
			StaleSweepIntervalSeconds: req.StaleSweepIntervalSeconds,
			OncallBroadcastEnabled:    req.OncallBroadcastEnabled,
			OncallBroadcastTime:       req.OncallBroadcastTime,
			Channels:                  req.Channels,
		}
		if err := h.ops.UpsertSettings(ctx, tx, settings); err != nil {
			return err
		}

		opsSettings := toOpsSettings(settings)
		tasks := []struct {
			enabled bool
			kind    string
		}{
			{req.DailyReportEnabled, ops.TimerKindDailyReport},
			{req.StaleSweepEnabled, ops.TimerKindStaleSweep},
			{req.OncallBroadcastEnabled, ops.TimerKindOncallBroadcast},
		}
		for _, task := range tasks {
			if !task.enabled {
				if err := h.sched.Cancel(ctx, tx, task.kind, teamID); err != nil {
					return fmt.Errorf("api: 取消 ops 定时器失败(kind=%s): %w", task.kind, err)
				}
				continue
			}
			// err 只表示时区解析失败已回退 UTC；validate 已确认 timezone 合法，
			// 这里不会触发，忽略处理同 app.ReconcileOpsTimers。
			fireAt, _ := ops.NextFire(task.kind, opsSettings, now)
			payload, err := ops.EncodePayload(fireAt)
			if err != nil {
				return err
			}
			// 用 Schedule（替换语义）而非 ScheduleIfAbsent：保存语义是"配置变了
			// 按新配置重排期"，需要覆盖同 (kind, teamID) 的既有 pending 定时器。
			if err := h.sched.Schedule(ctx, tx, task.kind, teamID, fireAt, payload); err != nil {
				return fmt.Errorf("api: 注册 ops 定时器失败(kind=%s): %w", task.kind, err)
			}
		}
		result = settings
		return nil
	})
	if err != nil {
		var verr *validationError
		if errors.As(err, &verr) {
			unprocessable(c, verr.msg)
			return
		}
		h.handleStoreErr(c, err, "保存运营配置")
		return
	}
	c.JSON(http.StatusOK, opsSettingsJSON(result))
}
