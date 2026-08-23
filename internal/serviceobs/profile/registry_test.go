// Profile Registry 的测试矩阵。核心命题只有三条，其余都是它们的展开：
//
//  1. 合法 Profile 进 active registry，非法 Profile 只 quarantine 自己，不阻断其它服务；
//  2. resolver 的 typed not_found/ambiguous 是配置错误（quarantine），DB/error/cancellation
//     是基础设施错误（中止构造）——把后者折叠成前者会让一次数据库抖动看起来像"所有服务
//     都没配 Profile"；
//  3. Registry 只在构造时加载一次，构造后只读、可并发、交出的是深拷贝。
package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
)

const testdataDir = "testdata"

// fakeCatalog 是最小 tool catalog：只回答 Profile 引用的 key 是否已发布。
type fakeCatalog struct{ keys map[string]struct{} }

func newFakeCatalog(keys ...string) *fakeCatalog {
	catalog := &fakeCatalog{keys: map[string]struct{}{}}
	for _, key := range keys {
		catalog.keys[key] = struct{}{}
	}
	return catalog
}

func (c *fakeCatalog) Has(key string) bool {
	_, ok := c.keys[key]
	return ok
}

func canonicalCatalog() *fakeCatalog {
	return newFakeCatalog(
		"context.resolve_focus.v1",
		"topology.expand_neighbors.v1",
		"metrics.query_semantic_signal.v1",
		"evidence.search_logs.v1",
		"evidence.search_traces.v1",
		"domain.list_service_faults.v1",
	)
}

// fakeResolver 把 fixture 里的符号 ID 映射成 typed 结果。
//
// 它刻意只按精确 ID 查表：fixture 的符号 ID 不会被"当成 entity_key 猜一下"，这与真实
// resolver 的约定一致（Registry 不做 entity_key fallback）。
type fakeResolver struct {
	services    map[string]ServiceResolution
	entities    map[string]EntityResolution
	reachable   map[string]map[string]struct{}
	serviceErr  error
	entityErr   error
	reachErr    error
	mu          sync.Mutex
	serviceArgs []string
	entityArgs  []string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		services: map[string]ServiceResolution{
			"svc_e2b_fixture": {State: ResolutionFound, EntityID: "entity_e2b_service", Environment: "production"},
			"svc_postgresql":  {State: ResolutionFound, EntityID: "entity_pg_service", Environment: "production"},
			"svc_draft_only":  {State: ResolutionFound, EntityID: "entity_draft_service", Environment: "production"},
			"svc_ambiguous":   {State: ResolutionAmbiguous},
		},
		entities: map[string]EntityResolution{
			"entity_e2b_service":      {State: ResolutionFound, Environment: "production"},
			"entity_e2b_api":          {State: ResolutionFound, Environment: "production"},
			"entity_e2b_orchestrator": {State: ResolutionFound, Environment: "production"},
			"entity_e2b_host_fleet":   {State: ResolutionFound, Environment: "global"},
			"entity_unrelated_island": {State: ResolutionFound, Environment: "production"},
			"entity_staging_api":      {State: ResolutionFound, Environment: "staging"},
			"entity_pg_service":       {State: ResolutionFound, Environment: "production"},
			"entity_pg_primary":       {State: ResolutionFound, Environment: "production"},
			"entity_draft_service":    {State: ResolutionFound, Environment: "production"},
		},
		reachable: map[string]map[string]struct{}{
			"entity_e2b_service": {
				"entity_e2b_api":          {},
				"entity_e2b_orchestrator": {},
				"entity_e2b_host_fleet":   {},
				"entity_staging_api":      {},
			},
			"entity_pg_service":    {"entity_pg_primary": {}},
			"entity_draft_service": {"entity_e2b_api": {}, "entity_e2b_orchestrator": {}},
		},
	}
}

func (r *fakeResolver) ResolveService(_ context.Context, serviceID string) (ServiceResolution, error) {
	r.mu.Lock()
	r.serviceArgs = append(r.serviceArgs, serviceID)
	r.mu.Unlock()
	if r.serviceErr != nil {
		return ServiceResolution{}, r.serviceErr
	}
	if resolution, ok := r.services[serviceID]; ok {
		return resolution, nil
	}
	return ServiceResolution{State: ResolutionNotFound}, nil
}

