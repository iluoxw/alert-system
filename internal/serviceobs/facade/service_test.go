package facade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/cache"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/serviceobs/view"
)

var (
	fixedNow    = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)
	factAnchor  = time.Date(2026, 8, 22, 2, 5, 13, 0, time.UTC)
	fixedClock  = func() time.Time { return fixedNow }
	staticReqID = func() (string, error) { return "req_facade_test", nil }
)

// ---- 测试替身 ----

// fakeCandidates 是候选扫描端口的替身。它记账被下传的 filter，用来证明 metadata 过滤
// 参数化下传，而 health/data_status 从不下推。
type fakeCandidates struct {
	items   []serviceobs.ServiceIdentity
	request serviceobs.CandidateScanRequest
	calls   int
	err     error
}

func (f *fakeCandidates) ScanCandidates(
	ctx context.Context, req serviceobs.CandidateScanRequest,
) (serviceobs.CandidateSet, error) {
	f.calls++
	f.request = req
	if f.err != nil {
		return serviceobs.CandidateSet{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return serviceobs.CandidateSet{}, err
	}
	return serviceobs.CandidateSet{Items: f.items, ReadAt: fixedNow}, nil
}

// fakeSummaries 记录每个 key 被读了几次：目录必须对每个候选恰好读一次缓存。
type fakeSummaries struct {
	entries map[string]cache.Summary
	stale   map[string]bool
	lookups map[string]int
	total   int
}

func newFakeSummaries() *fakeSummaries {
	return &fakeSummaries{
		entries: map[string]cache.Summary{},
		stale:   map[string]bool{},
		lookups: map[string]int{},
	}
}

func (f *fakeSummaries) Lookup(key cache.Key, placeholder cache.Summary) cache.Result {
	f.total++
	f.lookups[key.ServiceID]++
	summary, ok := f.entries[key.ServiceID]
	if !ok {
		return cache.Result{Summary: placeholder, State: cache.StateMiss, Placeholder: true, Enqueued: true}
	}
	if f.stale[key.ServiceID] {
		return cache.Result{Summary: summary, State: cache.StateStale, Stale: true, Enqueued: true}
	}
	return cache.Result{Summary: summary, State: cache.StateFresh}
}

// fakeRegistrations / fakeTopology / fakeDomain 只提供总览与工作区需要的最小事实。
type fakeReaders struct {
	facts       map[string]serviceobs.ServiceFacts
	topologyErr error
	domainErr   error
	observed    time.Time
}

func (f *fakeReaders) ListServices(context.Context, serviceobs.RegistrationFilter) (serviceobs.RegistrationList, error) {
	return serviceobs.RegistrationList{Items: []serviceobs.ServiceIdentity{}}, nil
}

func (f *fakeReaders) ReadService(_ context.Context, serviceID string) (serviceobs.ServiceFacts, error) {
	facts, ok := f.facts[serviceID]
	if !ok {
		return serviceobs.ServiceFacts{}, serviceobs.ErrServiceNotFound
	}
	return facts, nil
}

func (f *fakeReaders) ReadTopology(_ context.Context, req serviceobs.TopologyRequest) (serviceobs.TopologyFacts, error) {
	if f.topologyErr != nil {
		return serviceobs.TopologyFacts{}, f.topologyErr
	}
	return serviceobs.TopologyFacts{
		SnapshotID: "topo_test", ObservedAt: f.observed,
		Nodes: []serviceobs.TopologyNode{{
			EntityID: req.ServiceEntityID, EntityKey: "platform/example", Name: "Example",
			Layer: model.EntityLayerPlatform, Kind: "infrastructure_capability",
			Environment: "production", ObservedAt: f.observed,
		}},
		Relations: []serviceobs.TopologyRelation{},
		ReadAt:    fixedNow,
	}, nil
}

func (f *fakeReaders) ReadDomain(_ context.Context, _ serviceobs.DomainRequest) (serviceobs.DomainFacts, error) {
	if f.domainErr != nil {
		return serviceobs.DomainFacts{}, f.domainErr
	}
	return serviceobs.DomainFacts{
		BoundaryEntityIDs: []string{},
		Alerts:            []serviceobs.DomainAlert{},
		Faults:            []serviceobs.DomainFault{},
		Impacts:           []serviceobs.DomainImpact{},
		Incidents:         []serviceobs.DomainIncident{},
		Counts:            model.DomainCounts{},
		LatestFactAt:      factAnchor,
		ReadAt:            fixedNow,
	}, nil
}

// fakeProfiles 只对显式登记的 service 返回 published Profile。
type fakeProfiles struct {
	published   map[string]model.Profile
	diagnostics map[string]profile.Diagnostic
}

func (f fakeProfiles) PublishedForService(serviceID string) (model.Profile, bool) {
	published, ok := f.published[serviceID]
	return published, ok
}

func (f fakeProfiles) DiagnosticForService(serviceID string) (profile.Diagnostic, bool) {
	diagnostic, ok := f.diagnostics[serviceID]
	return diagnostic, ok
}

// ---- 装配 helper ----

func identity(serviceID, name, owner string, tier model.ServiceTier) serviceobs.ServiceIdentity {
	return serviceobs.ServiceIdentity{
		ServiceID: serviceID, EntityID: "entity-" + serviceID, EntityKey: "platform/" + serviceID,
		Name: name, Description: name + " 描述", Layer: model.EntityLayerPlatform,
		Kind: model.EntityKindInfrastructureCapability, Environment: "production",
		OwnerTeamID: "team-1", OwnerTeamName: owner, Tier: tier, Enabled: true,
		EntityObservedAt: factAnchor, UpdatedAt: factAnchor.Add(-time.Hour),
	}
}

func summary(status model.Health, state model.DataState, faults, impacts int) cache.Summary {
	return cache.Summary{
		Health: model.HealthSummary{
			Status: status, ReasonCodes: []string{"signal_unavailable:golden.latency"},
			EvaluatedAt: model.NewTimestamp(fixedNow),
		},
		Data:          model.DataStatus{State: state, Sources: []model.SourceStatus{}},
		GoldenSignals: []model.SignalValue{},
		DomainCounts:  model.DomainCounts{ActiveFaults: faults, ConfirmedImpacts: impacts},
		ObservedAt:    factAnchor,
	}
}

func newService(t *testing.T, candidates *fakeCandidates, summaries SummaryCache, readers *fakeReaders, profiles fakeProfiles) *Service {
	t.Helper()
	assembler := &soctx.Assembler{
		Registrations: readers,
		Topology:      readers,
		Domain:        readers,
		Observations:  serviceobs.NewUnavailableObservationReader(),
		Profiles:      profiles,
		Clock:         fixedClock,
		NewRequestID:  staticReqID,
	}
	service, err := New(Options{
		Assembler: assembler, Candidates: candidates, Profiles: profiles,
		Summaries: summaries, Clock: fixedClock, NewRequestID: staticReqID,
		Limits: Limits{MaxWindowSeconds: 86400},
	})
	if err != nil {
		t.Fatalf("构造 facade 失败: %v", err)
	}
	return service
}

func newValidator(t *testing.T) *contract.Validator {
	t.Helper()
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("编译契约 schema 失败: %v", err)
	}
	return validator
}

