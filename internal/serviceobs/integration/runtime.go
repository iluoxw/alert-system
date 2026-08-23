package integration

import (
	"context"
	"errors"
	"fmt"

	"alert-system/internal/serviceobs/model"
)

// RuntimeBindingSource 按 service_id + environment + Profile 返回已校验的 CompiledIntegration 副本。
//
// 它不得接受 SIP 原文、endpoint、query 或 secret；也不能只按 profile key/version 命中后
// 跳过 digest 与 bundle identity 交叉核对。
type RuntimeBindingSource interface {
	ResolveCompiled(
		ctx context.Context, serviceID, environment string, profile model.Profile,
	) (CompiledIntegration, error)
}

var (
	// ErrRuntimeBindingNotFound 表示没有匹配当前 service/environment/profile 的激活单元。
	ErrRuntimeBindingNotFound = errors.New("integration: 未找到匹配的 CompiledIntegration")
	// ErrRuntimeProfileDigestMismatch 表示 Profile 与 CompiledIntegration 未配成同一激活单元。
	ErrRuntimeProfileDigestMismatch = errors.New("integration: Profile digest 与 CompiledIntegration 不一致")
	// ErrRuntimeIdentityMismatch 表示 CompiledIntegration 与请求上下文身份不一致。
	ErrRuntimeIdentityMismatch = errors.New("integration: CompiledIntegration 身份与请求不一致")
)

// StaticRuntimeBindingSource 是测试与 Batch B fixture 用的只读 binding 来源。
type StaticRuntimeBindingSource struct {
	entries map[string]CompiledIntegration
}

// NewStaticRuntimeBindingSource 构造静态 source。entries 会被深拷贝，避免共享可变 slice。
func NewStaticRuntimeBindingSource(entries map[string]CompiledIntegration) *StaticRuntimeBindingSource {
	copied := make(map[string]CompiledIntegration, len(entries))
	for key, compiled := range entries {
		copied[key] = CloneCompiledIntegration(compiled)
	}
	return &StaticRuntimeBindingSource{entries: copied}
}

func runtimeKey(serviceID, environment, profileKey string, profileVersion int) string {
	return serviceID + "\x1e" + environment + "\x1e" + profileKey + "\x1e" + fmt.Sprintf("%d", profileVersion)
}

// ResolveCompiled 返回深拷贝，并用 compiler 相同 canonical marshal 复核 Profile digest。
func (s *StaticRuntimeBindingSource) ResolveCompiled(
	_ context.Context, serviceID, environment string, profile model.Profile,
) (CompiledIntegration, error) {
	if s == nil || len(s.entries) == 0 {
		return CompiledIntegration{}, ErrRuntimeBindingNotFound
	}
	key := runtimeKey(serviceID, environment, profile.ProfileKey, profile.ProfileVersion)
	compiled, ok := s.entries[key]
	if !ok {
		return CompiledIntegration{}, ErrRuntimeBindingNotFound
	}
	if compiled.ServiceID != serviceID || compiled.Environment != environment ||
		compiled.ProfileKey != profile.ProfileKey || compiled.ProfileVersion != profile.ProfileVersion {
		return CompiledIntegration{}, ErrRuntimeIdentityMismatch
	}
	if err := AssertProfileDigestMatches(profile, compiled.ProfileDigest); err != nil {
		return CompiledIntegration{}, err
	}
	return CloneCompiledIntegration(compiled), nil
}

// AssertProfileDigestMatches 用 integration compiler 相同的 canonical marshal 重算 digest。
func AssertProfileDigestMatches(profile model.Profile, expectedDigest string) error {
	digest, err := ProfileDigest(profile)
	if err != nil {
		return fmt.Errorf("integration: 计算 Profile digest 失败: %w", err)
	}
	if digest != expectedDigest {
		return ErrRuntimeProfileDigestMismatch
	}
	return nil
}

// ProfileDigest 对 decode/normalize 后的 Profile 副本做 canonical marshal 并取 digest。
func ProfileDigest(profile model.Profile) (string, error) {
	normalized := profile
	normalized.Normalize()
	raw, err := CanonicalMarshal(normalized)
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

// CanonicalMarshal 输出与 compiler 相同的稳定缩进 JSON。
func CanonicalMarshal(value any) ([]byte, error) {
	return marshalCanonical(value)
}

// CloneCompiledIntegration 深拷贝 CompiledIntegration，避免 TOCTOU 混版。
func CloneCompiledIntegration(compiled CompiledIntegration) CompiledIntegration {
	cloned := compiled
	cloned.RootEntityIDs = append([]string(nil), compiled.RootEntityIDs...)
	cloned.Bindings = make([]CompiledBinding, len(compiled.Bindings))
	for i, binding := range compiled.Bindings {
		cloned.Bindings[i] = cloneCompiledBinding(binding)
	}
	cloned.TopologyPolicy = cloneCompiledTopologyPolicy(compiled.TopologyPolicy)
	return cloned
}

func cloneCompiledBinding(binding CompiledBinding) CompiledBinding {
	cloned := binding
	cloned.ResolvedTargetIDs = append([]string(nil), binding.ResolvedTargetIDs...)
	cloned.FocusKind = binding.FocusKind
	cloned.ResolvedJourneyStages = make([]CompiledJourneyStageTarget, len(binding.ResolvedJourneyStages))
	for i, stage := range binding.ResolvedJourneyStages {
		cloned.ResolvedJourneyStages[i] = CompiledJourneyStageTarget{
			StageKey:    stage.StageKey,
			EntityIDs:   append([]string(nil), stage.EntityIDs...),
			RelationIDs: append([]string(nil), stage.RelationIDs...),
		}
	}
	cloned.SideOutputs = append([]SideOutput(nil), binding.SideOutputs...)
	cloned.Params = cloneTemplateParams(binding.Params)
	if len(binding.IdentityMappings) > 0 {
		cloned.IdentityMappings = append([]IdentityMapping(nil), binding.IdentityMappings...)
	}
	return cloned
}

func cloneTemplateParams(params TemplateParams) TemplateParams {
	cloned := params
	cloned.Labels = append([]LabelEquality(nil), params.Labels...)
	cloned.CommonLabels = append([]LabelEquality(nil), params.CommonLabels...)
	cloned.NumeratorLabels = append([]LabelEquality(nil), params.NumeratorLabels...)
	cloned.DenominatorLabels = append([]LabelEquality(nil), params.DenominatorLabels...)
	cloned.GroupBy = append([]string(nil), params.GroupBy...)
	cloned.SelectFields = append([]string(nil), params.SelectFields...)
	cloned.StageMappings = append([]StageValueMapping(nil), params.StageMappings...)
	cloned.StatusMappings = append([]StatusValueMapping(nil), params.StatusMappings...)
	if params.Quantile != nil {
		value := *params.Quantile
		cloned.Quantile = &value
	}
	if params.RangeSeconds != nil {
		value := *params.RangeSeconds
		cloned.RangeSeconds = &value
	}
	if params.RowLimit != nil {
		value := *params.RowLimit
		cloned.RowLimit = &value
	}
	return cloned
}

func cloneCompiledTopologyPolicy(policy CompiledTopologyPolicy) CompiledTopologyPolicy {
	cloned := CompiledTopologyPolicy{
		AliasNamespaces:   append([]string(nil), policy.AliasNamespaces...),
		StaleAfterSeconds: policy.StaleAfterSeconds,
		MaxHints:          policy.MaxHints,
	}
	cloned.IdentityMappings = append([]IdentityMapping(nil), policy.IdentityMappings...)
	return cloned
}
