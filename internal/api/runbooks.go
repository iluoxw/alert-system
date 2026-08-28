package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/runbook"
	"alert-system/internal/store"
)

type runbookService interface {
	ListDefinitions(ctx context.Context) ([]domain.RunbookDefinition, error)
	ListRuns(ctx context.Context, incidentID string) ([]domain.RunbookRun, error)
	Execute(ctx context.Context, incidentID, actionKey string, parameters map[string]any, actor incidentservice.Actor) (domain.RunbookRun, error)
}

type runbookHandlers struct {
	router  *Router
	service runbookService
}

func (r *Router) RegisterRunbookRoutes(service runbookService) *Router {
	h := &runbookHandlers{router: r, service: service}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/runbooks", h.listDefinitions)
	authed.GET("/incidents/:id/runbook-runs", h.listRuns)
	authed.POST("/incidents/:id/runbook-runs", h.execute)
	return r
}

func runbookActor(c *gin.Context) incidentservice.Actor {
	rbac, _ := UserFromContext(c)
	return incidentservice.Actor{UserID: rbac.UserID, Display: rbac.Username}
}

func (h *runbookHandlers) handleError(c *gin.Context, err error, operation string) {
	switch {
	case errors.Is(err, store.ErrIncidentNotFound), errors.Is(err, store.ErrRunbookRunNotFound),
		errors.Is(err, store.ErrRunbookNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
	case errors.Is(err, runbook.ErrInvalidParameters):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
	case errors.Is(err, runbook.ErrGuardRejected), errors.Is(err, runbook.ErrActionNotAllowed),
		errors.Is(err, runbook.ErrActionDisabled):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, runbook.ErrExecutorDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	default:
		h.router.logger.Error("api: Runbook "+operation+"失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

func (h *runbookHandlers) listDefinitions(c *gin.Context) {
	items, err := h.service.ListDefinitions(c.Request.Context())
	if err != nil {
		h.handleError(c, err, "列表查询")
		return
	}
	if items == nil {
		items = []domain.RunbookDefinition{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *runbookHandlers) listRuns(c *gin.Context) {
	items, err := h.service.ListRuns(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.handleError(c, err, "执行记录查询")
		return
	}
	if items == nil {
		items = []domain.RunbookRun{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *runbookHandlers) execute(c *gin.Context) {
	var request struct {
		ActionKey  string         `json:"action_key"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	run, err := h.service.Execute(c.Request.Context(), c.Param("id"), request.ActionKey, request.Parameters, runbookActor(c))
	if err != nil {
		h.handleError(c, err, "执行")
		return
	}
	c.JSON(http.StatusCreated, run)
}
