package alertsystem

import (
	"context"
	"errors"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/store"
)

// 这些测试覆盖 adapter 的编排语义：边界如何计算、bounds 如何显式标记、focus 白名单从哪里
// 来、typed 错误如何分流、cancellation 是否原样下传。真实 SQL 由
// internal/store 的 integration 测试覆盖（需要真实 PostgreSQL）。

const (
	serviceUUID  = "11111111-1111-4111-8111-111111111111"
	entityUUID   = "22222222-2222-4222-8222-222222222222"
	rootUUID     = "33333333-3333-4333-8333-333333333333"
	providerUUID = "44444444-4444-4444-8444-444444444444"
	otherUUID    = "55555555-5555-4555-8555-555555555555"
)

var fixedNow = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

// fakeReader 记录调用并返回注入结果。它刻意对每个方法都记账，这样"目录不应该逐行拉
// aliases/dependencies"这类要求可以被断言，而不是只靠 code review。
type fakeReader struct {
	identities          []store.ServiceIdentityRow
	identitiesTruncated bool
	identitiesFilter    store.ServiceIdentityFilter
	identitiesErr       error

	identity    store.ServiceIdentityRow
	identityErr error

	dependencies    []store.DeclaredEdgeRow
	dependenciesErr error

	neighborhood      store.NeighborhoodRows
	neighborhoodQuery store.NeighborhoodQuery
	neighborhoodErr   error

	boundary      []string
	boundaryArgs  boundaryCall
	boundaryErr   error
	reachable     map[string]struct{}
	reachableErr  error
	serviceResRow store.ServiceResolutionRow
	serviceResErr error
	entityResRow  store.EntityResolutionRow
	entityResErr  error

	domainRows  store.DomainFactsRows
	domainQuery store.DomainFactsQuery
	domainErr   error

	calls map[string]int
}

type boundaryCall struct {
	serviceEntityID string
	rootEntityIDs   []string
	maxDepth        int
	maxEntities     int
}

func newFakeReader() *fakeReader {
	return &fakeReader{calls: map[string]int{}}
}

func (f *fakeReader) record(name string) { f.calls[name]++ }

func (f *fakeReader) ListServiceIdentities(_ context.Context, filter store.ServiceIdentityFilter) ([]store.ServiceIdentityRow, bool, error) {
	f.record("ListServiceIdentities")
	f.identitiesFilter = filter
	return f.identities, f.identitiesTruncated, f.identitiesErr
}

func (f *fakeReader) GetServiceIdentity(_ context.Context, _ string) (store.ServiceIdentityRow, error) {
	f.record("GetServiceIdentity")
	return f.identity, f.identityErr
}

func (f *fakeReader) ListDeclaredDependencies(_ context.Context, _ string, _ int) ([]store.DeclaredEdgeRow, error) {
	f.record("ListDeclaredDependencies")
	return f.dependencies, f.dependenciesErr
}

func (f *fakeReader) ListCanonicalNeighborhood(_ context.Context, query store.NeighborhoodQuery) (store.NeighborhoodRows, error) {
	f.record("ListCanonicalNeighborhood")
	f.neighborhoodQuery = query
	return f.neighborhood, f.neighborhoodErr
}

func (f *fakeReader) ServiceBoundaryEntityIDs(_ context.Context, serviceEntityID string, rootEntityIDs []string, maxDepth, maxEntities int) ([]string, error) {
	f.record("ServiceBoundaryEntityIDs")
	f.boundaryArgs = boundaryCall{serviceEntityID, rootEntityIDs, maxDepth, maxEntities}
	return f.boundary, f.boundaryErr
}

func (f *fakeReader) DeclaredReachableEntityIDs(_ context.Context, _ string, _ int) (map[string]struct{}, error) {
	f.record("DeclaredReachableEntityIDs")
	return f.reachable, f.reachableErr
}

func (f *fakeReader) ResolveServiceByID(_ context.Context, _ string) (store.ServiceResolutionRow, error) {
	f.record("ResolveServiceByID")
	return f.serviceResRow, f.serviceResErr
}

func (f *fakeReader) ResolveEntityByID(_ context.Context, _ string) (store.EntityResolutionRow, error) {
	f.record("ResolveEntityByID")
	return f.entityResRow, f.entityResErr
}

