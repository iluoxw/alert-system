package context

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
)

var testNow = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)

const (
	testServiceID = "11111111-1111-4111-8111-111111111111"
	testEntityID  = "22222222-2222-4222-8222-222222222222"
	testRootID    = "33333333-3333-4333-8333-333333333333"
	testPeerID    = "44444444-4444-4444-8444-444444444444"
)

// 真实事实时间锚：它比 testNow 早，测试据此证明"从未观测过"的 signal 不会用 wall clock。
var testFactAnchor = testNow.Add(-17 * time.Minute)

type fakeRegistrations struct {
	facts    serviceobs.ServiceFacts
	err      error
	deadline time.Duration
}

func (f *fakeRegistrations) ListServices(stdcontext.Context, serviceobs.RegistrationFilter) (serviceobs.RegistrationList, error) {
	return serviceobs.RegistrationList{Items: []serviceobs.ServiceIdentity{}}, nil
}

func (f *fakeRegistrations) ReadService(ctx stdcontext.Context, _ string) (serviceobs.ServiceFacts, error) {
	f.deadline = remaining(ctx)
	if f.err != nil {
		return serviceobs.ServiceFacts{}, f.err
	}
	return f.facts, nil
}

type fakeTopology struct {
	facts    serviceobs.TopologyFacts
	err      error
	request  serviceobs.TopologyRequest
	deadline time.Duration
}

func (f *fakeTopology) ReadTopology(ctx stdcontext.Context, req serviceobs.TopologyRequest) (serviceobs.TopologyFacts, error) {
	f.deadline = remaining(ctx)
	f.request = req
	if f.err != nil {
		return serviceobs.TopologyFacts{}, f.err
	}
	return f.facts, nil
}

type fakeDomain struct {
	facts    serviceobs.DomainFacts
	err      error
	request  serviceobs.DomainRequest
	deadline time.Duration
}

func (f *fakeDomain) ReadDomain(ctx stdcontext.Context, req serviceobs.DomainRequest) (serviceobs.DomainFacts, error) {
	f.deadline = remaining(ctx)
	f.request = req
	if f.err != nil {
		return serviceobs.DomainFacts{}, f.err
	}
	return f.facts, nil
}

type fakeProfiles struct {
	published  map[string]model.Profile
	diagnostic map[string]profile.Diagnostic
}

func (f *fakeProfiles) PublishedForService(serviceID string) (model.Profile, bool) {
	published, ok := f.published[serviceID]
	return published, ok
}

func (f *fakeProfiles) DiagnosticForService(serviceID string) (profile.Diagnostic, bool) {
	diagnostic, ok := f.diagnostic[serviceID]
	return diagnostic, ok
}

func remaining(ctx stdcontext.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline)
}

func testIdentity() serviceobs.ServiceIdentity {
	return serviceobs.ServiceIdentity{
		ServiceID: testServiceID, EntityID: testEntityID, EntityKey: "platform/example",
		Name: "Example", Description: "示例平台能力", Layer: model.EntityLayerPlatform,
		Kind: model.EntityKindInfrastructureCapability, Environment: "production",
		OwnerTeamID: "team-1", OwnerTeamName: "Platform SRE", Tier: model.ServiceTier1,
		EntityObservedAt: testFactAnchor, UpdatedAt: testFactAnchor.Add(-time.Hour),
	}
}

