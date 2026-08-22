package model

// 本文件镜像 service-observability-page-view.schema.json 的三个 oneOf 分支。Page View
// 是面向路由的显示投影：它只格式化已组装的事实，不发原始查询、不重算 Health，也不写
// 领域状态。

type ServiceIdentity struct {
	ServiceID     string      `json:"service_id"`
	EntityID      string      `json:"entity_id"`
	EntityKey     string      `json:"entity_key"`
	Name          string      `json:"name"`
	Description   string      `json:"description"`
	Layer         EntityLayer `json:"layer"`
	Kind          EntityKind  `json:"kind"`
	Environment   string      `json:"environment"`
	OwnerTeamID   string      `json:"owner_team_id"`
	OwnerTeamName string      `json:"owner_team_name"`
	Tier          ServiceTier `json:"tier"`
}

// ProfileRef 为 nil 表示该服务尚未配置 Profile：Data 必须是 unconfigured，页面不猜
// 指标也不猜布局。
type ProfileRef struct {
	ProfileKey     string        `json:"profile_key"`
	ProfileVersion int           `json:"profile_version"`
	Status         ProfileStatus `json:"status"`
}

// HealthSummary 的 ReasonCodes 必须列出导致最终状态的 binding key，禁止综合分数。
type HealthSummary struct {
	Status      Health    `json:"status"`
	ReasonCodes []string  `json:"reason_codes"`
	EvaluatedAt Timestamp `json:"evaluated_at"`
}

type SourceStatus struct {
	SourceKey  string       `json:"source_key"`
	State      Availability `json:"state"`
	Freshness  Freshness    `json:"freshness,omitempty"`
	ObservedAt Timestamp    `json:"observed_at,omitempty"`
	ReasonCode string       `json:"reason_code,omitempty"`
	Retryable  bool         `json:"retryable"`
}

type DataStatus struct {
	State   DataState      `json:"state"`
	Sources []SourceStatus `json:"sources"`
}

// SignalValue 是 signal 的显示投影。DisplayValue 是已格式化文本；Sparkline 只有真的
// 拿到 samples 时才出现，没有样本不得画折线。
type SignalValue struct {
	BindingKey           string          `json:"binding_key"`
	Label                string          `json:"label"`
	Family               SignalFamily    `json:"family"`
	Semantic             SignalSemantic  `json:"semantic"`
	ScopeKind            ScopeKind       `json:"scope_kind"`
	ScopeRef             string          `json:"scope_ref"`
	Value                *float64        `json:"value"`
	DisplayValue         string          `json:"display_value"`
	Unit                 string          `json:"unit"`
	Aggregation          Aggregation     `json:"aggregation"`
	WindowSeconds        int             `json:"window_seconds"`
	ObservedAt           Timestamp       `json:"observed_at"`
	Freshness            Freshness       `json:"freshness"`
	Availability         Availability    `json:"availability"`
	Health               SignalHealth    `json:"health"`
	SourceKey            string          `json:"source_key"`
	MeasurementRole      MeasurementRole `json:"measurement_role,omitempty"`
	ProxyReason          string          `json:"proxy_reason,omitempty"`
	ThresholdSource      string          `json:"threshold_source,omitempty"`
	BaselineDeltaPercent *float64        `json:"baseline_delta_percent,omitempty"`
	UnknownReason        string          `json:"unknown_reason,omitempty"`
	Sparkline            []float64       `json:"sparkline,omitempty"`
}

// DomainCounts 只展示，不参与 Health 计算：Fault/Incident 数量不能改写信号健康。
type DomainCounts struct {
	ActiveFaults     int `json:"active_faults"`
	ConfirmedImpacts int `json:"confirmed_impacts"`
	ActiveIncidents  int `json:"active_incidents"`
}

type SectionStatus struct {
	SectionKey string       `json:"section_key"`
	State      Availability `json:"state"`
	ReasonCode string       `json:"reason_code,omitempty"`
	Retryable  bool         `json:"retryable"`
}

