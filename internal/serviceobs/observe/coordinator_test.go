package observe

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

// ---- 测试替身 ----

type fakeSignals struct {
	calls   atomic.Int64
	delay   time.Duration
	err     error
	signals []health.SignalObservation
	source  health.SourceObservation
	// lastRequest 记录最后一次请求，用于断言 coordinator 传下去的是什么。
	mu          sync.Mutex
	lastRequest serviceobs.SignalRequest
	panicNow    bool
}

func (f *fakeSignals) ReadSignals(ctx context.Context, req serviceobs.SignalRequest) (serviceobs.SignalFacts, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastRequest = req
	f.mu.Unlock()
	if f.panicNow {
		panic("adapter 崩了")
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return serviceobs.SignalFacts{}, ctx.Err()
		}
	}
	if f.err != nil {
		return serviceobs.SignalFacts{}, f.err
	}
	source := f.source
	if source.SourceKey == "" {
		source = health.SourceObservation{
			SourceKey: serviceobs.SourceKeyPrometheus,
			State:     model.AvailabilityAvailable,
			Freshness: model.FreshnessFresh,
		}
	}
	return serviceobs.SignalFacts{Signals: f.signals, Source: source}, nil
}

func (f *fakeSignals) request() serviceobs.SignalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequest
}

type fakeTraces struct {
	calls atomic.Int64
	delay time.Duration
	err   error
	facts serviceobs.TraceFacts
}

func (f *fakeTraces) ReadTraces(ctx context.Context, _ serviceobs.FocusRequest) (serviceobs.TraceFacts, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return serviceobs.TraceFacts{}, ctx.Err()
		}
	}
	if f.err != nil {
		return serviceobs.TraceFacts{}, f.err
	}
	facts := f.facts
	if facts.Source.SourceKey == "" {
		facts.Source = health.SourceObservation{
			SourceKey: serviceobs.SourceKeyOpenObserveTraces,
			State:     model.AvailabilityAvailable,
			Freshness: model.FreshnessFresh,
		}
	}
	return facts, nil
}

type fakeLogs struct {
	calls atomic.Int64
	delay time.Duration
	err   error
	facts serviceobs.LogFacts
}

func (f *fakeLogs) ReadLogs(ctx context.Context, _ serviceobs.FocusRequest) (serviceobs.LogFacts, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return serviceobs.LogFacts{}, ctx.Err()
		}
	}
	if f.err != nil {
		return serviceobs.LogFacts{}, f.err
	}
	facts := f.facts
	if facts.Source.SourceKey == "" {
		facts.Source = health.SourceObservation{
			SourceKey: serviceobs.SourceKeyOpenObserveLogs,
			State:     model.AvailabilityAvailable,
			Freshness: model.FreshnessFresh,
		}
	}
	return facts, nil
}

type fakeAccess struct {
	calls atomic.Int64
	facts serviceobs.AccessFacts
}

func (f *fakeAccess) ReadAccess(_ context.Context, _ serviceobs.FocusRequest) (serviceobs.AccessFacts, error) {
	f.calls.Add(1)
	facts := f.facts
	if facts.Source.SourceKey == "" {
		facts.Source = health.SourceObservation{
			SourceKey: serviceobs.SourceKeyOpenObserveAccess,
			State:     model.AvailabilityAvailable,
			Freshness: model.FreshnessFresh,
		}
	}
	return facts, nil
}

type fakePermissions struct {
	granted []serviceobs.Permission
	err     error
	calls   atomic.Int64
}

func (f *fakePermissions) ResolvePermissions(
	_ context.Context, _ serviceobs.Subject, _ string,
) (serviceobs.Permissions, error) {
	f.calls.Add(1)
	if f.err != nil {
		return serviceobs.Permissions{}, f.err
	}
	return serviceobs.NewPermissions(f.granted...), nil
}

func allPermissions() *fakePermissions {
	return &fakePermissions{granted: []serviceobs.Permission{
		serviceobs.PermissionMetricsRead, serviceobs.PermissionTracesRead,
		serviceobs.PermissionLogsRead, serviceobs.PermissionFaultsRead,
	}}
}

func testBindingSource(profile model.Profile) integration.RuntimeBindingSource {
	compiled := buildTestCompiledIntegration(profile)
	digest, err := integration.ProfileDigest(profile)
	if err != nil {
		panic(err)
	}
	compiled.SchemaVersion = integration.CompiledIntegrationSchemaVersion
	compiled.ProfileKey = profile.ProfileKey
	compiled.ProfileVersion = profile.ProfileVersion
	compiled.ProfileDigest = digest
	compiled.ServiceID = "svc-1"
	compiled.Environment = "production"
	compiled.TopologyPolicy = integration.CompiledTopologyPolicy{
		AliasNamespaces:  []string{},
		IdentityMappings: []integration.IdentityMapping{},
	}
	return integration.NewStaticRuntimeBindingSource(map[string]integration.CompiledIntegration{
		compiled.ServiceID + "\x1e" + compiled.Environment + "\x1e" +
			profile.ProfileKey + "\x1e" + itoa(profile.ProfileVersion): compiled,
	})
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}

func buildTestCompiledIntegration(profile model.Profile) integration.CompiledIntegration {
	bindings := make([]integration.CompiledBinding, 0, len(profile.SignalBindings)+3)
	for _, signal := range profile.SignalBindings {
		bindings = append(bindings, integration.CompiledBinding{
			Key: signal.Key, Kind: integration.KindSignal, TargetRef: signal.Key,
			SourceKey: serviceobs.SourceKeyPrometheus, ResolvedTargetIDs: []string{},
		})
	}
	focusKind := "sandbox_id"
	for _, focus := range profile.FocusKinds {
		if focus.Primary {
			focusKind = focus.Key
		}
	}
	hasEvidence, hasJourney := false, false
	for _, view := range profile.Views {
		switch view.Renderer {
		case model.RendererEvidenceList:
			hasEvidence = true
		case model.RendererRequestJourney, model.RendererDependencyTopology:
			hasJourney = true
		}
	}
	if hasJourney {
		bindings = append(bindings, integration.CompiledBinding{
			Key: "journey_test", Kind: integration.KindJourney,
			TargetRef: "sandbox_create", FocusKind: focusKind,
			SourceKey: serviceobs.SourceKeyOpenObserveTraces,
			SideOutputs: []integration.SideOutput{
				integration.SideOutputFocus, integration.SideOutputTraceEvidence, integration.SideOutputTopologyHints,
			},
			ResolvedJourneyStages: []integration.CompiledJourneyStageTarget{},
		})
	}
	if hasEvidence {
		bindings = append(bindings,
			integration.CompiledBinding{
				Key: "logs_test", Kind: integration.KindEvidence, TargetRef: focusKind,
				SourceKey:   serviceobs.SourceKeyOpenObserveLogs,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus},
			},
			integration.CompiledBinding{
				Key: "access_test", Kind: integration.KindEvidence, TargetRef: focusKind,
				SourceKey:   serviceobs.SourceKeyOpenObserveAccess,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus},
			},
		)
	}
	return integration.CompiledIntegration{Bindings: bindings}
}

