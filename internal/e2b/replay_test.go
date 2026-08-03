package e2b

import (
	"testing"

	"alert-system/internal/domain"
)

func TestGoldenFixturesLoadAndPreserveSignalBoundary(t *testing.T) {
	topology, err := LoadTopologyFixture()
	if err != nil {
		t.Fatalf("加载拓扑失败: %v", err)
	}
	if len(topology.Nodes) < 8 || len(topology.Edges) < 6 {
		t.Fatalf("拓扑过小，nodes=%d edges=%d", len(topology.Nodes), len(topology.Edges))
	}

	stageA, err := LoadReplayFixture("stage_a.json")
	if err != nil {
		t.Fatalf("加载 stage A 失败: %v", err)
	}
	if stageA.Expectations.CapabilityFaultStatus != domain.FaultStatusCandidate || stageA.Expectations.IncidentSuggested {
		t.Fatalf("stage A 预期应为 candidate 且不建议 Incident: %+v", stageA.Expectations)
	}
	events, err := stageA.AlertEvents()
	if err != nil {
		t.Fatalf("转换 AlertEvents 失败: %v", err)
	}
	if len(events) != len(stageA.Signals) {
		t.Fatalf("events=%d signals=%d", len(events), len(stageA.Signals))
	}
	for _, event := range events {
		if event.Fingerprint == "" || event.ResourceRef["topology.external_ref"] == "" || event.Labels["scenario_id"] == "" {
			t.Fatalf("事件缺少 fingerprint/resource ref: %+v", event)
		}
	}
	if payloads := stageA.GenericPayloads(); len(payloads) != len(stageA.Signals) {
		t.Fatalf("generic payloads=%d signals=%d", len(payloads), len(stageA.Signals))
	}

	stageB, err := LoadReplayFixture("stage_b.json")
	if err != nil {
		t.Fatalf("加载 stage B 失败: %v", err)
	}
	if stageB.Expectations.CapabilityFaultStatus != domain.FaultStatusActive || !stageB.Expectations.IncidentSuggested {
		t.Fatalf("stage B 预期应为 active 且建议 Incident: %+v", stageB.Expectations)
	}

	recovery, err := LoadReplayFixture("recovery.json")
	if err != nil {
		t.Fatalf("加载 recovery 失败: %v", err)
	}
	if recovery.Phase != ReplayPhaseRecovery {
		t.Fatalf("recovery phase=%q", recovery.Phase)
	}
}
