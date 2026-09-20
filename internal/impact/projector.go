// Package impact projects canonical EntityRelation paths into conservative
// application Impact. Graph reachability alone is never enough to confirm an
// outage; direct application evidence or a human decision is required.
package impact

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

const (
	defaultMaxDepth                = 8
	defaultStaleAfter              = 24 * time.Hour
	decommissionedImpactRationale  = "实体已下线"
	decommissionedImpactConfidence = 0.2
)

type topologyView interface {
	ListBlastRadiusByNodeID(ctx context.Context, tx pgx.Tx, nodeID string, maxDepth int, staleAfter time.Duration) ([]domain.BlastRadiusNode, error)
	ListDependencyProvidersByNodeID(ctx context.Context, tx pgx.Tx, nodeID string, maxDepth int, staleAfter time.Duration) ([]domain.BlastRadiusNode, error)
}

type faultImpactStore interface {
	HasActiveDirectEvidenceForFault(ctx context.Context, tx pgx.Tx, faultID string, since time.Time) (bool, error)
	HasActiveDirectEvidenceForNode(ctx context.Context, tx pgx.Tx, nodeID string, since time.Time) (bool, error)
	ReplaceAutomatedImpacts(ctx context.Context, tx pgx.Tx, faultID string, impacts []domain.FaultImpact) error
	UpsertAutomatedImpact(ctx context.Context, tx pgx.Tx, faultID string, impact domain.FaultImpact) error
	ListForNode(ctx context.Context, tx pgx.Tx, nodeID string, limit int) ([]domain.Fault, error)
	ListOpenForNodeIDs(ctx context.Context, tx pgx.Tx, nodeIDs []string, limit int) ([]domain.Fault, error)
}

type Projector struct {
	topology   topologyView
	faults     faultImpactStore
	maxDepth   int
	staleAfter time.Duration
}

func NewProjector(topology *store.TopologyRepo, faults *store.FaultRepo) *Projector {
	return &Projector{topology: topology, faults: faults, maxDepth: defaultMaxDepth, staleAfter: defaultStaleAfter}
}

func isApplication(node domain.ResourceNode) bool {
	return node.Layer == domain.EntityLayerApplication || node.Kind == domain.ResourceNodeKindBusinessSystem
}

// classifyAutomatedImpact 只产出自动化结论。下线实体优先于 stale / 直接证据，
// 一律 ruled_out；人工 confirmed / 人工 ruled_out 不走这里，由 ReplaceAutomatedImpacts
// 按 decision_origin='human' 拒绝覆盖（不变量 16）。
func classifyAutomatedImpact(node domain.ResourceNode, depth int, stale, hasDirect bool) (domain.ImpactState, string, float64) {
	if node.LifecycleState == domain.EntityLifecycleDecommissioned {
		return domain.ImpactStateRuledOut, decommissionedImpactRationale, decommissionedImpactConfidence
	}
	if stale {
		return domain.ImpactStatePotential,
			fmt.Sprintf("依赖图反向可达（%d 跳），但 observed 关系已过期", depth), 0.4
	}
	if hasDirect {
		if depth == 0 {
			return domain.ImpactStateConfirmed, "应用 direct_impact Alert 直接确认自身受影响", 0.95
		}
		return domain.ImpactStateConfirmed,
			fmt.Sprintf("依赖图反向可达（%d 跳），且应用 direct_impact Alert 与 Fault 时间窗重叠", depth), 0.95
	}
	return domain.ImpactStatePotential,
		fmt.Sprintf("依赖图反向可达（%d 跳），尚无直接业务退化证据", depth), 0.65
}

func decommissionedAutomatedImpact(nodeID string) domain.FaultImpact {
	return domain.FaultImpact{
		NodeID:         nodeID,
		State:          domain.ImpactStateRuledOut,
		Path:           []string{nodeID},
		Rationale:      decommissionedImpactRationale,
		Confidence:     decommissionedImpactConfidence,
		DecisionOrigin: domain.DecisionOriginAutomation,
	}
}

