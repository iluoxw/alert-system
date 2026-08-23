package view

import (
	stdcontext "context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
)

// canonical fixture 只在测试里出现。live adapter、assembler、projector 与 cache 都不读
// docs/contracts 的 example，本包也不导出任何 fixture 加载器——fixtures 绝不进入 live
// 数据路径。
const (
	canonicalExamplesDir = "../../../docs/contracts/service-observability/v1/examples"
	secondServiceProfile = "../profile/testdata/valid-second-service.json"
)

// 固定时钟：契约 fixture 的 assembled_at 是 02:09:30Z，观测是 02:09:20Z，Profile 的
// freshness TTL 是 60s，因此固定时钟下 fixture 观测恒为 fresh。
var fixtureNow = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)

// fixtureFactAnchor 是"已提交事实里最新的真实时间"。它必须早于 fixtureNow，
// 这样"从未观测过"的 signal 用它渲染 observed_at 时，测试能证明没有用 wall clock。
var fixtureFactAnchor = time.Date(2026, 8, 22, 2, 5, 13, 0, time.UTC)

func loadJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
}

func loadCanonicalProfile(t *testing.T) model.Profile {
	t.Helper()
	var published model.Profile
	loadJSON(t, filepath.Join(canonicalExamplesDir, "e2b.profile.json"), &published)
	published.Normalize()
	return published
}

// canonicalContext 是 e2b.context.json 里 projector 需要的部分。
type canonicalContext struct {
	Service struct {
		ServiceID     string   `json:"service_id"`
		EntityKey     string   `json:"entity_key"`
		Name          string   `json:"name"`
		RootEntityIDs []string `json:"root_entity_ids"`
	} `json:"service"`
	Focus    *model.Focus `json:"focus"`
	Topology struct {
		SnapshotID string                  `json:"snapshot_id"`
		ObservedAt model.Timestamp         `json:"observed_at"`
		Nodes      []model.ContextNode     `json:"nodes"`
		Relations  []model.ContextRelation `json:"relations"`
	} `json:"topology"`
	Signals      []model.Signal           `json:"signals"`
	EvidenceRefs []model.EvidenceRef      `json:"evidence_refs"`
	DomainState  model.ContextDomainState `json:"domain_state"`
}

func loadCanonicalContext(t *testing.T) canonicalContext {
	t.Helper()
	var fixture canonicalContext
	loadJSON(t, filepath.Join(canonicalExamplesDir, "e2b.context.json"), &fixture)
	return fixture
}

func loadCanonicalIdentity(t *testing.T) serviceobs.ServiceIdentity {
	t.Helper()
	var directory model.DirectoryView
	loadJSON(t, filepath.Join(canonicalExamplesDir, "e2b.directory-view.json"), &directory)
	if len(directory.Items) == 0 {
		t.Fatal("目录 fixture 至少要有一行")
	}
	identity := directory.Items[0].Service
	return serviceobs.ServiceIdentity{
		ServiceID: identity.ServiceID, EntityID: identity.EntityID, EntityKey: identity.EntityKey,
		Name: identity.Name, Description: identity.Description, Layer: identity.Layer,
		Kind: identity.Kind, Environment: identity.Environment, OwnerTeamID: identity.OwnerTeamID,
		OwnerTeamName: identity.OwnerTeamName, Tier: identity.Tier,
		DashboardURL: "/external/example/dashboard", RunbookURL: "/runbooks/example",
		EntityObservedAt: fixtureFactAnchor, UpdatedAt: fixtureFactAnchor.Add(-time.Hour),
	}
}

// fixtureReaders 用契约 fixture 的事实驱动通用 adapter 端口。这里没有任何 E2B 分支：
// 所有 E2B 专属内容都来自 Profile 与 fixture 数据。
type fixtureReaders struct {
	identity     serviceobs.ServiceIdentity
	dependencies []serviceobs.DeclaredDependency
	topology     serviceobs.TopologyFacts
	domain       serviceobs.DomainFacts
	observations serviceobs.ObservationFacts
	topologyErr  error
	domainErr    error
}

func (f *fixtureReaders) ListServices(stdcontext.Context, serviceobs.RegistrationFilter) (serviceobs.RegistrationList, error) {
	return serviceobs.RegistrationList{Items: []serviceobs.ServiceIdentity{f.identity}}, nil
}

func (f *fixtureReaders) ReadService(stdcontext.Context, string) (serviceobs.ServiceFacts, error) {
	return serviceobs.ServiceFacts{Identity: f.identity, Dependencies: f.dependencies}, nil
}

func (f *fixtureReaders) ReadTopology(stdcontext.Context, serviceobs.TopologyRequest) (serviceobs.TopologyFacts, error) {
	if f.topologyErr != nil {
		return serviceobs.TopologyFacts{}, f.topologyErr
	}
	return f.topology, nil
}

func (f *fixtureReaders) ReadDomain(stdcontext.Context, serviceobs.DomainRequest) (serviceobs.DomainFacts, error) {
	if f.domainErr != nil {
		return serviceobs.DomainFacts{}, f.domainErr
	}
	return f.domain, nil
}

func (f *fixtureReaders) ReadObservations(stdcontext.Context, serviceobs.ObservationRequest) (serviceobs.ObservationFacts, error) {
	return f.observations, nil
}

// fixtureProfiles 实现 soctx.ProfileSource。fixture 场景只关心"有没有 published
// Profile"，没有 quarantine 分支。
type fixtureProfiles struct{ published map[string]model.Profile }

func (f fixtureProfiles) PublishedForService(serviceID string) (model.Profile, bool) {
	published, ok := f.published[serviceID]
	return published, ok
}

func (fixtureProfiles) DiagnosticForService(string) (profile.Diagnostic, bool) {
	return profile.Diagnostic{}, false
}