type Link struct {
	Kind  LinkKind `json:"kind"`
	Label string   `json:"label"`
	Href  string   `json:"href"`
}

type FaultSummary struct {
	FaultID              string        `json:"fault_id"`
	Title                string        `json:"title"`
	State                string        `json:"state"`
	Severity             FaultSeverity `json:"severity"`
	OwnerTeamName        string        `json:"owner_team_name,omitempty"`
	DurationSeconds      *int          `json:"duration_seconds,omitempty"`
	ConfirmedImpactCount int           `json:"confirmed_impact_count"`
	PotentialImpactCount int           `json:"potential_impact_count"`
	UpdatedAt            Timestamp     `json:"updated_at"`
}

// DependencyRow 的 GroupKey/GroupLabel 是服务端投影事实：浏览器不按实体名猜分组。
type DependencyRow struct {
	EntityID   string      `json:"entity_id"`
	Name       string      `json:"name"`
	Layer      EntityLayer `json:"layer"`
	GroupKey   string      `json:"group_key"`
	GroupLabel string      `json:"group_label"`
	Role       string      `json:"role"`
	Provenance Provenance  `json:"provenance"`
	Health     Health      `json:"health"`
	Freshness  Freshness   `json:"freshness"`
	ObservedAt Timestamp   `json:"observed_at,omitempty"`
}

type TimelineEvent struct {
	EventID     string       `json:"event_id"`
	Kind        TimelineKind `json:"kind"`
	Title       string       `json:"title"`
	Source      string       `json:"source"`
	OccurredAt  Timestamp    `json:"occurred_at"`
	EntityIDs   []string     `json:"entity_ids,omitempty"`
	RelationIDs []string     `json:"relation_ids,omitempty"`
	DomainRef   string       `json:"domain_ref,omitempty"`
}

type ViewFocusKind struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Placeholder  string `json:"placeholder"`
	ValuePattern string `json:"value_pattern"`
	Primary      bool   `json:"primary"`
}

type ViewScope struct {
	Tenant      string    `json:"tenant"`
	Environment string    `json:"environment"`
	From        Timestamp `json:"from"`
	To          Timestamp `json:"to"`
	Timezone    string    `json:"timezone"`
}

type JourneyStageView struct {
	Key         string            `json:"key"`
	Label       string            `json:"label"`
	State       JourneyStageState `json:"state"`
	EntityIDs   []string          `json:"entity_ids"`
	RelationIDs []string          `json:"relation_ids"`
	EvidenceIDs []string          `json:"evidence_ids"`
	ReasonCode  string            `json:"reason_code,omitempty"`
}

type TopologyNodeView struct {
	EntityID  string        `json:"entity_id"`
	Name      string        `json:"name"`
	Layer     string        `json:"layer"`
	Kind      string        `json:"kind"`
	Health    Health        `json:"health"`
	Freshness Freshness     `json:"freshness"`
	Signals   []SignalValue `json:"signals"`
}

type TopologyRelationView struct {
	RelationID     string        `json:"relation_id"`
	SourceEntityID string        `json:"source_entity_id"`
	TargetEntityID string        `json:"target_entity_id"`
	Kind           string        `json:"kind"`
	Provenance     Provenance    `json:"provenance"`
	Health         Health        `json:"health"`
	Freshness      Freshness     `json:"freshness"`
	Signals        []SignalValue `json:"signals"`
}

type WorkspaceTopology struct {
	SnapshotID string                 `json:"snapshot_id"`
	Partial    bool                   `json:"partial"`
	Nodes      []TopologyNodeView     `json:"nodes"`
	Relations  []TopologyRelationView `json:"relations"`
}