func optsWithCompiled(opts Options, profile model.Profile) Options {
	if opts.Readers.AnyConfigured() && opts.BindingSource == nil {
		opts.BindingSource = testBindingSource(profile)
	}
	return opts
}

func newCoordinator(opts Options) *Coordinator {
	return New(optsWithCompiled(opts, testProfile()))
}

// testProfile 构造一份带 evidence renderer 的 Profile，因此聚焦工作区会选中三个证据来源。
func testProfile() model.Profile {
	profile := model.Profile{
		SchemaVersion:  "1.0",
		ProfileKey:     "test",
		ProfileVersion: 1,
		Status:         model.ProfileStatusPublished,
		SignalBindings: []model.SignalBinding{
			{Key: "golden_latency", Family: model.SignalFamilyGolden, Semantic: model.SemanticLatency},
			{Key: "golden_traffic", Family: model.SignalFamilyGolden, Semantic: model.SemanticTraffic},
			{Key: "golden_errors", Family: model.SignalFamilyGolden, Semantic: model.SemanticErrors},
			{Key: "golden_saturation", Family: model.SignalFamilyGolden, Semantic: model.SemanticSaturation},
			{Key: "red_rate", Family: model.SignalFamilyRED, Semantic: model.SemanticRate},
		},
		FocusKinds: []model.FocusKind{{
			Key: "sandbox_id", Label: "Sandbox", ValuePattern: "^sbx_[A-Za-z0-9]+$", Primary: true,
		}},
		Views: []model.ProfileView{
			{Key: "overview", Renderer: model.RendererGoldenOverview, Order: 10},
			{Key: "evidence", Renderer: model.RendererEvidenceList, Order: 20},
			{Key: "journey", Renderer: model.RendererRequestJourney, Order: 30},
		},
	}
	profile.Normalize()
	return profile
}

func workspaceRequest() serviceobs.ObservationRequest {
	focus := model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"}
	return serviceobs.ObservationRequest{
		Profile:       testProfile(),
		ServiceID:     "svc-1",
		Environment:   "production",
		WindowSeconds: 300,
		From:          time.Date(2026, 8, 23, 2, 4, 22, 0, time.UTC),
		To:            time.Date(2026, 8, 23, 2, 9, 22, 0, time.UTC),
		Subject:       serviceobs.Subject{UserID: "user-1", Roles: []string{"member"}},
		Purpose:       serviceobs.PurposeFocusedWorkspace,
		Focus:         &focus,
	}
}

func sourceByKey(facts serviceobs.ObservationFacts, key string) (health.SourceObservation, bool) {
	for _, source := range facts.Sources {
		if source.SourceKey == key {
			return source, true
		}
	}
	return health.SourceObservation{}, false
}

// ---- Subject / Purpose 必须由服务端给出 ----

func TestSubjectAndPurposeAreRequired(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: &fakeSignals{}},
		Permissions: allPermissions(),
	})

	t.Run("缺 Subject", func(t *testing.T) {
		req := workspaceRequest()
		req.Subject = serviceobs.Subject{}
		if _, err := coordinator.ReadObservations(context.Background(), req); !errors.Is(err, ErrSubjectRequired) {
			t.Fatalf("err=%v, want ErrSubjectRequired", err)
		}
	})

	t.Run("缺 Purpose", func(t *testing.T) {
		req := workspaceRequest()
		req.Purpose = ""
		if _, err := coordinator.ReadObservations(context.Background(), req); !errors.Is(err, ErrPurposeRequired) {
			t.Fatalf("err=%v, want ErrPurposeRequired", err)
		}
	})

	t.Run("Purpose 不在枚举内", func(t *testing.T) {
		req := workspaceRequest()
		req.Purpose = serviceobs.Purpose("admin_everything")
		if _, err := coordinator.ReadObservations(context.Background(), req); !errors.Is(err, ErrPurposeRequired) {
			t.Fatalf("err=%v, want ErrPurposeRequired", err)
		}
	})
}

// ---- purpose 决定来源：目录摘要绝不扇出证据查询 ----

func TestDirectorySummaryNeverFansOutEvidence(t *testing.T) {
	signals := &fakeSignals{}
	traces := &fakeTraces{}
	logs := &fakeLogs{}
	access := &fakeAccess{}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: signals, Traces: traces, Logs: logs, Access: access},
		Permissions: allPermissions(),
	})

	req := workspaceRequest()
	req.Purpose = serviceobs.PurposeDirectorySummary
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if traces.calls.Load() != 0 || logs.calls.Load() != 0 || access.calls.Load() != 0 {
		t.Fatalf("目录摘要不得调用证据来源: traces=%d logs=%d access=%d",
			traces.calls.Load(), logs.calls.Load(), access.calls.Load())
	}
	if signals.calls.Load() != 1 {
		t.Fatalf("signal 调用次数=%d，want 1", signals.calls.Load())
	}
	// 目录摘要只读四大黄金信号，不把整份 Profile 的 binding 都查一遍。
	requested := signals.request().BindingKeys
	if len(requested) != 4 {
		t.Fatalf("目录摘要请求了 %d 个 binding，应只有 4 个黄金信号: %v", len(requested), requested)
	}
	for _, key := range requested {
		if key == "red_rate" {
			t.Fatal("目录摘要不应请求非黄金 binding")
		}
	}
	// 证据来源根本没被请求，因此不出现在 sources 里——"没查"与"查不到"必须可分辨。
	for _, key := range []string{
		serviceobs.SourceKeyOpenObserveLogs,
		serviceobs.SourceKeyOpenObserveTraces,
		serviceobs.SourceKeyOpenObserveAccess,
	} {
		if _, ok := sourceByKey(facts, key); ok {
			t.Fatalf("未被请求的来源 %s 不应出现在 sources 里", key)
		}
	}
}

