package integration

import (
	"fmt"
	"sort"
)

// 本文件是 provider binding 的编译期校验。
//
// 它回答四个问题，且每一个都 fail closed：
//
//  1. kind/target 是否指向 Profile 里真实存在的对象；
//  2. 每种 kind 的多重性是否满足（signal/journey 恰好一个，focus 至多一个，
//     evidence 按 class 至多一个）；
//  3. template 是否允许充当这个 kind、参数是否恰好满足契约、输出字段是否是它真的会产出的；
//  4. tool 是否已发布、是否在 package 的 tool_refs 里、capability 是否等于 ToolDescriptor 的
//     source_adapter、template 所需权限是否被 descriptor 覆盖。
//
// 还有一条不那么显然但同样重要的规则：**side output 参与唯一性判定**。一次 trace 读取本来
// 就能同时给出 journey 阶段、focus 结论、trace EvidenceRef 与 topology hints；如果 package 又
// 为 focus 或 trace evidence 单独建一条 binding，那就是同一次查询被发两遍。compiler 因此把
// "谁提供了 focus / 谁提供了某个 evidence class"算成一个集合，重复即拒绝。

// ToolDescriptorSource 是 compiler 需要的 Tool catalog 能力子集。
//
// 用最小接口注入而不是直接依赖 *tools.Catalog：integration 包因此不必知道 catalog 怎么加载，
// 测试也能给一个只含两三个 descriptor 的替身。
type ToolDescriptorSource interface {
	// Get 返回已发布 descriptor。不存在时返回错误。
	Get(key string) (ToolDescriptor, error)
}

// ToolDescriptor 是 compiler 真正需要的 descriptor 字段。
//
// 它刻意不是 model.ToolDescriptor 的别名：compiler 只需要 adapter 与权限两项，
// 收窄接口能避免把整个 Page View 契约拖进编写期包。
type ToolDescriptor struct {
	Key                 string
	SourceAdapter       string
	RequiredPermissions []string
}

// resolvedBinding 是一条通过全部校验的 binding，已经带上由受信 registry 推导的事实。
type resolvedBinding struct {
	Key         string
	Kind        BindingKind
	TargetRef   string
	ToolKey     string
	TemplateRef string
	Params      TemplateParams
	// 以下四项全部由 registry 推导，package 无法自报。
	SourceKey          string
	Capability         Capability
	RequiredPermission string
	EvidenceClass      EvidenceClass
	SideOutputs        []SideOutput
}

// bindingContext 收集 Profile 模板里可被引用的目标集合。
type bindingContext struct {
	signalKeys map[string]SignalBinding
	focusKeys  map[string]struct{}
	journeys   map[string]Journey
	toolRefs   map[string]struct{}
}

func newBindingContext(pkg Package) bindingContext {
	ctx := bindingContext{
		signalKeys: map[string]SignalBinding{},
		focusKeys:  map[string]struct{}{},
		journeys:   map[string]Journey{},
		toolRefs:   map[string]struct{}{},
	}
	for _, binding := range pkg.ProfileTemplate.SignalBindings {
		ctx.signalKeys[binding.Key] = binding
	}
	for _, focus := range pkg.ProfileTemplate.FocusKinds {
		ctx.focusKeys[focus.Key] = struct{}{}
	}
	for _, journey := range pkg.ProfileTemplate.Journeys {
		ctx.journeys[journey.Key] = journey
	}
	for _, ref := range pkg.ProfileTemplate.ToolRefs {
		ctx.toolRefs[ref] = struct{}{}
	}
	return ctx
}

