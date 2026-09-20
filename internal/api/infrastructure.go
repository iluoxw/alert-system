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
	if errors.Is(err, store.ErrManagedEntityReadOnly) || errors.Is(err, store.ErrManagedEntityIdentityImmutable) ||
		errors.Is(err, store.ErrServiceDependencyInvalid) {
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
	EntityKey    string              `json:"entity_key"`
	Layer        domain.EntityLayer  `json:"layer"`
	Name         string              `json:"name"`
	Provider     string              `json:"provider"`
	Environment  string              `json:"environment"`
	OwnerTeamID  string              `json:"owner_team_id"`
	Labels       map[string]string   `json:"labels"`
	Aliases      []aliasRequest      `json:"aliases"`
	Dependencies []dependencyRequest `json:"dependencies"`
}

// managedEntityResponse 保持受管实体响应的向后兼容：实体字段仍平铺在顶层（旧客户端
// 与 E2E 直接读 `id`），只追加 aliases/dependencies 两个集合字段；集合恒为 [] 不为 null。
type managedEntityResponse struct {
	domain.ResourceNode
	Aliases      []domain.EntityAlias    `json:"aliases"`
	Dependencies []domain.DependencyEdge `json:"dependencies"`
}

func newManagedEntityResponse(node domain.ResourceNode, aliases []domain.EntityAlias, dependencies []domain.DependencyEdge) managedEntityResponse {
	if aliases == nil {
		aliases = []domain.EntityAlias{}
	}
	if dependencies == nil {
		dependencies = []domain.DependencyEdge{}
	}
	return managedEntityResponse{ResourceNode: node, Aliases: aliases, Dependencies: dependencies}
}

// managedEntityForm 是 managedEntityRequest 校验通过后的落地形态。ResourceNode
// 本身不携带 aliases/dependencies（避免把这两个字段污染到拓扑同步、告警响应等
// 所有消费 ResourceNode 的路径），单独拆出来传给 store 层。
type managedEntityForm struct {
	Node         domain.ResourceNode
	Aliases      []domain.EntityAlias
	Dependencies []domain.DependencyEdge
}

func (request managedEntityRequest) domain() (managedEntityForm, error) {
	request.EntityKey = strings.TrimSpace(request.EntityKey)
	request.Name = strings.TrimSpace(request.Name)
	request.Provider = strings.TrimSpace(request.Provider)
	request.Environment = strings.TrimSpace(request.Environment)
	if !entityKeyPattern.MatchString(request.EntityKey) || request.Name == "" || request.Environment == "" ||
		!uuidPattern.MatchString(request.OwnerTeamID) ||
		(request.Layer != domain.EntityLayerInfrastructure && request.Layer != domain.EntityLayerPlatform) {
		return managedEntityForm{}, errors.New("受管实体字段非法")
	}
	form := managedEntityForm{Node: domain.ResourceNode{
		EntityKey: request.EntityKey, Layer: request.Layer, Managed: true,
		Name: request.Name, Provider: request.Provider, Environment: request.Environment,
		OwnerTeamID: request.OwnerTeamID, Labels: request.Labels,
	}}
	// 字段缺失（nil）与显式空数组语义不同：缺失表示"不修改"，[] 表示"清空"。
	// 旧客户端/脚本或缓存的旧前端调 PUT 时不带这两个字段，若一律当作清空，一次
	// 改名就会抹掉实体全部别名与依赖边——告警映射随之静默失效。
	if request.Aliases != nil {
		form.Aliases = []domain.EntityAlias{}
	}
	if request.Dependencies != nil {
		form.Dependencies = []domain.DependencyEdge{}
	}
	seenAliases := map[string]struct{}{}
	for _, item := range request.Aliases {
		namespace, value := strings.TrimSpace(item.Namespace), strings.TrimSpace(item.Value)
		if namespace == "" || value == "" {
			return form, errors.New("Entity Alias namespace/value 不能为空")
		}
		key := namespace + "\x00" + value
		if _, ok := seenAliases[key]; ok {
			return form, errors.New("同一实体内 Entity Alias 不可重复")
		}
		seenAliases[key] = struct{}{}
		aliasEnabled := true
		if item.Enabled != nil {
			aliasEnabled = *item.Enabled
		}
		form.Aliases = append(form.Aliases, domain.EntityAlias{Namespace: namespace, Value: value, Enabled: aliasEnabled})
	}
	seenTargets := map[string]struct{}{}
	for _, item := range request.Dependencies {
		item.ToEntityID = strings.TrimSpace(item.ToEntityID)
		if item.Criticality == "" {
			item.Criticality = domain.DependencyCriticalityMedium
		}
		if !uuidPattern.MatchString(item.ToEntityID) || !item.Criticality.IsValid() {
			return form, errors.New("依赖字段非法")
		}
		if _, exists := seenTargets[item.ToEntityID]; exists {
			return form, errors.New("同一实体不能重复声明依赖")
		}
		seenTargets[item.ToEntityID] = struct{}{}
		form.Dependencies = append(form.Dependencies, domain.DependencyEdge{
			ToNodeID: item.ToEntityID, Relation: domain.DependencyRelationDependsOn,
			Criticality: item.Criticality, Origin: domain.RelationOriginDeclared,
		})
	}
	return form, nil
}