// buildFixtureReaders 把 canonical context 的事实翻译成 ports 事实。
func buildFixtureReaders(t *testing.T, fixture canonicalContext, identity serviceobs.ServiceIdentity) *fixtureReaders {
	t.Helper()
	readers := &fixtureReaders{identity: identity}

	readers.topology = serviceobs.TopologyFacts{
		SnapshotID: fixture.Topology.SnapshotID,
		ObservedAt: mustTime(t, fixture.Topology.ObservedAt),
	}
	// live 邻域一定包含 service entity 自身（它是 seed），并且它到 Profile roots 之间有
	// declared 边。canonical context fixture 只画了 root 及其下游，所以这里补上，
	// 否则依赖台账这一投影根本没有输入可测。
	readers.topology.Nodes = append(readers.topology.Nodes, serviceobs.TopologyNode{
		EntityID: identity.EntityID, EntityKey: identity.EntityKey, Name: identity.Name,
		Layer: identity.Layer, Kind: string(identity.Kind), Environment: identity.Environment,
		ObservedAt: mustTime(t, fixture.Topology.ObservedAt),
	})
	for i, rootID := range fixture.Service.RootEntityIDs {
		readers.topology.Relations = append(readers.topology.Relations, serviceobs.TopologyRelation{
			RelationID:     "rel_service_root_" + strconv.Itoa(i),
			SourceEntityID: identity.EntityID, TargetEntityID: rootID,
			Kind: "part_of", Provenance: model.ProvenanceDeclared, Criticality: "critical",
			ObservedAt: fixtureFactAnchor,
		})
	}
	for _, node := range fixture.Topology.Nodes {
		readers.topology.Nodes = append(readers.topology.Nodes, serviceobs.TopologyNode{
			EntityID: node.EntityID, EntityKey: node.EntityKey, Name: node.Name,
			Layer: model.EntityLayer(node.Layer), Kind: node.Kind,
			Environment: "production", ObservedAt: mustTime(t, fixture.Topology.ObservedAt),
		})
	}
	for _, relation := range fixture.Topology.Relations {
		readers.topology.Relations = append(readers.topology.Relations, serviceobs.TopologyRelation{
			RelationID: relation.RelationID, SourceEntityID: relation.SourceEntityID,
			TargetEntityID: relation.TargetEntityID, Kind: relation.Kind,
			Provenance: relation.Provenance, Criticality: "high",
			ObservedAt: mustTime(t, fixture.Topology.ObservedAt),
			Stale:      relation.Freshness == model.FreshnessStale,
		})
	}

	readers.domain = serviceobs.DomainFacts{
		BoundaryEntityIDs: fixture.Service.RootEntityIDs,
		Counts:            model.DomainCounts{ActiveFaults: 1, ConfirmedImpacts: 0, ActiveIncidents: 0},
		LatestFactAt:      fixtureFactAnchor,
	}
	for _, alert := range fixture.DomainState.Alerts {
		readers.domain.Alerts = append(readers.domain.Alerts, serviceobs.DomainAlert{
			AlertID: alert.ID, Title: "边界内 Alert", State: alert.State,
			SourceState: alert.State, Severity: "critical",
			StartsAt: mustTime(t, alert.UpdatedAt), UpdatedAt: mustTime(t, alert.UpdatedAt),
		})
	}
	for _, fault := range fixture.DomainState.Faults {
		readers.domain.Faults = append(readers.domain.Faults, serviceobs.DomainFault{
			FaultID: fault.ID, Title: "边界内 Fault", State: fault.State,
			Severity: model.FaultSeverityCritical, EntityID: fixture.Service.RootEntityIDs[0],
			FirstDetectedAt: fixtureFactAnchor, UpdatedAt: mustTime(t, fault.UpdatedAt),
		})
	}

	// 每条 fixture signal 变成一次观测。evaluator 依据 Profile 阈值重新判定健康，
	// 因此"projector 不重算 Health"这件事是被结构保证的。
	for _, signal := range fixture.Signals {
		readers.observations.Signals = append(readers.observations.Signals, health.SignalObservation{
			BindingKey: signal.BindingKey, Availability: signal.Availability,
			Value: signal.Value, ObservedAt: mustTime(t, signal.ObservedAt),
			SourceKey: signal.SourceKey, UnknownReason: signal.UnknownReason,
			ThresholdSource: signal.ThresholdSource,
		})
	}
	readers.observations.Sources = []health.SourceObservation{{
		SourceKey: "prometheus_e2b_uat", State: model.AvailabilityAvailable,
		Freshness: model.FreshnessFresh, ObservedAt: fixtureNow.Add(-10 * time.Second), Retryable: true,
	}}
	readers.observations.EvidenceRefs = fixture.EvidenceRefs

	for _, relation := range readers.topology.Relations {
		if relation.SourceEntityID != identity.EntityID {
			continue
		}
		readers.dependencies = append(readers.dependencies, serviceobs.DeclaredDependency{
			RelationID: relation.RelationID, FromEntityID: relation.SourceEntityID,
			ToEntityID: relation.TargetEntityID, Provenance: model.ProvenanceDeclared,
			Relation: relation.Kind, ObservedAt: fixtureFactAnchor,
		})
	}
	return readers
}

func mustTime(t *testing.T, ts model.Timestamp) time.Time {
	t.Helper()
	parsed, err := ts.Time()
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", ts, err)
	}
	return parsed.UTC()
}

// newFixtureAssembler 构造一个由 canonical fixture 驱动的 assembler。
func newFixtureAssembler(readers *fixtureReaders, published *model.Profile) *soctx.Assembler {
	profiles := fixtureProfiles{published: map[string]model.Profile{}}
	if published != nil {
		profiles.published[published.ServiceRef.ServiceID] = *published
	}
	return &soctx.Assembler{
		Registrations: readers, Topology: readers, Domain: readers, Observations: readers,
		Profiles: profiles,
		// fixture 场景模拟"遥测与证据都已接入"，用来驱动 main 状态的契约回归。
		TelemetryConnected: true, EvidenceConnected: true,
		Clock:        func() time.Time { return fixtureNow },
		NewRequestID: func() (string, error) { return "req_fixture_projection", nil },
	}
}

func assembleFixture(t *testing.T, kind soctx.Kind, readers *fixtureReaders, published *model.Profile, focus *model.Focus) *soctx.AssemblySnapshot {
	t.Helper()
	assembler := newFixtureAssembler(readers, published)
	snapshot, err := assembler.Assemble(stdcontext.Background(), soctx.Request{
		Kind: kind, ServiceID: readers.identity.ServiceID,
		Subject:  soctx.Subject{UserID: "user_fixture_sre", Roles: []string{"service_viewer"}},
		Timezone: "Asia/Shanghai", Focus: focus,
	})
	if err != nil {
		t.Fatalf("装配 fixture 快照失败: %v", err)
	}
	return snapshot
}

func newValidator(t *testing.T) *contract.Validator {
	t.Helper()
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	return validator
}

func TestCanonicalFixtureDrivesGenericProjectorAcrossThreePageViews(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	validator := newValidator(t)

	overviewSnapshot := assembleFixture(t, soctx.KindOverview, readers, &published, nil)
	overview := ProjectOverview(overviewSnapshot)
	if err := validator.ValidateValue(contract.KindPageView, overview); err != nil {
		t.Fatalf("总览未通过 Page View schema: %v", err)
	}

	workspaceSnapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(workspaceSnapshot)
	if err := validator.ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("工作区未通过 Page View schema: %v", err)
	}

	directory := ProjectDirectory(DirectoryInput{
		RequestID:   overviewSnapshot.RequestID,
		AssembledAt: overviewSnapshot.AssembledAt,
		Rows: []DirectoryRow{{
			Identity: identity, Profile: ProjectProfileRef(&published),
			Health: overview.Health, DataStatus: overview.DataStatus,
			GoldenSignals: overview.GoldenSignals, DomainCounts: overview.DomainCounts,
			ObservedAt: overviewSnapshot.FactAnchor,
		}},
		Summary: model.DirectorySummary{Managed: 1},
	})
	if err := validator.ValidateValue(contract.KindPageView, directory); err != nil {
		t.Fatalf("目录未通过 Page View schema: %v", err)
	}

	// identity 在三页之间不漂移。
	if overview.Service != directory.Items[0].Service || overview.Service != workspace.Service {
		t.Fatal("三页的 service identity 必须一致")
	}
	// profile 引用不漂移。
	if overview.Profile == nil || *overview.Profile != *workspace.Profile ||
		overview.Profile.ProfileKey != published.ProfileKey {
		t.Fatalf("profile 引用漂移: %+v", overview.Profile)
	}
	// scope/focus 来自 context，不由 projector 发明。
	if workspace.Scope.Tenant != workspaceSnapshot.Scope.Tenant ||
		workspace.Scope.From != workspaceSnapshot.Scope.TimeRange.From ||
		workspace.Scope.To != workspaceSnapshot.Scope.TimeRange.To ||
		workspace.Scope.Timezone != "Asia/Shanghai" {
		t.Fatalf("scope 漂移: %+v", workspace.Scope)
	}
	if workspace.Focus == nil || workspace.Focus.Kind != fixture.Focus.Kind ||
		workspace.Focus.Value != fixture.Focus.Value {
		t.Fatalf("focus 漂移: %+v", workspace.Focus)
	}
	// Health 只来自 evaluator，三页一致。
	if overview.Health.Status != workspace.Health.Status {
		t.Fatalf("三页 Health 必须来自同一个 evaluator: %q vs %q",
			overview.Health.Status, workspace.Health.Status)
	}
	if workspace.Health.Status != model.HealthCritical {
		t.Fatalf("fixture 主状态应为 Critical，实际 %q", workspace.Health.Status)
	}
}