// ---- 目录：完整候选摘要 + 单次缓存读取 ----

// 摘要分母必须覆盖 metadata 过滤后的完整候选集合，即使一页只显示一小部分；并且每个
// 候选的缓存条目在一次请求内恰好读一次——摘要与结果筛选共用同一份 request-local 快照。
func TestDirectorySummaryCoversCompleteCandidateSetWithOneLookupPerCandidate(t *testing.T) {
	candidates := &fakeCandidates{items: []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier0),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier1),
		identity("svc-c", "Charlie", "Data SRE", model.ServiceTier1),
		identity("svc-d", "Delta", "Data SRE", model.ServiceTier2),
		identity("svc-e", "Echo", "App Team", model.ServiceTier2),
	}}
	summaries := newFakeSummaries()
	summaries.entries["svc-a"] = summary(model.HealthCritical, model.DataStatePartial, 2, 1)
	summaries.entries["svc-b"] = summary(model.HealthHealthy, model.DataStateComplete, 0, 0)
	summaries.entries["svc-c"] = summary(model.HealthCritical, model.DataStateComplete, 1, 0)
	summaries.entries["svc-d"] = summary(model.HealthUnknown, model.DataStateUnconfigured, 0, 0)
	// svc-e 故意不在缓存里：cache miss 必须返回占位，而不是让整页失败。
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{}}
	service := newService(t, candidates, summaries, readers, fakeProfiles{})

	directory, err := service.Directory(context.Background(), DirectoryQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Directory 失败: %v", err)
	}

	if len(directory.Items) != 2 {
		t.Fatalf("一页应只返回 2 行，实际 %d", len(directory.Items))
	}
	// 分母是完整候选集合，不是当前分页。
	if directory.Summary.Managed != 5 {
		t.Fatalf("摘要 managed=%d, want 5（完整候选集合）", directory.Summary.Managed)
	}
	if directory.Summary.Critical != 2 {
		t.Fatalf("摘要 critical=%d, want 2", directory.Summary.Critical)
	}
	if directory.Summary.Partial != 1 {
		t.Fatalf("摘要 partial=%d, want 1", directory.Summary.Partial)
	}
	// svc-d 是 unconfigured 缓存条目，svc-e 是 cache miss 且没有 Profile -> 占位也是
	// unconfigured。两者都必须进分母。
	if directory.Summary.Unconfigured != 2 {
		t.Fatalf("摘要 unconfigured=%d, want 2", directory.Summary.Unconfigured)
	}
	if summaries.total != 5 {
		t.Fatalf("缓存读取总次数=%d, want 5（每个候选恰好一次）", summaries.total)
	}
	for serviceID, count := range summaries.lookups {
		if count != 1 {
			t.Fatalf("%s 被读了 %d 次缓存，必须恰好一次", serviceID, count)
		}
	}
	// health/data_status 绝不下推到候选查询，否则摘要分母会随筛选漂移。
	if candidates.request.Filter.Environment != "" || candidates.request.Filter.Tier != "" {
		t.Fatalf("未传的 metadata 过滤不应被伪造: %+v", candidates.request.Filter)
	}
	if err := newValidator(t).ValidateValue(contract.KindPageView, directory); err != nil {
		t.Fatalf("目录响应必须过 Page View schema: %v", err)
	}
}

