package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// UnresolvedIdentityStore 是待对齐身份列表端点的仓储子集。
type UnresolvedIdentityStore interface {
	ListUnresolvedIdentities(ctx context.Context, tx pgx.Tx, limit, offset int) ([]store.UnresolvedIdentity, error)
}

type topologyAdminHandlers struct {
	router   *Router
	topology UnresolvedIdentityStore
}

// RegisterTopologyAdminRoutes 挂载 GET /api/v1/admin/topology/unresolved。
// admin only（R1.4）；空集合返回 []（不变量 13）。
func (r *Router) RegisterTopologyAdminRoutes(topology UnresolvedIdentityStore) *Router {
	h := &topologyAdminHandlers{router: r, topology: topology}
	admin := r.engine.Group("/api/v1/admin")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/topology/unresolved", h.listUnresolved)
	return r
}

func (h *topologyAdminHandlers) listUnresolved(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	ctx := c.Request.Context()
	var items []store.UnresolvedIdentity
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		items, listErr = h.topology.ListUnresolvedIdentities(ctx, tx, limit, offset)
		return listErr
	})
	if err != nil {
		h.router.logger.Error("api: 查询待对齐身份失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	if items == nil {
		items = []store.UnresolvedIdentity{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