func (f *fakeReader) ReadDomainFacts(_ context.Context, query store.DomainFactsQuery) (store.DomainFactsRows, error) {
	f.record("ReadDomainFacts")
	f.domainQuery = query
	return f.domainRows, f.domainErr
}

// fakeRunner 把 fakeReader 交给 adapter，并记录开了几次只读事务。每个 adapter 调用必须
// 恰好开一次：这就是"registration/topology/domain 各自在独立只读事务内一致读取"。
type fakeRunner struct {
	reader *fakeReader
	txs    int
	err    error
}

func (r *fakeRunner) WithRead(ctx context.Context, fn func(Reader) error) error {
	r.txs++
	if r.err != nil {
		return r.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(r.reader)
}

func newRunner(reader *fakeReader) *fakeRunner { return &fakeRunner{reader: reader} }

func identityRow() store.ServiceIdentityRow {
	return store.ServiceIdentityRow{
		ServiceID:        serviceUUID,
		EntityID:         entityUUID,
		EntityKey:        "platform/example",
		Name:             "Example",
		Description:      "示例平台能力",
		Layer:            domain.EntityLayerPlatform,
		Kind:             domain.ResourceNodeKindInfrastructureCapability,
		Environment:      "production",
		OwnerTeamID:      "66666666-6666-4666-8666-666666666666",
		OwnerTeamName:    "Platform SRE",
		Tier:             domain.ServiceTier1,
		Enabled:          true,
		EntityObservedAt: fixedNow.Add(-90 * time.Second),
		UpdatedAt:        fixedNow.Add(-10 * time.Minute),
	}
}

func TestListServicesIsLightweightAndParameterized(t *testing.T) {
	reader := newFakeReader()
	reader.identities = []store.ServiceIdentityRow{identityRow()}
	reader.identitiesTruncated = true
	runner := newRunner(reader)
	adapter := NewRegistrationAdapter(runner, Limits{DirectoryLimit: 50}, fixedClock)

	list, err := adapter.ListServices(context.Background(), serviceobs.RegistrationFilter{
		Environment: "production",
		TeamID:      "66666666-6666-4666-8666-666666666666",
		Tier:        "tier-1",
		Query:       "exa",
		Limit:       500,
	})
	if err != nil {
		t.Fatalf("ListServices 失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ServiceID != serviceUUID {
		t.Fatalf("目录结果不符: %+v", list.Items)
	}
	if !list.Truncated {
		t.Fatal("truncated 必须原样上传，否则调用方无法知道还有更多候选行")
	}
	if !list.ReadAt.Equal(fixedNow) {
		t.Fatalf("ReadAt 应来自注入时钟，实际 %v", list.ReadAt)
	}
	// metadata 过滤必须参数化下传，不在 adapter 里做内存过滤。
	if reader.identitiesFilter.Environment != "production" ||
		reader.identitiesFilter.TeamID != "66666666-6666-4666-8666-666666666666" ||
		reader.identitiesFilter.Tier != "tier-1" || reader.identitiesFilter.Query != "exa" {
		t.Fatalf("metadata 过滤未参数化下传: %+v", reader.identitiesFilter)
	}
	// limit 必须被 adapter 上界收敛。
	if reader.identitiesFilter.Limit != 50 {
		t.Fatalf("limit 未被收敛到 adapter 上界，实际 %d", reader.identitiesFilter.Limit)
	}
	// 目录绝不逐行拉 aliases / dependencies。
	if reader.calls["ListDeclaredDependencies"] != 0 {
		t.Fatal("目录查询不得逐行读取 declared dependencies（N+1）")
	}
	if runner.txs != 1 {
		t.Fatalf("目录查询应恰好开一个只读事务，实际 %d", runner.txs)
	}
}

func TestReadServiceReturnsDeclaredDependencies(t *testing.T) {
	reader := newFakeReader()
	reader.identity = identityRow()
	reader.dependencies = []store.DeclaredEdgeRow{{
		RelationID:   "77777777-7777-4777-8777-777777777777",
		FromEntityID: entityUUID,
		ToEntityID:   providerUUID,
		ToEntityKey:  "infrastructure/example/db",
		ToEntityName: "PostgreSQL",
		ToLayer:      domain.EntityLayerInfrastructure,
		ToKind:       "resource",
		Relation:     "depends_on",
		Criticality:  "critical",
		ObservedAt:   fixedNow.Add(-time.Hour),
	}}
	runner := newRunner(reader)
	adapter := NewRegistrationAdapter(runner, Limits{}, fixedClock)

	facts, err := adapter.ReadService(context.Background(), serviceUUID)
	if err != nil {
		t.Fatalf("ReadService 失败: %v", err)
	}
	if len(facts.Dependencies) != 1 {
		t.Fatalf("declared 依赖数量不符: %+v", facts.Dependencies)
	}
	dependency := facts.Dependencies[0]
	// 方向固定 consumer -> provider，登记依赖永远是 declared。
	if dependency.FromEntityID != entityUUID || dependency.ToEntityID != providerUUID {
		t.Fatalf("依赖方向错误: %+v", dependency)
	}
	if dependency.Provenance != model.ProvenanceDeclared {
		t.Fatalf("登记依赖必须是 declared，实际 %q", dependency.Provenance)
	}
	if dependency.Criticality != "critical" {
		t.Fatalf("criticality 必须原样保留，实际 %q", dependency.Criticality)
	}
	// 身份与依赖在同一个一致读事务内完成，不会出现撕裂快照。
	if runner.txs != 1 {
		t.Fatalf("ReadService 应恰好开一个只读事务，实际 %d", runner.txs)
	}
	// FactAnchor 必须是真实持久化时间，不是当前时间。
	anchor := facts.FactAnchor()
	if !anchor.Equal(reader.identity.EntityObservedAt) {
		t.Fatalf("FactAnchor 应取最新真实事实时间，实际 %v", anchor)
	}
	if anchor.Equal(fixedNow) {
		t.Fatal("FactAnchor 绝不能等于当前时间")
	}
}

func TestReadServiceMapsMissingAndMalformedIDToNotFound(t *testing.T) {
	for name, injected := range map[string]error{
		"不存在":     store.ErrServiceObservabilityNotFound,
		"ID 形状非法": store.ErrServiceObservabilityInvalidID,
	} {
		t.Run(name, func(t *testing.T) {
			reader := newFakeReader()
			reader.identityErr = injected
			adapter := NewRegistrationAdapter(newRunner(reader), Limits{}, fixedClock)
			_, err := adapter.ReadService(context.Background(), "whatever")
			if !errors.Is(err, serviceobs.ErrServiceNotFound) {
				t.Fatalf("应统一映射成 ErrServiceNotFound（不泄露存在性），实际 %v", err)
			}
		})
	}
}

func TestReadServicePropagatesInfrastructureError(t *testing.T) {
	reader := newFakeReader()
	sentinel := errors.New("connection refused")
	reader.identityErr = sentinel
	adapter := NewRegistrationAdapter(newRunner(reader), Limits{}, fixedClock)
	_, err := adapter.ReadService(context.Background(), serviceUUID)
	if errors.Is(err, serviceobs.ErrServiceNotFound) {
		t.Fatal("基础设施错误绝不能被折叠成 not found")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("基础设施错误必须原样上抛，实际 %v", err)
	}
}

func TestReadServicePropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := NewRegistrationAdapter(newRunner(newFakeReader()), Limits{}, fixedClock)
	_, err := adapter.ReadService(ctx, serviceUUID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation 必须原样下传，实际 %v", err)
	}
}

func TestReadTopologySeedsServiceRootsAndDeclaredDependencies(t *testing.T) {
	reader := newFakeReader()
	reader.neighborhood = store.NeighborhoodRows{
		Nodes: []store.NeighborhoodNodeRow{{
			EntityID: entityUUID, EntityKey: "platform/example", Name: "Example",
			Layer: domain.EntityLayerPlatform, Kind: "infrastructure_capability",
			Environment: "production", Depth: 0, ObservedAt: fixedNow.Add(-2 * time.Minute),
		}},
	}
	adapter := NewTopologyAdapter(newRunner(reader), Limits{}, fixedClock)

	_, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{
		ServiceEntityID: entityUUID,
		RootEntityIDs:   []string{rootUUID},
		DeclaredDependencies: []serviceobs.DeclaredDependency{
			{ToEntityID: providerUUID},
		},
	})
	if err != nil {
		t.Fatalf("ReadTopology 失败: %v", err)
	}
	seeds := reader.neighborhoodQuery.SeedEntityIDs
	for _, want := range []string{entityUUID, rootUUID, providerUUID} {
		if !containsString(seeds, want) {
			t.Fatalf("seed 集合缺少 %s: %v", want, seeds)
		}
	}
}