func (r *fakeResolver) ResolveEntity(_ context.Context, entityID string) (EntityResolution, error) {
	r.mu.Lock()
	r.entityArgs = append(r.entityArgs, entityID)
	r.mu.Unlock()
	if r.entityErr != nil {
		return EntityResolution{}, r.entityErr
	}
	if resolution, ok := r.entities[entityID]; ok {
		return resolution, nil
	}
	return EntityResolution{State: ResolutionNotFound}, nil
}

func (r *fakeResolver) DeclaredReachableEntityIDs(_ context.Context, serviceEntityID string, _ int) (map[string]struct{}, error) {
	if r.reachErr != nil {
		return nil, r.reachErr
	}
	out := map[string]struct{}{}
	for id := range r.reachable[serviceEntityID] {
		out[id] = struct{}{}
	}
	return out, nil
}

func loadTestdata(t *testing.T, names ...string) *MapSource {
	t.Helper()
	source := &MapSource{Documents: map[string][]byte{}}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(testdataDir, name))
		if err != nil {
			t.Fatalf("读取 testdata %s 失败: %v", name, err)
		}
		source.Documents[name] = raw
	}
	return source
}

func newRegistry(t *testing.T, source Source, resolver Resolver) *Registry {
	t.Helper()
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	registry, err := New(context.Background(), Options{
		Source: source, Validator: validator, Tools: canonicalCatalog(), Resolver: resolver,
	})
	if err != nil {
		t.Fatalf("构造 Registry 失败: %v", err)
	}
	return registry
}

// 未配置 Profile 目录是合法状态：启动成功，registry 为空。总览据此显示 Unconfigured。
func TestNewWithoutSourceYieldsEmptyRegistry(t *testing.T) {
	registry := newRegistry(t, nil, newFakeResolver())
	if len(registry.ListPublished()) != 0 || len(registry.Quarantined()) != 0 {
		t.Fatalf("未配置目录时 registry 必须为空: %+v", registry.Stats())
	}
	if _, ok := registry.PublishedForService("svc_e2b_fixture"); ok {
		t.Fatalf("空 registry 不应返回 Profile")
	}
	if _, ok := registry.DiagnosticForService("svc_e2b_fixture"); ok {
		t.Fatalf("空 registry 不应返回配置错误诊断")
	}
}

