package runbook

import (
	"context"
	"testing"

	"alert-system/internal/domain"
)

func TestCanonicalSHA256StableAcrossMapOrder(t *testing.T) {
	left := map[string]any{"b": true, "a": []any{"sid-a91c", float64(1)}}
	right := map[string]any{"a": []any{"sid-a91c", float64(1)}, "b": true}
	if canonicalSHA256(left) != canonicalSHA256(right) {
		t.Fatal("canonical plan hash must not depend on map insertion order")
	}
	right["tampered"] = true
	if canonicalSHA256(left) == canonicalSHA256(right) {
		t.Fatal("changed plan content must change SHA256")
	}
}

func TestSanitizeAuditParametersRedactsExecutionAndSecrets(t *testing.T) {
	got := sanitizeAuditParameters(map[string]any{
		"command": "pkill firecracker", "api_key": "secret", "ticket_ref": "INC-1",
	})
	if got["command"] != "[redacted]" || got["api_key"] != "[redacted]" || got["ticket_ref"] != "INC-1" {
		t.Fatalf("unexpected sanitized audit parameters: %+v", got)
	}
}

func TestFakeExecutorHasHardActionAllowlist(t *testing.T) {
	executor := NewFakeExecutor()
	for _, action := range []string{"shell", ActionLifecycleVerify} {
		if _, err := executor.Execute(context.Background(), ExecutionRequest{ActionKey: action}); err == nil {
			t.Fatalf("fake executor should reject %q", action)
		}
	}
	if len(executor.Executions()) != 0 {
		t.Fatal("rejected actions must never be recorded as executed")
	}
}

func TestFakeCleanupApplyOnlyUsesPlanTargets(t *testing.T) {
	executor := NewFakeExecutor()
	result, err := executor.Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionCleanupApply, Mode: domain.RunbookModeApply,
		Parameters: map[string]any{"target_refs": []any{"untrusted-extra"}},
		Plan: map[string]any{"candidates": []any{
			map[string]any{"target_ref": "sid-a91c", "eligible_for_cleanup": true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	targets, _ := result["deleted_target_refs"].([]any)
	if len(targets) != 1 || targets[0] != "sid-a91c" {
		t.Fatalf("apply must consume immutable plan targets only: %+v", result)
	}
}