func TestReadTopologyWithoutProfileStillGivesDeclaredOverview(t *testing.T) {
	// 无 Profile：没有 root，但 service entity 与 registration declared dependencies
	// 仍然构成基础 declared 概览。
	reader := newFakeReader()
	reader.neighborhood = store.NeighborhoodRows{
		Nodes: []store.NeighborhoodNodeRow{
			{EntityID: entityUUID, Name: "Example", Layer: domain.EntityLayerPlatform, Kind: "infrastructure_capability"},
			{EntityID: providerUUID, Name: "PostgreSQL", Layer: domain.EntityLayerInfrastructure, Kind: "resource", Depth: 1},
		},
		Relations: []store.NeighborhoodRelationRow{{
			RelationID:     "77777777-7777-4777-8777-777777777777",
			SourceEntityID: entityUUID, TargetEntityID: providerUUID,
			Relation: "depends_on", Origin: "declared", Criticality: "critical",
			ObservedAt: fixedNow.Add(-72 * time.Hour), Stale: false,
		}},
	}
	adapter := NewTopologyAdapter(newRunner(reader), Limits{}, fixedClock)

	facts, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{
		ServiceEntityID:      entityUUID,
		DeclaredDependencies: []serviceobs.DeclaredDependency{{ToEntityID: providerUUID}},
	})
	if err != nil {
		t.Fatalf("ReadTopology 失败: %v", err)
	}
	if len(facts.Nodes) != 2 || len(facts.Relations) != 1 {
		t.Fatalf("无 Profile 时仍应有基础 declared 概览: %+v", facts)
	}
	if facts.Relations[0].Provenance != model.ProvenanceDeclared {
		t.Fatalf("declared 边的 provenance 错误: %q", facts.Relations[0].Provenance)
	}
	if facts.Relations[0].Stale {
		t.Fatal("declared 配置在被显式替换前一直有效，不因时间过期")
	}
	if facts.Partial {
		t.Fatal("未达上限时不应标 partial")
	}
}

