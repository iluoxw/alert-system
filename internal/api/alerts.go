package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// AlertsStore 抽象 store.AlertRepo 用到的方法（列表/详情/处置动作，R4/NFR-可用性）。
// *store.AlertRepo 天然满足这个接口。与已有的 AlertStore（lark_callback.go，只有
// GetByID/UpdateStatus）刻意分开命名——本包同一个具体类型可以同时结构性满足两个
// 接口，互不干扰；这里额外需要 List，没有理由反过来扩大 lark 回调那个更窄的接口。
type AlertsStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
	List(ctx context.Context, tx pgx.Tx, filter store.AlertFilter) (store.AlertPage, error)
	// FingerprintStats/ListRecentResolvedByFingerprint 供告警详情页"噪声画像"
	// （ui-enrichment 任务4）读取同 fingerprint 的历史发生次数/平均耗时/最近已解决
	// 记录。*store.AlertRepo 天然满足。
	FingerprintStats(ctx context.Context, tx pgx.Tx, fingerprint string, since time.Time) (int, *float64, error)
	ListRecentResolvedByFingerprint(ctx context.Context, tx pgx.Tx, fingerprint, excludeID string, limit int) ([]store.ResolvedRef, error)
}

// AlertTimelineStore 抽象 store.TimelineRepo 用到的方法。与已有的 TimelineStore
// （lark_callback.go，只有 Append）分开命名，理由同 AlertsStore。
type AlertTimelineStore interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
	ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error)
}

// ChangeEventLister 抽象 store.ChangeEventRepo.ListRecentByResource，供告警详情页
// 展示"最近变更"（R7.4/R7.5）。
type ChangeEventLister interface {
	ListRecentByResource(ctx context.Context, tx pgx.Tx, resourceRef map[string]string, since time.Time, limit int) ([]store.ChangeEvent, error)
}

// EscalationStopper 抽象 oncall.Chain.Stop：取消 alert 的人员升级链定时器。本包不
// import oncall（避免 api -> oncall 的直接依赖，装配细节留给 main），只声明结构性
// 满足需要的这一个方法——*oncall.Chain 天然满足。Ack/Resolve 都要调用它：认领或
// 解决之后不应该再有升级链继续推进。
type EscalationStopper interface {
	Stop(ctx context.Context, tx pgx.Tx, alertID string) error
}

// LifecycleTimerCanceler 抽象取消告警生命周期收尾类定时器（TTL 自动恢复、
// repeat_interval 重复提醒等）。任务 24 的 notify.LifecycleHandlers.
// CancelLifecycleTimers 方法签名与此一致，天然结构性满足——本包不 import notify，
// 不直接依赖任务 24 的具体符号，只依赖这个形状；真实装配（把 *notify.
// LifecycleHandlers 传进来）留给 main。只有 Resolve 需要调用：告警进入终态后不应
// 再有 TTL/重复提醒定时器残留触发。
type LifecycleTimerCanceler interface {
	CancelLifecycleTimers(ctx context.Context, tx pgx.Tx, alertID string) error
}

// defaultRecentChangesWindow/defaultRecentChangesLimit 是详情页"最近变更"
// （R7.5）的默认回看窗口与条数上限。MVP 阶段不做成可配置项——按团队/路由细分
// 展示窗口不是本任务范围，未来如需要可在这里加参数化。
const (
	defaultRecentChangesWindow = 24 * time.Hour
	defaultRecentChangesLimit  = 20
)

// maxResolveNoteLength 是 Resolve 处置说明（R5，"怎么修的"）的字符数上限，design.md
// C5 定的 1000 字符；超限 422（复用 unprocessable，与 config.go 的语义校验同一口径）。
const maxResolveNoteLength = 1000