// DiagnosticSummary 在缺证据时必须是 incomplete，并列出 MissingRequirements；
// candidate/confirmed cause 不允许由缺失遥测推导。
type DiagnosticSummary struct {
	Level               DiagnosticLevel `json:"level"`
	Title               string          `json:"title"`
	Statements          []string        `json:"statements"`
	EvidenceIDs         []string        `json:"evidence_ids"`
	MissingRequirements []string        `json:"missing_requirements"`
}

type WorkspaceDomainState struct {
	Alerts    []DomainRef `json:"alerts"`
	Faults    []DomainRef `json:"faults"`
	Impacts   []DomainRef `json:"impacts"`
	Incidents []DomainRef `json:"incidents"`
}

// Capabilities 只从服务端 session 计算。metrics/logs/traces/evidence 只有对应 adapter
// 真正注册并授权时才为 true。
type Capabilities struct {
	ViewMetrics           bool `json:"view_metrics"`
	ViewLogs              bool `json:"view_logs"`
	ViewTraces            bool `json:"view_traces"`
	ViewDomain            bool `json:"view_domain"`
	OpenEvidenceDeepLinks bool `json:"open_evidence_deep_links"`
}

type DirectorySummary struct {
	Managed      int `json:"managed"`
	Critical     int `json:"critical"`
	Partial      int `json:"partial"`
	Unconfigured int `json:"unconfigured"`
}

type DirectoryItem struct {
	Service       ServiceIdentity `json:"service"`
	Profile       *ProfileRef     `json:"profile"`
	Health        HealthSummary   `json:"health"`
	DataStatus    DataStatus      `json:"data_status"`
	GoldenSignals []SignalValue   `json:"golden_signals"`
	DomainCounts  DomainCounts    `json:"domain_counts"`
	Links         []Link          `json:"links,omitempty"`
	ObservedAt    Timestamp       `json:"observed_at"`
}

type DirectoryView struct {
	SchemaVersion string           `json:"schema_version"`
	ViewKind      ViewKind         `json:"view_kind"`
	RequestID     string           `json:"request_id"`
	AssembledAt   Timestamp        `json:"assembled_at"`
	Summary       DirectorySummary `json:"summary"`
	Items         []DirectoryItem  `json:"items"`
	// NextCursor 为 nil 表示没有下一页；空字符串不是合法的"无下一页"表达。
	NextCursor *string `json:"next_cursor"`
}

type OverviewView struct {
	SchemaVersion string          `json:"schema_version"`
	ViewKind      ViewKind        `json:"view_kind"`
	RequestID     string          `json:"request_id"`
	AssembledAt   Timestamp       `json:"assembled_at"`
	Service       ServiceIdentity `json:"service"`
	Profile       *ProfileRef     `json:"profile"`
	Health        HealthSummary   `json:"health"`
	DataStatus    DataStatus      `json:"data_status"`
	GoldenSignals []SignalValue   `json:"golden_signals"`
	DomainCounts  DomainCounts    `json:"domain_counts"`
	Faults        []FaultSummary  `json:"faults"`
	Dependencies  []DependencyRow `json:"dependencies"`
	RecentEvents  []TimelineEvent `json:"recent_events"`
	FocusKinds    []ViewFocusKind `json:"focus_kinds"`
	Links         []Link          `json:"links"`
	Sections      []SectionStatus `json:"sections"`
}

type WorkspaceView struct {
	SchemaVersion     string               `json:"schema_version"`
	ViewKind          ViewKind             `json:"view_kind"`
	RequestID         string               `json:"request_id"`
	ContextVersion    int                  `json:"context_version"`
	AssembledAt       Timestamp            `json:"assembled_at"`
	Service           ServiceIdentity      `json:"service"`
	Profile           *ProfileRef          `json:"profile"`
	Scope             ViewScope            `json:"scope"`
	Focus             *Focus               `json:"focus"`
	Health            HealthSummary        `json:"health"`
	DataStatus        DataStatus           `json:"data_status"`
	GoldenSignals     []SignalValue        `json:"golden_signals"`
	Journey           []JourneyStageView   `json:"journey"`
	Topology          WorkspaceTopology    `json:"topology"`
	DiagnosticSummary DiagnosticSummary    `json:"diagnostic_summary"`
	EvidenceRefs      []EvidenceRef        `json:"evidence_refs"`
	DomainState       WorkspaceDomainState `json:"domain_state"`
	Timeline          []TimelineEvent      `json:"timeline"`
	Capabilities      Capabilities         `json:"capabilities"`
	Sections          []SectionStatus      `json:"sections"`
}