// 结果筛选只筛当前页，不改摘要分母，也不触发第二轮缓存读取。
func TestDirectoryResultFiltersDoNotChangeSummaryDenominatorOrRereadCache(t *testing.T) {
	items := []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier0),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier1),
		identity("svc-c", "Charlie", "Data SRE", model.ServiceTier1),
	}
	build := func() (*fakeCandidates, *fakeSummaries) {
		summaries := newFakeSummaries()
		summaries.entries["svc-a"] = summary(model.HealthCritical, model.DataStatePartial, 2, 1)
		summaries.entries["svc-b"] = summary(model.HealthHealthy, model.DataStateComplete, 0, 0)
		summaries.entries["svc-c"] = summary(model.HealthUnknown, model.DataStatePartial, 0, 0)
		return &fakeCandidates{items: items}, summaries
	}

	candidates, summaries := build()
	service := newService(t, candidates, summaries, &fakeReaders{}, fakeProfiles{})
	unfiltered, err := service.Directory(context.Background(), DirectoryQuery{})
	if err != nil {
		t.Fatalf("Directory 失败: %v", err)
	}

	filteredCandidates, filteredSummaries := build()
	filteredService := newService(t, filteredCandidates, filteredSummaries, &fakeReaders{}, fakeProfiles{})
	filtered, err := filteredService.Directory(context.Background(), DirectoryQuery{
		Health:     []model.Health{model.HealthCritical},
		DataStatus: []model.DataState{model.DataStatePartial},
	})
	if err != nil {
		t.Fatalf("带筛选的 Directory 失败: %v", err)
	}

	if len(filtered.Items) != 1 || filtered.Items[0].Service.ServiceID != "svc-a" {
		t.Fatalf("结果筛选未生效: %+v", filtered.Items)
	}
	if filtered.Summary != unfiltered.Summary {
		t.Fatalf("摘要分母被结果筛选改写: filtered=%+v unfiltered=%+v",
			filtered.Summary, unfiltered.Summary)
	}
	if filteredSummaries.total != 3 {
		t.Fatalf("带筛选时缓存读取=%d, want 3（仍然每个候选恰好一次）", filteredSummaries.total)
	}
}

// 过期快照必须显式 Stale 并保留真实 observed_at；未配置 Profile 优先于新鲜度。
func TestDirectoryStaleSnapshotIsExplicitAndUnconfiguredWins(t *testing.T) {
	candidates := &fakeCandidates{items: []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier0),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier1),
	}}
	summaries := newFakeSummaries()
	summaries.entries["svc-a"] = summary(model.HealthCritical, model.DataStateComplete, 1, 1)
	summaries.stale["svc-a"] = true
	summaries.entries["svc-b"] = summary(model.HealthUnknown, model.DataStateUnconfigured, 0, 0)
	summaries.stale["svc-b"] = true
	service := newService(t, candidates, summaries, &fakeReaders{}, fakeProfiles{})

	directory, err := service.Directory(context.Background(), DirectoryQuery{})
	if err != nil {
		t.Fatalf("Directory 失败: %v", err)
	}
	byID := map[string]model.DirectoryItem{}
	for _, item := range directory.Items {
		byID[item.Service.ServiceID] = item
	}
	if byID["svc-a"].DataStatus.State != model.DataStateStale {
		t.Fatalf("过期快照必须显式 Stale，实际 %q", byID["svc-a"].DataStatus.State)
	}
	if byID["svc-b"].DataStatus.State != model.DataStateUnconfigured {
		t.Fatalf("未配置 Profile 优先于新鲜度，实际 %q", byID["svc-b"].DataStatus.State)
	}
	if byID["svc-a"].ObservedAt != model.NewTimestamp(factAnchor) {
		t.Fatalf("observed_at 必须是真实事实时间，实际 %q", byID["svc-a"].ObservedAt)
	}
}

// cache miss 只返回 Unknown 占位并入队，绝不在目录请求内逐行扇出所有 source。
func TestDirectoryCacheMissReturnsPlaceholderWithoutFanout(t *testing.T) {
	candidates := &fakeCandidates{items: []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier0),
	}}
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{}}
	published := publishedProfileFixture("svc-a")
	service := newService(t, candidates, newFakeSummaries(), readers,
		fakeProfiles{published: map[string]model.Profile{"svc-a": published}})

	directory, err := service.Directory(context.Background(), DirectoryQuery{})
	if err != nil {
		t.Fatalf("Directory 失败: %v", err)
	}
	item := directory.Items[0]
	if item.Health.Status != model.HealthUnknown {
		t.Fatalf("cache miss 必须是 Unknown 占位，实际 %q", item.Health.Status)
	}
	if item.ObservedAt != model.NewTimestamp(factAnchor) {
		t.Fatalf("占位 observed_at 必须是真实事实时间而不是当前时间，实际 %q", item.ObservedAt)
	}
	if item.Profile == nil || item.Profile.ProfileVersion != published.ProfileVersion {
		t.Fatalf("已发布 Profile 必须出现在目录行: %+v", item.Profile)
	}
	// 目录路径不得走 assembler：那就是逐行扇出。
	if len(readers.facts) != 0 {
		t.Fatal("目录不得在请求内读取单服务事实")
	}
}