// 显式配置的目录不可读必须让构造失败：否则部署者会误以为 Profile 已加载。
func TestNewFailsWhenSourceUnreadable(t *testing.T) {
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	sentinel := errors.New("permission denied")
	_, err = New(context.Background(), Options{
		Source: &MapSource{ListErr: sentinel}, Validator: validator,
		Tools: canonicalCatalog(), Resolver: newFakeResolver(),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("目录不可读应中止构造, got %v", err)
	}

	if _, err := New(context.Background(), Options{
		Source:    NewDirSource(filepath.Join(t.TempDir(), "does-not-exist")),
		Validator: validator, Tools: canonicalCatalog(), Resolver: newFakeResolver(),
	}); err == nil {
		t.Fatalf("不存在的目录应中止构造")
	}
}

// canonical E2B example + fake resolver + canonical tool catalog 必须成功。
func TestNewAcceptsCanonicalE2BProfile(t *testing.T) {
	registry := newRegistry(t, loadTestdata(t, "valid-e2b.json"), newFakeResolver())
	if quarantined := registry.Quarantined(); len(quarantined) != 0 {
		t.Fatalf("canonical E2B Profile 不应被隔离: %+v", quarantined)
	}
	profile, ok := registry.PublishedForService("svc_e2b_fixture")
	if !ok {
		t.Fatalf("E2B Profile 应进入 active registry")
	}
	if profile.ProfileKey != "e2b" || profile.Status != model.ProfileStatusPublished {
		t.Fatalf("Profile 内容错误: %+v", profile.ProfileKey)
	}
	if len(profile.GoldenBindings()) != 4 {
		t.Fatalf("golden binding=%d, want 4", len(profile.GoldenBindings()))
	}
	if stats := registry.Stats(); stats.Published != 1 || stats.Quarantined != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(registry.ListPublished()) != 1 {
		t.Fatalf("ListPublished 数量错误")
	}
}

// 非 service 作用域省略 refs 或给空数组都是合法 v1 Profile：canonical profile schema 里
// scope.refs 是 optional 且没有 minItems，设计交接稿、契约 README 与 Phase 2 计划也都没有
// 要求它非空。Registry 不得自行缩小合法 Profile 集合，否则一份完全符合契约的 Profile 会被
// 无授权地 quarantine。
func TestNewAcceptsNonServiceScopeWithoutRefs(t *testing.T) {
	registry := newRegistry(t, loadTestdata(t, "valid-scope-refs-optional.json"), newFakeResolver())

	if quarantined := registry.Quarantined(); len(quarantined) != 0 {
		t.Fatalf("省略/留空 scope.refs 的合法 Profile 不应被隔离: %+v", quarantined)
	}
	profile, ok := registry.PublishedForService("svc_e2b_fixture")
	if !ok {
		t.Fatalf("省略/留空 scope.refs 的 Profile 应进入 active registry")
	}

	// 确认 fixture 真的覆盖了三种写法，否则这条回归会变成空跑。
	var omitted, empty, resourceOmitted bool
	for _, b := range profile.SignalBindings {
		switch b.Key {
		case "red_api_orchestrator_rate":
			omitted = b.Scope.Kind == model.ScopeKindRelation && b.Scope.Refs == nil
		case "red_api_orchestrator_errors":
			empty = b.Scope.Kind == model.ScopeKindRelation && b.Scope.Refs != nil && len(b.Scope.Refs) == 0
		case "use_host_placement_saturation":
			resourceOmitted = b.Scope.Kind == model.ScopeKindResource && b.Scope.Refs == nil
		}
	}
	if !omitted || !empty || !resourceOmitted {
		t.Fatalf("fixture 未覆盖三种写法: relation 省略=%v relation 空=%v resource 省略=%v",
			omitted, empty, resourceOmitted)
	}
}

// 每一类结构/引用错误都要落到明确的稳定 reason code，而不是笼统的"校验失败"。
func TestNewQuarantinesInvalidDocuments(t *testing.T) {
	cases := []struct {
		file       string
		reasonCode string
	}{
		{"invalid-schema-profile-version.json", ReasonSchemaInvalid},
		{"invalid-unknown-field.json", ReasonUnknownField},
		// 顶层 forbidden key 先被第 2 步的 unknown-field 拒绝命中，这正是"双重拒绝"
		// 想要的结果：文档在还没进 schema 之前就已经出局。递归扫描器本身由
		// TestScanForbiddenKeysIsRecursiveAndKeyOnly 单独覆盖。
		{"invalid-forbidden-key.json", ReasonUnknownField},
		{"invalid-focus-pattern.json", ReasonInvalidFocusPattern},
		{"invalid-zero-primary-focus.json", ReasonPrimaryFocusNotUnique},
		{"invalid-two-primary-focus.json", ReasonPrimaryFocusNotUnique},
		{"invalid-dangling-signal-ref.json", ReasonDanglingRef},
		{"invalid-dangling-journey-ref.json", ReasonDanglingRef},
		{"invalid-undeclared-tool-key.json", ReasonDanglingRef},
		{"invalid-threshold-direction.json", ReasonInvalidThreshold},
		{"invalid-golden-missing-traffic.json", ReasonGoldenBindingInvalid},
		{"invalid-golden-extra-unprojected.json", ReasonGoldenBindingInvalid},
		{"invalid-golden-overview-truncated.json", ReasonGoldenBindingInvalid},
		{"invalid-two-golden-overview-views.json", ReasonGoldenBindingInvalid},
		{"invalid-duplicate-view-order.json", ReasonDuplicateKey},
		{"invalid-proxy-reason-blank.json", ReasonProxyReasonMissing},
		{"invalid-unpublished-tool.json", ReasonToolNotPublished},
		{"invalid-service-not-found.json", ReasonRefNotFound},
		{"invalid-service-ambiguous.json", ReasonRefAmbiguous},
		{"invalid-root-not-found.json", ReasonRefNotFound},
		{"invalid-root-unreachable.json", ReasonRootUnreachable},
		{"invalid-root-environment-mismatch.json", ReasonRootEnvironmentMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			registry := newRegistry(t, loadTestdata(t, tc.file), newFakeResolver())
			if len(registry.ListPublished()) != 0 {
				t.Fatalf("非法 Profile 不得进入 active registry")
			}
			quarantined := registry.Quarantined()
			if len(quarantined) != 1 {
				t.Fatalf("隔离记录数=%d, want 1: %+v", len(quarantined), quarantined)
			}
			if quarantined[0].ReasonCode != tc.reasonCode {
				t.Fatalf("reason_code=%q want %q (detail=%q)",
					quarantined[0].ReasonCode, tc.reasonCode, quarantined[0].Detail)
			}
			if quarantined[0].Document != tc.file {
				t.Fatalf("document=%q want %q", quarantined[0].Document, tc.file)
			}
		})
	}
}

