package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

var (
	ErrServiceNotFound           = errors.New("store: 应用服务不存在")
	ErrServiceEntityKeyImmutable = errors.New("store: canonical entity key 创建后不可修改")
	ErrServiceDependencyInvalid  = errors.New("store: 服务依赖非法")
	ErrMappingPolicyNotFound     = errors.New("store: Alert Mapping 策略不存在")
	ErrMappingResultNotFound     = errors.New("store: Alert Mapping 结果不存在")
)

type ServiceCatalogRepo struct{}

func NewServiceCatalogRepo() *ServiceCatalogRepo { return &ServiceCatalogRepo{} }

func validateService(service domain.ServiceRegistration) error {
	if strings.TrimSpace(service.Entity.EntityKey) == "" || strings.TrimSpace(service.Entity.Name) == "" ||
		strings.TrimSpace(service.Entity.Environment) == "" || service.OwnerTeamID == "" || !service.Tier.IsValid() {
		return fmt.Errorf("store: 应用服务字段非法")
	}
	for _, alias := range service.Aliases {
		if strings.TrimSpace(alias.Namespace) == "" || strings.TrimSpace(alias.Value) == "" {
			return fmt.Errorf("store: Entity Alias 字段非法")
		}
	}
	seenTargets := map[string]struct{}{}
	for _, dependency := range service.Dependencies {
		if strings.TrimSpace(dependency.ToNodeID) == "" || dependency.Relation != domain.DependencyRelationDependsOn ||
			!dependency.Criticality.IsValid() {
			return fmt.Errorf("%w: 字段不完整", ErrServiceDependencyInvalid)
		}
		if _, exists := seenTargets[dependency.ToNodeID]; exists {
			return fmt.Errorf("%w: 目标不可重复", ErrServiceDependencyInvalid)
		}
		seenTargets[dependency.ToNodeID] = struct{}{}
	}
	return nil
}

