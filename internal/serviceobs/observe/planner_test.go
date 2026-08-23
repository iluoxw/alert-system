package observe_test

import (
	"testing"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/observe"
)

func compiledFixture() integration.CompiledIntegration {
	return integration.CompiledIntegration{
		Bindings: []integration.CompiledBinding{
			{
				Key: "signal_golden_create_traffic", Kind: integration.KindSignal,
				TargetRef: "golden_create_traffic", SourceKey: serviceobs.SourceKeyPrometheus,
			},
			{
				Key: "journey_sandbox_create", Kind: integration.KindJourney,
				TargetRef: "sandbox_create", FocusKind: "sandbox_id",
				SourceKey: serviceobs.SourceKeyOpenObserveTraces,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus},
			},
			{
				Key: "evidence_sandbox_logs", Kind: integration.KindEvidence,
				TargetRef: "sandbox_id", SourceKey: serviceobs.SourceKeyOpenObserveLogs,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus},
			},
			{
				Key: "evidence_access", Kind: integration.KindEvidence,
				TargetRef: "sandbox_id", SourceKey: serviceobs.SourceKeyOpenObserveAccess,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus},
			},
		},
	}
}

func profileWithEvidenceRenderer() model.Profile {
	return model.Profile{
		SignalBindings: []model.SignalBinding{{Key: "golden_create_traffic"}},
		Views: []model.ProfileView{{
			Key: "evidence", Renderer: model.RendererEvidenceList, Order: 1,
		}},
	}
}

func profileWithoutEvidenceRenderer() model.Profile {
	return model.Profile{
		SignalBindings: []model.SignalBinding{{Key: "golden_create_traffic"}},
		Views: []model.ProfileView{{
			Key: "overview", Renderer: model.RendererGoldenOverview, Order: 1,
		}},
	}
}

func TestPlanSourcesIgnoresRendererWhenCompiledHasTracesOnly(t *testing.T) {
	compiled := integration.CompiledIntegration{
		Bindings: []integration.CompiledBinding{
			{Key: "traces", Kind: integration.KindEvidence, TargetRef: "sandbox_id",
				SourceKey: serviceobs.SourceKeyOpenObserveTraces,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus}},
		},
	}
	profile := profileWithEvidenceRenderer() // has access renderer but compiled 无 access binding
	_ = profile
	plan := observe.PlanRuntimeSourcesForTest(serviceobs.PurposeFocusedWorkspace,
		&model.Focus{Kind: "sandbox_id", Value: "sbx_1"}, nil, compiled)
	if len(plan.Access) != 0 {
		t.Fatalf("access planned=%d, want 0", len(plan.Access))
	}
}

func TestPlanSourcesFocusedWithoutFocusSkipsEvidence(t *testing.T) {
	plan := observe.PlanRuntimeSourcesForTest(serviceobs.PurposeFocusedWorkspace, nil, nil, compiledFixture())
	if len(plan.Traces)+len(plan.Logs)+len(plan.Access) != 0 {
		t.Fatalf("evidence should not be planned without focus")
	}
}

func TestPlanSourcesFocusUnknownWhenNoFocusBindings(t *testing.T) {
	compiled := integration.CompiledIntegration{
		Bindings: []integration.CompiledBinding{
			{Key: "signal", Kind: integration.KindSignal, SourceKey: serviceobs.SourceKeyPrometheus},
		},
	}
	plan := observe.PlanRuntimeSourcesForTest(serviceobs.PurposeFocusedWorkspace,
		&model.Focus{Kind: "sandbox_id", Value: "sbx_1"}, nil, compiled)
	if !plan.FocusUnknown {
		t.Fatal("expected FocusUnknown when focus asked but no focus-affecting bindings")
	}
}

func TestPlanSourcesDirectoryOnlySignals(t *testing.T) {
	plan := observe.PlanRuntimeSourcesForTest(serviceobs.PurposeDirectorySummary, nil,
		[]string{"signal_golden_create_traffic"}, compiledFixture())
	if len(plan.Signals) != 1 || len(plan.Traces) != 0 {
		t.Fatalf("directory plan signals=%d traces=%d", len(plan.Signals), len(plan.Traces))
	}
}

func TestPlanSourcesFocusKindFilter(t *testing.T) {
	compiled := integration.CompiledIntegration{
		Bindings: []integration.CompiledBinding{
			{Key: "logs", Kind: integration.KindEvidence, TargetRef: "trace_id",
				SourceKey: serviceobs.SourceKeyOpenObserveLogs,
				SideOutputs: []integration.SideOutput{integration.SideOutputFocus}},
		},
	}
	plan := observe.PlanRuntimeSourcesForTest(serviceobs.PurposeFocusedWorkspace,
		&model.Focus{Kind: "sandbox_id", Value: "sbx_1"}, nil, compiled)
	if len(plan.Logs) != 0 || !plan.FocusUnknown {
		t.Fatalf("wrong focus kind should not plan logs: logs=%d focusUnknown=%v", len(plan.Logs), plan.FocusUnknown)
	}
}
