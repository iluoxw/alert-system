// Package correlation 将已提交 Alert 与拓扑投影关联成可解释的技术 Fault。
package correlation

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

const (
	E2BCapabilityCorrelationKey = "e2b:capability:sandbox-runtime:placement-degradation"
	E2BCapabilityResourceRef    = "e2b.sandbox-runtime"
)

type FaultProposal struct {
	Fault                 domain.Fault
	ResourceExternalRef   string
	Evidence              []domain.FaultEvidence
	ContributingFaultKeys []string
	ConfirmedImpactRefs   []string
}

type E2BAssessment struct {
	Faults            []FaultProposal
	IncidentSuggested bool
}

type evidenceRule struct {
	role       domain.EvidenceRole
	confidence float64
	rationale  string
}

var e2bEvidenceRules = map[string]evidenceRule{
	"object_missing": {
		role: domain.EvidenceRoleCauseCandidate, confidence: 0.9,
		rationale: "恢复对象缺失可解释特定 Sandbox 无法 resume，并可能触发无效重试",
	},
	"invalid_resume_retry": {
		role: domain.EvidenceRoleTrigger, confidence: 0.9,
		rationale: "持续 connect/auto-resume 触发跨节点放置重试",
	},
	"firecracker_gauge_drift": {
		role: domain.EvidenceRoleAmplifier, confidence: 0.82,
		rationale: "Firecracker 进程数高于 running gauge，支持残留实例放大资源压力",
	},
	"node_load_pressure": {
		role: domain.EvidenceRoleSupporting, confidence: 0.72,
		rationale: "节点负载和 HugePages 余量支持容量正在收紧的判断",
	},
	"node_exhausted": {
		role: domain.EvidenceRoleSymptom, confidence: 0.88,
		rationale: "Node exhausted 是共享放置池资源耗尽的下游症状",
	},
	"envd_timeout": {
		role: domain.EvidenceRoleSymptom, confidence: 0.82,
		rationale: "envd readiness timeout 表明 Sandbox 生命周期无法按时完成",
	},
	"sandbox_create_slo_breach": {
		role: domain.EvidenceRoleDirectImpact, confidence: 1,
		rationale: "用户级 create 请求失败率超过 SLO，直接证明放置能力已退化",
	},
	"lifecycle_probe_failure": {
		role: domain.EvidenceRoleDirectImpact, confidence: 1,
		rationale: "create→pause→resume 生命周期探针失败，直接证明核心能力受损",
	},
	"f5_stream_stall": {
		role: domain.EvidenceRoleExcluded, confidence: 1,
		rationale: "F5 单向流式 stall 属于独立因果链，不解释 placement 资源耗尽",
	},
}