func (r *ServiceCatalogRepo) Create(ctx context.Context, tx pgx.Tx, service domain.ServiceRegistration) (domain.ServiceRegistration, error) {
	if err := validateService(service); err != nil {
		return domain.ServiceRegistration{}, err
	}
	now := time.Now()
	labels, err := marshalJSONObject(service.Entity.Labels)
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	attributes, err := marshalJSONObject(service.Entity.Attributes)
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	var entityID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO resource_nodes (
			entity_key, layer, managed, source, external_ref, kind, name, provider,
			environment, owner_team_id, labels, attributes, last_observed_at
		) VALUES ($1, 'application', true, 'service_registry', $1, 'business_system',
			$2, '', $3, $4, $5, $6, $7)
		RETURNING id
	`, service.Entity.EntityKey, service.Entity.Name, service.Entity.Environment,
		service.OwnerTeamID, labels, attributes, now).Scan(&entityID); err != nil {
		return domain.ServiceRegistration{}, fmt.Errorf("store: 创建服务实体失败: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO service_registrations (
			entity_id, owner_team_id, description, tier, dashboard_url, runbook_url, enabled
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, entityID, service.OwnerTeamID, service.Description, service.Tier,
		service.DashboardURL, service.RunbookURL, service.Enabled).Scan(&service.ID); err != nil {
		return domain.ServiceRegistration{}, fmt.Errorf("store: 创建服务登记失败: %w", err)
	}
	if err := r.replaceAliases(ctx, tx, entityID, service.Aliases); err != nil {
		return domain.ServiceRegistration{}, err
	}
	if err := r.replaceDependencies(ctx, tx, entityID, service.Entity.Environment, service.Dependencies); err != nil {
		return domain.ServiceRegistration{}, err
	}
	return r.GetByID(ctx, tx, service.ID)
}

func (r *ServiceCatalogRepo) Update(ctx context.Context, tx pgx.Tx, id string, service domain.ServiceRegistration) (domain.ServiceRegistration, error) {
	if err := validateService(service); err != nil {
		return domain.ServiceRegistration{}, err
	}
	var entityID, currentEntityKey string
	if err := tx.QueryRow(ctx, `
		SELECT sr.entity_id::text, n.entity_key
		FROM service_registrations sr JOIN resource_nodes n ON n.id = sr.entity_id
		WHERE sr.id = $1
	`, id).Scan(&entityID, &currentEntityKey); errors.Is(err, pgx.ErrNoRows) {
		return domain.ServiceRegistration{}, ErrServiceNotFound
	} else if err != nil {
		return domain.ServiceRegistration{}, err
	}
	if service.Entity.EntityKey != currentEntityKey {
		return domain.ServiceRegistration{}, ErrServiceEntityKeyImmutable
	}
	labels, err := marshalJSONObject(service.Entity.Labels)
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	attributes, err := marshalJSONObject(service.Entity.Attributes)
	if err != nil {
		return domain.ServiceRegistration{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE resource_nodes SET name = $2,
			environment = $3, owner_team_id = $4, labels = $5, attributes = $6,
			last_observed_at = now(), managed = true, layer = 'application'
		WHERE id = $1
	`, entityID, service.Entity.Name, service.Entity.Environment,
		service.OwnerTeamID, labels, attributes); err != nil {
		return domain.ServiceRegistration{}, fmt.Errorf("store: 更新服务实体失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE service_registrations SET owner_team_id = $2, description = $3, tier = $4,
			dashboard_url = $5, runbook_url = $6, enabled = $7 WHERE id = $1
	`, id, service.OwnerTeamID, service.Description, service.Tier,
		service.DashboardURL, service.RunbookURL, service.Enabled); err != nil {
		return domain.ServiceRegistration{}, fmt.Errorf("store: 更新服务登记失败: %w", err)
	}
	if err := r.replaceAliases(ctx, tx, entityID, service.Aliases); err != nil {
		return domain.ServiceRegistration{}, err
	}
	if err := r.replaceDependencies(ctx, tx, entityID, service.Entity.Environment, service.Dependencies); err != nil {
		return domain.ServiceRegistration{}, err
	}
	return r.GetByID(ctx, tx, id)
}

// serviceRegistrySource 是应用服务依赖边的写入来源；replaceEntityAliases/
// replaceEntityDependencies 定义在 infrastructure.go，供 ServiceCatalogRepo 与
// TopologyRepo（受管实体）共用，避免同一段 DELETE+INSERT 逻辑抄两份。
const serviceRegistrySource = "service_registry"

func (r *ServiceCatalogRepo) replaceAliases(ctx context.Context, tx pgx.Tx, entityID string, aliases []domain.EntityAlias) error {
	return replaceEntityAliases(ctx, tx, entityID, aliases)
}

func (r *ServiceCatalogRepo) replaceDependencies(ctx context.Context, tx pgx.Tx, entityID, environment string, dependencies []domain.DependencyEdge) error {
	return replaceEntityDependencies(ctx, tx, serviceRegistrySource, entityID, environment, dependencies)
}

const serviceSelect = `
	SELECT ` + resourceNodeColumnsQualified + `,
		sr.id::text, sr.owner_team_id::text, t.name, sr.description, sr.tier,
		sr.dashboard_url, sr.runbook_url, sr.enabled, sr.created_at, sr.updated_at
	FROM service_registrations sr
	JOIN resource_nodes n ON n.id = sr.entity_id
	JOIN teams t ON t.id = sr.owner_team_id
`

func scanService(row topologyRowScanner) (domain.ServiceRegistration, error) {
	service := domain.ServiceRegistration{
		Aliases:      []domain.EntityAlias{},
		Dependencies: []domain.DependencyEdge{},
	}
	entity, err := scanResourceNode(row, &service.ID, &service.OwnerTeamID, &service.OwnerTeamName,
		&service.Description, &service.Tier, &service.DashboardURL, &service.RunbookURL,
		&service.Enabled, &service.CreatedAt, &service.UpdatedAt)
	if err != nil {
		return service, err
	}
	service.Entity = entity
	return service, nil
}

func (r *ServiceCatalogRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.ServiceRegistration, error) {
	service, err := scanService(tx.QueryRow(ctx, serviceSelect+` WHERE sr.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return service, ErrServiceNotFound
	}
	if err != nil {
		return service, fmt.Errorf("store: 查询应用服务失败: %w", err)
	}
	service.Aliases, err = r.ListAliases(ctx, tx, service.Entity.ID)
	if err != nil {
		return service, err
	}
	service.Dependencies, err = r.ListDependencies(ctx, tx, service.Entity.ID)
	return service, err
}

