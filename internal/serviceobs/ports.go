// Package serviceobs 定义 Service Observability 只读聚合边界的 ports。
//
// 依赖方向是硬约束：`api -> facade/view/context -> ports`，adapter 实现 ports。本包
// 只依赖 model 与 health（两个叶子包），绝不 import store、vendor SDK 或任何领域私有
// package——否则"第二个服务只需登记 service、发布 Profile、绑定通用 tools"就不再成立。
//
// ports 返回的是**规范化事实 + 每个 source 的状态**，不返回 Page View：显示投影是
// view 包的职责，Health/Data 判定是 health 包的职责。
//
// Phase 2 的三条口径在这里就固化下来：
//
//  1. 生产主落地是 alert-system：identity、declared topology 与 Alert/Fault/Impact/
//     Incident 全部来自本系统已提交的事实；
//  2. 遥测尚未接入，因此 telemetry port 只有一个显式的 UnavailableObservationReader，
//     它既不读契约 fixture 也不访问外部网络；
//  3. 缺失/过期/失败只在最小范围降级为 Unknown/Unavailable，declared topology 与
//     domain 事实必须保留。
package serviceobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/model"
)

// source key 是稳定机器码，会进 Page View 的 data_status.sources 与 signal.source_key。
// 只能增加不能改写：前端按 code 本地化文案。
const (
	// SourceKeyRegistration 是 Service Registration 与 canonical service entity。
	SourceKeyRegistration = "alert_system_registration"
	// SourceKeyTopology 是 canonical Entity/Relation 邻域。
	SourceKeyTopology = "alert_system_topology"
	// SourceKeyDomain 是 Alert/Fault/Impact/Incident 领域事实。
	SourceKeyDomain = "alert_system_domain"
	// SourceKeyTelemetry 是 Golden/RED/USE 的语义指标来源。Phase 2 未接入，命名保持
	// provider-neutral，切 VictoriaMetrics 时只换 provider 配置。
	SourceKeyTelemetry = "prometheus_compatible"
)

// 稳定 reason code。
const (
	// ReasonTelemetryNotConnected 是 Phase 2 全部 signal 的固定 unknown reason：
	// 遥测尚未接入，不是一次查询失败，也不是"没有异常"。
	ReasonTelemetryNotConnected = "telemetry_not_connected"
	// ReasonTopologyReadFailed 表示 canonical 邻域读取失败。它只降级 topology section。
	ReasonTopologyReadFailed = "topology_read_failed"
	// ReasonDomainReadFailed 表示领域事实读取失败。
	ReasonDomainReadFailed = "domain_read_failed"
	// ReasonTopologyBoundsReached 表示达到 max nodes/relations 上限而显式截断。
	ReasonTopologyBoundsReached = "topology_bounds_reached"
	// ReasonDomainBoundsReached 表示达到 domain entity 边界上限而显式截断。
	ReasonDomainBoundsReached = "domain_bounds_reached"
	// ReasonSignalScopeUnresolved 表示某个已声明 binding 的 scope.refs 缺省、为空或
	// 悬空，无法挂到 bounded topology 上的任何 node/relation。它是局部缺口，不是
	// "这个 binding 不存在"——evaluator 仍保留该 binding 的 Unknown 结果。
	ReasonSignalScopeUnresolved = "signal_scope_unresolved"
	// ReasonSignalScopeAggregateNotProjectable 表示某个 binding 的 scope 解析到多个
	// canonical 对象，因此它的数值是跨对象聚合值（例如 max over hosts）。
	//
	// Page View v1 的 signalValue.scope_ref 是单值，把同一个聚合数值复制到每个对象上会
	// 把"这一组里最坏的是 91%"伪装成"这个对象是 91%"。所以这类 binding 不做逐对象展示，
	// 完整判定保留在 evaluator 与 DiagnosticContext 里，缺口用这个稳定 reason 表达。
	// 注意健康意见仍然逐对象生效：知道一组里最坏值越了阈值，就不能声称其中任何一个健康。
	ReasonSignalScopeAggregateNotProjectable = "signal_scope_aggregate_not_projectable"
	// ReasonProfileUnconfigured 表示该服务没有 active published Profile。
	ReasonProfileUnconfigured = "profile_unconfigured"
	// ReasonFocusNotObserved 表示 focus 查询成功但当前边界内没有匹配事实。
	ReasonFocusNotObserved = "focus_not_observed"
)

