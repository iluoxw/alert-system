package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type infrastructureHandlers struct {
	router    *Router
	topology  *store.TopologyRepo
	faults    *store.FaultRepo
	incidents *store.IncidentRepo
	alerts    *store.AlertRepo
}

func (r *Router) RegisterInfrastructureRoutes(topology *store.TopologyRepo, faults *store.FaultRepo, incidents *store.IncidentRepo, alerts *store.AlertRepo) *Router {
	h := &infrastructureHandlers{router: r, topology: topology, faults: faults, incidents: incidents, alerts: alerts}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/infrastructure/overview", h.overview)
	authed.GET("/infrastructure/nodes", h.listNodes)
	authed.GET("/infrastructure/nodes/:id", h.nodeDetail)
	authed.GET("/faults", h.listFaults)
	authed.GET("/faults/:id", h.faultDetail)
	authed.PUT("/faults/:id/evidence/:alert_id", h.decideEvidence)
	authed.PUT("/faults/:id/impacts/:node_id", h.decideImpact)
	admin := authed.Group("/admin")
	admin.Use(r.requireAdmin())
	admin.POST("/entities", h.createManagedEntity)
	admin.PUT("/entities/:id", h.updateManagedEntity)
	return r
}

func (h *infrastructureHandlers) handleError(c *gin.Context, err error, operation string) {
	if errors.Is(err, store.ErrResourceNodeNotFound) || errors.Is(err, store.ErrFaultNotFound) ||
		errors.Is(err, store.ErrFaultImpactNotFound) ||
		errors.Is(err, store.ErrIncidentNotFound) || errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}
	if errors.Is(err, store.ErrManagedEntityReadOnly) || errors.Is(err, store.ErrManagedEntityIdentityImmutable) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		c.JSON(http.StatusConflict, gin.H{"error": "canonical entity key 已存在"})
		return
	}
	h.router.logger.Error("api: Infrastructure "+operation+"失败", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

type managedEntityRequest struct {
	EntityKey   string             `json:"entity_key"`
	Layer       domain.EntityLayer `json:"layer"`
	Name        string             `json:"name"`
	Provider    string             `json:"provider"`
	Environment string             `json:"environment"`
	OwnerTeamID string             `json:"owner_team_id"`
	Labels      map[string]string  `json:"labels"`
}

func (request managedEntityRequest) domain() (domain.ResourceNode, error) {
	request.EntityKey = strings.TrimSpace(request.EntityKey)
	request.Name = strings.TrimSpace(request.Name)
	request.Provider = strings.TrimSpace(request.Provider)
	request.Environment = strings.TrimSpace(request.Environment)
	if !entityKeyPattern.MatchString(request.EntityKey) || request.Name == "" || request.Environment == "" ||
		!uuidPattern.MatchString(request.OwnerTeamID) ||
		(request.Layer != domain.EntityLayerInfrastructure && request.Layer != domain.EntityLayerPlatform) {
		return domain.ResourceNode{}, errors.New("受管实体字段非法")
	}
	return domain.ResourceNode{
		EntityKey: request.EntityKey, Layer: request.Layer, Managed: true,
		Name: request.Name, Provider: request.Provider, Environment: request.Environment,
		OwnerTeamID: request.OwnerTeamID, Labels: request.Labels,
	}, nil
}

func (h *infrastructureHandlers) createManagedEntity(c *gin.Context) {
	var request managedEntityRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	node, err := request.domain()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		node, err = h.topology.CreateManagedEntity(c, tx, node)
		return err
	})
	if err != nil {
		h.handleError(c, err, "受管实体创建")
		return
	}
	c.JSON(http.StatusCreated, node)
}

func (h *infrastructureHandlers) updateManagedEntity(c *gin.Context) {
	var request managedEntityRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	node, err := request.domain()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		node, err = h.topology.UpdateManagedEntity(c, tx, c.Param("id"), node)
		return err
	})
	if err != nil {
		h.handleError(c, err, "受管实体更新")
		return
	}
	c.JSON(http.StatusOK, node)
}