func TestReadTopologyMarksPartialWhenBoundsReached(t *testing.T) {
	reader := newFakeReader()
	reader.neighborhood = store.NeighborhoodRows{
		Nodes:              []store.NeighborhoodNodeRow{{EntityID: entityUUID, Name: "Example"}},
		NodesTruncated:     true,
		RelationsTruncated: false,
	}
	adapter := NewTopologyAdapter(newRunner(reader), Limits{}, fixedClock)
	facts, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{ServiceEntityID: entityUUID})
	if err != nil {
		t.Fatalf("ReadTopology 失败: %v", err)
	}
	if !facts.Partial {
		t.Fatal("达到 node 上限必须显式 partial，不能静默截断")
	}
	if facts.PartialReason != serviceobs.ReasonTopologyBoundsReached {
		t.Fatalf("partial reason 必须稳定，实际 %q", facts.PartialReason)
	}
}

func TestReadTopologyKeepsObservedStaleAndDeterministicSnapshotID(t *testing.T) {
	reader := newFakeReader()
	reader.neighborhood = store.NeighborhoodRows{
		Nodes: []store.NeighborhoodNodeRow{{EntityID: entityUUID, Name: "Example"}},
		Relations: []store.NeighborhoodRelationRow{{
			RelationID:     "88888888-8888-4888-8888-888888888888",
			SourceEntityID: entityUUID, TargetEntityID: providerUUID,
			Relation: "depends_on", Origin: "observed",
			ObservedAt: fixedNow.Add(-48 * time.Hour), Stale: true,
		}},
	}
	adapter := NewTopologyAdapter(newRunner(reader), Limits{}, fixedClock)
	first, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{ServiceEntityID: entityUUID})
	if err != nil {
		t.Fatalf("ReadTopology 失败: %v", err)
	}
	if first.Relations[0].Provenance != model.ProvenanceObserved || !first.Relations[0].Stale {
		t.Fatalf("observed 关系过期必须标 stale: %+v", first.Relations[0])
	}
	second, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{ServiceEntityID: entityUUID})
	if err != nil {
		t.Fatalf("第二次 ReadTopology 失败: %v", err)
	}
	if first.SnapshotID != second.SnapshotID || first.SnapshotID == "" {
		t.Fatalf("snapshot ID 必须由内容确定性派生: %q vs %q", first.SnapshotID, second.SnapshotID)
	}
	// ObservedAt 是邻域里最新的真实观测时间，不是读取时刻。
	if first.ObservedAt.Equal(fixedNow) {
		t.Fatal("topology ObservedAt 不能是读取时刻")
	}
}

