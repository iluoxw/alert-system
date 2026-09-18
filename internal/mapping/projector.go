// Package mapping implements the generic, deterministic Alert -> Entity -> Fault
// projection. Business semantics live in data, not in core branches.
package mapping

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

const (
	standardEntityKeyLabel = "sre.entity.key"
	standardFaultKindLabel = "sre.fault.kind"
	standardEvidenceLabel  = "sre.evidence.role"
)

type Projector struct {
	policies *store.AlertMappingRepo
	catalog  *store.ServiceCatalogRepo
	topology *store.TopologyRepo
	faults   *store.FaultRepo
	impacts  ImpactProjector
	now      func() time.Time
}

type ImpactProjector interface {
	RebuildFault(ctx context.Context, tx pgx.Tx, fault domain.Fault) error
	RebuildUpstreamForConsumer(ctx context.Context, tx pgx.Tx, consumerNodeID string) error
}

func NewProjector(policies *store.AlertMappingRepo, catalog *store.ServiceCatalogRepo, topology *store.TopologyRepo, faults *store.FaultRepo, impacts ImpactProjector) *Projector {
	return &Projector{policies: policies, catalog: catalog, topology: topology, faults: faults, impacts: impacts, now: time.Now}
}

type resolvedContract struct {
	Policy            *domain.AlertMappingPolicy
	Entity            domain.ResourceNode
	FaultKind         string
	FaultScope        domain.FaultScope
	EvidenceRole      domain.EvidenceRole
	CorrelationLabels []string
	Confidence        float64
}

func alertLabelView(alert domain.Alert) map[string]string {
	view := make(map[string]string, len(alert.Labels)+len(alert.ResourceRef)+3)
	for key, value := range alert.ResourceRef {
		view[key] = value
	}
	for key, value := range alert.Labels {
		view[key] = value
	}
	view["severity"] = string(alert.Severity)
	view["source"] = alert.Source
	return view
}

// Project evaluates and persists one Alert in the caller's transaction. It is
// safe to replay: mapping result, Fault, evidence and impact all use upsert keys.
func (p *Projector) Project(ctx context.Context, tx pgx.Tx, alert domain.Alert) (domain.AlertMappingResult, error) {
	result := domain.AlertMappingResult{AlertID: alert.ID, EvaluatedAt: p.now()}
	contract, status, reason, explanation, err := p.resolveContract(ctx, tx, alert)
	if err != nil {
		return result, err
	}
	if status != domain.AlertMappingMapped {
		result.Status, result.ReasonCode, result.Explanation = status, reason, explanation
		if err := p.faults.RemoveAutomatedEvidenceForAlertExcept(ctx, tx, alert.ID, ""); err != nil {
			return result, err
		}
		return p.policies.UpsertResult(ctx, tx, result)
	}

	correlationKey := buildCorrelationKey(contract.Entity.EntityKey, contract.FaultKind, contract.CorrelationLabels, alertLabelView(alert))
	faultStatus := domain.FaultStatusCandidate
	if contract.EvidenceRole == domain.EvidenceRoleDirectImpact {
		faultStatus = domain.FaultStatusActive
	}
	if isResolved(alert) {
		faultStatus = domain.FaultStatusMonitoring
	}
	firstDetected := alert.StartsAt
	if firstDetected.IsZero() {
		firstDetected = result.EvaluatedAt
	}

	// 转移判定需要 Upsert 之前的状态：Upsert 的 (xmax=0) 只能告诉我们"是否新建"，
	// 判断"candidate→active"还需要旧状态。GetByCorrelationKey 用 QueryRow（单行，
	// 非流式 Rows），Scan 后连接即释放，与随后的 Upsert 不冲突（不变量 20）。
	previous, err := p.faults.GetByCorrelationKey(ctx, tx, correlationKey)
	if err != nil && !errors.Is(err, store.ErrFaultNotFound) {
		return result, err
	}

	fault, created, err := p.faults.Upsert(ctx, tx, buildFaultUpsertInput(contract, alert, correlationKey, faultStatus, firstDetected, result.EvaluatedAt))
	if err != nil {
		return result, err
	}
	result.Transition = determineFaultTransition(created, previous.Status, fault.Status)
	rationale := fmt.Sprintf("Alert 命中 %s，作为 %s 证据映射到 %s", policyDisplay(contract.Policy), contract.EvidenceRole, contract.Entity.EntityKey)
	if err := p.faults.UpsertAutomatedEvidence(ctx, tx, fault.ID, domain.FaultEvidence{
		AlertID: alert.ID, Role: contract.EvidenceRole, Rationale: rationale,
		Confidence: contract.Confidence, DecisionOrigin: domain.DecisionOriginAutomation,
	}); err != nil {
		return result, err
	}
	if err := p.faults.RemoveAutomatedEvidenceForAlertExcept(ctx, tx, alert.ID, fault.ID); err != nil {
		return result, err
	}

	if p.impacts != nil {
		if contract.Entity.Layer == domain.EntityLayerApplication {
			if err := p.impacts.RebuildUpstreamForConsumer(ctx, tx, contract.Entity.ID); err != nil {
				return result, err
			}
		} else if err := p.impacts.RebuildFault(ctx, tx, fault); err != nil {
			return result, err
		}
	}

	result.Status = domain.AlertMappingMapped
	result.PolicyID = policyID(contract.Policy)
	result.EntityID = contract.Entity.ID
	result.FaultID = fault.ID
	result.ReasonCode = "mapped"
	result.Explanation = rationale
	result.CorrelationKey = correlationKey
	return p.policies.UpsertResult(ctx, tx, result)
}