func testProfile() model.Profile {
	degraded, critical := 85.0, 95.0
	return model.Profile{
		SchemaVersion: model.SchemaVersion, ProfileKey: "example", ProfileVersion: 1,
		Status: model.ProfileStatusPublished,
		ServiceRef: model.ProfileServiceRef{
			ServiceID: testServiceID, RootEntityIDs: []string{testRootID},
		},
		FocusKinds: []model.FocusKind{{
			Key: "instance_id", Label: "实例 ID", ValuePattern: `^ins_[a-z0-9]+$`,
			Placeholder: "ins_abc", Primary: true,
		}},
		Journeys: []model.Journey{{
			Key: "request", Label: "请求", FocusKind: "instance_id",
			Stages: []model.JourneyStageDef{{Key: "accept", Label: "ACCEPT", EntityRefs: []string{testEntityID}}},
		}},
		SignalBindings: []model.SignalBinding{
			{
				Key: "golden_latency", Label: "延迟 P95", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticLatency, Scope: model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey: "metrics.query_semantic_signal.v1", OutputField: "series.p95_ms",
				Unit: "ms", Aggregation: model.AggregationP95,
			},
			{
				Key: "golden_traffic", Label: "流量", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticTraffic, Scope: model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey: "metrics.query_semantic_signal.v1", OutputField: "series.rpm",
				Unit: "req/min", Aggregation: model.AggregationRate,
			},
			{
				Key: "golden_errors", Label: "错误率", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticErrors, Scope: model.SignalBindingScope{Kind: model.ScopeKindService},
				ToolKey: "metrics.query_semantic_signal.v1", OutputField: "series.error_percent",
				Unit: "percent", Aggregation: model.AggregationAvg,
			},
			{
				Key: "golden_saturation", Label: "容量压力", Family: model.SignalFamilyGolden,
				Semantic: model.SemanticSaturation,
				Scope:    model.SignalBindingScope{Kind: model.ScopeKindResource, Refs: []string{testPeerID}},
				ToolKey:  "metrics.query_semantic_signal.v1", OutputField: "series.pressure",
				Unit: "percent", Aggregation: model.AggregationMax,
				MeasurementRole: model.MeasurementRoleProxy,
				ProxyReason:     "使用最坏宿主容量压力近似服务饱和度，接入直接排队指标前不得解释为排队压力。",
				Thresholds: &model.SignalThresholds{
					Mode: model.ThresholdModeAbsolute, Direction: model.ThresholdDirectionAbove,
					Degraded: degraded, Critical: critical,
				},
			},
		},
		ToolRefs: []string{"metrics.query_semantic_signal.v1"},
		Limits: model.ProfileLimits{
			DefaultWindowSeconds: 300, MaxWindowSeconds: 3600, MaxNodes: 50,
			MaxRelations: 100, MaxRows: 100, MaxEvidenceRefs: 50, FreshnessTTLSeconds: 60,
		},
	}
}

func testTopologyFacts() serviceobs.TopologyFacts {
	return serviceobs.TopologyFacts{
		SnapshotID: "topo_test", ObservedAt: testFactAnchor.Add(2 * time.Minute),
		Nodes: []serviceobs.TopologyNode{
			{EntityID: testEntityID, EntityKey: "platform/example", Name: "Example",
				Layer: model.EntityLayerPlatform, Kind: "infrastructure_capability",
				ObservedAt: testFactAnchor},
			{EntityID: testPeerID, EntityKey: "infrastructure/example/hosts", Name: "Host Fleet",
				Layer: model.EntityLayerInfrastructure, Kind: "resource", Depth: 1,
				ObservedAt: testFactAnchor},
		},
		Relations: []serviceobs.TopologyRelation{{
			RelationID: "rel-1", SourceEntityID: testEntityID, TargetEntityID: testPeerID,
			Kind: "depends_on", Provenance: model.ProvenanceDeclared, Criticality: "critical",
			ObservedAt: testFactAnchor.Add(2 * time.Minute),
		}},
	}
}

func testDomainFacts() serviceobs.DomainFacts {
	return serviceobs.DomainFacts{
		BoundaryEntityIDs: []string{testEntityID, testPeerID},
		Faults: []serviceobs.DomainFault{{
			FaultID: "fault-1", Title: "容量失守", State: "active",
			Severity: model.FaultSeverityCritical, EntityID: testEntityID,
			FirstDetectedAt: testFactAnchor, UpdatedAt: testFactAnchor.Add(time.Minute),
			ConfirmedImpactCount: 1,
		}},
		Counts:       model.DomainCounts{ActiveFaults: 1, ConfirmedImpacts: 1},
		LatestFactAt: testFactAnchor.Add(time.Minute),
	}
}

