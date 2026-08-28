package runbook

import (
	"testing"
)

func TestSanitizeAuditParametersRedactsExecutionAndSecrets(t *testing.T) {
	got := sanitizeAuditParameters(map[string]any{
		"command": "pkill firecracker", "api_key": "secret", "ticket_ref": "INC-1",
	})
	if got["command"] != "[redacted]" || got["api_key"] != "[redacted]" || got["ticket_ref"] != "INC-1" {
		t.Fatalf("unexpected sanitized audit parameters: %+v", got)
	}
}

func TestValidateActionParametersRejectsShellFields(t *testing.T) {
	if err := validateActionParameters(map[string]any{"shell": "rm -rf /"}); err == nil {
		t.Fatal("shell 参数应被拒绝")
	}
}