func (p *Projector) resolveContract(ctx context.Context, tx pgx.Tx, alert domain.Alert) (resolvedContract, domain.AlertMappingStatus, string, string, error) {
	if !isAuthoritativeAlertmanager(alert) {
		return resolvedContract{}, domain.AlertMappingUnmapped, "unsupported_source",
			"Alert Mapping 仅接受已登记 Alertmanager occurrence；兼容来源只保留 Alert", nil
	}
	view := alertLabelView(alert)
	policies, err := p.policies.ListPolicies(ctx, tx, true, 500)
	if err != nil {
		return resolvedContract{}, "", "", "", err
	}
	for i := range policies {
		policy := policies[i]
		if policy.SourceIntegrationID != "" && policy.SourceIntegrationID != alert.SourceIntegrationID {
			continue
		}
		if !policy.Matchers.Matches(view) {
			continue
		}
		entity, status, reason, explanation, err := p.resolvePolicyEntity(ctx, tx, policy, view)
		if err != nil || status != domain.AlertMappingMapped {
			return resolvedContract{}, status, reason, explanation, err
		}
		return resolvedContract{
			Policy: &policy, Entity: entity, FaultKind: policy.FaultKind,
			FaultScope: policy.FaultScope, EvidenceRole: policy.EvidenceRole,
			CorrelationLabels: policy.CorrelationLabels, Confidence: policy.Confidence,
		}, domain.AlertMappingMapped, "mapped", "策略命中", nil
	}

	entityKey := strings.TrimSpace(view[standardEntityKeyLabel])
	faultKind := strings.TrimSpace(view[standardFaultKindLabel])
	role := domain.EvidenceRole(strings.TrimSpace(view[standardEvidenceLabel]))
	if entityKey == "" || faultKind == "" || !role.IsValid() {
		return resolvedContract{}, domain.AlertMappingUnmapped, "no_policy",
			"没有已发布策略命中，且缺少完整的 sre.entity.key / sre.fault.kind / sre.evidence.role 标准标签", nil
	}
	nodes, err := p.catalog.ResolveEntityKey(ctx, tx, entityKey)
	if err != nil {
		return resolvedContract{}, "", "", "", err
	}
	if len(nodes) == 0 {
		return resolvedContract{}, domain.AlertMappingUnmapped, "entity_key_not_found", "标准 entity key 尚未登记", nil
	}
	if len(nodes) > 1 {
		return resolvedContract{}, domain.AlertMappingConflict, "entity_key_conflict", "标准 entity key 指向多个实体", nil
	}
	scope := domain.FaultScope(view["sre.fault.scope"])
	if !scope.IsValid() {
		scope = domain.FaultScopeCapability
	}
	correlationLabels := splitCSV(view["sre.correlation.labels"])
	return resolvedContract{Entity: nodes[0], FaultKind: faultKind, FaultScope: scope,
			EvidenceRole: role, CorrelationLabels: correlationLabels, Confidence: 1},
		domain.AlertMappingMapped, "mapped_standard_contract", "受信标准标签契约", nil
}

// buildFaultUpsertInput 组装待 Upsert 的 Fault：OwnerTeamID 只取
// contract.Entity.OwnerTeamID（即 resource_nodes.owner_team_id，由 catalog/topology
// 仓储在实体登记时权威写入），解析不到就是空字符串，交给 store 层 NULLIF 转 NULL。
// 禁止依据 Alert 标签或平台数据推断 owner（不变量 15，R3.3）——本函数因此不读取
// alert.Labels/ResourceRef 中的任何字段来决定 OwnerTeamID。
// 纯函数：不做 I/O，便于覆盖"有 owner / 无 owner"两种情形（R3.2）。
func buildFaultUpsertInput(contract resolvedContract, alert domain.Alert, correlationKey string, status domain.FaultStatus, firstDetected, evaluatedAt time.Time) domain.Fault {
	return domain.Fault{
		CorrelationKey: correlationKey,
		Scope:          contract.FaultScope,
		Status:         status,
		Severity:       alert.Severity,
		Kind:           contract.FaultKind,
		Title:          contract.Entity.Name + " · " + humanize(contract.FaultKind),
		Summary:        "由确定性 Alert Mapping 投影：" + alert.Title,
		ResourceNodeID: contract.Entity.ID,
		EntityRef:      contract.Entity.EntityKey,
		OwnerTeamID:    contract.Entity.OwnerTeamID,
		Confidence:     contract.Confidence,
		Metadata: map[string]any{
			"mapping_policy_id": policyID(contract.Policy),
			"mapping_version":   policyVersion(contract.Policy),
			"source":            "alert_mapping",
		},
		FirstDetected: firstDetected,
		LastEvaluated: evaluatedAt,
	}
}