func newAssembler(t *testing.T, published *model.Profile) (*Assembler, *fakeRegistrations, *fakeTopology, *fakeDomain) {
	t.Helper()
	registrations := &fakeRegistrations{facts: serviceobs.ServiceFacts{
		Identity: testIdentity(), ReadAt: testNow,
		Dependencies: []serviceobs.DeclaredDependency{{
			RelationID: "rel-1", FromEntityID: testEntityID, ToEntityID: testPeerID,
			ToEntityName: "Host Fleet", ToLayer: model.EntityLayerInfrastructure,
			Relation: "depends_on", Criticality: "critical",
			Provenance: model.ProvenanceDeclared, ObservedAt: testFactAnchor,
		}},
	}}
	topology := &fakeTopology{facts: testTopologyFacts()}
	domain := &fakeDomain{facts: testDomainFacts()}
	profiles := &fakeProfiles{published: map[string]model.Profile{}, diagnostic: map[string]profile.Diagnostic{}}
	if published != nil {
		profiles.published[testServiceID] = *published
	}
	assembler := &Assembler{
		Registrations: registrations,
		Topology:      topology,
		Domain:        domain,
		Observations:  serviceobs.NewUnavailableObservationReader(),
		Profiles:      profiles,
		Clock:         func() time.Time { return testNow },
		NewRequestID:  func() (string, error) { return "req_test_fixed", nil },
	}
	return assembler, registrations, topology, domain
}

func TestAssembleKeepsFactsWhenTelemetryIsNotConnected(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)

	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
		Subject: Subject{UserID: "user-1", Roles: []string{"member"}},
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}

	// declared topology 与 domain 完全保留。
	if len(snapshot.TopologyFacts.Nodes) != 2 || len(snapshot.DomainFacts.Faults) != 1 {
		t.Fatalf("未接遥测不得清空 declared topology 或 domain: %+v", snapshot)
	}
	// Golden/RED/USE 全部 Unknown，Data 落到 Partial。
	if snapshot.Evaluation.Health.Status != model.HealthUnknown {
		t.Fatalf("必需信号全缺时 Health 必须 Unknown，实际 %q", snapshot.Evaluation.Health.Status)
	}
	if snapshot.Evaluation.Data.State != model.DataStatePartial {
		t.Fatalf("遥测不可用应让 Data Partial，实际 %q", snapshot.Evaluation.Data.State)
	}
	for _, signal := range snapshot.Evaluation.Signals {
		if signal.Availability == model.SignalAvailabilityAvailable {
			t.Fatalf("%s 不应有可用观测", signal.Binding.Key)
		}
		if signal.UnknownReason != serviceobs.ReasonTelemetryNotConnected {
			t.Fatalf("%s unknown reason 必须稳定，实际 %q", signal.Binding.Key, signal.UnknownReason)
		}
	}
	// Fault 数量绝不改写 Health。
	if snapshot.DomainFacts.Counts.ActiveFaults != 1 {
		t.Fatal("领域计数应原样保留")
	}
	// 事实时间锚必须是真实持久化时间，不是当前时间。
	if snapshot.FactAnchor.Equal(testNow) || snapshot.FactAnchor.IsZero() {
		t.Fatalf("FactAnchor 必须是真实事实时间，实际 %v", snapshot.FactAnchor)
	}
	// Phase 2 capabilities：只有 view_domain 为 true。
	want := model.Capabilities{ViewDomain: true}
	if snapshot.Capabilities != want {
		t.Fatalf("capabilities 不符: %+v", snapshot.Capabilities)
	}
}