// statsWindow/statsRecentResolvedLimit 是详情页"噪声画像"（ui-enrichment 任务4）
// 的固定回看窗口与最近已解决记录条数，MVP 阶段服务端固定、不接受查询参数——理由
// 同 defaultRecentChangesWindow：按需细分不是本任务范围。
const (
	statsWindow              = 30 * 24 * time.Hour
	statsRecentResolvedLimit = 5
)

// uuidPattern 用于在查询/校验阶段提前拒绝格式非法的 team_id，避免把非法输入一路
// 带到 SQL 的 ::uuid 转换上产生数据库层 500（而不是更合适的 400）。
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// alertsHandler 是告警列表/详情/处置端点的实现集合（R4：处置入口；R7.5：详情页
// "最近变更"；NFR-可用性：列表过滤/分页）。escalation/lifecycle 可为 nil——语义与
// notify.LifecycleHandlers.escalation 可为 nil 一致：对应的定时器取消动作会被跳过，
// 供尚未完成升级链/生命周期定时器装配的部署过渡使用，不应作为长期状态。
// changeEvents 同样可为 nil，此时详情响应的 recent_changes 恒为空列表。
type alertsHandler struct {
	router *Router

	alerts       AlertsStore
	timeline     AlertTimelineStore
	changeEvents ChangeEventLister
	escalation   EscalationStopper
	lifecycle    LifecycleTimerCanceler
}

// RegisterAlertRoutes 挂载告警查询/处置端点到 /api/v1（R4/R7.5/NFR-可用性）。
// 独立于 NewRouter/mount() 之外——可以在 Router 构造完成后随时调用，因为这里依赖
// 的具体仓储/升级链/生命周期定时器的装配顺序由 main 决定，未必先于 Router 本身
// 就绪。所有端点都要求登录（session 保护），不要求 admin：R8.2 里"member 可查看
// 与处置告警"，处置动作是一线值班动作，只有团队/路由/值班/集成等配置管理端点才
// 要求 admin（见 requireAdmin 的既有用法）。
func (r *Router) RegisterAlertRoutes(alerts AlertsStore, timeline AlertTimelineStore, changeEvents ChangeEventLister, escalation EscalationStopper, lifecycle LifecycleTimerCanceler) *Router {
	h := &alertsHandler{
		router:       r,
		alerts:       alerts,
		timeline:     timeline,
		changeEvents: changeEvents,
		escalation:   escalation,
		lifecycle:    lifecycle,
	}

	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/alerts", h.list)
	authed.GET("/alerts/:id", h.detail)
	authed.GET("/alerts/:id/stats", h.stats)
	authed.POST("/alerts/:id/ack", h.ack)
	authed.POST("/alerts/:id/resolve", h.resolve)
	authed.POST("/alerts/:id/handoff", h.handoff)
	authed.POST("/alerts/:id/notes", h.addNote)
	return r
}