func (r *ServiceCatalogRepo) GetByEntityID(ctx context.Context, tx pgx.Tx, entityID string) (domain.ServiceRegistration, error) {
	service, err := scanService(tx.QueryRow(ctx, serviceSelect+` WHERE sr.entity_id = $1`, entityID))
	if errors.Is(err, pgx.ErrNoRows) {
		return service, ErrServiceNotFound
	}
	if err != nil {
		return service, err
	}
	service.Aliases, err = r.ListAliases(ctx, tx, service.Entity.ID)
	if err != nil {
		return service, err
	}
	service.Dependencies, err = r.ListDependencies(ctx, tx, service.Entity.ID)
	return service, err
}

func (r *ServiceCatalogRepo) List(ctx context.Context, tx pgx.Tx, limit int) ([]domain.ServiceRegistration, error) {
	rows, err := tx.Query(ctx, serviceSelect+` ORDER BY sr.enabled DESC, sr.tier, n.name LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询应用服务列表失败: %w", err)
	}
	services := []domain.ServiceRegistration{}
	for rows.Next() {
		service, err := scanService(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// pgx 的单连接事务不能在 rows 仍打开时执行下一条查询。先完整扫描并显式
	// Close，再逐服务读取 Alias，避免 conn busy。
	rows.Close()
	for i := range services {
		services[i].Aliases, err = r.ListAliases(ctx, tx, services[i].Entity.ID)
		if err != nil {
			return nil, err
		}
		services[i].Dependencies, err = r.ListDependencies(ctx, tx, services[i].Entity.ID)
		if err != nil {
			return nil, err
		}
	}
	return services, nil
}

func (r *ServiceCatalogRepo) ListDependencies(ctx context.Context, tx pgx.Tx, entityID string) ([]domain.DependencyEdge, error) {
	return listEntityDependencies(ctx, tx, serviceRegistrySource, entityID)
}

func (r *ServiceCatalogRepo) ListAliases(ctx context.Context, tx pgx.Tx, entityID string) ([]domain.EntityAlias, error) {
	return listEntityAliases(ctx, tx, entityID)
}

func (r *ServiceCatalogRepo) ResolveEntityKey(ctx context.Context, tx pgx.Tx, entityKey string) ([]domain.ResourceNode, error) {
	rows, err := tx.Query(ctx, `SELECT `+resourceNodeColumns+` FROM resource_nodes WHERE entity_key = $1`, entityKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []domain.ResourceNode
	for rows.Next() {
		node, err := scanResourceNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (r *ServiceCatalogRepo) ResolveAlias(ctx context.Context, tx pgx.Tx, namespace, value string) ([]domain.ResourceNode, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+resourceNodeColumnsQualified+`
		FROM entity_aliases a
		JOIN resource_nodes n ON n.id = a.entity_id
		WHERE a.namespace = $1 AND a.alias_value = $2 AND a.enabled
		ORDER BY n.entity_key
	`, namespace, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []domain.ResourceNode
	for rows.Next() {
		node, err := scanResourceNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

type AlertMappingRepo struct{}

func NewAlertMappingRepo() *AlertMappingRepo { return &AlertMappingRepo{} }

func marshalMappingPolicy(policy domain.AlertMappingPolicy) ([]byte, []byte, error) {
	matchers, err := json.Marshal(policy.Matchers)
	if err != nil {
		return nil, nil, err
	}
	labels, err := json.Marshal(policy.CorrelationLabels)
	return matchers, labels, err
}

func (r *AlertMappingRepo) CreatePolicy(ctx context.Context, tx pgx.Tx, policy domain.AlertMappingPolicy) (domain.AlertMappingPolicy, error) {
	if err := policy.Validate(); err != nil {
		return policy, err
	}
	matchers, labels, err := marshalMappingPolicy(policy)
	if err != nil {
		return policy, err
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO alert_mapping_policies (
			name, priority, source_integration_id, matchers, target_entity_id,
			alias_namespace, alias_label, fault_kind, fault_scope, evidence_role,
			correlation_labels, confidence, enabled
		) VALUES ($1, $2, NULLIF($3, '')::uuid, $4, NULLIF($5, '')::uuid,
			$6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+mappingPolicyColumns,
		policy.Name, policy.Priority, policy.SourceIntegrationID, matchers, policy.TargetEntityID,
		policy.AliasNamespace, policy.AliasLabel, policy.FaultKind, policy.FaultScope,
		policy.EvidenceRole, labels, policy.Confidence, policy.Enabled)
	return scanMappingPolicy(row)
}

func (r *AlertMappingRepo) UpdatePolicy(ctx context.Context, tx pgx.Tx, id string, policy domain.AlertMappingPolicy) (domain.AlertMappingPolicy, error) {
	if err := policy.Validate(); err != nil {
		return policy, err
	}
	matchers, labels, err := marshalMappingPolicy(policy)
	if err != nil {
		return policy, err
	}
	row := tx.QueryRow(ctx, `
		UPDATE alert_mapping_policies SET
			name = $2, priority = $3, source_integration_id = NULLIF($4, '')::uuid,
			matchers = $5, target_entity_id = NULLIF($6, '')::uuid,
			alias_namespace = $7, alias_label = $8, fault_kind = $9, fault_scope = $10,
			evidence_role = $11, correlation_labels = $12, confidence = $13,
			enabled = $14, version = version + 1
		WHERE id = $1 RETURNING `+mappingPolicyColumns,
		id, policy.Name, policy.Priority, policy.SourceIntegrationID, matchers,
		policy.TargetEntityID, policy.AliasNamespace, policy.AliasLabel, policy.FaultKind,
		policy.FaultScope, policy.EvidenceRole, labels, policy.Confidence, policy.Enabled)
	updated, err := scanMappingPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, ErrMappingPolicyNotFound
	}
	return updated, err
}

const mappingPolicyColumns = `
	id::text, name, priority, COALESCE(source_integration_id::text, ''), matchers,
	COALESCE(target_entity_id::text, ''), alias_namespace, alias_label, fault_kind,
	fault_scope, evidence_role, correlation_labels, confidence, enabled, version,
	created_at, updated_at
`

func scanMappingPolicy(row configRowScanner) (domain.AlertMappingPolicy, error) {
	var policy domain.AlertMappingPolicy
	var matchers, labels []byte
	if err := row.Scan(&policy.ID, &policy.Name, &policy.Priority, &policy.SourceIntegrationID,
		&matchers, &policy.TargetEntityID, &policy.AliasNamespace, &policy.AliasLabel,
		&policy.FaultKind, &policy.FaultScope, &policy.EvidenceRole, &labels,
		&policy.Confidence, &policy.Enabled, &policy.Version, &policy.CreatedAt, &policy.UpdatedAt); err != nil {
		return policy, err
	}
	if err := json.Unmarshal(matchers, &policy.Matchers); err != nil {
		return policy, err
	}
	if err := json.Unmarshal(labels, &policy.CorrelationLabels); err != nil {
		return policy, err
	}
	return policy, nil
}

func (r *AlertMappingRepo) GetPolicy(ctx context.Context, tx pgx.Tx, id string) (domain.AlertMappingPolicy, error) {
	policy, err := scanMappingPolicy(tx.QueryRow(ctx, `SELECT `+mappingPolicyColumns+` FROM alert_mapping_policies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, ErrMappingPolicyNotFound
	}
	return policy, err
}

