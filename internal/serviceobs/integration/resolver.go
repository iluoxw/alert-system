package integration

import "context"

// ResolutionState 是 selector 解析的 typed 结果。
//
// 它与 profile.ResolutionState 的动机完全一致：把"确实不存在 / 确实不唯一"与"暂时看不到"
// 彻底分开。前两者是 package 配置错误，编译必须 fail closed；后者是基础设施错误，必须以
// Go error 返回。把数据库抖动折叠成 not_found 会让一次连接失败看起来像"这个环境里没有这个
// 服务"，从而编出一个引用错误 ID 的 Profile。
type ResolutionState string

const (
	ResolutionFound     ResolutionState = "found"
	ResolutionNotFound  ResolutionState = "not_found"
	ResolutionAmbiguous ResolutionState = "ambiguous"
)

// ServiceSelectorResolution 是 service selector 的解析结果。
type ServiceSelectorResolution struct {
	State ResolutionState
	// ServiceID 是真实的 service_registrations.id。
	ServiceID string
	// EntityID 是该 registration 的 canonical service entity（resource_nodes.id）。
	EntityID string
	// Environment 是 registration 的真实环境，用于与 package target 交叉校验。
	Environment string
}

// EntitySelectorResolution 是 entity selector 的解析结果，EntityID 是真实 resource_nodes.id。
type EntitySelectorResolution struct {
	State    ResolutionState
	EntityID string
	// Environment 为 "global" 表示跨环境共享实体。
	Environment string
}

// RelationSelectorResolution 是 relation selector 的解析结果，RelationID 是真实依赖边主键。
type RelationSelectorResolution struct {
	State      ResolutionState
	RelationID string
}

// SelectorResolver 由 alert-system adapter 实现（后续 batch）。
//
// 契约：not_found/ambiguous 通过 ResolutionState 表达且 error 为 nil；数据库不可用、查询
// 失败、context 取消一律返回非 nil error。实现绝不允许把后者伪装成前者。
//
// 全部方法都带 environment：同一个 entity_key 可以在多个环境存在，解析必须锚定到 package
// 声明的目标环境，否则会把另一个环境的拓扑编进 Profile。
type SelectorResolver interface {
	ResolveServiceSelector(ctx context.Context, environment, entityKey string) (ServiceSelectorResolution, error)
	ResolveEntitySelector(ctx context.Context, environment, entityKey string) (EntitySelectorResolution, error)
	ResolveRelationSelector(ctx context.Context, environment string, selector RelationSelector) (RelationSelectorResolution, error)
}