// list 处理 GET /api/v1/alerts：severity/status/team_id/label[k]=v 组合过滤
// （AND 语义，标签用 gin 的 QueryMap 解析 `label[env]=prod&label[region]=us` 这种
// 形式）+ limit/offset 分页。响应的 limit 字段回显本次实际生效的分页大小（未指定
// 时是 store.DefaultAlertPageLimit），供前端渲染分页控件。
func (h *alertsHandler) list(c *gin.Context) {
	var filter store.AlertFilter

	if v := c.Query("severity"); v != "" {
		sev := domain.Severity(v)
		if !sev.IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid severity"})
			return
		}
		filter.Severity = sev
	}
	if v := c.Query("status"); v != "" {
		st := domain.Status(v)
		if !st.IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		filter.Status = st
	}
	if v := c.Query("source_state"); v != "" {
		state := domain.SourceState(v)
		if !state.IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_state"})
			return
		}
		filter.SourceState = state
	}
	if v := c.Query("response_state"); v != "" {
		state := domain.ResponseState(v)
		if !state.IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid response_state"})
			return
		}
		filter.ResponseState = state
	}
	if v := c.Query("suppression_state"); v != "" {
		state := domain.SuppressionState(v)
		if !state.IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid suppression_state"})
			return
		}
		filter.SuppressionState = state
	}
	if v := c.Query("source_id"); v != "" {
		if !uuidPattern.MatchString(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
			return
		}
		filter.SourceID = v
	}
	if v := c.Query("team_id"); v != "" {
		if !uuidPattern.MatchString(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid team_id"})
			return
		}
		filter.TeamID = v
	}
	if labels := c.QueryMap("label"); len(labels) > 0 {
		filter.Labels = labels
	}

	limit, ok := parseNonNegativeQueryInt(c, "limit")
	if !ok {
		return
	}
	offset, ok := parseNonNegativeQueryInt(c, "offset")
	if !ok {
		return
	}
	filter.Limit = limit
	filter.Offset = offset

	ctx := c.Request.Context()
	var page store.AlertPage
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		p, listErr := h.alerts.List(ctx, tx, filter)
		if listErr != nil {
			return listErr
		}
		page = p
		return nil
	})
	if err != nil {
		h.router.logger.Error("api: 查询告警列表失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	effectiveLimit := filter.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = store.DefaultAlertPageLimit
	}

	resp := make([]alertResponse, len(page.Alerts))
	for i, a := range page.Alerts {
		resp[i] = newAlertResponse(a)
	}
	c.JSON(http.StatusOK, gin.H{
		"alerts": resp,
		"total":  page.Total,
		"limit":  effectiveLimit,
		"offset": filter.Offset,
	})
}

// parseNonNegativeQueryInt 解析形如 ?key=N 的非负整数查询参数；key 未提供时返回
// (0, true)（表示"不设置该过滤/分页维度"）。解析失败或为负数时直接写 400 响应并
// 返回 ok=false，调用方据此提前 return。
func parseNonNegativeQueryInt(c *gin.Context, key string) (int, bool) {
	v := c.Query(key)
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + key})
		return 0, false
	}
	return n, true
}

// detail 处理 GET /api/v1/alerts/:id：告警全字段 + 完整时间线 + 最近变更（R7.5）。
// 最近变更按告警当前 ResourceRef 关联查询；ResourceRef 为空时不查询（JSONB 包含
// 匹配 `@> '{}'` 会命中全表，查了反而是错的），recent_changes 直接返回空列表。
func (h *alertsHandler) detail(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var (
		alert   domain.Alert
		entries []domain.TimelineEntry
		changes []store.ChangeEvent
	)
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		a, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}
		alert = a

		es, listErr := h.timeline.ListByAlert(ctx, tx, id)
		if listErr != nil {
			return listErr
		}
		entries = es

		if h.changeEvents != nil && len(alert.ResourceRef) > 0 {
			since := h.router.clock.Now().Add(-defaultRecentChangesWindow)
			cs, changesErr := h.changeEvents.ListRecentByResource(ctx, tx, alert.ResourceRef, since, defaultRecentChangesLimit)
			if changesErr != nil {
				return changesErr
			}
			changes = cs
		}
		return nil
	})
	if err != nil {
		h.respondActionError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"alert":          newAlertResponse(alert),
		"timeline":       newTimelineResponses(entries),
		"recent_changes": newChangeEventResponses(changes),
	})
}

