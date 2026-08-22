package model

// DiagnosticContext 镜像 diagnostic-context.schema.json：服务端按当前登录身份组装的
// 只读事实快照。它的 additionalProperties=false，所以 assembler 的内部字段（source
// status、Profile diagnostic 等）不能塞进这里，只能放在不对外序列化的装配快照上。
type DiagnosticContext struct {
	SchemaVersion  string         `json:"schema_version"`
	ContextVersion int            `json:"context_version"`
	RequestID      string         `json:"request_id"`
	AssembledAt    Timestamp      `json:"assembled_at"`
	Actor          Actor          `json:"actor"`
	Service        ContextService `json:"service"`
	Scope          ContextScope   `json:"scope"`
	// Focus 为 nil 表示无聚焦：此时只返回 service 级 declared topology，不发明
	// observed request path。
	Focus        *Focus             `json:"focus"`
	Topology     ContextTopology    `json:"topology"`
	Signals      []Signal           `json:"signals"`
	EvidenceRefs []EvidenceRef      `json:"evidence_refs"`
	DomainState  ContextDomainState `json:"domain_state"`
	Policy       ContextPolicy      `json:"policy"`
}

// Actor 完全由服务端 session 计算。PermissionSnapshotID 是对 user ID、规范化角色/
// capabilities 与 policy version 的指纹，绝不采信 URL 或客户端自报值。
type Actor struct {
	UserID               string   `json:"user_id"`
	TeamIDs              []string `json:"team_ids"`
	Roles                []string `json:"roles"`
	PermissionSnapshotID string   `json:"permission_snapshot_id"`
}

type ContextService struct {
	ServiceID      string   `json:"service_id"`
	EntityKey      string   `json:"entity_key"`
	Name           string   `json:"name"`
	RootEntityIDs  []string `json:"root_entity_ids"`
	ProfileKey     string   `json:"profile_key"`
	ProfileVersion int      `json:"profile_version"`
}

type ContextScope struct {
	Tenant      string    `json:"tenant"`
	Environment string    `json:"environment"`
	TimeRange   TimeRange `json:"time_range"`
	Timezone    string    `json:"timezone"`
}

type TimeRange struct {
	From Timestamp `json:"from"`
	To   Timestamp `json:"to"`
}

type Focus struct {
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Journey string `json:"journey,omitempty"`
	Stage   string `json:"stage,omitempty"`
}

type ContextTopology struct {
	SnapshotID string    `json:"snapshot_id"`
	ObservedAt Timestamp `json:"observed_at"`
	Freshness  Freshness `json:"freshness"`
	// Partial 为 true 表示达到 max nodes/relations 上限而显式截断，不是静默丢弃。
	Partial   bool              `json:"partial"`
	Nodes     []ContextNode     `json:"nodes"`
	Relations []ContextRelation `json:"relations"`
}

// ContextNode 的 Health 在没有任何 signal 时必须是 unknown：拓扑里存在一个节点不等于
// 它健康。
type ContextNode struct {
	EntityID  string    `json:"entity_id"`
	EntityKey string    `json:"entity_key"`
	Name      string    `json:"name"`
	Layer     string    `json:"layer"`
	Kind      string    `json:"kind"`
	Health    Health    `json:"health"`
	Freshness Freshness `json:"freshness"`
}

// ContextRelation 的方向固定为 consumer/caller -> provider/callee；Fault 爆炸半径仍从
// provider 沿反向边遍历，页面箭头不改变领域遍历规则。
type ContextRelation struct {
	RelationID     string     `json:"relation_id"`
	SourceEntityID string     `json:"source_entity_id"`
	TargetEntityID string     `json:"target_entity_id"`
	Kind           string     `json:"kind"`
	Provenance     Provenance `json:"provenance"`
	Freshness      Freshness  `json:"freshness"`
}

// Signal 是一次规范化观测。schema 强制：availability != available 时 Value 必须是
// null、Health 必须是 unknown，并且必须给出 UnknownReason——这条正是"缺数不假绿"的
// 机器可校验形式。
type Signal struct {
	BindingKey      string             `json:"binding_key"`
	Family          SignalFamily       `json:"family"`
	Semantic        SignalSemantic     `json:"semantic"`
	ScopeKind       ScopeKind          `json:"scope_kind"`
	ScopeRef        string             `json:"scope_ref"`
	Value           *float64           `json:"value"`
	Unit            string             `json:"unit"`
	Aggregation     Aggregation        `json:"aggregation"`
	WindowSeconds   int                `json:"window_seconds"`
	ObservedAt      Timestamp          `json:"observed_at"`
	Freshness       Freshness          `json:"freshness"`
	Availability    SignalAvailability `json:"availability"`
	Health          SignalHealth       `json:"health"`
	SourceKey       string             `json:"source_key"`
	MeasurementRole MeasurementRole    `json:"measurement_role,omitempty"`
	ProxyReason     string             `json:"proxy_reason,omitempty"`
	UnknownReason   string             `json:"unknown_reason,omitempty"`
	ThresholdSource string             `json:"threshold_source,omitempty"`
}

type ContextDomainState struct {
	Alerts    []DomainRef `json:"alerts"`
	Faults    []DomainRef `json:"faults"`
	Impacts   []DomainRef `json:"impacts"`
	Incidents []DomainRef `json:"incidents"`
}

type DomainRef struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	UpdatedAt Timestamp `json:"updated_at"`
}

// ContextPolicy 是本次请求的有效限制，取 Profile、ToolDescriptor、当前权限策略与
// 系统全局限制中的最小值。
type ContextPolicy struct {
	AllowedToolKeys  []string `json:"allowed_tool_keys"`
	MaxWindowSeconds int      `json:"max_window_seconds"`
	MaxRows          int      `json:"max_rows"`
	RedactedFields   []string `json:"redacted_fields"`
}

func (c *DiagnosticContext) Normalize() {
	c.Actor.TeamIDs = nonNilSlice(c.Actor.TeamIDs)
	c.Actor.Roles = nonNilSlice(c.Actor.Roles)
	c.Service.RootEntityIDs = nonNilSlice(c.Service.RootEntityIDs)
	c.Topology.Nodes = nonNilSlice(c.Topology.Nodes)
	c.Topology.Relations = nonNilSlice(c.Topology.Relations)
	c.Signals = nonNilSlice(c.Signals)
	c.EvidenceRefs = nonNilSlice(c.EvidenceRefs)
	for i := range c.EvidenceRefs {
		c.EvidenceRefs[i].Normalize()
	}
	c.DomainState.Alerts = nonNilSlice(c.DomainState.Alerts)
	c.DomainState.Faults = nonNilSlice(c.DomainState.Faults)
	c.DomainState.Impacts = nonNilSlice(c.DomainState.Impacts)
	c.DomainState.Incidents = nonNilSlice(c.DomainState.Incidents)
	c.Policy.AllowedToolKeys = nonNilSlice(c.Policy.AllowedToolKeys)
	c.Policy.RedactedFields = nonNilSlice(c.Policy.RedactedFields)
}