// ---- 目录：游标 ----

// 分页必须走完全部候选，且不重复、不漏行；next_cursor 只在真的还有下一页时出现。
func TestDirectoryCursorPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	items := make([]serviceobs.ServiceIdentity, 0, 7)
	summaries := newFakeSummaries()
	for _, name := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf"} {
		serviceID := "svc-" + strings.ToLower(name)
		items = append(items, identity(serviceID, name, "Platform SRE", model.ServiceTier1))
		summaries.entries[serviceID] = summary(model.HealthCritical, model.DataStatePartial, 1, 1)
	}
	service := newService(t, &fakeCandidates{items: items}, summaries, &fakeReaders{}, fakeProfiles{})

	seen := []string{}
	cursor := ""
	for page := 0; page < 10; page++ {
		directory, err := service.Directory(context.Background(), DirectoryQuery{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", page+1, err)
		}
		for _, item := range directory.Items {
			seen = append(seen, item.Service.ServiceID)
		}
		if directory.Summary.Managed != 7 {
			t.Fatalf("每页摘要分母都必须是完整候选集合，实际 %d", directory.Summary.Managed)
		}
		if directory.NextCursor == nil {
			break
		}
		if len(*directory.NextCursor) > MaxCursorLength {
			t.Fatalf("游标长度 %d 超过上限 %d", len(*directory.NextCursor), MaxCursorLength)
		}
		cursor = *directory.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("分页共返回 %d 行，want 7: %v", len(seen), seen)
	}
	unique := map[string]struct{}{}
	for _, id := range seen {
		if _, duplicate := unique[id]; duplicate {
			t.Fatalf("分页重复返回 %s: %v", id, seen)
		}
		unique[id] = struct{}{}
	}
}

// 游标行在两页之间被删除时，必须按同一个比较函数的插入位置继续，而不是失效或从头开始。
func TestDirectoryCursorSurvivesDeletedAnchorRow(t *testing.T) {
	items := []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier1),
		identity("svc-c", "Charlie", "Platform SRE", model.ServiceTier1),
		identity("svc-d", "Delta", "Platform SRE", model.ServiceTier1),
	}
	summaries := newFakeSummaries()
	for _, item := range items {
		summaries.entries[item.ServiceID] = summary(model.HealthCritical, model.DataStatePartial, 1, 1)
	}
	candidates := &fakeCandidates{items: items}
	service := newService(t, candidates, summaries, &fakeReaders{}, fakeProfiles{})

	first, err := service.Directory(context.Background(), DirectoryQuery{Limit: 2})
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if first.NextCursor == nil {
		t.Fatal("第一页应有 next_cursor")
	}
	if first.Items[1].Service.ServiceID != "svc-b" {
		t.Fatalf("第一页末行应是 svc-b，实际 %s", first.Items[1].Service.ServiceID)
	}

	// 两页之间删掉游标锚点那一行（以及第一页的另一行）。
	candidates.items = []serviceobs.ServiceIdentity{items[2], items[3]}
	second, err := service.Directory(context.Background(), DirectoryQuery{
		Limit: 2, Cursor: *first.NextCursor,
	})
	if err != nil {
		t.Fatalf("锚点行被删除后翻页不应失败: %v", err)
	}
	got := []string{}
	for _, item := range second.Items {
		got = append(got, item.Service.ServiceID)
	}
	if len(got) != 2 || got[0] != "svc-c" || got[1] != "svc-d" {
		t.Fatalf("应从插入位置继续，实际 %v", got)
	}
	if second.NextCursor != nil {
		t.Fatalf("已到最后一页，next_cursor 必须是 null，实际 %q", *second.NextCursor)
	}
}

