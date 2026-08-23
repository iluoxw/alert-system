package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
)

// Compiler 把一份 SIP 文档编译成使用真实 ID 的 v1 Profile。
//
// 编译顺序刻意固定，且每一步都 fail closed：
//
//  1. schema/unknown-field/禁止键/版本兼容（Validator.Decode）；
//  2. 纯结构校验与引用完整性（不查库）；
//  3. provider template 存在性、参数契约与 output 字段交叉校验；
//  4. tool_refs 必须全部已发布；
//  5. selector 唯一解析成真实 ID——零命中或多命中都失败，没有 entity_key fallback，
//     也没有任何 fixture ID 兜底路径；
//  6. 生成 Profile 后再走一遍**现有** Profile schema + 结构 + tool catalog 校验；
//  7. 输出不可变编译清单（含内容摘要与回滚所需的上一版本引用）。
type Compiler struct {
	// Validator 校验 SIP 文档，必填。
	Validator *Validator
	// Profiles 校验编译产物，必填。它就是 Profile Registry 用的那套校验，因此"编译产物
	// 一定是合法 v1 Profile"不是靠 compiler 自己重写一份规则。
	Profiles *profile.DocumentValidator
	// Tools 提供 ToolDescriptor 查询，必填。没有它就无法校验 capability 与权限，
	// 一条 binding 可以用 Prometheus 的工具去执行 OpenObserve 模板。
	Tools ToolDescriptorSource
	// Resolver 解析 SIP selector，必填。
	Resolver SelectorResolver
	// ProfileReferences 是 Profile Registry 用的引用解析器，必填。
	//
	// 它与 Resolver 是两件事：Resolver 把 entity_key 解析成 ID；ProfileReferences 校验
	// 生成出来的 Profile 在**同一套 Registry 规则**下是否可加载（root 同环境或 global、
	// 从 service entity 有界可达）。少了它，compiler 会编出一份 Registry 随后会
	// quarantine 的 Profile。
	ProfileReferences profile.Resolver
	// MaxReachableDepth 为 0 时使用 profile.DefaultMaxReachableDepth。
	MaxReachableDepth int
	// AllowPublished 默认 false：compiler 拒绝 status=published 的 package。
	//
	// `status: published` 只是发布意图，不是自授权。Task 9 的发布 gate 在拿到 discovery
	// digest、owner attestation、HTTPS 与逐条 sandbox_id 证据之后才是唯一有资格把它置 true
	// 的调用方；它刻意不接任何普通运行配置，避免一个 YAML 开关就能激活未验证的绑定。
	AllowPublished bool
	// Now 为 nil 时使用 time.Now；显式注入以便测试固定时钟。
	Now func() time.Time
}

// CompileRequest 是一次编译请求。
type CompileRequest struct {
	// Raw 是 package 文档原始字节。ContentDigest 对它取摘要——被审查的就是这份字节。
	Raw []byte
	// PreviousProfileDigest / PreviousIntegrationDigest 是上一**对**已发布产物的摘要，
	// 用于回滚引用。可为空（首个版本）。
	PreviousProfileDigest     string
	PreviousIntegrationDigest string
}

// CompileManifest 是不可变编译清单。
//
// 它只含发布与回滚需要的元数据：绝不包含凭据、endpoint、查询文本或 package 正文。
type CompileManifest struct {
	PackageKey      string   `json:"package_key"`
	PackageVersion  int      `json:"package_version"`
	PackageStatus   Status   `json:"package_status"`
	CompilerVersion int      `json:"compiler_version"`
	ProfileKey      string   `json:"profile_key"`
	ProfileVersion  int      `json:"profile_version"`
	Environment     string   `json:"environment"`
	ServiceID       string   `json:"service_id"`
	ServiceEntityID string   `json:"service_entity_id"`
	RootEntityIDs   []string `json:"root_entity_ids"`
	TemplateRefs    []string `json:"template_refs"`
	ContentDigest   string   `json:"content_digest"`
	ProfileDigest   string   `json:"profile_digest"`
	// IntegrationDigest 与 ProfileDigest 一起构成唯一激活单元。加载器必须同时校验两者：
	// 只认一个摘要就无法发现"两半来自不同版本"。
	IntegrationDigest string `json:"integration_digest"`
	// PreviousProfileDigest / PreviousIntegrationDigest 为空表示这是首个版本。回滚是重新
	// 激活上一**对**不可变产物，而不是就地修改 Profile 或只回退一半。
	PreviousProfileDigest     string    `json:"previous_profile_digest,omitempty"`
	PreviousIntegrationDigest string    `json:"previous_integration_digest,omitempty"`
	CompiledAt                time.Time `json:"compiled_at"`
}