// validateProviderBindings 校验全部 provider binding 并返回解析结果。
func validateProviderBindings(pkg Package, tools ToolDescriptorSource) ([]resolvedBinding, error) {
	ctx := newBindingContext(pkg)

	seenKeys := map[string]struct{}{}
	// 多重性账本。focusProviders / evidenceProviders 同时统计显式 binding 与 side output，
	// 因此"journey 已经顺带给出 focus"再加一条 focus binding 会被判成重复。
	signalProviders := map[string]string{}
	journeyProviders := map[string]string{}
	// explicitFocus 与 sideFocus 分开记账。
	//
	// 多个来源各自报告 focus 结论是**正常**的：coordinator 按 matched > unknown > empty 合并
	// 它们，"traces 命中而 logs 不知道"必须是 matched。所以 side output 之间不冲突。
	// 真正的死配置是：某个来源已经顺带给出这个 focus 的判定，package 还为它单独建一条
	// kind=focus binding——那是第三次查询，且结论重复。
	explicitFocus := map[string]string{}
	sideFocus := map[string]string{}
	evidenceProviders := map[string]string{}

	resolved := make([]resolvedBinding, 0, len(pkg.Bindings))
	for _, binding := range pkg.Bindings {
		if _, exists := seenKeys[binding.Key]; exists {
			return nil, failf(ReasonDuplicateKey, "provider_bindings[%s] 重复", binding.Key)
		}
		seenKeys[binding.Key] = struct{}{}

		if !binding.Kind.IsValid() {
			return nil, failf(ReasonUnknownBindingKind,
				"provider_bindings[%s].kind=%q 不在允许的 kind 内", binding.Key, binding.Kind)
		}
		template, ok := LookupTemplate(binding.TemplateRef)
		if !ok {
			return nil, failf(ReasonUnknownTemplate,
				"provider_bindings[%s] 引用未注册的 template %s", binding.Key, binding.TemplateRef)
		}
		if !template.AllowsKind(binding.Kind) {
			return nil, failf(ReasonTemplateKindMismatch,
				"provider_bindings[%s] template %s 不能充当 kind=%s",
				binding.Key, template.Ref, binding.Kind)
		}
		if err := template.validateParams(binding.Key, binding.Params); err != nil {
			return nil, err
		}
		if err := validateRatioSelectors(binding, template); err != nil {
			return nil, err
		}
		if err := validateToolBinding(binding, template, ctx, tools); err != nil {
			return nil, err
		}

		// target 解析 + 多重性。
		focusForBinding := ""
		switch binding.Kind {
		case KindSignal:
			signal, ok := ctx.signalKeys[binding.TargetRef]
			if !ok {
				return nil, failf(ReasonDanglingRef,
					"provider_bindings[%s].target_ref=%s 不是已声明的 signal binding",
					binding.Key, binding.TargetRef)
			}
			// provider binding 与它服务的 signal 必须用同一个 tool：不一致意味着 Profile 里
			// 声明的 tool 和真正执行的 tool 是两个东西，权限与审计都会对不上。
			if signal.ToolKey != binding.ToolKey {
				return nil, failf(ReasonBindingToolMismatch,
					"provider_bindings[%s].tool_key=%s 与 signal_bindings[%s].tool_key=%s 不一致",
					binding.Key, binding.ToolKey, signal.Key, signal.ToolKey)
			}
			if !template.allowsOutput(signal.OutputField) {
				return nil, failf(ReasonTemplateOutputUnknown,
					"signal_bindings[%s].output_field=%s 不是 template %s 声明的输出字段",
					signal.Key, signal.OutputField, template.Ref)
			}
			if previous, exists := signalProviders[binding.TargetRef]; exists {
				return nil, failf(ReasonDuplicateBindingTarget,
					"signal %s 同时被 %s 与 %s 绑定，必须恰好一条",
					binding.TargetRef, previous, binding.Key)
			}
			signalProviders[binding.TargetRef] = binding.Key
		case KindJourney:
			journey, ok := ctx.journeys[binding.TargetRef]
			if !ok {
				return nil, failf(ReasonDanglingRef,
					"provider_bindings[%s].target_ref=%s 不是已声明的 journey",
					binding.Key, binding.TargetRef)
			}
			if previous, exists := journeyProviders[binding.TargetRef]; exists {
				return nil, failf(ReasonDuplicateBindingTarget,
					"journey %s 同时被 %s 与 %s 绑定，必须恰好一条",
					binding.TargetRef, previous, binding.Key)
			}
			journeyProviders[binding.TargetRef] = binding.Key
			// journey 的 focus 语义来自它声明的 focus_kind：trace 读取顺带给出的 focus
			// 结论属于那个 focus kind。
			focusForBinding = journey.FocusKind
		case KindFocus:
			if _, ok := ctx.focusKeys[binding.TargetRef]; !ok {
				return nil, failf(ReasonDanglingRef,
					"provider_bindings[%s].target_ref=%s 不是已声明的 focus kind",
					binding.Key, binding.TargetRef)
			}
			focusForBinding = binding.TargetRef
		case KindEvidence:
			if _, ok := ctx.focusKeys[binding.TargetRef]; !ok {
				return nil, failf(ReasonDanglingRef,
					"provider_bindings[%s].target_ref=%s 不是已声明的 focus kind",
					binding.Key, binding.TargetRef)
			}
			if template.EvidenceClass == "" {
				return nil, failf(ReasonTemplateKindMismatch,
					"provider_bindings[%s] template %s 没有声明 evidence class",
					binding.Key, template.Ref)
			}
			// 同一 focus 的不同 class 可以共存（logs + traces + access），
			// 只有相同 (target, class/source) 才算重复。
			slot := evidenceSlot(binding.TargetRef, template)
			if previous, exists := evidenceProviders[slot]; exists {
				return nil, failf(ReasonDuplicateBindingTarget,
					"focus %s 的 %s 证据同时被 %s 与 %s 绑定",
					binding.TargetRef, template.EvidenceClass, previous, binding.Key)
			}
			evidenceProviders[slot] = binding.Key
			focusForBinding = binding.TargetRef
		}

		// focus 记账：显式 binding 至多一条，且不能与任何 side output 提供者共存。
		if binding.Kind == KindFocus {
			if previous, exists := explicitFocus[focusForBinding]; exists {
				return nil, failf(ReasonDuplicateBindingTarget,
					"focus %s 同时被 %s 与 %s 显式绑定，至多一条",
					focusForBinding, previous, binding.Key)
			}
			explicitFocus[focusForBinding] = binding.Key
		} else if focusForBinding != "" && template.ProvidesSideOutput(SideOutputFocus) {
			sideFocus[focusForBinding] = binding.Key
		}
		// trace EvidenceRef 是 side output 时，同一 focus 不能再显式绑 traces class。
		if focusForBinding != "" && template.ProvidesSideOutput(SideOutputTraceEvidence) &&
			binding.Kind != KindEvidence {
			slot := focusForBinding + "\x1e" + string(EvidenceClassTraces)
			if previous, exists := evidenceProviders[slot]; exists {
				return nil, failf(ReasonDuplicateBindingTarget,
					"focus %s 的 traces 证据同时来自 %s 与 %s",
					focusForBinding, previous, binding.Key)
			}
			evidenceProviders[slot] = binding.Key
		}

		resolved = append(resolved, resolvedBinding{
			Key:                binding.Key,
			Kind:               binding.Kind,
			TargetRef:          binding.TargetRef,
			ToolKey:            binding.ToolKey,
			TemplateRef:        binding.TemplateRef,
			Params:             binding.Params,
			SourceKey:          template.SourceKey,
			Capability:         template.Capability,
			RequiredPermission: template.RequiredPermission,
			EvidenceClass:      template.EvidenceClass,
			SideOutputs:        append([]SideOutput(nil), template.SideOutputs...),
		})
	}

	// 缺失的必填多重性：每个 signal 与每个 journey 都必须恰好有一条 binding。
	// 没有 binding 的 signal 会永久 Unknown，没有 binding 的 journey 会永久没有阶段——
	// 两者都是"配了但永远看不到"，与 golden 投影校验同一口径。
	if missing := missingTargets(ctx.signalKeys, signalProviders); len(missing) > 0 {
		return nil, failf(ReasonBindingNotBound,
			"signal_bindings %v 没有 provider binding，会永久 Unknown", missing)
	}
	if missing := missingJourneys(ctx.journeys, journeyProviders); len(missing) > 0 {
		return nil, failf(ReasonBindingNotBound,
			"journeys %v 没有 provider binding，阶段会永久缺失", missing)
	}
	// 显式 focus binding 与已有 side output 冲突：那是第三次查询，结论还重复。
	for focusKind, explicit := range explicitFocus {
		if side, exists := sideFocus[focusKind]; exists {
			return nil, failf(ReasonDuplicateBindingTarget,
				"focus %s 的判定已由 %s 的 side output 提供，%s 这条显式 binding 是重复查询",
				focusKind, side, explicit)
		}
	}

	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].Key < resolved[j].Key })
	return resolved, nil
}