// 游标绑定版本 + 全部 filter/sort/direction 指纹：任何不匹配都在查询之前 422。
func TestDirectoryCursorRejectsMismatchedQueryAndMalformedInput(t *testing.T) {
	items := []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier1),
		identity("svc-c", "Charlie", "Data SRE", model.ServiceTier2),
	}
	summaries := newFakeSummaries()
	for _, item := range items {
		summaries.entries[item.ServiceID] = summary(model.HealthCritical, model.DataStatePartial, 1, 1)
	}
	candidates := &fakeCandidates{items: items}
	service := newService(t, candidates, summaries, &fakeReaders{}, fakeProfiles{})

	base := DirectoryQuery{Limit: 1, Environment: "production"}
	first, err := service.Directory(context.Background(), base)
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	cursor := *first.NextCursor
	callsBefore := candidates.calls

	mismatches := []struct {
		name  string
		query DirectoryQuery
	}{
		{"环境变了", DirectoryQuery{Limit: 1, Environment: "staging", Cursor: cursor}},
		{"团队变了", DirectoryQuery{Limit: 1, Environment: "production", TeamID: "team-9", Cursor: cursor}},
		{"tier 变了", DirectoryQuery{Limit: 1, Environment: "production", Tier: "tier-1", Cursor: cursor}},
		{"q 变了", DirectoryQuery{Limit: 1, Environment: "production", Query: "alpha", Cursor: cursor}},
		{"health 筛选变了", DirectoryQuery{Limit: 1, Environment: "production",
			Health: []model.Health{model.HealthCritical}, Cursor: cursor}},
		{"data 筛选变了", DirectoryQuery{Limit: 1, Environment: "production",
			DataStatus: []model.DataState{model.DataStatePartial}, Cursor: cursor}},
		{"排序字段变了", DirectoryQuery{Limit: 1, Environment: "production",
			Sort: view.DirectorySort{Key: view.SortKeyService}, Cursor: cursor}},
		{"排序方向变了", DirectoryQuery{Limit: 1, Environment: "production",
			Sort: view.DirectorySort{Descending: true}, Cursor: cursor}},
		{"编码非法", DirectoryQuery{Limit: 1, Environment: "production", Cursor: "not*base64"}},
		{"超长", DirectoryQuery{Limit: 1, Environment: "production",
			Cursor: strings.Repeat("a", MaxCursorLength+1)}},
	}
	for _, tc := range mismatches {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Directory(context.Background(), tc.query); !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("应返回 ErrInvalidCursor，实际 %v", err)
			}
		})
	}
	// 全部拒绝都必须发生在候选查询之前。
	if candidates.calls != callsBefore {
		t.Fatalf("游标校验必须在任何查询之前完成，实际额外发起了 %d 次扫描",
			candidates.calls-callsBefore)
	}

	// 只换页大小是合法的：limit 不参与指纹。
	if _, err := service.Directory(context.Background(), DirectoryQuery{
		Limit: 2, Environment: "production", Cursor: cursor,
	}); err != nil {
		t.Fatalf("只改 limit 不应让游标失效: %v", err)
	}
}

// 显式排序必须同时决定分页顺序与展示顺序，二者不能错位。
func TestDirectoryExplicitSortDrivesPaginationOrder(t *testing.T) {
	items := []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier2),
		identity("svc-b", "Bravo", "Platform SRE", model.ServiceTier0),
		identity("svc-c", "Charlie", "Data SRE", model.ServiceTier1),
	}
	summaries := newFakeSummaries()
	summaries.entries["svc-a"] = summary(model.HealthCritical, model.DataStatePartial, 3, 3)
	summaries.entries["svc-b"] = summary(model.HealthHealthy, model.DataStateComplete, 0, 0)
	summaries.entries["svc-c"] = summary(model.HealthDegraded, model.DataStatePartial, 1, 1)
	service := newService(t, &fakeCandidates{items: items}, summaries, &fakeReaders{}, fakeProfiles{})

	spec := view.DirectorySort{Key: view.SortKeyTier}
	page, err := service.Directory(context.Background(), DirectoryQuery{Limit: 2, Sort: spec})
	if err != nil {
		t.Fatalf("Directory 失败: %v", err)
	}
	if page.Items[0].Service.ServiceID != "svc-b" || page.Items[1].Service.ServiceID != "svc-c" {
		t.Fatalf("显式排序未驱动首页顺序: %+v", page.Items)
	}
	next, err := service.Directory(context.Background(), DirectoryQuery{
		Limit: 2, Sort: spec, Cursor: *page.NextCursor,
	})
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].Service.ServiceID != "svc-a" {
		t.Fatalf("显式排序下的游标位置错位: %+v", next.Items)
	}
}

// 候选超界必须显式失败，且归入"基数超限"语义（可由收窄筛选解决），不是基础设施故障。
func TestDirectoryPropagatesCandidateOverflowAsScopeError(t *testing.T) {
	candidates := &fakeCandidates{err: serviceobs.ErrCandidateSetTooLarge}
	service := newService(t, candidates, newFakeSummaries(), &fakeReaders{}, fakeProfiles{})

	_, err := service.Directory(context.Background(), DirectoryQuery{})
	if !errors.Is(err, serviceobs.ErrCandidateSetTooLarge) {
		t.Fatalf("候选超界必须显式上抛，实际 %v", err)
	}
	if !errors.Is(err, serviceobs.ErrScopeTooBroad) {
		t.Fatalf("候选超界应归入 scope too broad 语义，实际 %v", err)
	}
}

