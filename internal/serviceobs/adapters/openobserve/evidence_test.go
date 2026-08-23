package openobserve

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

func TestBuildLogEvidenceRefsRedactsRawPayload(t *testing.T) {
	t.Parallel()
	hits := []map[string]json.RawMessage{
		{
			"_timestamp": mustRawJSON(t, 1751444644327758),
			"sandbox_id": mustRawJSON(t, "sbx-123"),
			"level":      mustRawJSON(t, "error"),
			"log":        mustRawJSON(t, "super secret request body"),
		},
	}
	binding := sampleLogsBinding("sandbox_id", "e2b_logs")
	refs := buildLogEvidenceRefs(
		binding,
		"svc_test",
		&model.Focus{Kind: "sandbox_id", Value: "sbx-123"},
		hits,
		time.Now().UTC(),
	)
	if len(refs) != 1 {
		t.Fatalf("refs=%d", len(refs))
	}
	ref := refs[0]
	if strings.Contains(ref.Summary, "super secret") {
		t.Fatalf("summary leaked payload: %q", ref.Summary)
	}
	if ref.Locator.Dataset != "e2b_logs" || ref.Locator.Provider != "openobserve" {
		t.Fatalf("locator=%+v", ref.Locator)
	}
	if len(ref.Redactions) == 0 {
		t.Fatal("expected redactions list")
	}
}

func TestBuildDiscoveryManifestBlocksMissingAccessFocus(t *testing.T) {
	t.Parallel()
	manifest, err := BuildDiscoveryManifest(
		readFixture(t, "streams_list.json"),
		readFixture(t, "stream_schema.json"),
		readFixture(t, "stream_settings_unknown_retention.json"),
		readFixture(t, "stream_stats.json"),
		integration.TemplateParams{
			FocusField: "sandbox_id",
			StageField: "operation_name",
			StageMappings: []integration.StageValueMapping{{
				StageKey: "create", Values: []string{"SandboxCreate"},
			}},
		},
	)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !manifest.ProductionBlocked {
		t.Fatal("expected production gate blocked")
	}
	if manifest.ContentDigest == "" {
		t.Fatal("expected digest")
	}
	if !manifest.Streams[0].RequiredFields.FocusField {
		t.Fatal("expected focus field present")
	}
	if !manifest.Streams[0].RetentionUnknown {
		t.Fatal("retention must stay unknown without explicit policy")
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
