package profile

import "context"

// ResolutionState 是外部引用解析的 typed 结果。
//
// 它存在的唯一目的是把"确实不存在 / 确实不唯一"与"暂时看不到"彻底分开：前两者是配置
// 错误，只 quarantine 单个 Profile；后者是基础设施错误，必须以 Go error 返回并中止整个
// Registry 构造。把 DB 不可用折叠成 not_found 会让一次数据库抖动看起来像"所有服务都没
// 配 Profile"，这正是"缺数不假绿"要防的那类静默降级。
type ResolutionState string

const (
	ResolutionFound     ResolutionState = "found"
	ResolutionNotFound  ResolutionState = "not_found"
	ResolutionAmbiguous ResolutionState = "ambiguous"
)

// ServiceResolution 是 v1 契约里 service_ref.service_id 的解析结果。
// 输入严格是当前环境内真实的 service_registrations.id，不做 entity_key fallback。
type ServiceResolution struct {
	State ResolutionState
	// EntityID 是该 registration 的 canonical service entity（resource_nodes.id）。
	EntityID string
	// Environment 用于校验 root entity 与 service 同环境。
	Environment string
}

// EntityResolution 是 root_entity_ids 的解析结果，输入是真实的 resource_nodes.id。
type EntityResolution struct {
	State ResolutionState
	// Environment 为 "global" 表示跨环境共享实体，允许被任意环境的 service 引用。
	Environment string
}

// Resolver 由 alert-system adapter 实现（Slice B 的 profileresolver.go）。
//
// 契约：not_found/ambiguous 通过 ResolutionState 表达且 error 为 nil；数据库不可用、
// 查询失败、context 取消一律返回非 nil error。实现绝不允许把后者伪装成前者。
type Resolver interface {
	ResolveService(ctx context.Context, serviceID string) (ServiceResolution, error)
	ResolveEntity(ctx context.Context, entityID string) (EntityResolution, error)
	// DeclaredReachableEntityIDs 返回从 service entity 出发，沿 declared
	// depends_on/part_of canonical 边有界可达的 entity ID 集合（不含 service entity
	// 自身，由 Registry 单独接受）。maxDepth 是硬上界。
	DeclaredReachableEntityIDs(ctx context.Context, serviceEntityID string, maxDepth int) (map[string]struct{}, error)
}
