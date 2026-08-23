package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/observe"
	"alert-system/internal/serviceobs/provider"
)

// Config 是 OpenObserve adapter 的构造参数。
type Config struct {
	Client       *provider.Client
	Organization string
}

// Adapter 实现 TraceReader / LogReader / AccessReader。
type Adapter struct {
	client       *provider.Client
	organization string
}

// New 构造 adapter。client 为 nil 时返回 ErrNotConnected。
func New(cfg Config) (*Adapter, error) {
	if cfg.Client == nil {
		return nil, ErrNotConnected
	}
	if err := validateIdentifier(cfg.Organization); err != nil {
		return nil, err
	}
	return &Adapter{client: cfg.Client, organization: cfg.Organization}, nil
}

var (
	_ serviceobs.TraceReader  = (*Adapter)(nil)
	_ serviceobs.LogReader    = (*Adapter)(nil)
	_ serviceobs.AccessReader = (*Adapter)(nil)
)

// ReadTraces 读取 trace/journey/focus 事实。
func (a *Adapter) ReadTraces(ctx context.Context, req serviceobs.FocusRequest) (serviceobs.TraceFacts, error) {
	readAt := time.Now().UTC()
	if req.Focus == nil {
		return serviceobs.TraceFacts{
			Focus:  serviceobs.FocusNotRequested,
			Source: emptySource(serviceobs.SourceKeyOpenObserveTraces),
			ReadAt: readAt,
		}, nil
	}
	bindings, err := filterBindings(req.Bindings, serviceobs.SourceKeyOpenObserveTraces, traceBindingAllowed)
	if err != nil {
		return serviceobs.TraceFacts{}, err
	}
	if len(bindings) == 0 {
		return serviceobs.TraceFacts{
			Focus:  serviceobs.FocusEmpty,
			Source: emptySource(serviceobs.SourceKeyOpenObserveTraces),
			ReadAt: readAt,
		}, nil
	}

	result, err := a.executeBindings(ctx, req, bindings)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return serviceobs.TraceFacts{}, err
		}
		if errors.Is(err, observe.ErrUpstreamAuthFailed) {
			return serviceobs.TraceFacts{}, err
		}
	}
	return materializeTraceFacts(req, bindings, result, readAt), nil
}

// ReadLogs 读取日志证据。
func (a *Adapter) ReadLogs(ctx context.Context, req serviceobs.FocusRequest) (serviceobs.LogFacts, error) {
	readAt := time.Now().UTC()
	if req.Focus == nil {
		return serviceobs.LogFacts{
			Focus:  serviceobs.FocusNotRequested,
			Source: emptySource(serviceobs.SourceKeyOpenObserveLogs),
			ReadAt: readAt,
		}, nil
	}
	bindings, err := filterBindings(req.Bindings, serviceobs.SourceKeyOpenObserveLogs, logsBindingAllowed)
	if err != nil {
		return serviceobs.LogFacts{}, err
	}
	if len(bindings) == 0 {
		return serviceobs.LogFacts{
			Focus:  serviceobs.FocusEmpty,
			Source: emptySource(serviceobs.SourceKeyOpenObserveLogs),
			ReadAt: readAt,
		}, nil
	}
	result, err := a.executeBindings(ctx, req, bindings)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return serviceobs.LogFacts{}, err
		}
		if errors.Is(err, observe.ErrUpstreamAuthFailed) {
			return serviceobs.LogFacts{}, err
		}
	}
	return materializeLogFacts(req, bindings, result, readAt), nil
}

