package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/notify"
	"alert-system/internal/store"
)

type incidentService interface {
	Declare(ctx context.Context, command incidentservice.DeclareCommand, actor incidentservice.Actor) (domain.Incident, bool, error)
	List(ctx context.Context, status domain.IncidentStatus, limit int) ([]domain.Incident, error)
	Detail(ctx context.Context, id string) (domain.IncidentDetail, error)
	LinkFault(ctx context.Context, incidentID, faultID string, actor incidentservice.Actor) (domain.IncidentFaultLink, bool, error)
	SetPrimaryFault(ctx context.Context, incidentID, faultID string, actor incidentservice.Actor) (domain.IncidentFaultLink, error)
	SetFaultDisposition(ctx context.Context, incidentID, faultID string, disposition domain.IncidentFaultDisposition, rationale string, actor incidentservice.Actor) (domain.IncidentFaultLink, error)
	TransitionLinkedFault(ctx context.Context, incidentID, faultID string, to domain.FaultStatus, rationale string, actor incidentservice.Actor) (domain.Fault, error)
	AssignRole(ctx context.Context, incidentID string, role domain.IncidentRole, userID string, actor incidentservice.Actor) (domain.IncidentRoleAssignment, error)
	CreateTask(ctx context.Context, incidentID string, command incidentservice.CreateTaskCommand, actor incidentservice.Actor) (domain.IncidentTask, error)
	UpdateTask(ctx context.Context, incidentID, taskID string, status domain.IncidentTaskStatus, ownerUserID string, actor incidentservice.Actor) (domain.IncidentTask, error)
	AddHumanTimeline(ctx context.Context, incidentID string, kind domain.IncidentTimelineKind, summary string, detail map[string]any, occurredAt time.Time, actor incidentservice.Actor) (domain.IncidentTimelineEntry, error)
	UpdateRecoveryCriterion(ctx context.Context, incidentID, key string, status domain.RecoveryCriterionStatus, evidence map[string]any, observedAt time.Time, actor incidentservice.Actor) (domain.RecoveryCriterion, error)
	Transition(ctx context.Context, incidentID string, to domain.IncidentStatus, actor incidentservice.Actor) (domain.Incident, error)
}

type incidentHandlers struct {
	router  *Router
	service incidentService
}

func incidentForResponse(incident domain.Incident) domain.Incident {
	incident.CommunicationTargetRef = notify.MaskTargetForAudit(
		incident.CommunicationChannel, incident.CommunicationTargetRef,
	)
	return incident
}

// RegisterIncidentRoutes 挂载事故协作平面。所有写操作都要求登录，并由 service
// 强制携带当前用户 Actor；自动关联器没有绕开这一入口直接创建 Incident 的能力。
func (r *Router) RegisterIncidentRoutes(service incidentService) *Router {
	h := &incidentHandlers{router: r, service: service}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/incidents", h.list)
	authed.GET("/incidents/:id", h.detail)
	authed.POST("/incidents", h.declare)
	authed.POST("/incidents/:id/faults", h.linkFault)
	authed.PUT("/incidents/:id/faults/:fault_id/primary", h.setPrimaryFault)
	authed.PUT("/incidents/:id/faults/:fault_id/disposition", h.setFaultDisposition)
	authed.POST("/incidents/:id/faults/:fault_id/transitions", h.transitionLinkedFault)
	authed.PUT("/incidents/:id/roles/:role", h.assignRole)
	authed.POST("/incidents/:id/tasks", h.createTask)
	authed.PATCH("/incidents/:id/tasks/:task_id", h.updateTask)
	authed.POST("/incidents/:id/timeline", h.addTimeline)
	authed.PUT("/incidents/:id/recovery-criteria/:key", h.updateRecovery)
	authed.POST("/incidents/:id/transitions", h.transition)
	return r
}

func incidentActor(c *gin.Context) incidentservice.Actor {
	rbac, _ := UserFromContext(c)
	return incidentservice.Actor{UserID: rbac.UserID, Display: rbac.Username}
}

