package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTopologySnapshotValidate(t *testing.T) {
	snapshot := TopologySnapshot{
		SchemaVersion: 1,
		Source:        "e2b-replay",
		ObservedAt:    time.Now(),
		Nodes: []ResourceNode{
			{ExternalRef: "runtime", Name: "Sandbox Runtime", Kind: ResourceNodeKindInfrastructureCapability},
			{ExternalRef: "business", Name: "Preview", Kind: ResourceNodeKindBusinessSystem},
		},
		Edges: []DependencyEdge{{
			FromExternalRef: "business",
			ToExternalRef:   "runtime",
			Relation:        DependencyRelationDependsOn,
		}},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("合法拓扑校验失败: %v", err)
	}

	bad := snapshot
	bad.Edges = append([]DependencyEdge(nil), snapshot.Edges...)
	bad.Edges[0].ToExternalRef = "missing"
	if err := bad.Validate(); !errors.Is(err, ErrInvalidTopologySnapshot) {
		t.Fatalf("悬空边应返回 ErrInvalidTopologySnapshot, got %v", err)
	}
}
