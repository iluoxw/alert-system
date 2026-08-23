package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// 本文件是 server-only 运行时产物 CompiledIntegration。
//
// 它存在的理由很具体：SIP 里的 template_ref 与 typed params 是**服务端**执行查询必需的，
// 但它们既不能塞进 v1 Profile（Profile 是浏览器契约，逐层 additionalProperties=false 也装不下），
// 又不能让 adapter 在运行时回头重读 SIP 原文（那等于绕过编译与发布语义）。所以一次编译必须
// 原子产出两份配对产物：Profile 给页面，CompiledIntegration 给受信 adapter。
//
// 三条硬约束：
//
//  1. 它禁止 raw query、endpoint、header、secret——生成后再过一次递归禁止键扫描；
//  2. 它不属于浏览器契约：view/context/api/web 都不得导入或序列化它；
//  3. 它与 Profile 通过 manifest 的两个摘要配成**唯一激活单元**。缺任一半、摘要错配或单独
//     激活都必须 quarantine 整对，绝不能静默混用一新一旧。

// CompiledIntegrationSchemaVersion 是 server-only 产物自身的版本线。
//
// 它与 SIP 的 schema_version 分开：运行时产物的形状可以在不改 package 契约的情况下演进。
const CompiledIntegrationSchemaVersion = "1.0"

// CompiledIntegration 是受信 adapter 唯一的运行时 binding 来源。
type CompiledIntegration struct {
	SchemaVersion   string `json:"schema_version"`
	PackageKey      string `json:"package_key"`
	PackageVersion  int    `json:"package_version"`
	PackageStatus   Status `json:"package_status"`
	CompilerVersion int    `json:"compiler_version"`
	ProfileKey      string `json:"profile_key"`
	ProfileVersion  int    `json:"profile_version"`
	Environment     string `json:"environment"`
	// ProfileDigest 是配对锚点：加载时必须与 manifest 及实际 Profile 字节一致。
	ProfileDigest string `json:"profile_digest"`
	// ServiceID / ServiceEntityID / RootEntityIDs 是已解析的真实 ID，不含 entity-key fallback。
	ServiceID       string   `json:"service_id"`
	ServiceEntityID string   `json:"service_entity_id"`
	RootEntityIDs   []string `json:"root_entity_ids"`
	// Bindings 按 key 稳定排序。
	Bindings []CompiledBinding `json:"bindings"`
	// TopologyPolicy 是 discovery/overlay 的治理快照。
	TopologyPolicy CompiledTopologyPolicy `json:"topology_policy"`
}

// CompiledBinding 是一条可执行的 binding：已解析目标 + typed params + registry 推导事实。
type CompiledBinding struct {
	Key       string      `json:"key"`
	Kind      BindingKind `json:"kind"`
	TargetRef string      `json:"target_ref"`
	// ResolvedTargetIDs 是 target 在 canonical graph 里的真实 ID。
	//
	// signal 的 scope refs、journey 的 stage refs 都在这里落成真实 ID，adapter 因此不需要
	// 回头查 Profile 或 SIP。focus/evidence 的 target 是 focus kind，没有 canonical ID，
	// 此时为空数组而不是 null。
	ResolvedTargetIDs  []string       `json:"resolved_target_ids"`
	ToolKey            string         `json:"tool_key"`
	TemplateRef        string         `json:"template_ref"`
	SourceKey          string         `json:"source_key"`
	Capability         Capability     `json:"capability"`
	RequiredPermission string         `json:"required_permission"`
	EvidenceClass      EvidenceClass  `json:"evidence_class,omitempty"`
	SideOutputs        []SideOutput   `json:"side_outputs"`
	Params             TemplateParams `json:"params"`
}

// CompiledTopologyPolicy 是 topology policy 的编译快照。
type CompiledTopologyPolicy struct {
	AliasNamespaces   []string `json:"alias_namespaces"`
	IdentityFields    []string `json:"identity_fields"`
	InstanceRole      string   `json:"instance_role,omitempty"`
	StaleAfterSeconds int      `json:"stale_after_seconds"`
	MaxHints          int      `json:"max_hints,omitempty"`
}