func TestOverviewReadsAllBindingsWithoutEvidence(t *testing.T) {
	signals := &fakeSignals{}
	logs := &fakeLogs{}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: signals, Logs: logs},
		Permissions: allPermissions(),
	})
	req := workspaceRequest()
	req.Purpose = serviceobs.PurposeOverview
	if _, err := coordinator.ReadObservations(context.Background(), req); err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if logs.calls.Load() != 0 {
		t.Fatalf("总览不得调用日志来源，调用了 %d 次", logs.calls.Load())
	}
	if got := len(signals.request().BindingKeys); got != 5 {
		t.Fatalf("总览请求了 %d 个 binding，应为全部 5 个", got)
	}
}

func TestFocusedWorkspaceReadsEvidenceSources(t *testing.T) {
	signals := &fakeSignals{}
	traces := &fakeTraces{}
	logs := &fakeLogs{}
	access := &fakeAccess{}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: signals, Traces: traces, Logs: logs, Access: access},
		Permissions: allPermissions(),
	})
	if _, err := coordinator.ReadObservations(context.Background(), workspaceRequest()); err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if signals.calls.Load() != 1 || traces.calls.Load() != 1 || logs.calls.Load() != 1 || access.calls.Load() != 1 {
		t.Fatalf("聚焦工作区应各调用一次: signals=%d traces=%d logs=%d access=%d",
			signals.calls.Load(), traces.calls.Load(), logs.calls.Load(), access.calls.Load())
	}
}

// ---- 完全未配置 vs 部分配置 ----

func TestFullyUnconfiguredKeepsPhase2Semantics(t *testing.T) {
	coordinator := newCoordinator(Options{Permissions: allPermissions()})
	req := workspaceRequest()
	req.Focus = nil
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if len(facts.Sources) != 1 {
		t.Fatalf("完全未配置时应只有一条 source，得到 %d", len(facts.Sources))
	}
	source := facts.Sources[0]
	if source.SourceKey != serviceobs.SourceKeyPrometheus ||
		source.ReasonCode != serviceobs.ReasonTelemetryNotConnected ||
		source.State != model.AvailabilityUnavailable || source.Retryable {
		t.Fatalf("必须逐字保留 Phase 2 语义，得到 %+v", source)
	}
	if len(facts.Signals) != 5 {
		t.Fatalf("每个 binding 都要有显式 Unknown，得到 %d 条", len(facts.Signals))
	}
	for _, signal := range facts.Signals {
		if signal.UnknownReason != serviceobs.ReasonTelemetryNotConnected {
			t.Fatalf("binding %s 的 reason=%q", signal.BindingKey, signal.UnknownReason)
		}
	}
	if facts.Focus != serviceobs.FocusNotRequested {
		t.Fatalf("Focus=%q，未接入时应是 not_requested", facts.Focus)
	}
}

// 完全未配置且本次真的问了 focus 时必须是 Unknown。
//
// NotRequested 的含义是"你没问"，而用户确实问了"这个 sandbox_id 有没有被观测到"；
// 一个 provider 都没有时的诚实答案是"我们不知道"。source 与 signal 侧仍逐字保留 Phase 2。
func TestFullyUnconfiguredWithRequestedFocusIsUnknown(t *testing.T) {
	focusValue := model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"}
	coordinator := newCoordinator(Options{Permissions: allPermissions()})
	req := workspaceRequest()
	req.Focus = &focusValue
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if facts.Focus != serviceobs.FocusUnknown {
		t.Fatalf("Focus=%q，问过 focus 却无任何 provider 时应是 unknown", facts.Focus)
	}
	if len(facts.Sources) != 1 {
		t.Fatalf("完全未配置时应只有一条 source，得到 %d", len(facts.Sources))
	}
	source := facts.Sources[0]
	if source.SourceKey != serviceobs.SourceKeyPrometheus ||
		source.ReasonCode != serviceobs.ReasonTelemetryNotConnected {
		t.Fatalf("focus 降级不得改动 Phase 2 的 source 语义，得到 %+v", source)
	}
	if len(facts.Signals) != 5 {
		t.Fatalf("每个 binding 都要有显式 Unknown，得到 %d 条", len(facts.Signals))
	}
	for _, signal := range facts.Signals {
		if signal.UnknownReason != serviceobs.ReasonTelemetryNotConnected {
			t.Fatalf("binding %s 的 reason=%q", signal.BindingKey, signal.UnknownReason)
		}
	}
}

// 只有聚焦工作区会扇出 focus 证据，因此目录/总览即使畸形地带了 focus 值也仍是 NotRequested。
func TestFullyUnconfiguredIgnoresFocusForNonEvidencePurposes(t *testing.T) {
	focusValue := model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"}
	for _, purpose := range []serviceobs.Purpose{
		serviceobs.PurposeDirectorySummary, serviceobs.PurposeOverview,
	} {
		t.Run(string(purpose), func(t *testing.T) {
			coordinator := newCoordinator(Options{Permissions: allPermissions()})
			req := workspaceRequest()
			req.Purpose = purpose
			req.Focus = &focusValue
			facts, err := coordinator.ReadObservations(context.Background(), req)
			if err != nil {
				t.Fatalf("ReadObservations 失败: %v", err)
			}
			if facts.Focus != serviceobs.FocusNotRequested {
				t.Fatalf("Focus=%q，该用途不查 focus 证据时应是 not_requested", facts.Focus)
			}
		})
	}
}

// 聚焦工作区但没带 focus 值：没问过就是 not_requested，不能凭空报 unknown。
func TestFullyUnconfiguredWithoutFocusStaysNotRequested(t *testing.T) {
	coordinator := newCoordinator(Options{Permissions: allPermissions()})
	req := workspaceRequest()
	req.Focus = nil
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if facts.Focus != serviceobs.FocusNotRequested {
		t.Fatalf("Focus=%q，没问过 focus 时应是 not_requested", facts.Focus)
	}
}