// section key 是 Page View 的 sections[] 键。它们对应设计稿里的页面区块，
// 一个 section 失败只降级自己。
const (
	SectionKeyIdentity  = "identity"
	SectionKeyGolden    = "golden_signals"
	SectionKeyJourney   = "journey"
	SectionKeyTopology  = "topology"
	SectionKeyEvidence  = "evidence"
	SectionKeyDomain    = "domain_state"
	SectionKeyTimeline  = "timeline"
	SectionKeyDiagnosis = "diagnostic_summary"
)

// TenantDefault 是当前单租户部署的固定 scope.tenant。它是服务端常量，不接受 URL 输入。
const TenantDefault = "default"

// PolicyVersion 参与 permission_snapshot_id 指纹，保证策略语义变化时快照 ID 必然变化。
const PolicyVersion = "serviceobs-v1"

var (
	// ErrServiceNotFound 表示该 service registration 不存在。上层统一 404，不泄露
	// 其它环境或其它团队的服务是否存在。
	ErrServiceNotFound = errors.New("serviceobs: 服务不存在")
	// ErrProfileUnconfigured 表示该服务没有 active published Profile。总览仍可用，
	// 工作区必须 409——不猜指标也不猜布局。
	ErrProfileUnconfigured = errors.New("serviceobs: 服务未配置 Service Profile")
	// ErrInvalidFocus 表示 focus kind 不在已发布 Profile 的 focus_kinds 内，或 value
	// 不满足该 focus 的 value_pattern。
	ErrInvalidFocus = errors.New("serviceobs: focus 非法")
	// ErrScopeTooBroad 表示时间窗或基数超出 Profile/global 限制。超限显式报错，
	// 不静默截断。
	ErrScopeTooBroad = errors.New("serviceobs: 查询范围超出限制")
	// ErrCandidateSetTooLarge 表示 metadata 过滤后的候选服务数超过显式安全上界。
	//
	// 它必须是一个错误而不是一次静默截断：目录摘要的分母是"完整候选集合"，返回一个
	// 被截掉尾巴的计数等于报出一个看起来完整、实际错误的数字。调用方按"基数超限"
	// 处理（可由收窄 environment/team/tier/q 解决），不是基础设施故障。
	ErrCandidateSetTooLarge = fmt.Errorf("%w: 候选服务集合超过安全上界", ErrScopeTooBroad)
)

// ServiceIdentity 是 Service Registration 与 canonical service entity 的规范化身份。
//
// UpdatedAt/EntityObservedAt 是数据库里真实持久化的时间，供"没有观测"的信号与 cache
// 占位取真实时间锚使用，绝不能用当前时间伪装成观测时间。
type ServiceIdentity struct {
	ServiceID     string
	EntityID      string
	EntityKey     string
	Name          string
	Description   string
	Layer         model.EntityLayer
	Kind          model.EntityKind
	Environment   string
	OwnerTeamID   string
	OwnerTeamName string
	Tier          model.ServiceTier
	DashboardURL  string
	RunbookURL    string
	Enabled       bool
	// EntityObservedAt 是 canonical entity 的 last_observed_at。
	EntityObservedAt time.Time
	// UpdatedAt 是 registration 行的最后变更时间。
	UpdatedAt time.Time
}

// FactAnchor 返回该 identity 里最新的真实持久化时间。
func (s ServiceIdentity) FactAnchor() time.Time {
	return laterOf(s.UpdatedAt, s.EntityObservedAt)
}

// RegistrationFilter 是目录的 metadata 过滤条件。全部参数化下传，不拼 SQL。
type RegistrationFilter struct {
	Environment string
	TeamID      string
	Tier        string
	// Query 是名称/entity key 的模糊匹配片段，长度上限由 adapter 收敛。
	Query string
	Limit int
}

// RegistrationList 是目录用的轻量 identity 结果集。
//
// 它刻意不带 aliases 与 dependencies：目录有几百行，逐行再查两张表就是 N+1。
type RegistrationList struct {
	Items []ServiceIdentity
	// Truncated 为 true 表示还有更多候选行被 limit 截断。
	Truncated bool
	// ReadAt 是这次一致读的时刻，与 Items 里的事实时间不是一回事。
	ReadAt time.Time
}

