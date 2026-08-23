package integration_test

import (
	"context"
	"testing"

	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

func TestStaticRuntimeBindingSourceCloneAndDigest(t *testing.T) {
	profile := model.Profile{
		ProfileKey:     "e2b",
		ProfileVersion: 1,
		Status:         model.ProfileStatusDraft,
		ServiceRef: model.ProfileServiceRef{
			ServiceID: "svc-1",
		},
		SignalBindings: []model.SignalBinding{{Key: "golden_create_traffic"}},
	}
	profile.Normalize()
	digest, err := integration.ProfileDigest(profile)
	if err != nil {
		t.Fatalf("ProfileDigest: %v", err)
	}

	compiled := integration.CompiledIntegration{
		SchemaVersion:  integration.CompiledIntegrationSchemaVersion,
		ProfileKey:     "e2b",
		ProfileVersion: 1,
		Environment:    "production",
		ServiceID:      "svc-1",
		ProfileDigest:  digest,
		Bindings: []integration.CompiledBinding{{
			Key:               "signal_golden_create_traffic",
			Kind:              integration.KindSignal,
			TargetRef:         "golden_create_traffic",
			ResolvedTargetIDs: []string{"entity-1"},
			SourceKey:         integration.SourceKeyPrometheus,
		}},
		TopologyPolicy: integration.CompiledTopologyPolicy{
			AliasNamespaces:   []string{"otel_service"},
			IdentityMappings:  []integration.IdentityMapping{},
			StaleAfterSeconds: 300,
		},
	}

	source := integration.NewStaticRuntimeBindingSource(map[string]integration.CompiledIntegration{
		"svc-1\x1eproduction\x1ee2b\x1e1": compiled,
	})
	got, err := source.ResolveCompiled(context.Background(), "svc-1", "production", profile)
	if err != nil {
		t.Fatalf("ResolveCompiled: %v", err)
	}
	if len(got.Bindings[0].ResolvedTargetIDs) != 1 || got.Bindings[0].ResolvedTargetIDs[0] != "entity-1" {
		t.Fatalf("binding targets=%v", got.Bindings[0].ResolvedTargetIDs)
	}

	// 篡改 profile 语义字段必须 fail closed。
	tampered := profile
	tampered.SignalBindings[0].Label = "changed"
	if _, err := source.ResolveCompiled(context.Background(), "svc-1", "production", tampered); err == nil {
		t.Fatal("tampered profile should fail digest check")
	}
}

func TestCanonicalMarshalRoundTripStable(t *testing.T) {
	compiled := integration.CompiledIntegration{
		SchemaVersion: integration.CompiledIntegrationSchemaVersion,
		PackageKey:    "e2b",
		Bindings:      []integration.CompiledBinding{},
		TopologyPolicy: integration.CompiledTopologyPolicy{
			AliasNamespaces:  []string{},
			IdentityMappings: []integration.IdentityMapping{},
		},
	}
	first, err := integration.CanonicalMarshal(compiled)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := integration.CanonicalMarshal(compiled)
	if err != nil {
		t.Fatalf("marshal2: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("canonical marshal must be byte-stable")
	}
}