func evidenceSlot(target string, template Template) string {
	return target + "\x1e" + string(template.EvidenceClass)
}

func missingTargets(declared map[string]SignalBinding, bound map[string]string) []string {
	missing := make([]string, 0)
	for key := range declared {
		if _, ok := bound[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

func missingJourneys(declared map[string]Journey, bound map[string]string) []string {
	missing := make([]string, 0)
	for key := range declared {
		if _, ok := bound[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// validateToolBinding 交叉校验 tool 与 template。
//
// 四道检查缺一不可：tool 必须在 package 声明的 tool_refs 里（否则 Profile 的 tool 清单与
// 真正执行的工具不一致）、必须已发布、capability 必须等于 descriptor 的 source_adapter
// （否则会用 Prometheus 的工具去执行 OpenObserve 模板）、template 所需权限必须被 descriptor
// 覆盖（否则运行时会用一个不要求该权限的工具去读需要该权限的数据）。
func validateToolBinding(
	binding ProviderBinding, template Template, ctx bindingContext, tools ToolDescriptorSource,
) error {
	if _, ok := ctx.toolRefs[binding.ToolKey]; !ok {
		return failf(ReasonDanglingRef,
			"provider_bindings[%s].tool_key=%s 未在 profile_template.tool_refs 中声明",
			binding.Key, binding.ToolKey)
	}
	descriptor, err := tools.Get(binding.ToolKey)
	if err != nil {
		return failf(ReasonToolNotPublished,
			"provider_bindings[%s].tool_key=%s 未发布", binding.Key, binding.ToolKey)
	}
	if descriptor.SourceAdapter != string(template.Capability) {
		return failf(ReasonTemplateCapability,
			"provider_bindings[%s] template %s 的 capability=%s 与 tool %s 的 source_adapter=%s 不一致",
			binding.Key, template.Ref, template.Capability, descriptor.Key, descriptor.SourceAdapter)
	}
	if !containsPermission(descriptor.RequiredPermissions, template.RequiredPermission) {
		return failf(ReasonTemplatePermission,
			"provider_bindings[%s] template %s 需要权限 %s，但 tool %s 未声明它",
			binding.Key, template.Ref, template.RequiredPermission, descriptor.Key)
	}
	return nil
}

func containsPermission(declared []string, required string) bool {
	for _, permission := range declared {
		if permission == required {
			return true
		}
	}
	return false
}

// validateRatioSelectors 拒绝会恒算成 100% 的退化比率，以及同一有效选择器内的重复 label。
//
// 退化判定是"同 metric 且两侧有效选择器完全相同"：此时分子恒等于分母，任何非零样本都会
// 算出 100%。E2B 的第一版 error binding 正是这个形状。
func validateRatioSelectors(binding ProviderBinding, template Template) error {
	if !template.usesRatioSelectors() {
		// 非比率模板不该出现三组 label 槽位；validateParams 已经按 Optional 拒绝了，
		// 这里只做一次显式断言，避免将来放宽 Optional 时静默失效。
		if len(binding.Params.CommonLabels) > 0 || len(binding.Params.NumeratorLabels) > 0 ||
			len(binding.Params.DenominatorLabels) > 0 {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] template %s 不接受比率 label 分组", binding.Key, template.Ref)
		}
		return nil
	}

	numerator := effectiveSelector(binding.Params.CommonLabels, binding.Params.NumeratorLabels)
	denominator := effectiveSelector(binding.Params.CommonLabels, binding.Params.DenominatorLabels)
	if err := assertUniqueLabelNames(binding.Key, "numerator", numerator); err != nil {
		return err
	}
	if err := assertUniqueLabelNames(binding.Key, "denominator", denominator); err != nil {
		return err
	}
	if binding.Params.NumeratorMetric == binding.Params.DenominatorMetric &&
		selectorSignature(numerator) == selectorSignature(denominator) {
		return failf(ReasonRatioDegenerate,
			"provider_bindings[%s] 的分子与分母是同一 metric 且选择器完全相同，比率恒为 100%%",
			binding.Key)
	}
	return nil
}

// effectiveSelector 把 common 与单侧 label 合并成该侧真正生效的选择器。
func effectiveSelector(common, side []LabelEquality) []LabelEquality {
	out := make([]LabelEquality, 0, len(common)+len(side))
	out = append(out, common...)
	out = append(out, side...)
	return out
}

// assertUniqueLabelNames 拒绝同名 label：common 与某一侧重名同样拒绝。
//
// 重复 label 名在 PromQL 里是"最后一个生效"或直接语法错误，两种都不是 package 作者的意图；
// 与其让 adapter 去猜，不如在编译期就说清。
func assertUniqueLabelNames(bindingKey, side string, selector []LabelEquality) error {
	seen := make(map[string]struct{}, len(selector))
	for _, label := range selector {
		if _, exists := seen[label.Name]; exists {
			return failf(ReasonTemplateParamInvalid,
				"provider_bindings[%s] 的 %s 选择器里 label %s 重复", bindingKey, side, label.Name)
		}
		seen[label.Name] = struct{}{}
	}
	return nil
}

// selectorSignature 生成与顺序无关的选择器指纹。
func selectorSignature(selector []LabelEquality) string {
	parts := make([]string, 0, len(selector))
	for _, label := range selector {
		parts = append(parts, label.Name+"="+label.Value)
	}
	sort.Strings(parts)
	signature := ""
	for _, part := range parts {
		signature += part + "\x1e"
	}
	return signature
}

// usesRatioSelectors 判断该模板是否使用三组比率 label。
func (t Template) usesRatioSelectors() bool {
	for _, slot := range t.Optional {
		if slot == SlotNumeratorLabels || slot == SlotDenominatorLabels {
			return true
		}
	}
	return false
}

// String 只输出 key/kind/target 与推导事实，绝不输出参数值。
func (b resolvedBinding) String() string {
	return fmt.Sprintf("resolvedBinding{Key:%s, Kind:%s, Target:%s, Source:%s}",
		b.Key, b.Kind, b.TargetRef, b.SourceKey)
}