// DeclaredDependency 是服务负责人登记的结构依赖。方向固定 consumer -> provider：
// FromEntityID 消费/依赖 ToEntityID。Fault 爆炸半径仍沿反向边遍历。
type DeclaredDependency struct {
	RelationID   string
	FromEntityID string
	ToEntityID   string
	ToEntityKey  string
	ToEntityName string
	ToLayer      model.EntityLayer
	ToKind       string
	// Relation 是 canonical 关系类型（depends_on / part_of）。
	Relation string
	// Criticality 原样保留服务负责人声明的重要度，不由页面推导。
	Criticality string
	Provenance  model.Provenance
	ObservedAt  time.Time
}

// ServiceFacts 是单个服务的 registration 事实。它是关键依赖：读不到就没有 identity，
// 上层返回页面级错误而不是 Partial。
type ServiceFacts struct {
	Identity     ServiceIdentity
	Dependencies []DeclaredDependency
	ReadAt       time.Time
}

// FactAnchor 返回 registration 事实里最新的真实时间。
func (f ServiceFacts) FactAnchor() time.Time {
	anchor := f.Identity.FactAnchor()
	for _, dependency := range f.Dependencies {
		anchor = laterOf(anchor, dependency.ObservedAt)
	}
	return anchor
}

// RegistrationReader 读取 Service Registration 与 canonical service entity。
type RegistrationReader interface {
	// ListServices 只返回 enabled registration 的轻量身份，metadata 过滤参数化。
	ListServices(ctx context.Context, filter RegistrationFilter) (RegistrationList, error)
	// ReadService 返回单个服务的身份与 declared dependencies。不存在返回
	// ErrServiceNotFound。
	ReadService(ctx context.Context, serviceID string) (ServiceFacts, error)
}

// DefaultMaxDirectoryCandidates 是目录完整候选集合的默认安全上界。
//
// 它是"这个部署有多少个受管服务"的量级上界，不是页面 limit：页面每次最多 200 行，
// 但摘要分母必须覆盖 metadata 过滤后的全部候选。
const DefaultMaxDirectoryCandidates = 2000

// DefaultCandidateBatchSize 是完整候选扫描的默认单批大小。
const DefaultCandidateBatchSize = 200

// CandidateScanRequest 是完整候选集合的有界扫描请求。
//
// 与 RegistrationFilter 的区别在语义：这里没有"页面 limit"，只有单批大小与全局安全
// 上界。metadata 过滤（environment/team/tier/q）之后的候选集合必须被完整走完，因为
// 目录摘要的分母就是它；health/data_status 是结果筛选，不参与这一层。
type CandidateScanRequest struct {
	Filter CandidateFilter
	// BatchSize <= 0 时使用 DefaultCandidateBatchSize，并受 adapter 的单批硬上界约束。
	BatchSize int
	// MaxCandidates <= 0 时使用 DefaultMaxDirectoryCandidates。恰好等于上界必须被接受，
	// 只有严格超过才返回 ErrCandidateSetTooLarge——实现方式是多取一行判断，而不是
	// "取满即报错"。
	MaxCandidates int
}

// CandidateFilter 只包含 metadata 过滤维度。
//
// 它刻意不含 health/data_status：那两个是对已算出的摘要做结果筛选，如果下推到候选
// 查询，摘要分母就会随筛选漂移。
type CandidateFilter struct {
	Environment string
	TeamID      string
	Tier        string
	// Query 是名称/entity key 的模糊匹配片段，长度上限由 adapter 收敛。
	Query string
}

// CandidateSet 是一次完整候选扫描的结果。
//
// 它没有 Truncated 字段：完整候选集合要么被完整返回，要么因超过安全上界而显式失败。
// 一个"可能被截断的完整集合"没有正确的用法。
type CandidateSet struct {
	Items []ServiceIdentity
	// ReadAt 是这次一致读的时刻。全部批次在同一个只读快照内完成，因此它对整批有效。
	ReadAt time.Time
}

// CandidateScanner 提供 metadata 过滤后完整候选集合的有界分批遍历。
//
// 它与 RegistrationReader 分开是为了让实现可以各自演进，也让既有调用方与测试替身不必
// 因为目录摘要口径的修正而被迫改签名。
type CandidateScanner interface {
	ScanCandidates(ctx context.Context, req CandidateScanRequest) (CandidateSet, error)
}

