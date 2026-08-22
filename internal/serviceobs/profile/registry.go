// Package profile 是 file-based Service Profile Registry。
//
// 发布语义（Phase 2）：
//
//   - Source 为 nil：启动成功，registry 为空。总览显示 Unconfigured，工作区 409。
//   - Source 存在但目录不可读：Registry 构造失败，让部署者立刻发现，而不是误以为
//     Profile 已加载。
//   - 单个文档非法：服务继续启动，该文档被 quarantine，不进 active registry。能安全
//     识别 service ID 时，该 service 通过 DiagnosticForService 拿到稳定配置错误 code。
//   - 同一 service 有多个 published Profile：全部冲突项 quarantine，绝不按文件名或
//     加载顺序猜一个。
//   - resolver 返回 typed not_found/ambiguous：quarantine 单个文档。resolver 返回
//     基础设施 error（DB 不可用、查询失败、context 取消）：中止整个 Registry 构造。
//
// Registry 只在构造时加载一次。本 Phase 不实现热加载、定时重扫或管理 API：新 Profile
// 与 quarantine 修复都需要进程重启。构造完成后 Registry 是只读的，可并发访问。
package profile

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
)

// DefaultMaxReachableDepth 是 root entity 可达性遍历的默认上界，与 canonical relation
// 遍历的既有上界一致。
const DefaultMaxReachableDepth = 8

// Options 是 Registry 的构造依赖。全部显式注入：profile 包不读环境变量、不连数据库。
type Options struct {
	// Source 为 nil 表示"未配置 Profile 目录"，构造出一个合法的空 registry。
	Source Source
	// Validator 提供离线 v1 schema 校验，必填。
	Validator *contract.Validator
	// Tools 是已发布 Tool catalog，必填：Profile 只能引用其中的 key。
	Tools toolCatalog
	// Resolver 解析真实 service/entity ID，必填。
	Resolver Resolver
	// MaxReachableDepth 为 0 时取 DefaultMaxReachableDepth。
	MaxReachableDepth int
}

// Quarantined 记录一个被隔离的文档。它只带定位信息与稳定 reason code，不带文档正文，
// 因此可以安全地进启动日志。
type Quarantined struct {
	Document   string
	ServiceID  string
	ProfileKey string
	ReasonCode string
	Detail     string
}

// Diagnostic 是某个 service 的配置错误摘要。总览据此显示 profile=null、
// Data Unconfigured 与稳定 reason code，而不是伪装成"这个服务没配 Profile"。
type Diagnostic struct {
	ServiceID   string
	ReasonCodes []string
	Documents   []string
}

// Stats 供启动日志使用：只有数量与稳定 reason code，没有文档正文和路径内容。
type Stats struct {
	Published    int
	NonPublished int
	Quarantined  int
}

// Registry 是构造后不可变的 Profile 注册表。
type Registry struct {
	published    map[string]model.Profile
	diagnostics  map[string]Diagnostic
	quarantined  []Quarantined
	nonPublished int
}

var (
	// ErrValidatorRequired 等三个错误表示装配缺依赖，属于编程错误而不是配置错误。
	ErrValidatorRequired = errors.New("profile: 必须注入 contract validator")
	ErrToolsRequired     = errors.New("profile: 必须注入 tool catalog")
	ErrResolverRequired  = errors.New("profile: 必须注入 resolver")
)

// candidate 是通过结构校验、等待引用解析与 published 去重的中间态。
type candidate struct {
	document string
	profile  model.Profile
}

// New 加载并校验目录里的全部 Profile。返回 error 只有三种情况：装配缺依赖、
// Source 不可读、resolver 报基础设施错误。单个文档的任何问题都只导致 quarantine。
func New(ctx context.Context, opts Options) (*Registry, error) {
	if opts.Validator == nil {
		return nil, ErrValidatorRequired
	}
	if opts.Tools == nil {
		return nil, ErrToolsRequired
	}
	if opts.Resolver == nil {
		return nil, ErrResolverRequired
	}
	maxDepth := opts.MaxReachableDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxReachableDepth
	}

	registry := &Registry{
		published:   map[string]model.Profile{},
		diagnostics: map[string]Diagnostic{},
	}
	if opts.Source == nil {
		return registry, nil
	}

	names, err := opts.Source.List()
	if err != nil {
		return nil, err
	}

	candidates := make([]candidate, 0, len(names))
	for _, name := range names {
		raw, readErr := opts.Source.Read(name)
		if readErr != nil {
			registry.quarantine(Quarantined{
				Document: name, ReasonCode: readReasonCode(readErr), Detail: "文档不可读或超过大小上限",
			})
			continue
		}

		profile, decodeErr := decodeProfile(opts.Validator, raw)
		if decodeErr != nil {
			// 结构非法时不引用文档内的 service ID：那个字段本身可能就是非法的。
			registry.quarantine(quarantinedFrom(name, model.Profile{}, decodeErr))
			continue
		}
		if structureErr := validateStructure(profile); structureErr != nil {
			registry.quarantine(quarantinedFrom(name, profile, structureErr))
			continue
		}

		// 第 7 步：外部引用解析。resolver 的基础设施错误在这里直接中止构造。
		refErr, fatal := resolveReferences(ctx, opts.Resolver, profile, maxDepth)
		if fatal != nil {
			return nil, fatal
		}
		if refErr != nil {
			registry.quarantine(quarantinedFrom(name, profile, refErr))
			continue
		}

		// 第 8 步：tool refs 必须全部已发布。
		if toolErr := validateToolRefs(profile, opts.Tools); toolErr != nil {
			registry.quarantine(quarantinedFrom(name, profile, toolErr))
			continue
		}

		candidates = append(candidates, candidate{document: name, profile: profile})
	}

	registry.indexCandidates(candidates)
	return registry, nil
}