// CompiledPackage 是一次成功编译的全部产物。
//
// 两份产物是**原子**的：调用方要么同时得到它们，要么什么都得不到。Bundle() 把它们与
// manifest 打包成唯一激活单元，加载前必须过 ValidateBundle。
type CompiledPackage struct {
	Manifest CompileManifest
	Profile  model.Profile
	// ProfileJSON 是浏览器契约产物的规范化字节，写盘/入库与摘要都用它。
	ProfileJSON []byte
	// Integration 是 server-only 运行时产物：typed provider bindings 与已解析 ID。
	Integration CompiledIntegration
	// IntegrationJSON 是它的规范化字节，摘要与落盘内容因此一致。
	IntegrationJSON []byte
}

// Bundle 返回可直接交给加载器的激活单元。
func (c CompiledPackage) Bundle() Bundle {
	return Bundle{Manifest: c.Manifest, Profile: c.ProfileJSON, Integration: c.IntegrationJSON}
}

var (
	// ErrValidatorRequired 等三个是装配缺依赖，属于编程错误而不是 package 配置错误。
	ErrValidatorRequired        = fmt.Errorf("integration: 必须注入 SIP validator")
	ErrProfileValidatorRequired = fmt.Errorf("integration: 必须注入 Profile document validator")
	ErrResolverRequired         = fmt.Errorf("integration: 必须注入 selector resolver")
	ErrToolSourceRequired       = fmt.Errorf("integration: 必须注入 ToolDescriptor source")
	ErrProfileResolverRequired  = fmt.Errorf("integration: 必须注入 Profile 引用解析器")
)