func TestAssembleUsesIndependentDeadlinePerAdapter(t *testing.T) {
	published := testProfile()
	assembler, registrations, topology, domain := newAssembler(t, &published)
	assembler.Deadlines = Deadlines{
		Registration: 2 * time.Second, Topology: 4 * time.Second,
		Domain: 6 * time.Second, Observations: 8 * time.Second,
	}
	if _, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	}); err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	// 每个 adapter 拿到自己的预算：一个慢 adapter 不能吃掉整个请求。
	for name, observed := range map[string]struct{ got, want time.Duration }{
		"registration": {registrations.deadline, 2 * time.Second},
		"topology":     {topology.deadline, 4 * time.Second},
		"domain":       {domain.deadline, 6 * time.Second},
	} {
		if observed.got <= 0 || observed.got > observed.want {
			t.Fatalf("%s 未拿到独立 deadline: got %v want <= %v", name, observed.got, observed.want)
		}
		if observed.got < observed.want/2 {
			t.Fatalf("%s deadline 明显偏小: got %v want ~%v", name, observed.got, observed.want)
		}
	}
}

func TestAssembleRegistrationFailureIsPageLevel(t *testing.T) {
	published := testProfile()
	assembler, registrations, _, _ := newAssembler(t, &published)
	registrations.err = serviceobs.ErrServiceNotFound
	if _, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	}); !errors.Is(err, serviceobs.ErrServiceNotFound) {
		t.Fatalf("registration 是关键依赖，必须返回页面级错误，实际 %v", err)
	}
}

func TestAssembleEnvironmentMismatchLooksLikeNotFound(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	if _, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID, Environment: "staging",
	}); !errors.Is(err, serviceobs.ErrServiceNotFound) {
		t.Fatalf("显式环境不匹配必须统一按不存在处理，实际 %v", err)
	}
}

func TestAssembleTopologyFailureOnlyDegradesTopology(t *testing.T) {
	published := testProfile()
	assembler, _, topology, _ := newAssembler(t, &published)
	topology.err = errors.New("topology unavailable")

	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("非关键 adapter 失败不得返回页面级错误: %v", err)
	}
	section, ok := snapshot.SectionState(serviceobs.SectionKeyTopology)
	if !ok || section.State != model.AvailabilityUnavailable ||
		section.ReasonCode != serviceobs.ReasonTopologyReadFailed {
		t.Fatalf("topology section 状态不符: %+v", section)
	}
	// domain 不受影响。
	if len(snapshot.DomainFacts.Faults) != 1 {
		t.Fatal("topology 失败不得让 domain 变空")
	}
	if domainSection, _ := snapshot.SectionState(serviceobs.SectionKeyDomain); domainSection.State != model.AvailabilityAvailable {
		t.Fatalf("domain section 应保持 available，实际 %+v", domainSection)
	}
	if snapshot.Evaluation.Data.State != model.DataStatePartial {
		t.Fatalf("局部失败应让 Data Partial，实际 %q", snapshot.Evaluation.Data.State)
	}
}

func TestAssembleDomainFailureOnlyDegradesDomain(t *testing.T) {
	published := testProfile()
	assembler, _, _, domain := newAssembler(t, &published)
	domain.err = errors.New("domain unavailable")

	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("domain 失败不得返回页面级错误: %v", err)
	}
	if section, _ := snapshot.SectionState(serviceobs.SectionKeyDomain); section.State != model.AvailabilityUnavailable {
		t.Fatalf("domain section 应 unavailable，实际 %+v", section)
	}
	// identity 与 declared topology 保留。
	if snapshot.Identity.ServiceID != testServiceID || len(snapshot.TopologyFacts.Nodes) != 2 {
		t.Fatal("domain 失败不得影响 identity 与 declared topology")
	}
}

func TestAssembleInvalidFocusIsRequestError(t *testing.T) {
	published := testProfile()
	cases := map[string]*model.Focus{
		"未由 Profile 声明":     {Kind: "customer_email", Value: "a@b.c"},
		"value 不满足 pattern": {Kind: "instance_id", Value: "NOT-MATCHING"},
	}
	for name, focus := range cases {
		t.Run(name, func(t *testing.T) {
			assembler, _, _, _ := newAssembler(t, &published)
			_, err := assembler.Assemble(stdcontext.Background(), Request{
				Kind: KindWorkspace, ServiceID: testServiceID, Focus: focus,
			})
			if !errors.Is(err, serviceobs.ErrInvalidFocus) {
				t.Fatalf("非法 focus 必须 typed 拒绝，实际 %v", err)
			}
		})
	}
}