// indexCandidates 执行第 9 步：每 service 至多一个 published Profile。冲突时把该 service
// 的所有 published 候选全部 quarantine，不按文件名或加载顺序挑一个。
func (r *Registry) indexCandidates(candidates []candidate) {
	byService := map[string][]candidate{}
	for _, item := range candidates {
		if item.profile.Status != model.ProfileStatusPublished {
			// draft/deprecated 是合法但未发布：不进 active registry，也不算配置错误。
			r.nonPublished++
			continue
		}
		serviceID := item.profile.ServiceRef.ServiceID
		byService[serviceID] = append(byService[serviceID], item)
	}

	serviceIDs := make([]string, 0, len(byService))
	for serviceID := range byService {
		serviceIDs = append(serviceIDs, serviceID)
	}
	sort.Strings(serviceIDs)

	for _, serviceID := range serviceIDs {
		group := byService[serviceID]
		if len(group) == 1 {
			r.published[serviceID] = group[0].profile
			continue
		}
		documents := make([]string, 0, len(group))
		for _, item := range group {
			documents = append(documents, item.document)
		}
		sort.Strings(documents)
		for _, item := range group {
			r.quarantine(Quarantined{
				Document:   item.document,
				ServiceID:  serviceID,
				ProfileKey: item.profile.ProfileKey,
				ReasonCode: ReasonDuplicatePublished,
				Detail:     fmt.Sprintf("service 有 %d 个 published Profile，全部隔离", len(documents)),
			})
		}
	}
}

// resolveReferences 校验 service ID 与 root entity IDs。
//
// 返回值语义：第一个返回值非 nil 表示配置错误（quarantine 单个文档），第二个非 nil
// 表示基础设施错误（中止整个 Registry 构造）。两者互斥。
func resolveReferences(ctx context.Context, resolver Resolver, profile model.Profile, maxDepth int) (error, error) {
	service, err := resolver.ResolveService(ctx, profile.ServiceRef.ServiceID)
	if err != nil {
		return nil, fmt.Errorf("profile: 解析 service %s 失败（基础设施错误，中止 Registry 构造）: %w",
			profile.ServiceRef.ServiceID, err)
	}
	switch service.State {
	case ResolutionFound:
	case ResolutionNotFound:
		return failf(ReasonRefNotFound, "service_ref.service_id 不存在"), nil
	case ResolutionAmbiguous:
		return failf(ReasonRefAmbiguous, "service_ref.service_id 不唯一"), nil
	default:
		return nil, fmt.Errorf("profile: resolver 返回未知 service 状态 %q", service.State)
	}

	reachable, err := resolver.DeclaredReachableEntityIDs(ctx, service.EntityID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("profile: 遍历 service %s 的 declared 可达实体失败（基础设施错误，中止 Registry 构造）: %w",
			profile.ServiceRef.ServiceID, err)
	}

	for _, rootID := range profile.ServiceRef.RootEntityIDs {
		entity, err := resolver.ResolveEntity(ctx, rootID)
		if err != nil {
			return nil, fmt.Errorf("profile: 解析 root entity 失败（基础设施错误，中止 Registry 构造）: %w", err)
		}
		switch entity.State {
		case ResolutionFound:
		case ResolutionNotFound:
			return failf(ReasonRefNotFound, "root_entity_ids 中存在不存在的实体"), nil
		case ResolutionAmbiguous:
			return failf(ReasonRefAmbiguous, "root_entity_ids 中存在不唯一的实体"), nil
		default:
			return nil, fmt.Errorf("profile: resolver 返回未知 entity 状态 %q", entity.State)
		}
		// global 实体跨环境共享，允许被任意环境的 service 引用；其余必须同环境，
		// 否则 root 会把另一个环境的拓扑拉进当前服务视图。
		if entity.Environment != "global" && entity.Environment != service.Environment {
			return failf(ReasonRootEnvironmentMismatch,
				"root entity 环境与 service 不一致"), nil
		}
		// root 必须是 service entity 本身，或从它经 declared depends_on/part_of 有界可达。
		if rootID == service.EntityID {
			continue
		}
		if _, ok := reachable[rootID]; !ok {
			return failf(ReasonRootUnreachable,
				"root entity 无法从 service entity 经 declared 关系在 %d 跳内到达", maxDepth), nil
		}
	}
	return nil, nil
}