// Compile 执行完整编译。
//
// 返回的两类错误必须被区别对待：typed *Failure 是 package 配置错误（拒绝发布这一份
// package）；其它 error 是基础设施错误（resolver 不可用等），调用方不能把它当成"这个
// package 非法"。
func (c *Compiler) Compile(ctx context.Context, req CompileRequest) (CompiledPackage, error) {
	if c.Validator == nil {
		return CompiledPackage{}, ErrValidatorRequired
	}
	if c.Profiles == nil {
		return CompiledPackage{}, ErrProfileValidatorRequired
	}
	if c.Tools == nil {
		return CompiledPackage{}, ErrToolSourceRequired
	}
	if c.Resolver == nil {
		return CompiledPackage{}, ErrResolverRequired
	}
	if c.ProfileReferences == nil {
		return CompiledPackage{}, ErrProfileResolverRequired
	}

	// 回滚引用必须成对：只记住上一份 Profile 的摘要，回滚时就会把新的 CompiledIntegration
	// 与旧的 Profile 拼在一起——那正是配对校验要防的混版形状。
	if err := assertPreviousDigestsPaired(CompileManifest{
		PreviousProfileDigest:     req.PreviousProfileDigest,
		PreviousIntegrationDigest: req.PreviousIntegrationDigest,
	}); err != nil {
		return CompiledPackage{}, err
	}

	pkg, err := c.Validator.Decode(req.Raw)
	if err != nil {
		return CompiledPackage{}, err
	}
	// draft-only 硬门：`status: published` 只是发布意图，不是自授权。
	if pkg.Status == StatusPublished && !c.AllowPublished {
		return CompiledPackage{}, failf(ReasonPublishNotAuthorized,
			"package status=published，但发布 gate 未授权；Task 9 需先提供 discovery digest 与 owner attestation")
	}
	if err := validateStructure(pkg); err != nil {
		return CompiledPackage{}, err
	}
	if err := c.Profiles.ValidateToolKeys(pkg.ProfileTemplate.ToolRefs); err != nil {
		return CompiledPackage{}, failf(ReasonToolNotPublished, "%s", err.Error())
	}
	// provider binding 的 target/kind/多重性/capability/permission 交叉校验。
	bindings, err := validateProviderBindings(pkg, c.Tools)
	if err != nil {
		return CompiledPackage{}, err
	}

	resolved, err := c.resolveSelectors(ctx, pkg)
	if err != nil {
		return CompiledPackage{}, err
	}

	generated := buildProfile(pkg, resolved)
	profileJSON, err := marshalCanonical(generated)
	if err != nil {
		return CompiledPackage{}, fmt.Errorf("integration: 序列化 Profile 产物失败: %w", err)
	}
	// 关键一步：编译产物必须过**现有**的 Profile 校验链，而不是 compiler 自证合法。
	if err := c.Profiles.ValidateDocument(profileJSON); err != nil {
		return CompiledPackage{}, failf(ReasonCompiledProfileInvalid,
			"编译产物未通过现有 Profile 校验: %s", err.Error())
	}
	// 再走一遍 Registry 的引用解析：root 必须同环境（或 global）且从 service entity 有界可达。
	// 少了这一步，compiler 会编出一份 Registry 随后会 quarantine 的 Profile。
	configErr, fatal := profile.ValidateReferences(ctx, c.ProfileReferences, generated, c.MaxReachableDepth)
	if fatal != nil {
		return CompiledPackage{}, fmt.Errorf(
			"integration: 校验 Profile 引用失败（基础设施错误，不能当成 package 非法）: %w", fatal)
	}
	if configErr != nil {
		return CompiledPackage{}, failf(ReasonCompiledProfileInvalid,
			"编译产物的引用不可加载: %s", configErr.Error())
	}

	profileDigest := digest(profileJSON)
	integration := buildCompiledIntegration(
		pkg, resolved, bindings, generated.ProfileKey, generated.ProfileVersion, profileDigest)
	integrationJSON, err := marshalCanonical(integration)
	if err != nil {
		return CompiledPackage{}, fmt.Errorf("integration: 序列化 CompiledIntegration 失败: %w", err)
	}

	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	compiled := CompiledPackage{
		Manifest: CompileManifest{
			PackageKey:                pkg.PackageKey,
			PackageVersion:            pkg.PackageVersion,
			PackageStatus:             pkg.Status,
			CompilerVersion:           CompilerVersion,
			ProfileKey:                generated.ProfileKey,
			ProfileVersion:            generated.ProfileVersion,
			Environment:               pkg.Target.Environment,
			ServiceID:                 resolved.ServiceID,
			ServiceEntityID:           resolved.ServiceEntityID,
			RootEntityIDs:             resolved.RootEntityIDs,
			TemplateRefs:              templateRefsOf(pkg),
			ContentDigest:             digest(req.Raw),
			ProfileDigest:             profileDigest,
			IntegrationDigest:         digest(integrationJSON),
			PreviousProfileDigest:     req.PreviousProfileDigest,
			PreviousIntegrationDigest: req.PreviousIntegrationDigest,
			CompiledAt:                now().UTC(),
		},
		Profile:         generated,
		ProfileJSON:     profileJSON,
		Integration:     integration,
		IntegrationJSON: integrationJSON,
	}
	// 自检：编译器自己产出的激活单元必须能过加载器的配对校验。这条断言把"生成路径与加载
	// 路径对同一份产物看法不一致"变成编译期失败，而不是部署时才发现。
	if err := ValidateBundle(compiled.Bundle()); err != nil {
		return CompiledPackage{}, fmt.Errorf("integration: 编译产物未通过激活单元自检: %w", err)
	}
	return compiled, nil
}

// globalEnvironment 是跨环境共享实体的环境标记，与 Profile Registry 的口径逐字一致。
const globalEnvironment = "global"