// stats 处理 GET /api/v1/alerts/:id/stats（ui-enrichment 任务4）：详情页"噪声画像"，
// 统计同 fingerprint 近 statsWindow 内的历史发生次数/已解决样本平均耗时，并列出最近
// 若干条已解决记录（各自带上处置说明/原因）供排查参考"上次是怎么处理的"。
// first_seen=true 表示这是该 fingerprint 迄今唯一一次出现（count<=1），提示前端不必
// 展示"历史"相关 UI。avg_duration_seconds 无已解决样本时序列化为 JSON null（而不是
// 0），区分"没有数据"与"平均耗时为 0"。
func (h *alertsHandler) stats(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	since := h.router.clock.Now().Add(-statsWindow)

	var (
		count      int
		avgSeconds *float64
		recent     []store.ResolvedRef
	)
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		alert, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}

		cnt, avg, statsErr := h.alerts.FingerprintStats(ctx, tx, alert.Fingerprint, since)
		if statsErr != nil {
			return statsErr
		}
		count = cnt
		avgSeconds = avg

		refs, listErr := h.alerts.ListRecentResolvedByFingerprint(ctx, tx, alert.Fingerprint, alert.ID, statsRecentResolvedLimit)
		if listErr != nil {
			return listErr
		}
		recent = refs
		return nil
	})
	if err != nil {
		h.respondActionError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"window_days":          int(statsWindow / (24 * time.Hour)),
		"count":                count,
		"first_seen":           count <= 1,
		"avg_duration_seconds": roundSecondsPtr(avgSeconds),
		"recent_resolved":      newResolvedRefResponses(recent),
	})
}

// addNote 处理 POST /api/v1/alerts/:id/notes（ui-enrichment 任务2）：详情页追加一条
// 独立备注（不代表任何状态转移，见 domain.AddNote）。空备注或超过 1000 字符
// （domain.ErrInvalidNote）-> 422，与 resolve 端点对超长 note 的映射口径一致。
func (h *alertsHandler) addNote(c *gin.Context) {
	var req struct {
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}

	id := c.Param("id")
	rbac, _ := UserFromContext(c)
	ctx := c.Request.Context()
	now := h.router.clock.Now()

	var entry domain.TimelineEntry
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		alert, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}
		e, noteErr := domain.AddNote(alert.ID, rbac.Username, req.Note, now)
		if noteErr != nil {
			return noteErr
		}
		if _, appendErr := h.timeline.Append(ctx, tx, e); appendErr != nil {
			return appendErr
		}
		entry = e
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNote) {
			unprocessable(c, "note 不能为空，且不超过 1000 字符")
			return
		}
		h.respondActionError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"entry": newTimelineResponses([]domain.TimelineEntry{entry})[0]})
}

// roundSecondsPtr 把 avg(EXTRACT(EPOCH FROM ...)) 算出的浮点秒数四舍五入为整数秒，
// 供 JSON 响应展示；nil 原样透传（"无样本"与"耗时为 0"是两码事，不能用 0 顶替）。
func roundSecondsPtr(v *float64) *int64 {
	if v == nil {
		return nil
	}
	r := int64(math.Round(*v))
	return &r
}

// ack 处理 POST /api/v1/alerts/:id/ack（R4.1/R4.2）。幂等口径（team-lead 裁决）：
// 告警已经是 acknowledged 或 resolved 时视为"重复同一动作"，不调用 domain.Ack、不
// 追加新的时间线条目，直接 200 返回当前状态，不报 409。无论本次是否真的发生了
// 状态转移，都无条件尝试停止升级链定时器（EscalationStopper.Stop 本身幂等）——
// 保证"Ack 之后一定没有残留的升级定时器"这个不变量，不依赖"第一次 Ack 时是否已经
// 停过"的历史。
func (h *alertsHandler) ack(c *gin.Context) {
	id := c.Param("id")
	rbac, _ := UserFromContext(c)
	ctx := c.Request.Context()
	now := h.router.clock.Now()

	var result domain.Alert
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		alert, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}

		if alert.Status == domain.StatusFiring {
			updated, entry, ackErr := domain.Ack(alert, rbac.UserID, rbac.Username, now)
			if ackErr != nil {
				return ackErr
			}
			if err := h.alerts.UpdateStatus(ctx, tx, updated); err != nil {
				return err
			}
			if _, err := h.timeline.Append(ctx, tx, entry); err != nil {
				return err
			}
			alert = updated
		}

		if h.escalation != nil {
			if err := h.escalation.Stop(ctx, tx, alert.ID); err != nil {
				return err
			}
		}
		result = alert
		return nil
	})
	if err != nil {
		h.respondActionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"alert": newAlertResponse(result)})
}