func (h *infrastructureHandlers) overview(c *gin.Context) {
	ctx := c.Request.Context()
	var result store.InfrastructureOverview
	var nodes []domain.ResourceNode
	var faults []domain.Fault
	var incidents []domain.Incident
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = h.topology.Overview(ctx, tx)
		if err != nil {
			return err
		}
		nodes, err = h.topology.ListNodes(ctx, tx, "", 200)
		if err != nil {
			return err
		}
		faults, err = h.faults.List(ctx, tx, "", 50)
		if err != nil {
			return err
		}
		allIncidents, err := h.incidents.List(ctx, tx, "", 50)
		if err != nil {
			return err
		}
		for _, incident := range allIncidents {
			if incident.Status != domain.IncidentStatusResolved {
				incidents = append(incidents, incidentForResponse(incident))
			}
		}
		return nil
	})
	if err != nil {
		h.handleError(c, err, "总览查询")
		return
	}
	if nodes == nil {
		nodes = []domain.ResourceNode{}
	}
	if faults == nil {
		faults = []domain.Fault{}
	}
	if incidents == nil {
		incidents = []domain.Incident{}
	}
	c.JSON(http.StatusOK, gin.H{
		"summary": result, "nodes": nodes, "faults": faults, "incidents": incidents,
	})
}

func (h *infrastructureHandlers) listNodes(c *gin.Context) {
	kind := domain.ResourceNodeKind(c.Query("kind"))
	if kind != "" && !kind.IsValid() {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid node kind"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	ctx := c.Request.Context()
	var items []domain.ResourceNode
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		items, err = h.topology.ListNodes(ctx, tx, kind, limit)
		return err
	})
	if err != nil {
		h.handleError(c, err, "资源列表查询")
		return
	}
	if items == nil {
		items = []domain.ResourceNode{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *infrastructureHandlers) nodeDetail(c *gin.Context) {
	ctx := c.Request.Context()
	var node domain.ResourceNode
	var edges []domain.DependencyEdge
	var faults []domain.Fault
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		node, err = h.topology.GetByID(ctx, tx, c.Param("id"))
		if err != nil {
			return err
		}
		edges, err = h.topology.ListEdgesForNode(ctx, tx, node.ID)
		if err != nil {
			return err
		}
		faults, err = h.faults.ListForNode(ctx, tx, node.ID, 50)
		return err
	})
	if err != nil {
		h.handleError(c, err, "资源详情查询")
		return
	}
	if edges == nil {
		edges = []domain.DependencyEdge{}
	}
	if faults == nil {
		faults = []domain.Fault{}
	}
	c.JSON(http.StatusOK, gin.H{"node": node, "edges": edges, "faults": faults})
}

