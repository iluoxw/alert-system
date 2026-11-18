package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/agentdelivery"
)

type AgentDeliveryDetailsStore interface {
	MessageDetail(context.Context, pgx.Tx, string) (agentdelivery.MessageDetail, error)
}

func (r *Router) RegisterAgentDeliveryDetails(repo AgentDeliveryDetailsStore) {
	admin := r.engine.Group("/api/v1/admin", r.sessionAuth(), r.requireAdmin())
	admin.GET("/agent-deliveries/:id", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "投递消息 ID 格式错误"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		var detail agentdelivery.MessageDetail
		err = r.tx.WithTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
				return err
			}
			var err error
			detail, err = repo.MessageDetail(ctx, tx, id.String())
			return err
		})
		switch {
		case errors.Is(err, agentdelivery.ErrMessageNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "投递记录不存在"})
		case err != nil:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "投递记录暂不可用"})
		default:
			c.JSON(http.StatusOK, detail)
		}
	})
}
