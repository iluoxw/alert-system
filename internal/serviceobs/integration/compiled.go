package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"alert-system/internal/serviceobs/model"
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
	// ErrBundleInvalidArtifact 表示某一份产物无法按契约严格解析：未知字段、尾随 JSON、
	// 或根本不是合法文档。它与"摘要不匹配"是两件事——摘要对得上但结构非法说明生成侧
	// 与加载侧对同一份字节的理解不同。
	ErrBundleInvalidArtifact = errors.New("integration: 激活单元内的产物无法严格解析")
	// ErrBundleIdentityMismatch 表示 manifest、CompiledIntegration 与实际 Profile 三者
	// 之间的激活身份字段不一致。
	//
	// 这些字段在三处重复出现是刻意的冗余：单看任何一处都无法发现"有人只改了 manifest"。
	// 最典型的攻击面是把 manifest 的 package_status 从 draft 改成 published——它绕过了
	// compiler 的 AllowPublished 门，但实际 Profile 产物仍然是 draft，交叉核对因此能抓住。
	ErrBundleIdentityMismatch = errors.New("integration: 激活身份字段不一致")
	// ErrBundlePreviousDigestUnpaired 表示回滚引用只给了一半。
	//
	// 回滚的单位是**一对**产物：只记住上一份 Profile 的摘要，回滚时就会把新的
	// CompiledIntegration 与旧的 Profile 拼在一起。
	ErrBundlePreviousDigestUnpaired = errors.New("integration: 回滚引用必须成对出现或同时为空")
)

// ValidateBundle 是加载/激活前的唯一入口。
//
// 它必须在任何一半被使用之前调用：Profile Registry 与 adapter 各自只看自己那一半，
// 谁都发现不了"两半来自不同版本"，所以配对校验只能在这里做一次。
//
// 校验分四层，顺序是刻意的：
//
//  1. 两份产物都在场，且字节摘要与 manifest 记录一致（先挡篡改，后面的解析才有意义）；
//  2. 运行时产物里没有禁止内容（对原始字节的内容策略，不依赖 Go 结构形状）；
//  3. 两份产物都能按契约**严格**解析——未知字段与尾随 JSON 都拒绝；
//  4. manifest、CompiledIntegration 与**实际 Profile** 三者的激活身份字段逐项一致；
//  5. 回滚引用成对出现或同时为空。
func ValidateBundle(bundle Bundle) error {
	if len(bundle.Profile) == 0 {
		return ErrBundleProfileMissing
	}
	if len(bundle.Integration) == 0 {
		return ErrBundleIntegrationMissing
	}
	profileDigest := digest(bundle.Profile)
	if profileDigest != bundle.Manifest.ProfileDigest {
		return ErrBundleProfileDigestMismatch
	}
	if digest(bundle.Integration) != bundle.Manifest.IntegrationDigest {
		return ErrBundleIntegrationDigestMismatch
	}

	// 禁止键扫描放在严格解码**之前**：它是对原始字节的内容策略，与 Go 结构当前长什么样
	// 无关。放在后面的话，将来某天有人给 CompiledIntegration 加了一个装 endpoint 的字段，
	// 严格解码会先放行，这道闸门的语义就被稀释成"只挡未知字段"。
	var generic any
	if err := json.Unmarshal(bundle.Integration, &generic); err != nil {
		return fmt.Errorf("%w: CompiledIntegration 无法解析为通用结构", ErrBundleInvalidArtifact)
	}
	if path, found := scanForbiddenKeys(generic, ""); found {
		return fmt.Errorf("%w: 禁止的键 %s", ErrBundleForbiddenContent, path)
	}

	var compiled CompiledIntegration
	if err := strictDecodeJSON(bundle.Integration, &compiled); err != nil {
		return fmt.Errorf("%w: CompiledIntegration %s", ErrBundleInvalidArtifact, err)
	}
	// 实际 Profile 产物必须被真的解析出来，而不是只算一个摘要就放过去。摘要只能证明
	// "字节没被改"，证明不了"这份 Profile 描述的是同一个服务"。
	var profileDoc model.Profile
	if err := strictDecodeJSON(bundle.Profile, &profileDoc); err != nil {
		return fmt.Errorf("%w: Profile %s", ErrBundleInvalidArtifact, err)
	}

	if compiled.SchemaVersion != CompiledIntegrationSchemaVersion {
		return fmt.Errorf("%w: CompiledIntegration schema_version", ErrBundleIdentityMismatch)
	}
	if err := assertActivationIdentity(bundle.Manifest, compiled, profileDoc, profileDigest); err != nil {
		return err
	}
	return assertPreviousDigestsPaired(bundle.Manifest)
}

