package view

import (
	"sort"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/model"
)

// 与契约 workspaceView 的 maxItems 对齐。
const (
	maxWorkspaceJourneyStages = 50
	maxWorkspaceTimeline      = 500
	maxWorkspaceStatements    = 20
	maxWorkspaceMissing       = 20
)

// ProjectWorkspace 生成 `/services/:service_id/observability` 工作区。
//
// 它只在存在 active published Profile 时被调用（assembler 已经用
// serviceobs.ErrProfileUnconfigured 挡住未配置的服务），所以这里不需要再猜配置。
func ProjectWorkspace(snapshot *soctx.AssemblySnapshot) model.WorkspaceView {
	nodeIDs := make(map[string]struct{}, len(snapshot.TopologyFacts.Nodes))
	for _, node := range snapshot.TopologyFacts.Nodes {
		nodeIDs[node.EntityID] = struct{}{}
	}
	relationIDs := make(map[string]struct{}, len(snapshot.TopologyFacts.Relations))
	for _, relation := range snapshot.TopologyFacts.Relations {
		relationIDs[relation.RelationID] = struct{}{}
	}
	attached := attachSignals(snapshot, nodeIDs, relationIDs)

	view := model.WorkspaceView{
		SchemaVersion:  model.SchemaVersion,
		ViewKind:       model.ViewKindObservabilityWorkspace,
		RequestID:      snapshot.RequestID,
		ContextVersion: 1,
		AssembledAt:    model.NewTimestamp(snapshot.AssembledAt),
		Service:        ProjectIdentity(snapshot.Identity),
		Profile:        ProjectProfileRef(snapshot.Profile),
		Scope: model.ViewScope{
			Tenant:      snapshot.Scope.Tenant,
			Environment: snapshot.Scope.Environment,
			From:        snapshot.Scope.TimeRange.From,
			To:          snapshot.Scope.TimeRange.To,
			Timezone:    snapshot.Scope.Timezone,
		},
		Focus:         snapshot.Focus,
		Health:        ProjectHealth(snapshot.Evaluation),
		DataStatus:    ProjectDataStatus(snapshot.Evaluation),
		GoldenSignals: GoldenSignals(snapshot),
		Journey:       projectJourney(snapshot),
		Topology:      projectWorkspaceTopology(snapshot, attached),
		EvidenceRefs:  projectEvidenceRefs(snapshot),
		DomainState:   ProjectDomainRefs(snapshot),
		Timeline:      ProjectTimeline(snapshot, maxWorkspaceTimeline),
		Capabilities:  snapshot.Capabilities,
	}
	view.DiagnosticSummary = projectDiagnosticSummary(snapshot, attached)
	view.Sections = ProjectSections(snapshot, workspaceSections(snapshot, attached)...)
	view.Normalize()
	return view
}

// workspaceSections 追加投影级 section。
//
// signal_scope 是关键一条：某些已声明 binding 的 scope.refs 挂不到当前 bounded topology
// 时，Page View v1 没有承载它们的槽位。这里不发明 node/relation、不扩 schema、也不把它们
// 错误挂到 service 节点，而是用一个稳定 section 把局部缺口显式暴露出来。
func workspaceSections(snapshot *soctx.AssemblySnapshot, attached attachment) []model.SectionStatus {
	sections := []model.SectionStatus{}
	if len(attached.unresolved) > 0 {
		sections = append(sections, model.SectionStatus{
			SectionKey: SectionKeySignalScope,
			State:      model.AvailabilityUnknown,
			ReasonCode: serviceobs.ReasonSignalScopeUnresolved,
			Retryable:  false,
		})
	}
	if snapshot.Focus == nil {
		// 无 focus 时不发明 observed journey：journey 区块是 Empty，而不是伪造一条路径。
		sections = append(sections, model.SectionStatus{
			SectionKey: serviceobs.SectionKeyJourney,
			State:      model.AvailabilityEmpty,
			Retryable:  false,
		})
	}
	sections = append(sections, model.SectionStatus{
		SectionKey: serviceobs.SectionKeyTimeline,
		State:      timelineSectionState(snapshot),
		Retryable:  true,
	})
	sections = append(sections, model.SectionStatus{
		SectionKey: serviceobs.SectionKeyDiagnosis,
		State:      model.AvailabilityAvailable,
		Retryable:  false,
	})
	return sections
}

