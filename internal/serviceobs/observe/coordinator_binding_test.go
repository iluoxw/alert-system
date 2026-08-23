package observe

import (
	"context"
	"testing"
)

func TestBindingSourcePlansFocusEvidence(t *testing.T) {
	profile := testProfile()
	source := testBindingSource(profile)
	compiled, err := source.ResolveCompiled(context.Background(), "svc-1", "production", profile)
	if err != nil {
		t.Fatalf("ResolveCompiled: %v", err)
	}
	req := workspaceRequest()
	plan := planRuntimeSources(req.Purpose, req.Focus, nil, compiled)
	if plan.FocusUnknown {
		t.Fatal("expected focus bindings, got FocusUnknown plan")
	}
	if len(plan.Traces) == 0 || len(plan.Logs) == 0 {
		t.Fatalf("traces=%d logs=%d access=%d", len(plan.Traces), len(plan.Logs), len(plan.Access))
	}
}