func TestAssembleFocusWhitelistIsPassedToDomainAdapter(t *testing.T) {
	published := testProfile()
	assembler, _, _, domain := newAssembler(t, &published)
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindWorkspace, ServiceID: testServiceID,
		Focus: &model.Focus{Kind: "instance_id", Value: "ins_abc"},
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	// 白名单必须恰好是 Profile 声明的 focus_kinds key 集合。
	if len(domain.request.AllowedFocusKinds) != 1 || domain.request.AllowedFocusKinds[0] != "instance_id" {
		t.Fatalf("focus 白名单来源应是 Profile.focus_kinds: %v", domain.request.AllowedFocusKinds)
	}
	// journey 只能来自 Profile 声明，不接受客户端自报。
	if snapshot.Focus == nil || snapshot.Focus.Journey != "request" {
		t.Fatalf("focus journey 必须由 Profile 决定: %+v", snapshot.Focus)
	}
}

func TestAssembleWorkspaceRequiresProfile(t *testing.T) {
	assembler, _, _, _ := newAssembler(t, nil)
	if _, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindWorkspace, ServiceID: testServiceID,
	}); !errors.Is(err, serviceobs.ErrProfileUnconfigured) {
		t.Fatalf("工作区在 Profile 缺失时必须 typed 409，实际 %v", err)
	}
}

func TestAssembleOverviewSurvivesMissingProfile(t *testing.T) {
	assembler, _, _, _ := newAssembler(t, nil)
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("Profile 缺失时总览必须仍可用: %v", err)
	}
	if snapshot.Profile != nil {
		t.Fatal("没有 active published Profile 时 Profile 必须为 nil")
	}
	// 契约的 service.root_entity_ids/profile_key/profile_version 都是必填非空，
	// 所以未配置 Profile 时不存在合法的 DiagnosticContext。
	if snapshot.Context != nil {
		t.Fatal("Profile 缺失时不得构造 DiagnosticContext")
	}
	if snapshot.Evaluation.Data.State != model.DataStateUnconfigured {
		t.Fatalf("Profile 缺失应让 Data Unconfigured，实际 %q", snapshot.Evaluation.Data.State)
	}
	// identity 与 domain 仍然保留。
	if snapshot.Identity.Name != "Example" || len(snapshot.DomainFacts.Faults) != 1 {
		t.Fatal("Profile 缺失不得清空 identity 或 domain")
	}
}

func TestAssembleWindowValidation(t *testing.T) {
	published := testProfile()

	t.Run("缺省窗口来自 Profile 且只取一次 now", func(t *testing.T) {
		assembler, _, _, _ := newAssembler(t, &published)
		snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
			Kind: KindOverview, ServiceID: testServiceID,
		})
		if err != nil {
			t.Fatalf("Assemble 失败: %v", err)
		}
		if snapshot.WindowSeconds != published.Limits.DefaultWindowSeconds {
			t.Fatalf("缺省窗口应取 Profile 默认值，实际 %d", snapshot.WindowSeconds)
		}
		if snapshot.Scope.TimeRange.To != model.NewTimestamp(testNow) {
			t.Fatalf("to 应等于服务端单次取到的 now，实际 %q", snapshot.Scope.TimeRange.To)
		}
	})

	t.Run("只传一个必须拒绝", func(t *testing.T) {
		assembler, _, _, _ := newAssembler(t, &published)
		_, err := assembler.Assemble(stdcontext.Background(), Request{
			Kind: KindOverview, ServiceID: testServiceID, From: testNow.Add(-time.Minute),
		})
		if !errors.Is(err, ErrInvalidTimeRange) {
			t.Fatalf("只传 from 必须 typed 拒绝，实际 %v", err)
		}
	})

	t.Run("超过上限必须显式报错而不是静默截断", func(t *testing.T) {
		assembler, _, _, _ := newAssembler(t, &published)
		_, err := assembler.Assemble(stdcontext.Background(), Request{
			Kind: KindOverview, ServiceID: testServiceID,
			From: testNow.Add(-48 * time.Hour), To: testNow,
		})
		if !errors.Is(err, serviceobs.ErrScopeTooBroad) {
			t.Fatalf("超限必须 typed 拒绝，实际 %v", err)
		}
	})

	t.Run("顺序颠倒必须拒绝", func(t *testing.T) {
		assembler, _, _, _ := newAssembler(t, &published)
		_, err := assembler.Assemble(stdcontext.Background(), Request{
			Kind: KindOverview, ServiceID: testServiceID,
			From: testNow, To: testNow.Add(-time.Minute),
		})
		if !errors.Is(err, ErrInvalidTimeRange) {
			t.Fatalf("from 晚于 to 必须拒绝，实际 %v", err)
		}
	})
}