func timelineSectionState(snapshot *soctx.AssemblySnapshot) model.Availability {
	if len(snapshot.DomainFacts.Alerts) == 0 && len(snapshot.DomainFacts.Faults) == 0 &&
		len(snapshot.DomainFacts.Impacts) == 0 && len(snapshot.DomainFacts.Incidents) == 0 {
		return model.AvailabilityEmpty
	}
	return model.AvailabilityAvailable
}

// projectJourney 投影请求旅程。
//
// 无 focus 时返回空数组：没有聚焦就没有"这一次请求走过哪些阶段"这件事，发明一条 journey
// 等于伪造观测。有 focus 但没有观测数据时，每个阶段是 Unknown 而不是 ok/pending 猜测；
// 阶段的 canonical entity/relation refs 原样保留，即使它们落在当前 bounded 投影之外。
func projectJourney(snapshot *soctx.AssemblySnapshot) []model.JourneyStageView {
	stages := []model.JourneyStageView{}
	if snapshot.Profile == nil || snapshot.Focus == nil {
		return stages
	}
	for _, journey := range snapshot.Profile.Journeys {
		if journey.FocusKind != snapshot.Focus.Kind {
			continue
		}
		for _, stage := range journey.Stages {
			view := model.JourneyStageView{
				Key:         stage.Key,
				Label:       stage.Label,
				State:       journeyStageState(snapshot),
				EntityIDs:   append([]string{}, stage.EntityRefs...),
				RelationIDs: append([]string{}, stage.RelationRefs...),
				EvidenceIDs: []string{},
			}
			if view.State == model.JourneyStageStateUnknown {
				view.ReasonCode = serviceobs.ReasonTelemetryNotConnected
			}
			stages = append(stages, view)
		}
		break
	}
	return capSlice(stages, maxWorkspaceJourneyStages)
}

// journeyStageState 只能由观测决定。
//
// 本 Phase 没有接入 trace/log 观测，所以每个阶段都是 Unknown：未观察到既不等于成功，
// 也不等于失败。Phase 3 接入 focus adapter 后才由观测事实决定 ok/failed/pending。
func journeyStageState(*soctx.AssemblySnapshot) model.JourneyStageState {
	return model.JourneyStageStateUnknown
}

// projectEvidenceRefs 投影 EvidenceRef。它只搬运已脱敏的引用：原始日志正文、trace
// attributes 与凭据留在源系统，绝不进 Page View。
func projectEvidenceRefs(snapshot *soctx.AssemblySnapshot) []model.EvidenceRef {
	refs := make([]model.EvidenceRef, 0, len(snapshot.EvidenceRefs))
	refs = append(refs, snapshot.EvidenceRefs...)
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].EvidenceID < refs[j].EvidenceID })
	return refs
}

// projectWorkspaceTopology 投影动态拓扑。
//
// 没有 signal 的节点/关系 Health 必须是 Unknown：存在不等于健康。observed 关系过期只让
// freshness=stale，declared 底图保留。
func projectWorkspaceTopology(snapshot *soctx.AssemblySnapshot, attached attachment) model.WorkspaceTopology {
	topology := model.WorkspaceTopology{
		SnapshotID: snapshot.TopologyFacts.SnapshotID,
		Partial:    snapshot.TopologyFacts.Partial,
		Nodes:      []model.TopologyNodeView{},
		Relations:  []model.TopologyRelationView{},
	}
	if topology.SnapshotID == "" {
		// 拓扑不可用时仍要一个稳定非空 ID：它表示"这次没有可用拓扑"，不是某张真实的图。
		topology.SnapshotID = "topo_unavailable"
	}
	for _, node := range snapshot.TopologyFacts.Nodes {
		signals := attached.nodeSignals[node.EntityID]
		topology.Nodes = append(topology.Nodes, model.TopologyNodeView{
			EntityID:  node.EntityID,
			Name:      node.Name,
			Layer:     string(node.Layer),
			Kind:      node.Kind,
			Health:    aggregateSignalHealth(signals),
			Freshness: nodeViewFreshness(node),
			Signals:   signals,
		})
	}
	for _, relation := range snapshot.TopologyFacts.Relations {
		signals := attached.relationSignals[relation.RelationID]
		freshness := model.FreshnessFresh
		if relation.Stale {
			freshness = model.FreshnessStale
		} else if relation.ObservedAt.IsZero() {
			freshness = model.FreshnessUnknown
		}
		topology.Relations = append(topology.Relations, model.TopologyRelationView{
			RelationID:     relation.RelationID,
			SourceEntityID: relation.SourceEntityID,
			TargetEntityID: relation.TargetEntityID,
			Kind:           relation.Kind,
			Provenance:     relation.Provenance,
			Health:         aggregateSignalHealth(signals),
			Freshness:      freshness,
			Signals:        signals,
		})
	}
	sort.SliceStable(topology.Nodes, func(i, j int) bool {
		if topology.Nodes[i].Layer != topology.Nodes[j].Layer {
			return topology.Nodes[i].Layer < topology.Nodes[j].Layer
		}
		if topology.Nodes[i].Name != topology.Nodes[j].Name {
			return topology.Nodes[i].Name < topology.Nodes[j].Name
		}
		return topology.Nodes[i].EntityID < topology.Nodes[j].EntityID
	})
	sort.SliceStable(topology.Relations, func(i, j int) bool {
		return topology.Relations[i].RelationID < topology.Relations[j].RelationID
	})
	return topology
}

