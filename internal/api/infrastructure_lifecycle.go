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
	"alert-system/internal/impact"
	"alert-system/internal/store"
)

// EntityLifecycleStore 是生命周期端点用到的仓储子集。*entityLifecycleAdapter
// 与测试 fake 都满足，避免 HTTP 测试依赖 PostgreSQL（对齐 Fault ack 的拆法）。
type EntityLifecycleStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.ResourceNode, error)
	SetEntityLifecycle(ctx context.Context, tx pgx.Tx, id, actorID, reason string, state domain.EntityLifecycleState, at time.Time) (domain.ResourceNode, bool, error)
	HasActiveRelatedFaults(ctx context.Context, tx pgx.Tx, nodeID string) (bool, error)
	Preview(ctx context.Context, tx pgx.Tx, nodeID string) (store.EntityLifecyclePreview, error)
	CascadeDecommission(ctx context.Context, tx pgx.Tx, node domain.ResourceNode, actorID, actorName, reason string, at time.Time) ([]domain.Fault, error)
	NoteRestore(ctx context.Context, tx pgx.Tx, node domain.ResourceNode, actorID, actorName string, at time.Time) error
	RebuildImpacts(ctx context.Context, tx pgx.Tx, node domain.ResourceNode) error
}

var errEntityHasActiveFaults = errors.New("entity has active faults; set cascade_faults=true")

type entityLifecycleHandlers struct {
	router *Router
	store  EntityLifecycleStore
}

type entityLifecycleAdapter struct {
	topology  *store.TopologyRepo
	faults    *store.FaultRepo
	incidents *store.IncidentRepo
	alerts    *store.AlertRepo
	impact    *impact.Projector
}

func newEntityLifecycleAdapter(topology *store.TopologyRepo, faults *store.FaultRepo, incidents *store.IncidentRepo, alerts *store.AlertRepo) *entityLifecycleAdapter {
	return &entityLifecycleAdapter{
		topology: topology, faults: faults, incidents: incidents, alerts: alerts,
		impact: impact.NewProjector(topology, faults),
	}
}

func (a *entityLifecycleAdapter) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.ResourceNode, error) {
	return a.topology.GetByID(ctx, tx, id)
}

func (a *entityLifecycleAdapter) SetEntityLifecycle(ctx context.Context, tx pgx.Tx, id, actorID, reason string, state domain.EntityLifecycleState, at time.Time) (domain.ResourceNode, bool, error) {
	return a.topology.SetEntityLifecycle(ctx, tx, id, actorID, reason, state, at)
}

func (a *entityLifecycleAdapter) HasActiveRelatedFaults(ctx context.Context, tx pgx.Tx, nodeID string) (bool, error) {
	return a.faults.HasActiveRelatedToNode(ctx, tx, nodeID)
}

func (a *entityLifecycleAdapter) Preview(ctx context.Context, tx pgx.Tx, nodeID string) (store.EntityLifecyclePreview, error) {
	return store.PreviewEntityLifecycle(ctx, tx, a.faults, a.incidents, a.alerts, nodeID)
}

func (a *entityLifecycleAdapter) CascadeDecommission(ctx context.Context, tx pgx.Tx, node domain.ResourceNode, actorID, actorName, reason string, at time.Time) ([]domain.Fault, error) {
	return store.CascadeFaultsForDecommission(ctx, tx, a.faults, a.incidents, node, actorID, actorName, reason, at)
}

func (a *entityLifecycleAdapter) NoteRestore(ctx context.Context, tx pgx.Tx, node domain.ResourceNode, actorID, actorName string, at time.Time) error {
	return store.NoteEntityRestore(ctx, tx, a.faults, a.incidents, node, actorID, actorName, at)
}

func (a *entityLifecycleAdapter) RebuildImpacts(ctx context.Context, tx pgx.Tx, node domain.ResourceNode) error {
	if node.LifecycleState == domain.EntityLifecycleDecommissioned {
		return a.impact.RuleOutDecommissionedImpacts(ctx, tx, node)
	}
	return a.impact.RebuildRelatedToNode(ctx, tx, node.ID)
}

