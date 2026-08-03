package runbook

import (
	"errors"
	"testing"
	"time"
)

func TestUATPolicyRequiresServerSideAuthorizationAndAllowlistedTemplate(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	policy := NewUATPolicy(UATPolicyConfig{
		Enabled:                true,
		EnvironmentID:          "e2b-uat-isolated",
		AuthorizationRef:       "CHG-2026-0802",
		AuthorizationExpiresAt: now.Add(time.Hour),
		AllowedTemplateIDs:     []string{"tpl-uat-safe"},
	})
	valid := map[string]any{
		"environment_id":    "e2b-uat-isolated",
		"authorization_ref": "CHG-2026-0802",
		"template_id":       "tpl-uat-safe",
		"template_alias":    "golden-probe",
		"confirm_cleanup":   true,
	}
	if err := policy.Authorize(valid, now); err != nil {
		t.Fatalf("valid UAT authorization rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong environment", mutate: func(p map[string]any) { p["environment_id"] = "production" }},
		{name: "wrong authorization", mutate: func(p map[string]any) { p["authorization_ref"] = "self-approved" }},
		{name: "template outside allowlist", mutate: func(p map[string]any) { p["template_id"] = "tpl-production" }},
		{name: "cleanup not confirmed", mutate: func(p map[string]any) { p["confirm_cleanup"] = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parameters := cloneParameters(valid)
			tt.mutate(parameters)
			if err := policy.Authorize(parameters, now); !errors.Is(err, ErrUATAuthorizationRequired) {
				t.Fatalf("expected authorization rejection, got %v", err)
			}
		})
	}
}

func TestUATPolicyDefaultsToDeniedAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	parameters := map[string]any{
		"environment_id": "uat", "authorization_ref": "CHG-1",
		"template_id": "tpl-safe", "confirm_cleanup": true,
	}
	if err := (UATPolicy{}).Authorize(parameters, now); !errors.Is(err, ErrUATAuthorizationRequired) {
		t.Fatalf("zero policy must deny, got %v", err)
	}
	policy := NewUATPolicy(UATPolicyConfig{
		Enabled: true, EnvironmentID: "uat", AuthorizationRef: "CHG-1",
		AuthorizationExpiresAt: now, AllowedTemplateIDs: []string{"tpl-safe"},
	})
	if err := policy.Authorize(parameters, now); !errors.Is(err, ErrUATAuthorizationRequired) {
		t.Fatalf("authorization must be invalid at its expiry instant, got %v", err)
	}
}

func TestValidateLifecycleParametersRejectsUnknownOrSecretFields(t *testing.T) {
	base := map[string]any{
		"environment_id": "uat", "authorization_ref": "CHG-1",
		"template_id": "tpl-safe", "confirm_cleanup": true,
	}
	if err := validateActionParameters(ActionLifecycleVerify, base); err != nil {
		t.Fatalf("valid lifecycle parameters rejected: %v", err)
	}
	for _, key := range []string{"api_key", "command", "keep"} {
		parameters := cloneParameters(base)
		parameters[key] = "not-allowed"
		if err := validateActionParameters(ActionLifecycleVerify, parameters); !errors.Is(err, ErrInvalidParameters) {
			t.Fatalf("field %q must be rejected, got %v", key, err)
		}
	}
}

func cloneParameters(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
