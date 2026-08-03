package main

import (
	"strings"
	"testing"
	"time"

	"alert-system/internal/runbook"
)

func TestBuildE2BUATRuntimeIsExplicitAndRejectsProduction(t *testing.T) {
	setValidUATEnvironment(t)
	if runtime, err := buildE2BUATRuntime(false); err != nil {
		t.Fatal(err)
	} else if _, ok := runtime.Executor.(runbook.DisabledExecutor); !ok || runtime.Policy.Enabled() {
		t.Fatalf("disabled flag must ignore ambient UAT config: %+v", runtime)
	}

	t.Setenv("E2B_UAT_API_URL", "https://api.agi.exc.show")
	if _, err := buildE2BUATRuntime(true); err == nil {
		t.Fatal("production E2B URL must never be accepted as UAT")
	}
	t.Setenv("E2B_UAT_API_URL", "https://api.uat.agi.exc.show")
	if _, err := buildE2BUATRuntime(true); err == nil {
		t.Fatal("preflight-confirmed production ingress alias must remain blocked")
	}
}

func TestBuildE2BUATRuntimeRequiresDedicatedCompleteConfiguration(t *testing.T) {
	for _, missing := range []string{
		"E2B_UAT_API_URL", "E2B_UAT_API_KEY", "E2B_UAT_ENVIRONMENT_ID",
		"E2B_UAT_API_ALLOWED_IPS", "E2B_UAT_TEMPLATE_IDS", "E2B_UAT_AUTHORIZATION_REF",
		"E2B_UAT_AUTHORIZATION_EXPIRES_AT",
	} {
		t.Run(missing, func(t *testing.T) {
			setValidUATEnvironment(t)
			t.Setenv(missing, "")
			_, err := buildE2BUATRuntime(true)
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("expected missing %s error, got %v", missing, err)
			}
		})
	}
}

func TestBuildE2BUATRuntimeCreatesAllowlistedPolicy(t *testing.T) {
	setValidUATEnvironment(t)
	runtime, err := buildE2BUATRuntime(true)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Policy.Enabled() {
		t.Fatal("explicit complete config must enable policy")
	}
	if err := runtime.Policy.Authorize(map[string]any{
		"environment_id": "e2b-uat-isolated", "authorization_ref": "CHG-2026-0802",
		"template_id": "tpl-a", "confirm_cleanup": true,
	}, time.Now()); err != nil {
		t.Fatalf("allowlisted template rejected: %v", err)
	}
}

func setValidUATEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("E2B_UAT_API_URL", "https://api.uat.example.com")
	t.Setenv("E2B_UAT_API_KEY", "dedicated-uat-key")
	t.Setenv("E2B_UAT_API_ALLOWED_IPS", "192.0.2.10")
	t.Setenv("E2B_UAT_ENVIRONMENT_ID", "e2b-uat-isolated")
	t.Setenv("E2B_UAT_TEMPLATE_IDS", "tpl-a,tpl-b")
	t.Setenv("E2B_UAT_AUTHORIZATION_REF", "CHG-2026-0802")
	t.Setenv("E2B_UAT_AUTHORIZATION_EXPIRES_AT", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
}