// ReadAccess 读取 access 证据。
func (a *Adapter) ReadAccess(ctx context.Context, req serviceobs.FocusRequest) (serviceobs.AccessFacts, error) {
	readAt := time.Now().UTC()
	if req.Focus == nil {
		return serviceobs.AccessFacts{
			Focus:  serviceobs.FocusNotRequested,
			Source: emptySource(serviceobs.SourceKeyOpenObserveAccess),
			ReadAt: readAt,
		}, nil
	}
	bindings, err := filterBindings(req.Bindings, serviceobs.SourceKeyOpenObserveAccess, accessBindingAllowed)
	if err != nil {
		return serviceobs.AccessFacts{}, err
	}
	if len(bindings) == 0 {
		return serviceobs.AccessFacts{
			Focus:  serviceobs.FocusEmpty,
			Source: emptySource(serviceobs.SourceKeyOpenObserveAccess),
			ReadAt: readAt,
		}, nil
	}
	result, err := a.executeBindings(ctx, req, bindings)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return serviceobs.AccessFacts{}, err
		}
		if errors.Is(err, observe.ErrUpstreamAuthFailed) {
			return serviceobs.AccessFacts{}, err
		}
	}
	return materializeAccessFacts(req, bindings, result, readAt), nil
}

type bindingExecution struct {
	Binding     integration.CompiledBinding
	Hits        []map[string]json.RawMessage
	Err         error
	Fingerprint string
}

type aggregateExecution struct {
	ByFingerprint map[string]bindingExecution
}

func traceBindingAllowed(binding integration.CompiledBinding) bool {
	switch binding.Kind {
	case integration.KindJourney, integration.KindFocus:
		return true
	case integration.KindEvidence:
		return binding.EvidenceClass == integration.EvidenceClassTraces
	default:
		return false
	}
}

func logsBindingAllowed(binding integration.CompiledBinding) bool {
	return binding.Kind == integration.KindEvidence && binding.EvidenceClass == integration.EvidenceClassLogs
}

func accessBindingAllowed(binding integration.CompiledBinding) bool {
	return binding.Kind == integration.KindEvidence && binding.EvidenceClass == integration.EvidenceClassAccess
}

