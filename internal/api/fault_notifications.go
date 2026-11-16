package api

import (
	"alert-system/internal/app"
	"alert-system/internal/store"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
)

type FaultNotificationService interface {
	Preview(context.Context, pgx.Tx, string, bool) (app.FaultNotificationPreview, error)
	Submit(context.Context, pgx.Tx, string, bool, string, string) (app.FaultNotificationPreview, error)
}

func (r *Router) RegisterFaultNotificationRoutes(service FaultNotificationService) *Router {
	admin := r.engine.Group("/api/v1/admin", r.sessionAuth(), r.requireAdmin())
	handle := func(submit bool) gin.HandlerFunc {
		return func(c *gin.Context) {
			id := c.Param("id")
			if _, e := uuid.Parse(id); e != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Fault ID 格式错误"})
				return
			}
			retry := c.Query("retry_failed") == "true"
			actor, _ := UserFromContext(c)
			var result app.FaultNotificationPreview
			err := r.tx.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
				var e error
				if !submit {
					if _, e = tx.Exec(c.Request.Context(), "SET TRANSACTION READ ONLY"); e != nil {
						return e
					}
				}
				if submit {
					result, e = service.Submit(c.Request.Context(), tx, id, retry, actor.UserID, actor.Username)
				} else {
					result, e = service.Preview(c.Request.Context(), tx, id, retry)
				}
				return e
			})
			if err != nil {
				if errors.Is(err, store.ErrFaultNotFound) {
					c.JSON(404, gin.H{"error": "资源不存在"})
				} else {
					c.JSON(500, gin.H{"error": "内部错误"})
				}
				return
			}
			c.JSON(200, result)
		}
	}
	admin.GET("/faults/:id/notifications", handle(false))
	admin.POST("/faults/:id/notifications/preview", handle(false))
	admin.POST("/faults/:id/notifications/submit", handle(true))
	return r
}