func (h *infrastructureHandlers) createManagedEntity(c *gin.Context) {
	var request managedEntityRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	form, err := request.domain()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	var node domain.ResourceNode
	var aliases []domain.EntityAlias
	var dependencies []domain.DependencyEdge
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		node, err = h.topology.CreateManagedEntity(c, tx, form.Node, form.Aliases, form.Dependencies)
		if err != nil {
			return err
		}
		if aliases, err = h.topology.ListAliases(c, tx, node.ID); err != nil {
			return err
		}
		dependencies, err = h.topology.ListDependenciesForSource(c, tx, store.EntityRegistrySource, node.ID)
		return err
	})
	if err != nil {
		h.handleError(c, err, "受管实体创建")
		return
	}
	c.JSON(http.StatusCreated, newManagedEntityResponse(node, aliases, dependencies))
}

func (h *infrastructureHandlers) updateManagedEntity(c *gin.Context) {
	var request managedEntityRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	form, err := request.domain()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	// 依赖指向自身在创建阶段无法判断（实体 ID 尚未生成），但更新阶段已知 URL 中
	// 的目标 ID，提前拒绝给出更明确的错误；store 层仍会用同样的规则兜底校验。
	for _, dependency := range form.Dependencies {
		if dependency.ToNodeID == c.Param("id") {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "受管实体不能依赖自身"})
			return
		}
	}
	var node domain.ResourceNode
	var aliases []domain.EntityAlias
	var dependencies []domain.DependencyEdge
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		node, err = h.topology.UpdateManagedEntity(c, tx, c.Param("id"), form.Node, form.Aliases, form.Dependencies)
		if err != nil {
			return err
		}
		if aliases, err = h.topology.ListAliases(c, tx, node.ID); err != nil {
			return err
		}
		dependencies, err = h.topology.ListDependenciesForSource(c, tx, store.EntityRegistrySource, node.ID)
		return err
	})
	if err != nil {
		h.handleError(c, err, "受管实体更新")
		return
	}
	c.JSON(http.StatusOK, newManagedEntityResponse(node, aliases, dependencies))
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
		nodes, err = h.topology.ListNodes(ctx, tx, "", domain.EntityLifecycleActive, 200)
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
	lifecycle := domain.EntityLifecycleState(c.DefaultQuery("lifecycle", string(domain.EntityLifecycleActive)))
	if lifecycle != domain.EntityLifecycleAll && !lifecycle.IsValid() {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid lifecycle"})
		return
	}
	ctx := c.Request.Context()
	var items []domain.ResourceNode
	err := h.router.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		items, err = h.topology.ListNodes(ctx, tx, kind, lifecycle, limit)
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
	var aliases []domain.EntityAlias
	var dependencies []domain.DependencyEdge
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
		if err != nil {
			return err
		}
		if aliases, err = h.topology.ListAliases(ctx, tx, node.ID); err != nil {
			return err
		}
		// 依赖按节点自己的写入 source 过滤，只回显该节点自己声明的边（entity_registry/
		// service_registry/拓扑同步各自的 declared 边），供编辑表单回显；已在 edges
		// 里的双向全量视图不受影响。
		dependencies, err = h.topology.ListDependenciesForSource(ctx, tx, node.Source, node.ID)
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
	if aliases == nil {
		aliases = []domain.EntityAlias{}
	}
	if dependencies == nil {
		dependencies = []domain.DependencyEdge{}
	}
	c.JSON(http.StatusOK, gin.H{
		"node": node, "edges": edges, "faults": faults,
		"aliases": aliases, "dependencies": dependencies,
	})
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
