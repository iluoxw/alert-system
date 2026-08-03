package runbook

import "context"

// availabilityExecutor 让目录接口展示真实运行时能力；它不是授权判断，执行时仍必须
// 经过 Service 的角色、UAT policy 和参数护栏。
type availabilityExecutor interface {
	Availability(actionKey string) (bool, string)
}

func executorAvailability(executor Executor, actionKey string) (bool, string) {
	if provider, ok := executor.(availabilityExecutor); ok {
		return provider.Availability(actionKey)
	}
	return true, ""
}

func (DisabledExecutor) Availability(string) (bool, string) {
	return false, "服务端未配置该动作的执行器"
}

func (f *FakeExecutor) Availability(actionKey string) (bool, string) {
	switch actionKey {
	case ActionStopInvalidRetries, ActionSnapshotAudit, ActionFCAudit, ActionCleanupDryRun, ActionCleanupApply:
		return true, ""
	default:
		return false, "本地假执行器不支持该动作"
	}
}

type RoutingExecutor struct {
	fallback Executor
	routes   map[string]Executor
}

func NewRoutingExecutor(fallback Executor, routes map[string]Executor) *RoutingExecutor {
	if fallback == nil {
		fallback = DisabledExecutor{}
	}
	copyRoutes := make(map[string]Executor, len(routes))
	for actionKey, executor := range routes {
		if executor != nil {
			copyRoutes[actionKey] = executor
		}
	}
	return &RoutingExecutor{fallback: fallback, routes: copyRoutes}
}

func (r *RoutingExecutor) Execute(ctx context.Context, request ExecutionRequest) (map[string]any, error) {
	if executor, ok := r.routes[request.ActionKey]; ok {
		return executor.Execute(ctx, request)
	}
	return r.fallback.Execute(ctx, request)
}

func (r *RoutingExecutor) Availability(actionKey string) (bool, string) {
	if executor, ok := r.routes[actionKey]; ok {
		return executorAvailability(executor, actionKey)
	}
	return executorAvailability(r.fallback, actionKey)
}