func TestWorkspaceProjectionDoesNotDriftFromContext(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if snapshot.Context == nil {
		t.Fatal("有 Profile 时必须构造 DiagnosticContext")
	}

	// topology refs 与 context 完全一致。
	if got, want := idsOfNodes(workspace.Topology.Nodes), idsOfContextNodes(snapshot.Context.Topology.Nodes); !equalStringSets(got, want) {
		t.Fatalf("topology node refs 漂移: %v vs %v", got, want)
	}
	if got, want := idsOfRelations(workspace.Topology.Relations), idsOfContextRelations(snapshot.Context.Topology.Relations); !equalStringSets(got, want) {
		t.Fatalf("topology relation refs 漂移: %v vs %v", got, want)
	}
	if workspace.Topology.SnapshotID != snapshot.Context.Topology.SnapshotID {
		t.Fatal("topology snapshot ID 漂移")
	}

	// signal 投影必须与 context 的 (value, availability, health, measurement_role) 一致。
	contextSignals := map[string]model.Signal{}
	for _, signal := range snapshot.Context.Signals {
		contextSignals[signal.BindingKey] = signal
	}
	projected := map[string]model.SignalValue{}
	collect := func(signals []model.SignalValue) {
		for _, signal := range signals {
			projected[signal.BindingKey] = signal
		}
	}
	collect(workspace.GoldenSignals)
	for _, node := range workspace.Topology.Nodes {
		collect(node.Signals)
	}
	for _, relation := range workspace.Topology.Relations {
		collect(relation.Signals)
	}
	// canonical fixture 里每个 binding 都能挂到 bounded topology，所以全部 10 个都应被投影。
	if len(projected) != len(contextSignals) {
		t.Fatalf("可挂载 signal 应全部被投影: %d vs %d", len(projected), len(contextSignals))
	}
	for key, signal := range projected {
		reference, ok := contextSignals[key]
		if !ok {
			t.Fatalf("投影出了 context 里不存在的 signal %q", key)
		}
		if !equalFloatPtr(signal.Value, reference.Value) {
			t.Fatalf("%s value 漂移: %v vs %v", key, signal.Value, reference.Value)
		}
		if string(signal.Availability) != string(reference.Availability) {
			t.Fatalf("%s availability 漂移: %q vs %q", key, signal.Availability, reference.Availability)
		}
		if signal.Health != reference.Health {
			t.Fatalf("%s health 漂移: %q vs %q", key, signal.Health, reference.Health)
		}
		if signal.MeasurementRole != reference.MeasurementRole {
			t.Fatalf("%s measurement_role 漂移: %q vs %q", key, signal.MeasurementRole, reference.MeasurementRole)
		}
		if signal.ScopeRef != reference.ScopeRef || signal.ScopeKind != reference.ScopeKind {
			t.Fatalf("%s scope 漂移: %q/%q vs %q/%q", key,
				signal.ScopeKind, signal.ScopeRef, reference.ScopeKind, reference.ScopeRef)
		}
	}

	// domain refs 与 context 完全一致。
	if !equalDomainRefs(workspace.DomainState.Alerts, snapshot.Context.DomainState.Alerts) ||
		!equalDomainRefs(workspace.DomainState.Faults, snapshot.Context.DomainState.Faults) ||
		!equalDomainRefs(workspace.DomainState.Impacts, snapshot.Context.DomainState.Impacts) ||
		!equalDomainRefs(workspace.DomainState.Incidents, snapshot.Context.DomainState.Incidents) {
		t.Fatal("domain refs 漂移")
	}
	// evidence refs 与 context 完全一致。
	if len(workspace.EvidenceRefs) != len(snapshot.Context.EvidenceRefs) {
		t.Fatalf("evidence refs 漂移: %d vs %d", len(workspace.EvidenceRefs), len(snapshot.Context.EvidenceRefs))
	}
	// journey stage 的 canonical refs 原样保留，即使它们落在 bounded 投影之外。
	stageKeys := []string{}
	for _, stage := range workspace.Journey {
		stageKeys = append(stageKeys, stage.Key)
	}
	wantStages := []string{}
	for _, journey := range published.Journeys {
		if journey.FocusKind == fixture.Focus.Kind {
			for _, stage := range journey.Stages {
				wantStages = append(wantStages, stage.Key)
			}
		}
	}
	if len(stageKeys) != len(wantStages) {
		t.Fatalf("journey stage 数量/顺序漂移: %v vs %v", stageKeys, wantStages)
	}
	for i := range stageKeys {
		if stageKeys[i] != wantStages[i] {
			t.Fatalf("journey stage 顺序漂移: %v vs %v", stageKeys, wantStages)
		}
	}
}

func TestGoldenSignalsAlwaysProjectedAndProxyLabelPreserved(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)

	if len(workspace.GoldenSignals) != 4 {
		t.Fatalf("golden_signals 恒为四项，实际 %d", len(workspace.GoldenSignals))
	}
	wantSemantics := model.GoldenSemantics()
	for i, signal := range workspace.GoldenSignals {
		if signal.Semantic != wantSemantics[i] {
			t.Fatalf("golden 顺序应固定 latency/traffic/errors/saturation，实际 %q", signal.Semantic)
		}
	}
	// Traffic 没有方向阈值，永远中性，不能因为上升着红或着绿。
	traffic := workspace.GoldenSignals[1]
	if traffic.Health != model.SignalHealthNeutral {
		t.Fatalf("Traffic 必须中性，实际 %q", traffic.Health)
	}
	// HugePages 容量压力 proxy 的角色与原因不可丢。
	saturation := workspace.GoldenSignals[3]
	if saturation.MeasurementRole != model.MeasurementRoleProxy || saturation.ProxyReason == "" {
		t.Fatalf("proxy 角色与原因必须原样保留: %+v", saturation)
	}
	// 真实 USE saturation 缺失必须是 Unknown，不能复用 utilization 冒充。
	var placement model.SignalValue
	for _, node := range workspace.Topology.Nodes {
		for _, signal := range node.Signals {
			if signal.BindingKey == "use_host_placement_saturation" {
				placement = signal
			}
		}
	}
	if placement.BindingKey == "" {
		t.Fatal("USE saturation binding 必须被投影")
	}
	if placement.Availability == model.AvailabilityAvailable || placement.Value != nil ||
		placement.Health != model.SignalHealthUnknown || placement.UnknownReason == "" {
		t.Fatalf("缺失的真实 saturation 必须 Unknown 且带原因: %+v", placement)
	}
	if placement.DisplayValue != DisplayUnknown {
		t.Fatalf("没有数值必须显示 Unknown，实际 %q", placement.DisplayValue)
	}
}