// TopologyRequest 是 bounded canonical neighborhood 的请求。
type TopologyRequest struct {
	ServiceEntityID string
	// RootEntityIDs 是 Profile 声明的 root。为空表示没有 Profile：此时仍要以 service
	// entity 与 registration declared dependencies 给出基础总览。
	RootEntityIDs []string
	// DeclaredDependencies 来自 registration 事实，保证"无 Profile 也有基础 declared
	// 概览"不依赖 Profile 是否存在。
	DeclaredDependencies []DeclaredDependency
	MaxNodes             int
	MaxRelations         int
	MaxDepth             int
	// StaleAfter 是 observed 关系的 freshness TTL。declared 关系不因时间过期。
	StaleAfter time.Duration
}

// TopologyNode 是 canonical 邻域里的一个实体。
//
// 它不带 Health：拓扑里存在一个节点不等于它健康，Health 只能由 signal 经 evaluator
// 得出，没有 signal 就是 Unknown。
type TopologyNode struct {
	EntityID    string
	EntityKey   string
	Name        string
	Layer       model.EntityLayer
	Kind        string
	Environment string
	Depth       int
	ObservedAt  time.Time
}

// TopologyRelation 的方向固定 consumer/caller -> provider/callee。
//
// 归一化在 adapter 内完成：指向同一个 (source, target, kind) 的多行依赖边被折叠成一条
// canonical 关系，同时存在 declared 与 observed 时 Provenance 是 declared_and_observed。
// RelationID 始终是一个真实存在的依赖边主键（两者共存时取 declared 行的 ID），绝不是合成
// ID；被折叠的全部真实行 ID 保留在 MergedRelationIDs 里，因此 Profile 的 relation_refs 与
// signal scope refs 无论引用了哪一行都仍能命中。
type TopologyRelation struct {
	RelationID     string
	SourceEntityID string
	TargetEntityID string
	Kind           string
	Provenance     model.Provenance
	Criticality    string
	// ObservedAt 是组内最新的真实观测时间。
	ObservedAt time.Time
	// Stale 只表达 observed overlay 是否过期：declared 配置在被显式替换前一直有效，
	// 所以一条 declared_and_observed 关系可以既 stale 又保留 declared 底图。
	Stale bool
	// MergedRelationIDs 是被折叠进本条 canonical 关系的全部真实依赖边 ID（含
	// RelationID），按 ID 升序。
	MergedRelationIDs []string
}

// ResolvesRelationRef 判断一个 Profile 引用的关系 ID 是否指向这条 canonical 关系。
//
// 它同时接受 canonical ID 与任何被折叠的成员 ID：归一化不能让原本有效的
// relation_refs / signal scope refs 变成悬空引用。
func (r TopologyRelation) ResolvesRelationRef(relationID string) bool {
	if relationID == "" {
		return false
	}
	if relationID == r.RelationID {
		return true
	}
	for _, member := range r.MergedRelationIDs {
		if member == relationID {
			return true
		}
	}
	return false
}

// TopologyFacts 是一次 bounded 邻域读取的结果。
type TopologyFacts struct {
	SnapshotID string
	// ObservedAt 是邻域里最新的真实关系/实体观测时间，不是读取时刻。
	ObservedAt time.Time
	// Partial 为 true 表示达到 node/relation 上限而显式截断。
	Partial       bool
	PartialReason string
	Nodes         []TopologyNode
	Relations     []TopologyRelation
	ReadAt        time.Time
}

// TopologyReader 读取 canonical Entity/Relation 邻域。
type TopologyReader interface {
	ReadTopology(ctx context.Context, req TopologyRequest) (TopologyFacts, error)
}

