// Package view 把装配好的事实快照投影成三条路由消费的 Page View。
//
// 它只做稳定的显示投影与格式化：不发原始查询、不重算 Health（三页共用 health 包的同一个
// evaluator）、不写领域状态。所有 slice 输出稳定排序且非 null，同一份快照两次投影逐字节
// 一致——否则翻页与刷新会莫名重排。
//
// 这里没有任何 `if service == e2b` 分支：E2B 只是第一个 Service Profile，它的 focus、
// journey、renderer 与信号绑定全部来自 Profile 文档。
//
// 契约 fixture 只在测试里出现：本包不读 docs/contracts 的任何 example，也不导出 fixture
// 加载器——fixtures 绝不进入 live 数据路径。
package view

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"alert-system/internal/safelink"
	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/model"
)

// DisplayUnknown 是所有不可用观测的显示值。契约要求 display_value 非空，而"没有数值"
// 必须显示成 Unknown，绝不能显示 0 或复用旧值。
const DisplayUnknown = "Unknown"

// SectionKeySignalScope 表达"某个已声明 binding 的 scope.refs 挂不到当前 bounded
// topology"这一局部缺口。
//
// Page View v1 没有通用的 unattached-signals 顶层槽，只有 golden_signals 与
// topology node/relation 的 signals。所以对解析不到的 non-golden binding，投影既不发明
// node/relation，也不错误挂到 service 节点，而是用这个 section + diagnostic summary 的
// missing_requirements 显式暴露缺口。evaluator 与 AssemblySnapshot 仍完整保留该 binding
// 的 Unknown 判定。
const SectionKeySignalScope = "signal_scope"

// dependency group 的稳定 key/label。group_key 必须满足契约的
// `^[a-z][a-z0-9_]{0,63}$`，所以直接用 canonical layer。
var dependencyGroupLabels = map[model.EntityLayer]string{
	model.EntityLayerPlatform:       "平台能力",
	model.EntityLayerInfrastructure: "基础设施与资源",
	model.EntityLayerApplication:    "应用服务",
}

const dependencyGroupFallbackKey = "other"

// ProjectHealth 把 evaluator 结果原样投影，绝不二次计算。
func ProjectHealth(evaluation health.Evaluation) model.HealthSummary {
	summary := evaluation.Health
	summary.ReasonCodes = cloneStrings(summary.ReasonCodes)
	return summary
}

// ProjectDataStatus 把 evaluator 的 Data 判定原样投影。
func ProjectDataStatus(evaluation health.Evaluation) model.DataStatus {
	status := model.DataStatus{State: evaluation.Data.State, Sources: []model.SourceStatus{}}
	status.Sources = append(status.Sources, evaluation.Data.Sources...)
	return status
}

// ProjectProfileRef 生成 Page View 的 profile 引用。返回 nil 表示未配置 Profile：
// 页面必须走 Unconfigured 路径，不猜指标也不猜布局。
func ProjectProfileRef(published *model.Profile) *model.ProfileRef {
	if published == nil {
		return nil
	}
	return &model.ProfileRef{
		ProfileKey:     published.ProfileKey,
		ProfileVersion: published.ProfileVersion,
		Status:         published.Status,
	}
}

// ProjectIdentity 把 registration 身份投影成契约 serviceIdentity。
func ProjectIdentity(identity serviceobs.ServiceIdentity) model.ServiceIdentity {
	return model.ServiceIdentity{
		ServiceID:     identity.ServiceID,
		EntityID:      identity.EntityID,
		EntityKey:     identity.EntityKey,
		Name:          identity.Name,
		Description:   identity.Description,
		Layer:         identity.Layer,
		Kind:          identity.Kind,
		Environment:   identity.Environment,
		OwnerTeamID:   identity.OwnerTeamID,
		OwnerTeamName: identity.OwnerTeamName,
		Tier:          identity.Tier,
	}
}

