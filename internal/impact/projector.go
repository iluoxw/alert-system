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
	defaultMaxDepth   = 8
	defaultStaleAfter = 24 * time.Hour
)

type Projector struct {
	topology   *store.TopologyRepo
	faults     *store.FaultRepo
	maxDepth   int
	staleAfter time.Duration
}

func NewProjector(topology *store.TopologyRepo, faults *store.FaultRepo) *Projector {
	return &Projector{topology: topology, faults: faults, maxDepth: defaultMaxDepth, staleAfter: defaultStaleAfter}
}

func isApplication(node domain.ResourceNode) bool {
	return node.Layer == domain.EntityLayerApplication || node.Kind == domain.ResourceNodeKindBusinessSystem
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
		state := domain.ImpactStatePotential
		confidence := 0.65
		rationale := fmt.Sprintf("依赖图反向可达（%d 跳），尚无直接业务退化证据", reached.Depth)
		if reached.Stale {
			confidence = 0.4
			rationale = fmt.Sprintf("依赖图反向可达（%d 跳），但 observed 关系已过期", reached.Depth)
		} else if hasDirect {
			state = domain.ImpactStateConfirmed
			confidence = 0.95
			if reached.Node.ID == fault.ResourceNodeID {
				rationale = "应用 direct_impact Alert 直接确认自身受影响"
			} else {
				rationale = fmt.Sprintf("依赖图反向可达（%d 跳），且应用 direct_impact Alert 与 Fault 时间窗重叠", reached.Depth)
			}
		}
		impacts = append(impacts, domain.FaultImpact{
			NodeID: reached.Node.ID, State: state, Path: reached.Path,
			Rationale: rationale, Confidence: confidence, DecisionOrigin: domain.DecisionOriginAutomation,
		})
	}
	return p.faults.ReplaceAutomatedImpacts(ctx, tx, fault.ID, impacts)
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
