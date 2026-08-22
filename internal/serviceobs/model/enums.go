package model

import "time"

// Timestamp 是契约里的 RFC 3339 date-time 字符串。
type Timestamp string

// NewTimestamp 按契约格式（UTC、秒级 Z 后缀）渲染一个时间点。
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp(t.UTC().Format(time.RFC3339))
}

// Time 解析回 time.Time；非法格式返回错误而不是零值静默通过。
func (ts Timestamp) Time() (time.Time, error) {
	return time.Parse(time.RFC3339, string(ts))
}

// Health 是服务/节点/关系的运行健康。它与 DataStatus 正交：缺数不等于不健康，
// 不健康也不等于数据不完整。
type Health string

const (
	HealthHealthy  Health = "healthy"
	HealthDegraded Health = "degraded"
	HealthCritical Health = "critical"
	HealthUnknown  Health = "unknown"
)

func (h Health) IsValid() bool {
	switch h {
	case HealthHealthy, HealthDegraded, HealthCritical, HealthUnknown:
		return true
	default:
		return false
	}
}

// SignalHealth 比 Health 多一个 neutral：没有方向阈值的 Traffic/Rate 永远是 neutral，
// 不进入 worst-of 聚合，也不能因为数值上升被着红或着绿。
type SignalHealth string

const (
	SignalHealthHealthy  SignalHealth = "healthy"
	SignalHealthDegraded SignalHealth = "degraded"
	SignalHealthCritical SignalHealth = "critical"
	SignalHealthUnknown  SignalHealth = "unknown"
	SignalHealthNeutral  SignalHealth = "neutral"
)

func (h SignalHealth) IsValid() bool {
	switch h {
	case SignalHealthHealthy, SignalHealthDegraded, SignalHealthCritical,
		SignalHealthUnknown, SignalHealthNeutral:
		return true
	default:
		return false
	}
}

// Freshness 表示当前观测时间是否仍在 TTL 内。它独立于 Availability：一次成功查询
// 也可能返回过期数据。
type Freshness string

const (
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
	FreshnessUnknown Freshness = "unknown"
)

func (f Freshness) IsValid() bool {
	switch f {
	case FreshnessFresh, FreshnessStale, FreshnessUnknown:
		return true
	default:
		return false
	}
}

// Availability 是 Page View 里 source/section/signal 的可用性词表。它刻意没有
// expired：过期只属于 EvidenceAvailability，source 过期通过 Freshness 表达。
type Availability string

const (
	AvailabilityAvailable   Availability = "available"
	AvailabilityEmpty       Availability = "empty"
	AvailabilityUnknown     Availability = "unknown"
	AvailabilityForbidden   Availability = "forbidden"
	AvailabilityUnavailable Availability = "unavailable"
)

func (a Availability) IsValid() bool {
	switch a {
	case AvailabilityAvailable, AvailabilityEmpty, AvailabilityUnknown,
		AvailabilityForbidden, AvailabilityUnavailable:
		return true
	default:
		return false
	}
}

// SignalAvailability 是 DiagnosticContext 里 signal 的可用性词表，比 Page View 的少
// 一个 empty：一个 signal 要么有值，要么就是 unknown/forbidden/unavailable。
type SignalAvailability string

const (
	SignalAvailabilityAvailable   SignalAvailability = "available"
	SignalAvailabilityUnknown     SignalAvailability = "unknown"
	SignalAvailabilityForbidden   SignalAvailability = "forbidden"
	SignalAvailabilityUnavailable SignalAvailability = "unavailable"
)

func (a SignalAvailability) IsValid() bool {
	switch a {
	case SignalAvailabilityAvailable, SignalAvailabilityUnknown,
		SignalAvailabilityForbidden, SignalAvailabilityUnavailable:
		return true
	default:
		return false
	}
}

// DataState 表示页面数据的覆盖与新鲜度，与 Health 正交。
type DataState string

const (
	DataStateComplete     DataState = "complete"
	DataStatePartial      DataState = "partial"
	DataStateStale        DataState = "stale"
	DataStateUnconfigured DataState = "unconfigured"
)

