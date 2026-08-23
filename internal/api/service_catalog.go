package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/domain"
	"alert-system/internal/pipeline"
	"alert-system/internal/safelink"
	"alert-system/internal/store"
)

var entityKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{2,127}$`)

type serviceCatalogHandlers struct {
	router    *Router
	services  *store.ServiceCatalogRepo
	mappings  *store.AlertMappingRepo
	projector pipeline.AlertProjector
	alerts    *store.AlertRepo
	impacts   impactReprojector
}

type impactReprojector interface {
	ProviderNodeIDs(ctx context.Context, tx pgx.Tx, consumerNodeID string) ([]string, error)
	RebuildForProviderNodeIDs(ctx context.Context, tx pgx.Tx, providerNodeIDs []string) error
}

func (r *Router) RegisterServiceCatalogRoutes(services *store.ServiceCatalogRepo, mappings *store.AlertMappingRepo, projector pipeline.AlertProjector, impacts impactReprojector, alerts *store.AlertRepo) *Router {
	h := &serviceCatalogHandlers{router: r, services: services, mappings: mappings, projector: projector, impacts: impacts, alerts: alerts}
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/services", h.listServices)
	authed.GET("/services/:id", h.getService)
	authed.GET("/alerts/:id/mapping", h.getAlertMapping)
	authed.GET("/alert-mapping/results", h.listMappingResults)
	authed.GET("/alert-mapping/summary", h.mappingSummary)

	admin := authed.Group("/admin")
	admin.Use(r.requireAdmin())
	admin.POST("/services", h.createService)
	admin.PUT("/services/:id", h.updateService)
	admin.GET("/alert-mappings", h.listPolicies)
	admin.POST("/alert-mappings", h.createPolicy)
	admin.PUT("/alert-mappings/:id", h.updatePolicy)
	admin.DELETE("/alert-mappings/:id", h.deletePolicy)
	admin.POST("/alert-mappings/preview", h.previewPolicy)
	admin.POST("/alert-mapping/results/:alert_id/reprocess", h.reprocessAlert)
	return r
}

type aliasRequest struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
	Enabled   *bool  `json:"enabled"`
}

type dependencyRequest struct {
	ToEntityID  string                       `json:"to_entity_id"`
	Criticality domain.DependencyCriticality `json:"criticality"`
}

// reasonCodedError 让 422 响应同时带稳定机器码和中文说明。前端按 reason_code 分支，
// 不解析文案；文案可以本地化调整而不破坏调用方。
type reasonCodedError struct {
	code    string
	message string
	wrapped error
}

func (e *reasonCodedError) Error() string { return e.message }
func (e *reasonCodedError) Unwrap() error { return e.wrapped }

func reasonCodeOf(err error) string {
	var coded *reasonCodedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return ""
}

// respondUnprocessable 统一 422 输出形状：error 是给人看的说明，reason_code 是给前端
// 分支用的稳定机器码（没有机器码时不输出该键，保持既有响应形状不变）。
func respondUnprocessable(c *gin.Context, err error) {
	body := gin.H{"error": err.Error()}
	if code := reasonCodeOf(err); code != "" {
		body["reason_code"] = code
	}
	c.JSON(http.StatusUnprocessableEntity, body)
}

type serviceRequest struct {
	EntityKey string `json:"entity_key"`
	Name      string `json:"name"`
	// EntityLayer/EntityKind 是 V2 的可选服务分类。用 pointer 才能区分"旧客户端未传"
	// 与"显式传空字符串"：前者按 create 缺省 / update 保留处理，后者一律 422。
	EntityLayer  *string             `json:"entity_layer"`
	EntityKind   *string             `json:"entity_kind"`
	Environment  string              `json:"environment"`
	OwnerTeamID  string              `json:"owner_team_id"`
	Description  string              `json:"description"`
	Tier         domain.ServiceTier  `json:"tier"`
	DashboardURL string              `json:"dashboard_url"`
	RunbookURL   string              `json:"runbook_url"`
	Enabled      *bool               `json:"enabled"`
	Aliases      []aliasRequest      `json:"aliases"`
	Dependencies []dependencyRequest `json:"dependencies"`
	Labels       map[string]string   `json:"labels"`
}

// classification 解析可选的 entity_layer/entity_kind。两者都未传时返回空 pair 表示
// "未传"，由 create 补缺省、update 在事务内保留当前值；只传一个字段无法判断意图，
// 直接拒绝而不是猜另一半。
func (request serviceRequest) classification() (domain.EntityLayer, domain.ResourceNodeKind, error) {
	if request.EntityLayer == nil && request.EntityKind == nil {
		return "", "", nil
	}
	if request.EntityLayer == nil || request.EntityKind == nil {
		return "", "", &reasonCodedError{
			code:    "service_classification_incomplete",
			message: "entity_layer 与 entity_kind 必须同时提供",
		}
	}
	layer := domain.EntityLayer(strings.TrimSpace(*request.EntityLayer))
	kind := domain.ResourceNodeKind(strings.TrimSpace(*request.EntityKind))
	if err := domain.ValidateServiceClassification(layer, kind); err != nil {
		return "", "", &reasonCodedError{
			code:    "service_classification_invalid",
			message: "服务类型只支持 application/business_system 或 platform/infrastructure_capability",
			wrapped: err,
		}
	}
	return layer, kind, nil
}

// applyCreateClassificationDefault 只在 create 路径补旧 V1 兼容缺省。update 绝不能
// 调它——那会让不带分类字段的旧客户端把 platform 服务改回 application。
func applyCreateClassificationDefault(service *domain.ServiceRegistration) {
	if service.Entity.Layer == "" && service.Entity.Kind == "" {
		service.Entity.Layer, service.Entity.Kind = domain.DefaultServiceClassification()
	}
}

func (request serviceRequest) domain() (domain.ServiceRegistration, error) {
	request.EntityKey = strings.TrimSpace(request.EntityKey)
	request.Name = strings.TrimSpace(request.Name)
	request.Environment = strings.TrimSpace(request.Environment)
	if !entityKeyPattern.MatchString(request.EntityKey) || request.Name == "" || request.Environment == "" ||
		!uuidPattern.MatchString(request.OwnerTeamID) || !request.Tier.IsValid() {
		return domain.ServiceRegistration{}, errors.New("应用服务字段非法")
	}
	layer, kind, err := request.classification()
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	// Dashboard/Runbook 会被运营页面渲染成值班人直接点开的链接，因此写入时就按共享策略
	// 收敛：只接受 alert-system 内部单斜杠路径或绝对 http(s) URL。空值仍然合法——
	// 未配置 Dashboard/Runbook 是正常状态。
	dashboardURL, err := validateServiceLink(request.DashboardURL, "dashboard_url")
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	runbookURL, err := validateServiceLink(request.RunbookURL, "runbook_url")
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	service := domain.ServiceRegistration{
		Entity: domain.ResourceNode{EntityKey: request.EntityKey, Layer: layer,
			Managed: true, Kind: kind, Name: request.Name,
			Environment: request.Environment, OwnerTeamID: request.OwnerTeamID, Labels: request.Labels},
		OwnerTeamID: request.OwnerTeamID, Description: strings.TrimSpace(request.Description),
		Tier: request.Tier, DashboardURL: dashboardURL,
		RunbookURL: runbookURL, Enabled: enabled,
	}
	seen := map[string]struct{}{}
	for _, item := range request.Aliases {
		namespace, value := strings.TrimSpace(item.Namespace), strings.TrimSpace(item.Value)
		if namespace == "" || value == "" {
			return service, errors.New("Entity Alias namespace/value 不能为空")
		}
		key := namespace + "\x00" + value
		if _, ok := seen[key]; ok {
			return service, errors.New("同一服务内 Entity Alias 不可重复")
		}
		seen[key] = struct{}{}
		aliasEnabled := true
		if item.Enabled != nil {
			aliasEnabled = *item.Enabled
		}
		service.Aliases = append(service.Aliases, domain.EntityAlias{Namespace: namespace, Value: value, Enabled: aliasEnabled})
	}
	dependencyTargets := map[string]struct{}{}
	for _, item := range request.Dependencies {
		item.ToEntityID = strings.TrimSpace(item.ToEntityID)
		if item.Criticality == "" {
			item.Criticality = domain.DependencyCriticalityMedium
		}
		if !uuidPattern.MatchString(item.ToEntityID) || !item.Criticality.IsValid() {
			return service, errors.New("服务依赖字段非法")
		}
		if _, exists := dependencyTargets[item.ToEntityID]; exists {
			return service, errors.New("同一服务不能重复声明依赖实体")
		}
		dependencyTargets[item.ToEntityID] = struct{}{}
		service.Dependencies = append(service.Dependencies, domain.DependencyEdge{
			ToNodeID: item.ToEntityID, Relation: domain.DependencyRelationDependsOn,
			Criticality: item.Criticality, Origin: domain.RelationOriginDeclared,
		})
	}
	return service, nil
}

// reasonServiceLinkInvalid 是 Dashboard/Runbook 链接非法的稳定机器码。create 与 update
// 共用同一个 code，前端只需要一条分支。
const reasonServiceLinkInvalid = "service_link_invalid"

// validateServiceLink 按共享的 safelink 策略校验一个登记链接。
//
// 空值合法（未配置）；其余必须是 alert-system 内部单斜杠路径或绝对 http(s) URL。
// 拒绝 javascript/data/vbscript/mailto/自定义 scheme、裸主机、scheme-relative、控制字符
// 与反斜杠伪造 authority——这是对 V1"只 trim"口径的有意安全收紧，见 internal/safelink。
func validateServiceLink(raw, field string) (string, error) {
	value := safelink.Normalize(raw)
	if value == "" {
		return "", nil
	}
	if !safelink.IsSafe(value) {
		return "", &reasonCodedError{
			code:    reasonServiceLinkInvalid,
			message: field + " 只支持 alert-system 内部路径（以 / 开头）或绝对 http(s) URL",
		}
	}
	return value, nil
}

func (h *serviceCatalogHandlers) handleStoreError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrServiceNotFound) || errors.Is(err, store.ErrMappingPolicyNotFound) ||
		errors.Is(err, store.ErrMappingResultNotFound) || errors.Is(err, store.ErrAlertNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}
	if errors.Is(err, store.ErrServiceEntityKeyImmutable) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "canonical entity key 创建后不可修改"})
		return
	}
	if errors.Is(err, store.ErrServiceClassificationImmutable) {
		respondUnprocessable(c, &reasonCodedError{
			code:    "service_classification_immutable",
			message: "服务类型创建后不可修改；分类迁移需要人工重新裁决 confirmed Impact",
			wrapped: err,
		})
		return
	}
	if errors.Is(err, domain.ErrInvalidServiceClassification) {
		respondUnprocessable(c, &reasonCodedError{
			code:    "service_classification_invalid",
			message: "服务类型只支持 application/business_system 或 platform/infrastructure_capability",
			wrapped: err,
		})
		return
	}
	if errors.Is(err, store.ErrServiceDependencyInvalid) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		c.JSON(http.StatusConflict, gin.H{"error": "名称、优先级、entity key 或同一实体 Alias 已存在"})
		return
	}
	h.router.logger.Error("api: service catalog / mapping 操作失败", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

func (h *serviceCatalogHandlers) listServices(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	var services []domain.ServiceRegistration
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		services, err = h.services.List(c, tx, limit)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	if services == nil {
		services = []domain.ServiceRegistration{}
	}
	c.JSON(http.StatusOK, gin.H{"services": services})
}

func (h *serviceCatalogHandlers) getService(c *gin.Context) {
	var service domain.ServiceRegistration
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		service, err = h.services.GetByID(c, tx, c.Param("id"))
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, service)
}

func (h *serviceCatalogHandlers) createService(c *gin.Context) {
	var request serviceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	service, err := request.domain()
	if err != nil {
		respondUnprocessable(c, err)
		return
	}
	applyCreateClassificationDefault(&service)
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		service, err = h.services.Create(c, tx, service)
		if err != nil || h.impacts == nil {
			return err
		}
		providers, err := h.impacts.ProviderNodeIDs(c, tx, service.Entity.ID)
		if err != nil {
			return err
		}
		return h.impacts.RebuildForProviderNodeIDs(c, tx, providers)
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, service)
}

func (h *serviceCatalogHandlers) updateService(c *gin.Context) {
	var request serviceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	// update 不补缺省：分类为空表示"沿用当前登记"，由 store 在同一事务内读取并保留。
	service, err := request.domain()
	if err != nil {
		respondUnprocessable(c, err)
		return
	}
	err = h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var oldProviders []string
		if h.impacts != nil {
			current, getErr := h.services.GetByID(c, tx, c.Param("id"))
			if getErr != nil {
				return getErr
			}
			oldProviders, getErr = h.impacts.ProviderNodeIDs(c, tx, current.Entity.ID)
			if getErr != nil {
				return getErr
			}
		}
		var err error
		service, err = h.services.Update(c, tx, c.Param("id"), service)
		if err != nil || h.impacts == nil {
			return err
		}
		newProviders, err := h.impacts.ProviderNodeIDs(c, tx, service.Entity.ID)
		if err != nil {
			return err
		}
		return h.impacts.RebuildForProviderNodeIDs(c, tx, append(oldProviders, newProviders...))
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, service)
}

func normalizePolicy(policy *domain.AlertMappingPolicy) {
	policy.Name = strings.TrimSpace(policy.Name)
	policy.AliasNamespace = strings.TrimSpace(policy.AliasNamespace)
	policy.AliasLabel = strings.TrimSpace(policy.AliasLabel)
	policy.FaultKind = strings.TrimSpace(policy.FaultKind)
	if policy.FaultScope == "" {
		policy.FaultScope = domain.FaultScopeCapability
	}
	if policy.Confidence == 0 {
		policy.Confidence = 0.9
	}
	if policy.Version == 0 {
		policy.Version = 1
	}
}

func (h *serviceCatalogHandlers) listPolicies(c *gin.Context) {
	var policies []domain.AlertMappingPolicy
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		policies, err = h.mappings.ListPolicies(c, tx, false, 500)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	if policies == nil {
		policies = []domain.AlertMappingPolicy{}
	}
	c.JSON(http.StatusOK, gin.H{"policies": policies})
}

func (h *serviceCatalogHandlers) createPolicy(c *gin.Context) {
	var policy domain.AlertMappingPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	normalizePolicy(&policy)
	if err := policy.Validate(); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		policy, err = h.mappings.CreatePolicy(c, tx, policy)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, policy)
}

func (h *serviceCatalogHandlers) updatePolicy(c *gin.Context) {
	var policy domain.AlertMappingPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	normalizePolicy(&policy)
	if err := policy.Validate(); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		policy, err = h.mappings.UpdatePolicy(c, tx, c.Param("id"), policy)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, policy)
}

func (h *serviceCatalogHandlers) deletePolicy(c *gin.Context) {
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error { return h.mappings.DeletePolicy(c, tx, c.Param("id")) })
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *serviceCatalogHandlers) previewPolicy(c *gin.Context) {
	var request struct {
		Policy domain.AlertMappingPolicy `json:"policy"`
		Limit  int                       `json:"limit"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	normalizePolicy(&request.Policy)
	if err := request.Policy.Validate(); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	if request.Limit <= 0 || request.Limit > 100 {
		request.Limit = 20
	}
	var matches []alertResponse
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		page, err := h.alerts.List(c, tx, store.AlertFilter{Limit: 500})
		if err != nil {
			return err
		}
		for _, alert := range page.Alerts {
			labels := make(map[string]string, len(alert.Labels)+len(alert.ResourceRef)+2)
			for k, v := range alert.ResourceRef {
				labels[k] = v
			}
			for k, v := range alert.Labels {
				labels[k] = v
			}
			labels["severity"] = string(alert.Severity)
			labels["source"] = alert.Source
			if request.Policy.SourceIntegrationID != "" && request.Policy.SourceIntegrationID != alert.SourceIntegrationID {
				continue
			}
			if request.Policy.Matchers.Matches(labels) {
				matches = append(matches, newAlertResponse(alert))
				if len(matches) >= request.Limit {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	if matches == nil {
		matches = []alertResponse{}
	}
	c.JSON(http.StatusOK, gin.H{"matches": matches, "count": len(matches)})
}

type mappingResultResponse struct {
	Result domain.AlertMappingResult `json:"result"`
	Alert  alertResponse             `json:"alert"`
}

func (h *serviceCatalogHandlers) listMappingResults(c *gin.Context) {
	status := domain.AlertMappingStatus(c.Query("status"))
	if status != "" && !status.IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mapping status"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	var items []mappingResultResponse
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		results, err := h.mappings.ListResults(c, tx, status, limit)
		if err != nil {
			return err
		}
		for _, result := range results {
			alert, err := h.alerts.GetByID(c, tx, result.AlertID)
			if err != nil {
				return err
			}
			items = append(items, mappingResultResponse{Result: result, Alert: newAlertResponse(alert)})
		}
		return nil
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	if items == nil {
		items = []mappingResultResponse{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *serviceCatalogHandlers) mappingSummary(c *gin.Context) {
	var summary map[string]int
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		summary, err = h.mappings.Summary(c, tx)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, summary)
}

func (h *serviceCatalogHandlers) getAlertMapping(c *gin.Context) {
	var result domain.AlertMappingResult
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		var err error
		result, err = h.mappings.GetResult(c, tx, c.Param("id"))
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *serviceCatalogHandlers) reprocessAlert(c *gin.Context) {
	var result domain.AlertMappingResult
	err := h.router.tx.WithTx(c, func(tx pgx.Tx) error {
		alert, err := h.alerts.GetByID(c, tx, c.Param("alert_id"))
		if err != nil {
			return err
		}
		result, err = h.projector.Project(c, tx, alert)
		return err
	})
	if err != nil {
		h.handleStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