func (v *DirectoryView) Normalize() {
	v.Items = nonNilSlice(v.Items)
	for i := range v.Items {
		v.Items[i].GoldenSignals = nonNilSlice(v.Items[i].GoldenSignals)
		v.Items[i].DataStatus.Sources = nonNilSlice(v.Items[i].DataStatus.Sources)
		v.Items[i].Health.ReasonCodes = nonNilSlice(v.Items[i].Health.ReasonCodes)
	}
}

func (v *OverviewView) Normalize() {
	v.Health.ReasonCodes = nonNilSlice(v.Health.ReasonCodes)
	v.DataStatus.Sources = nonNilSlice(v.DataStatus.Sources)
	v.GoldenSignals = nonNilSlice(v.GoldenSignals)
	v.Faults = nonNilSlice(v.Faults)
	v.Dependencies = nonNilSlice(v.Dependencies)
	v.RecentEvents = nonNilSlice(v.RecentEvents)
	v.FocusKinds = nonNilSlice(v.FocusKinds)
	v.Links = nonNilSlice(v.Links)
	v.Sections = nonNilSlice(v.Sections)
}

func (v *WorkspaceView) Normalize() {
	v.Health.ReasonCodes = nonNilSlice(v.Health.ReasonCodes)
	v.DataStatus.Sources = nonNilSlice(v.DataStatus.Sources)
	v.GoldenSignals = nonNilSlice(v.GoldenSignals)
	v.Journey = nonNilSlice(v.Journey)
	for i := range v.Journey {
		v.Journey[i].EntityIDs = nonNilSlice(v.Journey[i].EntityIDs)
		v.Journey[i].RelationIDs = nonNilSlice(v.Journey[i].RelationIDs)
		v.Journey[i].EvidenceIDs = nonNilSlice(v.Journey[i].EvidenceIDs)
	}
	v.Topology.Nodes = nonNilSlice(v.Topology.Nodes)
	for i := range v.Topology.Nodes {
		v.Topology.Nodes[i].Signals = nonNilSlice(v.Topology.Nodes[i].Signals)
	}
	v.Topology.Relations = nonNilSlice(v.Topology.Relations)
	for i := range v.Topology.Relations {
		v.Topology.Relations[i].Signals = nonNilSlice(v.Topology.Relations[i].Signals)
	}
	v.DiagnosticSummary.Statements = nonNilSlice(v.DiagnosticSummary.Statements)
	v.DiagnosticSummary.EvidenceIDs = nonNilSlice(v.DiagnosticSummary.EvidenceIDs)
	v.DiagnosticSummary.MissingRequirements = nonNilSlice(v.DiagnosticSummary.MissingRequirements)
	v.EvidenceRefs = nonNilSlice(v.EvidenceRefs)
	for i := range v.EvidenceRefs {
		v.EvidenceRefs[i].Normalize()
	}
	v.DomainState.Alerts = nonNilSlice(v.DomainState.Alerts)
	v.DomainState.Faults = nonNilSlice(v.DomainState.Faults)
	v.DomainState.Impacts = nonNilSlice(v.DomainState.Impacts)
	v.DomainState.Incidents = nonNilSlice(v.DomainState.Incidents)
	v.Timeline = nonNilSlice(v.Timeline)
	v.Sections = nonNilSlice(v.Sections)
}
