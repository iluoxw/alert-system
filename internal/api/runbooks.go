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
	Approve(ctx context.Context, incidentID, planRunID, planSHA string, decision domain.RunbookApprovalDecision, comment string, actor incidentservice.Actor) (domain.RunbookApproval, error)
	Apply(ctx context.Context, incidentID string, command runbook.ApplyCommand, actor incidentservice.Actor) (domain.RunbookRun, error)
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
	authed.POST("/incidents/:id/runbook-runs/:run_id/approval", h.approve)
	authed.POST("/incidents/:id/runbook-apply", h.apply)
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
		errors.Is(err, runbook.ErrActionDisabled), errors.Is(err, runbook.ErrUATAuthorizationRequired),
		errors.Is(err, store.ErrRunbookApplyInFlight), errors.Is(err, store.ErrRunbookApprovalAbsent):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, runbook.ErrExecutorDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case errors.Is(err, runbook.ErrE2BUATTransport), errors.Is(err, runbook.ErrE2BUATOperation),
		errors.Is(err, runbook.ErrUATCleanupFailed):
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
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

func (h *runbookHandlers) approve(c *gin.Context) {
	var request struct {
		PlanSHA256 string                         `json:"plan_sha256"`
		Decision   domain.RunbookApprovalDecision `json:"decision"`
		Comment    string                         `json:"comment"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	approval, err := h.service.Approve(c.Request.Context(), c.Param("id"), c.Param("run_id"), request.PlanSHA256, request.Decision, request.Comment, runbookActor(c))
	if err != nil {
		h.handleError(c, err, "审批")
		return
	}
	c.JSON(http.StatusOK, approval)
}

func (h *runbookHandlers) apply(c *gin.Context) {
	var request struct {
		PlanRunID         string `json:"plan_run_id"`
		ConfirmPlanSHA256 string `json:"confirm_plan_sha256"`
		RetriesStopped    bool   `json:"retries_stopped"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	run, err := h.service.Apply(c.Request.Context(), c.Param("id"), runbook.ApplyCommand{
		PlanRunID: request.PlanRunID, ConfirmPlanSHA256: request.ConfirmPlanSHA256,
		RetriesStopped: request.RetriesStopped,
	}, runbookActor(c))
	if err != nil {
		h.handleError(c, err, "apply")
		return
	}
	c.JSON(http.StatusCreated, run)
}