// determineFaultTransition 判定本次 Upsert 是否让 Fault 进入 active（R5.1/R5.10），
// 只关心这一件事：
//   - created==true 且新状态是 active → 直接以 active 创建（R5.10 direct firing）；
//   - created==false 且旧状态不是 active、新状态是 active → 任何"非 active → active"
//     的转移都算，包括 candidate 升级为 active，也包括 monitoring 复发重新升级为
//     active（Upsert 的 ON CONFLICT 允许这条路径：观察期内故障复发）；
//   - 其余情况（含旧状态已是 active 的重复评估）均为 None，避免同一次 active 转移
//     被多个调用入口（pipeline/对账两处）并发触发时重复产生通知意图（R5.11）。
//
// previous 为空值 FaultStatus("") 表示 correlation_key 此前不存在（配合 created
// 使用；GetByCorrelationKey 命中 ErrFaultNotFound 时调用方就传零值）。
// 纯函数：不做 I/O，便于表驱动测试覆盖 created / candidate→active / monitoring→active /
// 已 active 不重复触发四种情形。
func determineFaultTransition(created bool, previous, next domain.FaultStatus) domain.FaultTransition {
	if next != domain.FaultStatusActive {
		return domain.FaultTransitionNone
	}
	if created {
		return domain.FaultTransitionCreatedActive
	}
	if previous == domain.FaultStatusActive {
		return domain.FaultTransitionNone
	}
	return domain.FaultTransitionEnteredActive
}

func isAuthoritativeAlertmanager(alert domain.Alert) bool {
	return domain.IsAuthoritativeSourceKind(alert.Source) && strings.TrimSpace(alert.SourceIntegrationID) != "" && strings.TrimSpace(alert.UpstreamFingerprint) != ""
}

func (p *Projector) resolvePolicyEntity(ctx context.Context, tx pgx.Tx, policy domain.AlertMappingPolicy, labels map[string]string) (domain.ResourceNode, domain.AlertMappingStatus, string, string, error) {
	if policy.TargetEntityID != "" {
		node, err := p.topology.GetByID(ctx, tx, policy.TargetEntityID)
		if errors.Is(err, store.ErrResourceNodeNotFound) {
			return node, domain.AlertMappingUnmapped, "target_entity_not_found", "策略目标实体不存在", nil
		}
		return node, domain.AlertMappingMapped, "mapped", "固定目标实体", err
	}
	value := strings.TrimSpace(labels[policy.AliasLabel])
	if value == "" {
		return domain.ResourceNode{}, domain.AlertMappingUnmapped, "alias_label_missing",
			fmt.Sprintf("Alert 缺少 alias label %s", policy.AliasLabel), nil
	}
	nodes, err := p.catalog.ResolveAlias(ctx, tx, policy.AliasNamespace, value)
	if err != nil {
		return domain.ResourceNode{}, "", "", "", err
	}
	if len(nodes) == 0 {
		return domain.ResourceNode{}, domain.AlertMappingUnmapped, "alias_not_found",
			fmt.Sprintf("Alias %s=%s 尚未治理", policy.AliasNamespace, value), nil
	}
	if len(nodes) > 1 {
		return domain.ResourceNode{}, domain.AlertMappingConflict, "alias_conflict",
			fmt.Sprintf("Alias %s=%s 被 %d 个实体同时声明", policy.AliasNamespace, value, len(nodes)), nil
	}
	return nodes[0], domain.AlertMappingMapped, "mapped", "Alias 唯一解析", nil
}

func isResolved(alert domain.Alert) bool {
	if alert.UpstreamFingerprint != "" {
		return alert.SourceState == domain.SourceStateResolved
	}
	return alert.Status == domain.StatusResolved
}

func buildCorrelationKey(entityKey, faultKind string, correlationLabels []string, labels map[string]string) string {
	keys := append([]string(nil), correlationLabels...)
	sort.Strings(keys)
	parts := []string{entityKey, faultKind}
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "fault:" + entityKey + ":" + faultKind + ":" + hex.EncodeToString(sum[:6])
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func humanize(value string) string { return strings.ReplaceAll(value, "_", " ") }

func policyID(policy *domain.AlertMappingPolicy) string {
	if policy == nil {
		return ""
	}
	return policy.ID
}

func policyVersion(policy *domain.AlertMappingPolicy) int {
	if policy == nil {
		return 1
	}
	return policy.Version
}

func policyDisplay(policy *domain.AlertMappingPolicy) string {
	if policy == nil {
		return "标准标签契约"
	}
	return "策略「" + policy.Name + "」v" + fmt.Sprint(policy.Version)
}