// assertActivationIdentity 逐项核对三处重复的激活身份字段。
//
// 错误只报**字段名**，不回显值：这些字段里有 service/entity ID 与环境名，属于部署拓扑，
// 而校验失败会进启动日志与 quarantine 记录。
func assertActivationIdentity(
	manifest CompileManifest, compiled CompiledIntegration,
	profileDoc model.Profile, profileDigest string,
) error {
	mismatch := func(field string) error {
		return fmt.Errorf("%w: %s", ErrBundleIdentityMismatch, field)
	}

	// manifest <-> CompiledIntegration。
	if manifest.CompilerVersion != compiled.CompilerVersion {
		return mismatch("compiler_version")
	}
	if manifest.PackageKey != compiled.PackageKey {
		return mismatch("package_key")
	}
	if manifest.PackageVersion != compiled.PackageVersion {
		return mismatch("package_version")
	}
	if manifest.PackageStatus != compiled.PackageStatus {
		return mismatch("package_status")
	}
	if manifest.Environment != compiled.Environment {
		return mismatch("environment")
	}
	if manifest.ServiceID != compiled.ServiceID {
		return mismatch("service_id")
	}
	if manifest.ServiceEntityID != compiled.ServiceEntityID {
		return mismatch("service_entity_id")
	}
	if !equalStrings(manifest.RootEntityIDs, compiled.RootEntityIDs) {
		return mismatch("root_entity_ids")
	}
	if manifest.ProfileKey != compiled.ProfileKey {
		return mismatch("profile_key")
	}
	if manifest.ProfileVersion != compiled.ProfileVersion {
		return mismatch("profile_version")
	}
	// 摘要要与 manifest 记录**和**实际字节三方一致。
	if manifest.ProfileDigest != compiled.ProfileDigest || compiled.ProfileDigest != profileDigest {
		return mismatch("profile_digest")
	}

	// 实际 Profile 产物是最强的锚点：它就是 Registry 会加载的那份文档。
	if profileDoc.ProfileKey != manifest.ProfileKey {
		return mismatch("profile.profile_key")
	}
	if profileDoc.ProfileVersion != manifest.ProfileVersion {
		return mismatch("profile.profile_version")
	}
	if profileDoc.ServiceRef.ServiceID != manifest.ServiceID {
		return mismatch("profile.service_ref.service_id")
	}
	if !equalStrings(profileDoc.ServiceRef.RootEntityIDs, manifest.RootEntityIDs) {
		return mismatch("profile.service_ref.root_entity_ids")
	}
	// package status 与实际 Profile status 必须满足编译时的同一映射。
	//
	// 这一条专门堵"只改 manifest"的提权：把 manifest 的 package_status 改成 published 能
	// 绕过 compiler 的 AllowPublished 门，但它改不了已经签好摘要的 Profile 产物——那份
	// 文档里的 status 仍然是 draft，于是在这里失败。
	if profileStatus(manifest.PackageStatus) != profileDoc.Status {
		return mismatch("profile.status")
	}
	return nil
}

// assertPreviousDigestsPaired 要求回滚引用同时为空或同时存在。
func assertPreviousDigestsPaired(manifest CompileManifest) error {
	hasProfile := manifest.PreviousProfileDigest != ""
	hasIntegration := manifest.PreviousIntegrationDigest != ""
	if hasProfile != hasIntegration {
		return ErrBundlePreviousDigestUnpaired
	}
	return nil
}

// strictDecodeJSON 按契约严格解码：拒绝未知字段，也拒绝合法文档之后的尾随 JSON。
//
// 尾随内容必须拒绝的理由与游标解码一致：`{...}{...}` 里第一个对象能解析成功，于是不同
// 实现可能各自读到不同的一半；摘要还照样对得上，因为字节没被改，只是被"多写了一份"。
func strictDecodeJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解码失败")
	}
	// 第二次 Decode 必须恰好是 io.EOF：任何剩余 token 都说明文档不止一个 JSON 值。
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("存在尾随内容")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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