func TestReadTopologyEmptySeedsDoesNotWidenQuery(t *testing.T) {
	reader := newFakeReader()
	adapter := NewTopologyAdapter(newRunner(reader), Limits{}, fixedClock)
	facts, err := adapter.ReadTopology(context.Background(), serviceobs.TopologyRequest{})
	if err != nil {
		t.Fatalf("ReadTopology 失败: %v", err)
	}
	if len(facts.Nodes) != 0 || len(facts.Relations) != 0 {
		t.Fatal("没有 seed 时必须返回空邻域，不能发明节点")
	}
	if len(reader.neighborhoodQuery.SeedEntityIDs) != 1 || reader.neighborhoodQuery.SeedEntityIDs[0] != "" {
		// 空 service entity 只会作为一个非法 seed 下传，由 store 层归一化后丢弃。
		t.Fatalf("seed 集合被意外扩大: %v", reader.neighborhoodQuery.SeedEntityIDs)
	}
}

func domainRows() store.DomainFactsRows {
	return store.DomainFactsRows{
		Alerts: []store.DomainAlertRow{{
			AlertID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Title: "HugePages 紧张",
			Status: "firing", UpstreamFingerprint: "fp-1", SourceState: "firing",
			ResponseState: "unacknowledged", SuppressionState: "silenced", Severity: "critical",
			StartsAt: fixedNow.Add(-5 * time.Minute), UpdatedAt: fixedNow.Add(-4 * time.Minute),
		}},
		Faults: []store.DomainFaultRow{{
			FaultID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Title: "Placement 容量失守",
			Status: "active", Severity: "critical", Kind: "capacity", Scope: "capability",
			EntityID: entityUUID, EntityName: "Example", OwnerTeamName: "Platform SRE",
			FirstDetectedAt: fixedNow.Add(-13 * time.Minute), UpdatedAt: fixedNow.Add(-2 * time.Minute),
			ConfirmedImpactCount: 1, PotentialImpactCount: 2,
		}},
		Impacts: []store.DomainImpactRow{{
			FaultID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", EntityID: providerUUID,
			EntityName: "Checkout", Layer: domain.EntityLayerApplication, State: "confirmed",
			UpdatedAt: fixedNow.Add(-time.Minute),
		}},
		Incidents: []store.DomainIncidentRow{{
			IncidentID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Number: 42,
			Title: "Sandbox 创建失败", Status: "mitigating", Severity: "critical",
			Relation: "primary", UpdatedAt: fixedNow.Add(-30 * time.Second),
		}},
		Counts:       store.DomainCountRow{ActiveFaults: 1, ConfirmedImpacts: 1, ActiveIncidents: 1},
		LatestFactAt: fixedNow.Add(-30 * time.Second),
	}
}