// resolve 处理 POST /api/v1/alerts/:id/resolve（R4.1，reason=manual；R5 可选 note）。
// note 为可选处置说明，body 可以整体省略（走既有的无 body 调用方式）；提供时超过
// maxResolveNoteLength 字符 -> 422（复用 config.go 的 unprocessable 语义校验口径）。
// 幂等口径同 ack：已经 resolved 视为重复同一动作，直接 200 返回当前状态，不报 409、
// 不追加新时间线——本次传入的 note 也随之丢弃（不重写时间线，不变量 6 一字不改）。
// 无论本次是否真的发生了状态转移，都无条件尝试停止升级链定时器与 TTL/repeat 等
// 生命周期定时器（两者均幂等）——保证"Resolve 之后一定没有残留定时器"这个不变量。
func (h *alertsHandler) resolve(c *gin.Context) {
	var req struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
			return
		}
	}
	if utf8.RuneCountInString(req.Note) > maxResolveNoteLength {
		unprocessable(c, fmt.Sprintf("note 超出长度上限（%d 字符）", maxResolveNoteLength))
		return
	}

	id := c.Param("id")
	rbac, _ := UserFromContext(c)
	ctx := c.Request.Context()
	now := h.router.clock.Now()

	var result domain.Alert
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		alert, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}

		if alert.Status != domain.StatusResolved {
			updated, entry, resolveErr := domain.Resolve(alert, rbac.Username, domain.ResolveReasonManual, req.Note, now)
			if resolveErr != nil {
				return resolveErr
			}
			if err := h.alerts.UpdateStatus(ctx, tx, updated); err != nil {
				return err
			}
			if _, err := h.timeline.Append(ctx, tx, entry); err != nil {
				return err
			}
			alert = updated
		}

		if h.escalation != nil {
			if err := h.escalation.Stop(ctx, tx, alert.ID); err != nil {
				return err
			}
		}
		if h.lifecycle != nil {
			if err := h.lifecycle.CancelLifecycleTimers(ctx, tx, alert.ID); err != nil {
				return err
			}
		}
		result = alert
		return nil
	})
	if err != nil {
		h.respondActionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"alert": newAlertResponse(result)})
}