// forbidden key 的递归扫描是纵深防御：v1 的 Profile schema 每层都是
// additionalProperties=false，所以 schema 已经先拦住了它。这里直接单测扫描器本身，
// 保证将来某个 schema 分支被放开时仍然 fail closed。
func TestScanForbiddenKeysIsRecursiveAndKeyOnly(t *testing.T) {
	for _, key := range []string{"query", "promql", "sql", "logql", "shell", "secret",
		"token", "headers", "prompt", "html", "executable"} {
		nested := map[string]any{
			"views": []any{
				map[string]any{"key": "overview", "extra": map[string]any{key: "x"}},
			},
		}
		path, found := scanForbiddenKeys(nested, "")
		if !found {
			t.Fatalf("嵌套的 %q 必须被扫出来", key)
		}
		if !strings.HasSuffix(path, "/"+key) {
			t.Fatalf("path=%q 应以 /%s 结尾", path, key)
		}
	}

	// 只看键名：合法说明文字里出现 SQL/查询等词不算违规。
	clean := map[string]any{
		"signal_bindings": []any{map[string]any{
			"key":          "golden_create_latency",
			"proxy_reason": "该指标由服务端 query template 计算，不含 SQL 或 PromQL 正文",
		}},
	}
	if path, found := scanForbiddenKeys(clean, ""); found {
		t.Fatalf("合法说明文字不应被判违规: %s", path)
	}
}

// 单个坏文件不能阻断其它合法 Profile；两个服务各自独立。
func TestOneBadDocumentDoesNotBlockOthers(t *testing.T) {
	source := loadTestdata(t,
		"valid-e2b.json",
		"valid-second-service.json",
		"invalid-schema-profile-version.json",
		"invalid-root-unreachable.json",
	)
	registry := newRegistry(t, source, newFakeResolver())

	if _, ok := registry.PublishedForService("svc_e2b_fixture"); !ok {
		t.Fatalf("E2B Profile 应仍然 active")
	}
	if _, ok := registry.PublishedForService("svc_postgresql"); !ok {
		t.Fatalf("第二个服务的 Profile 应仍然 active")
	}
	stats := registry.Stats()
	if stats.Published != 2 || stats.Quarantined != 2 {
		t.Fatalf("stats=%+v want published=2 quarantined=2", stats)
	}
}

