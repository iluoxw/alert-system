package openobserve

import (
	"encoding/json"
	"strings"
	"testing"

	"alert-system/internal/serviceobs/integration"
)

func TestEscapeSQLLiteralDoublesSingleQuotes(t *testing.T) {
	t.Parallel()
	got, err := escapeSQLLiteral("abc'def")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc''def" {
		t.Fatalf("literal=%q want abc''def", got)
	}
}

func TestEscapeSQLLiteralRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	_, err := escapeSQLLiteral("bad\x00value")
	if err == nil {
		t.Fatal("expected error for control character")
	}
}

func TestValidateIdentifierAllowlist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		allowed bool
	}{
		{"sandbox_id", "sandbox_id", true},
		{"dotted", "service.name", true},
		{"sandboxid is valid identifier but never used as fallback", "sandboxid", true},
		{"reject injection", "field;drop", false},
		{"reject empty", "", false},
		{"reject leading digit", "1bad", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateIdentifier(tc.value)
			if tc.allowed && err != nil {
				t.Fatalf("value=%q should be allowed: %v", tc.value, err)
			}
			if !tc.allowed && err == nil {
				t.Fatalf("value=%q should be rejected", tc.value)
			}
		})
	}
}

func TestBuildSearchQueryUsesExactFocusFieldNotSandboxidFallback(t *testing.T) {
	t.Parallel()
	windowFrom, windowTo := testWindow()
	plan, err := buildSearchQuery(
		"default",
		sampleLogsBinding("sandbox_id", "e2b_logs"),
		"sbx-123",
		windowFrom,
		windowTo,
		10,
	)
	if err != nil {
		t.Fatalf("build query: %v", err)
	}
	sql := searchSQL(plan)
	if !strings.Contains(sql, `"sandbox_id" = 'sbx-123'`) {
		t.Fatalf("sql=%q missing exact sandbox_id predicate", sql)
	}
	if strings.Contains(sql, "sandboxid") {
		t.Fatalf("sql must not reference sandboxid fallback: %q", sql)
	}
	if plan.Path != "api/default/_search" {
		t.Fatalf("path=%q", plan.Path)
	}
}

func TestBuildSearchQueryInjectionDoesNotBreakOutOfLiteral(t *testing.T) {
	t.Parallel()
	windowFrom, windowTo := testWindow()
	plan, err := buildSearchQuery(
		"default",
		sampleLogsBinding("sandbox_id", "e2b_logs"),
		"'; DROP TABLE e2b_logs; --",
		windowFrom,
		windowTo,
		10,
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	sql := searchSQL(plan)
	if !strings.Contains(sql, "''") {
		t.Fatalf("sql should escape quotes, got %q", sql)
	}
	if strings.Count(sql, `'`)%2 != 0 {
		t.Fatalf("unbalanced quotes in sql: %q", sql)
	}
}

func TestQueryFingerprintDedupsIdenticalBindings(t *testing.T) {
	t.Parallel()
	b1 := sampleTraceBinding("sandbox_id", "default")
	b2 := b1
	b2.Key = "duplicate_binding"
	windowFrom, windowTo := testWindow()
	p1, err := buildSearchQuery("default", b1, "sbx-1", windowFrom, windowTo, 25)
	if err != nil {
		t.Fatalf("p1: %v", err)
	}
	p2, err := buildSearchQuery("default", b2, "sbx-1", windowFrom, windowTo, 25)
	if err != nil {
		t.Fatalf("p2: %v", err)
	}
	if p1.Fingerprint != p2.Fingerprint {
		t.Fatalf("fingerprints differ for identical execution: %s vs %s", p1.Fingerprint, p2.Fingerprint)
	}
}

func searchSQL(plan queryPlan) string {
	var req searchRequest
	if err := json.Unmarshal(plan.Body, &req); err != nil {
		panic(err)
	}
	return req.Query.SQL
}

func sampleLogsBinding(focusField, stream string) integration.CompiledBinding {
	limit := 50
	return integration.CompiledBinding{
		Key:           "logs_binding",
		Kind:          integration.KindEvidence,
		TemplateRef:   "logs_by_focus.v1",
		SourceKey:     integration.SourceKeyOpenObserveLogs,
		Capability:    integration.CapabilityOpenObserve,
		EvidenceClass: integration.EvidenceClassLogs,
		SideOutputs:   []integration.SideOutput{integration.SideOutputFocus},
		Params: integration.TemplateParams{
			Stream:       stream,
			FocusField:   focusField,
			SelectFields: []string{focusField, "level"},
			RowLimit:     &limit,
		},
	}
}

func sampleTraceBinding(focusField, stream string) integration.CompiledBinding {
	limit := 25
	return integration.CompiledBinding{
		Key:         "trace_binding",
		Kind:        integration.KindJourney,
		TemplateRef: "trace_by_focus.v1",
		SourceKey:   integration.SourceKeyOpenObserveTraces,
		Capability:  integration.CapabilityOpenObserve,
		SideOutputs: []integration.SideOutput{
			integration.SideOutputFocus,
			integration.SideOutputTraceEvidence,
			integration.SideOutputTopologyHints,
		},
		Params: integration.TemplateParams{
			Stream:      stream,
			FocusField:  focusField,
			StageField:  "operation_name",
			StatusField: "span_status",
			StageMappings: []integration.StageValueMapping{{
				StageKey: "create",
				Values:   []string{"SandboxCreate"},
			}},
			StatusMappings: []integration.StatusValueMapping{{
				State:  "ok",
				Values: []string{"OK"},
			}},
			IdentityMappings: []integration.IdentityMapping{
				{Field: "service_name", Namespace: "service", Role: integration.IdentityRoleCaller},
				{Field: "peer_service", Namespace: "service", Role: integration.IdentityRoleCallee},
			},
			RowLimit: &limit,
		},
	}
}
