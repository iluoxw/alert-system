package alertsystem

import (
	"context"
	"fmt"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/store"
)

// maxFocusValueLength 是 focus value 的长度上界。focus value 是不可信输入，即便
// Profile 的 value_pattern 已经校验过，参数化匹配前仍要有硬上界。
const maxFocusValueLength = 256

// DomainAdapter 实现 serviceobs.DomainReader。
//
// 它的实体边界完全独立计算：service entity ∪ Profile roots ∪ 沿 declared 边有界可达的
// 提供者。**它不接受也不读取 topology adapter 的任何返回**，所以 topology 查询失败只会
// 降级 topology section；domain 既不会变空，也不会因为"拿不到拓扑"退化成扫全库。
type DomainAdapter struct {
	runner ReadRunner
	limits Limits
	clock  Clock
}

// NewDomainAdapter 构造 domain adapter。
func NewDomainAdapter(runner ReadRunner, limits Limits, clock Clock) *DomainAdapter {
	return &DomainAdapter{runner: runner, limits: limits.withDefaults(), clock: clock}
}

var _ serviceobs.DomainReader = (*DomainAdapter)(nil)

// ReadDomain 读取稳定边界内的 active Alert/Fault/Impact/Incident。
//
// focus 处理口径：允许的 focus kind 集合就是当前已发布 Profile 的 focus_kinds key 精确
// 集合（Profile 已经过 identifier 与 value_pattern 校验），因此这是 Profile 驱动的通用
// 实现，不含任何领域私有分支。不在该集合内的 kind 返回 typed ErrInvalidFocus，绝不猜
// 标签名，也绝不忽略 focus 后返回整个边界的事实。
func (a *DomainAdapter) ReadDomain(
	ctx context.Context, req serviceobs.DomainRequest,
) (serviceobs.DomainFacts, error) {
	focusKey, focusValue, err := resolveFocusMatch(req.Focus, req.AllowedFocusKinds)
	if err != nil {
		return serviceobs.DomainFacts{}, err
	}

	boundaryDepth := req.MaxBoundaryDepth
	if boundaryDepth <= 0 {
		boundaryDepth = a.limits.BoundaryDepth
	}
	maxEntities := req.MaxBoundaryEntities
	if maxEntities <= 0 || maxEntities > a.limits.MaxBoundaryEntities {
		maxEntities = a.limits.MaxBoundaryEntities
	}
	limit := req.Limit
	if limit <= 0 || limit > a.limits.DomainLimit {
		limit = a.limits.DomainLimit
	}

	facts := serviceobs.DomainFacts{
		BoundaryEntityIDs: []string{},
		Alerts:            []serviceobs.DomainAlert{},
		Faults:            []serviceobs.DomainFault{},
		Impacts:           []serviceobs.DomainImpact{},
		Incidents:         []serviceobs.DomainIncident{},
	}
	err = a.runner.WithRead(ctx, func(reader Reader) error {
		boundary, err := reader.ServiceBoundaryEntityIDs(ctx, req.ServiceEntityID,
			req.RootEntityIDs, boundaryDepth, maxEntities)
		if err != nil {
			return err
		}
		facts.BoundaryEntityIDs = boundary
		if len(boundary) == 0 {
			// 边界为空就什么都不查。宁可返回空，也不放宽成"查全库"。
			return nil
		}
		rows, err := reader.ReadDomainFacts(ctx, store.DomainFactsQuery{
			EntityIDs:  boundary,
			FocusKey:   focusKey,
			FocusValue: focusValue,
			Limit:      limit,
		})
		if err != nil {
			return err
		}
		projectDomainFacts(&facts, rows)
		return nil
	})
	if err != nil {
		return serviceobs.DomainFacts{}, fmt.Errorf("alertsystem: 读取领域事实失败: %w", err)
	}
	facts.ReadAt = a.clock.now()
	return facts, nil
}

