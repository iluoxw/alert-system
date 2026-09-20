package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/ops"
)

// IndicatorsQuery 是先行指标查询，生产注入 ops.QueryIndicators；单测注入内存 stub。
type IndicatorsQuery func(ctx context.Context, tx pgx.Tx) (ops.Indicators, error)

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
	var ind ops.Indicators
	err := h.router.tx.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
		var qerr error
		ind, qerr = h.query(c.Request.Context(), tx)
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
		"unresolved_identity_count":         ind.UnresolvedIdentityCount,
	}
}
