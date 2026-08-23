package alertsystem

import (
	"context"
	"errors"
	"fmt"

	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/store"
)

// ProfileResolver 是 Profile Registry 的真实外部引用解析器。
//
// 它严格按 v1 契约的字段本义查询：`service_ref.service_id` 是
// `service_registrations.id`，`root_entity_ids` 是 `resource_nodes.id`。**绝不做
// entity_key fallback**——那会把跨环境不可移植的符号 ID 猜成真实实体，正是 §4.2 明令
// 禁止的偷换语义。
//
// typed 分流是它存在的核心价值：
//
//   - 确实不存在 / 形状非法 -> ResolutionNotFound，只 quarantine 单个 Profile；
//   - 不唯一 -> ResolutionAmbiguous，同样只 quarantine 单个 Profile；
//   - 数据库不可用、查询失败、context 取消 -> 非 nil error，中止整个 Registry 构造。
//
// 把最后一类折叠成 not_found 会让一次数据库抖动看起来像"所有服务都没配 Profile"，
// 这正是"缺数不假绿"要防的静默降级。
type ProfileResolver struct {
	runner ReadRunner
	limits Limits
}

// NewProfileResolver 构造真实 resolver。
func NewProfileResolver(runner ReadRunner, limits Limits) *ProfileResolver {
	return &ProfileResolver{runner: runner, limits: limits.withDefaults()}
}

var _ profile.Resolver = (*ProfileResolver)(nil)

// ResolveService 解析 service_ref.service_id。
//
// 除了存在性，还校验 canonical service entity 必须是 managed：自动发现的 unmanaged
// 实体不是权威服务身份，不能作为 Profile 的绑定目标。
func (r *ProfileResolver) ResolveService(
	ctx context.Context, serviceID string,
) (profile.ServiceResolution, error) {
	var resolution profile.ServiceResolution
	err := r.runner.WithRead(ctx, func(reader Reader) error {
		row, err := reader.ResolveServiceByID(ctx, serviceID)
		if err != nil {
			return err
		}
		if !row.Managed {
			// 存在但不是受管服务实体：这是配置错误，不是基础设施错误。
			resolution = profile.ServiceResolution{State: profile.ResolutionNotFound}
			return nil
		}
		resolution = profile.ServiceResolution{
			State:       profile.ResolutionFound,
			EntityID:    row.EntityID,
			Environment: row.Environment,
		}
		return nil
	})
	if err != nil {
		if notFound, ok := classifyResolutionError(err); ok {
			return profile.ServiceResolution{State: notFound}, nil
		}
		return profile.ServiceResolution{}, fmt.Errorf("alertsystem: 解析 Profile service_ref 失败: %w", err)
	}
	return resolution, nil
}

// ResolveEntity 解析 root_entity_ids 中的一个实体。
//
// 环境校验由 Registry 完成（它同时持有 service 的环境）；这里只负责返回真实环境值，
// 并拒绝把单台 `infrastructure/resource` 之外的判断塞进 resolver。
func (r *ProfileResolver) ResolveEntity(
	ctx context.Context, entityID string,
) (profile.EntityResolution, error) {
	var resolution profile.EntityResolution
	err := r.runner.WithRead(ctx, func(reader Reader) error {
		row, err := reader.ResolveEntityByID(ctx, entityID)
		if err != nil {
			return err
		}
		resolution = profile.EntityResolution{
			State:       profile.ResolutionFound,
			Environment: row.Environment,
		}
		return nil
	})
	if err != nil {
		if notFound, ok := classifyResolutionError(err); ok {
			return profile.EntityResolution{State: notFound}, nil
		}
		return profile.EntityResolution{}, fmt.Errorf("alertsystem: 解析 Profile root entity 失败: %w", err)
	}
	return resolution, nil
}

// DeclaredReachableEntityIDs 返回从 service entity 出发沿 declared depends_on/part_of
// 有界可达的实体集合。它只做可达性，不做环境判断。
func (r *ProfileResolver) DeclaredReachableEntityIDs(
	ctx context.Context, serviceEntityID string, maxDepth int,
) (map[string]struct{}, error) {
	if maxDepth <= 0 {
		maxDepth = r.limits.ReachableDepth
	}
	var reachable map[string]struct{}
	err := r.runner.WithRead(ctx, func(reader Reader) error {
		found, err := reader.DeclaredReachableEntityIDs(ctx, serviceEntityID, maxDepth)
		if err != nil {
			return err
		}
		reachable = found
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrServiceObservabilityInvalidID) {
			// service entity ID 形状非法：没有可达集合，但这不是基础设施错误。
			// service 解析那一步已经会 quarantine 该 Profile。
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("alertsystem: 遍历 declared 可达实体失败: %w", err)
	}
	return reachable, nil
}

// classifyResolutionError 只把"确实不存在"与"形状非法"识别成 typed not_found。
// 其它任何 error（含 context 取消、连接失败、SQL 错误）都必须原样上抛。
//
// 这里没有 ambiguous 分支，而且这正是按 ID 解析的收益：service_registrations.id 与
// resource_nodes.id 都是主键，一个 ID 不可能解析出两个对象。ambiguous 是 entity_key
// fallback 才会引入的风险，而本 resolver 明确不做 fallback。
func classifyResolutionError(err error) (profile.ResolutionState, bool) {
	switch {
	case errors.Is(err, store.ErrServiceObservabilityNotFound),
		errors.Is(err, store.ErrServiceObservabilityInvalidID):
		return profile.ResolutionNotFound, true
	default:
		return "", false
	}
}
