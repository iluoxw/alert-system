package reconcile

import (
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
)

var overlayNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func resolvedEntry(namespace, value, entityID, entityKey, name string) serviceobs.AliasEntry {
	identity := serviceobs.ExternalIdentity{Namespace: namespace, Value: value}
	return serviceobs.AliasEntry{
		Identity: identity,
		State:    serviceobs.AliasResolved,
		EntityID: entityID,
		Resolved: &serviceobs.AliasResolvedEntry{
			Identity:  identity,
			EntityID:  entityID,
			EntityKey: entityKey,
			Name:      name,
			Layer:     model.EntityLayerPlatform,
			Kind:      "infrastructure_capability",
		},
	}
}

func TestReconcileOverlayDeclaredOnlyUnchanged(t *testing.T) {
	declared := []serviceobs.TopologyRelation{{
		RelationID:        "decl-1",
		SourceEntityID:    "svc-a",
		TargetEntityID:    "svc-b",
		Kind:              "depends_on",
		Provenance:        model.ProvenanceDeclared,
		MergedRelationIDs: []string{"decl-1"},
	}}
	result := ReconcileOverlay(Request{
		Scope:             Scope{ServiceID: "service-1", ProfileKey: "example", ProfileVersion: 1},
		Now:               overlayNow,
		DeclaredRelations: declared,
		AliasResolution:   serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{}, Issues: []serviceobs.ReconciliationIssue{}},
	})
	if len(result.Relations) != 1 || result.Relations[0].Provenance != model.ProvenanceDeclared {
		t.Fatalf("declared-only 必须保留: %+v", result.Relations)
	}
	if len(result.Inventory) != 0 {
		t.Fatalf("无 instance hint 时不应产生 inventory: %+v", result.Inventory)
	}
}

func TestReconcileOverlayObservedOnlyUsesDeterministicID(t *testing.T) {
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "caller-a", "entity-caller", "platform/caller", "Caller"),
		resolvedEntry("service", "callee-b", "entity-callee", "platform/callee", "Callee"),
	}}
	hint := serviceobs.ObservedTopologyHint{
		Source:     serviceobs.ExternalIdentity{Namespace: "service", Value: "caller-a"},
		Target:     serviceobs.ExternalIdentity{Namespace: "service", Value: "callee-b"},
		Kind:       "calls",
		ObservedAt: overlayNow.Add(-time.Minute),
		SourceKey:  serviceobs.SourceKeyOpenObserveTraces,
	}
	scope := Scope{ServiceID: "11111111-1111-4111-8111-111111111111", ProfileKey: "example", ProfileVersion: 3}
	first := ReconcileOverlay(Request{
		Scope: scope, Now: overlayNow, AliasResolution: aliases, TopologyHints: []serviceobs.ObservedTopologyHint{hint},
	})
	second := ReconcileOverlay(Request{
		Scope: scope, Now: overlayNow, AliasResolution: aliases, TopologyHints: []serviceobs.ObservedTopologyHint{hint},
	})
	if len(first.Relations) != 1 {
		t.Fatalf("应创建 observed-only 边: %+v", first.Relations)
	}
	relation := first.Relations[0]
	if relation.Provenance != model.ProvenanceObserved {
		t.Fatalf("provenance=%q", relation.Provenance)
	}
	if !strings.HasPrefix(relation.RelationID, "obs_") || len(relation.RelationID) >= 128 {
		t.Fatalf("observed-only ID 非法: %q", relation.RelationID)
	}
	if relation.SourceEntityID != "entity-caller" || relation.TargetEntityID != "entity-callee" {
		t.Fatalf("方向必须 caller -> callee: %+v", relation)
	}
	if first.Relations[0].RelationID != second.Relations[0].RelationID {
		t.Fatalf("相同输入 ID 必须稳定: %q vs %q", first.Relations[0].RelationID, second.Relations[0].RelationID)
	}

	changedScope := scope
	changedScope.ProfileVersion = 4
	changed := ReconcileOverlay(Request{
		Scope: changedScope, Now: overlayNow, AliasResolution: aliases, TopologyHints: []serviceobs.ObservedTopologyHint{hint},
	})
	if changed.Relations[0].RelationID == first.Relations[0].RelationID {
		t.Fatal("scope 变化必须改变 observed-only ID")
	}
}