// AssessE2BFailedToPlace 同时生成实例 Fault 和共享能力 Fault。升级只取决于是否存在
// direct_impact 证据，不读取 attempt_count，也不会用重复告警数量投票。
func AssessE2BFailedToPlace(alerts []domain.Alert, now time.Time) E2BAssessment {
	active := make([]domain.Alert, 0, len(alerts))
	for _, alert := range alerts {
		if alert.Status == domain.StatusResolved {
			continue
		}
		if _, known := e2bEvidenceRules[alert.Labels["e2b.evidence_class"]]; known {
			active = append(active, alert)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].StartsAt.Equal(active[j].StartsAt) {
			return active[i].ID < active[j].ID
		}
		return active[i].StartsAt.Before(active[j].StartsAt)
	})

	instanceBySID := make(map[string][]domain.Alert)
	for _, alert := range active {
		if alert.Labels["e2b.evidence_class"] != "object_missing" {
			continue
		}
		if sid := alert.Labels["sandbox_id_hash"]; sid != "" {
			instanceBySID[sid] = append(instanceBySID[sid], alert)
		}
	}

	assessment := E2BAssessment{}
	instanceKeys := make([]string, 0, len(instanceBySID))
	for sid := range instanceBySID {
		instanceKeys = append(instanceKeys, sid)
	}
	sort.Strings(instanceKeys)
	for _, sid := range instanceKeys {
		group := instanceBySID[sid]
		first := earliestAlertTime(group, now)
		key := "e2b:instance:sandbox:" + sid + ":resume-unrecoverable"
		evidence := make([]domain.FaultEvidence, 0, len(group))
		for _, alert := range group {
			evidence = append(evidence, domain.FaultEvidence{
				AlertID: alert.ID, Role: domain.EvidenceRoleDirectImpact,
				Rationale:  "恢复对象已不存在，直接证明该脱敏 SID 无法按当前快照恢复",
				Confidence: 0.98, DecisionOrigin: domain.DecisionOriginAutomation,
			})
		}
		assessment.Faults = append(assessment.Faults, FaultProposal{
			Fault: domain.Fault{
				CorrelationKey: key, Scope: domain.FaultScopeInstance, Status: domain.FaultStatusActive,
				Severity: domain.SeverityWarning, Kind: "sandbox_resume_unrecoverable",
				Title: "Sandbox 实例无法恢复", Summary: "脱敏 SID " + sid + " 的恢复对象缺失",
				EntityRef: "sandbox:" + sid, Confidence: 0.98,
				Metadata: map[string]any{"sandbox_id_hash": sid}, FirstDetected: first, LastEvaluated: now,
			},
			ResourceExternalRef: E2BCapabilityResourceRef,
			Evidence:            evidence,
		})
	}

	var capabilityEvidence []domain.FaultEvidence
	classes := map[string]struct{}{}
	directImpact := false
	confirmedRefs := map[string]struct{}{}
	firstDetected := now
	for _, alert := range active {
		class := alert.Labels["e2b.evidence_class"]
		rule := e2bEvidenceRules[class]
		capabilityEvidence = append(capabilityEvidence, domain.FaultEvidence{
			AlertID: alert.ID, Role: rule.role, Rationale: rule.rationale,
			Confidence: rule.confidence, DecisionOrigin: domain.DecisionOriginAutomation,
		})
		if rule.role != domain.EvidenceRoleExcluded {
			classes[class] = struct{}{}
			if alert.StartsAt.Before(firstDetected) {
				firstDetected = alert.StartsAt
			}
		}
		if rule.role == domain.EvidenceRoleDirectImpact {
			directImpact = true
			if ref := alert.Labels["affected_business_system"]; ref != "" {
				confirmedRefs[ref] = struct{}{}
			}
		}
	}

	// 只有 excluded 证据不能凭空建立 E2B capability Fault。
	if len(classes) == 0 {
		return assessment
	}
	status := domain.FaultStatusCandidate
	severity := domain.SeverityWarning
	confidence := candidateConfidence(classes)
	summary := "底层恢复失败与资源压力正在放大，但尚无用户级 SLO 失守证据"
	if directImpact {
		status = domain.FaultStatusActive
		severity = domain.SeverityCritical
		confidence = 0.95
		if hasCausalSupport(classes) {
			confidence = 0.99
		}
		summary = "用户级 create/resume SLO 已受损，且存在可解释的恢复失败与资源耗尽证据链"
	}
	confirmed := make([]string, 0, len(confirmedRefs))
	for ref := range confirmedRefs {
		confirmed = append(confirmed, ref)
	}
	sort.Strings(confirmed)
	contributing := make([]string, 0, len(instanceKeys))
	for _, sid := range instanceKeys {
		contributing = append(contributing, "e2b:instance:sandbox:"+sid+":resume-unrecoverable")
	}
	assessment.Faults = append(assessment.Faults, FaultProposal{
		Fault: domain.Fault{
			CorrelationKey: E2BCapabilityCorrelationKey,
			Scope:          domain.FaultScopeCapability, Status: status, Severity: severity,
			Kind: "sandbox_placement_degradation", Title: "E2B Sandbox Placement 能力退化",
			Summary: summary, EntityRef: "capability:" + E2BCapabilityResourceRef, Confidence: confidence,
			Metadata: map[string]any{
				"correlation_rule": "e2b_failed_to_place_v1",
				"direct_impact":    directImpact,
			},
			FirstDetected: firstDetected, LastEvaluated: now,
		},
		ResourceExternalRef:   E2BCapabilityResourceRef,
		Evidence:              capabilityEvidence,
		ContributingFaultKeys: contributing,
		ConfirmedImpactRefs:   confirmed,
	})
	assessment.IncidentSuggested = directImpact
	return assessment
}

func earliestAlertTime(alerts []domain.Alert, fallback time.Time) time.Time {
	result := fallback
	for _, alert := range alerts {
		if !alert.StartsAt.IsZero() && alert.StartsAt.Before(result) {
			result = alert.StartsAt
		}
	}
	return result
}

func candidateConfidence(classes map[string]struct{}) float64 {
	// 每种独立证据类型最多贡献一次，避免 attempt 重试或重复采集把置信度刷高。
	weights := map[string]float64{
		"object_missing": 0.12, "invalid_resume_retry": 0.12,
		"firecracker_gauge_drift": 0.12, "node_load_pressure": 0.07,
		"node_exhausted": 0.07, "envd_timeout": 0.07,
	}
	confidence := 0.35
	for class := range classes {
		confidence += weights[class]
	}
	return math.Min(confidence, 0.85)
}

