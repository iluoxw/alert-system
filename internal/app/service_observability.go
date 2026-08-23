package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"alert-system/internal/config"
	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/adapters/alertsystem"
	"alert-system/internal/serviceobs/cache"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/facade"
	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/serviceobs/tools"
	"alert-system/internal/store"
)

// 本文件是 Service Observability 只读读路径的生产装配。
//
// 它把 Tool catalog、契约 validator、file-based Profile Registry、alert-system 只读
// adapter、Phase 2 的 Unavailable telemetry reader、共享 evaluator、目录摘要缓存与
// facade 组装成一个整体，并负责有界关闭。cmd/server 只注册 API 与 lifecycle，不含任何
// 投影规则。
//
// 三条装配口径：
//
//  1. Profile 目录为空是合法的空 registry（总览 Unconfigured、工作区 409）；显式配置
//     但不可读、或 resolver 报基础设施错误，必须让启动失败——否则部署者会误以为
//     Profile 已加载；
//  2. 启动日志只输出 published/non-published/quarantined 计数与稳定 reason code，
//     绝不输出文档正文、service ID 或路径内容；
//  3. 关闭时等待 cache worker 有界退出。

// ServiceObservabilityStats 是启动自检与日志需要的计数。
type ServiceObservabilityStats struct {
	ProfilesPublished    int
	ProfilesNonPublished int
	ProfilesQuarantined  int
	// QuarantineReasons 是去重排序后的稳定 reason code 集合，可以安全进日志。
	QuarantineReasons []string
	RefreshWorkers    int
	RefreshQueue      int
	CacheMaxEntries   int
}

// ServiceObservability 是装配好的只读读路径。
type ServiceObservability struct {
	// PageViews 是 API 层唯一依赖的编排入口。
	PageViews *facade.Service
	Registry  *profile.Registry
	Cache     *cache.Coordinator
	Snapshots *cache.MemoryStore
	Stats     ServiceObservabilityStats
}

// BuildServiceObservability 装配只读读路径并启动 bounded refresh worker。
func BuildServiceObservability(
	ctx context.Context, db *store.Store, cfg config.ServiceObservabilityConfig, logger *slog.Logger,
) (*ServiceObservability, error) {
	if logger == nil {
		logger = slog.Default()
	}

	validator, err := contract.Default()
	if err != nil {
		return nil, fmt.Errorf("app: 编译 Service Observability 契约 schema 失败: %w", err)
	}
	catalog, err := tools.Canonical()
	if err != nil {
		return nil, fmt.Errorf("app: 加载只读 Tool catalog 失败: %w", err)
	}

	runner := alertsystem.NewRunner(db)
	limits := alertsystem.Limits{}
	registrations := alertsystem.NewRegistrationAdapter(runner, limits, nil)
	topology := alertsystem.NewTopologyAdapter(runner, limits, nil)
	domain := alertsystem.NewDomainAdapter(runner, limits, nil)
	resolver := alertsystem.NewProfileResolver(runner, limits)

	// Profile 目录为空 -> Source 为 nil -> 合法的空 registry。显式目录不可读或 resolver
	// 报基础设施错误都会在这里返回错误并阻断启动。
	var source profile.Source
	if cfg.ProfileDir != "" {
		source = profile.NewDirSource(cfg.ProfileDir)
	}
	registry, err := profile.New(ctx, profile.Options{
		Source:    source,
		Validator: validator,
		Tools:     catalog,
		Resolver:  resolver,
	})
	if err != nil {
		return nil, fmt.Errorf("app: 加载 Service Profile Registry 失败: %w", err)
	}

	assembler := &soctx.Assembler{
		Registrations: registrations,
		Topology:      topology,
		Domain:        domain,
		// Phase 2 唯一注册的遥测端口：它既不读契约 fixture 也不访问外部网络，
		// 只把 Golden/RED/USE 显式降级为 Unknown/Unavailable。
		Observations: serviceobs.NewUnavailableObservationReader(),
		Profiles:     registry,
		// 语义指标与 logs/traces/evidence adapter 都还没接入，因此对应 capabilities
		// 必须是 false——不能因为"接口存在"就报 true。
		TelemetryConnected: false,
		EvidenceConnected:  false,
		Limits:             soctx.Limits{MaxWindowSeconds: cfg.MaxWindowSeconds},
	}

	pageViews, err := facade.New(facade.Options{
		Assembler:  assembler,
		Candidates: registrations,
		Profiles:   registry,
		Limits:     facade.Limits{MaxWindowSeconds: cfg.MaxWindowSeconds},
	})
	if err != nil {
		return nil, fmt.Errorf("app: 装配 Service Observability facade 失败: %w", err)
	}

	snapshots, err := cache.NewMemoryStore(cache.MemoryOptions{
		TTL:        cfg.CacheTTL,
		MaxStale:   cfg.CacheMaxStale,
		MaxEntries: cfg.CacheMaxEntries,
	})
	if err != nil {
		return nil, fmt.Errorf("app: 构造目录摘要缓存失败: %w", err)
	}
	coordinator, err := cache.New(cache.Options{
		Store: snapshots,
		// 刷新走的是与总览完全相同的装配 + evaluator 路径，因此目录与总览不可能对
		// 同一个服务给出不同的 Health/Data。
		Refresh:   pageViews.RefreshSummary,
		Workers:   cfg.RefreshWorkers,
		QueueSize: cfg.RefreshQueue,
	})
	if err != nil {
		return nil, fmt.Errorf("app: 构造目录摘要刷新协调器失败: %w", err)
	}
	// facade 与 coordinator 互为依赖（facade 提供 refresh、coordinator 提供查找），
	// 只能有一处后置赋值来打破这个环。它发生在开始服务请求之前。
	pageViews.SetSummaryCache(coordinator)
	coordinator.Start()

	assembled := &ServiceObservability{
		PageViews: pageViews,
		Registry:  registry,
		Cache:     coordinator,
		Snapshots: snapshots,
		Stats:     serviceObservabilityStats(registry, cfg),
	}
	assembled.logStartup(logger, cfg)
	return assembled, nil
}