func TestProjectorHandlesDanglingScopeRefsWithoutSynthesizingTopology(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	// 把一个 USE binding 的 scope.refs 指向不存在的实体，另一个清空 refs。
	// 两者都不能让 projector 发明 node/relation 或扩大查询边界。
	danglingKey, emptyKey := "", ""
	for i := range published.SignalBindings {
		binding := &published.SignalBindings[i]
		if binding.Family != model.SignalFamilyUSE {
			continue
		}
		if danglingKey == "" {
			binding.Scope.Refs = []string{"entity_does_not_exist"}
			danglingKey = binding.Key
			continue
		}
		if emptyKey == "" {
			binding.Scope.Refs = nil
			emptyKey = binding.Key
		}
	}
	if danglingKey == "" || emptyKey == "" {
		t.Fatal("fixture Profile 应至少有两个 USE binding")
	}

	// 基线取 adapter 真实返回的邻域大小：悬空 scope 不得让投影多出任何对象。
	nodesBefore := len(readers.topology.Nodes)
	relationsBefore := len(readers.topology.Relations)
	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)

	validator := newValidator(t)
	if err := validator.ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("悬空 scope 下工作区仍必须过 schema: %v", err)
	}
	// 既不发明节点，也不发明关系。
	if len(workspace.Topology.Nodes) != nodesBefore || len(workspace.Topology.Relations) != relationsBefore {
		t.Fatalf("悬空 scope 不得合成拓扑对象: %d/%d vs %d/%d",
			len(workspace.Topology.Nodes), len(workspace.Topology.Relations), nodesBefore, relationsBefore)
	}
	// evaluator 与 DiagnosticContext 仍完整保留这两个 binding 的最小范围 Unknown 结果。
	for _, key := range []string{danglingKey, emptyKey} {
		found := false
		for _, evaluation := range snapshot.Evaluation.Signals {
			if evaluation.Binding.Key != key {
				continue
			}
			found = true
			if evaluation.Availability == model.SignalAvailabilityAvailable && evaluation.Value == nil {
				t.Fatalf("%s 的 evaluator 结果自相矛盾: %+v", key, evaluation)
			}
		}
		if !found {
			t.Fatalf("悬空 binding %q 不得被当成不存在而丢弃", key)
		}
		if !containsBindingKey(snapshot.Context.Signals, key) {
			t.Fatalf("DiagnosticContext 必须保留悬空 binding %q", key)
		}
	}
	// 它们不得被错误挂到 service 节点或任何其它节点上。
	for _, node := range workspace.Topology.Nodes {
		for _, signal := range node.Signals {
			if signal.BindingKey == danglingKey || signal.BindingKey == emptyKey {
				t.Fatalf("悬空 binding %q 不得挂到节点 %q", signal.BindingKey, node.EntityID)
			}
		}
	}
	// 局部缺口必须通过稳定 section 与 incomplete 摘要显式表达。
	section := findSection(workspace.Sections, SectionKeySignalScope)
	if section == nil || section.State != model.AvailabilityUnknown ||
		section.ReasonCode != serviceobs.ReasonSignalScopeUnresolved {
		t.Fatalf("悬空 scope 必须有稳定 section: %+v", section)
	}
	if workspace.DiagnosticSummary.Level != model.DiagnosticLevelIncomplete {
		t.Fatalf("存在局部缺口时摘要必须 incomplete，实际 %q", workspace.DiagnosticSummary.Level)
	}
	missing := workspace.DiagnosticSummary.MissingRequirements
	for _, key := range []string{danglingKey, emptyKey} {
		if !containsString(missing, serviceobs.ReasonSignalScopeUnresolved+":"+key) {
			t.Fatalf("missing_requirements 应列出 %q: %v", key, missing)
		}
	}
	// Golden binding 无论如何都必须出现在 golden_signals。
	if len(workspace.GoldenSignals) != 4 {
		t.Fatalf("golden_signals 恒为四项，实际 %d", len(workspace.GoldenSignals))
	}
}

func TestGoldenBindingWithDanglingScopeStillProjectedAsUnknown(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	// Golden saturation 的 scope 指向不存在的实体，并且不返回它的观测。
	var goldenKey string
	for i := range published.SignalBindings {
		binding := &published.SignalBindings[i]
		if binding.Family == model.SignalFamilyGolden && binding.Semantic == model.SemanticSaturation {
			binding.Scope.Refs = []string{"entity_does_not_exist"}
			goldenKey = binding.Key
		}
	}
	filtered := readers.observations.Signals[:0:0]
	for _, signal := range readers.observations.Signals {
		if signal.BindingKey != goldenKey {
			filtered = append(filtered, signal)
		}
	}
	readers.observations.Signals = filtered

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("工作区必须仍过 schema: %v", err)
	}
	if len(workspace.GoldenSignals) != 4 {
		t.Fatalf("Golden binding 必须始终出现，实际 %d 项", len(workspace.GoldenSignals))
	}
	golden := workspace.GoldenSignals[3]
	if golden.BindingKey != goldenKey {
		t.Fatalf("第四张 Golden 卡应是 saturation，实际 %q", golden.BindingKey)
	}
	if golden.Value != nil || golden.Health != model.SignalHealthUnknown || golden.UnknownReason == "" {
		t.Fatalf("Golden 缺失必须按 Unknown 降级: %+v", golden)
	}
	// scope_ref 必须非空且是声明值，绝不因为悬空就把它挂到 service 节点上。
	if golden.ScopeRef != "entity_does_not_exist" {
		t.Fatalf("scope_ref 应保留 Profile 声明值，实际 %q", golden.ScopeRef)
	}
}