// ProjectSignal 把一个 binding 的判定结果投影成显示用 signalValue。
//
// 两条契约不变式在这里成立：availability=available 一定有数值；否则 value 必为 null、
// health 必为 unknown 且带非空 unknown_reason。observed_at 用真实事实时间锚渲染"从未
// 观测过"，不用 wall clock。
func ProjectSignal(
	evaluation health.SignalEvaluation, serviceID string, windowSeconds int, factAnchor time.Time,
) model.SignalValue {
	signal := model.SignalValue{
		BindingKey:           evaluation.Binding.Key,
		Label:                signalLabel(evaluation.Binding),
		Family:               evaluation.Binding.Family,
		Semantic:             evaluation.Binding.Semantic,
		ScopeKind:            evaluation.Binding.Scope.Kind,
		ScopeRef:             soctx.ScopeRef(evaluation.Binding, serviceID),
		Value:                evaluation.Value,
		Unit:                 signalUnit(evaluation.Binding),
		Aggregation:          evaluation.Binding.Aggregation,
		WindowSeconds:        windowSeconds,
		ObservedAt:           model.NewTimestamp(soctx.ObservedAtOr(evaluation.ObservedAt, factAnchor)),
		Freshness:            evaluation.Freshness,
		Availability:         model.Availability(evaluation.Availability),
		Health:               evaluation.Health,
		SourceKey:            signalSourceKey(evaluation),
		MeasurementRole:      evaluation.MeasurementRole,
		ProxyReason:          evaluation.ProxyReason,
		ThresholdSource:      evaluation.ThresholdSource,
		BaselineDeltaPercent: evaluation.BaselineDeltaPercent,
		UnknownReason:        evaluation.UnknownReason,
		Sparkline:            cloneFloats(evaluation.Sparkline),
	}
	signal.DisplayValue = formatSignalValue(evaluation.Value, signal.Unit)
	if signal.Availability != model.AvailabilityAvailable && signal.UnknownReason == "" {
		// 契约要求非 available 必带 unknown_reason。这里只是兜底：evaluator 已经保证
		// 每条非 available 判定都有稳定 reason。
		signal.UnknownReason = string(signal.Availability)
	}
	return signal
}

func signalLabel(binding model.SignalBinding) string {
	if binding.Label != "" {
		return binding.Label
	}
	// 契约要求 label 非空。Profile schema 也要求 label，这里只是防御性兜底。
	return binding.Key
}

func signalUnit(binding model.SignalBinding) string {
	if binding.Unit != "" {
		return binding.Unit
	}
	return "count"
}

func signalSourceKey(evaluation health.SignalEvaluation) string {
	if evaluation.SourceKey != "" {
		return evaluation.SourceKey
	}
	return serviceobs.SourceKeyTelemetry
}

// formatSignalValue 生成 display_value。
//
// 单位归一化按契约 README 的口径：毫秒到秒的换算只是显示层，语义与阈值仍以 Profile 的
// 原始单位为准。没有数值一律显示 Unknown。
func formatSignalValue(value *float64, unit string) string {
	if value == nil {
		return DisplayUnknown
	}
	switch unit {
	case "ms":
		if *value >= 1000 {
			return formatNumber(*value/1000, 2) + "s"
		}
		return formatNumber(*value, 0) + "ms"
	case "percent":
		return formatNumber(*value, 1) + "%"
	default:
		return formatNumber(*value, 2) + " " + unit
	}
}

// formatNumber 保留最多 decimals 位小数并去掉尾随零，保证 display_value 稳定且紧凑。
func formatNumber(value float64, decimals int) string {
	formatted := strconv.FormatFloat(value, 'f', decimals, 64)
	if !strings.Contains(formatted, ".") {
		return formatted
	}
	formatted = strings.TrimRight(formatted, "0")
	return strings.TrimSuffix(formatted, ".")
}

// GoldenSignals 返回四张 Golden 卡。
//
// 它按 Profile 的 latency/traffic/errors/saturation 固定顺序输出，且**始终**包含全部
// golden binding：遥测未接入时它们按最小范围降级成 Unknown，绝不因为拿不到数值就消失。
func GoldenSignals(snapshot *soctx.AssemblySnapshot) []model.SignalValue {
	signals := make([]model.SignalValue, 0, 4)
	if snapshot.Profile == nil {
		return signals
	}
	byKey := evaluationsByKey(snapshot)
	for _, binding := range snapshot.Profile.GoldenBindings() {
		evaluation, ok := byKey[binding.Key]
		if !ok {
			continue
		}
		signals = append(signals, ProjectSignal(evaluation,
			snapshot.Identity.ServiceID, snapshot.WindowSeconds, snapshot.FactAnchor))
	}
	return signals
}

func evaluationsByKey(snapshot *soctx.AssemblySnapshot) map[string]health.SignalEvaluation {
	byKey := make(map[string]health.SignalEvaluation, len(snapshot.Evaluation.Signals))
	for _, evaluation := range snapshot.Evaluation.Signals {
		byKey[evaluation.Binding.Key] = evaluation
	}
	return byKey
}