func TestReconcileOverlayDeclaredAndObservedMerge(t *testing.T) {
	declared := []serviceobs.TopologyRelation{{
		RelationID:        "decl-real",
		SourceEntityID:    "entity-caller",
		TargetEntityID:    "entity-callee",
		Kind:              "depends_on",
		Provenance:        model.ProvenanceDeclared,
		MergedRelationIDs: []string{"decl-real"},
	}}
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "caller-a", "entity-caller", "platform/caller", "Caller"),
		resolvedEntry("service", "callee-b", "entity-callee", "platform/callee", "Callee"),
	}}
	result := ReconcileOverlay(Request{
		Scope:             Scope{ServiceID: "svc", ProfileKey: "example", ProfileVersion: 1},
		Now:               overlayNow,
		DeclaredRelations: declared,
		AliasResolution:   aliases,
		TopologyHints: []serviceobs.ObservedTopologyHint{{
			Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "caller-a"},
			Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "callee-b"},
			Kind:   "depends_on", ObservedAt: overlayNow.Add(-30 * time.Second),
		}},
	})
	if len(result.Relations) != 1 {
		t.Fatalf("应只有一条 canonical 边: %+v", result.Relations)
	}
	relation := result.Relations[0]
	if relation.RelationID != "decl-real" {
		t.Fatalf("必须保留 declared 真实 ID，实际 %q", relation.RelationID)
	}
	if relation.Provenance != model.ProvenanceDeclaredAndObserved {
		t.Fatalf("provenance=%q", relation.Provenance)
	}
}

func TestReconcileOverlayUnresolvedEndpointsSkipEdge(t *testing.T) {
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "caller-a", "entity-caller", "platform/caller", "Caller"),
		{Identity: serviceobs.ExternalIdentity{Namespace: "service", Value: "missing"}, State: serviceobs.AliasNotFound},
	}}
	result := ReconcileOverlay(Request{
		Scope:           Scope{ServiceID: "svc", ProfileKey: "example", ProfileVersion: 1},
		Now:             overlayNow,
		AliasResolution: aliases,
		TopologyHints: []serviceobs.ObservedTopologyHint{{
			Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "caller-a"},
			Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "missing"},
			Kind:   "calls", ObservedAt: overlayNow,
		}},
	})
	if len(result.Relations) != 0 {
		t.Fatalf("未解析端点不得建边: %+v", result.Relations)
	}
	if !issueContains(result.Issues, serviceobs.ReasonAliasNotFound, "service", 1) {
		t.Fatalf("应记录 alias_not_found: %+v", result.Issues)
	}
}

func TestReconcileOverlayConflictSkipsEdge(t *testing.T) {
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "caller-a", "entity-caller", "platform/caller", "Caller"),
		{Identity: serviceobs.ExternalIdentity{Namespace: "service", Value: "conflict"}, State: serviceobs.AliasConflict},
	}}
	result := ReconcileOverlay(Request{
		Scope:           Scope{ServiceID: "svc", ProfileKey: "example", ProfileVersion: 1},
		Now:             overlayNow,
		AliasResolution: aliases,
		TopologyHints: []serviceobs.ObservedTopologyHint{{
			Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "caller-a"},
			Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "conflict"},
			Kind:   "calls", ObservedAt: overlayNow,
		}},
	})
	if len(result.Relations) != 0 {
		t.Fatal("冲突端点不得建边")
	}
	if !issueContains(result.Issues, serviceobs.ReasonAliasConflict, "service", 1) {
		t.Fatalf("应记录 alias_conflict: %+v", result.Issues)
	}
}

func TestReconcileOverlayStaleHintMarksRelationStale(t *testing.T) {
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "caller-a", "entity-caller", "platform/caller", "Caller"),
		resolvedEntry("service", "callee-b", "entity-callee", "platform/callee", "Callee"),
	}}
	result := ReconcileOverlay(Request{
		Scope:           Scope{ServiceID: "svc", ProfileKey: "example", ProfileVersion: 1},
		Now:             overlayNow,
		StaleAfter:      time.Hour,
		AliasResolution: aliases,
		TopologyHints: []serviceobs.ObservedTopologyHint{{
			Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "caller-a"},
			Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "callee-b"},
			Kind:   "calls", ObservedAt: overlayNow.Add(-2 * time.Hour),
		}},
	})
	if len(result.Relations) != 1 || !result.Relations[0].Stale {
		t.Fatalf("过期 hint 必须标 stale: %+v", result.Relations)
	}
}

