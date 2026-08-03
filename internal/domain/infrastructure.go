package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResourceNodeKind 区分底层资源、SRE 对外提供的基础设施能力和消费该能力的业务系统。
// 三类节点共用一张图，才能从故障资源反向计算能力和业务爆炸半径。
type ResourceNodeKind string

const (
	ResourceNodeKindResource                 ResourceNodeKind = "resource"
	ResourceNodeKindInfrastructureCapability ResourceNodeKind = "infrastructure_capability"
	ResourceNodeKindBusinessSystem           ResourceNodeKind = "business_system"
)

func (k ResourceNodeKind) IsValid() bool {
	switch k {
	case ResourceNodeKindResource, ResourceNodeKindInfrastructureCapability, ResourceNodeKindBusinessSystem:
		return true
	default:
		return false
	}
}

// EntityLayer 是 canonical entity 的稳定分层。第一阶段显式治理 application，
// 现有 resource/infrastructure_capability 通过迁移自动映射到 infrastructure/platform。
type EntityLayer string

const (
	EntityLayerInfrastructure EntityLayer = "infrastructure"
	EntityLayerPlatform       EntityLayer = "platform"
	EntityLayerApplication    EntityLayer = "application"
)

func (l EntityLayer) IsValid() bool {
	return l == EntityLayerInfrastructure || l == EntityLayerPlatform || l == EntityLayerApplication
}

// DependencyRelation 采用“from 消费/依赖 to”的方向。爆炸半径从故障节点出发，
// 沿 dependency_edges.to_node_id -> from_node_id 反向遍历。
type DependencyRelation string

const (
	DependencyRelationDependsOn DependencyRelation = "depends_on"
	DependencyRelationPartOf    DependencyRelation = "part_of"
)

func (r DependencyRelation) IsValid() bool {
	switch r {
	case DependencyRelationDependsOn, DependencyRelationPartOf:
		return true
	default:
		return false
	}
}

type RelationOrigin string

const (
	RelationOriginObserved RelationOrigin = "observed"
	RelationOriginDeclared RelationOrigin = "declared"
)

func (o RelationOrigin) IsValid() bool {
	return o == RelationOriginObserved || o == RelationOriginDeclared
}

type DependencyCriticality string

const (
	DependencyCriticalityLow      DependencyCriticality = "low"
	DependencyCriticalityMedium   DependencyCriticality = "medium"
	DependencyCriticalityHigh     DependencyCriticality = "high"
	DependencyCriticalityCritical DependencyCriticality = "critical"
)

func (c DependencyCriticality) IsValid() bool {
	switch c {
	case DependencyCriticalityLow, DependencyCriticalityMedium, DependencyCriticalityHigh, DependencyCriticalityCritical:
		return true
	default:
		return false
	}
}

