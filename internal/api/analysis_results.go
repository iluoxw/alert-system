package api

import (
	"alert-system/internal/omaruntime"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
	"time"
)

type AnalysisResultsStore interface {
	List(context.Context, pgx.Tx, omaruntime.Filter) ([]omaruntime.Run, error)
	Result(context.Context, pgx.Tx, string) (omaruntime.Result, error)
}

// RegisterAnalysisResultRoutes is read-only and admin-only; no report can mutate Fault/Impact/Incident state.
func (r *Router) RegisterAnalysisResultRoutes(repo AnalysisResultsStore) *Router {
	admin := r.engine.Group("/api/v1/admin", r.sessionAuth(), r.requireAdmin())
	admin.GET("/analysis-runs", func(c *gin.Context) {
		filter := omaruntime.Filter{MessageID: c.Query("message_id"), AlertID: c.Query("alert_id"), FaultID: c.Query("fault_id"), SourceID: c.Query("source_id"), Role: c.Query("role"), Limit: 50}
		for _, id := range []string{filter.MessageID, filter.AlertID, filter.FaultID, filter.SourceID} {
			if id != "" {
				if _, e := uuid.Parse(id); e != nil {
					c.JSON(400, gin.H{"error": "关联ID格式错误"})
					return
				}
			}
		}
		if c.Query("limit") != "" {
			n, e := strconv.Atoi(c.Query("limit"))
			if e != nil || n < 1 || n > 100 {
				c.JSON(400, gin.H{"error": "limit必须在1至100之间"})
				return
			}
			filter.Limit = n
		}
		if len(filter.Role) > 64 {
			c.JSON(400, gin.H{"error": "role超出长度限制"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		items := []omaruntime.Run{}
		e := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); e != nil {
				return e
			}
			var e error
			items, e = repo.List(ctx, tx, filter)
			return e
		})
		if e != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "分析记录暂不可用"})
			return
		}
		if items == nil {
			items = []omaruntime.Run{}
		}
		c.JSON(200, gin.H{"items": items})
	})
	admin.GET("/analysis-runs/:id", func(c *gin.Context) {
		id := c.Param("id")
		if _, e := uuid.Parse(id); e != nil {
			c.JSON(400, gin.H{"error": "分析记录ID格式错误"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		var result omaruntime.Result
		e := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); e != nil {
				return e
			}
			var e error
			result, e = repo.Result(ctx, tx, id)
			return e
		})
		if errors.Is(e, omaruntime.ErrNotFound) {
			c.JSON(404, gin.H{"error": "分析记录不存在"})
			return
		}
		if e != nil {
			c.JSON(503, gin.H{"error": "分析结果暂不可用"})
			return
		}
		c.JSON(200, result)
	})
	return r
}