func TestReadDomainUsesIndependentBoundary(t *testing.T) {
	reader := newFakeReader()
	reader.boundary = []string{entityUUID, rootUUID}
	reader.domainRows = domainRows()
	runner := newRunner(reader)
	adapter := NewDomainAdapter(runner, Limits{BoundaryDepth: 2, MaxBoundaryEntities: 50, DomainLimit: 100}, fixedClock)

	facts, err := adapter.ReadDomain(context.Background(), serviceobs.DomainRequest{
		ServiceID:       serviceUUID,
		ServiceEntityID: entityUUID,
		RootEntityIDs:   []string{rootUUID},
	})
	if err != nil {
		t.Fatalf("ReadDomain 失败: %v", err)
	}
	// 边界只由 service entity 与 Profile roots 决定，绝不接受 topology 的返回。
	if reader.boundaryArgs.serviceEntityID != entityUUID ||
		len(reader.boundaryArgs.rootEntityIDs) != 1 || reader.boundaryArgs.rootEntityIDs[0] != rootUUID {
		t.Fatalf("边界入参不符: %+v", reader.boundaryArgs)
	}
	if reader.boundaryArgs.maxDepth != 2 || reader.boundaryArgs.maxEntities != 50 {
		t.Fatalf("边界上界未生效: %+v", reader.boundaryArgs)
	}
	if len(facts.BoundaryEntityIDs) != 2 {
		t.Fatalf("边界应原样返回供审计: %v", facts.BoundaryEntityIDs)
	}
	if facts.Counts.ActiveFaults != 1 || facts.Counts.ConfirmedImpacts != 1 || facts.Counts.ActiveIncidents != 1 {
		t.Fatalf("领域计数不符: %+v", facts.Counts)
	}
	// Silenced 只影响通知，仍是有效证据，必须保留。
	if facts.Alerts[0].SuppressionState != "silenced" || facts.Alerts[0].State != "firing" {
		t.Fatalf("silenced Alert 必须保留且状态取上游权威: %+v", facts.Alerts[0])
	}
	if facts.Faults[0].State != "active" || facts.Faults[0].Severity != model.FaultSeverityCritical {
		t.Fatalf("Fault 投影不符: %+v", facts.Faults[0])
	}
	if !facts.LatestFactAt.Equal(fixedNow.Add(-30 * time.Second)) {
		t.Fatalf("LatestFactAt 应是最新真实事实时间，实际 %v", facts.LatestFactAt)
	}
	if runner.txs != 1 {
		t.Fatalf("domain 应恰好开一个只读事务，实际 %d", runner.txs)
	}
}

func TestReadDomainSurvivesTopologyFailure(t *testing.T) {
	// topology adapter 完全失败，domain adapter 仍按自己独立计算的边界返回事实，
	// 而且不会把第二个服务的实体拉进边界。
	reader := newFakeReader()
	reader.neighborhoodErr = errors.New("topology query failed")
	reader.boundary = []string{entityUUID}
	reader.domainRows = domainRows()
	runner := newRunner(reader)

	topology := NewTopologyAdapter(runner, Limits{}, fixedClock)
	if _, err := topology.ReadTopology(context.Background(), serviceobs.TopologyRequest{ServiceEntityID: entityUUID}); err == nil {
		t.Fatal("topology 应返回错误")
	}

	domainAdapter := NewDomainAdapter(runner, Limits{}, fixedClock)
	facts, err := domainAdapter.ReadDomain(context.Background(), serviceobs.DomainRequest{
		ServiceID:       serviceUUID,
		ServiceEntityID: entityUUID,
	})
	if err != nil {
		t.Fatalf("topology 失败不得影响 domain: %v", err)
	}
	if len(facts.Faults) != 1 {
		t.Fatal("topology 失败时 domain 不得变空")
	}
	if containsString(reader.boundaryArgs.rootEntityIDs, otherUUID) {
		t.Fatal("边界不得扩大到第二个服务")
	}
}

func TestReadDomainEmptyBoundarySkipsQuery(t *testing.T) {
	reader := newFakeReader()
	reader.boundary = []string{}
	adapter := NewDomainAdapter(newRunner(reader), Limits{}, fixedClock)
	facts, err := adapter.ReadDomain(context.Background(), serviceobs.DomainRequest{ServiceEntityID: entityUUID})
	if err != nil {
		t.Fatalf("ReadDomain 失败: %v", err)
	}
	if reader.calls["ReadDomainFacts"] != 0 {
		t.Fatal("边界为空时不得发起领域查询（否则等于查全库）")
	}
	if len(facts.Faults) != 0 || facts.Counts.ActiveFaults != 0 {
		t.Fatalf("边界为空时必须返回空事实: %+v", facts)
	}
}

func TestReadDomainFocusWhitelistComesFromPublishedProfile(t *testing.T) {
	reader := newFakeReader()
	reader.boundary = []string{entityUUID}
	reader.domainRows = domainRows()
	reader.domainRows.FocusApplied = true
	adapter := NewDomainAdapter(newRunner(reader), Limits{}, fixedClock)

	facts, err := adapter.ReadDomain(context.Background(), serviceobs.DomainRequest{
		ServiceEntityID: entityUUID,
		// 白名单就是已发布 Profile 的 focus_kinds key 集合。
		AllowedFocusKinds: []string{"sandbox_id", "trace_id"},
		Focus:             &model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"},
	})
	if err != nil {
		t.Fatalf("ReadDomain 失败: %v", err)
	}
	if reader.domainQuery.FocusKey != "sandbox_id" || reader.domainQuery.FocusValue != "sbx_hp16c3" {
		t.Fatalf("focus 必须作为参数下传: %+v", reader.domainQuery)
	}
	if !facts.FocusApplied {
		t.Fatal("focus 生效必须显式可见")
	}
}