func filterBindings(
	bindings []integration.CompiledBinding,
	sourceKey string,
	allowed func(integration.CompiledBinding) bool,
) ([]integration.CompiledBinding, error) {
	out := make([]integration.CompiledBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.SourceKey != sourceKey {
			continue
		}
		if binding.Capability != integration.CapabilityOpenObserve {
			return nil, fmt.Errorf("%w: capability=%s", ErrInvalidBinding, binding.Capability)
		}
		if !allowed(binding) {
			return nil, fmt.Errorf("%w: kind=%s class=%s", ErrInvalidBinding, binding.Kind, binding.EvidenceClass)
		}
		out = append(out, binding)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (a *Adapter) executeBindings(
	ctx context.Context,
	req serviceobs.FocusRequest,
	bindings []integration.CompiledBinding,
) (aggregateExecution, error) {
	agg := aggregateExecution{ByFingerprint: map[string]bindingExecution{}}
	profileMax := req.Profile.Limits.MaxRows
	rowLimitDefault := req.Limits.MaxRows

	for _, binding := range bindings {
		rowLimit := effectiveRowLimit(binding, profileMax, rowLimitDefault)
		plan, err := buildSearchQuery(a.organization, binding, req.Focus.Value, req.From, req.To, rowLimit)
		if err != nil {
			agg.ByFingerprint[binding.Key] = bindingExecution{Binding: binding, Err: err}
			continue
		}
		if cached, ok := agg.ByFingerprint[plan.Fingerprint]; ok {
			cached.Binding = binding
			agg.ByFingerprint[binding.Key] = cached
			continue
		}

		resp, err := a.client.PostJSON(ctx, plan.Path, plan.Body)
		if err != nil {
			err = mapProviderError(err)
			if errors.Is(err, observe.ErrUpstreamAuthFailed) {
				return aggregateExecution{}, err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return aggregateExecution{}, err
			}
			agg.ByFingerprint[plan.Fingerprint] = bindingExecution{
				Binding: binding, Err: err, Fingerprint: plan.Fingerprint,
			}
			agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
			continue
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return aggregateExecution{}, observe.ErrUpstreamAuthFailed
		}
		if resp.StatusCode == 429 {
			agg.ByFingerprint[plan.Fingerprint] = bindingExecution{
				Binding: binding, Err: observe.ErrUpstreamRateLimited, Fingerprint: plan.Fingerprint,
			}
			agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err := mapHTTPError(resp.StatusCode, resp.Body)
			if isSchemaError(resp.StatusCode, resp.Body) {
				err = ErrSchemaError
			}
			agg.ByFingerprint[plan.Fingerprint] = bindingExecution{
				Binding: binding, Err: err, Fingerprint: plan.Fingerprint,
			}
			agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
			continue
		}
		parsed, err := parseSearchResponse(resp.Body)
		if err != nil {
			agg.ByFingerprint[plan.Fingerprint] = bindingExecution{
				Binding: binding, Err: err, Fingerprint: plan.Fingerprint,
			}
			agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
			continue
		}
		if err := hitsWithinRowLimit(parsed.Hits, rowLimit); err != nil {
			agg.ByFingerprint[plan.Fingerprint] = bindingExecution{
				Binding: binding, Err: observe.ErrResponseTooLarge, Fingerprint: plan.Fingerprint,
			}
			agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
			continue
		}
		agg.ByFingerprint[plan.Fingerprint] = bindingExecution{Binding: binding, Hits: parsed.Hits, Fingerprint: plan.Fingerprint}
		agg.ByFingerprint[binding.Key] = agg.ByFingerprint[plan.Fingerprint]
	}
	return agg, nil
}

func materializeLogFacts(
	req serviceobs.FocusRequest,
	bindings []integration.CompiledBinding,
	agg aggregateExecution,
	readAt time.Time,
) serviceobs.LogFacts {
	focus, source, refs := materializeEvidence(
		req, bindings, agg, readAt, serviceobs.SourceKeyOpenObserveLogs, buildLogEvidenceRefs,
	)
	return serviceobs.LogFacts{Focus: focus, EvidenceRefs: refs, Source: source, ReadAt: readAt}
}

func materializeAccessFacts(
	req serviceobs.FocusRequest,
	bindings []integration.CompiledBinding,
	agg aggregateExecution,
	readAt time.Time,
) serviceobs.AccessFacts {
	focus, source, refs := materializeEvidence(
		req, bindings, agg, readAt, serviceobs.SourceKeyOpenObserveAccess, buildAccessEvidenceRefs,
	)
	return serviceobs.AccessFacts{Focus: focus, EvidenceRefs: refs, Source: source, ReadAt: readAt}
}

type evidenceBuilder func(integration.CompiledBinding, string, *model.Focus, []map[string]json.RawMessage, time.Time) []model.EvidenceRef

func materializeEvidence(
	req serviceobs.FocusRequest,
	bindings []integration.CompiledBinding,
	agg aggregateExecution,
	readAt time.Time,
	sourceKey string,
	build evidenceBuilder,
) (serviceobs.FocusResult, health.SourceObservation, []model.EvidenceRef) {
	var refs []model.EvidenceRef
	successes := 0
	failures := 0
	matched := false
	for _, binding := range bindings {
		exec := agg.ByFingerprint[binding.Key]
		if exec.Err != nil {
			failures++
			continue
		}
		successes++
		if len(exec.Hits) > 0 {
			matched = true
			refs = append(refs, build(binding, req.ServiceID, req.Focus, exec.Hits, readAt)...)
		}
	}
	focus := serviceobs.FocusEmpty
	if matched {
		focus = serviceobs.FocusMatched
	} else if failures > 0 && successes == 0 {
		focus = serviceobs.FocusUnknown
	}
	return focus, buildSource(sourceKey, successes, failures, matched, readAt, agg), refs
}

func materializeTraceFacts(
	req serviceobs.FocusRequest,
	bindings []integration.CompiledBinding,
	agg aggregateExecution,
	readAt time.Time,
) serviceobs.TraceFacts {
	focus := serviceobs.FocusEmpty
	successes := 0
	failures := 0
	matched := false

	var evidence []model.EvidenceRef
	var stages []serviceobs.JourneyStageObservation
	var hints []serviceobs.ObservedTopologyHint
	var instanceHints []serviceobs.ObservedInstanceHint
	var issues []serviceobs.ReconciliationIssue
	seenFingerprints := map[string]struct{}{}

	for _, binding := range bindings {
		if err := validateIdentityMappings(binding); err != nil {
			failures++
			continue
		}
		exec := agg.ByFingerprint[binding.Key]
		if exec.Err != nil {
			failures++
			continue
		}
		if exec.Fingerprint != "" {
			if _, ok := seenFingerprints[exec.Fingerprint]; ok {
				successes++
				if len(exec.Hits) > 0 {
					matched = true
				}
				continue
			}
			seenFingerprints[exec.Fingerprint] = struct{}{}
		}
		successes++
		if len(exec.Hits) == 0 {
			continue
		}
		matched = true
		traceRefs := buildTraceEvidenceRefs(binding, req.ServiceID, req.Focus, exec.Hits, readAt)
		evidence = append(evidence, traceRefs...)
		if binding.Kind == integration.KindJourney || containsSideOutput(binding, integration.SideOutputFocus) {
			// focus matched via hits
		}
		if binding.Kind == integration.KindJourney {
			projectedStages, stageIssues := projectJourneyStages(binding, exec.Hits, traceRefs, readAt)
			stages = mergeJourneyStages(stages, projectedStages)
			issues = append(issues, stageIssues...)
		}
		if containsSideOutput(binding, integration.SideOutputTopologyHints) {
			projectedHints, projectedInstances, topoIssues := projectTopology(binding, exec.Hits, traceRefs, readAt)
			hints = append(hints, projectedHints...)
			instanceHints = append(instanceHints, projectedInstances...)
			issues = append(issues, topoIssues...)
		}
	}
	if matched {
		focus = serviceobs.FocusMatched
	} else if failures > 0 && successes == 0 {
		focus = serviceobs.FocusUnknown
	}
	if focus == serviceobs.FocusEmpty && len(bindings) > 0 && failures == 0 {
		focus = serviceobs.FocusEmpty
	}
	source := buildSource(serviceobs.SourceKeyOpenObserveTraces, successes, failures, matched, readAt, agg)
	serviceobs.SortIssues(issues)
	return serviceobs.TraceFacts{
		Focus:         focus,
		JourneyStages: stages,
		TopologyHints: hints,
		InstanceHints: instanceHints,
		EvidenceRefs:  evidence,
		Issues:        issues,
		Source:        source,
		ReadAt:        readAt,
	}
}

func containsSideOutput(binding integration.CompiledBinding, output integration.SideOutput) bool {
	for _, side := range binding.SideOutputs {
		if side == output {
			return true
		}
	}
	return false
}

func buildSource(
	sourceKey string,
	successes, failures int,
	matched bool,
	readAt time.Time,
	agg aggregateExecution,
) health.SourceObservation {
	switch {
	case failures == 0 && matched:
		return health.SourceObservation{
			SourceKey:  sourceKey,
			State:      model.AvailabilityAvailable,
			Freshness:  model.FreshnessFresh,
			ObservedAt: latestFromAggregate(agg),
			Retryable:  true,
		}
	case failures == 0 && !matched:
		return emptySource(sourceKey)
	case successes > 0:
		return health.SourceObservation{
			SourceKey:  sourceKey,
			State:      model.AvailabilityUnknown,
			Freshness:  model.FreshnessUnknown,
			ReasonCode: serviceobs.ReasonSourceReadFailed,
			Retryable:  true,
		}
	default:
		return health.SourceObservation{
			SourceKey:  sourceKey,
			State:      model.AvailabilityUnavailable,
			Freshness:  model.FreshnessUnknown,
			ReasonCode: serviceobs.ReasonSourceReadFailed,
			Retryable:  true,
		}
	}
}

func emptySource(sourceKey string) health.SourceObservation {
	return health.SourceObservation{
		SourceKey: sourceKey,
		State:     model.AvailabilityEmpty,
		Freshness: model.FreshnessUnknown,
		Retryable: true,
	}
}

func latestFromAggregate(agg aggregateExecution) time.Time {
	var latest time.Time
	for _, exec := range agg.ByFingerprint {
		if ts := latestObservedAt(exec.Hits); ts.After(latest) {
			latest = ts
		}
	}
	return latest
}

func projectJourneyStages(
	binding integration.CompiledBinding,
	hits []map[string]json.RawMessage,
	evidence []model.EvidenceRef,
	readAt time.Time,
) ([]serviceobs.JourneyStageObservation, []serviceobs.ReconciliationIssue) {
	issues := []serviceobs.ReconciliationIssue{}
	if binding.Params.StageField == "" || len(binding.Params.StageMappings) == 0 {
		return nil, issues
	}
	stageValueToKey := map[string]string{}
	for _, mapping := range binding.Params.StageMappings {
		for _, value := range mapping.Values {
			stageValueToKey[value] = mapping.StageKey
		}
	}
	statusValueToState := map[string]model.JourneyStageState{}
	for _, mapping := range binding.Params.StatusMappings {
		for _, value := range mapping.Values {
			switch mapping.State {
			case "ok":
				statusValueToState[value] = model.JourneyStageStateOK
			case "failed":
				statusValueToState[value] = model.JourneyStageStateFailed
			case "pending":
				statusValueToState[value] = model.JourneyStageStatePending
			}
		}
	}

	byStage := map[string]serviceobs.JourneyStageObservation{}
	evidenceIDs := evidenceIDs(evidence)
	for _, hit := range hits {
		stageRaw := fieldString(hit, binding.Params.StageField)
		stageKey, ok := stageValueToKey[stageRaw]
		if !ok {
			if stageRaw != "" {
				issues = append(issues, serviceobs.ReconciliationIssue{
					ReasonCode: serviceobs.ReasonStageMappingUnavailable,
					Namespace:  binding.Params.StageField,
					Count:      1,
					SourceKey:  binding.SourceKey,
				})
			}
			continue
		}
		state := model.JourneyStageStateUnknown
		if binding.Params.StatusField != "" {
			statusRaw := fieldString(hit, binding.Params.StatusField)
			mapped, ok := statusValueToState[statusRaw]
			if !ok {
				state = model.JourneyStageStateUnknown
			} else {
				state = mapped
			}
		}
		observedAt := recordTimestamp(hit)
		if observedAt.IsZero() {
			observedAt = readAt
		}
		reason := ""
		if state == model.JourneyStageStateUnknown {
			reason = serviceobs.ReasonStageMappingUnavailable
		}
		byStage[stageKey] = serviceobs.JourneyStageObservation{
			StageKey:    stageKey,
			State:       state,
			ObservedAt:  observedAt,
			EvidenceIDs: evidenceIDs,
			ReasonCode:  reason,
			SourceKey:   binding.SourceKey,
		}
	}
	stages := make([]serviceobs.JourneyStageObservation, 0, len(byStage))
	for _, stage := range byStage {
		stages = append(stages, stage)
	}
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].StageKey < stages[j].StageKey })
	return stages, issues
}