func TestMissingSignalObservedAtUsesFactAnchorNotWallClock(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	// 遥测完全未接入：这是 Phase 2 的真实形态。
	readers.observations = serviceobs.ObservationFacts{}

	assembler := newFixtureAssembler(readers, &published)
	assembler.Observations = serviceobs.NewUnavailableObservationReader()
	assembler.TelemetryConnected = false
	assembler.EvidenceConnected = false
	// 把 wall clock 推到很远的未来：任何用 now 冒充观测的实现都会在这里失败。
	future := fixtureNow.Add(500 * 24 * time.Hour)
	assembler.Clock = func() time.Time { return future }

	snapshot, err := assembler.Assemble(stdcontext.Background(), soctx.Request{
		Kind: soctx.KindWorkspace, ServiceID: identity.ServiceID, Focus: fixture.Focus,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("未接遥测的工作区仍必须过 schema: %v", err)
	}

	anchor := model.NewTimestamp(snapshot.FactAnchor)
	if anchor == workspace.AssembledAt {
		t.Fatal("事实时间锚不能等于 assembled_at")
	}
	checked := 0
	for _, signal := range workspace.GoldenSignals {
		if signal.ObservedAt != anchor {
			t.Fatalf("%s observed_at 应取真实事实时间锚 %q，实际 %q", signal.BindingKey, anchor, signal.ObservedAt)
		}
		if signal.ObservedAt == workspace.AssembledAt {
			t.Fatalf("%s observed_at 绝不能是 wall clock", signal.BindingKey)
		}
		checked++
	}
	if checked != 4 {
		t.Fatalf("应检查四张 Golden 卡，实际 %d", checked)
	}
	// declared topology 与 domain 必须完整保留。
	if len(workspace.Topology.Nodes) == 0 || len(workspace.DomainState.Faults) == 0 {
		t.Fatal("未接遥测不得清空 declared topology 或 domain")
	}
	// 每个没有 signal 的节点都必须是 Unknown：存在不等于健康。
	for _, node := range workspace.Topology.Nodes {
		if len(node.Signals) == 0 && node.Health != model.HealthUnknown {
			t.Fatalf("无 signal 的节点必须 Unknown: %+v", node)
		}
	}
	for _, relation := range workspace.Topology.Relations {
		if len(relation.Signals) == 0 && relation.Health != model.HealthUnknown {
			t.Fatalf("无 signal 的关系必须 Unknown: %+v", relation)
		}
	}
	if workspace.DataStatus.State != model.DataStatePartial {
		t.Fatalf("未接遥测应让 Data Partial，实际 %q", workspace.DataStatus.State)
	}
	if workspace.DiagnosticSummary.Level != model.DiagnosticLevelIncomplete {
		t.Fatalf("缺证据时摘要必须 incomplete，实际 %q", workspace.DiagnosticSummary.Level)
	}
}

func TestOverviewWithoutProfileKeepsIdentityAndDomain(t *testing.T) {
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	readers.observations = serviceobs.ObservationFacts{}

	snapshot := assembleFixture(t, soctx.KindOverview, readers, nil, nil)
	overview := ProjectOverview(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, overview); err != nil {
		t.Fatalf("Unconfigured 总览必须过 schema: %v", err)
	}
	if overview.Profile != nil {
		t.Fatal("未配置 Profile 时 profile 必须是 null")
	}
	if overview.DataStatus.State != model.DataStateUnconfigured {
		t.Fatalf("未配置 Profile 时 Data 必须 Unconfigured，实际 %q", overview.DataStatus.State)
	}
	if len(overview.GoldenSignals) != 0 {
		t.Fatal("没有 Profile 就没有 binding，不得猜指标")
	}
	// identity 与 domain 保留。
	if overview.Service.ServiceID != identity.ServiceID || len(overview.Faults) == 0 {
		t.Fatal("Unconfigured 不得清空 identity 或领域状态")
	}
	if len(overview.FocusKinds) != 0 {
		t.Fatal("没有 Profile 就没有 focus 启动器")
	}
	// declared 依赖仍然可见。
	if len(overview.Dependencies) == 0 {
		t.Fatal("无 Profile 时仍应保留 declared 依赖概览")
	}
	for _, dependency := range overview.Dependencies {
		if dependency.Health != model.HealthUnknown {
			t.Fatalf("没有 signal 的依赖必须 Unknown: %+v", dependency)
		}
	}
}

func TestOverviewTopologyFailureKeepsDomainSection(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	readers.topologyErr = errTopologyUnavailable

	snapshot := assembleFixture(t, soctx.KindOverview, readers, &published, nil)
	overview := ProjectOverview(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, overview); err != nil {
		t.Fatalf("topology 失败的总览必须仍过 schema: %v", err)
	}
	topology := findSection(overview.Sections, serviceobs.SectionKeyTopology)
	if topology == nil || topology.State != model.AvailabilityUnavailable {
		t.Fatalf("topology section 应 unavailable: %+v", topology)
	}
	domain := findSection(overview.Sections, serviceobs.SectionKeyDomain)
	if domain == nil || domain.State != model.AvailabilityAvailable {
		t.Fatalf("topology 失败不得连带降级 domain: %+v", domain)
	}
	if len(overview.Faults) == 0 {
		t.Fatal("topology 失败不得让领域状态消失")
	}
	if len(overview.Dependencies) != 0 {
		t.Fatal("拿不到拓扑时依赖台账应为空，而不是猜出来")
	}
}

func TestTimelineSectionMirrorsDomainFailureInsteadOfClaimingEmpty(t *testing.T) {
	// 时间线完全由领域事实派生。domain 读取失败时 DomainFacts 是空的，但那不是"这个服务
	// 没有事件"——报成 Empty 等于用一次失败的查询冒充"确认无事件"。
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	readers.domainErr = errDomainUnavailable

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("domain 失败的工作区仍必须过 schema: %v", err)
	}

	timeline := findSection(workspace.Sections, serviceobs.SectionKeyTimeline)
	if timeline == nil {
		t.Fatal("必须有 timeline section")
	}
	if timeline.State == model.AvailabilityEmpty {
		t.Fatal("domain 失败时 timeline 绝不能报 Empty")
	}
	domain := findSection(workspace.Sections, serviceobs.SectionKeyDomain)
	if domain == nil || domain.State != model.AvailabilityUnavailable {
		t.Fatalf("domain section 应 unavailable: %+v", domain)
	}
	// 最小范围镜像同一个状态与 reason/retryable。
	if timeline.State != domain.State {
		t.Fatalf("timeline 应镜像 domain 状态: %q vs %q", timeline.State, domain.State)
	}
	if timeline.ReasonCode == "" || timeline.ReasonCode != domain.ReasonCode {
		t.Fatalf("timeline 必须带与 domain 一致的稳定 reason: %+v", timeline)
	}
	if timeline.Retryable != domain.Retryable {
		t.Fatalf("retryable 应与 domain 一致: %+v", timeline)
	}
	if len(workspace.Timeline) != 0 {
		t.Fatal("读不到领域事实时不得凭空产生事件")
	}
	// 只降级领域相关区块：identity 与 declared topology 保留。
	if len(workspace.Topology.Nodes) == 0 {
		t.Fatal("domain 失败不得清空 declared topology")
	}
}

func TestTimelineSectionIsEmptyOnlyForSuccessfulZeroFacts(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)
	// 一次成功查询，边界内确实没有任何领域对象。
	readers.domain = serviceobs.DomainFacts{BoundaryEntityIDs: []string{identity.EntityID}}

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	timeline := findSection(workspace.Sections, serviceobs.SectionKeyTimeline)
	if timeline == nil || timeline.State != model.AvailabilityEmpty {
		t.Fatalf("成功查询但零事实才是 Empty: %+v", timeline)
	}
	if timeline.ReasonCode != "" {
		t.Fatalf("Empty 不需要失败 reason: %+v", timeline)
	}
}

func TestTimelineSectionIsAvailableWithFacts(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	timeline := findSection(workspace.Sections, serviceobs.SectionKeyTimeline)
	if timeline == nil || timeline.State != model.AvailabilityAvailable {
		t.Fatalf("有领域事实时 timeline 应 available: %+v", timeline)
	}
	if len(workspace.Timeline) == 0 {
		t.Fatal("有领域事实时应产生时间线事件")
	}
}

