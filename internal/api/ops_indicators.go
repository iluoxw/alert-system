package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/ops"
)

// IndicatorsQuery 是先行指标查询，生产注入 ops.QueryIndicators；单测注入内存 stub。
type IndicatorsQuery func(ctx context.Context, tx pgx.Tx, window ops.IndicatorWindow) (ops.Indicators, error)

// indicatorsWindow 是本端点给流量型指标用的统计窗口。固定近 24 小时：timers 的
// fired 行会被 retention_sweep 删除，全表计数不可解释（见 ops.IndicatorWindow）。
// 窗口长度随响应一起返回，前端据此渲染文案，不在页面里写死小时数。
const indicatorsWindow = 24 * time.Hour

// RegisterOpsIndicatorsRoutes 挂载 GET /api/v1/ops/indicators（admin only，R10.1）。
// 路径按规格是 /api/v1/ops/indicators，不放在 /api/v1/admin 前缀下；鉴权仍走
// sessionAuth + requireAdmin。
func (r *Router) RegisterOpsIndicatorsRoutes(query IndicatorsQuery) *Router {
	if query == nil {
		query = ops.QueryIndicators
	}
	h := &opsIndicatorsHandlers{router: r, query: query}
	admin := r.engine.Group("/api/v1")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/ops/indicators", h.get)
	return r
}

type opsIndicatorsHandlers struct {
	router *Router
	query  IndicatorsQuery
}

func (h *opsIndicatorsHandlers) get(c *gin.Context) {
	now := time.Now()
	window := ops.IndicatorWindow{From: now.Add(-indicatorsWindow), To: now}
	var ind ops.Indicators
	err := h.router.tx.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
		var qerr error
		ind, qerr = h.query(c.Request.Context(), tx, window)
		return qerr
	})
	if err != nil {
		h.router.logger.Error("api: 查询先行指标失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, indicatorsJSON(ind))
}

func indicatorsJSON(ind ops.Indicators) gin.H {
	return gin.H{
		"firing_mapping_rate":               ind.FiringMappingRate,
		"active_faults_without_owner":       ind.ActiveFaultsWithoutOwner,
		"fault_ack_duration_median_seconds": ind.FaultAckMedianSeconds,
		"recovery_nudge_fired_count":        ind.RecoveryNudgeFiredCount,
		"recovery_nudge_window_hours":       int(indicatorsWindow / time.Hour),
		"unresolved_identity_count":         ind.UnresolvedIdentityCount,
	}
}
