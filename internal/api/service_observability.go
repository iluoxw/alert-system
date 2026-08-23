package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	// time/tzdata 把 IANA 时区库编进二进制：parseTimezone 用 time.LoadLocation 判定
	// "这个时区是否真的存在"，如果依赖运行镜像里的 /usr/share/zoneinfo，一个精简基础
	// 镜像就会让**全部**合法 IANA 时区变成 422。空白导入是标准库提供的官方做法，
	// 不需要改动任何 Docker/Kubernetes/OTel 配置。
	_ "time/tzdata"

	"github.com/gin-gonic/gin"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/facade"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/view"
)

// 本文件是 Service Observability 三个只读聚合 API 的 HTTP 边界。
//
// 它只做四件事：解析并按白名单校验查询参数、从 session 计算 actor、调用 facade、把
// typed 错误映射成稳定状态码与 reason code。业务编排、Health/Data 判定与显示投影都不
// 在这里——那是 facade/context/view/health 的职责。
//
// 四条硬约束：
//
//  1. 参数是白名单：sort/direction/health/data_status 是枚举，ID 是 UUID，字符串有长度
//     上限。非法输入在任何数据库查询之前就 422；
//  2. 游标是 opaque 的：它绑定版本与全部 filter/sort/direction 的指纹，解码失败、版本
//     不符或指纹不匹配同样在查询之前 422，绝不把游标内容当查询条件下传；
//  3. actor 只来自服务端 session：URL 里任何自报角色、team、权限快照一律忽略；
//  4. 响应绝不泄露内部细节：Profile 路径、SQL、PromQL、凭据、Go error 文本都不出现在
//     响应里。局部失败是 200 + Partial/Forbidden/Empty/Stale，不是 500。
//
// 既有 GET /api/v1/services 的路径与响应结构不受影响：这是一组新增的独立路由。

// PageViewService 是本层依赖的 facade 只读接口。*facade.Service 直接满足它。
//
// 它接收 context.Context 而不是 *gin.Context：请求取消必须原样传导到 facade 与各
// adapter，而不是在 HTTP 层被吞掉。
type PageViewService interface {
	Directory(ctx context.Context, query facade.DirectoryQuery) (model.DirectoryView, error)
	Overview(ctx context.Context, query facade.OverviewQuery) (model.OverviewView, error)
	Workspace(ctx context.Context, query facade.WorkspaceQuery) (model.WorkspaceView, error)
}

// serviceObservabilityHandlers 持有 facade。它刻意不持有任何 store 仓储：这条路径的
// 全部数据访问都在 facade 之下。
type serviceObservabilityHandlers struct {
	router    *Router
	pageViews PageViewService
}

// RegisterServiceObservabilityRoutes 挂载三个只读聚合端点。
//
// 三条路由都只要求登录（member 可读）：当前产品没有 service-level visibility policy，
// 因此可见性与既有 GET /api/v1/services 一致——服务不存在统一 404，不新增虚假的团队级
// 403。
func (r *Router) RegisterServiceObservabilityRoutes(pageViews PageViewService) *Router {
	h := &serviceObservabilityHandlers{router: r, pageViews: pageViews}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/service-observability", h.directory)
	authed.GET("/service-observability/:service_id", h.overview)
	authed.GET("/service-observability/:service_id/workspace", h.workspace)
	return r
}

// 稳定 reason code。前端按 code 分支，文案可以本地化调整而不破坏调用方。
const (
	reasonInvalidFilter       = "invalid_filter"
	reasonInvalidCursor       = "invalid_cursor"
	reasonInvalidLimit        = "invalid_limit"
	reasonInvalidWindow       = "invalid_window"
	reasonInvalidTimeRange    = "invalid_time_range"
	reasonInvalidTimezone     = "invalid_timezone"
	reasonInvalidFocus        = "invalid_focus"
	reasonScopeTooBroad       = "scope_too_broad"
	reasonProfileUnconfigured = "profile_unconfigured"
	reasonServiceNotFound     = "service_not_found"
	reasonUpstreamUnavailable = "service_observability_unavailable"
)