func TestAssembleScopeTenantIsServerConstant(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID, Timezone: "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	if snapshot.Scope.Tenant != serviceobs.TenantDefault {
		t.Fatalf("单租户部署 tenant 必须是服务端常量，实际 %q", snapshot.Scope.Tenant)
	}
	// timezone 只影响显示，不改变任何查询边界。
	if snapshot.Scope.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone 应原样保留，实际 %q", snapshot.Scope.Timezone)
	}
	if snapshot.Scope.Environment != "production" {
		t.Fatalf("environment 应来自 registration，实际 %q", snapshot.Scope.Environment)
	}
}

func TestPermissionSnapshotIDIsServerComputedAndStable(t *testing.T) {
	base := Subject{UserID: "user-1", Roles: []string{"member", "fault_reviewer"}, TeamIDs: []string{"t2", "t1"}}
	capabilities := model.Capabilities{ViewDomain: true}

	first := PermissionSnapshotID(base, capabilities)
	// 角色顺序不同不应改变指纹：输入先被规范化。
	reordered := Subject{UserID: "user-1", Roles: []string{"fault_reviewer", "member"}, TeamIDs: []string{"t1", "t2"}}
	if second := PermissionSnapshotID(reordered, capabilities); first != second {
		t.Fatalf("规范化后同一身份必须得到同一指纹: %q vs %q", first, second)
	}
	// 角色变化必须改变指纹。
	if changed := PermissionSnapshotID(Subject{UserID: "user-1", Roles: []string{"member"}}, capabilities); changed == first {
		t.Fatal("角色变化必须改变权限快照 ID")
	}
	// capabilities 变化必须改变指纹。
	if changed := PermissionSnapshotID(base, model.Capabilities{ViewDomain: true, ViewLogs: true}); changed == first {
		t.Fatal("capabilities 变化必须改变权限快照 ID")
	}
	if len(first) == 0 || len(first) > 128 {
		t.Fatalf("权限快照 ID 长度必须落在契约范围内，实际 %d", len(first))
	}
}

func TestDiagnosticContextMirrorsSchemaWithoutInternalFields(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind:      KindWorkspace,
		Subject:   Subject{UserID: "user-1", Roles: []string{"member"}, TeamIDs: []string{"team-1"}},
		ServiceID: testServiceID,
		Focus:     &model.Focus{Kind: "instance_id", Value: "ins_abc"},
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	if snapshot.Context == nil {
		t.Fatal("有 Profile 时必须构造 DiagnosticContext")
	}

	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	if err := validator.ValidateValue(contract.KindDiagnosticContext, snapshot.Context); err != nil {
		t.Fatalf("DiagnosticContext 未通过 v1 schema: %v", err)
	}

	// additionalProperties=false：装配内部字段一个都不能出现在序列化结果里。
	raw, err := json.Marshal(snapshot.Context)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	for _, forbidden := range []string{"subject", "sources", "sections", "fact_anchor", "profile_diagnostic", "capabilities"} {
		if _, exists := decoded[forbidden]; exists {
			t.Fatalf("DiagnosticContext 不得包含装配内部字段 %q", forbidden)
		}
	}
	// 权限快照必须由服务端计算，且与 subject/capabilities 一致。
	want := PermissionSnapshotID(snapshot.Subject, snapshot.Capabilities)
	if snapshot.Context.Actor.PermissionSnapshotID != want {
		t.Fatal("permission_snapshot_id 必须由服务端指纹生成")
	}
	// assembled_at 与 observed_at 是两件事。
	if snapshot.Context.AssembledAt == snapshot.Context.Topology.ObservedAt {
		t.Fatal("assembled_at 与 topology observed_at 必须分离")
	}
}