// attachment 记录一次 signal 归属解析的结果。
type attachment struct {
	// nodeSignals / relationSignals 按 entity/relation ID 聚合已挂上的信号。
	nodeSignals     map[string][]model.SignalValue
	relationSignals map[string][]model.SignalValue
	// unresolved 是无法挂到 bounded topology 的 non-golden binding key，按 key 排序。
	unresolved []string
	// aggregateOnly 是 scope 解析到多个 canonical 对象、因此没有无损逐对象展示位置的
	// binding key，按 key 排序。
	aggregateOnly []string
}

// attachSignals 按 assembler 已解析好的归属把非 Golden signal 投影到拓扑对象上。
//
// 归属解析本身**不在这里做**：它由 assembler 的 resolvePlacements 一次算出，
// DiagnosticContext 的节点健康与这里的显示投影共用同一份结果。两处各解析一遍就会出现
// "context 说这个节点 Unknown、页面说它 Critical"的漂移，而这类漂移正是 Page View 不得
// 重算 Health 这条约束要防的。
//
// 四条硬约束仍然成立，只是判定来自共享结果：
//
//  1. scope.refs 缺省、为空或悬空时不发明 node/relation，也不扩大查询边界；
//  2. 不把这些 binding 错误挂到 service 节点上冒充"有观测"；
//  3. scope 解析到多个 canonical 对象时**不做逐对象展示**。那种 binding 的数值是跨对象
//     聚合值（例如 max over hosts），而 v1 的 signalValue.scope_ref 是单值：把同一个数值
//     复制到每个对象上，会把"这一组里最坏的是 91%"显示成"这个对象是 91%"，那是伪造逐对象
//     测量。健康意见仍然逐对象生效（由 assembler 投递），只有数值展示被留空；
//  4. 上述两类 binding 都仍然完整保留在 evaluator 结果与 DiagnosticContext 的 signals 里，
//     缺口通过稳定 section 与 missing_requirements 表达。
func attachSignals(snapshot *soctx.AssemblySnapshot) attachment {
	result := attachment{
		nodeSignals:     map[string][]model.SignalValue{},
		relationSignals: map[string][]model.SignalValue{},
		unresolved:      cloneStrings(snapshot.UnresolvedBindings),
		aggregateOnly:   cloneStrings(snapshot.AggregateOnlyBindings),
	}
	if snapshot.Profile == nil {
		return result
	}
	for _, evaluation := range snapshot.Evaluation.Signals {
		binding := evaluation.Binding
		if binding.Family == model.SignalFamilyGolden {
			// Golden 恒在顶层 golden_signals，不参与拓扑挂载。
			continue
		}
		placement, ok := snapshot.PlacementFor(binding.Key)
		if !ok {
			continue
		}
		// 只有恰好一个挂载目标时数值才明确属于该对象，逐对象展示才是无损的。
		nodeID, relationID, single := placement.SingleTarget()
		if !single {
			continue
		}
		signal := ProjectSignal(evaluation, snapshot.Identity.ServiceID,
			snapshot.WindowSeconds, snapshot.FactAnchor)
		if nodeID != "" {
			result.nodeSignals[nodeID] = append(result.nodeSignals[nodeID], signal)
		}
		if relationID != "" {
			result.relationSignals[relationID] = append(result.relationSignals[relationID], signal)
		}
	}
	sort.Strings(result.unresolved)
	sort.Strings(result.aggregateOnly)
	return result
}

// ProjectSections 把 adapter 级 section 状态与投影级缺口合并成稳定排序的 sections。
func ProjectSections(snapshot *soctx.AssemblySnapshot, extra ...model.SectionStatus) []model.SectionStatus {
	sections := make([]model.SectionStatus, 0, len(snapshot.Sections)+len(extra))
	sections = append(sections, snapshot.Sections...)
	sections = append(sections, extra...)
	sort.SliceStable(sections, func(i, j int) bool { return sections[i].SectionKey < sections[j].SectionKey })
	return sections
}

// ProjectDomainCounts 原样投影领域计数。它只展示，不参与 Health 计算。
func ProjectDomainCounts(snapshot *soctx.AssemblySnapshot) model.DomainCounts {
	return snapshot.DomainFacts.Counts
}