// Bundle 是"一次激活"的完整单元。加载器只接受 Bundle，不接受单独的一半。
type Bundle struct {
	Manifest    CompileManifest
	Profile     []byte
	Integration []byte
}

var (
	// ErrBundleProfileMissing / ErrBundleIntegrationMissing 表示只拿到了一半产物。
	ErrBundleProfileMissing     = errors.New("integration: 激活单元缺少 Profile 产物")
	ErrBundleIntegrationMissing = errors.New("integration: 激活单元缺少 CompiledIntegration 产物")
	// ErrBundleProfileDigestMismatch / ErrBundleIntegrationDigestMismatch 表示字节被篡改或
	// 与 manifest 记录的摘要不符。
	ErrBundleProfileDigestMismatch     = errors.New("integration: Profile 摘要与 manifest 不一致")
	ErrBundleIntegrationDigestMismatch = errors.New("integration: CompiledIntegration 摘要与 manifest 不一致")
	// ErrBundlePairBroken 表示两份产物互相不认：CompiledIntegration 里记录的 ProfileDigest
	// 指向另一个版本的 Profile。这正是"混用一新一旧"的形状。
	ErrBundlePairBroken = errors.New("integration: 两份产物未配成同一激活单元")
	// ErrBundleForbiddenContent 表示 CompiledIntegration 里出现了禁止内容。
	ErrBundleForbiddenContent = errors.New("integration: CompiledIntegration 含禁止内容")
)

// ValidateBundle 是加载/激活前的唯一入口。
//
// 它必须在任何一半被使用之前调用：Profile Registry 与 adapter 各自只看自己那一半，
// 谁都发现不了"两半来自不同版本"，所以配对校验只能在这里做一次。
func ValidateBundle(bundle Bundle) error {
	if len(bundle.Profile) == 0 {
		return ErrBundleProfileMissing
	}
	if len(bundle.Integration) == 0 {
		return ErrBundleIntegrationMissing
	}
	if digest(bundle.Profile) != bundle.Manifest.ProfileDigest {
		return ErrBundleProfileDigestMismatch
	}
	if digest(bundle.Integration) != bundle.Manifest.IntegrationDigest {
		return ErrBundleIntegrationDigestMismatch
	}

	var compiled CompiledIntegration
	if err := json.Unmarshal(bundle.Integration, &compiled); err != nil {
		return fmt.Errorf("%w: 无法解析", ErrBundleIntegrationMissing)
	}
	if compiled.SchemaVersion != CompiledIntegrationSchemaVersion {
		return fmt.Errorf("%w: schema_version=%q", ErrBundlePairBroken, compiled.SchemaVersion)
	}
	// 交叉锚点：CompiledIntegration 自己记的 ProfileDigest 必须指向同一份 Profile。
	if compiled.ProfileDigest != bundle.Manifest.ProfileDigest {
		return ErrBundlePairBroken
	}
	if compiled.PackageKey != bundle.Manifest.PackageKey ||
		compiled.PackageVersion != bundle.Manifest.PackageVersion ||
		compiled.ProfileVersion != bundle.Manifest.ProfileVersion ||
		compiled.Environment != bundle.Manifest.Environment {
		return ErrBundlePairBroken
	}

	// 纵深防御：即使编译路径将来被改动，禁止内容也不能进入运行时产物。
	var generic any
	if err := json.Unmarshal(bundle.Integration, &generic); err != nil {
		return fmt.Errorf("%w: 无法解析", ErrBundleIntegrationMissing)
	}
	if path, found := scanForbiddenKeys(generic, ""); found {
		return fmt.Errorf("%w: 禁止的键 %s", ErrBundleForbiddenContent, path)
	}
	return nil
}