func TestDiagnosticContextMissingSignalUsesFactAnchorNotWallClock(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	// 把 wall clock 推到很远的未来：如果 projector/assembler 用 now 填 observed_at，
	// 这个断言会立刻失败。
	future := testNow.Add(365 * 24 * time.Hour)
	assembler.Clock = func() time.Time { return future }

	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	anchor := model.NewTimestamp(snapshot.FactAnchor)
	for _, signal := range snapshot.Context.Signals {
		if signal.Availability == model.SignalAvailabilityAvailable {
			t.Fatalf("%s 不应有可用观测", signal.BindingKey)
		}
		if signal.ObservedAt != anchor {
			t.Fatalf("%s observed_at 应取真实事实时间锚 %q，实际 %q",
				signal.BindingKey, anchor, signal.ObservedAt)
		}
		if signal.ObservedAt == snapshot.Context.AssembledAt {
			t.Fatalf("%s observed_at 绝不能等于 assembled_at（伪装成刚刚观测）", signal.BindingKey)
		}
	}
	// 时间锚必须仍然是真实的持久化事实时间。
	if !snapshot.FactAnchor.Before(future) {
		t.Fatalf("FactAnchor 不能被 wall clock 拉到未来: %v", snapshot.FactAnchor)
	}
}

func TestObservedAtOrFallsBackToEpochWithoutAnchor(t *testing.T) {
	// 连事实锚都没有时退回 Unix epoch：它明确表示"没有任何观测"，而不是"刚刚观测过"。
	got := ObservedAtOr(time.Time{}, time.Time{})
	if !got.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("无锚点时应退回 epoch，实际 %v", got)
	}
	real := testFactAnchor
	if got := ObservedAtOr(real, testNow); !got.Equal(real) {
		t.Fatalf("有真实观测时必须原样保留，实际 %v", got)
	}
}

func TestScopeRefFallsBackToServiceWithoutRefs(t *testing.T) {
	binding := model.SignalBinding{Key: "use_saturation", Scope: model.SignalBindingScope{Kind: model.ScopeKindResource}}
	if got := ScopeRef(binding, testServiceID); got != testServiceID {
		t.Fatalf("scope.refs 缺省时应退回 service ID，实际 %q", got)
	}
	binding.Scope.Refs = []string{testPeerID}
	if got := ScopeRef(binding, testServiceID); got != testPeerID {
		t.Fatalf("有 refs 时必须用声明值，实际 %q", got)
	}
}

func TestPolicyTakesMinimumOfProfileAndGlobalLimits(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	assembler.Limits = Limits{MaxWindowSeconds: 7200, MaxRows: 50}
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	// Profile max window 3600 < 全局 7200，取最小值。
	if snapshot.Policy.MaxWindowSeconds != 3600 {
		t.Fatalf("有效窗口上限应取最小值，实际 %d", snapshot.Policy.MaxWindowSeconds)
	}
	// 全局 max rows 50 < Profile 100，取最小值。
	if snapshot.Policy.MaxRows != 50 {
		t.Fatalf("有效行数上限应取最小值，实际 %d", snapshot.Policy.MaxRows)
	}
	if len(snapshot.Policy.AllowedToolKeys) != 1 {
		t.Fatalf("allowed tool keys 应来自 Profile: %v", snapshot.Policy.AllowedToolKeys)
	}
}

