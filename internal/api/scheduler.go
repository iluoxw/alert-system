package api

import (
	"context"
	"net/http"
	"time"

	"alert-system/internal/scheduler"
	"github.com/gin-gonic/gin"
)

type SchedulerQuery func(context.Context) (scheduler.Status, error)

// RegisterSchedulerRoutes 只向管理员暴露后台状态。队列异常不剥夺持久化接入能力。
func (r *Router) RegisterSchedulerRoutes(query SchedulerQuery) *Router {
	admin := r.engine.Group("/api/v1")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/ops/scheduler", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		status, err := query(ctx)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "调度状态暂不可用"})
			return
		}
		if status.Pools == nil {
			status.Pools = []scheduler.PoolStatus{}
		}
		if status.Workers == nil {
			status.Workers = []scheduler.WorkerStatus{}
		}
		if status.Kinds == nil {
			status.Kinds = []scheduler.KindStatus{}
		}
		c.JSON(http.StatusOK, status)
	})
	return r
}

// SchedulerReady 只使用普通池逐 worker 状态；独立通知池不影响接入 readiness。
func SchedulerReady(s interface{ NormalWorkersReady() bool }) func(context.Context) error {
	return func(context.Context) error {
		if !s.NormalWorkersReady() {
			return ErrReadyWorkers
		}
		return nil
	}
}