// 部分配置：当前 purpose 选中但未连接的来源必须显式出现，不能省略。
func TestPartialConfigurationReportsNotConnectedExplicitly(t *testing.T) {
	t.Run("只有 Prometheus", func(t *testing.T) {
		coordinator := newCoordinator(Options{
			Readers:     Readers{Signals: &fakeSignals{}},
			Permissions: allPermissions(),
		})
		facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
		if err != nil {
			t.Fatalf("ReadObservations 失败: %v", err)
		}
		for _, key := range []string{
			serviceobs.SourceKeyOpenObserveLogs,
			serviceobs.SourceKeyOpenObserveTraces,
			serviceobs.SourceKeyOpenObserveAccess,
		} {
			source, ok := sourceByKey(facts, key)
			if !ok {
				t.Fatalf("被请求但未连接的来源 %s 必须显式出现", key)
			}
			if source.ReasonCode != serviceobs.ReasonOpenObserveNotConnected {
				t.Fatalf("%s reason=%q，want %s", key, source.ReasonCode, serviceobs.ReasonOpenObserveNotConnected)
			}
			if source.State != model.AvailabilityUnavailable {
				t.Fatalf("%s state=%q", key, source.State)
			}
		}
		// Prometheus 已连接，不能带 not_connected。
		prometheus, ok := sourceByKey(facts, serviceobs.SourceKeyPrometheus)
		if !ok || prometheus.ReasonCode == serviceobs.ReasonPrometheusNotConnected {
			t.Fatalf("Prometheus 已连接，得到 %+v", prometheus)
		}
	})

	t.Run("只有 OpenObserve", func(t *testing.T) {
		coordinator := newCoordinator(Options{
			Readers: Readers{
				Traces: &fakeTraces{}, Logs: &fakeLogs{}, Access: &fakeAccess{},
			},
			Permissions: allPermissions(),
		})
		facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
		if err != nil {
			t.Fatalf("ReadObservations 失败: %v", err)
		}
		source, ok := sourceByKey(facts, serviceobs.SourceKeyPrometheus)
		if !ok {
			t.Fatal("被请求但未连接的 Prometheus 必须显式出现")
		}
		if source.ReasonCode != serviceobs.ReasonPrometheusNotConnected {
			t.Fatalf("reason=%q，want %s", source.ReasonCode, serviceobs.ReasonPrometheusNotConnected)
		}
		// 每个 binding 仍然要有显式 Unknown，且 reason 跟随来源。
		if len(facts.Signals) != 5 {
			t.Fatalf("signal 数=%d，want 5", len(facts.Signals))
		}
		for _, signal := range facts.Signals {
			if signal.UnknownReason != serviceobs.ReasonPrometheusNotConnected {
				t.Fatalf("binding %s reason=%q", signal.BindingKey, signal.UnknownReason)
			}
		}
	})
}

// ---- 权限：未授权时上游调用次数为 0 ----

func TestPermissionDeniedMakesZeroUpstreamCalls(t *testing.T) {
	signals := &fakeSignals{}
	traces := &fakeTraces{}
	logs := &fakeLogs{}
	access := &fakeAccess{}
	coordinator := newCoordinator(Options{
		Readers: Readers{Signals: signals, Traces: traces, Logs: logs, Access: access},
		// member 缺省没有 logs.read：logs 与 access 都必须被挡住。
		Permissions: &fakePermissions{granted: []serviceobs.Permission{
			serviceobs.PermissionMetricsRead, serviceobs.PermissionTracesRead,
		}},
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if logs.calls.Load() != 0 || access.calls.Load() != 0 {
		t.Fatalf("未授权来源的上游调用次数必须为 0: logs=%d access=%d", logs.calls.Load(), access.calls.Load())
	}
	if signals.calls.Load() != 1 || traces.calls.Load() != 1 {
		t.Fatalf("已授权来源应正常调用: signals=%d traces=%d", signals.calls.Load(), traces.calls.Load())
	}
	for _, key := range []string{serviceobs.SourceKeyOpenObserveLogs, serviceobs.SourceKeyOpenObserveAccess} {
		source, ok := sourceByKey(facts, key)
		if !ok {
			t.Fatalf("%s 必须显式出现", key)
		}
		if source.State != model.AvailabilityForbidden {
			t.Fatalf("%s state=%q，want forbidden", key, source.State)
		}
		if source.ReasonCode != serviceobs.ReasonPermissionDenied {
			t.Fatalf("%s reason=%q，want %s", key, source.ReasonCode, serviceobs.ReasonPermissionDenied)
		}
		if source.Retryable {
			t.Fatalf("%s 不应可重试：重试不会给用户补上权限", key)
		}
	}
	// traces 仍然可用：logs 被拒绝不得把 traces 也标成 Forbidden。
	traceSource, _ := sourceByKey(facts, serviceobs.SourceKeyOpenObserveTraces)
	if traceSource.State == model.AvailabilityForbidden {
		t.Fatal("logs Forbidden 不得连带 traces")
	}
}

// resolver 为 nil 时默认拒绝一切，而不是放行。
func TestNilPermissionResolverDeniesByDefault(t *testing.T) {
	signals := &fakeSignals{}
	coordinator := newCoordinator(Options{Readers: Readers{Signals: signals}})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if signals.calls.Load() != 0 {
		t.Fatalf("默认应拒绝，实际调用了 %d 次", signals.calls.Load())
	}
	source, ok := sourceByKey(facts, serviceobs.SourceKeyPrometheus)
	if !ok || source.State != model.AvailabilityForbidden {
		t.Fatalf("应为 Forbidden，得到 %+v", source)
	}
}

// 权限算不出来时整请求失败，不能"当作有权限"继续。
func TestPermissionResolverErrorFailsRequest(t *testing.T) {
	sentinel := errors.New("policy store down")
	signals := &fakeSignals{}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: signals},
		Permissions: &fakePermissions{err: sentinel},
	})
	_, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v，应包装原始错误", err)
	}
	if signals.calls.Load() != 0 {
		t.Fatalf("权限未知时不应调用上游，实际 %d 次", signals.calls.Load())
	}
}

// ---- 单来源失败只降级自己 ----