func (s DataState) IsValid() bool {
	switch s {
	case DataStateComplete, DataStatePartial, DataStateStale, DataStateUnconfigured:
		return true
	default:
		return false
	}
}

// SignalFamily 固定语义：Golden 面向服务/用户旅程，RED 面向服务节点与调用边，
// USE 面向主机/池/队列/存储等资源。
type SignalFamily string

const (
	SignalFamilyGolden SignalFamily = "golden"
	SignalFamilyRED    SignalFamily = "red"
	SignalFamilyUSE    SignalFamily = "use"
)

func (f SignalFamily) IsValid() bool {
	switch f {
	case SignalFamilyGolden, SignalFamilyRED, SignalFamilyUSE:
		return true
	default:
		return false
	}
}

type SignalSemantic string

const (
	SemanticLatency     SignalSemantic = "latency"
	SemanticTraffic     SignalSemantic = "traffic"
	SemanticErrors      SignalSemantic = "errors"
	SemanticSaturation  SignalSemantic = "saturation"
	SemanticRate        SignalSemantic = "rate"
	SemanticDuration    SignalSemantic = "duration"
	SemanticUtilization SignalSemantic = "utilization"
)

func (s SignalSemantic) IsValid() bool {
	switch s {
	case SemanticLatency, SemanticTraffic, SemanticErrors, SemanticSaturation,
		SemanticRate, SemanticDuration, SemanticUtilization:
		return true
	default:
		return false
	}
}

// GoldenSemantics 是 family=golden 允许的四种 semantic，顺序即三层页面展示顺序。
// 每个 Profile 全局恰好各绑定一个，因此 golden_signals 恒为四项。
func GoldenSemantics() []SignalSemantic {
	return []SignalSemantic{SemanticLatency, SemanticTraffic, SemanticErrors, SemanticSaturation}
}

// AllowedSemantics 返回某个 family 合法的 semantic 集合，与 profile schema 的
// allOf/if-then 分支保持一致。
func AllowedSemantics(family SignalFamily) []SignalSemantic {
	switch family {
	case SignalFamilyGolden:
		return GoldenSemantics()
	case SignalFamilyRED:
		return []SignalSemantic{SemanticRate, SemanticErrors, SemanticDuration}
	case SignalFamilyUSE:
		return []SignalSemantic{SemanticUtilization, SemanticSaturation, SemanticErrors}
	default:
		return nil
	}
}

type ScopeKind string

const (
	ScopeKindService  ScopeKind = "service"
	ScopeKindEntity   ScopeKind = "entity"
	ScopeKindRelation ScopeKind = "relation"
	ScopeKindResource ScopeKind = "resource"
)

func (k ScopeKind) IsValid() bool {
	switch k {
	case ScopeKindService, ScopeKindEntity, ScopeKindRelation, ScopeKindResource:
		return true
	default:
		return false
	}
}

type Aggregation string

const (
	AggregationSum   Aggregation = "sum"
	AggregationAvg   Aggregation = "avg"
	AggregationMax   Aggregation = "max"
	AggregationMin   Aggregation = "min"
	AggregationCount Aggregation = "count"
	AggregationRate  Aggregation = "rate"
	AggregationP50   Aggregation = "p50"
	AggregationP95   Aggregation = "p95"
	AggregationP99   Aggregation = "p99"
)

func (a Aggregation) IsValid() bool {
	switch a {
	case AggregationSum, AggregationAvg, AggregationMax, AggregationMin,
		AggregationCount, AggregationRate, AggregationP50, AggregationP95, AggregationP99:
		return true
	default:
		return false
	}
}

// MeasurementRole 区分直接测量与显式代理。proxy 必须给出 proxy_reason，页面才能
// 说明"这是容量压力 proxy"而不是把它当成真实排队饱和度。
type MeasurementRole string

const (
	MeasurementRoleDirect MeasurementRole = "direct"
	MeasurementRoleProxy  MeasurementRole = "proxy"
)

