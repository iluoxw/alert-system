package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/alertsource"
	"alert-system/internal/store"
)

type AlertSourceStore interface {
	List(ctx context.Context, tx pgx.Tx, limit int) ([]store.AlertSource, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (store.AlertSource, error)
}

type AlertSourceReconciler interface {
	ReconcileSource(ctx context.Context, sourceID string) (alertsource.ReconcileResult, error)
}

type alertSourceHandlers struct {
	router     *Router
	sources    AlertSourceStore
	reconciler AlertSourceReconciler
}

func (r *Router) RegisterAlertSourceRoutes(sources AlertSourceStore, reconciler AlertSourceReconciler) *Router {
	h := &alertSourceHandlers{router: r, sources: sources, reconciler: reconciler}
	admin := r.Engine().Group("/api/v1/admin", r.sessionAuth(), r.requireAdmin())
	admin.GET("/alert-sources", h.list)
	admin.GET("/alert-sources/:id", h.get)
	admin.POST("/alert-sources/:id/reconcile", h.reconcile)
	return r
}

func (h *alertSourceHandlers) list(c *gin.Context) {
	ctx := c.Request.Context()
	var sources []store.AlertSource
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		sources, err = h.sources.List(ctx, tx, queryLimit(c))
		return err
	})
	if err != nil {
		h.router.logger.Error("api: 查询 Alertmanager sources 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	items := make([]gin.H, 0, len(sources))
	for _, source := range sources {
		items = append(items, alertSourceJSON(source, h.router.clock.Now()))
	}
	c.JSON(http.StatusOK, gin.H{"sources": items})
}

func (h *alertSourceHandlers) get(c *gin.Context) {
	ctx := c.Request.Context()
	var source store.AlertSource
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		source, err = h.sources.GetByID(ctx, tx, c.Param("id"))
		return err
	})
	if err != nil {
		if err == store.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Alertmanager 数据源不存在"})
			return
		}
		h.router.logger.Error("api: 查询 Alertmanager source 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, alertSourceJSON(source, h.router.clock.Now()))
}

func (h *alertSourceHandlers) reconcile(c *gin.Context) {
	if h.reconciler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Alertmanager 对账器未启用"})
		return
	}
	result, err := h.reconciler.ReconcileSource(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.router.logger.Warn("api: 手工触发 Alertmanager 对账失败", "source_id", c.Param("id"), "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 对账失败，请检查数据源健康详情与连接配置"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"source_id": result.SourceID, "snapshot_version": result.SnapshotVersion,
		"seen": result.Seen, "unknown": result.Unknown, "resolved": result.Resolved,
		"recovered": result.Recovered, "completed_at": result.CompletedAt,
	})
}

func alertSourceJSON(source store.AlertSource, now time.Time) gin.H {
	config := gin.H{
		"api_url":                    source.Config.APIURL,
		"bearer_token_env":           source.Config.BearerTokenEnv,
		"reconcile_interval_seconds": source.Config.ReconcileIntervalSeconds,
		"stale_after_seconds":        source.Config.StaleAfterSeconds,
		"missing_confirmations":      source.Config.MissingConfirmations,
	}
	return gin.H{
		"id": source.ID, "kind": source.Kind, "name": source.Name, "enabled": source.Enabled,
		"default_severity": source.DefaultSeverity, "config": config,
		"health": source.Health(now), "last_error": source.LastError,
		"consecutive_failures": source.ConsecutiveFailures,
		"snapshot_version":     source.SnapshotVersion,
		"firing_count":         source.FiringCount, "unknown_count": source.UnknownCount,
		"suppressed_count":  source.SuppressedCount,
		"last_webhook_at":   nullableAPITime(source.LastWebhookAt),
		"last_reconcile_at": nullableAPITime(source.LastReconcileAt),
		"last_success_at":   nullableAPITime(source.LastSuccessAt),
		"last_error_at":     nullableAPITime(source.LastErrorAt),
		"created_at":        source.CreatedAt, "updated_at": source.UpdatedAt,
	}
}

func nullableAPITime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