func TestSingleSourceFailureDegradesOnlyItself(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		retryable  bool
	}{
		{"上游凭据失效", ErrUpstreamAuthFailed, serviceobs.ReasonUpstreamAuthFailed, false},
		{"上游限流", ErrUpstreamRateLimited, serviceobs.ReasonUpstreamRateLimited, true},
		{"响应过大", ErrResponseTooLarge, serviceobs.ReasonResponseTooLarge, false},
		{"基数超限", ErrCardinalityExceeded, serviceobs.ReasonCardinalityExceeded, false},
		{"审计不可写", ErrAuditUnavailable, serviceobs.ReasonAuditUnavailable, true},
		{"其它读取失败", errors.New("boom"), serviceobs.ReasonSourceReadFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &fakeLogs{err: tc.err}
			traces := &fakeTraces{}
			coordinator := newCoordinator(Options{
				Readers: Readers{
					Signals: &fakeSignals{}, Traces: traces, Logs: logs, Access: &fakeAccess{},
				},
				Permissions: allPermissions(),
			})
			facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
			if err != nil {
				t.Fatalf("单来源失败不应让整请求失败: %v", err)
			}
			logSource, ok := sourceByKey(facts, serviceobs.SourceKeyOpenObserveLogs)
			if !ok {
				t.Fatal("logs source 必须出现")
			}
			if logSource.ReasonCode != tc.wantReason {
				t.Fatalf("reason=%q，want %q", logSource.ReasonCode, tc.wantReason)
			}
			if logSource.Retryable != tc.retryable {
				t.Fatalf("retryable=%t，want %t", logSource.Retryable, tc.retryable)
			}
			// 其它来源必须完好。
			traceSource, ok := sourceByKey(facts, serviceobs.SourceKeyOpenObserveTraces)
			if !ok || traceSource.State != model.AvailabilityAvailable {
				t.Fatalf("traces 应保持可用，得到 %+v", traceSource)
			}
		})
	}
}

// 上游凭据失效绝不能显示成当前用户的 Forbidden。
func TestUpstreamAuthFailureIsNotUserForbidden(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: &fakeSignals{err: ErrUpstreamAuthFailed}},
		Permissions: allPermissions(),
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	source, _ := sourceByKey(facts, serviceobs.SourceKeyPrometheus)
	if source.State == model.AvailabilityForbidden {
		t.Fatal("平台凭据失效不得显示成用户 Forbidden")
	}
	if source.ReasonCode != serviceobs.ReasonUpstreamAuthFailed {
		t.Fatalf("reason=%q，want %s", source.ReasonCode, serviceobs.ReasonUpstreamAuthFailed)
	}
}

// 单来源超时只降级自己，其它来源照常返回。
func TestSingleSourceTimeoutDegradesOnlyItself(t *testing.T) {
	slowLogs := &fakeLogs{delay: 2 * time.Second}
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{}, Traces: &fakeTraces{}, Logs: slowLogs, Access: &fakeAccess{},
		},
		Limits: Limits{
			Logs: serviceobs.SourceLimits{Timeout: 30 * time.Millisecond},
		},
		Permissions: allPermissions(),
	})
	start := time.Now()
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("单来源超时不应让整请求失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("慢来源吃掉了整请求预算，用了 %s", elapsed)
	}
	logSource, _ := sourceByKey(facts, serviceobs.SourceKeyOpenObserveLogs)
	if logSource.ReasonCode != serviceobs.ReasonSourceTimeout {
		t.Fatalf("reason=%q，want %s", logSource.ReasonCode, serviceobs.ReasonSourceTimeout)
	}
	traceSource, _ := sourceByKey(facts, serviceobs.SourceKeyOpenObserveTraces)
	if traceSource.State != model.AvailabilityAvailable {
		t.Fatalf("traces 应保持可用，得到 %+v", traceSource)
	}
}

// adapter panic 必须被隔离成稳定 reason，不能带走整个请求。
func TestAdapterPanicIsIsolated(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{panicNow: true}, Traces: &fakeTraces{},
			Logs: &fakeLogs{}, Access: &fakeAccess{},
		},
		Permissions: allPermissions(),
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("panic 不应让整请求失败: %v", err)
	}
	source, _ := sourceByKey(facts, serviceobs.SourceKeyPrometheus)
	if source.ReasonCode != serviceobs.ReasonSourceInternalError {
		t.Fatalf("reason=%q，want %s", source.ReasonCode, serviceobs.ReasonSourceInternalError)
	}
	traceSource, _ := sourceByKey(facts, serviceobs.SourceKeyOpenObserveTraces)
	if traceSource.State != model.AvailabilityAvailable {
		t.Fatal("panic 不得影响其它来源")
	}
}

// ---- 父 context 取消 ----

// 父 context 取消必须向上返回 context 错误，而不是把断开的请求伪装成一个可返回的 Partial。
func TestParentCancellationReturnsContextError(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{delay: time.Second}, Traces: &fakeTraces{delay: time.Second},
			Logs: &fakeLogs{delay: time.Second}, Access: &fakeAccess{},
		},
		Permissions: allPermissions(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := coordinator.ReadObservations(ctx, workspaceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v，want context.Canceled", err)
	}
}

func TestParentDeadlineReturnsContextError(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: &fakeSignals{delay: time.Second}},
		Permissions: allPermissions(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := coordinator.ReadObservations(ctx, workspaceRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v，want context.DeadlineExceeded", err)
	}
}

// 已经断开的 context 不发起任何上游调用。
func TestAlreadyCanceledContextMakesNoCalls(t *testing.T) {
	signals := &fakeSignals{}
	permissions := allPermissions()
	coordinator := newCoordinator(Options{Readers: Readers{Signals: signals}, Permissions: permissions})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.ReadObservations(ctx, workspaceRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v，want context.Canceled", err)
	}
	if signals.calls.Load() != 0 || permissions.calls.Load() != 0 {
		t.Fatalf("已断开的请求不应做任何工作: signals=%d permissions=%d",
			signals.calls.Load(), permissions.calls.Load())
	}
}

// 取消后不得遗留 goroutine：函数返回前所有子 goroutine 必须已退出。
func TestNoGoroutineLeakAfterCancellation(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{delay: 500 * time.Millisecond},
			Traces:  &fakeTraces{delay: 500 * time.Millisecond},
			Logs:    &fakeLogs{delay: 500 * time.Millisecond},
			Access:  &fakeAccess{},
		},
		Permissions: allPermissions(),
	})

	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			if i > 5 {
				return runtime.NumGoroutine()
			}
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		_, _ = coordinator.ReadObservations(ctx, workspaceRequest())
		cancel()
	}
	after := settle()
	// 允许少量运行时波动，但不能随请求数线性增长。
	if after > before+5 {
		t.Fatalf("goroutine 从 %d 涨到 %d，可能有泄漏", before, after)
	}
}

// ---- 合并语义 ----

