package view

import (
	"sort"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/model"
)

// 与契约 overviewView 的 maxItems 对齐。
const (
	maxOverviewFaults       = 50
	maxOverviewDependencies = 200
	maxOverviewEvents       = 100
)

// ProjectOverview 生成 `/services/:service_id` 服务总览。
//
// Profile 缺失时它仍然可用：identity、责任、declared 依赖与领域状态全部保留，只是
// profile 为 null、Data 为 Unconfigured、Golden 为空。这正是"不猜指标、不猜布局"。
func ProjectOverview(snapshot *soctx.AssemblySnapshot) model.OverviewView {
	view := model.OverviewView{
		SchemaVersion: model.SchemaVersion,
		ViewKind:      model.ViewKindServiceOverview,
		RequestID:     snapshot.RequestID,
		AssembledAt:   model.NewTimestamp(snapshot.AssembledAt),
		Service:       ProjectIdentity(snapshot.Identity),
		Profile:       ProjectProfileRef(snapshot.Profile),
		Health:        ProjectHealth(snapshot.Evaluation),
		DataStatus:    ProjectDataStatus(snapshot.Evaluation),
		GoldenSignals: GoldenSignals(snapshot),
		DomainCounts:  ProjectDomainCounts(snapshot),
		Faults:        ProjectFaults(snapshot, maxOverviewFaults),
		Dependencies:  projectDependencies(snapshot),
		RecentEvents:  ProjectTimeline(snapshot, maxOverviewEvents),
		FocusKinds:    ProjectFocusKinds(snapshot.Profile),
		Links:         ProjectLinks(snapshot),
		Sections:      ProjectSections(snapshot, profileDiagnosticSection(snapshot)...),
	}
	view.Normalize()
	return view
}

// profileDiagnosticSection 在 Profile 被 quarantine 时给出稳定配置错误 section。
//
// 它与"这个服务根本没配 Profile"必须可区分：后者没有 diagnostic，前者要告诉运维
// "你配了，但它非法"。
func profileDiagnosticSection(snapshot *soctx.AssemblySnapshot) []model.SectionStatus {
	if snapshot.Profile != nil || snapshot.ProfileDiagnostic == nil {
		return nil
	}
	reason := serviceobs.ReasonProfileUnconfigured
	if len(snapshot.ProfileDiagnostic.ReasonCodes) > 0 {
		// 只输出稳定 reason code，绝不输出文档正文或路径内容。
		reason = snapshot.ProfileDiagnostic.ReasonCodes[0]
	}
	return []model.SectionStatus{{
		SectionKey: "profile", State: model.AvailabilityUnavailable,
		ReasonCode: reason, Retryable: false,
	}}
}

// projectDependencies 生成关键依赖台账。
//
// 数据来源是 bounded canonical topology 里与 service entity 直接相连的关系：service 是
// source 时对端是 provider，service 是 target 时对端是 consumer。group_key/group_label
// 是服务端投影事实，浏览器不按实体名猜分组。
//
// 每行的 Health 恒为 Unknown（Phase 2 没有任何可用 signal）：拓扑里存在一个依赖不等于
// 它健康。
func projectDependencies(snapshot *soctx.AssemblySnapshot) []model.DependencyRow {
	nodes := make(map[string]serviceobs.TopologyNode, len(snapshot.TopologyFacts.Nodes))
	for _, node := range snapshot.TopologyFacts.Nodes {
		nodes[node.EntityID] = node
	}

	rows := make([]model.DependencyRow, 0, len(snapshot.TopologyFacts.Relations))
	seen := map[string]struct{}{}
	for _, relation := range snapshot.TopologyFacts.Relations {
		var peerID, role string
		switch snapshot.Identity.EntityID {
		case relation.SourceEntityID:
			peerID, role = relation.TargetEntityID, "provider"
		case relation.TargetEntityID:
			peerID, role = relation.SourceEntityID, "consumer"
		default:
			continue
		}
		peer, ok := nodes[peerID]
		if !ok {
			// 关系两端必须都在已投影的节点集合里。缺一端就跳过，绝不发明节点。
			continue
		}
		key := peerID + "|" + role
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		row := model.DependencyRow{
			EntityID:   peer.EntityID,
			Name:       peer.Name,
			Layer:      peer.Layer,
			GroupKey:   dependencyGroupKey(peer.Layer),
			GroupLabel: dependencyGroupLabel(peer.Layer),
			Role:       role,
			Provenance: relation.Provenance,
			Health:     model.HealthUnknown,
			Freshness:  dependencyFreshness(relation),
		}
		if !relation.ObservedAt.IsZero() {
			row.ObservedAt = model.NewTimestamp(relation.ObservedAt)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].GroupKey != rows[j].GroupKey {
			return rows[i].GroupKey < rows[j].GroupKey
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].EntityID < rows[j].EntityID
	})
	return capSlice(rows, maxOverviewDependencies)
}

func dependencyFreshness(relation serviceobs.TopologyRelation) model.Freshness {
	if relation.Stale {
		return model.FreshnessStale
	}
	if relation.ObservedAt.IsZero() {
		return model.FreshnessUnknown
	}
	return model.FreshnessFresh
}

func dependencyGroupKey(layer model.EntityLayer) string {
	if _, ok := dependencyGroupLabels[layer]; ok {
		return string(layer)
	}
	return dependencyGroupFallbackKey
}

func dependencyGroupLabel(layer model.EntityLayer) string {
	if label, ok := dependencyGroupLabels[layer]; ok {
		return label
	}
	return "其它"
}
