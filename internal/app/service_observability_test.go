package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"alert-system/internal/config"
	"alert-system/internal/serviceobs/cache"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/serviceobs/tools"
)

// 这些测试覆盖装配语义中不需要数据库的部分：Profile 目录的三种状态（未配置 / 合法 /
// 单文档非法）、启动日志只输出计数与稳定 reason code、以及关闭时等待 cache worker
// 有界退出。真实 adapter 的 SQL 由 internal/store 的 integration 测试覆盖。

// fakeResolver 让 Registry 在没有数据库的情况下可测。它对已登记的 ID 返回 found，
// 其余返回 not_found（配置错误，只 quarantine 单个文档），并可以模拟基础设施错误。
type fakeResolver struct {
	services map[string]profile.ServiceResolution
	entities map[string]profile.EntityResolution
	reach    map[string]struct{}
	err      error
}

func (f *fakeResolver) ResolveService(_ context.Context, serviceID string) (profile.ServiceResolution, error) {
	if f.err != nil {
		return profile.ServiceResolution{}, f.err
	}
	if resolution, ok := f.services[serviceID]; ok {
		return resolution, nil
	}
	return profile.ServiceResolution{State: profile.ResolutionNotFound}, nil
}

func (f *fakeResolver) ResolveEntity(_ context.Context, entityID string) (profile.EntityResolution, error) {
	if f.err != nil {
		return profile.EntityResolution{}, f.err
	}
	if resolution, ok := f.entities[entityID]; ok {
		return resolution, nil
	}
	return profile.EntityResolution{State: profile.ResolutionNotFound}, nil
}