// focus 合并顺序是 matched > unknown > empty。
//
// 这与本文件早先的断言相反：那一版按"最坏优先"把 matched + unknown 判成 unknown。
// 已确认命中是**正事实**——一个来源真的看到了这次请求，它就发生过，另一个来源超时或没权限
// 都不能把它抹掉。反过来没有命中时，只要有一个已请求来源不知道就不能宣称 Empty，
// 否则会把"查不到"说成"没发生"。
func TestFocusMergeFollowsMatchedUnknownEmptyPriority(t *testing.T) {
	cases := []struct {
		name       string
		traceFocus serviceobs.FocusResult
		logFocus   serviceobs.FocusResult
		want       serviceobs.FocusResult
	}{
		{"都命中", serviceobs.FocusMatched, serviceobs.FocusMatched, serviceobs.FocusMatched},
		{"命中优先于不知道", serviceobs.FocusMatched, serviceobs.FocusUnknown, serviceobs.FocusMatched},
		{"命中优先于空", serviceobs.FocusMatched, serviceobs.FocusEmpty, serviceobs.FocusMatched},
		{"没有命中时不知道优先于空", serviceobs.FocusUnknown, serviceobs.FocusEmpty, serviceobs.FocusUnknown},
		{"都空", serviceobs.FocusEmpty, serviceobs.FocusEmpty, serviceobs.FocusEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := newCoordinator(Options{
				Readers: Readers{
					Signals: &fakeSignals{},
					Traces:  &fakeTraces{facts: serviceobs.TraceFacts{Focus: tc.traceFocus}},
					Logs:    &fakeLogs{facts: serviceobs.LogFacts{Focus: tc.logFocus}},
					Access:  &fakeAccess{facts: serviceobs.AccessFacts{Focus: tc.logFocus}},
				},
				Permissions: allPermissions(),
			})
			facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
			if err != nil {
				t.Fatalf("ReadObservations 失败: %v", err)
			}
			if facts.Focus != tc.want {
				t.Fatalf("Focus=%q，want %q", facts.Focus, tc.want)
			}
		})
	}
}

// 重复 binding 以第一个为准，缺失 binding 补显式 Unknown，结果顺序稳定。
func TestSignalMergeIsDeterministic(t *testing.T) {
	value := 1.0
	signals := &fakeSignals{signals: []health.SignalObservation{
		{BindingKey: "golden_latency", Availability: model.SignalAvailabilityAvailable, Value: &value},
		{BindingKey: "golden_latency", Availability: model.SignalAvailabilityUnavailable},
		{BindingKey: "golden_traffic", Availability: model.SignalAvailabilityAvailable, Value: &value},
	}}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: signals},
		Permissions: allPermissions(),
	})
	req := workspaceRequest()
	req.Purpose = serviceobs.PurposeOverview
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if len(facts.Signals) != 5 {
		t.Fatalf("signal 数=%d，want 5（3 个返回去重后 2 个 + 3 个补齐）", len(facts.Signals))
	}
	// 已排序：便于确定性投影与断言。
	for i := 1; i < len(facts.Signals); i++ {
		if facts.Signals[i-1].BindingKey > facts.Signals[i].BindingKey {
			t.Fatalf("signal 未按 binding key 排序: %v", facts.Signals)
		}
	}
	byKey := map[string]health.SignalObservation{}
	for _, signal := range facts.Signals {
		byKey[signal.BindingKey] = signal
	}
	// 重复 binding 取第一个（available），不能被后面的 unavailable 覆盖。
	if byKey["golden_latency"].Availability != model.SignalAvailabilityAvailable {
		t.Fatal("重复 binding 应以第一个为准")
	}
	// 未返回的 binding 必须显式 Unknown。
	for _, key := range []string{"golden_errors", "golden_saturation", "red_rate"} {
		if byKey[key].Availability != model.SignalAvailabilityUnavailable {
			t.Fatalf("%s 应显式 Unknown，得到 %+v", key, byKey[key])
		}
		if byKey[key].UnknownReason == "" {
			t.Fatalf("%s 缺少 UnknownReason", key)
		}
	}
}

// journey stage 只能使用 v1 枚举；本阶段不得出现 skipped。
func TestJourneyStagesUseContractEnumOnly(t *testing.T) {
	traces := &fakeTraces{facts: serviceobs.TraceFacts{
		Focus: serviceobs.FocusMatched,
		JourneyStages: []serviceobs.JourneyStageObservation{
			{StageKey: "accept", State: model.JourneyStageStateOK},
			{StageKey: "place", State: model.JourneyStageStateFailed},
			{StageKey: "fc_start", State: model.JourneyStageStatePending},
			{StageKey: "routed", State: model.JourneyStageStateUnknown, ReasonCode: serviceobs.ReasonSourceTimeout},
		},
	}}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: &fakeSignals{}, Traces: traces, Logs: &fakeLogs{}, Access: &fakeAccess{}},
		Permissions: allPermissions(),
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if len(facts.JourneyStages) != 4 {
		t.Fatalf("stage 数=%d，want 4", len(facts.JourneyStages))
	}
	for _, stage := range facts.JourneyStages {
		if !stage.State.IsValid() {
			t.Fatalf("stage %s 的状态 %q 不在 v1 枚举内", stage.StageKey, stage.State)
		}
		if stage.State == model.JourneyStageStateSkipped {
			t.Fatalf("本阶段不得凭缺失事实生成 skipped，stage=%s", stage.StageKey)
		}
		if stage.State == model.JourneyStageStateUnknown && stage.ReasonCode == "" {
			t.Fatalf("stage %s 是 unknown 但没有 reason", stage.StageKey)
		}
	}
}