// handoff 处理 POST /api/v1/alerts/:id/handoff（R4.5）：body {"target": "..."}，把
// 责任人（AckedBy）转移给 target，不改变 Status。与 ack/resolve 不同，handoff 没有
// "重复同一动作"的幂等豁免——对已 resolved 的告警执行 handoff 是真正的非法操作
// （domain.Handoff 返回 ErrAlertResolved），按 409 处理（见 respondActionError）。
func (h *alertsHandler) handoff(c *gin.Context) {
	var req struct {
		Target string `json:"target"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target 不能为空"})
		return
	}

	id := c.Param("id")
	rbac, _ := UserFromContext(c)
	ctx := c.Request.Context()
	now := h.router.clock.Now()

	var result domain.Alert
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		alert, getErr := h.alerts.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}
		updated, entry, handoffErr := domain.Handoff(alert, rbac.Username, req.Target, now)
		if handoffErr != nil {
			return handoffErr
		}
		if err := h.alerts.UpdateStatus(ctx, tx, updated); err != nil {
			return err
		}
		if _, err := h.timeline.Append(ctx, tx, entry); err != nil {
			return err
		}
		result = updated
		return nil
	})
	if err != nil {
		h.respondActionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"alert": newAlertResponse(result)})
}

// respondActionError 把 GetByID/处置动作里可能出现的错误翻译成 HTTP 响应：告警不
// 存在 -> 404；domain 状态机报出的、不属于"重复同一动作"的非法转移
// （ErrAlertResolved/ErrInvalidTransition，如对已 resolved 的告警执行 handoff）->
// 409（表示与资源当前状态冲突，而不是请求本身格式有误，故未选用 400/422）；其余
// 一律 500 并记录日志（不回显内部错误细节）。
func (h *alertsHandler) respondActionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrAlertNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "告警不存在"})
	case errors.Is(err, domain.ErrAlertResolved), errors.Is(err, domain.ErrInvalidTransition):
		c.JSON(http.StatusConflict, gin.H{"error": "告警当前状态不允许该操作"})
	case errors.Is(err, domain.ErrAuthoritativeSourceManualResolve):
		c.JSON(http.StatusConflict, gin.H{"error": "Alertmanager 是该 Alert 的生命周期权威，请在上游恢复规则条件；本地仅支持认领、移交和备注"})
	default:
		h.router.logger.Error("api: 告警处置操作失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// ---- 响应 DTO：与 domain/store 的内部类型解耦，字段名统一 snake_case ----

// alertResponse 是告警对外的 JSON 表示。AckedAt/EndsAt 用指针，未设置时序列化为
// null 而不是 "0001-01-01T00:00:00Z"（domain 层用零值 time.Time 表示"未设置"）。
type alertResponse struct {
	ID                     string                  `json:"id"`
	Fingerprint            string                  `json:"fingerprint"`
	UpstreamFingerprint    string                  `json:"upstream_fingerprint,omitempty"`
	Status                 domain.Status           `json:"status"`
	SourceState            domain.SourceState      `json:"source_state"`
	ResponseState          domain.ResponseState    `json:"response_state"`
	SuppressionState       domain.SuppressionState `json:"suppression_state"`
	SuppressionDetails     map[string][]string     `json:"suppression_details,omitempty"`
	Authoritative          bool                    `json:"authoritative"`
	Severity               domain.Severity         `json:"severity"`
	SeverityFallback       bool                    `json:"severity_fallback"`
	Title                  string                  `json:"title"`
	Source                 string                  `json:"source"`
	Labels                 map[string]string       `json:"labels"`
	ResourceRef            map[string]string       `json:"resource_ref"`
	SignalContext          json.RawMessage         `json:"signal_context,omitempty"`
	ResolvePolicy          domain.ResolvePolicy    `json:"resolve_policy"`
	TTLSeconds             int                     `json:"ttl_seconds,omitempty"`
	SourceIntegrationID    string                  `json:"source_integration_id,omitempty"`
	SourceUpdatedAt        *time.Time              `json:"source_updated_at,omitempty"`
	LastReconciledAt       *time.Time              `json:"last_reconciled_at,omitempty"`
	ReconcileMissCount     int                     `json:"reconcile_miss_count"`
	SourceResolutionReason string                  `json:"source_resolution_reason,omitempty"`
	DedupCount             int                     `json:"dedup_count"`
	GroupKey               string                  `json:"group_key,omitempty"`
	TeamID                 string                  `json:"team_id,omitempty"`
	SilencedBy             []string                `json:"silenced_by,omitempty"`
	AckedBy                string                  `json:"acked_by,omitempty"`
	AckedAt                *time.Time              `json:"acked_at,omitempty"`
	Flapping               bool                    `json:"flapping"`
	StartsAt               time.Time               `json:"starts_at"`
	LastSeenAt             time.Time               `json:"last_seen_at"`
	EndsAt                 *time.Time              `json:"ends_at,omitempty"`
	CreatedAt              time.Time               `json:"created_at"`
	UpdatedAt              time.Time               `json:"updated_at"`
}

func newAlertResponse(a domain.Alert) alertResponse {
	resp := alertResponse{
		ID:                     a.ID,
		Fingerprint:            a.Fingerprint,
		UpstreamFingerprint:    a.UpstreamFingerprint,
		Status:                 a.Status,
		SourceState:            a.SourceState,
		ResponseState:          a.ResponseState,
		SuppressionState:       a.SuppressionState,
		SuppressionDetails:     a.SuppressionDetails,
		Authoritative:          a.IsAuthoritativeSource(),
		Severity:               a.Severity,
		SeverityFallback:       a.SeverityFallback,
		Title:                  a.Title,
		Source:                 a.Source,
		Labels:                 a.Labels,
		ResourceRef:            a.ResourceRef,
		SignalContext:          a.SignalContext,
		ResolvePolicy:          a.ResolvePolicy,
		TTLSeconds:             a.TTLSeconds,
		SourceIntegrationID:    a.SourceIntegrationID,
		ReconcileMissCount:     a.ReconcileMissCount,
		SourceResolutionReason: a.SourceResolutionReason,
		DedupCount:             a.DedupCount,
		GroupKey:               a.GroupKey,
		TeamID:                 a.TeamID,
		SilencedBy:             a.SilencedBy,
		AckedBy:                a.AckedBy,
		Flapping:               a.Flapping,
		StartsAt:               a.StartsAt,
		LastSeenAt:             a.LastSeenAt,
		CreatedAt:              a.CreatedAt,
		UpdatedAt:              a.UpdatedAt,
	}
	if !a.AckedAt.IsZero() {
		t := a.AckedAt
		resp.AckedAt = &t
	}
	if !a.EndsAt.IsZero() {
		t := a.EndsAt
		resp.EndsAt = &t
	}
	if !a.SourceUpdatedAt.IsZero() {
		t := a.SourceUpdatedAt
		resp.SourceUpdatedAt = &t
	}
	if !a.LastReconciledAt.IsZero() {
		t := a.LastReconciledAt
		resp.LastReconciledAt = &t
	}
	return resp
}

// timelineEntryResponse 省略 alert_id：详情响应里的时间线已经嵌在某个具体告警下，
// 重复该字段没有信息量。
type timelineEntryResponse struct {
	Actor  string                `json:"actor"`
	Action domain.TimelineAction `json:"action"`
	Detail map[string]any        `json:"detail,omitempty"`
	At     time.Time             `json:"at"`
}

func newTimelineResponses(entries []domain.TimelineEntry) []timelineEntryResponse {
	resp := make([]timelineEntryResponse, len(entries))
	for i, e := range entries {
		resp[i] = timelineEntryResponse{Actor: e.Actor, Action: e.Action, Detail: e.Detail, At: e.At}
	}
	return resp
}

// resolvedRefResponse 是 stats 端点"最近已解决"卡片的对外 JSON 表示，对应
// store.ResolvedRef 投影。duration_seconds 由 EndsAt-StartsAt 取整秒计算。
type resolvedRefResponse struct {
	AlertID         string    `json:"alert_id"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	DurationSeconds int64     `json:"duration_seconds"`
	Note            string    `json:"note"`
	Reason          string    `json:"reason"`
}

func newResolvedRefResponses(refs []store.ResolvedRef) []resolvedRefResponse {
	resp := make([]resolvedRefResponse, len(refs))
	for i, r := range refs {
		resp[i] = resolvedRefResponse{
			AlertID:         r.AlertID,
			StartsAt:        r.StartsAt,
			EndsAt:          r.EndsAt,
			DurationSeconds: int64(math.Round(r.EndsAt.Sub(r.StartsAt).Seconds())),
			Note:            r.Note,
			Reason:          r.Reason,
		}
	}
	return resp
}

type changeEventResponse struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	ResourceRef map[string]string `json:"resource_ref"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

func newChangeEventResponses(events []store.ChangeEvent) []changeEventResponse {
	resp := make([]changeEventResponse, len(events))
	for i, e := range events {
		resp[i] = changeEventResponse{ID: e.ID, Kind: e.Kind, ResourceRef: e.ResourceRef, Payload: e.Payload, OccurredAt: e.OccurredAt}
	}
	return resp
}