// readReasonCode 把 Source 的 typed 错误映射成稳定 reason code。
func readReasonCode(err error) string {
	switch {
	case errors.Is(err, ErrDocumentTooLarge):
		return ReasonTooLarge
	case errors.Is(err, ErrNotRegularJSONFile):
		return ReasonNotRegularJSONFile
	default:
		return ReasonReadFailed
	}
}

func quarantinedFrom(document string, profile model.Profile, err error) Quarantined {
	record := Quarantined{Document: document, ReasonCode: ReasonSchemaInvalid, Detail: "校验失败"}
	var failure *validationFailure
	if errors.As(err, &failure) {
		record.ReasonCode = failure.ReasonCode
		record.Detail = failure.Detail
	}
	record.ServiceID = profile.ServiceRef.ServiceID
	record.ProfileKey = profile.ProfileKey
	return record
}

func (r *Registry) quarantine(record Quarantined) {
	r.quarantined = append(r.quarantined, record)
	if record.ServiceID == "" {
		return
	}
	diagnostic := r.diagnostics[record.ServiceID]
	diagnostic.ServiceID = record.ServiceID
	if !containsString(diagnostic.ReasonCodes, record.ReasonCode) {
		diagnostic.ReasonCodes = append(diagnostic.ReasonCodes, record.ReasonCode)
		sort.Strings(diagnostic.ReasonCodes)
	}
	if !containsString(diagnostic.Documents, record.Document) {
		diagnostic.Documents = append(diagnostic.Documents, record.Document)
		sort.Strings(diagnostic.Documents)
	}
	r.diagnostics[record.ServiceID] = diagnostic
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

// PublishedForService 返回该 service 的 active published Profile 的深拷贝。
// 第二个返回值为 false 表示未配置或已被 quarantine——调用方必须走 Unconfigured 路径，
// 不能猜指标或猜布局。
func (r *Registry) PublishedForService(serviceID string) (model.Profile, bool) {
	profile, ok := r.published[serviceID]
	if !ok {
		return model.Profile{}, false
	}
	return profile.Clone(), true
}

// DiagnosticForService 返回该 service 的配置错误摘要。只有能安全识别 service ID 的
// quarantine 才会出现在这里；结构非法到连 service ID 都不可信的文档不会归属到任何服务。
func (r *Registry) DiagnosticForService(serviceID string) (Diagnostic, bool) {
	diagnostic, ok := r.diagnostics[serviceID]
	if !ok {
		return Diagnostic{}, false
	}
	return Diagnostic{
		ServiceID:   diagnostic.ServiceID,
		ReasonCodes: cloneStrings(diagnostic.ReasonCodes),
		Documents:   cloneStrings(diagnostic.Documents),
	}, true
}

// ListPublished 按 service ID 稳定排序返回全部 active published Profile 的深拷贝。
func (r *Registry) ListPublished() []model.Profile {
	serviceIDs := make([]string, 0, len(r.published))
	for serviceID := range r.published {
		serviceIDs = append(serviceIDs, serviceID)
	}
	sort.Strings(serviceIDs)
	out := make([]model.Profile, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		out = append(out, r.published[serviceID].Clone())
	}
	return out
}

// Quarantined 返回全部隔离记录的副本，按文档名稳定排序。
func (r *Registry) Quarantined() []Quarantined {
	out := make([]Quarantined, len(r.quarantined))
	copy(out, r.quarantined)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Document != out[j].Document {
			return out[i].Document < out[j].Document
		}
		return out[i].ReasonCode < out[j].ReasonCode
	})
	return out
}

// Stats 返回启动日志需要的计数。
func (r *Registry) Stats() Stats {
	return Stats{
		Published:    len(r.published),
		NonPublished: r.nonPublished,
		Quarantined:  len(r.quarantined),
	}
}

func cloneStrings(items []string) []string {
	if items == nil {
		return []string{}
	}
	out := make([]string, len(items))
	copy(out, items)
	return out
}