func (h *incidentHandlers) handleError(c *gin.Context, err error, operation string) {
	switch {
	case errors.Is(err, store.ErrIncidentNotFound), errors.Is(err, store.ErrIncidentTaskNotFound),
		errors.Is(err, store.ErrIncidentFaultLinkNotFound),
		errors.Is(err, store.ErrFaultNotFound), errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
	case errors.Is(err, incidentservice.ErrInvalidInput):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
	case errors.Is(err, incidentservice.ErrIncidentNotReady),
		errors.Is(err, domain.ErrInvalidIncidentTransition),
		errors.Is(err, domain.ErrRecoveryGateNotSatisfied),
		errors.Is(err, domain.ErrIncidentTasksNotClosed),
		errors.Is(err, domain.ErrIncidentFaultsNotReady),
		errors.Is(err, domain.ErrInvalidFaultTransition),
		errors.Is(err, incidentservice.ErrIncidentClosed),
		errors.Is(err, incidentservice.ErrPrimaryFaultCannotExclude),
		errors.Is(err, incidentservice.ErrIncidentFaultExcluded),
		errors.Is(err, store.ErrIncidentForFaultExists):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		h.router.logger.Error("api: Incident "+operation+"失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

func (h *incidentHandlers) list(c *gin.Context) {
	status := domain.IncidentStatus(c.Query("status"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	items, err := h.service.List(c.Request.Context(), status, limit)
	if err != nil {
		h.handleError(c, err, "列表查询")
		return
	}
	if items == nil {
		items = []domain.Incident{}
	}
	for index := range items {
		items[index] = incidentForResponse(items[index])
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *incidentHandlers) detail(c *gin.Context) {
	detail, err := h.service.Detail(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.handleError(c, err, "详情查询")
		return
	}
	detail.Incident = incidentForResponse(detail.Incident)
	c.JSON(http.StatusOK, detail)
}

func (h *incidentHandlers) declare(c *gin.Context) {
	var request struct {
		FaultID                string          `json:"fault_id"`
		Title                  string          `json:"title"`
		Summary                string          `json:"summary"`
		Severity               domain.Severity `json:"severity"`
		CommunicationChannel   string          `json:"communication_channel"`
		CommunicationTargetRef string          `json:"communication_target_ref"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	createdIncident, created, err := h.service.Declare(c.Request.Context(), incidentservice.DeclareCommand{
		FaultID: request.FaultID, Title: request.Title, Summary: request.Summary,
		Severity: request.Severity, CommunicationChannel: request.CommunicationChannel,
		CommunicationTargetRef: request.CommunicationTargetRef,
	}, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "宣布")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"incident": incidentForResponse(createdIncident), "created": created})
}

func (h *incidentHandlers) linkFault(c *gin.Context) {
	var request struct {
		FaultID string `json:"fault_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	link, created, err := h.service.LinkFault(c.Request.Context(), c.Param("id"), request.FaultID, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "Fault 关联")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"link": link, "created": created})
}

func (h *incidentHandlers) setPrimaryFault(c *gin.Context) {
	link, err := h.service.SetPrimaryFault(c.Request.Context(), c.Param("id"), c.Param("fault_id"), incidentActor(c))
	if err != nil {
		h.handleError(c, err, "Primary Fault 切换")
		return
	}
	c.JSON(http.StatusOK, gin.H{"link": link})
}

func (h *incidentHandlers) setFaultDisposition(c *gin.Context) {
	var request struct {
		Disposition domain.IncidentFaultDisposition `json:"disposition"`
		Rationale   string                          `json:"rationale"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	link, err := h.service.SetFaultDisposition(c.Request.Context(), c.Param("id"), c.Param("fault_id"), request.Disposition, request.Rationale, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "Fault 范围裁决")
		return
	}
	c.JSON(http.StatusOK, gin.H{"link": link})
}

func (h *incidentHandlers) transitionLinkedFault(c *gin.Context) {
	var request struct {
		To        domain.FaultStatus `json:"to"`
		Rationale string             `json:"rationale"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	fault, err := h.service.TransitionLinkedFault(c.Request.Context(), c.Param("id"), c.Param("fault_id"), request.To, request.Rationale, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "关联 Fault 生命周期推进")
		return
	}
	c.JSON(http.StatusOK, gin.H{"fault": fault})
}

func (h *incidentHandlers) assignRole(c *gin.Context) {
	var request struct {
		UserID string `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	assignment, err := h.service.AssignRole(c.Request.Context(), c.Param("id"), domain.IncidentRole(c.Param("role")), request.UserID, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "角色分配")
		return
	}
	c.JSON(http.StatusOK, assignment)
}

func (h *incidentHandlers) createTask(c *gin.Context) {
	var request struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		OwnerUserID string    `json:"owner_user_id"`
		DueAt       time.Time `json:"due_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	task, err := h.service.CreateTask(c.Request.Context(), c.Param("id"), incidentservice.CreateTaskCommand{
		Title: request.Title, Description: request.Description, OwnerUserID: request.OwnerUserID, DueAt: request.DueAt,
	}, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "任务创建")
		return
	}
	c.JSON(http.StatusCreated, task)
}

func (h *incidentHandlers) updateTask(c *gin.Context) {
	var request struct {
		Status      domain.IncidentTaskStatus `json:"status"`
		OwnerUserID string                    `json:"owner_user_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	task, err := h.service.UpdateTask(c.Request.Context(), c.Param("id"), c.Param("task_id"), request.Status, request.OwnerUserID, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "任务更新")
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *incidentHandlers) addTimeline(c *gin.Context) {
	var request struct {
		Kind       domain.IncidentTimelineKind `json:"kind"`
		Summary    string                      `json:"summary"`
		Detail     map[string]any              `json:"detail"`
		OccurredAt time.Time                   `json:"occurred_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	entry, err := h.service.AddHumanTimeline(c.Request.Context(), c.Param("id"), request.Kind, request.Summary, request.Detail, request.OccurredAt, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "时间线追加")
		return
	}
	c.JSON(http.StatusCreated, entry)
}

func (h *incidentHandlers) updateRecovery(c *gin.Context) {
	var request struct {
		Status     domain.RecoveryCriterionStatus `json:"status"`
		Evidence   map[string]any                 `json:"evidence"`
		ObservedAt time.Time                      `json:"observed_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	criterion, err := h.service.UpdateRecoveryCriterion(c.Request.Context(), c.Param("id"), c.Param("key"), request.Status, request.Evidence, request.ObservedAt, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "恢复判据更新")
		return
	}
	c.JSON(http.StatusOK, criterion)
}

func (h *incidentHandlers) transition(c *gin.Context) {
	var request struct {
		To domain.IncidentStatus `json:"to"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	updated, err := h.service.Transition(c.Request.Context(), c.Param("id"), request.To, incidentActor(c))
	if err != nil {
		h.handleError(c, err, "状态推进")
		return
	}
	c.JSON(http.StatusOK, incidentForResponse(updated))
}