func projectTopology(
	binding integration.CompiledBinding,
	hits []map[string]json.RawMessage,
	evidence []model.EvidenceRef,
	readAt time.Time,
) ([]serviceobs.ObservedTopologyHint, []serviceobs.ObservedInstanceHint, []serviceobs.ReconciliationIssue) {
	if err := validateIdentityMappings(binding); err != nil {
		return nil, nil, nil
	}

	byField := map[integration.IdentityRole]integration.IdentityMapping{}
	for _, mapping := range binding.Params.IdentityMappings {
		byField[mapping.Role] = mapping
	}
	caller, hasCaller := byField[integration.IdentityRoleCaller]
	callee, hasCallee := byField[integration.IdentityRoleCallee]
	callerInst, hasCallerInst := byField[integration.IdentityRoleCallerInstance]
	calleeInst, hasCalleeInst := byField[integration.IdentityRoleCalleeInstance]

	evidenceIDs := evidenceIDs(evidence)
	var hints []serviceobs.ObservedTopologyHint
	var instances []serviceobs.ObservedInstanceHint
	for _, hit := range hits {
		observedAt := recordTimestamp(hit)
		if observedAt.IsZero() {
			observedAt = readAt
		}
		if hasCaller && hasCallee {
			sourceValue := fieldString(hit, caller.Field)
			targetValue := fieldString(hit, callee.Field)
			if sourceValue != "" && targetValue != "" {
				hints = append(hints, serviceobs.ObservedTopologyHint{
					Source:      serviceobs.ExternalIdentity{Namespace: caller.Namespace, Value: sourceValue},
					Target:      serviceobs.ExternalIdentity{Namespace: callee.Namespace, Value: targetValue},
					Kind:        "calls",
					ObservedAt:  observedAt,
					EvidenceIDs: evidenceIDs,
					SourceKey:   binding.SourceKey,
				})
			}
		}
		if hasCaller && hasCallerInst {
			component := fieldString(hit, caller.Field)
			instance := fieldString(hit, callerInst.Field)
			if component != "" && instance != "" {
				instances = append(instances, serviceobs.ObservedInstanceHint{
					Component:   serviceobs.ExternalIdentity{Namespace: caller.Namespace, Value: component},
					Instance:    serviceobs.ExternalIdentity{Namespace: callerInst.Namespace, Value: instance},
					ObservedAt:  observedAt,
					EvidenceIDs: evidenceIDs,
					SourceKey:   binding.SourceKey,
				})
			}
		}
		if hasCallee && hasCalleeInst {
			component := fieldString(hit, callee.Field)
			instance := fieldString(hit, calleeInst.Field)
			if component != "" && instance != "" {
				instances = append(instances, serviceobs.ObservedInstanceHint{
					Component:   serviceobs.ExternalIdentity{Namespace: callee.Namespace, Value: component},
					Instance:    serviceobs.ExternalIdentity{Namespace: calleeInst.Namespace, Value: instance},
					ObservedAt:  observedAt,
					EvidenceIDs: evidenceIDs,
					SourceKey:   binding.SourceKey,
				})
			}
		}
	}
	return hints, instances, nil
}