// logStartup 输出启动摘要。
//
// 它只输出计数与稳定 reason code：Profile 文档正文、service ID、文件名与目录路径都不
// 进日志——那既可能泄露部署拓扑，也让日志变成配置内容的副本。
func (s *ServiceObservability) logStartup(logger *slog.Logger, cfg config.ServiceObservabilityConfig) {
	attrs := []any{
		"profiles_published", s.Stats.ProfilesPublished,
		"profiles_non_published", s.Stats.ProfilesNonPublished,
		"profiles_quarantined", s.Stats.ProfilesQuarantined,
		"profile_dir_configured", cfg.ProfileDir != "",
		"cache_ttl", cfg.CacheTTL.String(),
		"cache_max_stale", cfg.CacheMaxStale.String(),
		"cache_max_entries", cfg.CacheMaxEntries,
		"refresh_workers", cfg.RefreshWorkers,
		"refresh_queue", cfg.RefreshQueue,
		"max_window_seconds", cfg.MaxWindowSeconds,
	}
	if len(s.Stats.QuarantineReasons) > 0 {
		attrs = append(attrs, "quarantine_reasons", s.Stats.QuarantineReasons)
		logger.Warn("app: Service Observability Profile 存在被隔离的文档", attrs...)
		return
	}
	logger.Info("app: Service Observability 只读读路径已装配", attrs...)
}

// Shutdown 有界地停止 cache worker。
//
// 它必须等到 worker 真的退出：进程在后台刷新持有数据库连接时直接退出，会留下悬挂的
// 只读事务。等待超时返回错误，让调用方能明确报出"没停干净"而不是无限期挂住。
func (s *ServiceObservability) Shutdown(ctx context.Context) error {
	if s == nil || s.Cache == nil {
		return nil
	}
	if err := s.Cache.Shutdown(ctx); err != nil {
		return fmt.Errorf("app: 停止 Service Observability 刷新 worker 失败: %w", err)
	}
	return nil
}

func serviceObservabilityStats(
	registry *profile.Registry, cfg config.ServiceObservabilityConfig,
) ServiceObservabilityStats {
	stats := registry.Stats()
	seen := map[string]struct{}{}
	reasons := []string{}
	for _, record := range registry.Quarantined() {
		if record.ReasonCode == "" {
			continue
		}
		if _, exists := seen[record.ReasonCode]; exists {
			continue
		}
		seen[record.ReasonCode] = struct{}{}
		reasons = append(reasons, record.ReasonCode)
	}
	sort.Strings(reasons)
	return ServiceObservabilityStats{
		ProfilesPublished:    stats.Published,
		ProfilesNonPublished: stats.NonPublished,
		ProfilesQuarantined:  stats.Quarantined,
		QuarantineReasons:    reasons,
		RefreshWorkers:       cfg.RefreshWorkers,
		RefreshQueue:         cfg.RefreshQueue,
		CacheMaxEntries:      cfg.CacheMaxEntries,
	}
}

// DefaultShutdownBudget 是关闭刷新 worker 的默认有界预算。
const DefaultShutdownBudget = 5 * time.Second
