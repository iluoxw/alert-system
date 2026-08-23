package alertsystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/store"
)

// TopologyAdapter 实现 serviceobs.TopologyReader：从 service entity 与 Profile roots
// 构造 bounded canonical neighborhood。
type TopologyAdapter struct {
	runner ReadRunner
	limits Limits
	clock  Clock
}

// NewTopologyAdapter 构造 topology adapter。
func NewTopologyAdapter(runner ReadRunner, limits Limits, clock Clock) *TopologyAdapter {
	return &TopologyAdapter{runner: runner, limits: limits.withDefaults(), clock: clock}
}

var _ serviceobs.TopologyReader = (*TopologyAdapter)(nil)

// ReadTopology 返回 bounded canonical neighborhood。
//
// 三条口径：
//
//  1. seed 是 service entity 加 Profile roots。没有 Profile 时 seed 只有 service entity，
//     再叠加 registration declared dependencies，因此"无 Profile 也有基础 declared 概览"
//     不依赖 Profile 是否存在。
//  2. 关系方向统一 consumer/caller -> provider/callee，并原样保留 declared provenance、
//     criticality 与 observed_at。observed 关系过期只标 stale，declared 底图不消失。
//  3. 达到 max nodes/relations 上限时显式 Partial + 稳定 reason，绝不静默截断。
func (a *TopologyAdapter) ReadTopology(
	ctx context.Context, req serviceobs.TopologyRequest,
) (serviceobs.TopologyFacts, error) {
	seeds := []string{req.ServiceEntityID}
	seeds = append(seeds, req.RootEntityIDs...)
	// declared 依赖的目标也是 seed：它们是服务负责人明确登记的结构依赖，即使没有
	// Profile 也应该出现在基础概览里。
	for _, dependency := range req.DeclaredDependencies {
		seeds = append(seeds, dependency.ToEntityID)
	}

	query := neighborhoodQuery(seeds, req, a.limits)
	facts := serviceobs.TopologyFacts{
		Nodes:     []serviceobs.TopologyNode{},
		Relations: []serviceobs.TopologyRelation{},
	}
	err := a.runner.WithRead(ctx, func(reader Reader) error {
		rows, err := reader.ListCanonicalNeighborhood(ctx, query)
		if err != nil {
			return err
		}
		facts.Nodes = make([]serviceobs.TopologyNode, 0, len(rows.Nodes))
		for _, node := range rows.Nodes {
			facts.Nodes = append(facts.Nodes, serviceobs.TopologyNode{
				EntityID:    node.EntityID,
				EntityKey:   node.EntityKey,
				Name:        node.Name,
				Layer:       model.EntityLayer(node.Layer),
				Kind:        node.Kind,
				Environment: node.Environment,
				Depth:       node.Depth,
				ObservedAt:  node.ObservedAt,
			})
			facts.ObservedAt = laterTime(facts.ObservedAt, node.ObservedAt)
		}
		facts.Relations = make([]serviceobs.TopologyRelation, 0, len(rows.Relations))
		for _, relation := range rows.Relations {
			facts.Relations = append(facts.Relations, serviceobs.TopologyRelation{
				RelationID:     relation.RelationID,
				SourceEntityID: relation.SourceEntityID,
				TargetEntityID: relation.TargetEntityID,
				Kind:           relation.Relation,
				Provenance:     provenanceFor(relation.Origin),
				Criticality:    relation.Criticality,
				ObservedAt:     relation.ObservedAt,
				// declared 配置在被显式替换前一直有效，所以只有 observed 会 stale。
				Stale: relation.Stale && relation.Origin == string(model.ProvenanceObserved),
			})
			facts.ObservedAt = laterTime(facts.ObservedAt, relation.ObservedAt)
		}
		if rows.NodesTruncated || rows.RelationsTruncated {
			facts.Partial = true
			facts.PartialReason = serviceobs.ReasonTopologyBoundsReached
		}
		return nil
	})
	if err != nil {
		return serviceobs.TopologyFacts{}, fmt.Errorf("alertsystem: 读取 canonical 邻域失败: %w", err)
	}
	facts.SnapshotID = topologySnapshotID(facts)
	facts.ReadAt = a.clock.now()
	return facts, nil
}

func neighborhoodQuery(seeds []string, req serviceobs.TopologyRequest, limits Limits) store.NeighborhoodQuery {
	depth := req.MaxDepth
	if depth <= 0 {
		depth = limits.NeighborhoodDepth
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 || maxNodes > limits.MaxNodes {
		maxNodes = limits.MaxNodes
	}
	maxRelations := req.MaxRelations
	if maxRelations <= 0 || maxRelations > limits.MaxRelations {
		maxRelations = limits.MaxRelations
	}
	staleAfter := req.StaleAfter
	if staleAfter <= 0 {
		staleAfter = limits.ObservedStaleAfter
	}
	return store.NeighborhoodQuery{
		SeedEntityIDs: seeds,
		MaxDepth:      depth,
		MaxNodes:      maxNodes,
		MaxRelations:  maxRelations,
		StaleAfter:    staleAfter,
	}
}

// provenanceFor 把 dependency_edges.origin 映射成契约 provenance。
//
// 这里刻意不把同一对实体的 declared 与 observed 两行合并成 declared_and_observed：
// relation_id 必须是真实且稳定的依赖边主键，Profile 的 relation_refs 才能可靠绑定；
// 合并会强制发明一个随数据行增删而漂移的合成 ID。
func provenanceFor(origin string) model.Provenance {
	switch origin {
	case string(model.ProvenanceDeclared):
		return model.ProvenanceDeclared
	case string(model.ProvenanceObserved):
		return model.ProvenanceObserved
	default:
		// 未知 origin 不猜：inferred 明确表示"这是系统推断的，不是任何一方声明或观察到的"。
		return model.ProvenanceInferred
	}
}

// topologySnapshotID 由邻域内容确定性派生：同一份拓扑两次读取得到同一个 ID，
// 测试可以用固定时钟稳定断言，页面也能判断"这是不是同一张图"。
func topologySnapshotID(facts serviceobs.TopologyFacts) string {
	parts := make([]string, 0, len(facts.Nodes)+len(facts.Relations))
	for _, node := range facts.Nodes {
		parts = append(parts, "n:"+node.EntityID)
	}
	for _, relation := range facts.Relations {
		parts = append(parts, "r:"+relation.RelationID+":"+string(relation.Provenance))
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return "topo_" + hex.EncodeToString(digest[:16])
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