// DomainRequest 是领域事实读取请求。
//
// 关键设计：边界完全由 service entity、Profile roots 与 registration declared
// dependencies 决定，**不接受 topology adapter 的返回**。topology 失败只降级
// topology section，domain 既不会变空也不会放宽到全库。
type DomainRequest struct {
	ServiceID       string
	ServiceEntityID string
	RootEntityIDs   []string
	// MaxBoundaryDepth 限制沿 declared 边扩展边界的跳数。
	MaxBoundaryDepth int
	// MaxBoundaryEntities 是边界实体数硬上限。
	MaxBoundaryEntities int
	// AllowedFocusKinds 是当前已发布 Profile 的 focus_kinds key 精确集合。
	// focus 只能用其中的 kind 做参数化 JSONB 匹配：Profile 已经过 identifier 与
	// value_pattern 校验，所以这是 Profile 驱动的通用实现，不是任何领域分支。
	AllowedFocusKinds []string
	// Focus 为 nil 表示不聚焦：返回整个服务边界的领域事实。
	Focus *model.Focus
	Limit int
}

// DomainAlert 是一条 active Alert 的投影。Alertmanager 是 Alert 生命周期的唯一权威，
// 这里只读不写。
type DomainAlert struct {
	AlertID          string
	Title            string
	SourceState      string
	ResponseState    string
	SuppressionState string
	// State 是给契约 domain_state 用的单一状态字符串，取上游权威状态。
	State     string
	Severity  string
	StartsAt  time.Time
	UpdatedAt time.Time
}

// DomainFault 是一条未关闭 Fault 的投影。Fault 数量不改写 Health。
type DomainFault struct {
	FaultID              string
	Title                string
	State                string
	Severity             model.FaultSeverity
	Kind                 string
	Scope                string
	EntityID             string
	EntityName           string
	OwnerTeamName        string
	FirstDetectedAt      time.Time
	UpdatedAt            time.Time
	ConfirmedImpactCount int
	PotentialImpactCount int
}

// DomainImpact 是 FaultImpact 的投影。第一版只有 potential/confirmed/ruled_out 三态，
// 不引入模糊的 likely，也不自动推理副本/主备。
type DomainImpact struct {
	FaultID    string
	EntityID   string
	EntityName string
	Layer      model.EntityLayer
	State      string
	UpdatedAt  time.Time
}

// DomainIncident 是关联 Incident 的投影。Incident 必须人工宣布，这里只读。
type DomainIncident struct {
	IncidentID string
	Number     int64
	Title      string
	State      string
	Severity   string
	// Relation 是 primary / supporting。
	Relation  string
	UpdatedAt time.Time
}

// DomainFacts 是一次领域读取的结果。
type DomainFacts struct {
	// BoundaryEntityIDs 是本次使用的稳定实体边界，按 ID 稳定排序，便于审计与测试。
	BoundaryEntityIDs []string
	// BoundaryTruncated 为 true 表示实体边界达到 max entities 上限而截断。
	BoundaryTruncated bool
	Alerts            []DomainAlert
	Faults            []DomainFault
	Impacts           []DomainImpact
	Incidents         []DomainIncident
	// Counts 始终按整个服务边界统计，不受 focus 收窄影响：它描述的是服务，不是这次
	// 聚焦查询。
	Counts model.DomainCounts
	// FocusApplied 为 true 表示 focus 真正参与了参数化匹配。
	FocusApplied bool
	// LatestFactAt 是领域事实里最新的真实时间，供真实时间锚使用。
	LatestFactAt time.Time
	ReadAt       time.Time
}

// DomainReader 读取服务边界内的 active Alert/Fault/Impact/Incident。
type DomainReader interface {
	ReadDomain(ctx context.Context, req DomainRequest) (DomainFacts, error)
}

// ObservationRequest 是语义信号读取请求。它只携带语义 key 与范围，绝不携带 PromQL、
// SQL、LogQL 或凭据。
//
// Subject 与 Purpose 是 Phase 3 新增的**兼容扩展**：两者都必须由装配层从 session 与路由
// 决定，URL、query 与 SIP 都不能自报。Phase 2 的 UnavailableObservationReader 不看这两个
// 字段，因此既有调用方不受影响；ObservationCoordinator 则对它们 fail closed——一个没有
// 用途的读取请求无法判断"该不该扇出证据查询"，只能拒绝而不是猜。
type ObservationRequest struct {
	Profile       model.Profile
	ServiceID     string
	Environment   string
	WindowSeconds int
	From          time.Time
	To            time.Time
	Focus         *model.Focus
	// Subject 是服务端从 session 计算出的身份。
	Subject Subject
	// Purpose 是服务端按路由选择的读取用途。零值不是合法用途。
	Purpose Purpose
	// BindingKeys 为空表示"读 Profile 声明的全部 binding"；目录摘要只传摘要需要的那几个，
	// 避免一次列表刷新把整份 Profile 的 binding 全查一遍。
	BindingKeys []string
}