func TestNewRequestIDIsServerGeneratedAndContractShaped(t *testing.T) {
	first, err := NewRequestID()
	if err != nil {
		t.Fatalf("生成 request ID 失败: %v", err)
	}
	second, err := NewRequestID()
	if err != nil {
		t.Fatalf("生成 request ID 失败: %v", err)
	}
	if first == second {
		t.Fatal("request ID 必须每次不同")
	}
	if !requestIDPattern.MatchString(first) {
		t.Fatalf("request ID 必须满足契约格式，实际 %q", first)
	}
}

func TestAssembleReportsEmptyDomainAsEmptyNotFailure(t *testing.T) {
	published := testProfile()
	assembler, _, _, domain := newAssembler(t, &published)
	domain.facts = serviceobs.DomainFacts{
		BoundaryEntityIDs: []string{testEntityID}, FocusApplied: true,
	}
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindWorkspace, ServiceID: testServiceID,
		Focus: &model.Focus{Kind: "instance_id", Value: "ins_abc"},
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	section, _ := snapshot.SectionState(serviceobs.SectionKeyDomain)
	if section.State != model.AvailabilityEmpty {
		t.Fatalf("查询成功但无结果是 Empty，实际 %q", section.State)
	}
	if section.ReasonCode != serviceobs.ReasonFocusNotObserved {
		t.Fatalf("focus 未命中应给稳定 reason，实际 %q", section.ReasonCode)
	}
	// Empty 是一次成功查询，不把 Data 变成 Partial（Partial 只由遥测不可用造成）。
	for _, source := range snapshot.Evaluation.Data.Sources {
		if source.SourceKey == serviceobs.SourceKeyDomain && source.State != model.AvailabilityEmpty {
			t.Fatalf("domain source 应为 empty，实际 %q", source.State)
		}
	}
}

func TestAssembleIsDeterministic(t *testing.T) {
	published := testProfile()
	assembler, _, _, _ := newAssembler(t, &published)
	request := Request{Kind: KindWorkspace, ServiceID: testServiceID,
		Subject: Subject{UserID: "user-1", Roles: []string{"member"}},
		Focus:   &model.Focus{Kind: "instance_id", Value: "ins_abc"}}

	first, err := assembler.Assemble(stdcontext.Background(), request)
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	second, err := assembler.Assemble(stdcontext.Background(), request)
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	firstRaw, _ := json.Marshal(first.Context)
	secondRaw, _ := json.Marshal(second.Context)
	if string(firstRaw) != string(secondRaw) {
		t.Fatal("固定时钟与固定 request ID 下，两次装配必须逐字节一致")
	}
}

func TestAssembleSurfacesProfileDiagnostic(t *testing.T) {
	assembler, _, _, _ := newAssembler(t, nil)
	assembler.Profiles = &fakeProfiles{
		published: map[string]model.Profile{},
		diagnostic: map[string]profile.Diagnostic{testServiceID: {
			ServiceID: testServiceID, ReasonCodes: []string{profile.ReasonSchemaInvalid},
			Documents: []string{"example.json"},
		}},
	}
	snapshot, err := assembler.Assemble(stdcontext.Background(), Request{
		Kind: KindOverview, ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	if snapshot.ProfileDiagnostic == nil {
		t.Fatal("被 quarantine 的 Profile 必须能在总览显示稳定配置错误")
	}
	if snapshot.ProfileDiagnostic.ReasonCodes[0] != profile.ReasonSchemaInvalid {
		t.Fatalf("配置错误 reason 不符: %v", snapshot.ProfileDiagnostic.ReasonCodes)
	}
	// 有 diagnostic 也不能伪装成"已配置"。
	if snapshot.Profile != nil || snapshot.Evaluation.Data.State != model.DataStateUnconfigured {
		t.Fatal("被隔离的 Profile 不进 active registry，Data 仍是 Unconfigured")
	}
}