func TestReconcileOverlayBoundsStopProcessingHints(t *testing.T) {
	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "a1", "n1", "platform/a1", "A1"),
		resolvedEntry("service", "a2", "n2", "platform/a2", "A2"),
		resolvedEntry("service", "b1", "m1", "platform/b1", "B1"),
		resolvedEntry("service", "b2", "m2", "platform/b2", "B2"),
	}}
	result := ReconcileOverlay(Request{
		Scope:           Scope{ServiceID: "svc", ProfileKey: "example", ProfileVersion: 1},
		Now:             overlayNow,
		MaxHints:        1,
		AliasResolution: aliases,
		TopologyHints: []serviceobs.ObservedTopologyHint{
			{Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "a1"}, Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "b1"}, Kind: "calls", ObservedAt: overlayNow},
			{Source: serviceobs.ExternalIdentity{Namespace: "service", Value: "a2"}, Target: serviceobs.ExternalIdentity{Namespace: "service", Value: "b2"}, Kind: "calls", ObservedAt: overlayNow},
		},
	})
	if !result.Truncated || len(result.Relations) != 1 {
		t.Fatalf("bounds 应截断 hint 处理: truncated=%v relations=%+v", result.Truncated, result.Relations)
	}
}

func TestReconcileOverlayFrozenFifteenObservedSeventeenInventory(t *testing.T) {
	const (
		apiComponentID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		workerComponentID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		cacheComponentID  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		instanceNS        = "host_instance"
	)

	makeInstances := func(prefix string, count int) []serviceobs.ObservedInstanceHint {
		hints := make([]serviceobs.ObservedInstanceHint, 0, count)
		for i := 0; i < count; i++ {
			hints = append(hints, serviceobs.ObservedInstanceHint{
				Component:  serviceobs.ExternalIdentity{Namespace: "service", Value: prefix + "-component"},
				Instance:   serviceobs.ExternalIdentity{Namespace: instanceNS, Value: prefix + "-inst-" + string(rune('a'+i))},
				ObservedAt: overlayNow.Add(-time.Duration(i) * time.Minute),
				SourceKey:  serviceobs.SourceKeyOpenObserveTraces,
			})
		}
		return hints
	}

	aliases := serviceobs.AliasResolution{Entries: []serviceobs.AliasEntry{
		resolvedEntry("service", "api-component", apiComponentID, "platform/shared/api", "API Gateway"),
		resolvedEntry("service", "worker-component", workerComponentID, "platform/shared/worker", "Worker Pool"),
		resolvedEntry("service", "cache-component", cacheComponentID, "platform/shared/cache", "Cache Cluster"),
	}}
	for i := 0; i < 6; i++ {
		aliases.Entries = append(aliases.Entries, resolvedEntry(instanceNS, "api-inst-"+string(rune('a'+i)), "api-instance-"+string(rune('a'+i)), "platform/shared/api/instance", "API Instance"))
	}
	for i := 0; i < 6; i++ {
		aliases.Entries = append(aliases.Entries, resolvedEntry(instanceNS, "worker-inst-"+string(rune('a'+i)), "worker-instance-"+string(rune('a'+i)), "platform/shared/worker/instance", "Worker Instance"))
	}
	for i := 0; i < 5; i++ {
		aliases.Entries = append(aliases.Entries, resolvedEntry(instanceNS, "cache-inst-"+string(rune('a'+i)), "cache-instance-"+string(rune('a'+i)), "platform/shared/cache/instance", "Cache Instance"))
	}

	frozen := map[string]map[string]struct{}{
		apiComponentID:    {},
		workerComponentID: {},
		cacheComponentID:  {},
	}
	addFrozen := func(componentID, instanceValue string) {
		frozen[componentID][instanceNS+"\x1e"+instanceValue] = struct{}{}
	}
	for i := 0; i < 5; i++ {
		addFrozen(apiComponentID, "api-inst-"+string(rune('a'+i)))
	}
	for i := 0; i < 6; i++ {
		addFrozen(workerComponentID, "worker-inst-"+string(rune('a'+i)))
	}
	for i := 0; i < 4; i++ {
		addFrozen(cacheComponentID, "cache-inst-"+string(rune('a'+i)))
	}

	instanceHints := append(
		append(makeInstances("api", 6), makeInstances("worker", 6)...),
		makeInstances("cache", 5)...,
	)
	// 两个未知 instance alias：只增加 issue，不创建 canonical entity
	instanceHints = append(instanceHints,
		serviceobs.ObservedInstanceHint{
			Component:  serviceobs.ExternalIdentity{Namespace: "service", Value: "api-component"},
			Instance:   serviceobs.ExternalIdentity{Namespace: instanceNS, Value: "unknown-1"},
			ObservedAt: overlayNow,
		},
		serviceobs.ObservedInstanceHint{
			Component:  serviceobs.ExternalIdentity{Namespace: "service", Value: "worker-component"},
			Instance:   serviceobs.ExternalIdentity{Namespace: instanceNS, Value: "unknown-2"},
			ObservedAt: overlayNow,
		},
	)

	declared := []serviceobs.TopologyRelation{{
		RelationID: "decl-api-cache", SourceEntityID: apiComponentID, TargetEntityID: cacheComponentID,
		Kind: "depends_on", Provenance: model.ProvenanceDeclared, MergedRelationIDs: []string{"decl-api-cache"},
	}}

	result := ReconcileOverlay(Request{
		Scope:              Scope{ServiceID: "service-shared", ProfileKey: "shared-platform", ProfileVersion: 2},
		Now:                overlayNow,
		DeclaredRelations:  declared,
		AliasResolution:    aliases,
		InstanceHints:      instanceHints,
		FrozenInstanceSets: frozen,
	})

	if len(result.Relations) != 1 || result.Relations[0].RelationID != "decl-api-cache" {
		t.Fatalf("logical dependency 必须保持不变: %+v", result.Relations)
	}
	if len(result.Inventory) != 3 {
		t.Fatalf("应汇总 3 个 logical component，实际 %d", len(result.Inventory))
	}

	counts := map[string]ComponentInventory{}
	for _, item := range result.Inventory {
		counts[item.EntityID] = item
	}
	if counts[apiComponentID].FrozenCount != 5 || counts[apiComponentID].ObservedCount != 6 || counts[apiComponentID].AddedCount != 1 {
		t.Fatalf("api inventory 不符: %+v", counts[apiComponentID])
	}
	if counts[workerComponentID].FrozenCount != 6 || counts[workerComponentID].ObservedCount != 6 || counts[workerComponentID].AddedCount != 0 {
		t.Fatalf("worker inventory 不符: %+v", counts[workerComponentID])
	}
	if counts[cacheComponentID].FrozenCount != 4 || counts[cacheComponentID].ObservedCount != 5 || counts[cacheComponentID].AddedCount != 1 {
		t.Fatalf("cache inventory 不符: %+v", counts[cacheComponentID])
	}
	totalObserved := counts[apiComponentID].ObservedCount + counts[workerComponentID].ObservedCount + counts[cacheComponentID].ObservedCount
	if totalObserved != 17 {
		t.Fatalf("observed instance 总数应为 17，实际 %d", totalObserved)
	}
	totalFrozen := counts[apiComponentID].FrozenCount + counts[workerComponentID].FrozenCount + counts[cacheComponentID].FrozenCount
	if totalFrozen != 15 {
		t.Fatalf("frozen instance 总数应为 15，实际 %d", totalFrozen)
	}
	if !issueContains(result.Issues, serviceobs.ReasonAliasNotFound, instanceNS, 2) {
		t.Fatalf("未知 instance 只能增加 alias_not_found count: %+v", result.Issues)
	}
}

func issueContains(issues []serviceobs.ReconciliationIssue, reason, namespace string, count int) bool {
	for _, issue := range issues {
		if issue.ReasonCode == reason && issue.Namespace == namespace && issue.Count == count {
			return true
		}
	}
	return false
}
