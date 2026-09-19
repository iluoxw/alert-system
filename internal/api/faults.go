package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// FaultAckStore 是 Fault 认领端点用到的仓储子集（R4.3/R4.5）。*store.FaultRepo
// 天然满足。刻意不复用 infrastructureHandlers 里的具体类型，以便单测用内存 fake
// 覆盖首次/重复 ack、404，而不依赖 PostgreSQL。
type FaultAckStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error)
	Ack(ctx context.Context, tx pgx.Tx, id, actorID, actorName string, at time.Time) (domain.Fault, bool, error)
	AppendTimeline(ctx context.Context, tx pgx.Tx, entry domain.FaultTimelineEntry) (string, error)
}

type faultHandlers struct {
	router *Router
	faults FaultAckStore
}

// RegisterFaultRoutes 挂载 Fault 认领端点。登录即可（member 可处置，R4.5），
// 不要求 admin。列表与详情仍由 RegisterInfrastructureRoutes 提供，它们直接
// 序列化 domain.Fault，owner/acked 字段已在 JSON 标签里（R4.7）。
func (r *Router) RegisterFaultRoutes(faults FaultAckStore) *Router {
	h := &faultHandlers{router: r, faults: faults}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.POST("/faults/:id/ack", h.ack)
	return r
}

// ack 处理 POST /api/v1/faults/:id/ack。
//
// 同一事务内配对（任务 15 跨任务警告）：domain.AckFault 产出时间线条目 →
// FaultRepo.Ack 写 faults 表 → 仅当 AckFault 返回 ok=true 且 store.Ack 也
// 写成功时才 AppendTimeline。重复认领按不变量 6 返回 200 + 当前状态，不 409、
// 不重写时间线。错误分支一律不写时间线。
func (h *faultHandlers) ack(c *gin.Context) {
	id := c.Param("id")
	rbac, ok := UserFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
		return
	}
	now := h.router.clock.Now()
	ctx := c.Request.Context()

	var result domain.Fault
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		fault, getErr := h.faults.GetByID(ctx, tx, id)
		if getErr != nil {
			return getErr
		}

		updated, entry, wrote, ackErr := domain.AckFault(fault, rbac.UserID, rbac.Username, now)
		if ackErr != nil {
			return ackErr
		}
		if !wrote {
			// 已认领：短路返回当前状态，不调 store.Ack、不写时间线。
			result = fault
			return nil
		}

		stored, storedWrote, storeErr := h.faults.Ack(ctx, tx, updated.ID, rbac.UserID, rbac.Username, now)
		if storeErr != nil {
			return storeErr
		}
		if storedWrote {
			if _, err := h.faults.AppendTimeline(ctx, tx, entry); err != nil {
				return err
			}
		}
		result = stored
		return nil
	})
	if err != nil {
		h.respondAckError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"fault": result})
}

func (h *faultHandlers) respondAckError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrFaultNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
	case strings.Contains(err.Error(), "actorName 不能为空"):
		c.JSON(http.StatusBadRequest, gin.H{"error": "actorName 不能为空"})
	default:
		h.router.logger.Error("api: Fault 认领失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
