package observe

import (
	"sort"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

// sourcePlanEntry 是一次请求在某个 source 上选中的 compiled bindings。
type sourcePlanEntry struct {
	sourceKey string
	bindings  []integration.CompiledBinding
}

// runtimeSourcePlan 完全由 CompiledIntegration bindings 决定，不再看 Profile renderer。
type runtimeSourcePlan struct {
	Signals      []integration.CompiledBinding
	Traces       []integration.CompiledBinding
	Logs         []integration.CompiledBinding
	Access       []integration.CompiledBinding
	FocusUnknown bool
}

const (
	sourcePrometheus      = serviceobs.SourceKeyPrometheus
	sourceOpenObserveLogs = serviceobs.SourceKeyOpenObserveLogs
	sourceOpenObserveTraces = serviceobs.SourceKeyOpenObserveTraces
	sourceOpenObserveAccess = serviceobs.SourceKeyOpenObserveAccess
)

// planRuntimeSources 按 purpose、focus 与 compiled bindings 选择本次要执行的来源。
func planRuntimeSources(
	purpose serviceobs.Purpose,
	focus *model.Focus,
	requestedBindingKeys []string,
	compiled integration.CompiledIntegration,
) runtimeSourcePlan {
	plan := runtimeSourcePlan{}
	if len(compiled.Bindings) == 0 {
		if purpose.NeedsFocusEvidence() && focus != nil {
			plan.FocusUnknown = true
		}
		return plan
	}

	keyFilter := map[string]struct{}{}
	if len(requestedBindingKeys) > 0 {
		for _, key := range requestedBindingKeys {
			keyFilter[key] = struct{}{}
		}
	}

	focusKind := ""
	if focus != nil {
		focusKind = focus.Kind
	}

	focusAffecting := 0
	for _, binding := range compiled.Bindings {
		if len(keyFilter) > 0 && binding.Kind == integration.KindSignal {
			if _, ok := keyFilter[binding.Key]; !ok {
				continue
			}
		}
		switch binding.SourceKey {
		case sourcePrometheus:
			if binding.Kind == integration.KindSignal {
				plan.Signals = append(plan.Signals, binding)
			}
		case sourceOpenObserveTraces:
			if shouldPlanTraces(purpose, focus, binding, focusKind) {
				plan.Traces = append(plan.Traces, binding)
				if bindingAffectsFocus(binding, focusKind) {
					focusAffecting++
				}
			}
		case sourceOpenObserveLogs:
			if shouldPlanEvidence(purpose, focus, binding, focusKind) {
				plan.Logs = append(plan.Logs, binding)
				if bindingAffectsFocus(binding, focusKind) {
					focusAffecting++
				}
			}
		case sourceOpenObserveAccess:
			if shouldPlanEvidence(purpose, focus, binding, focusKind) {
				plan.Access = append(plan.Access, binding)
				if bindingAffectsFocus(binding, focusKind) {
					focusAffecting++
				}
			}
		}
	}

	sortBindings(plan.Signals)
	sortBindings(plan.Traces)
	sortBindings(plan.Logs)
	sortBindings(plan.Access)

	if purpose.NeedsFocusEvidence() && focus != nil && focusAffecting == 0 {
		plan.FocusUnknown = true
	}
	return plan
}

// PlanRuntimeSourcesForTest 导出 planner 供测试使用。
func PlanRuntimeSourcesForTest(
	purpose serviceobs.Purpose,
	focus *model.Focus,
	requestedBindingKeys []string,
	compiled integration.CompiledIntegration,
) runtimeSourcePlan {
	return planRuntimeSources(purpose, focus, requestedBindingKeys, compiled)
}

func shouldPlanTraces(
	purpose serviceobs.Purpose, focus *model.Focus, binding integration.CompiledBinding, focusKind string,
) bool {
	if !purpose.NeedsFocusEvidence() {
		return false
	}
	if focus == nil {
		return false
	}
	switch binding.Kind {
	case integration.KindJourney, integration.KindFocus, integration.KindEvidence:
		return bindingMatchesFocusKind(binding, focusKind)
	default:
		return false
	}
}

func shouldPlanEvidence(
	purpose serviceobs.Purpose, focus *model.Focus, binding integration.CompiledBinding, focusKind string,
) bool {
	if !purpose.NeedsFocusEvidence() {
		return false
	}
	if focus == nil {
		return false
	}
	if binding.Kind != integration.KindEvidence && binding.Kind != integration.KindFocus {
		return false
	}
	return bindingMatchesFocusKind(binding, focusKind)
}

func bindingMatchesFocusKind(binding integration.CompiledBinding, focusKind string) bool {
	if focusKind == "" {
		return false
	}
	switch binding.Kind {
	case integration.KindJourney:
		return binding.FocusKind == focusKind
	default:
		return binding.TargetRef == focusKind
	}
}

func bindingAffectsFocus(binding integration.CompiledBinding, focusKind string) bool {
	if !bindingMatchesFocusKind(binding, focusKind) {
		return false
	}
	if binding.Kind == integration.KindFocus || binding.Kind == integration.KindEvidence {
		return true
	}
	if binding.Kind == integration.KindJourney {
		for _, side := range binding.SideOutputs {
			if side == integration.SideOutputFocus {
				return true
			}
		}
	}
	return false
}

func containsKey(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

func sortBindings(bindings []integration.CompiledBinding) {
	sort.SliceStable(bindings, func(i, j int) bool {
		return bindings[i].Key < bindings[j].Key
	})
}