// resolveFocusMatch 把请求 focus 收敛成一对参数化匹配键值。
//
// 白名单来源是已发布 Profile 的 focus_kinds key 集合，绝不是一份静态"通用标签"清单——
// 那样会把 Profile 明确声明的 focus kind 排除在外。identifier 与长度检查是纵深防御。
func resolveFocusMatch(focus *model.Focus, allowedKinds []string) (string, string, error) {
	if focus == nil {
		return "", "", nil
	}
	if focus.Kind == "" || focus.Value == "" {
		return "", "", fmt.Errorf("%w: focus kind 与 value 都不能为空", serviceobs.ErrInvalidFocus)
	}
	allowed := false
	for _, kind := range allowedKinds {
		if kind == focus.Kind {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", "", fmt.Errorf("%w: focus kind 未由已发布 Profile 声明", serviceobs.ErrInvalidFocus)
	}
	if !identifierPattern.MatchString(focus.Kind) {
		return "", "", fmt.Errorf("%w: focus kind 不是合法 identifier", serviceobs.ErrInvalidFocus)
	}
	if len(focus.Value) > maxFocusValueLength {
		return "", "", fmt.Errorf("%w: focus value 超长", serviceobs.ErrInvalidFocus)
	}
	return focus.Kind, focus.Value, nil
}

func projectDomainFacts(facts *serviceobs.DomainFacts, rows store.DomainFactsRows) {
	facts.FocusApplied = rows.FocusApplied
	facts.LatestFactAt = rows.LatestFactAt
	facts.Counts = model.DomainCounts{
		ActiveFaults:     rows.Counts.ActiveFaults,
		ConfirmedImpacts: rows.Counts.ConfirmedImpacts,
		ActiveIncidents:  rows.Counts.ActiveIncidents,
	}

	facts.Alerts = make([]serviceobs.DomainAlert, 0, len(rows.Alerts))
	for _, alert := range rows.Alerts {
		facts.Alerts = append(facts.Alerts, serviceobs.DomainAlert{
			AlertID:          alert.AlertID,
			Title:            alert.Title,
			SourceState:      alert.SourceState,
			ResponseState:    alert.ResponseState,
			SuppressionState: alert.SuppressionState,
			State:            alertState(alert),
			Severity:         alert.Severity,
			StartsAt:         alert.StartsAt,
			UpdatedAt:        alert.UpdatedAt,
		})
	}
	facts.Faults = make([]serviceobs.DomainFault, 0, len(rows.Faults))
	for _, fault := range rows.Faults {
		facts.Faults = append(facts.Faults, serviceobs.DomainFault{
			FaultID:              fault.FaultID,
			Title:                fault.Title,
			State:                fault.Status,
			Severity:             model.FaultSeverity(fault.Severity),
			Kind:                 fault.Kind,
			Scope:                fault.Scope,
			EntityID:             fault.EntityID,
			EntityName:           fault.EntityName,
			OwnerTeamName:        fault.OwnerTeamName,
			FirstDetectedAt:      fault.FirstDetectedAt,
			UpdatedAt:            fault.UpdatedAt,
			ConfirmedImpactCount: fault.ConfirmedImpactCount,
			PotentialImpactCount: fault.PotentialImpactCount,
		})
	}
	facts.Impacts = make([]serviceobs.DomainImpact, 0, len(rows.Impacts))
	for _, impact := range rows.Impacts {
		facts.Impacts = append(facts.Impacts, serviceobs.DomainImpact{
			FaultID:    impact.FaultID,
			EntityID:   impact.EntityID,
			EntityName: impact.EntityName,
			Layer:      model.EntityLayer(impact.Layer),
			State:      impact.State,
			UpdatedAt:  impact.UpdatedAt,
		})
	}
	facts.Incidents = make([]serviceobs.DomainIncident, 0, len(rows.Incidents))
	for _, incident := range rows.Incidents {
		facts.Incidents = append(facts.Incidents, serviceobs.DomainIncident{
			IncidentID: incident.IncidentID,
			Number:     incident.Number,
			Title:      incident.Title,
			State:      incident.Status,
			Severity:   incident.Severity,
			Relation:   incident.Relation,
			UpdatedAt:  incident.UpdatedAt,
		})
	}
}

// alertState 给契约的 domain_state.alerts[].state 选一个单值。
//
// Alertmanager 是 Alert 生命周期的唯一权威，所以有 upstream fingerprint 时取
// source_state；本地来源才退回 status。绝不把"暂时看不到"折叠成 resolved。
func alertState(alert store.DomainAlertRow) string {
	if alert.UpstreamFingerprint != "" {
		return alert.SourceState
	}
	return alert.Status
}
