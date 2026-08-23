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
		facts.Relations = mergeCanonicalRelations(rows.Relations)
		for _, relation := range facts.Relations {
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

// mergeCanonicalRelations 把指向同一条 canonical 关系的多行依赖边归一成一条。
//
// 归一口径来自设计事实源：同一个 (source_entity_id, target_entity_id, kind) 上同时存在
// declared 与 observed 行时，必须投影成一条 declared_and_observed 关系。一行一条边的做法
// 让这个状态永远不可达，页面因此无法区分"只是声明过"与"声明了并且真的观察到了"。
//
// 三条不变式：
//
//  1. relation_id 永远是一个真实存在的 dependency_edges 主键，绝不是合成 ID。两者共存时
//     取 declared 行的 ID 作为 canonical：declared 配置只会被显式替换，不会像 observed
//     那样随时间老化，因此它是更稳的锚。只有 observed 时保留 observed 行自己的 ID。
//     同 origin 内多行时按 ID 升序取最小值，保证同一份数据两次读取结果一致。
//  2. MergedRelationIDs 列出被折叠进来的全部真实行 ID（含 canonical）。Profile 的
//     relation_refs 与 signal scope refs 因此仍能命中——无论它当初引用的是哪一行。
//  3. freshness 描述的是 observed overlay，因此 observed_at 与 stale 都只看 observed 成员：
//     observed_at 取**最新的 observed** 成员时间戳，stale 取**最新 observed 成员**自己的
//     过期判定。一条更新的 declared 配置时间戳绝不能让一个过期的实时叠加层看起来刚被观测
//     过；同样，一个更老的 observed 成员过期也不能让当前仍然新鲜的观测被判成 stale。
//     只有 declared 时 observed_at 取最新的 declared 时间戳。declared 底图始终存在，
//     所以 provenance 保持 declared_and_observed，关系不会消失，页面把过期的实时叠加层
//     画成灰色虚线而不是删掉这条边。
func mergeCanonicalRelations(rows []store.NeighborhoodRelationRow) []serviceobs.TopologyRelation {
	type canonicalKey struct {
		source string
		target string
		kind   string
	}
	groups := map[canonicalKey][]store.NeighborhoodRelationRow{}
	order := []canonicalKey{}
	for _, row := range rows {
		key := canonicalKey{row.SourceEntityID, row.TargetEntityID, row.Relation}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	// 组的输出顺序不依赖 map 迭代顺序。
	sort.Slice(order, func(i, j int) bool {
		if order[i].source != order[j].source {
			return order[i].source < order[j].source
		}
		if order[i].target != order[j].target {
			return order[i].target < order[j].target
		}
		return order[i].kind < order[j].kind
	})

	merged := make([]serviceobs.TopologyRelation, 0, len(order))
	for _, key := range order {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].RelationID < group[j].RelationID })

		relation := serviceobs.TopologyRelation{
			SourceEntityID: key.source,
			TargetEntityID: key.target,
			Kind:           key.kind,
		}
		var hasDeclared, hasObserved bool
		// observed overlay 的新鲜度只由最新的 observed 成员决定；declared 时间戳单独跟踪。
		var latestObserved, latestDeclared time.Time
		var newestObservedStale bool
		memberIDs := make([]string, 0, len(group))
		for _, row := range group {
			memberIDs = append(memberIDs, row.RelationID)
			if criticalityRank(row.Criticality) < criticalityRank(relation.Criticality) ||
				relation.Criticality == "" {
				relation.Criticality = row.Criticality
			}
			switch row.Origin {
			case string(model.ProvenanceDeclared):
				if !hasDeclared {
					// 第一个（ID 最小的）declared 行就是 canonical ID。
					relation.RelationID = row.RelationID
				}
				hasDeclared = true
				latestDeclared = laterTime(latestDeclared, row.ObservedAt)
			case string(model.ProvenanceObserved):
				switch {
				case !hasObserved || row.ObservedAt.After(latestObserved):
					latestObserved = row.ObservedAt
					newestObservedStale = row.Stale
				case row.ObservedAt.Equal(latestObserved):
					// 同一时刻的两条观测在同一个 TTL 下不该有不同结论。真出现冲突时
					// fail closed 成 stale：绝不在证据矛盾时声称叠加层是新鲜的。
					newestObservedStale = newestObservedStale || row.Stale
				}
				hasObserved = true
			}
		}
		relation.MergedRelationIDs = memberIDs
		if relation.RelationID == "" {
			// 没有 declared 行：保留第一个（ID 最小的）行自己的 ID。
			relation.RelationID = group[0].RelationID
		}
		relation.Provenance = mergedProvenance(hasDeclared, hasObserved)
		if hasObserved {
			relation.ObservedAt = latestObserved
			// declared 配置在被显式替换前一直有效，所以只有 observed overlay 会 stale。
			relation.Stale = newestObservedStale
		} else {
			relation.ObservedAt = latestDeclared
		}
		merged = append(merged, relation)
	}
	return merged
}

// mergedProvenance 依据组内出现过的 origin 决定 provenance。
func mergedProvenance(hasDeclared, hasObserved bool) model.Provenance {
	switch {
	case hasDeclared && hasObserved:
		return model.ProvenanceDeclaredAndObserved
	case hasDeclared:
		return model.ProvenanceDeclared
	case hasObserved:
		return model.ProvenanceObserved
	default:
		// origin 不在词表内时不猜：inferred 明确表示"这既不是谁声明的，也不是观察到的"。
		return model.ProvenanceInferred
	}
}

// criticalityRank 让"取最高重要度"有确定顺序。未知值排在最后，不会盖掉已知值。
func criticalityRank(criticality string) int {
	switch criticality {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
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