func (r MeasurementRole) IsValid() bool {
	return r == MeasurementRoleDirect || r == MeasurementRoleProxy
}

type ThresholdMode string

const (
	ThresholdModeAbsolute          ThresholdMode = "absolute"
	ThresholdModeBaselineDeviation ThresholdMode = "baseline_deviation"
)

func (m ThresholdMode) IsValid() bool {
	return m == ThresholdModeAbsolute || m == ThresholdModeBaselineDeviation
}

// ThresholdDirection 决定越阈值的方向。它是必填的：没有方向就无法判断 Degraded/
// Critical，只能保持 neutral。
type ThresholdDirection string

const (
	ThresholdDirectionAbove ThresholdDirection = "above"
	ThresholdDirectionBelow ThresholdDirection = "below"
)

func (d ThresholdDirection) IsValid() bool {
	return d == ThresholdDirectionAbove || d == ThresholdDirectionBelow
}

type ProfileStatus string

const (
	ProfileStatusDraft      ProfileStatus = "draft"
	ProfileStatusPublished  ProfileStatus = "published"
	ProfileStatusDeprecated ProfileStatus = "deprecated"
)

func (s ProfileStatus) IsValid() bool {
	switch s {
	case ProfileStatusDraft, ProfileStatusPublished, ProfileStatusDeprecated:
		return true
	default:
		return false
	}
}

// Renderer 是 Profile 能选择的通用视图集合。它是 allowlist：Profile 不能上传
// React/HTML 插件，只能引用服务端已实现的 renderer。
type Renderer string

const (
	RendererGoldenOverview     Renderer = "golden_overview"
	RendererRequestJourney     Renderer = "request_journey"
	RendererDependencyTopology Renderer = "dependency_topology"
	RendererResourceFleet      Renderer = "resource_fleet"
	RendererEvidenceList       Renderer = "evidence_list"
	RendererDomainTimeline     Renderer = "domain_timeline"
)

func (r Renderer) IsValid() bool {
	switch r {
	case RendererGoldenOverview, RendererRequestJourney, RendererDependencyTopology,
		RendererResourceFleet, RendererEvidenceList, RendererDomainTimeline:
		return true
	default:
		return false
	}
}

// Provenance 区分服务负责人登记的 declared 与当前时间窗观察到的 observed。
// declared 在遥测缺失时仍保留；observed 过期只变灰，不删除 declared 底图。
type Provenance string

const (
	ProvenanceDeclared            Provenance = "declared"
	ProvenanceObserved            Provenance = "observed"
	ProvenanceDeclaredAndObserved Provenance = "declared_and_observed"
	ProvenanceInferred            Provenance = "inferred"
)

func (p Provenance) IsValid() bool {
	switch p {
	case ProvenanceDeclared, ProvenanceObserved, ProvenanceDeclaredAndObserved, ProvenanceInferred:
		return true
	default:
		return false
	}
}

type EntityLayer string

const (
	EntityLayerInfrastructure EntityLayer = "infrastructure"
	EntityLayerPlatform       EntityLayer = "platform"
	EntityLayerApplication    EntityLayer = "application"
)

func (l EntityLayer) IsValid() bool {
	switch l {
	case EntityLayerInfrastructure, EntityLayerPlatform, EntityLayerApplication:
		return true
	default:
		return false
	}
}

type EntityKind string

const (
	EntityKindResource                 EntityKind = "resource"
	EntityKindInfrastructureCapability EntityKind = "infrastructure_capability"
	EntityKindBusinessSystem           EntityKind = "business_system"
)

func (k EntityKind) IsValid() bool {
	switch k {
	case EntityKindResource, EntityKindInfrastructureCapability, EntityKindBusinessSystem:
		return true
	default:
		return false
	}
}

type ServiceTier string

const (
	ServiceTier0 ServiceTier = "tier-0"
	ServiceTier1 ServiceTier = "tier-1"
	ServiceTier2 ServiceTier = "tier-2"
	ServiceTier3 ServiceTier = "tier-3"
)