func validateIdentityMappings(binding integration.CompiledBinding) error {
	roleCount := map[integration.IdentityRole]int{}
	for _, mapping := range binding.Params.IdentityMappings {
		roleCount[mapping.Role]++
	}
	for _, count := range roleCount {
		if count > 1 {
			return ErrInvalidIdentityMapping
		}
	}
	return nil
}

func mergeJourneyStages(existing, incoming []serviceobs.JourneyStageObservation) []serviceobs.JourneyStageObservation {
	byKey := map[string]serviceobs.JourneyStageObservation{}
	for _, stage := range existing {
		byKey[stage.StageKey] = stage
	}
	for _, stage := range incoming {
		if current, ok := byKey[stage.StageKey]; !ok || stage.ObservedAt.After(current.ObservedAt) {
			byKey[stage.StageKey] = stage
		}
	}
	out := make([]serviceobs.JourneyStageObservation, 0, len(byKey))
	for _, stage := range byKey {
		out = append(out, stage)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StageKey < out[j].StageKey })
	return out
}

func evidenceIDs(evidence []model.EvidenceRef) []string {
	ids := make([]string, 0, len(evidence))
	for _, ref := range evidence {
		ids = append(ids, ref.EvidenceID)
	}
	sort.Strings(ids)
	return ids
}

// ProjectInstanceHints 暴露 instance 侧输出供单测与后续 reconcile 层使用。
func ProjectInstanceHints(
	binding integration.CompiledBinding,
	hits []map[string]json.RawMessage,
	evidence []model.EvidenceRef,
	readAt time.Time,
) []serviceobs.ObservedInstanceHint {
	_, instances, _ := projectTopology(binding, hits, evidence, readAt)
	return instances
}