// draft/deprecated 是合法但未发布：不进 active registry，也不算配置错误。
func TestNonPublishedProfilesAreNotActiveAndNotQuarantined(t *testing.T) {
	registry := newRegistry(t, loadTestdata(t, "valid-draft-not-published.json"), newFakeResolver())
	if _, ok := registry.PublishedForService("svc_draft_only"); ok {
		t.Fatalf("draft Profile 不应 active")
	}
	if len(registry.Quarantined()) != 0 {
		t.Fatalf("draft Profile 不应被隔离: %+v", registry.Quarantined())
	}
	if stats := registry.Stats(); stats.NonPublished != 1 || stats.Published != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

// 同一 service 有两个 published Profile：全部隔离，绝不按文件名或加载顺序挑一个。
func TestDuplicatePublishedQuarantinesAllConflicts(t *testing.T) {
	registry := newRegistry(t,
		loadTestdata(t, "valid-e2b.json", "conflict-duplicate-published.json"), newFakeResolver())

	if _, ok := registry.PublishedForService("svc_e2b_fixture"); ok {
		t.Fatalf("冲突时不得挑一个作为 active Profile")
	}
	quarantined := registry.Quarantined()
	if len(quarantined) != 2 {
		t.Fatalf("隔离记录数=%d, want 2: %+v", len(quarantined), quarantined)
	}
	for _, record := range quarantined {
		if record.ReasonCode != ReasonDuplicatePublished {
			t.Fatalf("reason_code=%q want %q", record.ReasonCode, ReasonDuplicatePublished)
		}
		if record.ServiceID != "svc_e2b_fixture" {
			t.Fatalf("service_id=%q", record.ServiceID)
		}
	}

	diagnostic, ok := registry.DiagnosticForService("svc_e2b_fixture")
	if !ok {
		t.Fatalf("冲突服务应有配置错误诊断")
	}
	if len(diagnostic.ReasonCodes) != 1 || diagnostic.ReasonCodes[0] != ReasonDuplicatePublished {
		t.Fatalf("诊断 reason codes=%v", diagnostic.ReasonCodes)
	}
	if len(diagnostic.Documents) != 2 {
		t.Fatalf("诊断应列出两个冲突文档: %v", diagnostic.Documents)
	}
}

// 引用错误发生在能安全识别 service ID 之后，因此该 service 拿得到配置错误诊断；
// 而结构非法到 service ID 本身都不可信的文档不归属任何服务。
func TestDiagnosticOnlyForSafelyIdentifiedServices(t *testing.T) {
	registry := newRegistry(t, loadTestdata(t, "invalid-root-unreachable.json"), newFakeResolver())
	diagnostic, ok := registry.DiagnosticForService("svc_e2b_fixture")
	if !ok {
		t.Fatalf("引用错误应归属到 svc_e2b_fixture")
	}
	if len(diagnostic.ReasonCodes) != 1 || diagnostic.ReasonCodes[0] != ReasonRootUnreachable {
		t.Fatalf("reason codes=%v", diagnostic.ReasonCodes)
	}

	schemaBroken := newRegistry(t, loadTestdata(t, "invalid-schema-profile-version.json"), newFakeResolver())
	if _, ok := schemaBroken.DiagnosticForService("svc_e2b_fixture"); ok {
		t.Fatalf("schema 非法的文档不应把 service ID 当成可信值")
	}
	if len(schemaBroken.Quarantined()) != 1 {
		t.Fatalf("仍应记录一条隔离")
	}
}

// resolver 的基础设施错误必须中止整个构造。这三条覆盖 service 查询、entity 查询与
// declared 可达性遍历，任一失败都不能被折叠成 Unconfigured。
func TestResolverInfrastructureErrorAbortsConstruction(t *testing.T) {
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	sentinel := errors.New("connection refused")

	cases := []struct {
		name  string
		apply func(*fakeResolver)
	}{
		{"service 查询失败", func(r *fakeResolver) { r.serviceErr = sentinel }},
		{"entity 查询失败", func(r *fakeResolver) { r.entityErr = sentinel }},
		{"declared 可达性遍历失败", func(r *fakeResolver) { r.reachErr = sentinel }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := newFakeResolver()
			tc.apply(resolver)
			_, err := New(context.Background(), Options{
				Source: loadTestdata(t, "valid-e2b.json"), Validator: validator,
				Tools: canonicalCatalog(), Resolver: resolver,
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("基础设施错误必须中止构造, got %v", err)
			}
		})
	}
}

// context 取消同样是基础设施错误：不能因为一次取消就宣布所有服务未配置。
func TestResolverCancellationAbortsConstruction(t *testing.T) {
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolver := newFakeResolver()
	resolver.serviceErr = context.Canceled
	_, err = New(ctx, Options{
		Source: loadTestdata(t, "valid-e2b.json"), Validator: validator,
		Tools: canonicalCatalog(), Resolver: resolver,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消必须中止构造, got %v", err)
	}
}

// Registry 把 v1 文档里的 ID 原样交给 resolver：不改写、不追加前缀、不退化成 entity_key。
func TestRegistryPassesRealIDsToResolverWithoutFallback(t *testing.T) {
	resolver := newFakeResolver()
	newRegistry(t, loadTestdata(t, "valid-e2b.json"), resolver)

	resolver.mu.Lock()
	serviceArgs := append([]string(nil), resolver.serviceArgs...)
	entityArgs := append([]string(nil), resolver.entityArgs...)
	resolver.mu.Unlock()

	if len(serviceArgs) != 1 || serviceArgs[0] != "svc_e2b_fixture" {
		t.Fatalf("service resolver 入参=%v，必须原样是 service_ref.service_id", serviceArgs)
	}
	want := map[string]bool{"entity_e2b_api": true, "entity_e2b_orchestrator": true}
	if len(entityArgs) != len(want) {
		t.Fatalf("entity resolver 入参=%v", entityArgs)
	}
	for _, arg := range entityArgs {
		if !want[arg] {
			t.Fatalf("entity resolver 收到未声明的入参 %q（疑似 entity_key fallback）", arg)
		}
	}
}

// service entity 自身可以作为 root，不需要出现在可达集合里。
func TestServiceEntityItselfIsAcceptedAsRoot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testdataDir, "valid-e2b.json"))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	serviceRef, _ := document["service_ref"].(map[string]any)
	serviceRef["root_entity_ids"] = []any{"entity_e2b_service"}
	patched, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	registry := newRegistry(t, &MapSource{Documents: map[string][]byte{"self-root.json": patched}}, newFakeResolver())
	if len(registry.Quarantined()) != 0 {
		t.Fatalf("service entity 自身应被接受为 root: %+v", registry.Quarantined())
	}
	if _, ok := registry.PublishedForService("svc_e2b_fixture"); !ok {
		t.Fatalf("Profile 应 active")
	}
}