// buildCompiledIntegration 生成 server-only 产物。
func buildCompiledIntegration(
	pkg Package, resolved resolution, bindings []resolvedBinding,
	profileKey string, profileVersion int, profileDigest string,
) CompiledIntegration {
	compiled := CompiledIntegration{
		SchemaVersion:   CompiledIntegrationSchemaVersion,
		PackageKey:      pkg.PackageKey,
		PackageVersion:  pkg.PackageVersion,
		PackageStatus:   pkg.Status,
		CompilerVersion: CompilerVersion,
		ProfileKey:      profileKey,
		ProfileVersion:  profileVersion,
		Environment:     pkg.Target.Environment,
		ProfileDigest:   profileDigest,
		ServiceID:       resolved.ServiceID,
		ServiceEntityID: resolved.ServiceEntityID,
		RootEntityIDs:   append([]string(nil), resolved.RootEntityIDs...),
		Bindings:        make([]CompiledBinding, 0, len(bindings)),
		TopologyPolicy:  compileTopologyPolicy(pkg.TopologyPolicy),
	}
	signalScopes := map[string][]string{}
	for _, signal := range pkg.ProfileTemplate.SignalBindings {
		signalScopes[signal.Key] = resolvedScopeIDs(signal.Scope, resolved)
	}
	journeyRefs := map[string][]string{}
	for _, journey := range pkg.ProfileTemplate.Journeys {
		refs := make([]string, 0)
		for _, stage := range journey.Stages {
			for _, selector := range stage.EntitySelectors {
				refs = append(refs, resolved.entityID(selector.EntityKey))
			}
			for _, selector := range stage.RelationSelectors {
				refs = append(refs, resolved.relationID(relationKey(selector)))
			}
		}
		journeyRefs[journey.Key] = refs
	}

	for _, binding := range bindings {
		targets := []string{}
		switch binding.Kind {
		case KindSignal:
			targets = signalScopes[binding.TargetRef]
		case KindJourney:
			targets = journeyRefs[binding.TargetRef]
		}
		if targets == nil {
			targets = []string{}
		}
		sideOutputs := binding.SideOutputs
		if sideOutputs == nil {
			sideOutputs = []SideOutput{}
		}
		compiled.Bindings = append(compiled.Bindings, CompiledBinding{
			Key:                binding.Key,
			Kind:               binding.Kind,
			TargetRef:          binding.TargetRef,
			ResolvedTargetIDs:  targets,
			ToolKey:            binding.ToolKey,
			TemplateRef:        binding.TemplateRef,
			SourceKey:          binding.SourceKey,
			Capability:         binding.Capability,
			RequiredPermission: binding.RequiredPermission,
			EvidenceClass:      binding.EvidenceClass,
			SideOutputs:        sideOutputs,
			Params:             binding.Params,
		})
	}
	sort.SliceStable(compiled.Bindings, func(i, j int) bool {
		return compiled.Bindings[i].Key < compiled.Bindings[j].Key
	})
	return compiled
}

func resolvedScopeIDs(scope SignalScope, resolved resolution) []string {
	out := make([]string, 0, len(scope.EntitySelectors)+len(scope.RelationSelectors))
	for _, selector := range scope.EntitySelectors {
		out = append(out, resolved.entityID(selector.EntityKey))
	}
	for _, selector := range scope.RelationSelectors {
		out = append(out, resolved.relationID(relationKey(selector)))
	}
	return out
}

func compileTopologyPolicy(policy TopologyPolicy) CompiledTopologyPolicy {
	compiled := CompiledTopologyPolicy{
		AliasNamespaces:   append([]string(nil), policy.AliasNamespaces...),
		IdentityFields:    []string{},
		StaleAfterSeconds: policy.Overlay.StaleAfterSeconds,
	}
	if policy.Discovery != nil {
		compiled.IdentityFields = append([]string(nil), policy.Discovery.IdentityFields...)
		compiled.InstanceRole = policy.Discovery.InstanceRole
	}
	if compiled.IdentityFields == nil {
		compiled.IdentityFields = []string{}
	}
	if policy.Overlay.MaxHints != nil {
		compiled.MaxHints = *policy.Overlay.MaxHints
	}
	return compiled
}

// String 只输出 key/version/digest 摘要，不输出 params 与 ID。
func (c CompiledIntegration) String() string {
	return fmt.Sprintf("CompiledIntegration{Package:%s v%d %s, Profile:%s v%d, Bindings:%d, ProfileDigest:%s}",
		c.PackageKey, c.PackageVersion, c.PackageStatus,
		c.ProfileKey, c.ProfileVersion, len(c.Bindings), c.ProfileDigest)
}