// redactEnvironment 只保留"是否为空"这一点信息。
//
// 环境名本身不算高敏，但它属于部署拓扑；错误信息会进日志与编译报告，因此只输出一个
// 稳定占位而不是原文。
func redactEnvironment(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<empty>"
	}
	return "<other>"
}

// resolution 是全部 selector 解析结果的集合。
type resolution struct {
	ServiceID       string
	ServiceEntityID string
	RootEntityIDs   []string
	// entities/relations 用 selector 的规范化键索引，供 scope 与 journey stage 复用同一
	// 次解析结果，避免同一个 key 被解析多次。
	entities  map[string]string
	relations map[string]string
}

func (r resolution) entityID(key string) string   { return r.entities[key] }
func (r resolution) relationID(key string) string { return r.relations[key] }

func relationKey(selector RelationSelector) string {
	return selector.FromEntityKey + "\x1e" + selector.Kind + "\x1e" + selector.ToEntityKey
}

// resolveSelectors 把 package 里出现的每一个 selector 唯一解析成真实 ID。
func (c *Compiler) resolveSelectors(ctx context.Context, pkg Package) (resolution, error) {
	out := resolution{entities: map[string]string{}, relations: map[string]string{}}
	environment := pkg.Target.Environment

	service, err := c.Resolver.ResolveServiceSelector(ctx, environment, pkg.Target.ServiceSelector.EntityKey)
	if err != nil {
		return resolution{}, fmt.Errorf(
			"integration: 解析 service selector 失败（基础设施错误，不能当成 package 非法）: %w", err)
	}
	switch service.State {
	case ResolutionFound:
	case ResolutionNotFound:
		return resolution{}, failf(ReasonSelectorNotFound, "target.service_selector 在环境 %s 内没有命中", environment)
	case ResolutionAmbiguous:
		return resolution{}, failf(ReasonSelectorAmbiguous, "target.service_selector 在环境 %s 内命中多个", environment)
	default:
		return resolution{}, fmt.Errorf("integration: resolver 返回未知 service 状态 %q", service.State)
	}
	if service.ServiceID == "" || service.EntityID == "" {
		return resolution{}, fmt.Errorf("integration: resolver 返回 found 但缺少真实 ID")
	}
	// target 声明的环境必须与 registration 的真实环境一致：否则会把另一个环境的服务编进来。
	if service.Environment != environment {
		return resolution{}, failf(ReasonSelectorEnvironmentMismatch,
			"target.service_selector 解析到的服务不属于目标环境")
	}
	out.ServiceID = service.ServiceID
	out.ServiceEntityID = service.EntityID

	// 收集全部需要解析的 entity/relation selector，去重后一次性解析。
	entityKeys := map[string]struct{}{}
	relationSelectors := map[string]RelationSelector{}
	for _, selector := range pkg.Target.RootSelectors {
		entityKeys[selector.EntityKey] = struct{}{}
	}
	for _, journey := range pkg.ProfileTemplate.Journeys {
		for _, stage := range journey.Stages {
			for _, selector := range stage.EntitySelectors {
				entityKeys[selector.EntityKey] = struct{}{}
			}
			for _, selector := range stage.RelationSelectors {
				relationSelectors[relationKey(selector)] = selector
			}
		}
	}
	for _, binding := range pkg.ProfileTemplate.SignalBindings {
		for _, selector := range binding.Scope.EntitySelectors {
			entityKeys[selector.EntityKey] = struct{}{}
		}
		for _, selector := range binding.Scope.RelationSelectors {
			relationSelectors[relationKey(selector)] = selector
		}
	}

	for _, key := range sortedKeys(entityKeys) {
		entity, err := c.Resolver.ResolveEntitySelector(ctx, environment, key)
		if err != nil {
			return resolution{}, fmt.Errorf(
				"integration: 解析 entity selector 失败（基础设施错误，不能当成 package 非法）: %w", err)
		}
		switch entity.State {
		case ResolutionFound:
		case ResolutionNotFound:
			return resolution{}, failf(ReasonSelectorNotFound, "entity selector 在环境 %s 内没有命中", environment)
		case ResolutionAmbiguous:
			return resolution{}, failf(ReasonSelectorAmbiguous, "entity selector 在环境 %s 内命中多个", environment)
		default:
			return resolution{}, fmt.Errorf("integration: resolver 返回未知 entity 状态 %q", entity.State)
		}
		if entity.EntityID == "" {
			return resolution{}, fmt.Errorf("integration: resolver 返回 found 但缺少真实 entity ID")
		}
		// entity 的环境必须是目标环境，或显式 "global"（跨环境共享实体）。
		//
		// 这一条不能只靠 resolver 自觉过滤：resolver 是可替换的适配器，如果它按 entity_key
		// 全局查找，一个 staging 的实体就会被编进 production Profile。环境断言必须在
		// compiler 侧，与 Registry 的 root 环境校验同一口径。
		if entity.Environment != environment && entity.Environment != globalEnvironment {
			return resolution{}, failf(ReasonSelectorEnvironmentMismatch,
				"entity selector 解析到环境 %q，既不是目标环境也不是 global",
				redactEnvironment(entity.Environment))
		}
		out.entities[key] = entity.EntityID
	}

	for _, key := range sortedRelationKeys(relationSelectors) {
		selector := relationSelectors[key]
		relation, err := c.Resolver.ResolveRelationSelector(ctx, environment, selector)
		if err != nil {
			return resolution{}, fmt.Errorf(
				"integration: 解析 relation selector 失败（基础设施错误，不能当成 package 非法）: %w", err)
		}
		switch relation.State {
		case ResolutionFound:
		case ResolutionNotFound:
			return resolution{}, failf(ReasonSelectorNotFound, "relation selector 在环境 %s 内没有命中", environment)
		case ResolutionAmbiguous:
			return resolution{}, failf(ReasonSelectorAmbiguous, "relation selector 在环境 %s 内命中多个", environment)
		default:
			return resolution{}, fmt.Errorf("integration: resolver 返回未知 relation 状态 %q", relation.State)
		}
		if relation.RelationID == "" {
			return resolution{}, fmt.Errorf("integration: resolver 返回 found 但缺少真实 relation ID")
		}
		out.relations[key] = relation.RelationID
	}

	out.RootEntityIDs = make([]string, 0, len(pkg.Target.RootSelectors))
	for _, selector := range pkg.Target.RootSelectors {
		out.RootEntityIDs = append(out.RootEntityIDs, out.entityID(selector.EntityKey))
	}
	return out, nil
}