// global 实体跨环境共享：允许被任意环境的 service 引用。
func TestGlobalRootEntityIsAcceptedAcrossEnvironments(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join(testdataDir, "valid-e2b.json"))
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	serviceRef, _ := document["service_ref"].(map[string]any)
	serviceRef["root_entity_ids"] = []any{"entity_e2b_host_fleet"}
	patched, _ := json.Marshal(document)

	registry := newRegistry(t, &MapSource{Documents: map[string][]byte{"global-root.json": patched}}, newFakeResolver())
	if len(registry.Quarantined()) != 0 {
		t.Fatalf("global 实体应被接受为 root: %+v", registry.Quarantined())
	}
}

// 非 .json、symlink、超大文件、目录穿越名字都不能进入候选集合。
func TestDirSourceRejectsNonRegularAndOversizedDocuments(t *testing.T) {
	dir := t.TempDir()
	valid, err := os.ReadFile(filepath.Join(testdataDir, "valid-e2b.json"))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "valid.json"), valid, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested.json"), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, valid, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatalf("建 symlink 失败: %v", err)
	}

	source := NewDirSource(dir)
	names, err := source.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(names) != 1 || names[0] != "valid.json" {
		t.Fatalf("候选文档=%v，只应包含目录直属的普通 .json 文件", names)
	}

	for _, bad := range []string{"../outside.json", "sub/valid.json", "valid.yaml", ""} {
		if _, err := source.Read(bad); !errors.Is(err, ErrNotRegularJSONFile) {
			t.Fatalf("Read(%q) 应返回 ErrNotRegularJSONFile, got %v", bad, err)
		}
	}

	oversized := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(oversized, make([]byte, MaxDocumentBytes+1), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := source.Read("huge.json"); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("超大文档应返回 ErrDocumentTooLarge, got %v", err)
	}

	registry := newRegistry(t, source, newFakeResolver())
	if _, ok := registry.PublishedForService("svc_e2b_fixture"); !ok {
		t.Fatalf("合法 Profile 应 active")
	}
	quarantined := registry.Quarantined()
	if len(quarantined) != 1 || quarantined[0].ReasonCode != ReasonTooLarge {
		t.Fatalf("超大文档应以 %s 隔离: %+v", ReasonTooLarge, quarantined)
	}
}