// 请求取消必须原样传导到端口，不被吞成空目录。
func TestDirectoryPropagatesCancellation(t *testing.T) {
	candidates := &fakeCandidates{items: []serviceobs.ServiceIdentity{
		identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1),
	}}
	service := newService(t, candidates, newFakeSummaries(), &fakeReaders{}, fakeProfiles{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Directory(ctx, DirectoryQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消必须原样上抛，实际 %v", err)
	}
}

// ---- 总览与工作区 ----

// 没有 Profile 时总览仍然可用：identity/责任/领域状态保留，profile 为 null、Data
// Unconfigured。
func TestOverviewWithoutProfileStaysUsable(t *testing.T) {
	readers := &fakeReaders{
		facts: map[string]serviceobs.ServiceFacts{
			"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
		},
		observed: factAnchor,
	}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers, fakeProfiles{})

	overview, err := service.Overview(context.Background(), OverviewQuery{ServiceID: "svc-a"})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	if overview.Profile != nil {
		t.Fatalf("未配置 Profile 时 profile 必须是 null，实际 %+v", overview.Profile)
	}
	if overview.DataStatus.State != model.DataStateUnconfigured {
		t.Fatalf("未配置 Profile 时 Data 必须是 Unconfigured，实际 %q", overview.DataStatus.State)
	}
	if overview.Service.Name != "Alpha" || overview.Service.OwnerTeamName != "Platform SRE" {
		t.Fatalf("identity/责任必须保留: %+v", overview.Service)
	}
	if err := newValidator(t).ValidateValue(contract.KindPageView, overview); err != nil {
		t.Fatalf("总览必须过 Page View schema: %v", err)
	}
}

// 显式环境不匹配统一按服务不存在处理，不泄露其它环境是否存在。
func TestOverviewEnvironmentMismatchIsNotFound(t *testing.T) {
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{
		"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
	}}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers, fakeProfiles{})

	_, err := service.Overview(context.Background(), OverviewQuery{
		ServiceID: "svc-a", Environment: "staging",
	})
	if !errors.Is(err, serviceobs.ErrServiceNotFound) {
		t.Fatalf("环境不匹配应按服务不存在处理，实际 %v", err)
	}
	if _, err := service.Overview(context.Background(), OverviewQuery{ServiceID: "svc-missing"}); !errors.Is(err, serviceobs.ErrServiceNotFound) {
		t.Fatalf("不存在的服务应返回 ErrServiceNotFound，实际 %v", err)
	}
}

// 单个 section 失败只降级那个 section，总览仍然返回可用视图。
func TestOverviewSectionFailureStaysPartial(t *testing.T) {
	readers := &fakeReaders{
		facts: map[string]serviceobs.ServiceFacts{
			"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
		},
		topologyErr: errors.New("topology 查询失败"),
	}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers, fakeProfiles{})

	overview, err := service.Overview(context.Background(), OverviewQuery{ServiceID: "svc-a"})
	if err != nil {
		t.Fatalf("section 失败不得让整页失败: %v", err)
	}
	var topology model.SectionStatus
	for _, section := range overview.Sections {
		if section.SectionKey == serviceobs.SectionKeyTopology {
			topology = section
		}
	}
	if topology.State != model.AvailabilityUnavailable {
		t.Fatalf("topology section 应降级为 unavailable，实际 %q", topology.State)
	}
	// 领域 section 不受 topology 失败影响。
	for _, section := range overview.Sections {
		if section.SectionKey == serviceobs.SectionKeyDomain &&
			section.State == model.AvailabilityUnavailable {
			t.Fatal("topology 失败不得拖垮 domain section")
		}
	}
}

// 工作区在未配置 Profile 时必须是 typed ErrProfileUnconfigured（API 层 409）。
func TestWorkspaceWithoutProfileIsUnconfigured(t *testing.T) {
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{
		"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
	}}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers, fakeProfiles{})

	_, err := service.Workspace(context.Background(), WorkspaceQuery{ServiceID: "svc-a"})
	if !errors.Is(err, serviceobs.ErrProfileUnconfigured) {
		t.Fatalf("未配置 Profile 的工作区必须 typed 失败，实际 %v", err)
	}
}

// 已发布 Profile 下工作区可用；未接入遥测只让信号 Unknown，不假绿。
func TestWorkspaceWithProfileKeepsFactsAndUnknownSignals(t *testing.T) {
	published := publishedProfileFixture("svc-a")
	readers := &fakeReaders{
		facts: map[string]serviceobs.ServiceFacts{
			"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
		},
		observed: factAnchor,
	}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers,
		fakeProfiles{published: map[string]model.Profile{"svc-a": published}})

	workspace, err := service.Workspace(context.Background(), WorkspaceQuery{
		ServiceID: "svc-a",
		Focus:     &model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"},
	})
	if err != nil {
		t.Fatalf("Workspace 失败: %v", err)
	}
	if workspace.Health.Status != model.HealthUnknown {
		t.Fatalf("未接入遥测时 Health 必须 Unknown，实际 %q", workspace.Health.Status)
	}
	if workspace.DataStatus.State != model.DataStatePartial {
		t.Fatalf("未接入遥测时 Data 必须 Partial，实际 %q", workspace.DataStatus.State)
	}
	for _, signal := range workspace.GoldenSignals {
		if signal.Value != nil {
			t.Fatalf("未观测到的信号不得有数值: %+v", signal)
		}
		if signal.UnknownReason != serviceobs.ReasonTelemetryNotConnected {
			t.Fatalf("unknown_reason=%q, want %q", signal.UnknownReason,
				serviceobs.ReasonTelemetryNotConnected)
		}
	}
	if workspace.Capabilities.ViewMetrics || workspace.Capabilities.ViewLogs {
		t.Fatal("Phase 2 没有接入 metrics/logs adapter，capabilities 必须为 false")
	}
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("工作区必须过 Page View schema: %v", err)
	}
}