func TestNodeAndRelationHealthDoNotDriftFromContext(t *testing.T) {
	// 同一批 scoped evaluation 必须在 DiagnosticContext 与 Page View 得出同一个对象健康。
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if snapshot.Context == nil {
		t.Fatal("有 Profile 时必须构造 DiagnosticContext")
	}

	contextHealth := map[string]model.Health{}
	for _, node := range snapshot.Context.Topology.Nodes {
		contextHealth[node.EntityID] = node.Health
	}
	sawOpinionated := false
	for _, node := range workspace.Topology.Nodes {
		want, ok := contextHealth[node.EntityID]
		if !ok {
			t.Fatalf("工作区出现了 context 里没有的节点 %q", node.EntityID)
		}
		if node.Health != want {
			t.Fatalf("节点 %q 健康漂移: 页面 %q vs context %q", node.EntityID, node.Health, want)
		}
		if len(node.Signals) == 0 && node.Health != model.HealthUnknown {
			t.Fatalf("没有 signal 的节点必须 Unknown: %+v", node)
		}
		if len(node.Signals) > 0 && node.Health != model.HealthUnknown {
			sawOpinionated = true
		}
	}
	// fixture 的 host fleet 挂了 critical/degraded USE signal，因此至少要有一个节点
	// 得出非 Unknown 的结论——否则这个测试无法证明共享聚合真的生效。
	if !sawOpinionated {
		t.Fatal("fixture 应至少让一个挂了 signal 的节点得出非 Unknown 健康")
	}

	// 关系健康同样来自共享聚合。
	for _, relation := range workspace.Topology.Relations {
		want := snapshot.ObjectHealth("", relation.RelationID)
		if relation.Health != want {
			t.Fatalf("关系 %q 健康漂移: %q vs %q", relation.RelationID, relation.Health, want)
		}
		if len(relation.Signals) == 0 && relation.Health != model.HealthUnknown {
			t.Fatalf("没有 signal 的关系必须 Unknown: %+v", relation)
		}
	}

	// 总览的依赖台账复用同一份节点健康，绝不因"拓扑里存在"就显示绿色。
	overviewSnapshot := assembleFixture(t, soctx.KindOverview, readers, &published, nil)
	overview := ProjectOverview(overviewSnapshot)
	for _, dependency := range overview.Dependencies {
		want := overviewSnapshot.ObjectHealth(dependency.EntityID, "")
		if dependency.Health != want {
			t.Fatalf("依赖 %q 健康漂移: %q vs %q", dependency.EntityID, dependency.Health, want)
		}
		if dependency.Health == model.HealthHealthy {
			if got := overviewSnapshot.ObjectHealth(dependency.EntityID, ""); got != model.HealthHealthy {
				t.Fatalf("依赖不得凭拓扑里存在就显示绿色: %+v", dependency)
			}
		}
	}
}

func TestMergedRelationKeepsProfileScopeRefResolvable(t *testing.T) {
	// canonical 归一化把 declared 与 observed 两行折叠成一条边。Profile 的 signal scope
	// 可能引用的是被折叠掉的那一行，归一化绝不能因此让它变成悬空引用。
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	const declaredID = "rel_service_provider_declared"
	const observedID = "rel_api_orchestrator"
	// 找到 fixture 里 RED signal 引用的那条 observed 关系，给它加一个 declared 孪生行，
	// 并把 canonical ID 换成 declared 的那一个——正是归一化后的真实形态。
	replaced := false
	for i := range readers.topology.Relations {
		if readers.topology.Relations[i].RelationID != observedID {
			continue
		}
		readers.topology.Relations[i].RelationID = declaredID
		readers.topology.Relations[i].Provenance = model.ProvenanceDeclaredAndObserved
		readers.topology.Relations[i].MergedRelationIDs = []string{declaredID, observedID}
		replaced = true
	}
	if !replaced {
		t.Fatalf("fixture 应包含关系 %q", observedID)
	}

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("归一化后的工作区仍必须过 schema: %v", err)
	}

	// Profile 的 RED binding 仍然挂在这条 canonical 关系上，没有变成悬空。
	var canonical *model.TopologyRelationView
	for i := range workspace.Topology.Relations {
		if workspace.Topology.Relations[i].RelationID == declaredID {
			canonical = &workspace.Topology.Relations[i]
		}
	}
	if canonical == nil {
		t.Fatalf("应存在 canonical 关系 %q: %+v", declaredID, workspace.Topology.Relations)
	}
	if len(canonical.Signals) == 0 {
		t.Fatal("引用被折叠成员 ID 的 signal scope 必须仍能挂到 canonical 关系上")
	}
	if canonical.Provenance != model.ProvenanceDeclaredAndObserved {
		t.Fatalf("provenance 应是 declared_and_observed，实际 %q", canonical.Provenance)
	}
	for _, key := range snapshot.UnresolvedBindings {
		if strings.HasPrefix(key, "red_") {
			t.Fatalf("归一化不得让 RED binding 变成悬空引用: %v", snapshot.UnresolvedBindings)
		}
	}
	// 关系健康仍与 context 一致。
	if canonical.Health != snapshot.ObjectHealth("", declaredID) {
		t.Fatal("归一化后关系健康仍必须来自共享聚合")
	}
}