// RebuildFault replaces only automated Impact rows. Human decisions remain
// authoritative because FaultRepo rejects automated overwrites of human rows.
func (p *Projector) RebuildFault(ctx context.Context, tx pgx.Tx, fault domain.Fault) error {
	if fault.ID == "" || fault.ResourceNodeID == "" {
		return nil
	}
	radius, err := p.topology.ListBlastRadiusByNodeID(ctx, tx, fault.ResourceNodeID, p.maxDepth, p.staleAfter)
	if err != nil {
		return err
	}
	impacts := make([]domain.FaultImpact, 0, len(radius))
	for _, reached := range radius {
		if !isApplication(reached.Node) {
			continue
		}
		var hasDirect bool
		if reached.Node.ID == fault.ResourceNodeID {
			hasDirect, err = p.faults.HasActiveDirectEvidenceForFault(ctx, tx, fault.ID, fault.FirstDetected)
		} else {
			hasDirect, err = p.faults.HasActiveDirectEvidenceForNode(ctx, tx, reached.Node.ID, fault.FirstDetected)
		}
		if err != nil {
			return err
		}
		state, rationale, confidence := classifyAutomatedImpact(reached.Node, reached.Depth, reached.Stale, hasDirect)
		impacts = append(impacts, domain.FaultImpact{
			NodeID: reached.Node.ID, State: state, Path: reached.Path,
			Rationale: rationale, Confidence: confidence, DecisionOrigin: domain.DecisionOriginAutomation,
		})
	}
	return p.faults.ReplaceAutomatedImpacts(ctx, tx, fault.ID, impacts)
}

// RuleOutDecommissionedImpacts 在实体下线后重算关联 Fault，并把该节点上的自动
// potential 写成 ruled_out。遍历从下线根出发只含自身，其他 Fault 的半径不再含它，
// 因此 Rebuild 之后再 Upsert 一条自动化 ruled_out，避免影响行被静默删掉。
// UpsertAutomatedImpact 的 WHERE decision_origin <> 'human' 保住人工裁决。
func (p *Projector) RuleOutDecommissionedImpacts(ctx context.Context, tx pgx.Tx, node domain.ResourceNode) error {
	if node.ID == "" || node.LifecycleState != domain.EntityLifecycleDecommissioned {
		return nil
	}
	related, err := p.faults.ListForNode(ctx, tx, node.ID, 100)
	if err != nil {
		return err
	}
	for _, fault := range related {
		if err := p.RebuildFault(ctx, tx, fault); err != nil {
			return fmt.Errorf("impact: 下线后重算 Fault %s 失败: %w", fault.ID, err)
		}
		if !isApplication(node) {
			continue
		}
		if err := p.faults.UpsertAutomatedImpact(ctx, tx, fault.ID, decommissionedAutomatedImpact(node.ID)); err != nil {
			return fmt.Errorf("impact: 下线降级 Fault %s 失败: %w", fault.ID, err)
		}
	}
	return nil
}

// RebuildRelatedToNode 恢复实体后重算关联 Fault 的自动 Impact，不再写入 ruled_out。
func (p *Projector) RebuildRelatedToNode(ctx context.Context, tx pgx.Tx, nodeID string) error {
	related, err := p.faults.ListForNode(ctx, tx, nodeID, 100)
	if err != nil {
		return err
	}
	for _, fault := range related {
		if err := p.RebuildFault(ctx, tx, fault); err != nil {
			return fmt.Errorf("impact: 恢复后重算 Fault %s 失败: %w", fault.ID, err)
		}
	}
	return nil
}

func (p *Projector) ProviderNodeIDs(ctx context.Context, tx pgx.Tx, consumerNodeID string) ([]string, error) {
	providers, err := p.topology.ListDependencyProvidersByNodeID(ctx, tx, consumerNodeID, p.maxDepth, p.staleAfter)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.Node.ID)
	}
	return ids, nil
}

func (p *Projector) RebuildForProviderNodeIDs(ctx context.Context, tx pgx.Tx, providerNodeIDs []string) error {
	seen := make(map[string]struct{}, len(providerNodeIDs))
	unique := make([]string, 0, len(providerNodeIDs))
	for _, id := range providerNodeIDs {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	faults, err := p.faults.ListOpenForNodeIDs(ctx, tx, unique, 500)
	if err != nil {
		return err
	}
	for _, fault := range faults {
		if err := p.RebuildFault(ctx, tx, fault); err != nil {
			return fmt.Errorf("impact: 重算 Fault %s 失败: %w", fault.ID, err)
		}
	}
	return nil
}

func (p *Projector) RebuildUpstreamForConsumer(ctx context.Context, tx pgx.Tx, consumerNodeID string) error {
	providerIDs, err := p.ProviderNodeIDs(ctx, tx, consumerNodeID)
	if err != nil {
		return err
	}
	return p.RebuildForProviderNodeIDs(ctx, tx, providerIDs)
}