func nodeViewFreshness(node serviceobs.TopologyNode) model.Freshness {
	if node.ObservedAt.IsZero() {
		return model.FreshnessUnknown
	}
	return model.FreshnessFresh
}

// aggregateSignalHealth 由已挂载 signal 的 worst-of 得出节点/边健康。
//
// 没有任何 signal 就是 Unknown：拓扑里存在一个对象不能因此显示 Healthy。neutral 不参与
// 聚合（它本来就没有健康意见），但一个只有 neutral 信号的对象仍然是 Unknown，因为没有
// 任何信号对它的健康表态。
func aggregateSignalHealth(signals []model.SignalValue) model.Health {
	if len(signals) == 0 {
		return model.HealthUnknown
	}
	opinionated := false
	worst := model.HealthHealthy
	for _, signal := range signals {
		switch signal.Health {
		case model.SignalHealthCritical:
			return model.HealthCritical
		case model.SignalHealthDegraded:
			worst, opinionated = model.HealthDegraded, true
		case model.SignalHealthHealthy:
			opinionated = true
		case model.SignalHealthUnknown:
			return model.HealthUnknown
		}
	}
	if !opinionated {
		return model.HealthUnknown
	}
	return worst
}

// projectDiagnosticSummary 生成确定性诊断摘要。
//
// 本 Phase 只允许两种结论：必需来源缺失时 incomplete；全部来源可用且恰好只有一条已提交
// 权威事实时 observation。绝不因为"没有遥测"就给 candidate/confirmed cause。
func projectDiagnosticSummary(snapshot *soctx.AssemblySnapshot, attached attachment) model.DiagnosticSummary {
	missing := missingRequirements(snapshot, attached)
	if len(missing) > 0 {
		return model.DiagnosticSummary{
			Level: model.DiagnosticLevelIncomplete,
			Title: "诊断未完成：必需来源缺失",
			Statements: capSlice([]string{
				"Service Registration、declared topology 与已提交领域状态仍然可用。",
				"缺失来源只影响对应观测；页面不会用旧值或 0 代替未观测到的信号。",
			}, maxWorkspaceStatements),
			EvidenceIDs:         []string{},
			MissingRequirements: capSlice(missing, maxWorkspaceMissing),
		}
	}
	if len(snapshot.DomainFacts.Faults) == 1 {
		fault := snapshot.DomainFacts.Faults[0]
		return model.DiagnosticSummary{
			Level:               model.DiagnosticLevelObservation,
			Title:               fault.Title,
			Statements:          []string{"当前只有一条已提交的权威领域事实支持该观测。"},
			EvidenceIDs:         []string{},
			MissingRequirements: []string{},
		}
	}
	return model.DiagnosticSummary{
		Level:               model.DiagnosticLevelIncomplete,
		Title:               "诊断未完成：缺少可归因的单一权威事实",
		Statements:          []string{"当前证据不足以形成候选原因。"},
		EvidenceIDs:         []string{},
		MissingRequirements: []string{},
	}
}

// missingRequirements 列出阻止形成结论的缺口。它只列稳定机器码与 binding key，
// 不输出查询模板、路径或证据正文。
func missingRequirements(snapshot *soctx.AssemblySnapshot, attached attachment) []string {
	missing := []string{}
	for _, section := range snapshot.Sections {
		switch section.State {
		case model.AvailabilityUnavailable, model.AvailabilityForbidden, model.AvailabilityUnknown:
			reason := section.ReasonCode
			if reason == "" {
				reason = string(section.State)
			}
			missing = append(missing, section.SectionKey+":"+reason)
		}
	}
	for _, key := range attached.unresolved {
		missing = append(missing, serviceobs.ReasonSignalScopeUnresolved+":"+key)
	}
	sort.Strings(missing)
	return missing
}
