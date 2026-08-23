package integration

import (
	"sort"
	"strings"
	"unicode"
)

// validateStageStatusMappings 校验 stage/status 封闭字面映射。
func validateStageStatusMappings(binding ProviderBinding, template Template, ctx bindingContext) error {
	params := binding.Params
	if len(params.StageMappings) == 0 && len(params.StatusMappings) == 0 {
		if params.StageField != "" && template.ProvidesSideOutput(SideOutputFocus) {
			// stage_field 存在但没有 mappings 是合法的 draft 形状；adapter 会局部 unknown。
		}
		return nil
	}

	if len(params.StageMappings) > 0 && params.StageField == "" {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] 有 stage_mappings 但缺少 stage_field", binding.Key)
	}
	if len(params.StatusMappings) > 0 && params.StatusField == "" {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] 有 status_mappings 但缺少 status_field", binding.Key)
	}

	journeyStages := map[string]struct{}{}
	if binding.Kind == KindJourney {
		journey, ok := ctx.journeys[binding.TargetRef]
		if ok {
			for _, stage := range journey.Stages {
				journeyStages[stage.Key] = struct{}{}
			}
		}
	}

	seenStageValues := map[string]string{}
	for _, mapping := range params.StageMappings {
		if mapping.StageKey == "" {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] stage_mappings 缺少 stage_key", binding.Key)
		}
		if binding.Kind == KindJourney {
			if _, ok := journeyStages[mapping.StageKey]; !ok {
				return failf(ReasonTemplateParamInvalid,
					"provider_bindings[%s] stage_mappings 引用未知 stage %s",
					binding.Key, mapping.StageKey)
			}
		}
		if len(mapping.Values) == 0 {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] stage_mappings[%s] 缺少 values", binding.Key, mapping.StageKey)
		}
		for _, value := range mapping.Values {
			if err := assertLiteralMappingValue(binding.Key, "stage", value); err != nil {
				return err
			}
			if previous, exists := seenStageValues[value]; exists {
				return failf(ReasonTemplateParamInvalid,
					"provider_bindings[%s] stage value %q 同时映射到 %s 与 %s",
					binding.Key, value, previous, mapping.StageKey)
			}
			seenStageValues[value] = mapping.StageKey
		}
	}

	allowedStates := map[string]struct{}{
		"ok":      {},
		"failed":  {},
		"pending": {},
	}
	seenStatusValues := map[string]string{}
	for _, mapping := range params.StatusMappings {
		if _, ok := allowedStates[mapping.State]; !ok {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] status_mappings 含未知 state %q", binding.Key, mapping.State)
		}
		if len(mapping.Values) == 0 {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] status_mappings[%s] 缺少 values", binding.Key, mapping.State)
		}
		for _, value := range mapping.Values {
			if err := assertLiteralMappingValue(binding.Key, "status", value); err != nil {
				return err
			}
			if previous, exists := seenStatusValues[value]; exists {
				return failf(ReasonTemplateParamInvalid,
					"provider_bindings[%s] status value %q 同时映射到 %s 与 %s",
					binding.Key, value, previous, mapping.State)
			}
			seenStatusValues[value] = mapping.State
		}
	}
	return nil
}

func assertLiteralMappingValue(bindingKey, kind, value string) error {
	if value == "" {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] %s mapping 含空 value", bindingKey, kind)
	}
	if len(value) > 200 {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] %s mapping value 超长", bindingKey, kind)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] %s mapping value 含非法字符", bindingKey, kind)
		}
	}
	if strings.ContainsAny(value, `'"\\`) {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] %s mapping value 含引号或反斜杠", bindingKey, kind)
	}
	return nil
}

// validateIdentityMappings 校验 topology identity 映射。
func validateIdentityMappings(binding ProviderBinding, template Template, policy TopologyPolicy) error {
	mappings := binding.Params.IdentityMappings
	if len(mappings) == 0 {
		return nil
	}
	if !template.ProvidesSideOutput(SideOutputTopologyHints) && binding.Kind != KindJourney {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] template %s 不接受 identity_mappings", binding.Key, template.Ref)
	}

	allowedNamespaces := map[string]struct{}{}
	for _, namespace := range policy.AliasNamespaces {
		allowedNamespaces[namespace] = struct{}{}
	}

	roleCount := map[IdentityRole]int{}
	seenFieldRole := map[string]struct{}{}
	for _, mapping := range mappings {
		if mapping.Field == "" || mapping.Namespace == "" {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] identity_mappings 缺少 field 或 namespace", binding.Key)
		}
		if !mapping.Role.IsValid() {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] identity_mappings 含未知 role %q", binding.Key, mapping.Role)
		}
		if _, ok := allowedNamespaces[mapping.Namespace]; !ok {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] identity_mappings namespace %q 不在 allowlist",
				binding.Key, mapping.Namespace)
		}
		fieldRoleKey := mapping.Field + "\x1e" + string(mapping.Role)
		if _, exists := seenFieldRole[fieldRoleKey]; exists {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] identity_mappings 重复 field+role %s/%s",
				binding.Key, mapping.Field, mapping.Role)
		}
		seenFieldRole[fieldRoleKey] = struct{}{}
		roleCount[mapping.Role]++
		if roleCount[mapping.Role] > 1 {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] identity_mappings role %s 重复", binding.Key, mapping.Role)
		}
	}

	hasCaller := roleCount[IdentityRoleCaller] == 1
	hasCallee := roleCount[IdentityRoleCallee] == 1
	if template.ProvidesSideOutput(SideOutputTopologyHints) {
		if hasCaller != hasCallee {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] topology hints 需要 caller 与 callee 各恰好一条", binding.Key)
		}
	}
	if roleCount[IdentityRoleCallerInstance] > 0 && roleCount[IdentityRoleCaller] == 0 {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] caller_instance 缺少 caller", binding.Key)
	}
	if roleCount[IdentityRoleCalleeInstance] > 0 && roleCount[IdentityRoleCallee] == 0 {
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] callee_instance 缺少 callee", binding.Key)
	}

	sort.SliceStable(mappings, func(i, j int) bool {
		left, right := mappings[i], mappings[j]
		if left.Role != right.Role {
			return left.Role < right.Role
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		return left.Field < right.Field
	})
	return nil
}