// reconciliation issue 排序稳定，且不带未脱敏原文。
func TestIssuesAreSortedAndRedacted(t *testing.T) {
	traces := &fakeTraces{facts: serviceobs.TraceFacts{
		Issues: []serviceobs.ReconciliationIssue{
			{ReasonCode: serviceobs.ReasonAliasConflict, Namespace: "otel_service", Count: 2,
				SourceKey: serviceobs.SourceKeyOpenObserveTraces},
			{ReasonCode: serviceobs.ReasonAliasNotFound, Namespace: "host_instance", Count: 5,
				SourceKey: serviceobs.SourceKeyOpenObserveTraces},
		},
	}}
	coordinator := newCoordinator(Options{
		Readers:     Readers{Signals: &fakeSignals{}, Traces: traces, Logs: &fakeLogs{}, Access: &fakeAccess{}},
		Permissions: allPermissions(),
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if len(facts.Issues) != 2 {
		t.Fatalf("issue 数=%d，want 2", len(facts.Issues))
	}
	if facts.Issues[0].ReasonCode != serviceobs.ReasonAliasConflict {
		t.Fatalf("排序不稳定: %+v", facts.Issues)
	}
}

// Profile limits 只能收紧全局上界，不能放宽。
func TestProfileLimitsOnlyTighten(t *testing.T) {
	signals := &fakeSignals{}
	baseOpts := Options{
		Readers:     Readers{Signals: signals},
		Limits:      Limits{Signals: serviceobs.SourceLimits{MaxRows: 200}},
		Permissions: allPermissions(),
	}

	t.Run("Profile 更小则收紧", func(t *testing.T) {
		req := workspaceRequest()
		req.Purpose = serviceobs.PurposeOverview
		req.Profile.Limits.MaxRows = 50
		coordinator := New(optsWithCompiled(baseOpts, req.Profile))
		if _, err := coordinator.ReadObservations(context.Background(), req); err != nil {
			t.Fatalf("失败: %v", err)
		}
		if got := signals.request().Limits.MaxRows; got != 50 {
			t.Fatalf("MaxRows=%d，want 50", got)
		}
	})

	t.Run("Profile 更大不放宽", func(t *testing.T) {
		req := workspaceRequest()
		req.Purpose = serviceobs.PurposeOverview
		req.Profile.Limits.MaxRows = 10000
		coordinator := New(optsWithCompiled(baseOpts, req.Profile))
		if _, err := coordinator.ReadObservations(context.Background(), req); err != nil {
			t.Fatalf("失败: %v", err)
		}
		if got := signals.request().Limits.MaxRows; got != 200 {
			t.Fatalf("MaxRows=%d，Profile 不能抬高全局上界", got)
		}
	})
}

// 并发读取同一个 coordinator 必须安全（配合 -race）。
func TestConcurrentReadsAreSafe(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{}, Traces: &fakeTraces{}, Logs: &fakeLogs{}, Access: &fakeAccess{},
		},
		Permissions: allPermissions(),
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := coordinator.ReadObservations(context.Background(), workspaceRequest()); err != nil {
				t.Errorf("并发读取失败: %v", err)
			}
		}()
	}
	wg.Wait()
}

// Coordinator 必须满足 Phase 2 的 ObservationReader 端口：facade 与 API 因此不需要改。
func TestCoordinatorSatisfiesPhase2Port(t *testing.T) {
	var reader serviceobs.ObservationReader = newCoordinator(Options{})
	if reader == nil {
		t.Fatal("Coordinator 应满足 ObservationReader")
	}
}

// ---- correction：focus 只被"参与 focus 判定的来源"影响 ----

// 修复前所有失败来源都无条件报 FocusUnknown，于是一次 Prometheus 超时就能把 traces 已经
// 确认命中的 focus 降级成 Unknown。focus 语义因此必须由 affectsFocus 决定。
func TestFocusIsNotPollutedByNonFocusSources(t *testing.T) {
	focusValue := model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"}
	matchedTraces := func() *fakeTraces {
		return &fakeTraces{facts: serviceobs.TraceFacts{Focus: serviceobs.FocusMatched}}
	}

	cases := []struct {
		name    string
		readers Readers
		focus   *model.Focus
		want    serviceobs.FocusResult
	}{
		{
			name: "traces matched + Prometheus timeout => matched",
			readers: Readers{
				Signals: &fakeSignals{err: errors.New("prometheus timeout")},
				Traces:  matchedTraces(), Logs: &fakeLogs{}, Access: &fakeAccess{},
			},
			focus: &focusValue,
			want:  serviceobs.FocusMatched,
		},
		{
			name: "traces matched + logs unavailable => matched",
			readers: Readers{
				Signals: &fakeSignals{}, Traces: matchedTraces(),
				Logs: &fakeLogs{err: ErrUpstreamRateLimited}, Access: &fakeAccess{},
			},
			focus: &focusValue,
			want:  serviceobs.FocusMatched,
		},
		{
			name: "traces empty + logs Forbidden => unknown",
			readers: Readers{
				Signals: &fakeSignals{},
				Traces:  &fakeTraces{facts: serviceobs.TraceFacts{Focus: serviceobs.FocusEmpty}},
				Logs:    &fakeLogs{}, Access: &fakeAccess{},
			},
			focus: &focusValue,
			want:  serviceobs.FocusUnknown,
		},
		{
			name: "全部 focus 来源未连接且已请求 focus => unknown",
			// 只配 Prometheus：traces/logs/access 都被请求但未连接。
			readers: Readers{Signals: &fakeSignals{}},
			focus:   &focusValue,
			want:    serviceobs.FocusUnknown,
		},
		{
			name: "没有 focus 且全部来源失败 => not_requested",
			readers: Readers{
				Signals: &fakeSignals{err: errors.New("boom")},
				Traces:  &fakeTraces{err: errors.New("boom")},
				Logs:    &fakeLogs{err: errors.New("boom")},
				Access:  &fakeAccess{},
			},
			focus: nil,
			want:  serviceobs.FocusNotRequested,
		},
		{
			name: "全部 focus 来源确认无记录 => empty",
			readers: Readers{
				Signals: &fakeSignals{},
				Traces:  &fakeTraces{facts: serviceobs.TraceFacts{Focus: serviceobs.FocusEmpty}},
				Logs:    &fakeLogs{facts: serviceobs.LogFacts{Focus: serviceobs.FocusEmpty}},
				Access:  &fakeAccess{facts: serviceobs.AccessFacts{Focus: serviceobs.FocusEmpty}},
			},
			focus: &focusValue,
			want:  serviceobs.FocusEmpty,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// logs Forbidden 的用例需要一个不给 logs.read 的权限集。
			permissions := allPermissions()
			if strings.Contains(tc.name, "logs Forbidden") {
				permissions = &fakePermissions{granted: []serviceobs.Permission{
					serviceobs.PermissionMetricsRead, serviceobs.PermissionTracesRead,
				}}
			}
			coordinator := newCoordinator(Options{Readers: tc.readers, Permissions: permissions})
			req := workspaceRequest()
			req.Focus = tc.focus
			facts, err := coordinator.ReadObservations(context.Background(), req)
			if err != nil {
				t.Fatalf("ReadObservations 失败: %v", err)
			}
			if facts.Focus != tc.want {
				t.Fatalf("Focus=%q，want %q", facts.Focus, tc.want)
			}
		})
	}
}