func (t ServiceTier) IsValid() bool {
	switch t {
	case ServiceTier0, ServiceTier1, ServiceTier2, ServiceTier3:
		return true
	default:
		return false
	}
}

type FaultSeverity string

const (
	FaultSeverityInfo     FaultSeverity = "info"
	FaultSeverityWarning  FaultSeverity = "warning"
	FaultSeverityCritical FaultSeverity = "critical"
)

func (s FaultSeverity) IsValid() bool {
	switch s {
	case FaultSeverityInfo, FaultSeverityWarning, FaultSeverityCritical:
		return true
	default:
		return false
	}
}

type LinkKind string

const (
	LinkKindDashboard           LinkKind = "dashboard"
	LinkKindRunbook             LinkKind = "runbook"
	LinkKindServiceRegistration LinkKind = "service_registration"
	LinkKindEntity              LinkKind = "entity"
	LinkKindFault               LinkKind = "fault"
	LinkKindIncident            LinkKind = "incident"
)

func (k LinkKind) IsValid() bool {
	switch k {
	case LinkKindDashboard, LinkKindRunbook, LinkKindServiceRegistration,
		LinkKindEntity, LinkKindFault, LinkKindIncident:
		return true
	default:
		return false
	}
}

type TimelineKind string

const (
	TimelineKindDeployment    TimelineKind = "deployment"
	TimelineKindAlert         TimelineKind = "alert"
	TimelineKindFault         TimelineKind = "fault"
	TimelineKindImpact        TimelineKind = "impact"
	TimelineKindIncident      TimelineKind = "incident"
	TimelineKindHumanDecision TimelineKind = "human_decision"
	TimelineKindTelemetryGap  TimelineKind = "telemetry_gap"
	TimelineKindRecovery      TimelineKind = "recovery"
)

func (k TimelineKind) IsValid() bool {
	switch k {
	case TimelineKindDeployment, TimelineKindAlert, TimelineKindFault, TimelineKindImpact,
		TimelineKindIncident, TimelineKindHumanDecision, TimelineKindTelemetryGap, TimelineKindRecovery:
		return true
	default:
		return false
	}
}

type JourneyStageState string

const (
	JourneyStageStateOK      JourneyStageState = "ok"
	JourneyStageStateFailed  JourneyStageState = "failed"
	JourneyStageStatePending JourneyStageState = "pending"
	JourneyStageStateSkipped JourneyStageState = "skipped"
	JourneyStageStateUnknown JourneyStageState = "unknown"
)

func (s JourneyStageState) IsValid() bool {
	switch s {
	case JourneyStageStateOK, JourneyStageStateFailed, JourneyStageStatePending,
		JourneyStageStateSkipped, JourneyStageStateUnknown:
		return true
	default:
		return false
	}
}

// DiagnosticLevel 是确定性诊断结论的强度。页面不显示模型式置信度百分比；
// candidate_cause 以上必须列出 evidence ids 与缺失要求。
type DiagnosticLevel string

const (
	DiagnosticLevelObservation    DiagnosticLevel = "observation"
	DiagnosticLevelCorrelation    DiagnosticLevel = "correlation"
	DiagnosticLevelCandidateCause DiagnosticLevel = "candidate_cause"
	DiagnosticLevelConfirmedCause DiagnosticLevel = "confirmed_cause"
	DiagnosticLevelIncomplete     DiagnosticLevel = "incomplete"
)

func (l DiagnosticLevel) IsValid() bool {
	switch l {
	case DiagnosticLevelObservation, DiagnosticLevelCorrelation, DiagnosticLevelCandidateCause,
		DiagnosticLevelConfirmedCause, DiagnosticLevelIncomplete:
		return true
	default:
		return false
	}
}

type ViewKind string

const (
	ViewKindServiceDirectory       ViewKind = "service_directory"
	ViewKindServiceOverview        ViewKind = "service_overview"
	ViewKindObservabilityWorkspace ViewKind = "observability_workspace"
)

// SchemaVersion 是 contracts v1 的主版本常量，所有根文档都必须携带它。
const SchemaVersion = "1.0"