// ProjectFaults 把未关闭 Fault 投影成显示摘要，按严重度与持续时间稳定排序。
func ProjectFaults(snapshot *soctx.AssemblySnapshot, limit int) []model.FaultSummary {
	faults := make([]model.FaultSummary, 0, len(snapshot.DomainFacts.Faults))
	for _, fault := range snapshot.DomainFacts.Faults {
		summary := model.FaultSummary{
			FaultID:              fault.FaultID,
			Title:                fault.Title,
			State:                fault.State,
			Severity:             fault.Severity,
			OwnerTeamName:        fault.OwnerTeamName,
			ConfirmedImpactCount: fault.ConfirmedImpactCount,
			PotentialImpactCount: fault.PotentialImpactCount,
			UpdatedAt:            model.NewTimestamp(fault.UpdatedAt),
		}
		if !fault.FirstDetectedAt.IsZero() && !snapshot.AssembledAt.Before(fault.FirstDetectedAt) {
			duration := int(snapshot.AssembledAt.Sub(fault.FirstDetectedAt) / time.Second)
			summary.DurationSeconds = &duration
		}
		faults = append(faults, summary)
	}
	sort.SliceStable(faults, func(i, j int) bool {
		left, right := severityRank(faults[i].Severity), severityRank(faults[j].Severity)
		if left != right {
			return left < right
		}
		return faults[i].FaultID < faults[j].FaultID
	})
	return capSlice(faults, limit)
}

func severityRank(severity model.FaultSeverity) int {
	switch severity {
	case model.FaultSeverityCritical:
		return 0
	case model.FaultSeverityWarning:
		return 1
	default:
		return 2
	}
}

// ProjectTimeline 从已提交的领域事实派生统一时间线。
//
// 它只投影事实：每个事件的 occurred_at 都是数据库里的真实时间，没有任何推断或补齐。
func ProjectTimeline(snapshot *soctx.AssemblySnapshot, limit int) []model.TimelineEvent {
	events := make([]model.TimelineEvent, 0,
		len(snapshot.DomainFacts.Alerts)+len(snapshot.DomainFacts.Faults)+
			len(snapshot.DomainFacts.Impacts)+len(snapshot.DomainFacts.Incidents))
	for _, alert := range snapshot.DomainFacts.Alerts {
		events = append(events, model.TimelineEvent{
			EventID: "alert:" + alert.AlertID, Kind: model.TimelineKindAlert,
			Title: alert.Title, Source: serviceobs.SourceKeyDomain,
			OccurredAt: model.NewTimestamp(alert.StartsAt), DomainRef: alert.AlertID,
		})
	}
	for _, fault := range snapshot.DomainFacts.Faults {
		event := model.TimelineEvent{
			EventID: "fault:" + fault.FaultID, Kind: model.TimelineKindFault,
			Title: fault.Title, Source: serviceobs.SourceKeyDomain,
			OccurredAt: model.NewTimestamp(fault.FirstDetectedAt), DomainRef: fault.FaultID,
		}
		if fault.EntityID != "" {
			event.EntityIDs = []string{fault.EntityID}
		}
		events = append(events, event)
	}
	for _, impact := range snapshot.DomainFacts.Impacts {
		events = append(events, model.TimelineEvent{
			EventID: "impact:" + impact.FaultID + ":" + impact.EntityID,
			Kind:    model.TimelineKindImpact,
			Title:   impactTitle(impact), Source: serviceobs.SourceKeyDomain,
			OccurredAt: model.NewTimestamp(impact.UpdatedAt),
			EntityIDs:  []string{impact.EntityID}, DomainRef: impact.FaultID,
		})
	}
	for _, incident := range snapshot.DomainFacts.Incidents {
		events = append(events, model.TimelineEvent{
			EventID: "incident:" + incident.IncidentID, Kind: model.TimelineKindIncident,
			Title: incident.Title, Source: serviceobs.SourceKeyDomain,
			OccurredAt: model.NewTimestamp(incident.UpdatedAt), DomainRef: incident.IncidentID,
		})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].OccurredAt != events[j].OccurredAt {
			// 最近的事件在前。
			return events[i].OccurredAt > events[j].OccurredAt
		}
		return events[i].EventID < events[j].EventID
	})
	return capSlice(events, limit)
}

func impactTitle(impact serviceobs.DomainImpact) string {
	name := impact.EntityName
	if name == "" {
		name = impact.EntityID
	}
	return fmt.Sprintf("%s 影响 %s", impactStateLabel(impact.State), name)
}

func impactStateLabel(state string) string {
	switch state {
	case "confirmed":
		return "已确认"
	case "ruled_out":
		return "已排除"
	default:
		return "潜在"
	}
}

// ProjectFocusKinds 把 Profile 声明的 focus 投影成工作区启动器。
func ProjectFocusKinds(published *model.Profile) []model.ViewFocusKind {
	kinds := []model.ViewFocusKind{}
	if published == nil {
		return kinds
	}
	for _, focus := range published.FocusKinds {
		kinds = append(kinds, model.ViewFocusKind{
			Key: focus.Key, Label: focus.Label, Placeholder: focus.Placeholder,
			ValuePattern: focus.ValuePattern, Primary: focus.Primary,
		})
	}
	return kinds
}