// focus 必须由已发布 Profile 声明且满足 value_pattern；越界窗口显式 422 语义。
func TestWorkspaceRejectsInvalidFocusAndOversizedWindow(t *testing.T) {
	published := publishedProfileFixture("svc-a")
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{
		"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
	}}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers,
		fakeProfiles{published: map[string]model.Profile{"svc-a": published}})

	if _, err := service.Workspace(context.Background(), WorkspaceQuery{
		ServiceID: "svc-a", Focus: &model.Focus{Kind: "not_declared", Value: "x"},
	}); !errors.Is(err, serviceobs.ErrInvalidFocus) {
		t.Fatalf("未声明的 focus kind 必须 ErrInvalidFocus，实际 %v", err)
	}
	if _, err := service.Workspace(context.Background(), WorkspaceQuery{
		ServiceID: "svc-a", Focus: &model.Focus{Kind: "sandbox_id", Value: "不满足 pattern"},
	}); !errors.Is(err, serviceobs.ErrInvalidFocus) {
		t.Fatalf("value_pattern 不匹配必须 ErrInvalidFocus，实际 %v", err)
	}
	if _, err := service.Workspace(context.Background(), WorkspaceQuery{
		ServiceID: "svc-a",
		From:      fixedNow.Add(-48 * time.Hour), To: fixedNow,
	}); !errors.Is(err, serviceobs.ErrScopeTooBroad) {
		t.Fatalf("超出 Profile 窗口上界必须 ErrScopeTooBroad，实际 %v", err)
	}
	if _, err := service.Workspace(context.Background(), WorkspaceQuery{
		ServiceID: "svc-a", From: fixedNow.Add(-time.Minute),
	}); !errors.Is(err, soctx.ErrInvalidTimeRange) {
		t.Fatalf("只传 from 必须 ErrInvalidTimeRange，实际 %v", err)
	}
}

// 两个时间都缺省时用服务端单次 now 与 Profile 默认窗口生成 [now-window, now]。
func TestWorkspaceDefaultWindowUsesProfileDefault(t *testing.T) {
	published := publishedProfileFixture("svc-a")
	readers := &fakeReaders{facts: map[string]serviceobs.ServiceFacts{
		"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
	}}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers,
		fakeProfiles{published: map[string]model.Profile{"svc-a": published}})

	workspace, err := service.Workspace(context.Background(), WorkspaceQuery{ServiceID: "svc-a"})
	if err != nil {
		t.Fatalf("Workspace 失败: %v", err)
	}
	from, err := workspace.Scope.From.Time()
	if err != nil {
		t.Fatalf("解析 from 失败: %v", err)
	}
	to, err := workspace.Scope.To.Time()
	if err != nil {
		t.Fatalf("解析 to 失败: %v", err)
	}
	if !to.Equal(fixedNow) {
		t.Fatalf("to 应是服务端单次 now，实际 %v", to)
	}
	if got := int(to.Sub(from).Seconds()); got != published.Limits.DefaultWindowSeconds {
		t.Fatalf("默认窗口=%ds, want %ds", got, published.Limits.DefaultWindowSeconds)
	}
}

// 刷新函数与总览走同一条装配 + evaluator 路径，摘要只保留显示事实与真实 observed_at。
func TestRefreshSummaryUsesSameEvaluatorAndRealObservedAt(t *testing.T) {
	published := publishedProfileFixture("svc-a")
	readers := &fakeReaders{
		facts: map[string]serviceobs.ServiceFacts{
			"svc-a": {Identity: identity("svc-a", "Alpha", "Platform SRE", model.ServiceTier1)},
		},
		observed: factAnchor,
	}
	service := newService(t, &fakeCandidates{}, newFakeSummaries(), readers,
		fakeProfiles{published: map[string]model.Profile{"svc-a": published}})

	refreshed, err := service.RefreshSummary(context.Background(), cache.Key{
		ServiceID: "svc-a", Environment: "production",
		ProfileVersion: published.ProfileVersion, DefaultWindowSeconds: 300,
	})
	if err != nil {
		t.Fatalf("RefreshSummary 失败: %v", err)
	}
	overview, err := service.Overview(context.Background(), OverviewQuery{
		ServiceID: "svc-a", WindowSeconds: 300,
	})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	if refreshed.Health.Status != overview.Health.Status {
		t.Fatalf("目录摘要与总览必须共用同一个 evaluator: %q vs %q",
			refreshed.Health.Status, overview.Health.Status)
	}
	if refreshed.Data.State != overview.DataStatus.State {
		t.Fatalf("Data 判定漂移: %q vs %q", refreshed.Data.State, overview.DataStatus.State)
	}
	if len(refreshed.GoldenSignals) != len(overview.GoldenSignals) {
		t.Fatalf("Golden 卡数量漂移: %d vs %d",
			len(refreshed.GoldenSignals), len(overview.GoldenSignals))
	}
	if !refreshed.ObservedAt.Equal(factAnchor) {
		t.Fatalf("摘要 observed_at 必须是真实事实时间，实际 %v", refreshed.ObservedAt)
	}
	if refreshed.ObservedAt.Equal(fixedNow) {
		t.Fatal("摘要 observed_at 绝不能是刷新时刻")
	}
}