func TestMultiRefAggregateIsNotFabricatedPerObject(t *testing.T) {
	// scope 解析到多个 canonical 对象时数值是跨对象聚合值（max over hosts）。v1 的
	// signalValue.scope_ref 是单值，把同一个数值复制到每个对象上会把"这一组里最坏的是
	// 99%"显示成"这个对象是 99%"——那是伪造逐对象测量。契约安全的做法是：不做逐对象展示，
	// 保留完整判定，用稳定的局部诊断暴露缺口；健康意见仍逐对象生效。
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	// 取 fixture 拓扑里两个真实节点作为多 ref 目标。
	if len(readers.topology.Nodes) < 2 {
		t.Fatal("fixture 拓扑应至少有两个节点")
	}
	firstID := readers.topology.Nodes[0].EntityID
	secondID := readers.topology.Nodes[1].EntityID
	published.SignalBindings = append(published.SignalBindings, model.SignalBinding{
		Key: "use_fleet_worst_saturation", Label: "机群最坏饱和度", Family: model.SignalFamilyUSE,
		Semantic: model.SemanticSaturation,
		Scope:    model.SignalBindingScope{Kind: model.ScopeKindResource, Refs: []string{firstID, secondID}},
		ToolKey:  "metrics.query_semantic_signal.v1", OutputField: "series.pressure",
		Unit: "percent", Aggregation: model.AggregationMax,
		Thresholds: &model.SignalThresholds{
			Mode: model.ThresholdModeAbsolute, Direction: model.ThresholdDirectionAbove,
			Degraded: 85, Critical: 95,
		},
	})
	// 给这个聚合 binding 一个越过 critical 阈值的观测。
	value := 99.0
	readers.observations.Signals = append(readers.observations.Signals, health.SignalObservation{
		BindingKey: "use_fleet_worst_saturation", Availability: model.SignalAvailabilityAvailable,
		Value: &value, ObservedAt: fixtureNow.Add(-10 * time.Second),
		SourceKey: "prometheus_e2b_uat",
	})

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("多 ref 聚合场景仍必须过 schema: %v", err)
	}

	// 两个 ref 都被保留为挂载目标。
	placement, ok := snapshot.PlacementFor("use_fleet_worst_saturation")
	if !ok || len(placement.NodeIDs) != 2 {
		t.Fatalf("两个 ref 都必须保留: %+v", placement)
	}
	// 绝不把聚合数值复制到任何一个对象上。
	for _, node := range workspace.Topology.Nodes {
		for _, signal := range node.Signals {
			if signal.BindingKey == "use_fleet_worst_saturation" {
				t.Fatalf("跨对象聚合值不得伪装成节点 %q 的逐对象测量: %+v", node.EntityID, signal)
			}
		}
	}
	// 健康意见仍逐对象生效：知道最坏值越了阈值，就不能声称其中任何一个健康。
	for _, id := range []string{firstID, secondID} {
		if got := snapshot.ObjectHealth(id, ""); got != model.HealthCritical {
			t.Fatalf("对象 %s 应收到 critical 意见，实际 %q", id, got)
		}
		for _, node := range workspace.Topology.Nodes {
			if node.EntityID == id && node.Health != model.HealthCritical {
				t.Fatalf("页面节点 %s 健康应与共享聚合一致，实际 %q", id, node.Health)
			}
		}
	}
	// 完整判定仍保留在 DiagnosticContext 里，不是被丢弃。
	if !containsBindingKey(snapshot.Context.Signals, "use_fleet_worst_saturation") {
		t.Fatal("DiagnosticContext 必须保留完整判定")
	}
	// 缺口用稳定的局部诊断暴露。
	section := findSection(workspace.Sections, SectionKeySignalScope)
	if section == nil || section.State != model.AvailabilityUnknown {
		t.Fatalf("必须有稳定的 signal_scope 局部诊断: %+v", section)
	}
	if !containsString(workspace.DiagnosticSummary.MissingRequirements,
		serviceobs.ReasonSignalScopeAggregateNotProjectable+":use_fleet_worst_saturation") {
		t.Fatalf("missing_requirements 应列出聚合缺口: %v",
			workspace.DiagnosticSummary.MissingRequirements)
	}
	if workspace.DiagnosticSummary.Level != model.DiagnosticLevelIncomplete {
		t.Fatalf("存在局部缺口时摘要必须 incomplete，实际 %q", workspace.DiagnosticSummary.Level)
	}
	// 它绝不是"悬空"：悬空 reason 不应出现在这个 binding 上。
	if containsString(workspace.DiagnosticSummary.MissingRequirements,
		serviceobs.ReasonSignalScopeUnresolved+":use_fleet_worst_saturation") {
		t.Fatal("多目标聚合不是悬空引用")
	}
}

func TestSingleRefAfterMemberDedupStillProjectsPerObject(t *testing.T) {
	// canonical ID 与被折叠成员 ID 同时出现时去重成一个目标，因此仍然是无损的逐对象展示，
	// 不应被误判成多目标聚合而丢掉数值。
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	const canonicalID = "rel_canonical_edge"
	const memberID = "rel_api_orchestrator"
	replaced := false
	for i := range readers.topology.Relations {
		if readers.topology.Relations[i].RelationID != memberID {
			continue
		}
		readers.topology.Relations[i].RelationID = canonicalID
		readers.topology.Relations[i].Provenance = model.ProvenanceDeclaredAndObserved
		readers.topology.Relations[i].MergedRelationIDs = []string{canonicalID, memberID}
		replaced = true
	}
	if !replaced {
		t.Fatalf("fixture 应包含关系 %q", memberID)
	}
	// 让一个 RED binding 同时引用 canonical 与成员 ID。
	patched := false
	for i := range published.SignalBindings {
		binding := &published.SignalBindings[i]
		if binding.Family != model.SignalFamilyRED {
			continue
		}
		binding.Scope.Refs = []string{memberID, canonicalID}
		patched = true
	}
	if !patched {
		t.Fatal("fixture Profile 应包含 RED binding")
	}

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	workspace := ProjectWorkspace(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, workspace); err != nil {
		t.Fatalf("去重场景仍必须过 schema: %v", err)
	}

	var canonical *model.TopologyRelationView
	for i := range workspace.Topology.Relations {
		if workspace.Topology.Relations[i].RelationID == canonicalID {
			canonical = &workspace.Topology.Relations[i]
		}
	}
	if canonical == nil {
		t.Fatalf("应存在 canonical 关系 %q", canonicalID)
	}
	// 去重后是单目标，因此 RED 数值必须照常逐对象展示。
	if len(canonical.Signals) == 0 {
		t.Fatal("去重成单目标后必须保留逐对象数值展示")
	}
	for _, key := range snapshot.AggregateOnlyBindings {
		if strings.HasPrefix(key, "red_") {
			t.Fatalf("指向同一 canonical 关系的多个 ref 去重后不是聚合: %v",
				snapshot.AggregateOnlyBindings)
		}
	}
	// 每条 RED binding 只出现一次，不因为写了两个 ref 而重复展示。
	seen := map[string]int{}
	for _, signal := range canonical.Signals {
		seen[signal.BindingKey]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("binding %q 在同一对象上重复展示 %d 次", key, count)
		}
	}
}

func TestProjectionIsDeterministic(t *testing.T) {
	published := loadCanonicalProfile(t)
	fixture := loadCanonicalContext(t)
	identity := loadCanonicalIdentity(t)
	readers := buildFixtureReaders(t, fixture, identity)

	snapshot := assembleFixture(t, soctx.KindWorkspace, readers, &published, fixture.Focus)
	first, err := json.Marshal(ProjectWorkspace(snapshot))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	second, err := json.Marshal(ProjectWorkspace(snapshot))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("同一份快照两次投影必须逐字节一致")
	}
}

func TestSecondServiceProfileUsesSameGenericProjector(t *testing.T) {
	// 第二个服务只换 Profile：不改 renderer、不加分支。这是"无核心分支接入"的最小证据。
	var published model.Profile
	loadJSON(t, secondServiceProfile, &published)
	published.Normalize()

	identity := serviceobs.ServiceIdentity{
		ServiceID: published.ServiceRef.ServiceID, EntityID: "entity_second_service",
		EntityKey: "application/second", Name: "Second System", Description: "第二个服务",
		Layer: model.EntityLayerApplication, Kind: model.EntityKindBusinessSystem,
		Environment: "production", OwnerTeamID: "team-2", OwnerTeamName: "App Team",
		Tier: model.ServiceTier2, EntityObservedAt: fixtureFactAnchor, UpdatedAt: fixtureFactAnchor,
	}
	readers := &fixtureReaders{
		identity: identity,
		topology: serviceobs.TopologyFacts{
			SnapshotID: "topo_second",
			ObservedAt: fixtureFactAnchor,
			Nodes: []serviceobs.TopologyNode{{
				EntityID: identity.EntityID, EntityKey: identity.EntityKey, Name: identity.Name,
				Layer: model.EntityLayerApplication, Kind: "business_system", ObservedAt: fixtureFactAnchor,
			}},
		},
		domain: serviceobs.DomainFacts{BoundaryEntityIDs: []string{identity.EntityID}},
	}
	assembler := newFixtureAssembler(readers, &published)
	assembler.Observations = serviceobs.NewUnavailableObservationReader()
	assembler.TelemetryConnected = false
	assembler.EvidenceConnected = false

	snapshot, err := assembler.Assemble(stdcontext.Background(), soctx.Request{
		Kind: soctx.KindOverview, ServiceID: identity.ServiceID,
	})
	if err != nil {
		t.Fatalf("第二个服务装配失败: %v", err)
	}
	overview := ProjectOverview(snapshot)
	if err := newValidator(t).ValidateValue(contract.KindPageView, overview); err != nil {
		t.Fatalf("第二个服务的总览必须过同一份 schema: %v", err)
	}
	if len(overview.GoldenSignals) != 4 {
		t.Fatalf("第二个服务同样恒有四张 Golden 卡，实际 %d", len(overview.GoldenSignals))
	}
	if overview.Health.Status != model.HealthUnknown {
		t.Fatalf("未接遥测应 Unknown，实际 %q", overview.Health.Status)
	}
}