// ObservationFacts 是遥测 adapter 的返回：规范化观测 + 自己的 source 状态。
//
// 它复用 health 包的观测类型，避免再定义一套平行结构后与 evaluator 漂移。
//
// Phase 3 的新增字段全部是**追加**：既有调用方（Phase 2 的 assembler 与
// UnavailableObservationReader）不读它们，编译与行为都不变。新增字段表达的是 Phase 2 无法
// 表达的四件事——focus 是否命中、journey 每个阶段的观测状态、observed topology hint、
// 以及身份/边界的可审计缺口。
type ObservationFacts struct {
	Signals      []health.SignalObservation
	Sources      []health.SourceObservation
	EvidenceRefs []model.EvidenceRef
	ReadAt       time.Time
	// Focus 表达"没请求过 / 命中 / 查成功但没有匹配 / 不知道"。零值是 not_requested。
	Focus FocusResult
	// JourneyStages 只使用 Page View v1 的 stage 枚举，且本阶段不凭缺失事实生成 skipped。
	JourneyStages []JourneyStageObservation
	// TopologyHints 是候选 observed 边。它们必须先经 alias 唯一解析才可能进 overlay。
	TopologyHints []ObservedTopologyHint
	// Issues 是身份解析与边界缺口的可审计摘要，不含未脱敏原文。
	Issues []ReconciliationIssue
	// InstanceHints 是 context-local 动态 instance inventory 侧输出。
	InstanceHints []ObservedInstanceHint
}

// ObservationReader 是 Golden/RED/USE 与 Evidence 的读取端口。Phase 3 用
// Prometheus-compatible 与 OpenObserve adapter 替换实现，ports 不变。
type ObservationReader interface {
	ReadObservations(ctx context.Context, req ObservationRequest) (ObservationFacts, error)
}

// UnavailableObservationReader 是 Phase 2 唯一注册的遥测实现。
//
// 它对 Profile 声明的每个 binding 返回一条显式的 unavailable 观测，reason 固定为
// telemetry_not_connected；observed_at 保持零值，表示"从未观测过"，由 projector 用
// 真实时间锚渲染，绝不填当前时间。
//
// 三条红线：不读 docs/contracts 的 fixture、不访问外部网络、不复用任何历史绿值。
type UnavailableObservationReader struct{}

// NewUnavailableObservationReader 构造 Phase 2 的遥测端口。
func NewUnavailableObservationReader() UnavailableObservationReader {
	return UnavailableObservationReader{}
}

// ReadObservations 永不返回 error：遥测未接入是一个已知的稳定事实，不是查询失败。
// 它通过 availability=unavailable + freshness=unknown 表达，从而把 Data 推到 Partial。
func (UnavailableObservationReader) ReadObservations(
	ctx context.Context, req ObservationRequest,
) (ObservationFacts, error) {
	facts := ObservationFacts{
		Signals:      make([]health.SignalObservation, 0, len(req.Profile.SignalBindings)),
		EvidenceRefs: []model.EvidenceRef{},
		// 遥测未接入时没有任何 focus 语义：显式 not_requested，而不是让零值被误读成
		// "查过但没匹配"。
		Focus: FocusNotRequested,
	}
	for _, binding := range req.Profile.SignalBindings {
		facts.Signals = append(facts.Signals, health.SignalObservation{
			BindingKey:   binding.Key,
			Availability: model.SignalAvailabilityUnavailable,
			// 没有观测：ObservedAt 保持零值，Value 保持 nil。
			SourceKey:     SourceKeyTelemetry,
			UnknownReason: ReasonTelemetryNotConnected,
		})
	}
	facts.Sources = []health.SourceObservation{{
		SourceKey: SourceKeyTelemetry,
		State:     model.AvailabilityUnavailable,
		Freshness: model.FreshnessUnknown,
		// 没有成功读取，所以没有 observed_at。
		ReasonCode: ReasonTelemetryNotConnected,
		// 重试不会让未接入的遥测变可用。
		Retryable: false,
	}}
	return facts, nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