func hasCausalSupport(classes map[string]struct{}) bool {
	for _, class := range []string{"object_missing", "invalid_resume_retry", "firecracker_gauge_drift"} {
		if _, ok := classes[class]; ok {
			return true
		}
	}
	return false
}

type Evaluator struct {
	topology *store.TopologyRepo
	faults   *store.FaultRepo
}

func NewEvaluator(topology *store.TopologyRepo, faults *store.FaultRepo) *Evaluator {
	return &Evaluator{topology: topology, faults: faults}
}

// Persist 在一个数据库事务内保存 Fault、自动证据、contributes_to 关系与爆炸半径。
// 调用方应在 Alert 管道提交后异步调用，避免延长 ingest 的持锁事务。
func (e *Evaluator) Persist(ctx context.Context, tx pgx.Tx, topologySource string, assessment E2BAssessment) ([]domain.Fault, error) {
	stored := make([]domain.Fault, 0, len(assessment.Faults))
	byKey := make(map[string]domain.Fault, len(assessment.Faults))
	proposalByKey := make(map[string]FaultProposal, len(assessment.Faults))
	for _, proposal := range assessment.Faults {
		node, err := e.topology.GetByExternalRef(ctx, tx, topologySource, proposal.ResourceExternalRef)
		if err != nil {
			return nil, fmt.Errorf("correlation: 解析 Fault 资源 %s 失败: %w", proposal.ResourceExternalRef, err)
		}
		proposal.Fault.ResourceNodeID = node.ID
		fault, _, err := e.faults.Upsert(ctx, tx, proposal.Fault)
		if err != nil {
			return nil, fmt.Errorf("correlation: 保存 Fault %s 失败: %w", proposal.Fault.CorrelationKey, err)
		}
		if err := e.faults.ReplaceAutomatedEvidence(ctx, tx, fault.ID, proposal.Evidence); err != nil {
			return nil, fmt.Errorf("correlation: 保存 Fault 证据失败: %w", err)
		}
		stored = append(stored, fault)
		byKey[fault.CorrelationKey] = fault
		proposalByKey[fault.CorrelationKey] = proposal
	}

	for key, fault := range byKey {
		proposal := proposalByKey[key]
		for _, childKey := range proposal.ContributingFaultKeys {
			child, ok := byKey[childKey]
			if !ok {
				continue
			}
			if err := e.faults.UpsertRelation(ctx, tx, domain.FaultRelation{
				FromFaultID: child.ID, ToFaultID: fault.ID, Type: domain.FaultRelationContributesTo,
				Rationale:  "多个实例恢复失败及其重试共同消耗共享 Orchestrator 容量",
				Confidence: 0.86,
			}); err != nil {
				return nil, err
			}
		}
		if fault.Scope != domain.FaultScopeCapability {
			continue
		}
		radius, err := e.topology.ListBlastRadius(ctx, tx, topologySource, proposal.ResourceExternalRef, 8)
		if err != nil {
			return nil, fmt.Errorf("correlation: 计算爆炸半径失败: %w", err)
		}
		confirmed := make(map[string]struct{}, len(proposal.ConfirmedImpactRefs))
		for _, ref := range proposal.ConfirmedImpactRefs {
			confirmed[ref] = struct{}{}
		}
		impacts := make([]domain.FaultImpact, 0, len(radius))
		for _, item := range radius {
			state := domain.ImpactStatePotential
			if fault.Status == domain.FaultStatusActive && item.Depth == 0 {
				state = domain.ImpactStateConfirmed
			}
			if _, ok := confirmed[item.Node.ExternalRef]; ok {
				state = domain.ImpactStateConfirmed
			}
			confidence := math.Max(0.3, fault.Confidence-float64(item.Depth)*0.08)
			impacts = append(impacts, domain.FaultImpact{
				NodeID: item.Node.ID, State: state, Path: item.Path,
				Rationale:  fmt.Sprintf("%s 通过 %d 跳依赖路径受到影响", item.Node.Name, item.Depth),
				Confidence: confidence, DecisionOrigin: domain.DecisionOriginAutomation,
			})
		}
		if err := e.faults.ReplaceAutomatedImpacts(ctx, tx, fault.ID, impacts); err != nil {
			return nil, fmt.Errorf("correlation: 保存爆炸半径失败: %w", err)
		}
	}
	return stored, nil
}
