package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// routeCacheTTL 是路由规则缓存的存活时间：≤10s 保证配置变更后至多 10s 内对新事件
// 生效（R5.3），同时避免每个事件都查一次 routes 表。
const routeCacheTTL = 10 * time.Second

// RouteRepo 是 RouteStage 依赖的路由仓储子集，由 store.RouteRepo 实现（结构化满足）。
// ListOrdered 的返回约定按 priority 升序（与其 SQL 的 ORDER BY priority 一致）——
// RouteStage 本身不再排序，直接按传入顺序做 first-match-wins。
type RouteRepo interface {
	ListOrdered(ctx context.Context, tx pgx.Tx, limit int) ([]store.Route, error)
}

// RouteAlertRepo 是 RouteStage 依赖的告警仓储子集，用于把路由结果持久化回 alerts 表。
type RouteAlertRepo interface {
	UpdateRouting(ctx context.Context, tx pgx.Tx, id, teamID, groupKey string) error
}

// routeClock 抽象「当前时间」，供测试注入 fake clock 精确控制缓存过期，无需真的
// 等待 wall-clock 时间流逝（与 dedup/scheduler 同一模式）。
type routeClock interface {
	Now() time.Time
}

type realRouteClock struct{}

func (realRouteClock) Now() time.Time { return time.Now() }

// routeCache 是进程内缓存的一份路由规则快照及其过期时间。
type routeCache struct {
	routes    []store.Route
	expiresAt time.Time
}

// RouteStage 是管道的路由 stage（R5.1-5.3）：按 priority 升序对 routes.matchers
// 做 first-match-wins，命中即用该路由的目标团队并持久化到 pctx.Alert.TeamID；
// 无命中落到构造时注入的默认团队。路由规则在进程内缓存 ≤10s，缓存过期时用当前
// pctx.Tx 重查刷新（不引入额外连接，避免破坏调用方的事务一致性）。
type RouteStage struct {
	routes        RouteRepo
	alerts        RouteAlertRepo
	defaultTeamID string
	clock         routeClock

	mu    sync.RWMutex
	cache *routeCache
}

// NewRouteStage 构造 RouteStage。defaultTeamID 是所有路由规则都未命中时的兜底团队——
// teams 表当前没有 is_default 之类的标记列，因此由调用方在组装依赖时显式给出。
func NewRouteStage(routes RouteRepo, alerts RouteAlertRepo, defaultTeamID string) *RouteStage {
	return &RouteStage{
		routes:        routes,
		alerts:        alerts,
		defaultTeamID: defaultTeamID,
		clock:         realRouteClock{},
	}
}

// Name 返回 "route"。
func (s *RouteStage) Name() string { return "route" }

// Process 实现 Stage 接口。
func (s *RouteStage) Process(ctx context.Context, pctx *PipelineContext) error {
	// 迟到的 resolved 事件在 dedup stage 里因找不到现存告警而被忽略时，pctx.Alert
	// 仍是零值（ID==""）：没有告警可路由，直接跳过，不是错误。
	if pctx.Alert.ID == "" {
		return nil
	}

	routes, err := s.currentRoutes(ctx, pctx.Tx)
	if err != nil {
		return fmt.Errorf("route: 加载路由规则失败: %w", err)
	}

	labels := mergedMatchLabels(pctx.Alert)
	teamID := s.defaultTeamID
	result := RouteResult{TeamID: teamID}

	for i := range routes {
		if !routes[i].Matchers.Matches(labels) {
			continue
		}
		route := routes[i]
		teamID = route.TargetTeamID
		result = RouteResult{
			RouteID:        route.ID,
			TeamID:         teamID,
			GroupBy:        route.GroupBy,
			GroupWait:      time.Duration(route.GroupWaitSeconds) * time.Second,
			GroupInterval:  time.Duration(route.GroupIntervalSeconds) * time.Second,
			RepeatInterval: time.Duration(route.RepeatIntervalSeconds) * time.Second,
		}
		break
	}

	pctx.Route = result
	pctx.Alert.TeamID = teamID

	// group_key 原样传回：本 stage 不计算分组（那是分组 stage 的职责），传入
	// pctx.Alert.GroupKey 当前值只是为了在同一次 UPDATE 里一并写回，避免覆盖掉
	// dedup/GetByFingerprint 已经读出的既有 group_key（合并场景下可能非空）。
	if err := s.alerts.UpdateRouting(ctx, pctx.Tx, pctx.Alert.ID, teamID, pctx.Alert.GroupKey); err != nil {
		return fmt.Errorf("route: 持久化路由结果失败: %w", err)
	}
	return nil
}

// mergedMatchLabels 把 Alert.Labels 与 Alert.ResourceRef 合并为一份用于 matcher 求值
// 的标签视图：domain.MatcherSet.Matches 只接受单个 map，而 routes 表的设计意图是
// 同时按 labels/resource_ref 匹配。ResourceRef 打底、Labels 覆盖——键冲突时以
// Labels（本次事件更贴近业务语义的标签）为准。
func mergedMatchLabels(alert domain.Alert) map[string]string {
	merged := make(map[string]string, len(alert.Labels)+len(alert.ResourceRef))
	for k, v := range alert.ResourceRef {
		merged[k] = v
	}
	for k, v := range alert.Labels {
		merged[k] = v
	}
	return merged
}

// currentRoutes 返回当前缓存的路由规则，过期（或从未加载）则用 tx 重查并刷新缓存。
func (s *RouteStage) currentRoutes(ctx context.Context, tx pgx.Tx) ([]store.Route, error) {
	now := s.clock.Now()

	s.mu.RLock()
	if s.cache != nil && now.Before(s.cache.expiresAt) {
		routes := s.cache.routes
		s.mu.RUnlock()
		return routes, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// 双重检查：拿写锁期间可能已经有另一个 goroutine 完成了刷新。
	if s.cache != nil && now.Before(s.cache.expiresAt) {
		return s.cache.routes, nil
	}

	routes, err := s.routes.ListOrdered(ctx, tx, 0)
	if err != nil {
		return nil, err
	}
	s.cache = &routeCache{routes: routes, expiresAt: now.Add(routeCacheTTL)}
	return routes, nil
}