func (f *fakeResolver) DeclaredReachableEntityIDs(
	_ context.Context, _ string, _ int,
) (map[string]struct{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.reach, nil
}

func serviceObservabilityTestConfig() config.ServiceObservabilityConfig {
	return config.ServiceObservabilityConfig{
		CacheTTL:         45 * time.Second,
		CacheMaxStale:    5 * time.Minute,
		CacheMaxEntries:  64,
		RefreshWorkers:   2,
		RefreshQueue:     8,
		MaxWindowSeconds: 86400,
	}
}

// buildRegistry 复用生产装配里完全相同的 validator 与 Tool catalog，只把 resolver
// 换成 fake：这样"E2B example 能被真实 schema/catalog 接受"这件事仍然被验证。
func buildRegistry(t *testing.T, dir string, resolver profile.Resolver) (*profile.Registry, error) {
	t.Helper()
	validator, err := contract.Default()
	if err != nil {
		t.Fatalf("编译契约 schema 失败: %v", err)
	}
	catalog, err := tools.Canonical()
	if err != nil {
		t.Fatalf("加载 Tool catalog 失败: %v", err)
	}
	var source profile.Source
	if dir != "" {
		source = profile.NewDirSource(dir)
	}
	return profile.New(context.Background(), profile.Options{
		Source: source, Validator: validator, Tools: catalog, Resolver: resolver,
	})
}

// Profile 目录未配置是合法的空 registry：启动成功，没有任何 published Profile。
func TestServiceObservabilityEmptyProfileDirIsLegalEmptyRegistry(t *testing.T) {
	registry, err := buildRegistry(t, "", &fakeResolver{})
	if err != nil {
		t.Fatalf("未配置 Profile 目录不应让启动失败: %v", err)
	}
	stats := serviceObservabilityStats(registry, serviceObservabilityTestConfig())
	if stats.ProfilesPublished != 0 || stats.ProfilesQuarantined != 0 {
		t.Fatalf("空 registry 计数不符: %+v", stats)
	}
	if len(stats.QuarantineReasons) != 0 {
		t.Fatalf("空 registry 不应有隔离原因: %v", stats.QuarantineReasons)
	}
}

// 显式配置但不可读的目录必须让启动失败：静默降级会让部署者误以为 Profile 已加载。
func TestServiceObservabilityUnreadableProfileDirFailsStartup(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := buildRegistry(t, missing, &fakeResolver{}); err == nil {
		t.Fatal("不可读的 Profile 目录必须让启动失败")
	}
}

// 单个文档非法只 quarantine 它自己：合法 Profile 继续发布，启动日志只出稳定 reason code。
func TestServiceObservabilityQuarantinesSingleInvalidDocument(t *testing.T) {
	dir := t.TempDir()
	valid, err := os.ReadFile("../serviceobs/profile/testdata/valid-e2b.json")
	if err != nil {
		t.Fatalf("读取合法 Profile fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "valid.json"), valid, 0o600); err != nil {
		t.Fatalf("写入合法 Profile 失败: %v", err)
	}
	invalid, err := os.ReadFile("../serviceobs/profile/testdata/invalid-forbidden-key.json")
	if err != nil {
		t.Fatalf("读取非法 Profile fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invalid.json"), invalid, 0o600); err != nil {
		t.Fatalf("写入非法 Profile 失败: %v", err)
	}

	resolver := &fakeResolver{
		services: map[string]profile.ServiceResolution{
			"svc_e2b_fixture": {
				State: profile.ResolutionFound, EntityID: "entity_e2b_service", Environment: "production",
			},
		},
		entities: map[string]profile.EntityResolution{
			"entity_e2b_service":      {State: profile.ResolutionFound, Environment: "production"},
			"entity_e2b_api":          {State: profile.ResolutionFound, Environment: "production"},
			"entity_e2b_orchestrator": {State: profile.ResolutionFound, Environment: "production"},
			"entity_e2b_host_fleet":   {State: profile.ResolutionFound, Environment: "production"},
		},
		reach: map[string]struct{}{
			"entity_e2b_api": {}, "entity_e2b_orchestrator": {}, "entity_e2b_host_fleet": {},
		},
	}
	registry, err := buildRegistry(t, dir, resolver)
	if err != nil {
		t.Fatalf("单个坏文档不应阻断启动: %v", err)
	}
	stats := serviceObservabilityStats(registry, serviceObservabilityTestConfig())
	if stats.ProfilesPublished != 1 {
		t.Fatalf("合法 Profile 应继续发布，实际 published=%d", stats.ProfilesPublished)
	}
	if stats.ProfilesQuarantined != 1 {
		t.Fatalf("非法文档应被隔离，实际 quarantined=%d", stats.ProfilesQuarantined)
	}
	if len(stats.QuarantineReasons) != 1 || stats.QuarantineReasons[0] == "" {
		t.Fatalf("隔离原因必须是稳定 reason code: %v", stats.QuarantineReasons)
	}
}

// resolver 报基础设施错误必须中止整个 Registry 构造：把"暂时看不到"折叠成"没配 Profile"
// 会让一次数据库抖动看起来像全站 Unconfigured。
func TestServiceObservabilityResolverInfrastructureErrorBlocksStartup(t *testing.T) {
	dir := t.TempDir()
	valid, err := os.ReadFile("../serviceobs/profile/testdata/valid-e2b.json")
	if err != nil {
		t.Fatalf("读取合法 Profile fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "valid.json"), valid, 0o600); err != nil {
		t.Fatalf("写入合法 Profile 失败: %v", err)
	}
	if _, err := buildRegistry(t, dir, &fakeResolver{err: context.DeadlineExceeded}); err == nil {
		t.Fatal("resolver 基础设施错误必须阻断启动")
	}
}

// 启动日志只输出计数与稳定 reason code，不输出文档正文、service ID 或目录路径。
func TestServiceObservabilityStartupLogHasNoSensitiveContent(t *testing.T) {
	registry, err := buildRegistry(t, "", &fakeResolver{})
	if err != nil {
		t.Fatalf("构造空 registry 失败: %v", err)
	}
	var buffer strings.Builder
	logger := slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := serviceObservabilityTestConfig()
	cfg.ProfileDir = "/etc/alert-system/profiles/secret-topology"

	assembled := &ServiceObservability{Stats: serviceObservabilityStats(registry, cfg)}
	assembled.logStartup(logger, cfg)

	logged := buffer.String()
	if strings.Contains(logged, cfg.ProfileDir) {
		t.Fatalf("启动日志不得输出 Profile 目录路径: %s", logged)
	}
	for _, forbidden := range []string{"signal_bindings", "promql", "token", "svc_"} {
		if strings.Contains(strings.ToLower(logged), forbidden) {
			t.Fatalf("启动日志泄露 %q: %s", forbidden, logged)
		}
	}
	if !strings.Contains(logged, "profiles_published=0") {
		t.Fatalf("启动日志缺少 published 计数: %s", logged)
	}
	if !strings.Contains(logged, "profile_dir_configured=true") {
		t.Fatalf("启动日志应说明目录是否已配置（布尔而非路径）: %s", logged)
	}
}

// 关闭必须等待 cache worker 有界退出，并且重复关闭无副作用。
func TestServiceObservabilityShutdownWaitsForCacheWorkers(t *testing.T) {
	snapshots, err := cache.NewMemoryStore(cache.MemoryOptions{
		TTL: time.Second, MaxStale: time.Minute, MaxEntries: 8,
	})
	if err != nil {
		t.Fatalf("构造缓存失败: %v", err)
	}
	coordinator, err := cache.New(cache.Options{
		Store:   snapshots,
		Refresh: func(context.Context, cache.Key) (cache.Summary, error) { return cache.Summary{}, nil },
		Workers: 2, QueueSize: 4,
	})
	if err != nil {
		t.Fatalf("构造刷新协调器失败: %v", err)
	}
	coordinator.Start()
	if running := coordinator.Stats().RunningWorkers; running != 2 {
		t.Fatalf("启动后应有 2 个 worker，实际 %d", running)
	}

	assembled := &ServiceObservability{Cache: coordinator, Snapshots: snapshots}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultShutdownBudget)
	defer cancel()
	if err := assembled.Shutdown(ctx); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if running := coordinator.Stats().RunningWorkers; running != 0 {
		t.Fatalf("关闭后 worker 必须全部退出，实际 %d", running)
	}
	if err := assembled.Shutdown(ctx); err != nil {
		t.Fatalf("重复关闭必须无副作用: %v", err)
	}

	// 未装配时关闭是 no-op，避免 main 在装配失败路径上再炸一次。
	var nilAssembled *ServiceObservability
	if err := nilAssembled.Shutdown(ctx); err != nil {
		t.Fatalf("未装配时关闭应是 no-op: %v", err)
	}
}