// 参数长度与形状上界。字符串参数一律有界：无界输入会被下传到 ILIKE 或 JSONB 匹配。
const (
	maxEnvironmentLength = 64
	maxTierLength        = 16
	maxQueryLength       = 128
	maxServiceIDLength   = 64
	maxTimezoneLength    = 64
	maxFocusKindLength   = 64
	maxFocusValueLength  = 256
	maxCSVValues         = 8
)

var (
	// environmentPattern 覆盖既有登记里的环境命名（production/prod/staging/uat…）。
	environmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// tierPattern 与 service_registrations.tier 的 CHECK 一致。
	tierPattern = regexp.MustCompile(`^tier-[0-3]$`)
	// focusKindPattern 与 Profile schema 的 identifier 一致。
	focusKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	// timezonePattern 只接受 IANA 风格的显示时区。它只影响显示，不改变任何查询边界。
	timezonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+/-]{0,63}$`)
)

// directory 处理 GET /api/v1/service-observability。
func (h *serviceObservabilityHandlers) directory(c *gin.Context) {
	query, err := parseDirectoryQuery(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	query.Subject = subjectFromSession(c)

	result, err := h.pageViews.Directory(c.Request.Context(), query)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// overview 处理 GET /api/v1/service-observability/:service_id。
func (h *serviceObservabilityHandlers) overview(c *gin.Context) {
	serviceID, err := parseServiceID(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	environment, err := parseEnvironment(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	windowSeconds, err := parseWindowSeconds(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}

	result, err := h.pageViews.Overview(c.Request.Context(), facade.OverviewQuery{
		Subject:       subjectFromSession(c),
		ServiceID:     serviceID,
		Environment:   environment,
		WindowSeconds: windowSeconds,
	})
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// workspace 处理 GET /api/v1/service-observability/:service_id/workspace。
func (h *serviceObservabilityHandlers) workspace(c *gin.Context) {
	serviceID, err := parseServiceID(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	environment, err := parseEnvironment(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	focus, err := parseFocus(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	from, to, err := parseTimeRange(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	timezone, err := parseTimezone(c)
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}

	result, err := h.pageViews.Workspace(c.Request.Context(), facade.WorkspaceQuery{
		Subject:     subjectFromSession(c),
		ServiceID:   serviceID,
		Environment: environment,
		Focus:       focus,
		From:        from,
		To:          to,
		Timezone:    timezone,
	})
	if err != nil {
		respondServiceObservabilityError(c, h.router, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// subjectFromSession 只从服务端会话计算 actor。
//
// URL 里的任何自报角色、team 或权限快照都被忽略：客户端可控的权限输入等于让调用方
// 自选权限。team_ids 目前为空——当前没有 service-level visibility policy，也没有
// "列出某用户全部团队"的仓储；它只会进入服务端内部的 permission_snapshot_id 指纹，
// 不影响任何可见性判定，也不出现在响应里。
func subjectFromSession(c *gin.Context) soctx.Subject {
	rbac, _ := UserFromContext(c)
	roles := []string{"member"}
	if rbac.IsAdmin {
		roles = append(roles, "admin")
	}
	return soctx.Subject{UserID: rbac.UserID, Roles: roles}
}

// parseDirectoryQuery 按白名单解析目录参数。
func parseDirectoryQuery(c *gin.Context) (facade.DirectoryQuery, error) {
	query := facade.DirectoryQuery{}

	environment, err := parseEnvironment(c)
	if err != nil {
		return query, err
	}
	query.Environment = environment

	if teamID := strings.TrimSpace(c.Query("team_id")); teamID != "" {
		if !uuidPattern.MatchString(teamID) {
			return query, &reasonCodedError{code: reasonInvalidFilter, message: "team_id 必须是合法 UUID"}
		}
		query.TeamID = teamID
	}
	if tier := strings.TrimSpace(c.Query("tier")); tier != "" {
		if len(tier) > maxTierLength || !tierPattern.MatchString(tier) {
			return query, &reasonCodedError{code: reasonInvalidFilter, message: "tier 只支持 tier-0 到 tier-3"}
		}
		query.Tier = tier
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if len(q) > maxQueryLength || containsControlCharacters(q) {
			return query, &reasonCodedError{code: reasonInvalidFilter, message: "搜索片段过长或包含非法字符"}
		}
		query.Query = q
	}

	health, err := parseHealthFilter(c.Query("health"))
	if err != nil {
		return query, err
	}
	query.Health = health

	dataStatus, err := parseDataStatusFilter(c.Query("data_status"))
	if err != nil {
		return query, err
	}
	query.DataStatus = dataStatus

	sortSpec, err := parseDirectorySort(c)
	if err != nil {
		return query, err
	}
	query.Sort = sortSpec

	if cursor := c.Query("cursor"); cursor != "" {
		// 长度是解码前的第一道校验：超长游标不进 base64、不进 JSON、更不进 SQL。
		if len(cursor) > facade.MaxCursorLength {
			return query, &reasonCodedError{code: reasonInvalidCursor, message: "游标非法或已过期，请重新从第一页开始"}
		}
		query.Cursor = cursor
	}

	limit, err := parseLimit(c)
	if err != nil {
		return query, err
	}
	query.Limit = limit
	return query, nil
}

func parseDirectorySort(c *gin.Context) (view.DirectorySort, error) {
	spec := view.DirectorySort{}
	sortKey := strings.TrimSpace(c.Query("sort"))
	if sortKey != "" {
		if !view.IsDirectorySortKey(sortKey) {
			return spec, &reasonCodedError{
				code:    reasonInvalidFilter,
				message: "sort 只支持 service/owner/tier/health/fault/data/observed",
			}
		}
		spec.Key = view.DirectorySortKey(sortKey)
	}
	switch direction := strings.TrimSpace(c.Query("direction")); direction {
	case "", "asc":
	case "desc":
		spec.Descending = true
	default:
		return view.DirectorySort{}, &reasonCodedError{
			code: reasonInvalidFilter, message: "direction 只支持 asc 或 desc",
		}
	}
	return spec, nil
}

func parseLimit(c *gin.Context) (int, error) {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > facade.MaxPageLimit {
		return 0, &reasonCodedError{
			code:    reasonInvalidLimit,
			message: "limit 必须是 1 到 " + strconv.Itoa(facade.MaxPageLimit) + " 之间的整数",
		}
	}
	return limit, nil
}

func parseHealthFilter(raw string) ([]model.Health, error) {
	values, err := splitCSV(raw)
	if err != nil {
		return nil, err
	}
	statuses := make([]model.Health, 0, len(values))
	for _, value := range values {
		status := model.Health(value)
		switch status {
		case model.HealthHealthy, model.HealthDegraded, model.HealthCritical, model.HealthUnknown:
			statuses = append(statuses, status)
		default:
			return nil, &reasonCodedError{
				code:    reasonInvalidFilter,
				message: "health 只支持 healthy/degraded/critical/unknown",
			}
		}
	}
	return statuses, nil
}

func parseDataStatusFilter(raw string) ([]model.DataState, error) {
	values, err := splitCSV(raw)
	if err != nil {
		return nil, err
	}
	states := make([]model.DataState, 0, len(values))
	for _, value := range values {
		state := model.DataState(value)
		switch state {
		case model.DataStateComplete, model.DataStatePartial,
			model.DataStateStale, model.DataStateUnconfigured:
			states = append(states, state)
		default:
			return nil, &reasonCodedError{
				code:    reasonInvalidFilter,
				message: "data_status 只支持 complete/partial/stale/unconfigured",
			}
		}
	}
	return states, nil
}

// splitCSV 解析 CSV 枚举参数。空串表示不筛；重复值去重；数量有上界。
func splitCSV(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	if len(parts) > maxCSVValues {
		return nil, &reasonCodedError{code: reasonInvalidFilter, message: "筛选值过多"}
	}
	seen := make(map[string]struct{}, len(parts))
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, &reasonCodedError{code: reasonInvalidFilter, message: "筛选值不能为空"}
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func parseServiceID(c *gin.Context) (string, error) {
	serviceID := strings.TrimSpace(c.Param("service_id"))
	if serviceID == "" || len(serviceID) > maxServiceIDLength {
		// 形状都不满足的 ID 一律按"服务不存在"处理，不泄露存在性，也不下传数据库。
		return "", serviceobs.ErrServiceNotFound
	}
	return serviceID, nil
}

func parseEnvironment(c *gin.Context) (string, error) {
	environment := strings.TrimSpace(c.Query("environment"))
	if environment == "" {
		return "", nil
	}
	if len(environment) > maxEnvironmentLength || !environmentPattern.MatchString(environment) {
		return "", &reasonCodedError{code: reasonInvalidFilter, message: "environment 格式非法"}
	}
	return environment, nil
}

func parseWindowSeconds(c *gin.Context) (int, error) {
	raw := strings.TrimSpace(c.Query("window_seconds"))
	if raw == "" {
		return 0, nil
	}
	windowSeconds, err := strconv.Atoi(raw)
	if err != nil || windowSeconds < 1 {
		return 0, &reasonCodedError{code: reasonInvalidWindow, message: "window_seconds 必须是正整数"}
	}
	return windowSeconds, nil
}

// parseFocus 解析 focus。两个参数是一个整体：只传一个无法判断意图，静默补另一半会让
// 用户看到一个自己没请求过的 focus。
func parseFocus(c *gin.Context) (*model.Focus, error) {
	kind := strings.TrimSpace(c.Query("focus_kind"))
	value := strings.TrimSpace(c.Query("focus_value"))
	if kind == "" && value == "" {
		return nil, nil
	}
	if kind == "" || value == "" {
		return nil, &reasonCodedError{
			code: reasonInvalidFocus, message: "focus_kind 与 focus_value 必须同时提供",
		}
	}
	if len(kind) > maxFocusKindLength || !focusKindPattern.MatchString(kind) {
		return nil, &reasonCodedError{code: reasonInvalidFocus, message: "focus_kind 格式非法"}
	}
	if len(value) > maxFocusValueLength || containsControlCharacters(value) {
		return nil, &reasonCodedError{code: reasonInvalidFocus, message: "focus_value 过长或包含非法字符"}
	}
	return &model.Focus{Kind: kind, Value: value}, nil
}

// parseTimeRange 解析 from/to。
//
// 两者必须同时出现且是带偏移量的 RFC3339 绝对时间（解析后统一归一到 UTC）。都缺省时
// 返回两个零值，由服务端用**单次** now 与 Profile 默认窗口生成 [now-window, now]；
// 窗口是否整秒、是否超上界由 assembler 判定，这里不重复实现一套。
func parseTimeRange(c *gin.Context) (time.Time, time.Time, error) {
	rawFrom := strings.TrimSpace(c.Query("from"))
	rawTo := strings.TrimSpace(c.Query("to"))
	if rawFrom == "" && rawTo == "" {
		return time.Time{}, time.Time{}, nil
	}
	if rawFrom == "" || rawTo == "" {
		return time.Time{}, time.Time{}, &reasonCodedError{
			code: reasonInvalidTimeRange, message: "from 与 to 必须同时提供",
		}
	}
	from, err := parseAbsoluteTime(rawFrom)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseAbsoluteTime(rawTo)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func parseAbsoluteTime(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, &reasonCodedError{
			code:    reasonInvalidTimeRange,
			message: "from/to 必须是带时区的绝对 RFC3339 时间",
		}
	}
	return parsed.UTC(), nil
}

// parseTimezone 解析 timezone。
//
// 两道校验缺一不可：
//
//  1. 形状与长度（挡住控制字符、超长串与明显不是时区标识的输入）；
//  2. `time.LoadLocation` —— 形状合法但不存在的时区（例如 `Mars/Olympus`）必须 422
//     `invalid_timezone`，而不是被当成成功响应然后在前端静默回落成另一个时区显示。
//     "格式看起来对"不等于"这个时区真的存在"。
//
// timezone 仍然只影响显示，不改变 from/to 的查询边界。
func parseTimezone(c *gin.Context) (string, error) {
	timezone := strings.TrimSpace(c.Query("timezone"))
	if timezone == "" {
		return "", nil
	}
	if len(timezone) > maxTimezoneLength || !timezonePattern.MatchString(timezone) {
		return "", &reasonCodedError{code: reasonInvalidTimezone, message: "timezone 格式非法"}
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return "", &reasonCodedError{code: reasonInvalidTimezone, message: "timezone 不是可用的 IANA 时区"}
	}
	return timezone, nil
}

func containsControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// respondServiceObservabilityError 把 typed 错误映射成稳定状态码与 reason code。
//
// 映射表就是设计稿的 HTTP 与 section 状态表：
//
//	service 不存在 / 显式环境不匹配        -> 404 service_not_found
//	Profile 未配置（仅工作区）             -> 409 profile_unconfigured
//	focus 非法                             -> 422 invalid_focus
//	时间窗或基数超限                       -> 422 scope_too_broad
//	游标/筛选/枚举/长度非法                -> 422（各自的稳定 code）
//	关键依赖不可用、取消、未知错误         -> 503 页面级 error
//
// 局部来源失败根本不会走到这里：那是 200 + Partial/Forbidden/Empty/Stale 的 view 状态。
// 响应体只有 error 文案与 reason_code，绝不带 Go error 文本、路径、SQL 或查询模板。
func respondServiceObservabilityError(c *gin.Context, router *Router, err error) {
	var coded *reasonCodedError
	if errors.As(err, &coded) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": coded.message, "reason_code": coded.code})
		return
	}
	switch {
	case errors.Is(err, serviceobs.ErrServiceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "服务不存在", "reason_code": reasonServiceNotFound})
	case errors.Is(err, serviceobs.ErrProfileUnconfigured):
		c.JSON(http.StatusConflict, gin.H{
			"error":       "该服务尚未配置 Service Profile，无法打开可观测性工作区",
			"reason_code": reasonProfileUnconfigured,
		})
	case errors.Is(err, serviceobs.ErrInvalidFocus):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "focus 不满足该服务已发布 Profile 的定义", "reason_code": reasonInvalidFocus,
		})
	case errors.Is(err, serviceobs.ErrScopeTooBroad):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "查询范围超出限制，请收窄时间窗或筛选条件", "reason_code": reasonScopeTooBroad,
		})
	case errors.Is(err, facade.ErrInvalidCursor):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "游标非法或已过期，请重新从第一页开始", "reason_code": reasonInvalidCursor,
		})
	case errors.Is(err, soctx.ErrInvalidTimeRange):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":       "时间窗非法：from 与 to 必须成对出现、为绝对时间且是整秒窗口",
			"reason_code": reasonInvalidTimeRange,
		})
	case errors.Is(err, soctx.ErrInvalidTimezone):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "timezone 格式非法", "reason_code": reasonInvalidTimezone,
		})
	default:
		// 只有稳定机器码进响应；具体原因（含内部 error 文本）只进服务端日志。
		router.logger.Error("api: service observability 页面视图不可用", "err", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "服务可观测性视图暂时不可用", "reason_code": reasonUpstreamUnavailable,
		})
	}
}