// 装配缺少缓存时目录必须显式失败，绝不退化成逐行扇出。
func TestDirectoryRequiresSummaryCache(t *testing.T) {
	service := newService(t, &fakeCandidates{}, nil, &fakeReaders{}, fakeProfiles{})
	service.SetSummaryCache(nil)
	if _, err := service.Directory(context.Background(), DirectoryQuery{}); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("缺少缓存应显式失败，实际 %v", err)
	}
}

// publishedProfileFixture 构造一个最小但合法的 published Profile。
//
// 它不读 docs/contracts 的 example：facade 的 live 路径与它的测试都不依赖 fixture 文件。
func publishedProfileFixture(serviceID string) model.Profile {
	threshold := func(degraded, critical float64) *model.SignalThresholds {
		return &model.SignalThresholds{
			Mode: model.ThresholdModeAbsolute, Direction: model.ThresholdDirectionAbove,
			Degraded: degraded, Critical: critical,
		}
	}
	published := model.Profile{
		SchemaVersion:  model.SchemaVersion,
		ProfileKey:     "example.platform",
		ProfileVersion: 1,
		Status:         model.ProfileStatusPublished,
		ServiceRef: model.ProfileServiceRef{
			ServiceID: serviceID, RootEntityIDs: []string{"entity-" + serviceID},
		},
		FocusKinds: []model.FocusKind{{
			Key: "sandbox_id", Label: "Sandbox ID", ValuePattern: `^sbx_[a-z0-9]{6,32}$`,
			Placeholder: "sbx_xxxxxx", Primary: true,
		}},
		Journeys: []model.Journey{{
			Key: "create", Label: "创建", FocusKind: "sandbox_id",
			Stages: []model.JourneyStageDef{{Key: "place", Label: "调度"}},
		}},
		Views: []model.ProfileView{{
			Key: "golden", Renderer: model.RendererGoldenOverview, Title: "黄金信号", Order: 1,
			SignalRefs: []string{
				"golden.latency", "golden.traffic", "golden.errors", "golden.saturation",
			},
		}},
		SignalBindings: []model.SignalBinding{
			{
				Key: "golden.latency", Label: "创建 P95", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticLatency,
				Scope:    model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey:  "metrics.range_query.v1", OutputField: "value", Unit: "ms",
				Aggregation: model.AggregationP95, Thresholds: threshold(3000, 8000),
			},
			{
				Key: "golden.traffic", Label: "创建速率", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticTraffic,
				Scope:    model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey:  "metrics.range_query.v1", OutputField: "value", Unit: "rps",
				Aggregation: model.AggregationRate,
			},
			{
				Key: "golden.errors", Label: "创建错误率", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticErrors,
				Scope:    model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey:  "metrics.range_query.v1", OutputField: "value", Unit: "percent",
				Aggregation: model.AggregationAvg, Thresholds: threshold(5, 15),
			},
			{
				Key: "golden.saturation", Label: "容量压力", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticSaturation,
				Scope:    model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey:  "metrics.range_query.v1", OutputField: "value", Unit: "percent",
				Aggregation: model.AggregationMax, MeasurementRole: model.MeasurementRoleProxy,
				ProxyReason: "直接 admission 队列指标尚未接入，先用容量压力代理",
				Thresholds:  threshold(85, 95),
			},
		},
		ToolRefs: []string{"metrics.range_query.v1"},
		Limits: model.ProfileLimits{
			DefaultWindowSeconds: 300, MaxWindowSeconds: 3600, MaxNodes: 100,
			MaxRelations: 200, MaxRows: 200, MaxEvidenceRefs: 100, FreshnessTTLSeconds: 60,
		},
	}
	published.Normalize()
	return published
}

// 编译期保证 facade 的缓存依赖就是 cache.Coordinator 的读接口形状。
var _ SummaryCache = (*cache.Coordinator)(nil)

// 编译期保证 fake readers 覆盖 assembler 需要的三个端口。
var (
	_ serviceobs.RegistrationReader = (*fakeReaders)(nil)
	_ serviceobs.TopologyReader     = (*fakeReaders)(nil)
	_ serviceobs.DomainReader       = (*fakeReaders)(nil)
	_ serviceobs.CandidateScanner   = (*fakeCandidates)(nil)
)