func TestReadDomainRejectsFocusKindOutsideProfile(t *testing.T) {
	cases := map[string]serviceobs.DomainRequest{
		"未由 Profile 声明": {
			ServiceEntityID:   entityUUID,
			AllowedFocusKinds: []string{"sandbox_id"},
			Focus:             &model.Focus{Kind: "customer_email", Value: "a@b.c"},
		},
		"Profile 未声明任何 focus": {
			ServiceEntityID: entityUUID,
			Focus:           &model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"},
		},
		"value 为空": {
			ServiceEntityID:   entityUUID,
			AllowedFocusKinds: []string{"sandbox_id"},
			Focus:             &model.Focus{Kind: "sandbox_id"},
		},
		"value 超长": {
			ServiceEntityID:   entityUUID,
			AllowedFocusKinds: []string{"sandbox_id"},
			Focus:             &model.Focus{Kind: "sandbox_id", Value: string(make([]byte, maxFocusValueLength+1))},
		},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			reader := newFakeReader()
			reader.boundary = []string{entityUUID}
			adapter := NewDomainAdapter(newRunner(reader), Limits{}, fixedClock)
			_, err := adapter.ReadDomain(context.Background(), request)
			if !errors.Is(err, serviceobs.ErrInvalidFocus) {
				t.Fatalf("非法 focus 必须 typed 拒绝，实际 %v", err)
			}
			if reader.calls["ReadDomainFacts"] != 0 {
				t.Fatal("非法 focus 不得下发查询")
			}
		})
	}
}

func TestReadDomainWithoutFocusReadsWholeBoundary(t *testing.T) {
	reader := newFakeReader()
	reader.boundary = []string{entityUUID}
	reader.domainRows = domainRows()
	adapter := NewDomainAdapter(newRunner(reader), Limits{}, fixedClock)
	if _, err := adapter.ReadDomain(context.Background(), serviceobs.DomainRequest{
		ServiceEntityID:   entityUUID,
		AllowedFocusKinds: []string{"sandbox_id"},
	}); err != nil {
		t.Fatalf("ReadDomain 失败: %v", err)
	}
	if reader.domainQuery.FocusKey != "" || reader.domainQuery.FocusValue != "" {
		t.Fatalf("无 focus 时不得下传匹配条件: %+v", reader.domainQuery)
	}
}

func TestReadDomainPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := NewDomainAdapter(newRunner(newFakeReader()), Limits{}, fixedClock)
	if _, err := adapter.ReadDomain(ctx, serviceobs.DomainRequest{ServiceEntityID: entityUUID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation 必须原样下传，实际 %v", err)
	}
}

func TestUnavailableObservationReaderIsStableAndOffline(t *testing.T) {
	reader := serviceobs.NewUnavailableObservationReader()
	profileDoc := model.Profile{
		ProfileKey: "example", ProfileVersion: 1,
		SignalBindings: []model.SignalBinding{
			{Key: "golden_latency", Family: model.SignalFamilyGolden, Semantic: model.SemanticLatency},
			{Key: "golden_traffic", Family: model.SignalFamilyGolden, Semantic: model.SemanticTraffic},
		},
	}
	facts, err := reader.ReadObservations(context.Background(), serviceobs.ObservationRequest{Profile: profileDoc})
	if err != nil {
		t.Fatalf("遥测未接入是稳定事实，不应返回 error: %v", err)
	}
	if len(facts.Signals) != 2 {
		t.Fatalf("每个已声明 binding 都必须有一条显式观测: %+v", facts.Signals)
	}
	for _, signal := range facts.Signals {
		if signal.Availability != model.SignalAvailabilityUnavailable {
			t.Fatalf("%s availability 应为 unavailable，实际 %q", signal.BindingKey, signal.Availability)
		}
		if signal.UnknownReason != serviceobs.ReasonTelemetryNotConnected {
			t.Fatalf("%s unknown reason 必须稳定，实际 %q", signal.BindingKey, signal.UnknownReason)
		}
		if signal.Value != nil {
			t.Fatalf("%s 未接入遥测不得有数值", signal.BindingKey)
		}
		// 没有观测：observed_at 必须保持零值，绝不能填当前时间伪装成刚刚观测过。
		if !signal.ObservedAt.IsZero() {
			t.Fatalf("%s observed_at 必须是零值，实际 %v", signal.BindingKey, signal.ObservedAt)
		}
	}
	if len(facts.Sources) != 1 || facts.Sources[0].State != model.AvailabilityUnavailable ||
		facts.Sources[0].Freshness != model.FreshnessUnknown {
		t.Fatalf("source 状态不符: %+v", facts.Sources)
	}
	if facts.Sources[0].Retryable {
		t.Fatal("重试不会让未接入的遥测变可用")
	}
	if len(facts.EvidenceRefs) != 0 {
		t.Fatal("Phase 2 不得产出任何 EvidenceRef（也不得读取 fixture）")
	}
}