// ResourceNode 是 CMDB/运行时发现数据在事故响应域内的只读投影。
type ResourceNode struct {
	ID             string            `json:"id,omitempty"`
	EntityKey      string            `json:"entity_key"`
	Layer          EntityLayer       `json:"layer"`
	Managed        bool              `json:"managed"`
	Source         string            `json:"source,omitempty"`
	ExternalRef    string            `json:"external_ref"`
	Kind           ResourceNodeKind  `json:"kind"`
	Name           string            `json:"name"`
	Provider       string            `json:"provider,omitempty"`
	Environment    string            `json:"environment,omitempty"`
	OwnerTeamID    string            `json:"owner_team_id,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Attributes     map[string]any    `json:"attributes,omitempty"`
	LastObservedAt time.Time         `json:"last_observed_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at,omitempty"`
}

// DependencyEdge 表示 FromExternalRef 依赖 ToExternalRef。
type DependencyEdge struct {
	ID              string                `json:"id,omitempty"`
	Source          string                `json:"source,omitempty"`
	FromNodeID      string                `json:"from_node_id,omitempty"`
	ToNodeID        string                `json:"to_node_id,omitempty"`
	FromExternalRef string                `json:"from_external_ref"`
	ToExternalRef   string                `json:"to_external_ref"`
	FromNodeName    string                `json:"from_node_name,omitempty"`
	ToNodeName      string                `json:"to_node_name,omitempty"`
	Relation        DependencyRelation    `json:"relation"`
	Criticality     DependencyCriticality `json:"criticality,omitempty"`
	Origin          RelationOrigin        `json:"origin"`
	Evidence        map[string]any        `json:"evidence,omitempty"`
	ObservedAt      time.Time             `json:"observed_at,omitempty"`
}

// TopologySnapshot 是一次来源内的完整拓扑快照。导入时边集合按 Source 原子替换，
// 节点保留最后观测时间，避免临时缺失把历史上下文直接删除。
type TopologySnapshot struct {
	SchemaVersion int
	Source        string
	Environment   string
	ObservedAt    time.Time
	Nodes         []ResourceNode
	Edges         []DependencyEdge
}

var ErrInvalidTopologySnapshot = errors.New("domain: 非法拓扑快照")

// Validate 在进入持久化前拒绝悬空边、自环、重复节点和未知类型。
func (s TopologySnapshot) Validate() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema_version=%d", ErrInvalidTopologySnapshot, s.SchemaVersion)
	}
	if strings.TrimSpace(s.Source) == "" {
		return fmt.Errorf("%w: source 不能为空", ErrInvalidTopologySnapshot)
	}
	if s.ObservedAt.IsZero() {
		return fmt.Errorf("%w: observed_at 不能为空", ErrInvalidTopologySnapshot)
	}

	refs := make(map[string]struct{}, len(s.Nodes))
	for i, node := range s.Nodes {
		ref := strings.TrimSpace(node.ExternalRef)
		if ref == "" || strings.TrimSpace(node.Name) == "" || !node.Kind.IsValid() {
			return fmt.Errorf("%w: nodes[%d] 字段不完整", ErrInvalidTopologySnapshot, i)
		}
		if _, exists := refs[ref]; exists {
			return fmt.Errorf("%w: 重复 external_ref %q", ErrInvalidTopologySnapshot, ref)
		}
		refs[ref] = struct{}{}
	}

	edges := make(map[string]struct{}, len(s.Edges))
	for i, edge := range s.Edges {
		if !edge.Relation.IsValid() {
			return fmt.Errorf("%w: edges[%d] relation=%q", ErrInvalidTopologySnapshot, i, edge.Relation)
		}
		if edge.Criticality != "" && !edge.Criticality.IsValid() {
			return fmt.Errorf("%w: edges[%d] criticality=%q", ErrInvalidTopologySnapshot, i, edge.Criticality)
		}
		if edge.FromExternalRef == edge.ToExternalRef {
			return fmt.Errorf("%w: edges[%d] 不允许自环", ErrInvalidTopologySnapshot, i)
		}
		if _, ok := refs[edge.FromExternalRef]; !ok {
			return fmt.Errorf("%w: edges[%d] from=%q 不存在", ErrInvalidTopologySnapshot, i, edge.FromExternalRef)
		}
		if _, ok := refs[edge.ToExternalRef]; !ok {
			return fmt.Errorf("%w: edges[%d] to=%q 不存在", ErrInvalidTopologySnapshot, i, edge.ToExternalRef)
		}
		key := string(edge.Relation) + "\x00" + edge.FromExternalRef + "\x00" + edge.ToExternalRef
		if _, exists := edges[key]; exists {
			return fmt.Errorf("%w: edges[%d] 重复", ErrInvalidTopologySnapshot, i)
		}
		edges[key] = struct{}{}
	}
	return nil
}

// BlastRadiusNode 是从故障节点沿 depends_on 反向遍历得到的一个受影响节点。
type BlastRadiusNode struct {
	Node  ResourceNode
	Depth int
	Path  []string
	Stale bool
}