// ProjectLinks 生成责任与跳转入口。href 全部是 alert-system 内部路径或登记的 URL，
// 不含任何数据源凭据或查询模板。
func ProjectLinks(snapshot *soctx.AssemblySnapshot) []model.Link {
	return LinksForIdentity(snapshot.Identity)
}

// LinksForIdentity 只用 registration 身份生成入口。
//
// 目录读的是缓存摘要，没有装配快照，但每一行仍然需要"服务登记 / canonical 实体 /
// Dashboard / Runbook"这四个入口。它与 ProjectLinks 共用同一份实现，保证目录行与
// 总览页的入口逐字节一致。
func LinksForIdentity(identity serviceobs.ServiceIdentity) []model.Link {
	links := []model.Link{{
		Kind:  model.LinkKindServiceRegistration,
		Label: "服务登记",
		Href:  "/settings/services/" + identity.ServiceID,
	}, {
		Kind:  model.LinkKindEntity,
		Label: "canonical 实体",
		Href:  "/infrastructure/entities/" + identity.EntityID,
	}}
	// 历史遗留的非法登记值必须在投影层 fail closed：写入校验只管住新写入，早于它落库的
	// `javascript:` / scheme-relative / 裸主机值仍然在库里。这里只丢掉**那一个**链接，
	// 其余身份、责任与领域信息照常返回——不能因为一个坏链接就让整个服务无法查看。
	if dashboard := safelink.SafeOrEmpty(identity.DashboardURL); dashboard != "" {
		links = append(links, model.Link{
			Kind: model.LinkKindDashboard, Label: "Dashboard", Href: dashboard,
		})
	}
	if runbook := safelink.SafeOrEmpty(identity.RunbookURL); runbook != "" {
		links = append(links, model.Link{
			Kind: model.LinkKindRunbook, Label: "Runbook", Href: runbook,
		})
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].Kind < links[j].Kind })
	return links
}

// ProjectDomainRefs 把领域事实投影成契约 domainRef 集合。
func ProjectDomainRefs(snapshot *soctx.AssemblySnapshot) model.WorkspaceDomainState {
	if snapshot.Context != nil {
		// 有 Profile 时直接复用 DiagnosticContext 的投影，保证 context -> view 的
		// domain refs 逐字节不漂移。
		return model.WorkspaceDomainState{
			Alerts:    cloneDomainRefs(snapshot.Context.DomainState.Alerts),
			Faults:    cloneDomainRefs(snapshot.Context.DomainState.Faults),
			Impacts:   cloneDomainRefs(snapshot.Context.DomainState.Impacts),
			Incidents: cloneDomainRefs(snapshot.Context.DomainState.Incidents),
		}
	}
	state := model.WorkspaceDomainState{
		Alerts: []model.DomainRef{}, Faults: []model.DomainRef{},
		Impacts: []model.DomainRef{}, Incidents: []model.DomainRef{},
	}
	for _, alert := range snapshot.DomainFacts.Alerts {
		state.Alerts = append(state.Alerts, model.DomainRef{
			ID: alert.AlertID, State: alert.State, UpdatedAt: model.NewTimestamp(alert.UpdatedAt),
		})
	}
	for _, fault := range snapshot.DomainFacts.Faults {
		state.Faults = append(state.Faults, model.DomainRef{
			ID: fault.FaultID, State: fault.State, UpdatedAt: model.NewTimestamp(fault.UpdatedAt),
		})
	}
	for _, impact := range snapshot.DomainFacts.Impacts {
		state.Impacts = append(state.Impacts, model.DomainRef{
			ID: impact.FaultID + ":" + impact.EntityID, State: impact.State,
			UpdatedAt: model.NewTimestamp(impact.UpdatedAt),
		})
	}
	for _, incident := range snapshot.DomainFacts.Incidents {
		state.Incidents = append(state.Incidents, model.DomainRef{
			ID: incident.IncidentID, State: incident.State,
			UpdatedAt: model.NewTimestamp(incident.UpdatedAt),
		})
	}
	return state
}

func cloneDomainRefs(refs []model.DomainRef) []model.DomainRef {
	out := make([]model.DomainRef, len(refs))
	copy(out, refs)
	return out
}

func cloneStrings(items []string) []string {
	out := make([]string, len(items))
	copy(out, items)
	return out
}

func cloneFloats(values []float64) []float64 {
	if values == nil {
		return nil
	}
	out := make([]float64, len(values))
	copy(out, values)
	return out
}

func capSlice[T any](items []T, limit int) []T {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	return items[:limit]
}