func TestProfileResolverSeparatesNotFoundFromInfrastructureError(t *testing.T) {
	t.Run("service 不存在只 quarantine 单个 Profile", func(t *testing.T) {
		reader := newFakeReader()
		reader.serviceResErr = store.ErrServiceObservabilityNotFound
		resolver := NewProfileResolver(newRunner(reader), Limits{})
		resolution, err := resolver.ResolveService(context.Background(), serviceUUID)
		if err != nil {
			t.Fatalf("typed not_found 不应返回 error: %v", err)
		}
		if resolution.State != profile.ResolutionNotFound {
			t.Fatalf("状态应为 not_found，实际 %q", resolution.State)
		}
	})

	t.Run("符号 fixture ID 不被当成 entity_key 猜测", func(t *testing.T) {
		reader := newFakeReader()
		reader.serviceResErr = store.ErrServiceObservabilityInvalidID
		resolver := NewProfileResolver(newRunner(reader), Limits{})
		resolution, err := resolver.ResolveService(context.Background(), "svc_e2b_fixture")
		if err != nil {
			t.Fatalf("形状非法的 ID 应 typed not_found: %v", err)
		}
		if resolution.State != profile.ResolutionNotFound {
			t.Fatalf("状态应为 not_found，实际 %q", resolution.State)
		}
	})

	t.Run("unmanaged 实体不是权威服务身份", func(t *testing.T) {
		reader := newFakeReader()
		reader.serviceResRow = store.ServiceResolutionRow{
			ServiceID: serviceUUID, EntityID: entityUUID, Environment: "production", Managed: false,
		}
		resolver := NewProfileResolver(newRunner(reader), Limits{})
		resolution, err := resolver.ResolveService(context.Background(), serviceUUID)
		if err != nil {
			t.Fatalf("不应返回 error: %v", err)
		}
		if resolution.State != profile.ResolutionNotFound {
			t.Fatalf("unmanaged 实体应 not_found，实际 %q", resolution.State)
		}
	})

	t.Run("DB 不可用中止整个 Registry 构造", func(t *testing.T) {
		reader := newFakeReader()
		sentinel := errors.New("connection refused")
		reader.serviceResErr = sentinel
		resolver := NewProfileResolver(newRunner(reader), Limits{})
		_, err := resolver.ResolveService(context.Background(), serviceUUID)
		if !errors.Is(err, sentinel) {
			t.Fatalf("基础设施错误必须原样上抛，实际 %v", err)
		}
	})

	t.Run("context 取消是基础设施错误", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resolver := NewProfileResolver(newRunner(newFakeReader()), Limits{})
		_, err := resolver.ResolveService(ctx, serviceUUID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消必须原样上抛，实际 %v", err)
		}
	})

	t.Run("root entity 解析返回真实环境", func(t *testing.T) {
		reader := newFakeReader()
		reader.entityResRow = store.EntityResolutionRow{EntityID: rootUUID, Environment: "global"}
		resolver := NewProfileResolver(newRunner(reader), Limits{})
		resolution, err := resolver.ResolveEntity(context.Background(), rootUUID)
		if err != nil {
			t.Fatalf("ResolveEntity 失败: %v", err)
		}
		if resolution.State != profile.ResolutionFound || resolution.Environment != "global" {
			t.Fatalf("root entity 解析结果不符: %+v", resolution)
		}
	})
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