// buildProfile 生成 v1 Profile。
//
// service_ref、profile_version 与 status 全部由这里决定：
//   - service_ref 用解析出来的真实 ID；
//   - profile_version 跟随 package_version，保证两者可对照审计；
//   - draft package 只能编出 draft Profile，因此它不可能被 Registry 选成 active published。
func buildProfile(pkg Package, resolved resolution) model.Profile {
	template := pkg.ProfileTemplate
	generated := model.Profile{
		SchemaVersion:  pkg.Compatibility.ProfileSchemaVersion,
		ProfileKey:     template.ProfileKey,
		ProfileVersion: pkg.PackageVersion,
		Status:         profileStatus(pkg.Status),
		ServiceRef: model.ProfileServiceRef{
			ServiceID:     resolved.ServiceID,
			RootEntityIDs: resolved.RootEntityIDs,
		},
		ToolRefs: append([]string(nil), template.ToolRefs...),
		Limits: model.ProfileLimits{
			DefaultWindowSeconds: template.Limits.DefaultWindowSeconds,
			MaxWindowSeconds:     template.Limits.MaxWindowSeconds,
			MaxNodes:             template.Limits.MaxNodes,
			MaxRelations:         template.Limits.MaxRelations,
			MaxRows:              template.Limits.MaxRows,
			MaxEvidenceRefs:      template.Limits.MaxEvidenceRefs,
			FreshnessTTLSeconds:  template.Limits.FreshnessTTLSeconds,
		},
	}

	for _, focus := range template.FocusKinds {
		generated.FocusKinds = append(generated.FocusKinds, model.FocusKind{
			Key:          focus.Key,
			Label:        focus.Label,
			ValuePattern: focus.ValuePattern,
			Placeholder:  focus.Placeholder,
			Primary:      focus.Primary,
		})
	}
	for _, journey := range template.Journeys {
		converted := model.Journey{Key: journey.Key, Label: journey.Label, FocusKind: journey.FocusKind}
		for _, stage := range journey.Stages {
			convertedStage := model.JourneyStageDef{Key: stage.Key, Label: stage.Label}
			for _, selector := range stage.EntitySelectors {
				convertedStage.EntityRefs = append(convertedStage.EntityRefs, resolved.entityID(selector.EntityKey))
			}
			for _, selector := range stage.RelationSelectors {
				convertedStage.RelationRefs = append(convertedStage.RelationRefs, resolved.relationID(relationKey(selector)))
			}
			converted.Stages = append(converted.Stages, convertedStage)
		}
		generated.Journeys = append(generated.Journeys, converted)
	}
	for _, view := range template.Views {
		generated.Views = append(generated.Views, model.ProfileView{
			Key:        view.Key,
			Renderer:   model.Renderer(view.Renderer),
			Title:      view.Title,
			Order:      view.Order,
			JourneyRef: view.JourneyRef,
			SignalRefs: append([]string(nil), view.SignalRefs...),
			ToolRefs:   append([]string(nil), view.ToolRefs...),
		})
	}
	for _, binding := range template.SignalBindings {
		converted := model.SignalBinding{
			Key:             binding.Key,
			Label:           binding.Label,
			Family:          model.SignalFamily(binding.Family),
			Semantic:        model.SignalSemantic(binding.Semantic),
			Scope:           model.SignalBindingScope{Kind: model.ScopeKind(binding.Scope.Kind)},
			ToolKey:         binding.ToolKey,
			OutputField:     binding.OutputField,
			Unit:            binding.Unit,
			Aggregation:     model.Aggregation(binding.Aggregation),
			MeasurementRole: model.MeasurementRole(binding.MeasurementRole),
			ProxyReason:     binding.ProxyReason,
			BaselineRef:     binding.BaselineRef,
		}
		for _, selector := range binding.Scope.EntitySelectors {
			converted.Scope.Refs = append(converted.Scope.Refs, resolved.entityID(selector.EntityKey))
		}
		for _, selector := range binding.Scope.RelationSelectors {
			converted.Scope.Refs = append(converted.Scope.Refs, resolved.relationID(relationKey(selector)))
		}
		if binding.Thresholds != nil {
			converted.Thresholds = &model.SignalThresholds{
				Mode:      model.ThresholdMode(binding.Thresholds.Mode),
				Direction: model.ThresholdDirection(binding.Thresholds.Direction),
				Degraded:  binding.Thresholds.Degraded,
				Critical:  binding.Thresholds.Critical,
			}
		}
		generated.SignalBindings = append(generated.SignalBindings, converted)
	}
	generated.Normalize()
	return generated
}

