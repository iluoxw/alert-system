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

// Service 只承载“对外提供的能力”，因此分类被收敛成两个合法 pair。infrastructure/
// resource 是被能力承载的底层资源，不能登记成 Service；跨 pair 混搭（例如
// platform/business_system）会让爆炸半径与责任归属同时失真，必须拒绝。
func TestValidateServiceClassification(t *testing.T) {
	cases := []struct {
		name  string
		layer EntityLayer
		kind  ResourceNodeKind
		valid bool
	}{
		{"application 业务系统", EntityLayerApplication, ResourceNodeKindBusinessSystem, true},
		{"platform 基础设施能力", EntityLayerPlatform, ResourceNodeKindInfrastructureCapability, true},
		{"infrastructure 资源不可登记为服务", EntityLayerInfrastructure, ResourceNodeKindResource, false},
		{"platform 配业务系统", EntityLayerPlatform, ResourceNodeKindBusinessSystem, false},
		{"application 配基础设施能力", EntityLayerApplication, ResourceNodeKindInfrastructureCapability, false},
		{"infrastructure 配基础设施能力", EntityLayerInfrastructure, ResourceNodeKindInfrastructureCapability, false},
		{"空 layer", "", ResourceNodeKindBusinessSystem, false},
		{"空 kind", EntityLayerApplication, "", false},
		{"两者皆空", "", "", false},
		{"未知 layer", EntityLayer("workload"), ResourceNodeKindBusinessSystem, false},
		{"未知 kind", EntityLayerApplication, ResourceNodeKind("service"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServiceClassification(tc.layer, tc.kind)
			if tc.valid && err != nil {
				t.Fatalf("layer=%q kind=%q 应合法, got %v", tc.layer, tc.kind, err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatalf("layer=%q kind=%q 应非法", tc.layer, tc.kind)
				}
				if !errors.Is(err, ErrInvalidServiceClassification) {
					t.Fatalf("应返回 ErrInvalidServiceClassification, got %v", err)
				}
			}
		})
	}
}

// 旧 V1 调用方不传分类字段，缺省必须仍是 application/business_system，否则既有
// 服务登记会在一次升级后被静默改写成 platform。
func TestDefaultServiceClassification(t *testing.T) {
	layer, kind := DefaultServiceClassification()
	if layer != EntityLayerApplication || kind != ResourceNodeKindBusinessSystem {
		t.Fatalf("缺省分类=%q/%q, want application/business_system", layer, kind)
	}
	if err := ValidateServiceClassification(layer, kind); err != nil {
		t.Fatalf("缺省分类必须自洽合法: %v", err)
	}
}