func TestDirectorySortsByTriageOrderAndKeepsNullCursor(t *testing.T) {
	rows := []DirectoryRow{
		{
			Identity:   serviceobs.ServiceIdentity{ServiceID: "svc-healthy", EntityID: "e1", EntityKey: "k1", Name: "Healthy", Layer: model.EntityLayerPlatform, Kind: model.EntityKindInfrastructureCapability, Environment: "prod", OwnerTeamID: "t", OwnerTeamName: "Team", Tier: model.ServiceTier1},
			Health:     model.HealthSummary{Status: model.HealthHealthy, ReasonCodes: []string{"all_required_signals_healthy"}, EvaluatedAt: model.NewTimestamp(fixtureNow)},
			DataStatus: model.DataStatus{State: model.DataStateComplete, Sources: []model.SourceStatus{}},
			ObservedAt: fixtureFactAnchor,
		},
		{
			Identity:   serviceobs.ServiceIdentity{ServiceID: "svc-unknown", EntityID: "e2", EntityKey: "k2", Name: "Unknown", Layer: model.EntityLayerApplication, Kind: model.EntityKindBusinessSystem, Environment: "prod", OwnerTeamID: "t", OwnerTeamName: "Team", Tier: model.ServiceTier2},
			Health:     model.HealthSummary{Status: model.HealthUnknown, ReasonCodes: []string{"profile_unconfigured"}, EvaluatedAt: model.NewTimestamp(fixtureNow)},
			DataStatus: model.DataStatus{State: model.DataStateUnconfigured, Sources: []model.SourceStatus{}},
			ObservedAt: fixtureFactAnchor,
		},
		{
			Identity:     serviceobs.ServiceIdentity{ServiceID: "svc-critical", EntityID: "e3", EntityKey: "k3", Name: "Critical", Layer: model.EntityLayerPlatform, Kind: model.EntityKindInfrastructureCapability, Environment: "prod", OwnerTeamID: "t", OwnerTeamName: "Team", Tier: model.ServiceTier0},
			Health:       model.HealthSummary{Status: model.HealthCritical, ReasonCodes: []string{"signal_critical:x"}, EvaluatedAt: model.NewTimestamp(fixtureNow)},
			DataStatus:   model.DataStatus{State: model.DataStatePartial, Sources: []model.SourceStatus{}},
			DomainCounts: model.DomainCounts{ActiveFaults: 2, ConfirmedImpacts: 1},
			ObservedAt:   fixtureFactAnchor,
		},
	}
	directory := ProjectDirectory(DirectoryInput{
		RequestID: "req_directory_test", AssembledAt: fixtureNow,
		Rows: rows, Summary: SummaryOf(rows),
	})
	if err := newValidator(t).ValidateValue(contract.KindPageView, directory); err != nil {
		t.Fatalf("目录必须过 schema: %v", err)
	}
	want := []string{"svc-critical", "svc-unknown", "svc-healthy"}
	for i, id := range want {
		if directory.Items[i].Service.ServiceID != id {
			t.Fatalf("分诊排序错误: %+v", directory.Items)
		}
	}
	// 缺数不是健康，但也不能压过真实 Degraded/Critical。
	if directory.Summary.Managed != 3 || directory.Summary.Critical != 1 ||
		directory.Summary.Partial != 1 || directory.Summary.Unconfigured != 1 {
		t.Fatalf("摘要统计不符: %+v", directory.Summary)
	}
	// 没有下一页必须是 null，空字符串不是合法表达。
	if directory.NextCursor != nil {
		t.Fatalf("没有下一页时 next_cursor 必须是 null，实际 %v", *directory.NextCursor)
	}
	// 目录占位/摘要的 observed_at 必须是真实事实时间。
	if directory.Items[0].ObservedAt != model.NewTimestamp(fixtureFactAnchor) {
		t.Fatalf("目录行 observed_at 必须是真实事实时间，实际 %q", directory.Items[0].ObservedAt)
	}
}

func TestFormatSignalValueNeverFakesZero(t *testing.T) {
	cases := []struct {
		value *float64
		unit  string
		want  string
	}{
		{nil, "percent", DisplayUnknown},
		{floatPtr(3820), "ms", "3.82s"},
		{floatPtr(480), "ms", "480ms"},
		{floatPtr(6.4), "percent", "6.4%"},
		{floatPtr(18), "req/s", "18 req/s"},
		{floatPtr(0), "percent", "0%"},
	}
	for _, test := range cases {
		if got := formatSignalValue(test.value, test.unit); got != test.want {
			t.Fatalf("formatSignalValue(%v, %q) = %q, want %q", test.value, test.unit, got, test.want)
		}
	}
}

// ---- 测试辅助 ----

type staticError string

func (e staticError) Error() string { return string(e) }

const (
	errTopologyUnavailable staticError = "topology unavailable"
	errDomainUnavailable   staticError = "domain unavailable"
)

func floatPtr(value float64) *float64 { return &value }

func equalFloatPtr(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func idsOfNodes(nodes []model.TopologyNodeView) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.EntityID)
	}
	return ids
}

func idsOfContextNodes(nodes []model.ContextNode) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.EntityID)
	}
	return ids
}

func idsOfRelations(relations []model.TopologyRelationView) []string {
	ids := make([]string, 0, len(relations))
	for _, relation := range relations {
		ids = append(ids, relation.RelationID)
	}
	return ids
}

func idsOfContextRelations(relations []model.ContextRelation) []string {
	ids := make([]string, 0, len(relations))
	for _, relation := range relations {
		ids = append(ids, relation.RelationID)
	}
	return ids
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string{}, left...), append([]string{}, right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func equalDomainRefs(left, right []model.DomainRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func containsBindingKey(signals []model.Signal, key string) bool {
	for _, signal := range signals {
		if signal.BindingKey == key {
			return true
		}
	}
	return false
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func findSection(sections []model.SectionStatus, key string) *model.SectionStatus {
	for i := range sections {
		if sections[i].SectionKey == key {
			return &sections[i]
		}
	}
	return nil
}