func (h *infrastructureHandlers) listFaults(c *gin.Context) {
	status := domain.FaultStatus(c.Query("status"))
	if status != "" && !status.IsValid() {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid fault status"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	ctx := c.Request.Context()
	var items []domain.Fault
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		items, err = h.faults.List(ctx, tx, status, limit)
		return err
	})
	if err != nil {
		h.handleError(c, err, "Fault 列表查询")
		return
	}
	if items == nil {
		items = []domain.Fault{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

type faultEvidenceResponse struct {
	Evidence domain.FaultEvidence `json:"evidence"`
	Alert    alertResponse        `json:"alert"`
}

type faultImpactResponse struct {
	Impact    domain.FaultImpact    `json:"impact"`
	Node      domain.ResourceNode   `json:"node"`
	PathNodes []domain.ResourceNode `json:"path_nodes"`
}

func (h *infrastructureHandlers) faultDetail(c *gin.Context) {
	ctx := c.Request.Context()
	var fault domain.Fault
	var evidence []faultEvidenceResponse
	var impacts []faultImpactResponse
	var incident *domain.Incident
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		fault, err = h.faults.GetByID(ctx, tx, c.Param("id"))
		if err != nil {
			return err
		}
		links, err := h.faults.ListEvidence(ctx, tx, fault.ID)
		if err != nil {
			return err
		}
		for _, link := range links {
			alert, err := h.alerts.GetByID(ctx, tx, link.AlertID)
			if err != nil {
				return err
			}
			evidence = append(evidence, faultEvidenceResponse{Evidence: link, Alert: newAlertResponse(alert)})
		}
		impactLinks, err := h.faults.ListImpacts(ctx, tx, fault.ID)
		if err != nil {
			return err
		}
		for _, impact := range impactLinks {
			node, err := h.topology.GetByID(ctx, tx, impact.NodeID)
			if err != nil {
				return err
			}
			pathNodes := make([]domain.ResourceNode, 0, len(impact.Path))
			for _, pathNodeID := range impact.Path {
				pathNode, err := h.topology.GetByID(ctx, tx, pathNodeID)
				if err != nil {
					return err
				}
				pathNodes = append(pathNodes, pathNode)
			}
			impacts = append(impacts, faultImpactResponse{Impact: impact, Node: node, PathNodes: pathNodes})
		}
		linked, err := h.incidents.GetByFaultID(ctx, tx, fault.ID)
		if err == nil {
			redacted := incidentForResponse(linked)
			incident = &redacted
			return nil
		}
		if errors.Is(err, store.ErrIncidentNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		h.handleError(c, err, "Fault 详情查询")
		return
	}
	if evidence == nil {
		evidence = []faultEvidenceResponse{}
	}
	if impacts == nil {
		impacts = []faultImpactResponse{}
	}
	c.JSON(http.StatusOK, gin.H{"fault": fault, "evidence": evidence, "impacts": impacts, "incident": incident})
}

func (h *infrastructureHandlers) decideEvidence(c *gin.Context) {
	var request struct {
		Role       domain.EvidenceRole `json:"role"`
		Rationale  string              `json:"rationale"`
		Confidence float64             `json:"confidence"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if !request.Role.IsValid() || request.Rationale == "" || request.Confidence < 0 || request.Confidence > 1 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid evidence decision"})
		return
	}
	actor, _ := UserFromContext(c)
	ctx := c.Request.Context()
	var stored domain.FaultEvidence
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.faults.GetByID(ctx, tx, c.Param("id")); err != nil {
			return err
		}
		if _, err := h.alerts.GetByID(ctx, tx, c.Param("alert_id")); err != nil {
			return err
		}
		var err error
		now := time.Now()
		stored, err = h.faults.UpsertHumanEvidence(ctx, tx, domain.FaultEvidence{
			FaultID: c.Param("id"), AlertID: c.Param("alert_id"), Role: request.Role,
			Rationale: request.Rationale, Confidence: request.Confidence,
			DecisionOrigin: domain.DecisionOriginHuman, DecidedBy: actor.UserID, DecidedAt: now,
		})
		if err != nil {
			return err
		}
		incident, err := h.incidents.GetByFaultID(ctx, tx, c.Param("id"))
		if errors.Is(err, store.ErrIncidentNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = h.incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
			IncidentID: incident.ID, ActorUserID: actor.UserID, ActorDisplay: actor.Username,
			Kind: domain.IncidentTimelineDecision, Summary: "人工裁决 Fault 证据角色",
			Detail:     map[string]any{"fault_id": c.Param("id"), "alert_id": c.Param("alert_id"), "role": request.Role, "rationale": request.Rationale},
			OccurredAt: now,
		})
		return err
	})
	if err != nil {
		h.handleError(c, err, "人工证据裁决")
		return
	}
	c.JSON(http.StatusOK, stored)
}

func (h *infrastructureHandlers) decideImpact(c *gin.Context) {
	var request struct {
		State      domain.ImpactState `json:"state"`
		Rationale  string             `json:"rationale"`
		Confidence float64            `json:"confidence"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if !request.State.IsValid() || request.Rationale == "" || request.Confidence < 0 || request.Confidence > 1 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid impact decision"})
		return
	}

	actor, _ := UserFromContext(c)
	ctx := c.Request.Context()
	var stored domain.FaultImpact
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.faults.GetByID(ctx, tx, c.Param("id")); err != nil {
			return err
		}
		if _, err := h.topology.GetByID(ctx, tx, c.Param("node_id")); err != nil {
			return err
		}
		now := time.Now()
		var err error
		stored, err = h.faults.UpsertHumanImpact(ctx, tx, domain.FaultImpact{
			FaultID: c.Param("id"), NodeID: c.Param("node_id"), State: request.State,
			Rationale: request.Rationale, Confidence: request.Confidence,
			DecisionOrigin: domain.DecisionOriginHuman, DecidedBy: actor.UserID, DecidedAt: now,
		})
		if err != nil {
			return err
		}
		incident, err := h.incidents.GetByFaultID(ctx, tx, c.Param("id"))
		if errors.Is(err, store.ErrIncidentNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = h.incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
			IncidentID: incident.ID, ActorUserID: actor.UserID, ActorDisplay: actor.Username,
			Kind: domain.IncidentTimelineDecision, Summary: "人工裁决应用 Impact",
			Detail: map[string]any{
				"fault_id": c.Param("id"), "node_id": c.Param("node_id"),
				"state": request.State, "rationale": request.Rationale,
			},
			OccurredAt: now,
		})
		return err
	})
	if err != nil {
		h.handleError(c, err, "人工 Impact 裁决")
		return
	}
	c.JSON(http.StatusOK, stored)
}