func (r *AlertMappingRepo) ListPolicies(ctx context.Context, tx pgx.Tx, enabledOnly bool, limit int) ([]domain.AlertMappingPolicy, error) {
	query := `SELECT ` + mappingPolicyColumns + ` FROM alert_mapping_policies`
	if enabledOnly {
		query += ` WHERE enabled`
	}
	query += ` ORDER BY priority, name LIMIT $1`
	rows, err := tx.Query(ctx, query, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []domain.AlertMappingPolicy
	for rows.Next() {
		policy, err := scanMappingPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (r *AlertMappingRepo) DeletePolicy(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM alert_mapping_policies WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrMappingPolicyNotFound
	}
	return nil
}

func (r *AlertMappingRepo) UpsertResult(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) (domain.AlertMappingResult, error) {
	if result.AlertID == "" || !result.Status.IsValid() || result.ReasonCode == "" || result.Explanation == "" || result.EvaluatedAt.IsZero() {
		return result, fmt.Errorf("store: Alert Mapping 结果字段非法")
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO alert_mapping_results (
			alert_id, status, policy_id, entity_id, fault_id, reason_code,
			explanation, correlation_key, evaluated_at
		) VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid,
			NULLIF($5, '')::uuid, $6, $7, $8, $9)
		ON CONFLICT (alert_id) DO UPDATE SET
			status = EXCLUDED.status, policy_id = EXCLUDED.policy_id,
			entity_id = EXCLUDED.entity_id, fault_id = EXCLUDED.fault_id,
			reason_code = EXCLUDED.reason_code, explanation = EXCLUDED.explanation,
			correlation_key = EXCLUDED.correlation_key, evaluated_at = EXCLUDED.evaluated_at
		RETURNING alert_id::text, status, COALESCE(policy_id::text, ''),
			COALESCE(entity_id::text, ''), COALESCE(fault_id::text, ''), reason_code,
			explanation, correlation_key, evaluated_at, created_at, updated_at
	`, result.AlertID, result.Status, result.PolicyID, result.EntityID, result.FaultID,
		result.ReasonCode, result.Explanation, result.CorrelationKey, result.EvaluatedAt)
	if err := row.Scan(&result.AlertID, &result.Status, &result.PolicyID, &result.EntityID,
		&result.FaultID, &result.ReasonCode, &result.Explanation, &result.CorrelationKey,
		&result.EvaluatedAt, &result.CreatedAt, &result.UpdatedAt); err != nil {
		return result, err
	}
	return result, nil
}

const mappingResultSelect = `
	SELECT r.alert_id::text, r.status, COALESCE(r.policy_id::text, ''),
		COALESCE(p.name, ''), COALESCE(r.entity_id::text, ''), COALESCE(n.entity_key, ''),
		COALESCE(n.name, ''), COALESCE(r.fault_id::text, ''), r.reason_code,
		r.explanation, r.correlation_key, r.evaluated_at, r.created_at, r.updated_at
	FROM alert_mapping_results r
	LEFT JOIN alert_mapping_policies p ON p.id = r.policy_id
	LEFT JOIN resource_nodes n ON n.id = r.entity_id
`

func scanMappingResult(row configRowScanner) (domain.AlertMappingResult, error) {
	var result domain.AlertMappingResult
	err := row.Scan(&result.AlertID, &result.Status, &result.PolicyID, &result.PolicyName,
		&result.EntityID, &result.EntityKey, &result.EntityName, &result.FaultID,
		&result.ReasonCode, &result.Explanation, &result.CorrelationKey,
		&result.EvaluatedAt, &result.CreatedAt, &result.UpdatedAt)
	return result, err
}

func (r *AlertMappingRepo) GetResult(ctx context.Context, tx pgx.Tx, alertID string) (domain.AlertMappingResult, error) {
	result, err := scanMappingResult(tx.QueryRow(ctx, mappingResultSelect+` WHERE r.alert_id = $1`, alertID))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrMappingResultNotFound
	}
	return result, err
}

func (r *AlertMappingRepo) ListResults(ctx context.Context, tx pgx.Tx, status domain.AlertMappingStatus, limit int) ([]domain.AlertMappingResult, error) {
	query := mappingResultSelect
	args := []any{}
	if status != "" {
		query += ` WHERE r.status = $1`
		args = append(args, status)
	}
	query += fmt.Sprintf(` ORDER BY r.evaluated_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, clampLimit(limit))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []domain.AlertMappingResult
	for rows.Next() {
		result, err := scanMappingResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (r *AlertMappingRepo) Summary(ctx context.Context, tx pgx.Tx) (map[string]int, error) {
	rows, err := tx.Query(ctx, `SELECT status, count(*) FROM alert_mapping_results GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{"mapped": 0, "unmapped": 0, "conflict": 0, "ignored": 0}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		result[status] = count
	}
	return result, rows.Err()
}