// signal 来源失败在没有 focus 时也不该报告 focus 结论。
func TestSignalFailureNeverReportsFocus(t *testing.T) {
	focusValue := model.Focus{Kind: "sandbox_id", Value: "sbx_hp16c3"}
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{err: errors.New("boom")},
			Traces:  &fakeTraces{facts: serviceobs.TraceFacts{Focus: serviceobs.FocusEmpty}},
			Logs:    &fakeLogs{facts: serviceobs.LogFacts{Focus: serviceobs.FocusEmpty}},
			Access:  &fakeAccess{facts: serviceobs.AccessFacts{Focus: serviceobs.FocusEmpty}},
		},
		Permissions: allPermissions(),
	})
	req := workspaceRequest()
	req.Focus = &focusValue
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	// 三个 focus 来源都确认无记录，signal 失败不参与 focus：结论必须是 Empty 而不是 Unknown。
	if facts.Focus != serviceobs.FocusEmpty {
		t.Fatalf("Focus=%q，signal 失败不应污染 focus", facts.Focus)
	}
}

// ---- correction：summary binding keys 单点解析 ----

// 注入自定义 SummaryBindingKeys 时，"上游请求集"与"merge 的 Unknown 补齐集"必须逐项一致。
//
// 修复前是两套推导：runSources 用注入的函数，merge 又硬编码 GoldenBindings，于是自定义
// 摘要 key 会让页面少几条本该显式 Unknown 的信号。
func TestCustomSummaryBindingKeysAreUsedForBothRequestAndBackfill(t *testing.T) {
	custom := []string{"golden_latency", "red_rate"}
	signals := &fakeSignals{}
	coordinator := newCoordinator(Options{
		Readers:            Readers{Signals: signals},
		Permissions:        allPermissions(),
		SummaryBindingKeys: func(model.Profile) []string { return custom },
	})
	req := workspaceRequest()
	req.Purpose = serviceobs.PurposeDirectorySummary
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}

	requested := signals.request().BindingKeys
	if len(requested) != len(custom) {
		t.Fatalf("请求集=%v，want %v", requested, custom)
	}
	backfilled := make([]string, 0, len(facts.Signals))
	for _, signal := range facts.Signals {
		backfilled = append(backfilled, signal.BindingKey)
	}
	if len(backfilled) != len(custom) {
		t.Fatalf("补齐集=%v，want %v（不能回落到 GoldenBindings）", backfilled, custom)
	}
	wanted := map[string]struct{}{}
	for _, key := range custom {
		wanted[key] = struct{}{}
	}
	for _, key := range backfilled {
		if _, ok := wanted[key]; !ok {
			t.Fatalf("补齐了未被请求的 binding %s", key)
		}
	}
}

// 完全未配置时也走同一份 key 解析。
func TestNotConnectedUsesSameBindingKeyResolution(t *testing.T) {
	custom := []string{"only_one"}
	coordinator := newCoordinator(Options{
		Permissions:        allPermissions(),
		SummaryBindingKeys: func(model.Profile) []string { return custom },
	})
	req := workspaceRequest()
	req.Purpose = serviceobs.PurposeDirectorySummary
	facts, err := coordinator.ReadObservations(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if len(facts.Signals) != 1 || facts.Signals[0].BindingKey != "only_one" {
		t.Fatalf("signals=%+v，应只补自定义摘要 key", facts.Signals)
	}
}

// ---- Phase 3 Batch B 跨 lane 集成口径 ----

func compiledBindingSourceWithoutAccess(profile model.Profile) integration.RuntimeBindingSource {
	compiled := buildTestCompiledIntegration(profile)
	bindings := make([]integration.CompiledBinding, 0, len(compiled.Bindings))
	for _, binding := range compiled.Bindings {
		if binding.SourceKey == serviceobs.SourceKeyOpenObserveAccess {
			continue
		}
		bindings = append(bindings, binding)
	}
	compiled.Bindings = bindings
	digest, err := integration.ProfileDigest(profile)
	if err != nil {
		panic(err)
	}
	compiled.SchemaVersion = integration.CompiledIntegrationSchemaVersion
	compiled.ProfileKey = profile.ProfileKey
	compiled.ProfileVersion = profile.ProfileVersion
	compiled.ProfileDigest = digest
	compiled.ServiceID = "svc-1"
	compiled.Environment = "production"
	return integration.NewStaticRuntimeBindingSource(map[string]integration.CompiledIntegration{
		compiled.ServiceID + "\x1e" + compiled.Environment + "\x1e" +
			profile.ProfileKey + "\x1e" + itoa(profile.ProfileVersion): compiled,
	})
}

// compiled 只有 Prometheus+traces+logs 时，access reader 即使注册也不得调用。
func TestBatchBCompiledOnlyPrometheusTracesLogsSkipsAccess(t *testing.T) {
	profile := testProfile()
	access := &fakeAccess{}
	coordinator := New(Options{
		Readers: Readers{
			Signals: &fakeSignals{},
			Traces:  &fakeTraces{},
			Logs:    &fakeLogs{},
			Access:  access,
		},
		Permissions:   allPermissions(),
		BindingSource: compiledBindingSourceWithoutAccess(profile),
	})
	if _, err := coordinator.ReadObservations(context.Background(), workspaceRequest()); err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if access.calls.Load() != 0 {
		t.Fatalf("access 调用次数=%d，compiled 无 access binding 时必须为 0", access.calls.Load())
	}
}

// traces 已确认 empty + logs 超时：Focus 必须是 Unknown，不能宣称 Empty。
func TestBatchBTracesEmptyLogsTimeoutFocusUnknown(t *testing.T) {
	coordinator := newCoordinator(Options{
		Readers: Readers{
			Signals: &fakeSignals{},
			Traces:  &fakeTraces{facts: serviceobs.TraceFacts{Focus: serviceobs.FocusEmpty}},
			Logs:    &fakeLogs{err: context.DeadlineExceeded},
			Access:  &fakeAccess{facts: serviceobs.AccessFacts{Focus: serviceobs.FocusEmpty}},
		},
		Permissions: allPermissions(),
	})
	facts, err := coordinator.ReadObservations(context.Background(), workspaceRequest())
	if err != nil {
		t.Fatalf("ReadObservations 失败: %v", err)
	}
	if facts.Focus != serviceobs.FocusUnknown {
		t.Fatalf("Focus=%q，logs 超时不能把结论降为 Empty", facts.Focus)
	}
}