// profileStatus 把 package 状态映射成 Profile 状态。
//
// 它是单向收紧的：draft -> draft。任何未知状态都落到 draft，绝不"猜"成 published——
// 一个被误判为 published 的 Profile 会被 Registry 直接激活。
func profileStatus(status Status) model.ProfileStatus {
	if status == StatusPublished {
		return model.ProfileStatusPublished
	}
	return model.ProfileStatusDraft
}

// validateStructure 是纯函数结构校验：不查库、不读文件、不发网络请求。
func validateStructure(pkg Package) error {
	template := pkg.ProfileTemplate

	focusKeys, err := uniqueKeys("profile_template.focus_kinds", keysOf(template.FocusKinds, func(f FocusKind) string { return f.Key }))
	if err != nil {
		return err
	}
	primary := 0
	for _, focus := range template.FocusKinds {
		if _, compileErr := regexp.Compile(focus.ValuePattern); compileErr != nil {
			return failf(ReasonInvalidFocusPattern,
				"focus_kinds[%s].value_pattern 无法被 Go RE2 编译", focus.Key)
		}
		if focus.Primary {
			primary++
		}
	}
	if primary != 1 {
		return failf(ReasonDanglingRef, "primary focus 数量=%d，必须恰好 1 个", primary)
	}

	journeyKeys, err := uniqueKeys("profile_template.journeys", keysOf(template.Journeys, func(j Journey) string { return j.Key }))
	if err != nil {
		return err
	}
	for _, journey := range template.Journeys {
		if _, ok := focusKeys[journey.FocusKind]; !ok {
			return failf(ReasonDanglingRef, "journeys[%s].focus_kind=%s 未声明", journey.Key, journey.FocusKind)
		}
		if _, err := uniqueKeys("journeys["+journey.Key+"].stages",
			keysOf(journey.Stages, func(s JourneyStage) string { return s.Key })); err != nil {
			return err
		}
	}

	signalKeys, err := uniqueKeys("profile_template.signal_bindings",
		keysOf(template.SignalBindings, func(b SignalBinding) string { return b.Key }))
	if err != nil {
		return err
	}
	toolKeys, err := uniqueKeys("profile_template.tool_refs", template.ToolRefs)
	if err != nil {
		return err
	}
	for _, binding := range template.SignalBindings {
		if _, ok := toolKeys[binding.ToolKey]; !ok {
			return failf(ReasonDanglingRef,
				"signal_bindings[%s].tool_key=%s 未在 tool_refs 中声明", binding.Key, binding.ToolKey)
		}
	}

	viewKeys := map[string]struct{}{}
	viewOrders := map[int]struct{}{}
	for _, view := range template.Views {
		if _, exists := viewKeys[view.Key]; exists {
			return failf(ReasonDuplicateKey, "views[%s] 重复", view.Key)
		}
		viewKeys[view.Key] = struct{}{}
		if _, exists := viewOrders[view.Order]; exists {
			return failf(ReasonDuplicateKey, "views[%s].order=%d 与其它 view 冲突", view.Key, view.Order)
		}
		viewOrders[view.Order] = struct{}{}
		if view.JourneyRef != "" {
			if _, ok := journeyKeys[view.JourneyRef]; !ok {
				return failf(ReasonDanglingRef, "views[%s].journey_ref=%s 未声明", view.Key, view.JourneyRef)
			}
		}
		for _, ref := range view.SignalRefs {
			if _, ok := signalKeys[ref]; !ok {
				return failf(ReasonDanglingRef, "views[%s].signal_refs 引用未声明的 %s", view.Key, ref)
			}
		}
		for _, ref := range view.ToolRefs {
			if _, ok := toolKeys[ref]; !ok {
				return failf(ReasonDanglingRef, "views[%s].tool_refs 引用未在 tool_refs 中声明的 %s", view.Key, ref)
			}
		}
	}
	return nil
}

func templateRefsOf(pkg Package) []string {
	seen := map[string]struct{}{}
	for _, binding := range pkg.Bindings {
		seen[binding.TemplateRef] = struct{}{}
	}
	return sortedKeys(seen)
}

// marshalCanonical 输出稳定缩进的 JSON。摘要与落盘内容用同一份字节，避免"摘要对得上但
// 文件内容不同"。
func marshalCanonical(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func keysOf[T any](items []T, key func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, key(item))
	}
	return out
}

func uniqueKeys(field string, keys []string) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			return nil, failf(ReasonDuplicateKey, "%s 中 %s 重复", field, key)
		}
		seen[key] = struct{}{}
	}
	return seen, nil
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedRelationKeys(set map[string]RelationSelector) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// describeManifest 是审计友好的摘要输出，只含 key/version/digest，不含 ID 与路径内容。
func (m CompileManifest) String() string {
	return fmt.Sprintf("CompileManifest{Package:%s v%d %s, Profile:%s v%d, Templates:%s, Digest:%s}",
		m.PackageKey, m.PackageVersion, m.PackageStatus, m.ProfileKey, m.ProfileVersion,
		strings.Join(m.TemplateRefs, "+"), m.ProfileDigest)
}