// RegisterEntityLifecycleRoutes 挂载 admin 生命周期端点。生产路径由
// RegisterInfrastructureRoutes 接入真实 adapter；单测只注册本函数。
func (r *Router) RegisterEntityLifecycleRoutes(store EntityLifecycleStore) *Router {
	h := &entityLifecycleHandlers{router: r, store: store}
	admin := r.engine.Group("/api/v1/admin")
	admin.Use(r.sessionAuth(), r.requireAdmin())
	admin.GET("/entities/:id/lifecycle-preview", h.preview)
	admin.PATCH("/entities/:id/lifecycle", h.patch)
	return r
}

func (h *entityLifecycleHandlers) handleError(c *gin.Context, err error, operation string) {
	if errors.Is(err, errEntityHasActiveFaults) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, store.ErrResourceNodeNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}
	if errors.Is(err, store.ErrEntityLifecycleReasonRequired) ||
		errors.Is(err, store.ErrEntityLifecycleInvalid) ||
		errors.Is(err, store.ErrEntityLifecycleActorRequired) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	h.router.logger.Error("api: 实体生命周期 "+operation+"失败", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

func (h *entityLifecycleHandlers) preview(c *gin.Context) {
	ctx := c.Request.Context()
	var preview store.EntityLifecyclePreview
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.store.GetByID(ctx, tx, c.Param("id")); err != nil {
			return err
		}
		var err error
		preview, err = h.store.Preview(ctx, tx, c.Param("id"))
		return err
	})
	if err != nil {
		h.handleError(c, err, "前置检查")
		return
	}
	faults := preview.ActiveFaults
	if faults == nil {
		faults = []domain.Fault{}
	}
	incidents := make([]domain.Incident, 0, len(preview.OpenIncidents))
	for _, incident := range preview.OpenIncidents {
		incidents = append(incidents, incidentForResponse(incident))
	}
	alerts := make([]alertResponse, 0, len(preview.FiringAlerts))
	for _, alert := range preview.FiringAlerts {
		alerts = append(alerts, newAlertResponse(alert))
	}
	c.JSON(http.StatusOK, gin.H{
		"active_faults":  faults,
		"open_incidents": incidents,
		"firing_alerts":  alerts,
	})
}

type entityLifecycleRequest struct {
	State         domain.EntityLifecycleState `json:"state"`
	Reason        string                      `json:"reason"`
	CascadeFaults bool                        `json:"cascade_faults"`
}

func (h *entityLifecycleHandlers) patch(c *gin.Context) {
	var request entityLifecycleRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if !request.State.IsValid() {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid lifecycle"})
		return
	}
	if request.State == domain.EntityLifecycleDecommissioned && request.Reason == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": store.ErrEntityLifecycleReasonRequired.Error()})
		return
	}
	actor, ok := UserFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
		return
	}
	now := h.router.clock.Now()
	ctx := c.Request.Context()
	var entity domain.ResourceNode
	cascaded := []domain.Fault{}
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.store.GetByID(ctx, tx, c.Param("id")); err != nil {
			return err
		}
		if request.State == domain.EntityLifecycleDecommissioned && !request.CascadeFaults {
			hasActive, err := h.store.HasActiveRelatedFaults(ctx, tx, c.Param("id"))
			if err != nil {
				return err
			}
			if hasActive {
				return errEntityHasActiveFaults
			}
		}
		node, changed, err := h.store.SetEntityLifecycle(ctx, tx, c.Param("id"), actor.UserID, request.Reason, request.State, now)
		if err != nil {
			return err
		}
		entity = node
		if !changed {
			return nil
		}
		if request.State == domain.EntityLifecycleDecommissioned && request.CascadeFaults {
			closed, err := h.store.CascadeDecommission(ctx, tx, node, actor.UserID, actor.Username, request.Reason, now)
			if err != nil {
				return err
			}
			if closed != nil {
				cascaded = closed
			}
		}
		if request.State == domain.EntityLifecycleActive {
			if err := h.store.NoteRestore(ctx, tx, node, actor.UserID, actor.Username, now); err != nil {
				return err
			}
		}
		return h.store.RebuildImpacts(ctx, tx, node)
	})
	if err != nil {
		h.handleError(c, err, "变更")
		return
	}
	c.JSON(http.StatusOK, gin.H{"entity": entity, "cascaded_faults": cascaded})
}