// DirSource.Read 的安全边界：普通文件正常读到原始字节，symlink 即使按名字直接读也必须
// 被拒绝（Open 会跟随 symlink，所以拒绝必须发生在打开之前），大小上限在边界上精确生效。
func TestDirSourceReadRegularFileSymlinkAndSizeBoundary(t *testing.T) {
	dir := t.TempDir()
	content, err := os.ReadFile(filepath.Join(testdataDir, "valid-e2b.json"))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.json"), content, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	source := NewDirSource(dir)

	got, err := source.Read("plain.json")
	if err != nil {
		t.Fatalf("普通文件应可读: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("读到的内容与写入的不一致")
	}

	// symlink 指向目录内的合法文件也要拒绝：Read 的入参可能不来自 List。
	if err := os.Symlink(filepath.Join(dir, "plain.json"), filepath.Join(dir, "inside-link.json")); err != nil {
		t.Fatalf("建 symlink 失败: %v", err)
	}
	if _, err := source.Read("inside-link.json"); !errors.Is(err, ErrNotRegularJSONFile) {
		t.Fatalf("指向目录内文件的 symlink 也应被拒绝, got %v", err)
	}

	// symlink 指向目录外的文件（经典穿越形态）同样拒绝。
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside-link.json")); err != nil {
		t.Fatalf("建 symlink 失败: %v", err)
	}
	if _, err := source.Read("outside-link.json"); !errors.Is(err, ErrNotRegularJSONFile) {
		t.Fatalf("指向目录外文件的 symlink 应被拒绝, got %v", err)
	}

	// 目录即使叫 .json 也不是普通文件。
	if err := os.MkdirAll(filepath.Join(dir, "adir.json"), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if _, err := source.Read("adir.json"); !errors.Is(err, ErrNotRegularJSONFile) {
		t.Fatalf("目录应被拒绝, got %v", err)
	}

	// 大小上限的精确边界：恰好等于上限可读，多一个字节被拒。
	atLimit := filepath.Join(dir, "at-limit.json")
	if err := os.WriteFile(atLimit, bytes.Repeat([]byte("x"), MaxDocumentBytes), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	raw, err := source.Read("at-limit.json")
	if err != nil {
		t.Fatalf("恰好等于上限的文档应可读: %v", err)
	}
	if len(raw) != MaxDocumentBytes {
		t.Fatalf("读到 %d 字节, want %d", len(raw), MaxDocumentBytes)
	}

	overLimit := filepath.Join(dir, "over-limit.json")
	if err := os.WriteFile(overLimit, bytes.Repeat([]byte("x"), MaxDocumentBytes+1), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := source.Read("over-limit.json"); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("超过上限一个字节就应被拒绝, got %v", err)
	}

	// 无论成功还是失败，返回的内容都不可能超过上限。
	for _, name := range []string{"plain.json", "at-limit.json", "over-limit.json"} {
		body, _ := source.Read(name)
		if len(body) > MaxDocumentBytes {
			t.Fatalf("Read(%s) 返回 %d 字节，超过硬上限", name, len(body))
		}
	}
}

// 身份检查：路径在 Lstat 与实际读取之间被换掉时，绝不能返回替换目标的内容。
//
// 这个窗口只能通过并发替换来触及，所以这里用固定轮数的 rename 交换来撞它。覆盖是概率性的，
// 但断言是确定性的：任何一次成功的 Read 都必须返回登记文档的原始内容，永远不能返回诱饵。
// 实测中修复前的实现（Lstat 通过后再按路径 os.ReadFile）在第 321 次读取就返回了诱饵内容。
//
// symlink/目录/超限这些确定性用例在
// TestDirSourceReadRegularFileSymlinkAndSizeBoundary 里覆盖，所以即使某次运行没撞到窗口，
// 安全边界本身仍然有确定性的回归保护。
func TestDirSourceReadNeverReturnsSwappedContent(t *testing.T) {
	dir := t.TempDir()
	legit := []byte(`{"document":"legitimate-profile"}`)
	decoy := []byte(`{"document":"decoy-substituted-content"}`)

	target := filepath.Join(dir, "doc.json")
	if err := os.WriteFile(target, legit, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 诱饵放在目录外，模拟"symlink 把读取引向别处"。
	decoyPath := filepath.Join(t.TempDir(), "decoy.json")
	if err := os.WriteFile(decoyPath, decoy, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 两个候选：一个真正的普通文件，一个指向诱饵的 symlink。用 rename 原子地互换，
	// 保证目标路径始终存在，避免测试自己制造 ENOENT 噪声。
	regularStaging := filepath.Join(dir, ".staging-regular")
	linkStaging := filepath.Join(dir, ".staging-link")
	if err := os.WriteFile(regularStaging, legit, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.Symlink(decoyPath, linkStaging); err != nil {
		t.Fatalf("建 symlink 失败: %v", err)
	}

	source := NewDirSource(dir)
	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 换成 symlink→诱饵，再换回普通文件。每一步都是 rename，因此原子。
			_ = os.Symlink(decoyPath, linkStaging)
			_ = os.Rename(linkStaging, target)
			_ = os.WriteFile(regularStaging, legit, 0o600)
			_ = os.Rename(regularStaging, target)
		}
	}()

	for i := 0; i < 3000; i++ {
		body, err := source.Read("doc.json")
		if err != nil {
			// symlink 命中窗口时被拒绝是正确行为；错误类型不做强断言，因为竞态下
			// stat/open 也可能返回瞬时错误。
			continue
		}
		if bytes.Equal(body, decoy) {
			close(stop)
			swapper.Wait()
			t.Fatalf("第 %d 次 Read 返回了替换后的诱饵内容，说明读取没有绑定到已检查的文件", i)
		}
		if !bytes.Equal(body, legit) {
			close(stop)
			swapper.Wait()
			t.Fatalf("第 %d 次 Read 返回了意料之外的内容: %q", i, body)
		}
	}
	close(stop)
	swapper.Wait()
}

// 目录候选文件数超上限是部署错误（例如指向了日志目录），必须让构造失败。
func TestSourceRejectsTooManyCandidates(t *testing.T) {
	source := &MapSource{Documents: map[string][]byte{}}
	for i := 0; i <= MaxCandidateFiles; i++ {
		source.Documents[fmt.Sprintf("profile-%04d.json", i)] = []byte("{}")
	}
	if _, err := source.List(); err == nil {
		t.Fatalf("候选文件数超上限必须报错")
	}
}

// Registry 只在构造时加载一次：构造后改文件不生效，重建才重新评估。
func TestRegistryDoesNotHotReload(t *testing.T) {
	dir := t.TempDir()
	valid, err := os.ReadFile(filepath.Join(testdataDir, "valid-e2b.json"))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	path := filepath.Join(dir, "e2b.json")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	source := NewDirSource(dir)
	registry := newRegistry(t, source, newFakeResolver())
	if _, ok := registry.PublishedForService("svc_e2b_fixture"); !ok {
		t.Fatalf("Profile 应 active")
	}

	broken, err := os.ReadFile(filepath.Join(testdataDir, "invalid-schema-profile-version.json"))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}

	if _, ok := registry.PublishedForService("svc_e2b_fixture"); !ok {
		t.Fatalf("已构造的 Registry 不应热加载新内容")
	}
	if len(registry.Quarantined()) != 0 {
		t.Fatalf("已构造的 Registry 不应重新评估 quarantine")
	}

	rebuilt := newRegistry(t, source, newFakeResolver())
	if _, ok := rebuilt.PublishedForService("svc_e2b_fixture"); ok {
		t.Fatalf("重建后应发现文档已损坏")
	}
	if len(rebuilt.Quarantined()) != 1 {
		t.Fatalf("重建后应隔离损坏文档")
	}
}

// Registry 交出的必须是深拷贝：一个 handler 改了 Profile 不能污染共享配置。
func TestRegistryReturnsDeepCopies(t *testing.T) {
	registry := newRegistry(t, loadTestdata(t, "valid-e2b.json"), newFakeResolver())

	first, ok := registry.PublishedForService("svc_e2b_fixture")
	if !ok {
		t.Fatalf("Profile 应 active")
	}
	first.ProfileKey = "tampered"
	first.ServiceRef.RootEntityIDs[0] = "tampered"
	first.SignalBindings[0].Key = "tampered"
	first.SignalBindings[0].Thresholds.Critical = -1
	first.Views[0].SignalRefs[0] = "tampered"
	first.ToolRefs[0] = "tampered"

	second, _ := registry.PublishedForService("svc_e2b_fixture")
	if second.ProfileKey == "tampered" || second.ServiceRef.RootEntityIDs[0] == "tampered" ||
		second.SignalBindings[0].Key == "tampered" || second.SignalBindings[0].Thresholds.Critical == -1 ||
		second.Views[0].SignalRefs[0] == "tampered" || second.ToolRefs[0] == "tampered" {
		t.Fatalf("Registry 的共享配置被调用方改写")
	}

	listed := registry.ListPublished()
	listed[0].ProfileKey = "tampered"
	if again, _ := registry.PublishedForService("svc_e2b_fixture"); again.ProfileKey == "tampered" {
		t.Fatalf("ListPublished 返回的不是深拷贝")
	}

	records := registry.Quarantined()
	records = append(records, Quarantined{Document: "injected"})
	if len(registry.Quarantined()) != 0 {
		t.Fatalf("Quarantined 返回的不是副本")
	}
}

// 构造后的 Registry 是只读的，必须能并发访问（go test -race）。
func TestRegistryConcurrentReadsAreRaceFree(t *testing.T) {
	registry := newRegistry(t,
		loadTestdata(t, "valid-e2b.json", "valid-second-service.json", "invalid-root-unreachable.json"),
		newFakeResolver())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry.PublishedForService("svc_e2b_fixture")
			registry.PublishedForService("svc_postgresql")
			registry.DiagnosticForService("svc_e2b_fixture")
			registry.ListPublished()
			registry.Quarantined()
			registry.Stats()
		}()
	}
	wg.Wait()
}

// 装配缺依赖是编程错误，必须显式失败而不是构造出一个"永远 Unconfigured"的 Registry。
func TestNewRequiresInjectedDependencies(t *testing.T) {
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	if _, err := New(context.Background(), Options{Tools: canonicalCatalog(), Resolver: newFakeResolver()}); !errors.Is(err, ErrValidatorRequired) {
		t.Fatalf("缺 validator 应返回 ErrValidatorRequired, got %v", err)
	}
	if _, err := New(context.Background(), Options{Validator: validator, Resolver: newFakeResolver()}); !errors.Is(err, ErrToolsRequired) {
		t.Fatalf("缺 tool catalog 应返回 ErrToolsRequired, got %v", err)
	}
	if _, err := New(context.Background(), Options{Validator: validator, Tools: canonicalCatalog()}); !errors.Is(err, ErrResolverRequired) {
		t.Fatalf("缺 resolver 应返回 ErrResolverRequired, got %v", err)
	}
}
